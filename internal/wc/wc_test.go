package wc

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreateSaveOpen(t *testing.T) {
	root := t.TempDir()
	w, err := Create(root, Config{Server: "127.0.0.1:7419", Project: 1, Name: "web", Sparse: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrNotWorkingCopy) {
		t.Fatalf("open before first save: want ErrNotWorkingCopy, got %v", err)
	}
	w.Base = "1111111111111111111111111111111111111111"
	w.Draft = "abcdefgh"
	w.Conflicts = []string{"src/b.go", "src/a.go"}
	w.Index["src/a.go"] = Entry{Path: "src/a.go", Mode: "100644", SHA: "2222222222222222222222222222222222222222", Size: 3, MtimeNs: 42}
	w.Index["src/b.go"] = Entry{Path: "src/b.go", Mode: "100755", SHA: "3333333333333333333333333333333333333333", Size: -1}
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Open(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != w.Root || got.Base != w.Base || got.Draft != "abcdefgh" || !reflect.DeepEqual(got.Config, w.Config) ||
		!reflect.DeepEqual(got.Index, w.Index) || !reflect.DeepEqual(got.Conflicts, []string{"src/a.go", "src/b.go"}) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, w)
	}
	got.Draft, got.Conflicts = "", nil
	if err := got.Save(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"draft", "conflicts"} {
		if _, err := os.Stat(filepath.Join(root, ".sg", f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf(".sg/%s must be removed when empty", f)
		}
	}
	again, _ := Open(root)
	if again.Draft != "" || len(again.Conflicts) != 0 {
		t.Fatalf("cleared fields came back: %+v", again)
	}
	if _, err := Create(root, Config{}); err == nil {
		t.Fatal("Create over a complete working copy must fail")
	}
	if got.SortedPaths()[0] != "src/a.go" {
		t.Fatalf("SortedPaths = %v", got.SortedPaths())
	}
}

func TestOpenOutside(t *testing.T) {
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotWorkingCopy) {
		t.Fatalf("want ErrNotWorkingCopy, got %v", err)
	}
}

func TestSparseHelpers(t *testing.T) {
	got, err := NormalizeSparse([]string{"src/", "./docs", "src/sub", "a-b", "a", "a/b"})
	if err != nil || !reflect.DeepEqual(got, []string{"a", "a-b", "docs", "src"}) {
		t.Fatalf("NormalizeSparse = %v, %v", got, err)
	}
	for _, bad := range []string{"", ".", "/abs", "..", "../x"} {
		if _, err := NormalizeSparse([]string{bad}); err == nil {
			t.Errorf("NormalizeSparse(%q) must fail", bad)
		}
	}
	sp := []string{"src", "docs/api"}
	cases := map[string]bool{"src/a.go": true, "src": true, "srcx/a": false, "docs/api/x.md": true, "docs/readme.md": false}
	for p, want := range cases {
		if InSparse(sp, p) != want {
			t.Errorf("InSparse(%q) != %v", p, want)
		}
	}
	if !InSparse(nil, "anything") {
		t.Error("empty sparse set means everything")
	}
	trees := map[string]bool{"": true, "docs": true, "docs/api": true, "docs/api/v1": true, "docs/other": false, "lib": false}
	for d, want := range trees {
		if TreeNeeded(sp, d) != want {
			t.Errorf("TreeNeeded(%q) != %v", d, want)
		}
	}
}

func TestRepoPath(t *testing.T) {
	root := t.TempDir()
	w, _ := Create(root, Config{Project: 1, Name: "web"})
	w.Base = "1111111111111111111111111111111111111111"
	_ = w.Save()
	w, _ = Open(root)
	sub := filepath.Join(w.Root, "src")
	_ = os.MkdirAll(sub, 0o755)
	cases := []struct{ cwd, arg, want string }{
		{w.Root, "a.txt", "a.txt"},
		{sub, "x.go", "src/x.go"},
		{sub, "../a.txt", "a.txt"},
		{sub, ".", "src"},
		{w.Root, ".", ""},
		{sub, filepath.Join(w.Root, "b.txt"), "b.txt"},
	}
	for _, c := range cases {
		if got, err := w.RepoPath(c.cwd, c.arg); err != nil || got != c.want {
			t.Errorf("RepoPath(%q, %q) = %q, %v; want %q", c.cwd, c.arg, got, err, c.want)
		}
	}
	if _, err := w.RepoPath(sub, "../../outside"); err == nil {
		t.Error("path outside the working copy must fail")
	}
}

func TestLockIsExclusive(t *testing.T) {
	w, err := Create(t.TempDir(), Config{Server: "s:1", Project: 1, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := w.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Lock(); err == nil || !strings.Contains(err.Error(), "another sg command is running") {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock2, err := w.Lock()
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	unlock2()
}

func TestSaveIndexWritesOnlyIndex(t *testing.T) {
	w, err := Create(t.TempDir(), Config{Server: "s:1", Project: 1, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	w.Base = "b1"
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	w.Base, w.Draft = "b2", "dddddddd"
	w.Index["a"] = Entry{Path: "a", Mode: "100644", SHA: "s", Size: 1, MtimeNs: 2}
	if err := w.SaveIndex(); err != nil {
		t.Fatal(err)
	}
	got, err := Open(w.Root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != "b1" || got.Draft != "" || got.Index["a"] != w.Index["a"] {
		t.Fatalf("after SaveIndex: base %q draft %q index %v", got.Base, got.Draft, got.Index)
	}
}

// Users only see projects and folders, never repos.
func TestValidPathErrorSaysPath(t *testing.T) {
	err := ValidPath("../x")
	if err == nil || err.Error() != `invalid path "../x"` {
		t.Fatalf("ValidPath(../x) = %v", err)
	}
}
