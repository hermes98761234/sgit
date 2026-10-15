package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogBlameCatFromSubdirectory(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "src", "x.go"), "X\nmore\n")
	h.ok(w, "commit", "-m", "extend x", "--land")
	src := filepath.Join(w, "src")

	var log []struct {
		Sha     string `json:"sha"`
		Author  string `json:"author"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(unwrap(t, h.ok(src, "log", "x.go", "--json"), "commits"), &log); err != nil || len(log) != 2 || log[0].Subject != "extend x" || log[1].Subject != "seed" {
		t.Fatalf("log x.go = %+v (%v)", log, err)
	}
	var blame []struct {
		Sha  string `json:"sha"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(unwrap(t, h.ok(src, "blame", "x.go", "--json"), "lines"), &blame); err != nil || len(blame) != 2 || blame[1].Text != "more" || blame[1].Sha != log[0].Sha {
		t.Fatalf("blame = %+v (%v)", blame, err)
	}
	if got := h.ok(src, "cat", "main:src/x.go"); got != "X\nmore\n" {
		t.Fatalf("cat = %q", got)
	}
	outside := t.TempDir()
	if err := json.Unmarshal(unwrap(t, h.ok(outside, "log", "--project", "web", "--json"), "commits"), &log); err != nil || len(log) != 3 {
		t.Fatalf("log --project = %+v (%v)", log, err)
	}
	if got := h.ok(outside, "cat", "--project", "web", ":a.txt"); got != "A\n" {
		t.Fatalf("cat main default = %q", got)
	}
	if code, _, _ := h.run(outside, "cat", "--project", "web", "main:nope.txt"); code != 1 {
		t.Fatalf("cat missing: exit %d", code)
	}
	if out := h.ok(w, "log", "-n", "1"); !strings.Contains(out, "extend x") || strings.Contains(out, "seed") {
		t.Fatalf("log -n 1 = %q", out)
	}
}

func TestSparseAddRm(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "src")
	w := filepath.Join(root, "web")
	var list []string
	if err := json.Unmarshal(unwrap(t, h.ok(w, "sparse", "list", "--json"), "sparse"), &list); err != nil || len(list) != 1 || list[0] != "src" {
		t.Fatalf("list = %v (%v)", list, err)
	}
	if err := json.Unmarshal(unwrap(t, h.ok(w, "sparse", "--json", "list"), "sparse"), &list); err != nil || len(list) != 1 || list[0] != "src" {
		t.Fatalf("sparse --json list = %v (%v)", list, err)
	}
	h.ok(w, "sparse", "add", "docs")
	if readFile(t, filepath.Join(w, "docs", "d.md")) != "D\n" {
		t.Fatal("sparse add did not fetch docs")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status after add = %v", m)
	}
	writeFile(t, filepath.Join(w, "docs", "d.md"), "edited\n")
	if code, _, _ := h.run(w, "sparse", "rm", "docs"); code != 1 {
		t.Fatalf("rm with local changes: exit %d", code)
	}
	writeFile(t, filepath.Join(w, "docs", "d.md"), "D\n")
	h.ok(w, "sparse", "rm", "docs")
	if _, err := os.Stat(filepath.Join(w, "docs")); err == nil {
		t.Fatal("sparse rm must remove the files")
	}
	if code, _, _ := h.run(w, "sparse", "rm", "src"); code != 1 {
		t.Fatalf("removing the last sparse path: exit %d", code)
	}
	if code, _, _ := h.run(w, "sparse", "rm", "nope"); code != 1 {
		t.Fatalf("removing a non-sparse path: exit %d", code)
	}
	full := h.checkout(t.TempDir())
	if code, _, _ := h.run(full, "sparse", "add", "src"); code != 1 {
		t.Fatalf("sparse add on a full checkout: exit %d", code)
	}
}

func TestNotLoggedInAndReadOnly(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	h.cfg["nobody"] = t.TempDir()
	if code, _, errs := h.runAs("nobody", t.TempDir(), "log", "--project", "web"); code != 2 || !strings.Contains(errs, "not logged in") {
		t.Fatalf("not logged in: exit %d %q", code, errs)
	}
	reader := h.env.NewUser("bob-reader", "web", "read")
	h.loginAs("bob-reader", reader)
	root := t.TempDir()
	h.okAs("bob-reader", root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "nope\n")
	if code, _, _ := h.runAs("bob-reader", w, "commit", "-m", "x"); code != 2 {
		t.Fatalf("read-only commit: exit %d, want 2", code)
	}
}

func TestSparseAddParentKeepsLocalEdits(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a/b/x.txt": "X\n", "a/c.txt": "C\n"})
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "a/b")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a", "b", "x.txt"), "EDITED\n")
	h.ok(w, "sparse", "add", "a")
	if got := readFile(t, filepath.Join(w, "a", "b", "x.txt")); got != "EDITED\n" {
		t.Fatalf("local edit lost: %q", got)
	}
	if readFile(t, filepath.Join(w, "a", "c.txt")) != "C\n" {
		t.Fatal("a/c.txt not fetched")
	}
	var list []string
	if err := json.Unmarshal(unwrap(t, h.ok(w, "sparse", "list", "--json"), "sparse"), &list); err != nil || len(list) != 1 || list[0] != "a" {
		t.Fatalf("list = %v (%v)", list, err)
	}
	if _, m := h.status(w); m["a/b/x.txt"] != "modified" {
		t.Fatalf("status = %v", m)
	}
}

func TestSparseAddBlockedByUntrackedFile(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "src")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "docs", "d.md"), "mine\n")
	if code, _, _ := h.run(w, "sparse", "add", "docs"); code != 1 {
		t.Fatalf("sparse add over local file: exit %d", code)
	}
	if got := readFile(t, filepath.Join(w, "docs", "d.md")); got != "mine\n" {
		t.Fatalf("file clobbered: %q", got)
	}
	var list []string
	if err := json.Unmarshal(unwrap(t, h.ok(w, "sparse", "list", "--json"), "sparse"), &list); err != nil || len(list) != 1 || list[0] != "src" {
		t.Fatalf("list = %v (%v)", list, err)
	}
}

func TestSparseAddBlockedByFileAtAncestor(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "src")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "docs"), "mine\n")
	if code, _, _ := h.run(w, "sparse", "add", "docs"); code != 1 {
		t.Fatalf("sparse add over file at ancestor: exit %d", code)
	}
	if got := readFile(t, filepath.Join(w, "docs")); got != "mine\n" {
		t.Fatalf("file clobbered: %q", got)
	}
	var list []string
	if err := json.Unmarshal(unwrap(t, h.ok(w, "sparse", "list", "--json"), "sparse"), &list); err != nil || len(list) != 1 || list[0] != "src" {
		t.Fatalf("list = %v (%v)", list, err)
	}
}

var libFiles = map[string]string{"src/x.go": "X\n", "lib/l.txt": "L\n", "lib/sub/m.txt": "M\n"}

func TestSparseRmDoesNotFollowSymlinkedDir(t *testing.T) {
	h := newHarness(t)
	h.seed(libFiles)
	w := h.checkout(t.TempDir(), "--path", "src", "--path", "lib")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "l.txt"), "L\n")
	writeFile(t, filepath.Join(outside, "sub", "m.txt"), "M\n")
	if err := os.RemoveAll(filepath.Join(w, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w, "lib")); err != nil {
		t.Fatal(err)
	}
	code, _, errs := h.run(w, "sparse", "rm", "lib")
	if readFile(t, filepath.Join(outside, "l.txt")) != "L\n" || readFile(t, filepath.Join(outside, "sub", "m.txt")) != "M\n" {
		t.Fatalf("sparse rm (exit %d, %q) deleted files outside the working copy", code, errs)
	}
	if code == 0 {
		if _, err := os.Lstat(filepath.Join(w, "lib")); err == nil {
			t.Fatal("sparse rm succeeded but left lib in place")
		}
	}
	// Status must cope with the symlinked directory: its tracked files are gone.
	if _, m := h.status(w); code != 0 && (m["lib/l.txt"] != "deleted" || m["lib/sub/m.txt"] != "deleted") {
		t.Fatalf("status = %v", m)
	}
}

func TestSparseRmNarrowsBeforeDeleting(t *testing.T) {
	h := newHarness(t)
	h.seed(libFiles)
	w := h.checkout(t.TempDir(), "--path", "src", "--path", "lib")
	sub := filepath.Join(w, "lib", "sub")
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if code, _, _ := h.run(w, "sparse", "rm", "lib"); code != 1 {
		t.Fatalf("sparse rm with an undeletable file: exit %d", code)
	}
	var list []string
	if err := json.Unmarshal(unwrap(t, h.ok(w, "sparse", "list", "--json"), "sparse"), &list); err != nil || len(list) != 1 || list[0] != "src" {
		t.Fatalf("sparse set must be narrowed first: %v (%v)", list, err)
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status after a partial sparse rm = %v", m)
	}
}

func TestLogAttachedCount(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "a\n")
	h.ok(w, "commit", "-m", "second", "--land")
	for _, args := range [][]string{{"-n1"}, {"-1"}, {"-n", "1"}} {
		var log []struct{ Subject string }
		out := h.ok(w, append([]string{"log", "--json"}, args...)...)
		if err := json.Unmarshal(unwrap(t, out, "commits"), &log); err != nil || len(log) != 1 || log[0].Subject != "second" {
			t.Fatalf("log %v = %+v (%v)", args, log, err)
		}
	}
}
