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
	return testenv.Must(h.env.Project(h.env.AdminToken).GetDraft(context.Background(), &sgitv1.GetDraftRequest{Project: "web", Id: id}))
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
	files := h.env.ProjectFiles("web", d.Head)
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
	if h.env.ProjectFiles("web", h.getDraft(res.Draft).Head)["a.txt"] != "changed again\n" {
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
	g := h.env.ProjectGit(h.env.ProjectID("web"))
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
	testenv.Must(h.env.Project(h.env.AdminToken).AbandonDraft(context.Background(), &sgitv1.AbandonDraftRequest{Project: "web", Id: old}))
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
	if err := json.Unmarshal(unwrap(t, h.ok(w, "drafts", "--mine", "--json"), "drafts"), &list); err != nil || len(list) != 1 || list[0].ID != id || list[0].Title != "show me" {
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
	if h.env.ProjectFiles("web", h.getDraft(h.draftOf(w)).Head)["big.txt"] != big {
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
	if h.env.ProjectFiles("web", h.getDraft(id).Head)["a.txt"] != "v2\n" {
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

func (h *harness) openDrafts() []string {
	h.t.Helper()
	list := testenv.Must(h.env.Project(h.env.AdminToken).ListDrafts(context.Background(), &sgitv1.ListDraftsRequest{Project: "web", Status: "open"}))
	var ids []string
	for _, d := range list.Drafts {
		ids = append(ids, d.Id)
	}
	return ids
}

// A land that went through while the client lost the answer leaves
// .sg/draft pointing at a landed draft and the working copy on the old base.
// Retrying `commit --land` must sync instead of failing with "already on
// main", and must leave no open draft behind.
func TestCommitLandRetryAfterLostAnswer(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "landed\n")
	h.ok(w, "commit", "-m", "lost answer")
	id := h.draftOf(w)
	testenv.Must(h.env.Project(h.env.AdminToken).Land(context.Background(), &sgitv1.LandRequest{Project: "web", Id: id}))
	main := h.env.ProjectFiles("web", "main")

	code, out, errs := h.run(w, "commit", "-m", "lost answer", "--land")
	if code != 0 || !strings.Contains(out, "already on main") {
		t.Fatalf("retry: exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if d := h.draftOf(w); d != "" {
		t.Fatalf("retry: .sg/draft = %q", d)
	}
	if open := h.openDrafts(); len(open) != 0 {
		t.Fatalf("retry: open drafts %v", open)
	}
	// The working copy is now on main: another retry has nothing to commit.
	if code, _, errs := h.run(w, "commit", "-m", "lost answer", "--land"); code != 1 || !strings.Contains(errs, "nothing to commit") {
		t.Fatalf("second retry: exit %d: %s", code, errs)
	}
	if out := h.ok(w, "status"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("status = %q", out)
	}
	if got := h.env.ProjectFiles("web", "main"); len(got) != len(main) || got["a.txt"] != "landed\n" {
		t.Fatalf("main changed: %v", got)
	}

	// Edits made after the lost land still land on the retry.
	writeFile(t, filepath.Join(w, "a.txt"), "landed again\n")
	h.ok(w, "commit", "-m", "second")
	id = h.draftOf(w)
	testenv.Must(h.env.Project(h.env.AdminToken).Land(context.Background(), &sgitv1.LandRequest{Project: "web", Id: id}))
	writeFile(t, filepath.Join(w, "b.txt"), "more\n")
	h.ok(w, "commit", "-m", "more", "--land")
	if got := h.env.ProjectFiles("web", "main"); got["a.txt"] != "landed again\n" || got["b.txt"] != "more\n" {
		t.Fatalf("main = %v", got)
	}
	if open := h.openDrafts(); len(open) != 0 || h.draftOf(w) != "" {
		t.Fatalf("open drafts %v, .sg/draft %q", open, h.draftOf(w))
	}
}

// A lost-answer retry whose sync conflicts with later local edits reports
// already_on_main but not landed, and exits 3.
func TestCommitLandRetryAfterLostAnswerConflicts(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "landed\n")
	h.ok(w, "commit", "-m", "lost answer")
	id := h.draftOf(w)
	testenv.Must(h.env.Project(h.env.AdminToken).Land(context.Background(), &sgitv1.LandRequest{Project: "web", Id: id}))
	h.upstream(map[string]string{"src/x.go": "theirs\n"})
	writeFile(t, filepath.Join(w, "src", "x.go"), "mine\n")
	code, out, errs := h.run(w, "commit", "-m", "lost answer", "--land", "--json")
	var res struct {
		Landed        bool `json:"landed"`
		AlreadyOnMain bool `json:"already_on_main"`
		Sync          struct {
			Conflicts []string `json:"conflicts"`
		} `json:"sync"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&res); code != 3 || err != nil || res.Landed || !res.AlreadyOnMain || !contains(res.Sync.Conflicts, "src/x.go") {
		t.Fatalf("exit %d (%v) %+v\nstdout: %s\nstderr: %s", code, err, res, out, errs)
	}
}

// An open draft whose changes reached main another way lands as a no-op:
// it is closed, not left open, and the working copy syncs.
func TestCommitLandOpenDraftAlreadyOnMain(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w1 := h.checkout(t.TempDir())
	w2 := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w1, "a.txt"), "same\n")
	h.ok(w1, "commit", "-m", "one")
	id := h.draftOf(w1)
	writeFile(t, filepath.Join(w2, "a.txt"), "same\n")
	h.ok(w2, "commit", "-m", "two", "--land")
	code, out, errs := h.run(w1, "commit", "-m", "one", "--land", "--json")
	var res struct {
		Landed        bool   `json:"landed"`
		AlreadyOnMain bool   `json:"already_on_main"`
		Draft         string `json:"draft"`
	}
	if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil || !res.Landed || !res.AlreadyOnMain || res.Draft != id {
		t.Fatalf("exit %d (%v)\nstdout: %s\nstderr: %s", code, err, out, errs)
	}
	// Closed as landed, without claiming w2's commit as its own.
	if d := h.getDraft(id); d.Status != "landed" || d.LandedAs != "" {
		t.Fatalf("draft %s: status %q, landed_as %q", id, d.Status, d.LandedAs)
	}
	if open := h.openDrafts(); len(open) != 0 || h.draftOf(w1) != "" {
		t.Fatalf("open drafts %v, .sg/draft %q", open, h.draftOf(w1))
	}
}
