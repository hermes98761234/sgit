package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func (h *harness) draftOf(w string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(w, ".sg", "draft"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *harness) getDraft(id string) *sgitv1.Draft {
	h.t.Helper()
	return testenv.Must(h.env.Repo(h.env.AdminToken).GetDraft(context.Background(), &sgitv1.GetDraftRequest{Repo: "web", Id: id}))
}

func (h *harness) checkout(root string, extra ...string) string {
	h.t.Helper()
	h.ok(root, append([]string{"checkout", "web"}, extra...)...)
	return filepath.Join(root, "web")
}

func TestCommitCreatesAndUpdatesDraft(t *testing.T) {
	h := newHarness(t)
	main := h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(w, "new.txt"), "new\n")
	_ = os.Remove(filepath.Join(w, "docs", "d.md"))

	var res struct {
		Draft   string `json:"draft"`
		Version int    `json:"version"`
		Changes int    `json:"changes"`
	}
	if err := json.Unmarshal([]byte(h.ok(w, "commit", "-m", "first change", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 || res.Changes != 3 || h.draftOf(w) != res.Draft {
		t.Fatalf("commit = %+v, .sg/draft = %q", res, h.draftOf(w))
	}
	d := h.getDraft(res.Draft)
	files := h.env.Files("web", d.Head)
	if files["a.txt"] != "changed\n" || files["new.txt"] != "new\n" || files["docs/d.md"] != "" || d.Base != main || d.Title != "first change" {
		t.Fatalf("draft files = %v, draft = %+v", files, d)
	}
	if h.env.Files("web", "main")["a.txt"] != "A\n" {
		t.Fatal("commit without --land must not touch main")
	}
	writeFile(t, filepath.Join(w, "a.txt"), "changed again\n")
	out := h.ok(w, "commit", "-m", "second")
	if !strings.Contains(out, res.Draft) || !strings.Contains(out, "v2") {
		t.Fatalf("second commit = %q", out)
	}
	if h.env.Files("web", h.getDraft(res.Draft).Head)["a.txt"] != "changed again\n" {
		t.Fatal("draft not updated")
	}
}

func TestCommitNothingAndNoMessage(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	if code, _, errs := h.run(w, "commit", "-m", "x"); code != 1 || !strings.Contains(errs, "nothing to commit") {
		t.Fatalf("nothing: %d %q", code, errs)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "z\n")
	if code, _, _ := h.run(w, "commit"); code != 1 {
		t.Fatalf("missing -m: exit %d", code)
	}
}

func TestCommitSymlinkAndExecRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "run.sh"), "#!/bin/sh\necho hi\n")
	_ = os.Chmod(filepath.Join(w, "run.sh"), 0o755)
	if err := os.Symlink("a.txt", filepath.Join(w, "link")); err != nil {
		t.Fatal(err)
	}
	h.ok(w, "commit", "-m", "modes")
	d := h.getDraft(h.draftOf(w))
	g := h.env.Git("web")
	defer g.Close()
	files := testenv.Must(g.ListFiles(context.Background(), d.Head))
	if files["run.sh"].Mode != "100755" || files["link"].Mode != "120000" {
		t.Fatalf("modes on server = %+v", files)
	}
	root2 := t.TempDir()
	h.ok(root2, "checkout", "web", "copy", "--at", d.Head)
	fi, err := os.Stat(filepath.Join(root2, "copy", "run.sh"))
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exec bit lost: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(root2, "copy", "link")); err != nil || target != "a.txt" {
		t.Fatalf("symlink lost: %q %v", target, err)
	}
}

func TestCommitAfterDraftAbandonedElsewhere(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "one\n")
	h.ok(w, "commit", "-m", "one")
	old := h.draftOf(w)
	testenv.Must(h.env.Repo(h.env.AdminToken).AbandonDraft(context.Background(), &sgitv1.AbandonDraftRequest{Repo: "web", Id: old}))
	writeFile(t, filepath.Join(w, "a.txt"), "two\n")
	h.ok(w, "commit", "-m", "two")
	if nd := h.draftOf(w); nd == "" || nd == old {
		t.Fatalf("expected a new draft, got %q (old %q)", nd, old)
	}
}

func TestDraftsShowAbandon(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "shown\n")
	h.ok(w, "commit", "-m", "show me")
	id := h.draftOf(w)
	var list []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal([]byte(h.ok(w, "drafts", "--mine", "--json")), &list); err != nil || len(list) != 1 || list[0].ID != id || list[0].Title != "show me" {
		t.Fatalf("drafts = %+v (%v)", list, err)
	}
	if out := h.ok(w, "show", id); !strings.Contains(out, "+shown") || !strings.Contains(out, "show me") {
		t.Fatalf("show draft = %q", out)
	}
	if out := h.ok(w, "show", "main"); !strings.Contains(out, "seed") {
		t.Fatalf("show main = %q", out)
	}
	h.ok(w, "abandon")
	if h.draftOf(w) != "" || h.getDraft(id).Status != "abandoned" {
		t.Fatal("abandon must clear .sg/draft and abandon on the server")
	}
	if code, _, _ := h.run(w, "abandon"); code != 1 {
		t.Fatalf("abandon with no draft: exit %d", code)
	}
}

func TestCommitBlockedByConflictMarkers(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "x\n<<<<<<< main\nA\n=======\nB\n>>>>>>> local\n")
	writeFile(t, filepath.Join(w, ".sg", "conflicts"), "a.txt\n")
	if code, _, errs := h.run(w, "commit", "-m", "m"); code != 3 || !strings.Contains(errs, "a.txt") {
		t.Fatalf("markers: exit %d %q", code, errs)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "resolved\n")
	h.ok(w, "commit", "-m", "m")
	if _, err := os.Stat(filepath.Join(w, ".sg", "conflicts")); err == nil {
		t.Fatal(".sg/conflicts must be cleared once resolved")
	}
}

func TestCommitLargeFile(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	big := strings.Repeat("0123456789abcdef", 200<<10) // 3.2 MiB, multi-chunk upload
	writeFile(t, filepath.Join(w, "big.txt"), big)
	h.ok(w, "commit", "-m", "big")
	if h.env.Files("web", h.getDraft(h.draftOf(w)).Head)["big.txt"] != big {
		t.Fatal("big file corrupted")
	}
}

func TestCommitDraftFlagUpdatesThatDraft(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "v1\n")
	h.ok(w, "commit", "-m", "v1")
	id := h.draftOf(w)
	if err := os.Remove(filepath.Join(w, ".sg", "draft")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "v2\n")
	out := h.ok(w, "commit", "-m", "v2", "--draft", id)
	if !strings.Contains(out, id) || !strings.Contains(out, "v2") || h.draftOf(w) != id {
		t.Fatalf("commit --draft = %q, .sg/draft = %q", out, h.draftOf(w))
	}
	if h.env.Files("web", h.getDraft(id).Head)["a.txt"] != "v2\n" {
		t.Fatal("draft not updated")
	}
	if code, _, _ := h.run(w, "commit", "-m", "x", "--draft", "NOT-AN-ID"); code != 1 {
		t.Fatalf("bad draft id: exit %d", code)
	}
}

// A nested git clone or a file name the server rejects must not block commits.
func TestCommitSkipsNestedRepoAndBadNames(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "vendor", "lib", ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(w, "vendor", "lib", "v.txt"), "v\n")
	writeFile(t, filepath.Join(w, "new\nline"), "n\n")
	st := h.ok(w, "status")
	if !strings.Contains(st, "skipped:  \"new\\nline\"") || strings.Contains(st, ".git") {
		t.Fatalf("status = %q", st)
	}
	code, _, errs := h.run(w, "commit", "-m", "vendor", "--land")
	if code != 0 || !strings.Contains(errs, `skipping "new\nline"`) {
		t.Fatalf("commit = %d, stderr %q", code, errs)
	}
	if got := h.ok(w, "cat", "main:vendor/lib/v.txt"); got != "v\n" {
		t.Fatalf("v.txt = %q", got)
	}
}
