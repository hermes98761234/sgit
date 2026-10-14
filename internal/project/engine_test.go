package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

// Local helpers: importing testenv here would be an import cycle (testenv -> server -> project).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(err)
	}
	return a, b
}

type srcs map[string]*gitx.Repo

func (s srcs) Source(_ context.Context, repo string) (*gitx.Repo, error) {
	r, ok := s[repo]
	if !ok {
		return nil, fmt.Errorf("no repo %q", repo)
	}
	return r, nil
}

type env struct {
	t       *testing.T
	st      *store.Store
	srcs    srcs
	eng     *Engine
	clock   time.Time
	rebuilt []int64
}

var admin = gitx.Signature{Name: "admin", Email: "admin@sgit.local"}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st := must(store.Open(filepath.Join(dir, "sg.db")))
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, srcs: srcs{}, clock: time.Unix(1700000000, 0).UTC()}
	e.eng = New(Config{Root: filepath.Join(dir, "projects"), Store: st, Sources: e.srcs, Now: e.now,
		AfterRebuild: func(_ context.Context, id int64) error { e.rebuilt = append(e.rebuilt, id); return nil }})
	t.Cleanup(e.eng.CloseAll)
	return e
}

func (e *env) now() time.Time { e.clock = e.clock.Add(time.Second); return e.clock }

func (e *env) repo(name string) {
	ctx := context.Background()
	sig := admin
	sig.When = e.now()
	r := must(gitx.Init(ctx, filepath.Join(e.t.TempDir(), name+".git"), sig))
	e.t.Cleanup(func() { r.Close() })
	e.srcs[name] = r
	if err := e.st.CreateRepo(ctx, name, ""); err != nil {
		e.t.Fatal(err)
	}
}

// commit puts files on main of repo; seq records a land sequence row.
func (e *env) commit(repo, msg string, files map[string]string, seq bool) string {
	e.t.Helper()
	ctx := context.Background()
	r := e.srcs[repo]
	main := must(r.ReadRef(ctx, gitx.MainRef))
	var ch []gitx.Change
	for p, c := range files {
		op := gitx.OpAdd
		if c == "" {
			ch = append(ch, gitx.Change{Path: p, Op: gitx.OpDelete})
			continue
		}
		ch = append(ch, gitx.Change{Path: p, Op: op, BlobSHA: must(r.WriteBlob(ctx, []byte(c)))})
	}
	sig := gitx.Signature{Name: "alice", Email: "alice@sgit.local", When: e.now()}
	c := must(r.CommitTree(ctx, must(r.BuildTree(ctx, main, ch)), []string{main}, msg, sig, sig))
	if seq {
		if err := e.st.RecordLand(ctx, repo, c, sig.When); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) project(name string) *Handle {
	ctx := context.Background()
	p := must(e.st.CreateProject(ctx, name, e.now()))
	return must(e.eng.Get(ctx, p.ID))
}

func (e *env) mount(h *Handle, path, repo, folder string) string {
	e.t.Helper()
	sig := admin
	sig.When = e.now()
	return must(e.eng.Mount(context.Background(), h, store.Mount{Path: path, Repo: repo, Folder: folder}, sig))
}

func (e *env) subjects(h *Handle) []string {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.eng.CatchUp(ctx, h); err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, l := range must(h.Git.Log(ctx, "main", "", 1000)) {
		out = append(out, l.Subject)
	}
	return out
}

func (e *env) files(h *Handle) map[string]string {
	ctx := context.Background()
	out := map[string]string{}
	for p, fe := range must(h.Git.ListFiles(ctx, "main")) {
		out[p] = string(must(h.Git.ReadObject(ctx, fe.SHA)).Data)
	}
	return out
}

func TestFolderMountTranslatesOnlyTouchingCommits(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "add both", map[string]string{"src/a.go": "a1", "docs/x.md": "x1"}, true)
	e.commit("core", "docs only", map[string]string{"docs/x.md": "x2"}, true)
	c3 := e.commit("core", "src change", map[string]string{"src/a.go": "a2"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	if got := e.subjects(h); !reflect.DeepEqual(got, []string{"src change", "add both"}) {
		t.Fatalf("project log = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"lib/a.go": "a2"}) {
		t.Fatalf("project files = %v", f)
	}
	ctx := context.Background()
	head := must(h.Git.ReadCommit(ctx, must(h.Git.ReadRef(ctx, gitx.MainRef))))
	sc := must(e.srcs["core"].ReadCommit(ctx, c3))
	if head.AuthorRaw != sc.AuthorRaw || head.CommitterRaw != sc.CommitterRaw || head.Message != sc.Message {
		t.Fatalf("metadata not copied: %+v vs %+v", head, sc)
	}
	if head.SHA == c3 {
		t.Fatal("folder mount reproduced the source SHA")
	}
	if src := must(e.st.ProjectSource(ctx, h.ID, head.SHA, "core")); src != c3 {
		t.Fatalf("ProjectSource = %s, want %s", src, c3)
	}
}

func TestDotMountReproducesTheRepo(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	c1 := e.commit("core", "one", map[string]string{"a": "1"}, true)
	h := e.project("web")
	e.mount(h, ".", "core", ".")
	if got := e.subjects(h); !reflect.DeepEqual(got, []string{"one", "Initial commit"}) {
		t.Fatalf("log = %v", got)
	}
	if head := must(h.Git.ReadRef(context.Background(), gitx.MainRef)); head != c1 {
		t.Fatalf(". mount head %s != source %s (identical history must give identical SHAs)", head, c1)
	}
}

func TestTwoReposInterleaveByLandSequence(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	h := e.project("web")
	e.mount(h, "x", "x", ".")
	e.mount(h, "y", "y", ".")
	e.commit("x", "x1", map[string]string{"f": "1"}, true)
	e.commit("y", "y1", map[string]string{"g": "1"}, true)
	e.commit("x", "x2", map[string]string{"f": "2"}, true)
	got := e.subjects(h)
	if !reflect.DeepEqual(got[:3], []string{"x2", "y1", "x1"}) || got[3] != "Project layout: added `y/`" {
		t.Fatalf("log = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"x/f": "2", "y/g": "1"}) {
		t.Fatalf("files = %v", f)
	}
}

func TestSequenceBeatsCommitterDate(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	h := e.project("web")
	e.mount(h, "x", "x", ".")
	e.mount(h, "y", "y", ".")
	// y1 is committed (dated) first but landed (sequenced) second.
	ctx := context.Background()
	yc := e.commit("y", "y1", map[string]string{"g": "1"}, false)
	e.commit("x", "x1", map[string]string{"f": "1"}, true)
	if err := e.st.RecordLand(ctx, "y", yc, e.now()); err != nil {
		t.Fatal(err)
	}
	if got := e.subjects(h); !reflect.DeepEqual(got[:2], []string{"y1", "x1"}) {
		t.Fatalf("log = %v, want y1 after x1", got)
	}
}

func TestUnsequencedCommitsOrderByDateThenRepo(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	h := e.project("web")
	e.mount(h, "x", "x", ".")
	e.mount(h, "y", "y", ".")
	e.commit("y", "y1", map[string]string{"g": "1"}, false)
	e.commit("x", "x1", map[string]string{"f": "1"}, false)
	if got := e.subjects(h); !reflect.DeepEqual(got[:2], []string{"x1", "y1"}) {
		t.Fatalf("log = %v, want y1 then x1 by date", got)
	}
}

func TestForwardOnlyLayout(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "docs history", map[string]string{"docs/old.md": "o"}, true)
	e.commit("core", "src", map[string]string{"src/a": "a"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	e.mount(h, "docs", "core", "docs")
	got := e.subjects(h)
	if !reflect.DeepEqual(got, []string{"Project layout: added `docs/`", "src"}) {
		t.Fatalf("log after add = %v (older docs history must not be imported)", got)
	}
	if f := e.files(h); f["docs/old.md"] != "o" || f["lib/a"] != "a" {
		t.Fatalf("files = %v", f)
	}
	sig := admin
	sig.When = e.now()
	if _, err := e.eng.Unmount(context.Background(), h, "lib", sig); err != nil {
		t.Fatal(err)
	}
	if got := e.subjects(h); got[0] != "Project layout: removed `lib/`" {
		t.Fatalf("log after remove = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"docs/old.md": "o"}) {
		t.Fatalf("files after remove = %v", f)
	}
	ctx := context.Background()
	head := must(h.Git.ReadCommit(ctx, must(h.Git.ReadRef(ctx, gitx.MainRef))))
	if !strings.HasPrefix(head.AuthorRaw, "admin <admin@sgit.local> ") || strings.Contains(head.Message, "core") {
		t.Fatalf("layout commit = %+v", head)
	}
}

func TestMountValidation(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	ctx := context.Background()
	for _, m := range []store.Mount{
		{Path: "lib/sub", Repo: "core", Folder: "x"},
		{Path: ".", Repo: "core", Folder: "."},
		{Path: "lib", Repo: "core", Folder: "other"},
	} {
		if _, err := e.eng.Mount(ctx, h, m, admin); err == nil {
			t.Errorf("Mount(%+v) accepted", m)
		}
	}
	before := e.subjects(h)
	if _, err := e.eng.Mount(ctx, h, store.Mount{Path: "lib", Repo: "core", Folder: "src", ReadOnly: true}, admin); err != nil {
		t.Fatal(err)
	}
	if ms := must(e.st.Mounts(ctx, h.ID)); !ms[0].ReadOnly {
		t.Fatal("read-only flag not set")
	}
	if after := e.subjects(h); !reflect.DeepEqual(before, after) {
		t.Fatal("changing read-only created a commit")
	}
	if _, err := e.eng.Unmount(ctx, h, "nope", admin); err == nil {
		t.Fatal("unmount of a missing path accepted")
	}
}

func TestMissingFolderAppearsLater(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	h := e.project("web")
	e.mount(h, "lib", "core", "later")
	if got := e.subjects(h); !reflect.DeepEqual(got, []string{"Project layout: added `lib/`"}) {
		t.Fatalf("log = %v", got)
	}
	e.commit("core", "create later", map[string]string{"later/f": "1"}, true)
	if got := e.subjects(h); got[0] != "create later" { // catches up
		t.Fatalf("log after the folder appeared = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"lib/f": "1"}) {
		t.Fatalf("files = %v", f)
	}
}

func TestCatchUpWithoutMovesWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "one", map[string]string{"src/a": "1"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	ctx := context.Background()
	h1, s1 := must2(e.st.ProjectHead(ctx, h.ID))
	e.commit("core", "unrelated", map[string]string{"docs/z": "1"}, true)
	head := must(e.eng.CatchUp(ctx, h))
	h2, s2 := must2(e.st.ProjectHead(ctx, h.ID))
	if head != h1 || h2 != h1 || s2 != s1 {
		t.Fatalf("unrelated commit changed the project: %s/%d -> %s/%d", h1, s1, h2, s2)
	}
	cur := must(e.st.ProjectCursors(ctx, h.ID))
	if cur["core"] != must(e.srcs["core"].ReadRef(ctx, gitx.MainRef)) {
		t.Fatal("cursor did not advance past the skipped commit")
	}
}

func TestRebuildGivesSameSHAs(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	e.commit("x", "x0", map[string]string{"src/a": "1", "other/z": "z"}, true)
	h := e.project("web")
	e.mount(h, "a", "x", "src")
	e.mount(h, "b", "y", ".")
	e.commit("y", "y1", map[string]string{"g": "1"}, true)
	e.commit("x", "x1", map[string]string{"src/a": "2"}, true)
	sig := admin
	sig.When = e.now()
	if _, err := e.eng.Unmount(context.Background(), h, "b", sig); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want := must(h.Git.Log(ctx, "main", "", 100))
	e.eng.CloseAll()
	if err := os.RemoveAll(e.eng.Path(h.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := must(e.eng.Get(ctx, h.ID))
	got := must(h2.Git.Log(ctx, "main", "", 100))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt log differs:\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(e.rebuilt, []int64{h.ID}) {
		t.Fatalf("AfterRebuild calls = %v", e.rebuilt)
	}
}

func TestProjectRepoHoldsOnlyMountedObjects(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	src := e.commit("core", "both", map[string]string{"pub/a": "public", "secret/k": "top secret"}, true)
	h := e.project("web")
	e.mount(h, "pub", "core", "pub")
	ctx := context.Background()
	secretTree := must(e.srcs["core"].Subtrees(ctx, []string{src}, "secret"))[0]
	secretBlob := must(e.srcs["core"].WriteBlob(ctx, []byte("top secret"))) // same SHA as committed
	out, err := exec.Command("git", "--git-dir="+e.eng.Path(h.ID), "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{src, secretTree, secretBlob} {
		if strings.Contains(string(out), bad) {
			t.Fatalf("project repo holds foreign object %s", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(e.eng.Path(h.ID), "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("project repo has alternates: %v", err)
	}
}

func TestOverlappingRecordedMountsAreRefused(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	ctx := context.Background()
	// Bypass Mount's check, as a bad admin-layer write would.
	bad := store.Mount{Path: "lib/sub", Repo: "core", Folder: "docs", AddedAt: e.now()}
	if err := e.st.RecordProjectCommits(ctx, h.ID, nil, nil, &store.MountChange{Add: &bad}); err != nil {
		t.Fatal(err)
	}
	e.commit("core", "both", map[string]string{"src/a": "1", "docs/b": "2"}, true)
	if _, err := e.eng.CatchUp(ctx, h); !errors.Is(err, gitx.ErrInvalid) {
		t.Fatalf("CatchUp with overlapping mounts = %v, want ErrInvalid", err)
	}
	if _, err := e.eng.Unmount(ctx, h, "nope", admin); !errors.Is(err, gitx.ErrInvalid) {
		t.Fatalf("Unmount with overlapping mounts = %v, want ErrInvalid", err)
	}
}

// failing fails the n-th Source call (1-based) and passes the others through.
type failing struct {
	srcs  srcs
	n     int
	calls int
}

func (f *failing) Source(ctx context.Context, repo string) (*gitx.Repo, error) {
	f.calls++
	if f.calls == f.n {
		return nil, fmt.Errorf("injected failure")
	}
	return f.srcs.Source(ctx, repo)
}

func TestFailedFirstMountLeavesNoState(t *testing.T) {
	// Call 1 fails the import catch-up; call 2 fails the layout commit after
	// the catch-up recorded a cursor.
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			e := newEnv(t)
			e.repo("core")
			src := &failing{srcs: e.srcs, n: n}
			e.eng = New(Config{Root: filepath.Join(t.TempDir(), "projects"), Store: e.st, Sources: src, Now: e.now})
			t.Cleanup(e.eng.CloseAll)
			h := e.project("web")
			ctx := context.Background()
			m := store.Mount{Path: "lib", Repo: "core", Folder: "later"}
			if _, err := e.eng.Mount(ctx, h, m, admin); err == nil {
				t.Fatal("Mount with an injected failure succeeded")
			}
			if ms := must(e.st.Mounts(ctx, h.ID)); len(ms) != 0 {
				t.Fatalf("mounts after a failed first mount = %+v", ms)
			}
			if cur := must(e.st.ProjectCursors(ctx, h.ID)); len(cur) != 0 {
				t.Fatalf("cursors after a failed first mount = %v", cur)
			}
			if head, _ := must2(e.st.ProjectHead(ctx, h.ID)); head != "" {
				t.Fatalf("head after a failed first mount = %s", head)
			}
			if _, err := h.Git.ReadRef(ctx, gitx.MainRef); !errors.Is(err, gitx.ErrNotFound) {
				t.Fatalf("project main after a failed first mount: %v", err)
			}
			e.mount(h, "lib", "core", "later")
			if got := e.subjects(h); !reflect.DeepEqual(got, []string{"Project layout: added `lib/`"}) {
				t.Fatalf("log after retry = %v", got)
			}
		})
	}
}

func TestFailedRebuildLeavesNoRepoAndRetries(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "one", map[string]string{"src/a": "1"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	ctx := context.Background()
	want := must(h.Git.ReadRef(ctx, gitx.MainRef))
	e.eng.CloseAll()
	dir := e.eng.Path(h.ID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	hookErr := true
	eng := New(Config{Root: filepath.Dir(dir), Store: e.st, Sources: e.srcs, Now: e.now,
		AfterRebuild: func(context.Context, int64) error {
			if hookErr {
				return fmt.Errorf("hook failed")
			}
			e.rebuilt = append(e.rebuilt, h.ID)
			return nil
		}})
	t.Cleanup(eng.CloseAll)
	if _, err := eng.Get(ctx, h.ID); err == nil {
		t.Fatal("Get with a failing AfterRebuild succeeded")
	}
	for _, d := range []string{dir, dir + ".rebuild"} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s left behind after a failed rebuild: %v", filepath.Base(d), err)
		}
	}
	hookErr = false
	h2 := must(eng.Get(ctx, h.ID))
	if got := must(h2.Git.ReadRef(ctx, gitx.MainRef)); got != want || !reflect.DeepEqual(e.rebuilt, []int64{h.ID}) {
		t.Fatalf("retried rebuild: head %s (want %s), hook calls %v", got, want, e.rebuilt)
	}
}

func TestRebuildDoesNotBlockOtherProjects(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "one", map[string]string{"src/a": "1"}, true)
	slow, other := e.project("slow"), e.project("other")
	e.mount(slow, "lib", "core", "src")
	e.mount(other, "lib", "core", "src")
	e.eng.CloseAll()
	if err := os.RemoveAll(e.eng.Path(slow.ID)); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	eng := New(Config{Root: filepath.Dir(e.eng.Path(slow.ID)), Store: e.st, Sources: e.srcs, Now: e.now,
		AfterRebuild: func(context.Context, int64) error { close(entered); <-release; return nil }})
	t.Cleanup(eng.CloseAll)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { _, err := eng.Get(ctx, slow.ID); done <- err }()
	<-entered
	got := make(chan error, 1)
	go func() { _, err := eng.Get(ctx, other.ID); got <- err }()
	select { // must not wait for the rebuild
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("Get of another project waited for a rebuild")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
