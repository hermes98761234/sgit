package server

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hermes98761234/sgit/internal/store"
)

func TestStaleIncomingIsCleanedOnStart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	leftover := filepath.Join(dir, "repos", ".incoming", "x-1.git")
	if err := os.MkdirAll(leftover, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "HEAD"), []byte("ref: refs/heads/main\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{DataDir: dir}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := os.Stat(filepath.Join(dir, "repos", ".incoming")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".incoming survived server start (stat err %v)", err)
	}
	// The name the crashed import was building is free.
	release, err := srv.repos.reserve(t.Context(), "x")
	if err != nil {
		t.Fatalf("reserve x: %v", err)
	}
	release()
}
