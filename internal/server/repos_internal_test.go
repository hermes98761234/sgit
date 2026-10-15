package server

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

// A delete the store refuses after the directory was moved (a project mounted
// the repo in between) puts the directory back and leaves the repo readable.
func TestRefusedDeleteKeepsRepoReadable(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(Config{DataDir: dir}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sig := gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Now()}
	main, err := srv.repos.create(ctx, "core", "", sig)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "web", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := store.Mount{Path: "lib", Repo: "core", Folder: "src", AddedAt: time.Now()}
	if err := st.RecordProjectCommits(ctx, p.ID, nil, nil, &store.MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.repos.deleteRepo(ctx, "core", filepath.Join(dir, "deleted"), time.Now()); !errors.Is(err, store.ErrMounted) {
		t.Fatalf("deleteRepo = %v, want ErrMounted", err)
	}
	h, err := srv.repos.get(ctx, "core")
	if err != nil {
		t.Fatal(err)
	}
	if typ, _, err := h.git.ObjectInfo(ctx, main); err != nil || typ != "commit" {
		t.Fatalf("ObjectInfo after refused delete = %q, %v", typ, err)
	}
}

// rollbackNew keeps the directory when the store still references the repo
// (a mount row), and removes it when the repo can be deleted.
func TestRollbackNewKeepsMountedRepo(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(Config{DataDir: dir}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sig := gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Now()}
	for _, name := range []string{"core", "tmp"} {
		if _, err := srv.repos.create(ctx, name, "", sig); err != nil {
			t.Fatal(err)
		}
	}
	p, err := st.CreateProject(ctx, "web", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := store.Mount{Path: ".", Repo: "core", Folder: ".", AddedAt: time.Now()}
	if err := st.RecordProjectCommits(ctx, p.ID, nil, nil, &store.MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	if err := srv.repos.rollbackNew("core", srv.repos.path("core")); !errors.Is(err, store.ErrMounted) {
		t.Fatalf("rollbackNew(mounted) = %v, want ErrMounted", err)
	}
	if _, err := os.Stat(srv.repos.path("core")); err != nil {
		t.Fatalf("mounted repo directory was removed: %v", err)
	}
	if err := srv.repos.rollbackNew("tmp", srv.repos.path("tmp")); err != nil {
		t.Fatalf("rollbackNew(free) = %v", err)
	}
	if _, err := os.Stat(srv.repos.path("tmp")); !os.IsNotExist(err) {
		t.Fatalf("free repo directory still exists: %v", err)
	}
}
