package wc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/objfmt"
)

// track writes content at p, records it in the index as of Base, and ages the
// file so it is outside the racy window.
func track(t *testing.T, w *WC, p, content, mode string) {
	t.Helper()
	if err := WriteLocal(w.Root, p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if mode != "120000" {
		if err := os.Chtimes(w.Abs(p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	e, err := StatEntry(w.Root, p, mode, objfmt.BlobSHA([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	w.Index[p] = e
}

func newWC(t *testing.T) *WC {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	return &WC{Root: root, Config: Config{Server: "s:1", Project: 1, Name: "web"}, Base: "b", Index: map[string]Entry{}}
}

func kinds(changes []Change) map[string]Kind {
	m := map[string]Kind{}
	for _, c := range changes {
		m[c.Path] = c.Kind
	}
	return m
}

func TestStatusBasics(t *testing.T) {
	w := newWC(t)
	track(t, w, "same.txt", "same\n", "100644")
	track(t, w, "mod.txt", "old\n", "100644")
	track(t, w, "gone.txt", "bye\n", "100644")
	track(t, w, "run.sh", "#!/bin/sh\n", "100644")
	_ = os.MkdirAll(filepath.Join(w.Root, ".sg"), 0o755)
	_ = os.WriteFile(filepath.Join(w.Root, ".sg", "index"), []byte("meta"), 0o644)
	_ = WriteLocal(w.Root, "mod.txt", []byte("new content\n"), "100644")
	_ = os.Remove(w.Abs("gone.txt"))
	_ = os.Chmod(w.Abs("run.sh"), 0o755)
	_ = WriteLocal(w.Root, "dir/new.txt", []byte("fresh\n"), "100644")
	_ = WriteLocal(w.Root, ".gitignore", []byte("*.log\nbuild/\n"), "100644")
	_ = WriteLocal(w.Root, "debug.log", []byte("noise"), "100644")
	_ = WriteLocal(w.Root, "build/out.bin", []byte("bin"), "100644")

	changes, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(changes)
	want := map[string]Kind{"mod.txt": Modified, "gone.txt": Deleted, "run.sh": Modified, "dir/new.txt": Added, ".gitignore": Added}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v", changes)
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: got %v, want %v", p, got[p], k)
		}
	}
	for _, c := range changes {
		switch c.Path {
		case "mod.txt":
			if c.SHA != objfmt.BlobSHA([]byte("new content\n")) || c.Mode != "100644" {
				t.Errorf("mod.txt change = %+v", c)
			}
		case "run.sh":
			if c.Mode != "100755" {
				t.Errorf("run.sh mode change = %+v", c)
			}
		}
	}
	for i := 1; i < len(changes); i++ {
		if changes[i-1].Path >= changes[i].Path {
			t.Fatal("changes must be sorted by path")
		}
	}
}

func TestStatusTrackedFileIsNeverIgnored(t *testing.T) {
	w := newWC(t)
	track(t, w, "keep.log", "v1\n", "100644")
	_ = WriteLocal(w.Root, ".gitignore", []byte("*.log\n"), "100644")
	track(t, w, ".gitignore", "*.log\n", "100644")
	_ = WriteLocal(w.Root, "keep.log", []byte("v2\n"), "100644")
	changes, _ := w.Status()
	if k := kinds(changes); len(k) != 1 || k["keep.log"] != Modified {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusRespectsSparse(t *testing.T) {
	w := newWC(t)
	w.Config.Sparse = []string{"src"}
	track(t, w, "src/a.go", "a\n", "100644")
	_ = WriteLocal(w.Root, "src/b.go", []byte("b\n"), "100644")
	_ = WriteLocal(w.Root, "docs/outside.md", []byte("x\n"), "100644")
	changes, _ := w.Status()
	if k := kinds(changes); len(k) != 1 || k["src/b.go"] != Added {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusSymlink(t *testing.T) {
	w := newWC(t)
	track(t, w, "target.txt", "t\n", "100644")
	if err := os.Symlink("target.txt", w.Abs("link")); err != nil {
		t.Fatal(err)
	}
	changes, _ := w.Status()
	if len(changes) != 1 || changes[0].Path != "link" || changes[0].Mode != "120000" || changes[0].SHA != objfmt.BlobSHA([]byte("target.txt")) {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusDetectsRacyEdit(t *testing.T) {
	w := newWC(t)
	if err := WriteLocal(w.Root, "a.txt", []byte("aaaa"), "100644"); err != nil {
		t.Fatal(err)
	}
	e, _ := StatEntry(w.Root, "a.txt", "100644", objfmt.BlobSHA([]byte("aaaa")))
	w.Index["a.txt"] = e
	fi, _ := os.Lstat(w.Abs("a.txt"))
	// Same size, same mtime: only a re-hash can notice this edit.
	_ = WriteLocal(w.Root, "a.txt", []byte("bbbb"), "100644")
	_ = os.Chtimes(w.Abs("a.txt"), fi.ModTime(), fi.ModTime())
	changes, err := w.Status()
	if err != nil || len(changes) != 1 || changes[0].Kind != Modified {
		t.Fatalf("racy edit not detected: %+v, %v", changes, err)
	}
}

func TestStatusRefreshesStatCache(t *testing.T) {
	w := newWC(t)
	track(t, w, "a.txt", "abcd", "100644")
	e := w.Index["a.txt"]
	e.Size, e.MtimeNs = -1, 0
	w.Index["a.txt"] = e
	changes, _ := w.Status()
	if len(changes) != 0 {
		t.Fatalf("unchanged file reported: %+v", changes)
	}
	if got := w.Index["a.txt"]; got.Size != 4 || got.MtimeNs == 0 {
		t.Fatalf("stat cache not refreshed: %+v", got)
	}
}

func TestStatusFileReplacedByDirectory(t *testing.T) {
	w := newWC(t)
	track(t, w, "d", "file\n", "100644")
	_ = os.Remove(w.Abs("d"))
	_ = WriteLocal(w.Root, "d/inner.txt", []byte("in\n"), "100644")
	got := kinds(testStatus(t, w))
	if got["d"] != Deleted || got["d/inner.txt"] != Added || len(got) != 2 {
		t.Fatalf("changes = %v", got)
	}
}

func testStatus(t *testing.T, w *WC) []Change {
	t.Helper()
	c, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatusRacyAfterSettle(t *testing.T) {
	w := newWC(t)
	if err := WriteLocal(w.Root, "a.txt", []byte("aaaa"), "100644"); err != nil {
		t.Fatal(err)
	}
	e, err := StatEntry(w.Root, "a.txt", "100644", objfmt.BlobSHA([]byte("aaaa")))
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != -1 {
		t.Fatalf("fresh file cached size %d, want -1", e.Size)
	}
	// Same size edit, then age the file so it is settled when Status runs.
	if err := WriteLocal(w.Root, "a.txt", []byte("bbbb"), "100644"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(w.Abs("a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	// The cached stat matches the aged file: only Size -1 can save this edit.
	e.MtimeNs = old.UnixNano()
	w.Index["a.txt"] = e
	for i := 0; i < 2; i++ {
		if k := kinds(testStatus(t, w)); len(k) != 1 || k["a.txt"] != Modified {
			t.Fatalf("run %d: racy edit missed: %v", i, k)
		}
	}
}

func TestStatusRefreshesSettledFileAfterFreshEntry(t *testing.T) {
	w := newWC(t)
	if err := WriteLocal(w.Root, "b.txt", []byte("xyz"), "100644"); err != nil {
		t.Fatal(err)
	}
	e, err := StatEntry(w.Root, "b.txt", "100644", objfmt.BlobSHA([]byte("xyz")))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(w.Abs("b.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	w.Index["b.txt"] = e
	if c := testStatus(t, w); len(c) != 0 {
		t.Fatalf("unchanged file reported: %+v", c)
	}
	if got := w.Index["b.txt"]; got.Size != 3 {
		t.Fatalf("stat cache not refreshed: %+v", got)
	}
}

func TestStatusSkipsInvalidRepoPaths(t *testing.T) {
	w := newWC(t)
	if err := os.WriteFile(filepath.Join(w.Root, `weird\name.txt`), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = WriteLocal(w.Root, "new.txt", []byte("n\n"), "100644")
	changes, err := w.Status()
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if k := kinds(changes); len(k) != 1 || k["new.txt"] != Added {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusTrackedFileUnderSymlinkedDirIsDeleted(t *testing.T) {
	w := newWC(t)
	track(t, w, "lib/x", "x\n", "100644")
	track(t, w, "keep.txt", "k\n", "100644")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(w.Abs("lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, w.Abs("lib")); err != nil {
		t.Fatal(err)
	}
	got := kinds(testStatus(t, w))
	if len(got) != 2 || got["lib/x"] != Deleted || got["lib"] != Added {
		t.Fatalf("status = %v", got)
	}
}

func TestStatusSkipsSgTempFiles(t *testing.T) {
	w := newWC(t)
	track(t, w, "keep.txt", "k\n", "100644")
	for _, p := range []string{".keep.txt.tmp-123456", "d/.x.go.tmp-9", "link.sg-tmp-link"} {
		if err := os.MkdirAll(filepath.Dir(w.Abs(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(w.Abs(p), []byte("tmp"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(w.Abs(".env"), []byte("real"), 0o644)
	_ = os.WriteFile(w.Abs("a.tmp-1"), []byte("real"), 0o644)
	got := kinds(testStatus(t, w))
	if len(got) != 2 || got[".env"] != Added || got["a.tmp-1"] != Added {
		t.Fatalf("status = %v", got)
	}
}

func TestStatusNestedGitignore(t *testing.T) {
	w := newWC(t)
	_ = WriteLocal(w.Root, ".gitignore", []byte("*.log\n"), "100644")
	_ = WriteLocal(w.Root, "sub/.gitignore", []byte("*.tmp\nout/\n!keep.log\n"), "100644")
	_ = WriteLocal(w.Root, "sub/a.tmp", []byte("t"), "100644")
	_ = WriteLocal(w.Root, "sub/s.txt", []byte("s"), "100644")
	_ = WriteLocal(w.Root, "sub/out/x", []byte("x"), "100644")
	_ = WriteLocal(w.Root, "sub/deep/b.tmp", []byte("t"), "100644")
	_ = WriteLocal(w.Root, "sub/debug.log", []byte("l"), "100644")
	_ = WriteLocal(w.Root, "sub/keep.log", []byte("k"), "100644")
	_ = WriteLocal(w.Root, "top.tmp", []byte("t"), "100644")
	changes, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(changes)
	want := map[string]Kind{".gitignore": Added, "sub/.gitignore": Added, "sub/s.txt": Added, "sub/keep.log": Added, "top.tmp": Added}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A .gitignore that is not a regular file is not a rule file (as in git), and
// must not break status.
func TestStatusGitignoreNotAFile(t *testing.T) {
	w := newWC(t)
	_ = os.MkdirAll(filepath.Join(w.Root, ".gitignore"), 0o755)
	_ = WriteLocal(w.Root, ".gitignore/inner", []byte("*.txt\n"), "100644")
	_ = WriteLocal(w.Root, "sub/real", []byte("*.tmp\n"), "100644")
	_ = os.Symlink("real", filepath.Join(w.Root, "sub", ".gitignore"))
	_ = WriteLocal(w.Root, "sub/a.tmp", []byte("t"), "100644")
	_ = WriteLocal(w.Root, "a.txt", []byte("a"), "100644")
	changes, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(changes)
	for _, p := range []string{".gitignore/inner", "sub/real", "sub/.gitignore", "sub/a.tmp", "a.txt"} {
		if got[p] != Added {
			t.Fatalf("%s not added: %v", p, got)
		}
	}
}

// Nested repositories and working copies are not part of this one, and names
// the server cannot store are reported as skipped instead of breaking commit.
func TestStatusSkipsNestedReposAndUncommittableNames(t *testing.T) {
	w := newWC(t)
	_ = WriteLocal(w.Root, "vendor/lib/.git/HEAD", []byte("ref"), "100644")
	_ = WriteLocal(w.Root, "vendor/lib/v.txt", []byte("v"), "100644")
	_ = WriteLocal(w.Root, "nested/.sg/base", []byte("b"), "100644")
	_ = WriteLocal(w.Root, "sub/.GIT/x", []byte("x"), "100644")
	_ = WriteLocal(w.Root, "new\nline", []byte("n"), "100644")
	_ = WriteLocal(w.Root, "cr\r.txt", []byte("n"), "100644")
	_ = WriteLocal(w.Root, "bad\xff.txt", []byte("n"), "100644")
	_ = WriteLocal(w.Root, "ok.txt", []byte("ok"), "100644")
	changes, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(changes)
	if len(got) != 2 || got["vendor/lib/v.txt"] != Added || got["ok.txt"] != Added {
		t.Fatalf("changes = %v", got)
	}
	want := []string{"bad\xff.txt", "cr\r.txt", "new\nline"}
	if len(w.Skipped) != len(want) {
		t.Fatalf("skipped = %q, want %q", w.Skipped, want)
	}
	for i := range want {
		if w.Skipped[i] != want[i] {
			t.Fatalf("skipped = %q, want %q", w.Skipped, want)
		}
	}
}

// Names git treats as aliases of .git, and backslashes, are listed as skipped
// like other names the server refuses (repopath.Check), never silently
// dropped or sent.
func TestStatusSkipsGitAliasNames(t *testing.T) {
	w := newWC(t)
	_ = WriteLocal(w.Root, "lib/GIT~1/a.txt", []byte("a"), "100644")
	_ = WriteLocal(w.Root, "lib/.git./b.txt", []byte("b"), "100644")
	_ = WriteLocal(w.Root, "lib/.git /c.txt", []byte("c"), "100644")
	_ = os.WriteFile(filepath.Join(w.Root, "back\\slash"), []byte("d"), 0o644)
	_ = WriteLocal(w.Root, "lib/.gitx", []byte("e"), "100644")
	_ = os.Symlink("x", filepath.Join(w.Root, ".gitmodules"))
	changes, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(changes)
	if len(got) != 1 || got["lib/.gitx"] != Added {
		t.Fatalf("changes = %v", got)
	}
	want := []string{".gitmodules", "back\\slash", "lib/.git /c.txt", "lib/.git./b.txt", "lib/GIT~1/a.txt"}
	if strings.Join(w.Skipped, "|") != strings.Join(want, "|") {
		t.Fatalf("skipped = %q, want %q", w.Skipped, want)
	}
}
