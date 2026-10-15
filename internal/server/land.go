package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (r *projectService) Land(ctx context.Context, req *sgitv1.LandRequest) (*sgitv1.LandResponse, error) {
	pr, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	return r.landDraft(ctx, pr, u, req.Id)
}

func (r *projectService) CommitAndLand(ctx context.Context, req *sgitv1.CommitAndLandRequest) (*sgitv1.LandResponse, error) {
	pr, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	var d store.Draft
	if req.DraftId == "" {
		d, err = r.createDraft(ctx, pr, u, req.Base, req.Changes, req.Title, req.Message)
	} else {
		d, err = r.updateDraft(ctx, pr, u, req.DraftId, req.Base, req.Changes, req.Message)
	}
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return r.landDraft(ctx, pr, u, d.ID)
}

var errAlreadyOnMain = errors.New("changes already on main")

// errOutsideMounts means the landed tree would change a source path the
// caller may not write (merge-tree follows upstream renames, so a clean
// rebase can move the draft's content out of its folder).
var errOutsideMounts = errors.New("land would change paths outside the writable mounts")

// landSource lands head (a commit whose parent is base) onto main of a
// source repo: compare-and-swap, with a merge-tree rebase when main moved.
// Every path the land changes relative to main must pass allowed.
func (s *Server) landSource(ctx context.Context, h *repoHandle, base, head string, allowed func(sourcePath string) bool) (string, []gitx.ConflictFile, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()
	dc, err := h.git.ReadCommit(ctx, head)
	if err != nil {
		return "", nil, err
	}
	for attempt := 0; attempt < s.cfg.LandRetries; attempt++ {
		mainOld, err := h.git.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", nil, err
		}
		tree := dc.Tree
		if mainOld != base {
			mr, err := h.git.MergeTree(ctx, base, mainOld, head)
			if err != nil {
				return "", nil, err
			}
			if !mr.Clean {
				return "", mr.Conflicts, nil
			}
			tree = mr.Tree
		}
		mc, err := h.git.ReadCommit(ctx, mainOld)
		if err != nil {
			return "", nil, err
		}
		if tree == mc.Tree {
			return mainOld, nil, errAlreadyOnMain
		}
		changed, err := h.git.DiffTree(ctx, mc.Tree, tree)
		if err != nil {
			return "", nil, err
		}
		for _, c := range changed {
			if !allowed(c.Path) {
				return "", nil, errOutsideMounts
			}
		}
		committer := dc.Committer
		committer.When = s.cfg.Now()
		newC, err := h.git.CommitTree(ctx, tree, []string{mainOld}, dc.Message, dc.Author, committer)
		if err != nil {
			return "", nil, err
		}
		// Once main moves, the bookkeeping must not be cancelled by the
		// client; the sequence row is recorded under the same context.
		bg := context.WithoutCancel(ctx)
		// Recorded before main moves, so a catch-up never sees this commit
		// on main without its sequence number. A row for a commit that loses
		// the CAS is never looked up.
		if err := s.store.RecordLand(bg, h.name, newC, s.cfg.Now()); err != nil {
			return "", nil, fmt.Errorf("recording the land sequence: %w", err)
		}
		if s.cfg.BeforeCAS != nil {
			s.cfg.BeforeCAS(h.name)
		}
		err = h.git.UpdateRef(bg, gitx.MainRef, newC, mainOld)
		if errors.Is(err, gitx.ErrRefMoved) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		s.mirror.Notify(h.name)
		return newC, nil, nil
	}
	return "", nil, status.Errorf(codes.ResourceExhausted, "main kept moving; gave up after %d attempts", s.cfg.LandRetries)
}

// repoChanges is the part of a draft that lands in one source repo.
type repoChanges struct {
	repo    string
	mounts  []string      // mount paths touched
	project []gitx.Change // project paths (for rebuilding a partial draft)
	source  []gitx.Change // source paths
}

// splitByRepo maps project changes to source changes, grouped by repo and
// sorted by repo name, enforcing §6.1 again (mounts may have changed).
func splitByRepo(mounts []store.Mount, changes []gitx.Change) ([]*repoChanges, error) {
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	if err := checkPaths(mounts, paths); err != nil {
		return nil, err
	}
	by := map[string]*repoChanges{}
	for _, c := range changes {
		m, _ := mountFor(mounts, c.Path)
		g := by[m.Repo]
		if g == nil {
			g = &repoChanges{repo: m.Repo}
			by[m.Repo] = g
		}
		if !slices.Contains(g.mounts, m.Path) {
			g.mounts = append(g.mounts, m.Path)
		}
		g.project = append(g.project, c)
		sc := c
		sc.Path = sourcePath(m, c.Path)
		g.source = append(g.source, sc)
	}
	out := make([]*repoChanges, 0, len(by))
	for _, g := range by {
		sort.Strings(g.mounts)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].repo < out[j].repo })
	return out, nil
}

// landDraft implements §6.3 and §6.4 for an owned open draft.
func (r *projectService) landDraft(ctx context.Context, pr *proj, u store.User, id string) (*sgitv1.LandResponse, error) {
	if r.s.cfg.BeforeLand != nil {
		r.s.cfg.BeforeLand(pr.p.ID)
	}
	unlock, err := r.s.lock(ctx, pr)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// 1. Catch up (open did) and 2. re-check access now: a grant revoked or
	// downgraded after the draft was made blocks the land, and so does a
	// mount made read-only or removed since open read the mounts.
	if err := auth.Authorize(ctx, r.s.store, u, pr.p.ID, auth.ActionWrite); err != nil {
		if errors.Is(err, auth.ErrNoAccess) {
			return nil, auth.ProjectNotFound(pr.ref)
		}
		return nil, err
	}
	mounts, err := r.s.store.Mounts(ctx, pr.p.ID)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	pr.mounts = mounts
	d, err := r.loadOwnedOpenDraft(ctx, pr, u, id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	if cur, err := pr.h.Git.ReadRef(ctx, headRef(d.ID)); err != nil || cur != d.Head {
		return nil, status.Errorf(codes.Aborted, "draft %s is inconsistent (head ref moved); update it and retry", id)
	}
	dc, err := pr.h.Git.ReadCommit(ctx, d.Head)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	// 3. Diff against the project base and map to mounts.
	changes, err := pr.h.Git.DiffTree(ctx, d.Base, d.Head)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	groups, err := splitByRepo(pr.mounts, changes)
	if err != nil {
		return nil, err
	}
	if err := r.checkLayout(ctx, pr, d, groups); err != nil {
		return nil, err
	}
	// 4. Land repo by repo in repo-name order (§6.4).
	var landed []string // mount paths that landed
	landedAny := false
	for i, g := range groups {
		conflicts, err := r.landGroup(ctx, pr, d, dc, g)
		if errors.Is(err, errAlreadyOnMain) {
			continue
		}
		if err == nil && len(conflicts) == 0 {
			landed = append(landed, g.mounts...)
			landedAny = true
			continue
		}
		// Stop at the first conflict or error (§6.4).
		if !landedAny && err != nil {
			return nil, err
		}
		head, cerr := r.s.projects.CatchUp(ctx, pr.h)
		if cerr != nil {
			return nil, r.s.hide(ctx, cerr)
		}
		// After the catch-up every stage inside the mounts is in the project.
		conflicts = projectStages(ctx, pr.h.Git, conflicts)
		if !landedAny {
			return nil, conflictStatus(d.ID, head, conflicts, false, nil)
		}
		// A failed rebuild leaves the draft as it was (consistent); a retry
		// skips the landed repos (already on main). The answer stays partial.
		if _, rerr := r.rebuildDraft(ctx, pr, d, dc, groups[i:]); rerr != nil {
			r.s.log.ErrorContext(ctx, "partial land: rebuilding the draft failed", "request_id", auth.RequestID(ctx), "draft", d.ID, "err", rerr)
		}
		if err != nil {
			st := status.New(codes.Aborted, fmt.Sprintf("landed %s; the rest did not land: %s", strings.Join(landed, ", "), r.s.safeMessage(ctx, err)))
			withDet, derr := st.WithDetails(&sgitv1.ConflictDetail{DraftId: d.ID, Main: head, Partial: true, Landed: landed})
			if derr != nil {
				return nil, st.Err()
			}
			return nil, withDet.Err()
		}
		return nil, conflictStatus(d.ID, head, conflicts, true, landed)
	}
	if !landedAny {
		return nil, status.Errorf(codes.FailedPrecondition, "draft %s changes are already on main", id)
	}
	// 5. Catch up and answer with the new project head.
	head, err := r.s.projects.CatchUp(ctx, pr.h)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	bg := context.WithoutCancel(ctx)
	d.Status, d.LandedAs, d.UpdatedAt = store.DraftLanded, head, r.s.cfg.Now()
	if err := r.s.store.UpdateDraft(bg, d); err != nil {
		r.s.log.ErrorContext(bg, "landed but failed to mark draft", "draft", d.ID, "err", err)
	}
	parent := ""
	if hc, err := pr.h.Git.ReadCommit(ctx, head); err == nil && hc.Tree == dc.Tree && len(hc.Parents) == 1 {
		parent = hc.Parents[0]
	}
	return &sgitv1.LandResponse{Commit: head, DraftId: d.ID, Parent: parent}, nil
}

// landGroup builds the source commit for one repo on the source base recorded
// for the draft's project base (§6.3 step 4) and lands it. Conflicts come
// back with project paths. Source-side failures are hidden.
func (r *projectService) landGroup(ctx context.Context, pr *proj, d store.Draft, dc gitx.Commit, g *repoChanges) ([]gitx.ConflictFile, error) {
	// checkLayout guarantees the base recorded every touched repo.
	srcBase, err := r.s.store.ProjectSource(ctx, pr.p.ID, d.Base, g.repo)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	rh, err := r.s.repos.get(ctx, g.repo)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	var blobs []string
	for _, c := range g.source {
		if c.Op != gitx.OpDelete {
			blobs = append(blobs, c.BlobSHA)
		}
	}
	if _, err := gitx.CopyObjects(ctx, pr.h.Git, rh.git, blobs, nil); err != nil {
		return nil, r.s.hide(ctx, err)
	}
	tree, err := rh.git.BuildTree(ctx, srcBase, g.source)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	sc, err := rh.git.CommitTree(ctx, tree, []string{srcBase}, dc.Message, dc.Author, dc.Committer)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	_, conflicts, err := r.s.landSource(ctx, rh, srcBase, sc, func(sp string) bool { return writableSource(pr.mounts, g.repo, sp) })
	if errors.Is(err, errAlreadyOnMain) {
		return nil, err
	}
	if errors.Is(err, errOutsideMounts) { // never name the source path
		return nil, status.Errorf(codes.FailedPrecondition, "draft %s: main changed under the draft (files moved outside the writable mounts); sync and commit again", d.ID)
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted {
		return nil, err // our own message, safe
	}
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	for i := range conflicts {
		p, ok := projectPath(pr.mounts, g.repo, conflicts[i].Path)
		if !ok { // e.g. an upstream rename out of the mounts; never name the source path
			return nil, status.Errorf(codes.FailedPrecondition, "draft %s: main changed under the draft (files moved outside the project); sync and commit again", d.ID)
		}
		conflicts[i].Path = p
	}
	return conflicts, nil
}

// checkLayout refuses a draft whose project base was made under a different
// layout for a touched mount (a path unmounted and mounted again from another
// folder or repo, or mounted after the base): its changes would land in
// whatever the path maps to now. Mounts are forward only (§5.5), so the user
// syncs and commits again. The message names only the project path.
func (r *projectService) checkLayout(ctx context.Context, pr *proj, d store.Draft, groups []*repoChanges) error {
	layout, err := r.s.store.ProjectLayout(ctx, pr.p.ID, d.Base)
	if err != nil {
		return r.s.hide(ctx, err)
	}
	for _, g := range groups {
		for _, path := range g.mounts {
			i := slices.IndexFunc(pr.mounts, func(m store.Mount) bool { return m.Path == path })
			cur := store.LayoutMount{Path: path, Repo: pr.mounts[i].Repo, Folder: pr.mounts[i].Folder}
			j := slices.IndexFunc(layout, func(l store.LayoutMount) bool { return l.Path == path })
			switch {
			case j < 0:
				return status.Errorf(codes.FailedPrecondition, "draft %s was made before mount %s/ was added; sync and commit again", d.ID, path)
			case layout[j] != cur:
				return status.Errorf(codes.FailedPrecondition, "draft %s was made before mount %s/ changed; sync and commit again", d.ID, path)
			}
		}
	}
	return nil
}

// rebuildDraft rewrites the draft to hold only the changes of the groups that
// did not land, keeping its id (§6.4). It stays on its original project base
// so a retry still does a 3-way merge per repo against upstream instead of
// overwriting it; the client learns the new head from ConflictDetail.Main.
// On failure the draft is left unchanged (refs and store agree).
func (r *projectService) rebuildDraft(ctx context.Context, pr *proj, d store.Draft, dc gitx.Commit, rest []*repoChanges) (store.Draft, error) {
	var changes []gitx.Change
	for _, g := range rest {
		changes = append(changes, g.project...)
	}
	tree, err := pr.h.Git.BuildTree(ctx, d.Base, changes)
	if err != nil {
		return d, err
	}
	committer := dc.Committer
	committer.When = r.s.cfg.Now()
	c, err := pr.h.Git.CommitTree(ctx, tree, []string{d.Base}, dc.Message, dc.Author, committer)
	if err != nil {
		return d, err
	}
	bg := context.WithoutCancel(ctx)
	if err := pr.h.Git.UpdateRef(bg, headRef(d.ID), c, d.Head); err != nil {
		return d, err
	}
	nd := d
	nd.Version++
	nd.Head, nd.UpdatedAt = c, r.s.cfg.Now()
	err = pr.h.Git.UpdateRef(bg, versionRef(d.ID, nd.Version), c, "")
	if err == nil {
		if err = r.s.store.UpdateDraft(bg, nd); err == nil {
			return nd, nil
		}
		if rerr := pr.h.Git.UpdateRef(bg, versionRef(d.ID, nd.Version), gitx.ZeroSHA, c); rerr != nil {
			r.s.log.ErrorContext(bg, "rolling back draft version ref", "draft", d.ID, "err", rerr)
		}
	}
	if rerr := pr.h.Git.UpdateRef(bg, headRef(d.ID), d.Head, c); rerr != nil {
		r.s.log.ErrorContext(bg, "rolling back draft head ref", "draft", d.ID, "err", rerr)
	}
	return d, err
}

// safeMessage returns err's message when it is one of our own status errors
// (safe for clients), otherwise logs it and returns a request-id reference.
// An Internal status was already logged by hide, so it is not logged again.
func (s *Server) safeMessage(ctx context.Context, err error) string {
	st, ok := status.FromError(err)
	switch {
	case ok && st.Code() == codes.Internal:
		return fmt.Sprintf("internal error (request %s)", auth.RequestID(ctx))
	case ok && st.Code() != codes.Unknown:
		return st.Message()
	}
	return status.Convert(s.hide(ctx, err)).Message()
}

func conflictStatus(draftID, main string, files []gitx.ConflictFile, partial bool, landed []string) error {
	msg := fmt.Sprintf("conflict in %d file(s)", len(files))
	if partial { // also for callers that do not read the details
		msg = fmt.Sprintf("landed %s; the rest did not land: %s", strings.Join(landed, ", "), msg)
	}
	st := status.New(codes.Aborted, msg)
	det := &sgitv1.ConflictDetail{DraftId: draftID, Main: main, Partial: partial, Landed: landed}
	for _, f := range files {
		det.Files = append(det.Files, &sgitv1.ConflictFile{
			Path: strings.ToValidUTF8(f.Path, "�"), BaseSha: f.Base, OursSha: f.Ours, TheirsSha: f.Theirs,
		})
	}
	withDet, err := st.WithDetails(det)
	if err != nil {
		return st.Err()
	}
	return withDet.Err()
}

// projectStages blanks every conflict stage SHA the project repo does not
// hold: merge-tree follows renames, so a stage may come from outside the
// mounts, and such a SHA must never reach the client.
func projectStages(ctx context.Context, g *gitx.Repo, files []gitx.ConflictFile) []gitx.ConflictFile {
	held := func(sha string) string {
		if sha == "" {
			return ""
		}
		if _, _, err := g.ObjectInfo(ctx, sha); err != nil {
			return "" // missing, or unknown: never echo it
		}
		return sha
	}
	out := make([]gitx.ConflictFile, len(files))
	for i, f := range files {
		out[i] = gitx.ConflictFile{Path: f.Path, Base: held(f.Base), Ours: held(f.Ours), Theirs: held(f.Theirs)}
	}
	return out
}
