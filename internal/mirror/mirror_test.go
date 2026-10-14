package mirror_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/mirror"
	"github.com/hermes98761234/sgit/internal/store"
)

var sig = gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000000, 0)}

type fixture struct {
	t      *testing.T
	st     *store.Store
	root   string     // repos dir
	src    *gitx.Repo // the sgit repo "web"
	remote string     // bare repo standing in for GitHub
	url    string
}

func newFixture(t *testing.T, makeRemote bool) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st, root: filepath.Join(dir, "repos"), remote: filepath.Join(dir, "remote.git")}
	f.url = "file://" + f.remote
	src, err := gitx.Init(ctx, filepath.Join(f.root, "web.git"), sig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	f.src = src
	if err := st.CreateRepo(ctx, "web", f.url); err != nil {
		t.Fatal(err)
	}
	if makeRemote {
		f.makeRemote()
	}
	return f
}

func (f *fixture) makeRemote() {
	f.t.Helper()
	if err := gitx.InitBare(context.Background(), f.remote); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) start(backoffMax time.Duration) *mirror.Manager {
	f.t.Helper()
	m := mirror.New(mirror.Config{
		RepoDir:    func(name string) string { return filepath.Join(f.root, name+".git") },
		AllowLocal: true,
		BackoffMin: 5 * time.Millisecond,
		BackoffMax: backoffMax,
	}, f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(m.Close)
	return m
}

func (f *fixture) main() string {
	f.t.Helper()
	sha, err := f.src.ReadRef(context.Background(), gitx.MainRef)
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *fixture) remoteMain() string {
	f.t.Helper()
	sha, err := gitx.RemoteRef(context.Background(), f.url, gitx.MainRef, true)
	if err != nil {
		return "error: " + err.Error()
	}
	return sha
}

func (f *fixture) state() store.MirrorState {
	f.t.Helper()
	s, err := f.st.MirrorState(context.Background(), "web")
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// commit advances r's main by one empty-tree commit and returns it.
func commit(t *testing.T, r *gitx.Repo, msg string) string {
	t.Helper()
	ctx := context.Background()
	parent, err := r.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitTree(ctx, gitx.EmptyTree, []string{parent}, msg+"\n", sig, sig)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, gitx.MainRef, c, parent); err != nil {
		t.Fatal(err)
	}
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartCatchesUp(t *testing.T) {
	f := newFixture(t, true)
	f.start(50 * time.Millisecond) // no Notify: Start alone must queue the repo
	main := f.main()
	waitFor(t, "initial mirror", func() bool {
		s := f.state()
		return f.remoteMain() == main && s.LastMirrored == main && !s.Pending && s.LastError == ""
	})
}

func TestNotifyPushesNewCommits(t *testing.T) {
	f := newFixture(t, true)
	m := f.start(50 * time.Millisecond)
	first := f.main()
	waitFor(t, "initial mirror", func() bool { return f.state().LastMirrored == first })
	for i, msg := range []string{"two", "three"} {
		c := commit(t, f.src, msg)
		m.Notify("web")
		waitFor(t, "commit "+msg, func() bool { return f.remoteMain() == c && f.state().LastMirrored == c })
		if s := f.state(); s.Pending || s.LastError != "" || s.LastAttempt.IsZero() {
			t.Fatalf("after push %d: state %+v", i, s)
		}
	}
}

func TestRetriesUntilMirrorExists(t *testing.T) {
	f := newFixture(t, false) // the mirror does not exist yet
	f.start(20 * time.Millisecond)
	waitFor(t, "a recorded failure", func() bool {
		s := f.state()
		return s.LastError != "" && s.Pending && !s.LastAttempt.IsZero()
	})
	if s := f.state(); !strings.HasPrefix(s.LastError, "push failed: ") {
		t.Fatalf("LastError = %q, want a \"push failed: \" message", s.LastError)
	}
	f.makeRemote()
	main := f.main()
	waitFor(t, "recovery", func() bool {
		s := f.state()
		return f.remoteMain() == main && s.LastMirrored == main && s.LastError == "" && !s.Pending
	})
}

func TestLeaseMismatchIsNotOverwritten(t *testing.T) {
	f := newFixture(t, true)
	m := f.start(20 * time.Millisecond)
	first := f.main()
	waitFor(t, "initial mirror", func() bool { return f.state().LastMirrored == first })

	remote := gitx.Open(f.remote)
	defer remote.Close()
	stray := commit(t, remote, "made on the mirror")
	commit(t, f.src, "made in sgit")
	m.Notify("web")

	waitFor(t, "lease error", func() bool { return strings.Contains(f.state().LastError, "refusing to overwrite") })
	s := f.state()
	if want := "mirror main is no longer " + first + " (changed outside sgit); refusing to overwrite it"; s.LastError != want {
		t.Fatalf("LastError = %q, want %q", s.LastError, want)
	}
	if !s.Pending || s.LastMirrored != first {
		t.Fatalf("state = %+v, want pending with last_mirrored %s", s, first)
	}
	time.Sleep(100 * time.Millisecond) // several retries
	if got := f.remoteMain(); got != stray {
		t.Fatalf("mirror main = %s, want the stray commit %s kept", got, stray)
	}
}

func TestNonEmptyMirrorIsNotOverwritten(t *testing.T) {
	f := newFixture(t, false)
	other := gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1600000000, 0)}
	r, err := gitx.Init(context.Background(), f.remote, other) // a different initial commit
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := r.ReadRef(context.Background(), gitx.MainRef)
	r.Close()
	f.start(20 * time.Millisecond)
	want := "mirror already has a main branch that sgit did not push; refusing to overwrite it (a mirror must start empty)"
	waitFor(t, "non-empty error", func() bool { return f.state().LastError == want })
	if got := f.remoteMain(); got != theirs {
		t.Fatalf("mirror main = %s, want %s untouched", got, theirs)
	}
}

func TestAlreadyPushedIsRecorded(t *testing.T) {
	f := newFixture(t, true)
	main := f.main()
	// Simulate a crash after a successful push but before it was recorded.
	// Git treats an already-up-to-date ref as success before checking the
	// lease, so this goes through the normal path.
	if err := f.src.Push(context.Background(), f.url, main, "", true); err != nil {
		t.Fatal(err)
	}
	f.start(20 * time.Millisecond)
	waitFor(t, "recorded success", func() bool {
		s := f.state()
		return s.LastMirrored == main && s.LastError == "" && !s.Pending
	})
}

func TestUnknownAndUnmirroredReposAreIgnored(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	if err := f.st.CreateRepo(ctx, "plain", ""); err != nil {
		t.Fatal(err)
	}
	m := f.start(20 * time.Millisecond)
	m.Notify("plain")
	m.Notify("ghost") // not in the store at all
	main := f.main()
	waitFor(t, "web mirrored", func() bool { return f.state().LastMirrored == main })
	time.Sleep(50 * time.Millisecond)
	if s, _ := f.st.MirrorState(ctx, "plain"); !s.LastAttempt.IsZero() || s.Pending {
		t.Fatalf("repo without mirror_url was attempted: %+v", s)
	}
}

func TestCloseDuringBackoff(t *testing.T) {
	f := newFixture(t, false)
	m := f.start(time.Hour)
	waitFor(t, "a failure", func() bool { return f.state().LastError != "" })
	time.Sleep(50 * time.Millisecond) // let the backoff grow past a few retries
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while a worker was backing off")
	}
	m.Notify("web") // after Close: must neither panic nor start a worker
	m.Close()       // idempotent
}

func TestMissingRepoDirIsReported(t *testing.T) {
	f := newFixture(t, true)
	if err := os.RemoveAll(filepath.Join(f.root, "web.git")); err != nil {
		t.Fatal(err)
	}
	f.start(20 * time.Millisecond)
	waitFor(t, "check failure", func() bool {
		s := f.state()
		return strings.HasPrefix(s.LastError, "mirror check failed: ") && s.Pending
	})
}
