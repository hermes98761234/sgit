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

// landSource lands head (a commit whose parent is base) onto main of a
// source repo: compare-and-swap, with a merge-tree rebase when main moved.
func (s *Server) landSource(ctx context.Context, h *repoHandle, base, head string) (string, []gitx.ConflictFile, error) {
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

// landDraft implements §6.3 (and, from Task 8, §6.4) for an owned open draft.
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
	if len(groups) > 1 {
		return nil, status.Error(codes.FailedPrecondition, "this draft spans several mounts from different sources; land them separately") // removed in Task 8
	}
	landedAny := false
	for _, g := range groups {
		conflicts, err := r.landGroup(ctx, pr, d, dc, g)
		if errors.Is(err, errAlreadyOnMain) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 {
			head, cerr := r.s.projects.CatchUp(ctx, pr.h)
			if cerr != nil {
				return nil, r.s.hide(ctx, cerr)
			}
			return nil, conflictStatus(d.ID, head, conflicts, false, nil)
		}
		landedAny = true
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
	srcBase, err := r.s.store.ProjectSource(ctx, pr.p.ID, d.Base, g.repo)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.FailedPrecondition, "draft %s was made before mount %s/ was added; sync and commit again", d.ID, g.mounts[0])
	}
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
	_, conflicts, err := r.s.landSource(ctx, rh, srcBase, sc)
	if errors.Is(err, errAlreadyOnMain) {
		return nil, err
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted {
		return nil, err // our own message, safe
	}
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	for i := range conflicts {
		p, ok := projectPath(pr.mounts, g.repo, conflicts[i].Path)
		if !ok { // a conflict outside the touched folders is impossible; never leak it
			return nil, r.s.hide(ctx, fmt.Errorf("conflict outside mounts in %s", g.repo))
		}
		conflicts[i].Path = p
	}
	return conflicts, nil
}

func conflictStatus(draftID, main string, files []gitx.ConflictFile, partial bool, landed []string) error {
	st := status.New(codes.Aborted, fmt.Sprintf("conflict in %d file(s)", len(files)))
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
