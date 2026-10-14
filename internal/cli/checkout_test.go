package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type statusJSON struct {
	Repo    string `json:"repo"`
	Base    string `json:"base"`
	Draft   string `json:"draft"`
	Changes []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"changes"`
	Conflicts []string `json:"conflicts"`
}

func (h *harness) status(dir string) (statusJSON, map[string]string) {
	h.t.Helper()
	var s statusJSON
	if err := json.Unmarshal([]byte(h.ok(dir, "status", "--json")), &s); err != nil {
		h.t.Fatal(err)
	}
	m := map[string]string{}
	for _, c := range s.Changes {
		m[c.Path] = c.Status
	}
	return s, m
}

var seedFiles = map[string]string{"a.txt": "A\n", "src/x.go": "X\n", "docs/d.md": "D\n"}

func TestCheckoutFullAndSparse(t *testing.T) {
	h := newHarness(t)
	main := h.seed(seedFiles)
	root := t.TempDir()
	var res struct {
		Repo   string `json:"repo"`
		Dir    string `json:"dir"`
		Commit string `json:"commit"`
		Files  int    `json:"files"`
	}
	if err := json.Unmarshal([]byte(h.ok(root, "checkout", "web", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Commit != main || res.Repo != "web" {
		t.Fatalf("checkout json = %+v", res)
	}
	w := filepath.Join(root, "web")
	if readFile(t, filepath.Join(w, "src", "x.go")) != "X\n" || strings.TrimSpace(readFile(t, filepath.Join(w, ".sg", "base"))) != main {
		t.Fatal("checkout content or base wrong")
	}
	if s, m := h.status(w); len(m) != 0 || s.Base != main {
		t.Fatalf("fresh checkout not clean: %+v", s)
	}

	h.ok(root, "checkout", "web", "part", "--path", "src")
	if readFile(t, filepath.Join(root, "part", "src", "x.go")) != "X\n" {
		t.Fatal("sparse file missing")
	}
	if _, err := os.Stat(filepath.Join(root, "part", "a.txt")); err == nil {
		t.Fatal("file outside the sparse set was checked out")
	}
	if code, _, _ := h.run(root, "checkout", "web", "part"); code != 1 {
		t.Fatalf("checkout over an existing working copy: exit %d", code)
	}
	if code, _, _ := h.run(root, "checkout", "nope"); code != 1 {
		t.Fatalf("unknown repo: exit %d", code)
	}
}

func TestCheckoutResumes(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "A\n")
	writeFile(t, filepath.Join(w, "src", "x.go"), "WRONG half-written\n")
	writeFile(t, filepath.Join(w, "extra.txt"), "mine\n")
	h.ok(root, "checkout", "web")
	if readFile(t, filepath.Join(w, "src", "x.go")) != "X\n" || readFile(t, filepath.Join(w, "docs", "d.md")) != "D\n" {
		t.Fatal("resumed checkout content wrong")
	}
	if readFile(t, filepath.Join(w, "extra.txt")) != "mine\n" {
		t.Fatal("untracked file must be left alone")
	}
	if _, m := h.status(w); len(m) != 1 || m["extra.txt"] != "added" {
		t.Fatalf("status after resume = %v", m)
	}
}

func TestStatusReportsChangesFromAnyDirectory(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(w, "new.txt"), "new\n")
	_ = os.Remove(filepath.Join(w, "docs", "d.md"))
	for _, dir := range []string{w, filepath.Join(w, "src")} {
		_, m := h.status(dir)
		if len(m) != 3 || m["a.txt"] != "modified" || m["new.txt"] != "added" || m["docs/d.md"] != "deleted" {
			t.Fatalf("status from %s = %v", dir, m)
		}
	}
	out := h.ok(w, "status")
	if !strings.Contains(out, "modified: a.txt") || !strings.Contains(out, "new.txt") {
		t.Fatalf("text status = %q", out)
	}
	if code, _, _ := h.run(t.TempDir(), "status"); code != 1 {
		t.Fatalf("status outside a working copy: exit %d", code)
	}
}

func TestStatusAndDiffShowModeChange(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	if err := os.Chmod(filepath.Join(w, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, m := h.status(w); m["a.txt"] != "modified" {
		t.Fatalf("chmod +x not detected: %v", m)
	}
	out := h.ok(w, "diff")
	if !strings.Contains(out, "old mode 100644") || !strings.Contains(out, "new mode 100755") {
		t.Fatalf("diff = %q", out)
	}
}

func TestDiff(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "B\n")
	writeFile(t, filepath.Join(w, "new.txt"), "fresh\n")
	_ = os.Remove(filepath.Join(w, "docs", "d.md"))
	out := h.ok(w, "diff")
	for _, want := range []string{"--- a/a.txt", "+++ b/a.txt", "-A", "+B", "--- /dev/null", "+fresh", "+++ /dev/null", "-D"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff missing %q:\n%s", want, out)
		}
	}
	var js struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(h.ok(w, "diff", "--json")), &js); err != nil || js.Patch != out {
		t.Fatalf("json patch differs: %v", err)
	}
}

func TestDiffFromSubdirectory(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "B\n")
	writeFile(t, filepath.Join(w, "src", "x.go"), "Y\n")
	out := h.ok(filepath.Join(w, "src"), "diff", "x.go")
	if !strings.Contains(out, "src/x.go") || strings.Contains(out, "a.txt") {
		t.Fatalf("path-limited diff = %q", out)
	}
	if code, _, _ := h.run(filepath.Join(w, "src"), "diff", "../../outside"); code != 1 {
		t.Fatalf("path outside the working copy: exit %d", code)
	}
}

func TestDiffModeOnlyBinary(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"bin.dat": "bin\x00ary"})
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	if err := os.Chmod(filepath.Join(w, "bin.dat"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := h.ok(w, "diff")
	if !strings.Contains(out, "new mode 100755") || strings.Contains(out, "Binary files") {
		t.Fatalf("diff = %q", out)
	}
}

func TestDiffTrailingNewline(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "A")
	out := h.ok(w, "diff")
	if !strings.Contains(out, "No newline at end of file") || !strings.Contains(out, "-A\n") {
		t.Fatalf("diff = %q", out)
	}
}

func TestDiffBinaryContentChange(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"bin.dat": "bin\x00ary"})
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "bin.dat"), "other\x00bytes")
	out := h.ok(w, "diff")
	if !strings.Contains(out, "Binary files a/bin.dat and b/bin.dat differ") {
		t.Fatalf("diff = %q", out)
	}
}
