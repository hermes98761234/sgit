// Package project derives each project's bare repo from the mounted folders
// of its source repos (spec §5) and keeps it up to date.
package project

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

// Sources opens mounted source repos by name.
type Sources interface {
	Source(ctx context.Context, repo string) (*gitx.Repo, error)
}

// Config wires an Engine to its storage and source repos.
type Config struct {
	Root         string // <data>/projects
	Store        *store.Store
	Sources      Sources
	Now          func() time.Time
	AfterRebuild func(ctx context.Context, id int64) error // optional
}

// Handle is an open project repo.
type Handle struct {
	ID     int64
	Git    *gitx.Repo
	LandMu sync.Mutex // drafts and lands through this project (Task 7)
	mu     sync.Mutex // translation: catch-up, mount, unmount
}

// Engine keeps every project's derived repo in step with its mounts.
type Engine struct {
	cfg     Config
	mu      sync.Mutex
	open    map[int64]*Handle
	opening map[int64]*sync.Mutex // per-project lock held while opening or deleting a repo
}

func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{cfg: cfg, open: map[int64]*Handle{}, opening: map[int64]*sync.Mutex{}}
}

func (e *Engine) Path(id int64) string {
	return filepath.Join(e.cfg.Root, strconv.FormatInt(id, 10)+".git")
}

func LayoutMessage(verb, path string) string {
	return fmt.Sprintf("Project layout: %s `%s/`\n", verb, path)
}

// openLock returns the project's open lock, or the open handle if there is one.
func (e *Engine) openLock(id int64) (*Handle, *sync.Mutex) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.open[id]; ok {
		return h, nil
	}
	l := e.opening[id]
	if l == nil {
		l = &sync.Mutex{}
		e.opening[id] = l
	}
	return nil, l
}

// Get opens the project repo, creating it when the project has no commits
// and rebuilding it from the recorded history when it is missing. A rebuild
// holds only that project's open lock, so other projects are not delayed.
func (e *Engine) Get(ctx context.Context, id int64) (*Handle, error) {
	h, l := e.openLock(id)
	if h != nil {
		return h, nil
	}
	l.Lock()
	defer l.Unlock()
	if h, _ := e.openLock(id); h != nil { // opened while we waited
		return h, nil
	}
	dir := e.Path(id)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		head, _, err := e.cfg.Store.ProjectHead(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(e.cfg.Root, 0o750); err != nil {
			return nil, err
		}
		if head == "" {
			if err := gitx.InitBare(ctx, dir); err != nil {
				return nil, err
			}
		} else if err := e.rebuild(ctx, id, dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	h = &Handle{ID: id, Git: gitx.Open(dir)}
	e.mu.Lock()
	e.open[id] = h
	e.mu.Unlock()
	return h, nil
}

// CatchUp translates new source commits. When no mounted repo's main moved
// it costs one ref read per mounted repo and takes no lock.
func (e *Engine) CatchUp(ctx context.Context, h *Handle) (string, error) {
	mounts, err := e.cfg.Store.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	cursors, err := e.cfg.Store.ProjectCursors(ctx, h.ID)
	if err != nil {
		return "", err
	}
	moved := false
	for _, repo := range reposOf(mounts) {
		src, err := e.cfg.Sources.Source(ctx, repo)
		if err != nil {
			return "", err
		}
		main, err := src.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", err
		}
		if main != cursors[repo] {
			moved = true
			break
		}
	}
	if !moved {
		head, _, err := e.cfg.Store.ProjectHead(ctx, h.ID)
		return head, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return e.catchUpLocked(ctx, h)
}

func (e *Engine) catchUpLocked(ctx context.Context, h *Handle) (string, error) {
	st := e.cfg.Store
	head, seq, err := st.ProjectHead(ctx, h.ID)
	if err != nil {
		return "", err
	}
	if err := e.moveRef(ctx, h, head); err != nil { // the DB is the truth; repair a lagging ref
		return "", err
	}
	mounts, err := st.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	cursors, err := st.ProjectCursors(ctx, h.ID)
	if err != nil {
		return "", err
	}
	byRepo := map[string][]store.Mount{}
	for _, m := range mounts {
		byRepo[m.Repo] = append(byRepo[m.Repo], m)
	}
	cur := map[string]string{} // mount path -> current subtree ("" = absent)
	var queues []*queue
	for _, repo := range reposOf(mounts) {
		src, err := e.cfg.Sources.Source(ctx, repo)
		if err != nil {
			return "", err
		}
		main, err := src.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", err
		}
		c := cursors[repo]
		if c != "" {
			for _, m := range byRepo[repo] {
				t, err := src.Subtrees(ctx, []string{c}, m.Folder)
				if err != nil {
					return "", err
				}
				cur[m.Path] = norm(m.Path, t[0])
			}
		}
		if main == c {
			continue
		}
		list, err := src.FirstParents(ctx, main, c)
		if err != nil {
			return "", err
		}
		shas := make([]string, len(list))
		for i, fc := range list {
			shas[i] = fc.SHA
		}
		q := &queue{repo: repo, src: src, commits: list, trees: map[string][]string{}}
		for _, m := range byRepo[repo] {
			if q.trees[m.Path], err = src.Subtrees(ctx, shas, m.Folder); err != nil {
				return "", err
			}
		}
		if q.seqs, err = st.LandSeqs(ctx, repo, shas); err != nil {
			return "", err
		}
		queues = append(queues, q)
	}
	if len(queues) == 0 {
		return head, nil
	}
	sources := maps.Clone(cursors)
	if sources == nil {
		sources = map[string]string{}
	}
	layout := layoutOf(mounts)
	var out []store.ProjectCommit
	parent := head
	for q := next(queues); q != nil; q = next(queues) {
		c := q.commits[q.pos]
		var want, have []string
		changed := false
		for _, m := range byRepo[q.repo] {
			t := norm(m.Path, q.trees[m.Path][q.pos])
			if t == cur[m.Path] {
				continue
			}
			changed = true
			if t != "" {
				want = append(want, t)
			}
			if cur[m.Path] != "" {
				have = append(have, cur[m.Path])
			}
			cur[m.Path] = t
		}
		sources[q.repo] = c.SHA
		q.pos++
		if !changed {
			continue
		}
		if _, err := gitx.CopyObjects(ctx, q.src, h.Git, want, have); err != nil {
			return "", err
		}
		tree, err := compose(ctx, h.Git, mountTrees(mounts, cur))
		if err != nil {
			return "", err
		}
		sc, err := q.src.ReadCommit(ctx, c.SHA)
		if err != nil {
			return "", err
		}
		sha, err := h.Git.WriteCommit(ctx, tree, parents(parent), sc.AuthorRaw, sc.CommitterRaw, sc.Message)
		if err != nil {
			return "", err
		}
		seq++
		out = append(out, store.ProjectCommit{SHA: sha, Seq: seq, Kind: store.KindSource, Repo: q.repo,
			SourceSHA: c.SHA, Layout: layout, Sources: maps.Clone(sources)})
		parent = sha
	}
	if err := st.RecordProjectCommits(ctx, h.ID, out, sources, nil); err != nil {
		return "", err
	}
	if err := e.moveRef(ctx, h, parent); err != nil {
		return "", err
	}
	return parent, nil
}

// moveRef points the project's main at sha (no-op when sha is "" or current).
func (e *Engine) moveRef(ctx context.Context, h *Handle, sha string) error {
	if sha == "" {
		return nil
	}
	cur, err := h.Git.ReadRef(ctx, gitx.MainRef)
	if errors.Is(err, gitx.ErrNotFound) {
		cur = ""
	} else if err != nil {
		return err
	}
	if cur == sha {
		return nil
	}
	return h.Git.UpdateRef(ctx, gitx.MainRef, sha, cur)
}

func parents(p string) []string {
	if p == "" {
		return nil
	}
	return []string{p}
}

func reposOf(mounts []store.Mount) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mounts {
		if !seen[m.Repo] {
			seen[m.Repo] = true
			out = append(out, m.Repo)
		}
	}
	sort.Strings(out)
	return out
}

func layoutOf(mounts []store.Mount) []store.LayoutMount {
	var out []store.LayoutMount
	for _, m := range mounts {
		out = append(out, store.LayoutMount{Path: m.Path, Repo: m.Repo, Folder: m.Folder})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func mountTrees(mounts []store.Mount, cur map[string]string) []gitx.MountTree {
	out := make([]gitx.MountTree, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, gitx.MountTree{Path: m.Path, Tree: cur[m.Path]})
	}
	return out
}

// norm treats an empty folder tree as absent, except for a "." mount, so
// project trees never hold empty directories (an empty source repo's root
// mounted at a sub-path shows up as no entry at all).
func norm(path, tree string) string {
	if path != "." && tree == gitx.EmptyTree {
		return ""
	}
	return tree
}

// compose builds the root tree for mts, refusing overlapping mount paths
// (gitx.ComposeTree would silently drop or override one of them).
func compose(ctx context.Context, g *gitx.Repo, mts []gitx.MountTree) (string, error) {
	for i := range mts {
		for _, o := range mts[i+1:] {
			if overlaps(mts[i].Path, o.Path) {
				return "", fmt.Errorf("%w: mount path %q overlaps %q", gitx.ErrInvalid, o.Path, mts[i].Path)
			}
		}
	}
	return g.ComposeTree(ctx, mts)
}

func overlaps(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// Mount adds m. The first mount of a project without commits imports the
// history of the folder (§5.4); later mounts write one layout commit (§5.5).
// Re-mounting the same path with the same source only sets the read-only flag.
func (e *Engine) Mount(ctx context.Context, h *Handle, m store.Mount, actor gitx.Signature) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	head, err := e.catchUpLocked(ctx, h)
	if err != nil {
		return "", err
	}
	mounts, err := e.cfg.Store.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	for _, old := range mounts {
		if old.Path == m.Path {
			if old.Repo == m.Repo && old.Folder == m.Folder {
				return head, e.cfg.Store.SetMountReadOnly(ctx, h.ID, m.Path, m.ReadOnly)
			}
			return "", fmt.Errorf("%w: mount %q already exists; unmount it first", store.ErrExists, m.Path)
		}
		if overlaps(old.Path, m.Path) {
			return "", fmt.Errorf("%w: mount path %q overlaps %q", gitx.ErrInvalid, m.Path, old.Path)
		}
	}
	m.AddedAt = e.cfg.Now()
	if head == "" {
		sha, err := e.firstMountLocked(ctx, h, m, actor)
		if err != nil {
			if rerr := e.resetLocked(context.WithoutCancel(ctx), h); rerr != nil {
				return "", errors.Join(err, rerr)
			}
			return "", err
		}
		return sha, nil
	}
	return e.layoutLocked(ctx, h, actor, "added", m.Path, &store.MountChange{Add: &m})
}

// firstMountLocked records m on a project without commits and imports the
// history of its folder, or writes a layout commit when nothing touches it.
func (e *Engine) firstMountLocked(ctx context.Context, h *Handle, m store.Mount, actor gitx.Signature) (string, error) {
	if err := e.cfg.Store.RecordProjectCommits(ctx, h.ID, nil, nil, &store.MountChange{Add: &m}); err != nil {
		return "", err
	}
	head, err := e.catchUpLocked(ctx, h)
	if err != nil || head != "" {
		return head, err
	}
	return e.layoutLocked(ctx, h, actor, "added", m.Path, nil)
}

// resetLocked undoes a failed first mount: the project returns to having no
// mounts, commits or cursors, and its main ref is removed, so a retry starts
// clean. Objects already copied stay unreachable in the project repo.
func (e *Engine) resetLocked(ctx context.Context, h *Handle) error {
	cur, err := h.Git.ReadRef(ctx, gitx.MainRef)
	switch {
	case errors.Is(err, gitx.ErrNotFound):
	case err != nil:
		return err
	default:
		if err := h.Git.UpdateRef(ctx, gitx.MainRef, gitx.ZeroSHA, cur); err != nil {
			return err
		}
	}
	return e.cfg.Store.ResetProject(ctx, h.ID)
}

func (e *Engine) Unmount(ctx context.Context, h *Handle, path string, actor gitx.Signature) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := e.catchUpLocked(ctx, h); err != nil {
		return "", err
	}
	mounts, err := e.cfg.Store.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	for _, m := range mounts {
		if m.Path == path {
			return e.layoutLocked(ctx, h, actor, "removed", path, &store.MountChange{Remove: path})
		}
	}
	return "", fmt.Errorf("%w: mount %q", store.ErrNotFound, path)
}

// layoutLocked writes one layout commit holding the current content of every
// mount after applying mc. New repos start at their current main.
func (e *Engine) layoutLocked(ctx context.Context, h *Handle, actor gitx.Signature, verb, path string, mc *store.MountChange) (string, error) {
	st := e.cfg.Store
	head, seq, err := st.ProjectHead(ctx, h.ID)
	if err != nil {
		return "", err
	}
	mounts, err := st.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	cursors, err := st.ProjectCursors(ctx, h.ID)
	if err != nil {
		return "", err
	}
	if mc != nil && mc.Add != nil {
		mounts = append(mounts, *mc.Add)
	}
	if mc != nil && mc.Remove != "" {
		kept := mounts[:0]
		for _, m := range mounts {
			if m.Path != mc.Remove {
				kept = append(kept, m)
			}
		}
		mounts = kept
	}
	newCursors := map[string]string{}
	cur := map[string]string{}
	for _, m := range mounts {
		src, err := e.cfg.Sources.Source(ctx, m.Repo)
		if err != nil {
			return "", err
		}
		c, ok := newCursors[m.Repo]
		if !ok {
			if c, ok = cursors[m.Repo]; !ok || c == "" {
				if c, err = src.ReadRef(ctx, gitx.MainRef); err != nil {
					return "", err
				}
			}
			newCursors[m.Repo] = c
		}
		t, err := src.Subtrees(ctx, []string{c}, m.Folder)
		if err != nil {
			return "", err
		}
		tt := norm(m.Path, t[0])
		if tt != "" {
			if _, err := gitx.CopyObjects(ctx, src, h.Git, []string{tt}, nil); err != nil {
				return "", err
			}
		}
		cur[m.Path] = tt
	}
	tree, err := compose(ctx, h.Git, mountTrees(mounts, cur))
	if err != nil {
		return "", err
	}
	actor.When = time.Unix(actor.When.Unix(), 0).UTC()
	ident := gitx.FormatIdent(actor)
	msg := LayoutMessage(verb, path)
	sha, err := h.Git.WriteCommit(ctx, tree, parents(head), ident, ident, msg)
	if err != nil {
		return "", err
	}
	pc := store.ProjectCommit{SHA: sha, Seq: seq + 1, Kind: store.KindLayout, Layout: layoutOf(mounts),
		ActorName: actor.Name, ActorEmail: actor.Email, ActorWhen: actor.When.Unix(), Message: msg, Sources: maps.Clone(newCursors)}
	if err := st.RecordProjectCommits(ctx, h.ID, []store.ProjectCommit{pc}, newCursors, mc); err != nil {
		return "", err
	}
	return sha, e.moveRef(ctx, h, sha)
}

// rebuild recreates dir from the recorded derivation and checks every SHA.
// AfterRebuild runs before the rebuilt repo is moved into place, so a failed
// hook (or any other failure) leaves no repo and the next Get retries it all.
func (e *Engine) rebuild(ctx context.Context, id int64, dir string) (err error) {
	commits, err := e.cfg.Store.ProjectCommits(ctx, id)
	if err != nil {
		return err
	}
	stage := dir + ".rebuild"
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := gitx.InitBare(ctx, stage); err != nil {
		return err
	}
	g := gitx.Open(stage)
	defer func() {
		g.Close()
		if err != nil {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	copied := map[string]string{} // repo \x00 folder -> last tree copied
	parent := ""
	for _, pc := range commits {
		var mts []gitx.MountTree
		for _, lm := range pc.Layout {
			src, err := e.cfg.Sources.Source(ctx, lm.Repo)
			if err != nil {
				return err
			}
			t, err := src.Subtrees(ctx, []string{pc.Sources[lm.Repo]}, lm.Folder)
			if err != nil {
				return err
			}
			tt := norm(lm.Path, t[0])
			k := lm.Repo + "\x00" + lm.Folder
			if tt != "" && copied[k] != tt {
				var have []string
				if copied[k] != "" {
					have = []string{copied[k]}
				}
				if _, err := gitx.CopyObjects(ctx, src, g, []string{tt}, have); err != nil {
					return err
				}
				copied[k] = tt
			}
			mts = append(mts, gitx.MountTree{Path: lm.Path, Tree: tt})
		}
		tree, err := compose(ctx, g, mts)
		if err != nil {
			return err
		}
		var author, committer, msg string
		if pc.Kind == store.KindSource {
			src, err := e.cfg.Sources.Source(ctx, pc.Repo)
			if err != nil {
				return err
			}
			sc, err := src.ReadCommit(ctx, pc.SourceSHA)
			if err != nil {
				return err
			}
			author, committer, msg = sc.AuthorRaw, sc.CommitterRaw, sc.Message
		} else {
			ident := gitx.FormatIdent(gitx.Signature{Name: pc.ActorName, Email: pc.ActorEmail, When: time.Unix(pc.ActorWhen, 0)})
			author, committer, msg = ident, ident, pc.Message
		}
		sha, err := g.WriteCommit(ctx, tree, parents(parent), author, committer, msg)
		if err != nil {
			return err
		}
		if sha != pc.SHA {
			return fmt.Errorf("project %d: rebuilt commit %d is %s, recorded %s", id, pc.Seq, sha, pc.SHA)
		}
		parent = sha
	}
	if parent != "" {
		if err := g.UpdateRef(ctx, gitx.MainRef, parent, ""); err != nil {
			return err
		}
	}
	g.Close()
	if e.cfg.AfterRebuild != nil {
		if err := e.cfg.AfterRebuild(ctx, id); err != nil {
			return err
		}
	}
	return os.Rename(stage, dir)
}

// Delete closes and removes a project's derived repo (the DB rows are the caller's).
func (e *Engine) Delete(id int64) error {
	e.mu.Lock()
	l := e.opening[id]
	if l == nil {
		l = &sync.Mutex{}
		e.opening[id] = l
	}
	e.mu.Unlock()
	l.Lock() // wait for a running open or rebuild
	defer l.Unlock()
	e.mu.Lock()
	h := e.open[id]
	delete(e.open, id)
	e.mu.Unlock()
	if h != nil {
		h.mu.Lock() // wait for a running catch-up
		h.Git.Close()
		h.mu.Unlock()
	}
	return os.RemoveAll(e.Path(id))
}

func (e *Engine) CloseAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, h := range e.open {
		h.Git.Close()
		delete(e.open, id)
	}
}
