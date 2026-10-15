package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrackedReservedNameSkippedThenDeletable: a tracked file whose name the
// server cannot store is skipped (not a failed commit) when modified, and can
// be deleted through sg.
func TestTrackedReservedNameSkippedThenDeletable(t *testing.T) {
	h := newHarness(t)
	h.env.CreateRepoProject("web")
	h.env.CommitReservedName("web")
	root := t.TempDir()
	dir := filepath.Join(root, "w")
	h.ok(root, "checkout", "web", dir)
	if got := readFile(t, filepath.Join(dir, "GIT~1", "a.txt")); got != "a\n" {
		t.Fatalf("checked out GIT~1/a.txt = %q", got)
	}
	writeFile(t, filepath.Join(dir, "GIT~1", "a.txt"), "changed\n")
	writeFile(t, filepath.Join(dir, "ok.txt"), "ok2\n")
	if out := h.ok(dir, "status"); !strings.Contains(out, `skipped:  "GIT~1/a.txt"`) {
		t.Fatalf("status = %q", out)
	}
	code, out, errs := h.run(dir, "commit", "--land", "-m", "edit")
	if code != 0 || !strings.Contains(errs, `skipping "GIT~1/a.txt"`) {
		t.Fatalf("commit --land: exit %d\nstdout %s\nstderr %s", code, out, errs)
	}
	if f := h.env.Files("web", "main"); f["ok.txt"] != "ok2\n" || f["GIT~1/a.txt"] != "a\n" {
		t.Fatalf("after edit, main = %v", f)
	}
	if err := os.RemoveAll(filepath.Join(dir, "GIT~1")); err != nil {
		t.Fatal(err)
	}
	h.ok(dir, "commit", "--land", "-m", "drop")
	if f := h.env.Files("web", "main"); len(f) != 1 || f["ok.txt"] != "ok2\n" {
		t.Fatalf("after delete, main = %v", f)
	}
	if out := h.ok(dir, "status"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("status after delete = %q", out)
	}
}
