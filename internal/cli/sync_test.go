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
	"github.com/hermes98761234/sgit/internal/wc"
)

func (h *harness) mainSHA() string {
	h.t.Helper()
	return testenv.Must(h.env.Project(h.env.AdminToken).ResolveRev(context.Background(), &sgitv1.ResolveRevRequest{Project: "web", Rev: "main"})).Sha
}

// upstream lands changes on main directly through the API (as another user would).
func (h *harness) upstream(files map[string]string, deletes ...string) string {
	h.t.Helper()
	var ch []*sgitv1.Change
	for p, c := range files {
		ch = append(ch, testenv.Add(p, h.env.Upload(h.env.AdminToken, "web", []byte(c))[0]))
	}
	for _, p := range deletes {
		ch = append(ch, testenv.Delete(p))
	}
	resp := testenv.Must(h.env.Project(h.env.AdminToken).CommitAndLand(context.Background(),
		&sgitv1.CommitAndLandRequest{Project: "web", Base: h.mainSHA(), Changes: ch, Message: "upstream"}))
	return resp.Commit
}

type syncJSON struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Updated   []string `json:"updated"`
	Deleted   []string `json:"deleted"`
	Merged    []string `json:"merged"`
	Conflicts []string `json:"conflicts"`
	Warnings  []string `json:"warnings"`
}

func (h *harness) sync(dir string) (int, syncJSON) {
	h.t.Helper()
	code, out, errs := h.run(dir, "sync", "--json")
	var s syncJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		h.t.Fatalf("sync json %q (stderr %q): %v", out, errs, err)
	}
	return code, s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestSyncUpstreamOnly(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	head := h.upstream(map[string]string{"a.txt": "A2\n", "new.txt": "N\n"}, "docs/d.md")
	code, s := h.sync(w)
	if code != 0 || s.To != head || !contains(s.Updated, "a.txt") || !contains(s.Updated, "new.txt") || contains(s.Updated, "docs/d.md") || !contains(s.Deleted, "docs/d.md") || len(s.Deleted) != 1 {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if readFile(t, filepath.Join(w, "a.txt")) != "A2\n" || readFile(t, filepath.Join(w, "new.txt")) != "N\n" {
		t.Fatal("upstream changes not written")
	}
	if _, err := os.Stat(filepath.Join(w, "docs")); err == nil {
		t.Fatal("deleted file's now-empty directory must be pruned")
	}
	if st, m := h.status(w); len(m) != 0 || st.Base != head {
		t.Fatalf("after sync status = %+v", st)
	}
	if code, s := h.sync(w); code != 0 || len(s.Updated) != 0 {
		t.Fatalf("second sync must be a no-op: %d %+v", code, s)
	}
}

func TestSyncCleanMerge(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n4\n5\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "f.txt"), "one\n2\n3\n4\n5\n")
	h.upstream(map[string]string{"f.txt": "1\n2\n3\n4\nfive\n"})
	code, s := h.sync(w)
	if code != 0 || !contains(s.Merged, "f.txt") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if got := readFile(t, filepath.Join(w, "f.txt")); got != "one\n2\n3\n4\nfive\n" {
		t.Fatalf("merged = %q", got)
	}
	if _, m := h.status(w); m["f.txt"] != "modified" || len(m) != 1 {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncConflictWritesMarkersAndBlocksCommit(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "f.txt"), "1\nLOCAL\n3\n")
	h.upstream(map[string]string{"f.txt": "1\nUP\n3\n"})
	code, s := h.sync(w)
	if code != 3 || !contains(s.Conflicts, "f.txt") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	got := readFile(t, filepath.Join(w, "f.txt"))
	for _, want := range []string{"<<<<<<< main", "UP", "LOCAL", ">>>>>>> local"} {
		if !strings.Contains(got, want) {
			t.Fatalf("conflict file missing %q:\n%s", want, got)
		}
	}
	if st, _ := h.status(w); !contains(st.Conflicts, "f.txt") {
		t.Fatalf("status conflicts = %v", st.Conflicts)
	}
	if code, _, _ := h.run(w, "commit", "-m", "x"); code != 3 {
		t.Fatalf("commit with markers: exit %d", code)
	}
	writeFile(t, filepath.Join(w, "f.txt"), "1\nUP+LOCAL\n3\n")
	h.ok(w, "commit", "-m", "resolved")
}

func TestSyncOneSidedConflicts(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	_ = os.Remove(filepath.Join(w, "a.txt"))
	writeFile(t, filepath.Join(w, "src", "x.go"), "LOCAL X\n")
	h.upstream(map[string]string{"a.txt": "A2\n"}, "src/x.go")
	code, s := h.sync(w)
	if code != 0 || len(s.Warnings) != 2 {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if _, err := os.Stat(filepath.Join(w, "a.txt")); err == nil {
		t.Fatal("local deletion must be kept")
	}
	if readFile(t, filepath.Join(w, "src", "x.go")) != "LOCAL X\n" {
		t.Fatal("local edit of an upstream-deleted file must be kept")
	}
	if _, m := h.status(w); m["a.txt"] != "deleted" || m["src/x.go"] != "added" {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncConvergedAndAddAdd(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "same.txt"), "S\n")
	writeFile(t, filepath.Join(w, "both.txt"), "L\n")
	h.upstream(map[string]string{"same.txt": "S\n", "both.txt": "U\n"})
	code, s := h.sync(w)
	if code != 3 || !contains(s.Conflicts, "both.txt") || contains(s.Conflicts, "same.txt") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if _, m := h.status(w); m["same.txt"] != "" || m["both.txt"] != "modified" {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncSparseTouchesOnlySparse(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "src")
	w := filepath.Join(root, "web")
	h.upstream(map[string]string{"src/x.go": "X2\n", "docs/d.md": "D2\n"})
	if code, _ := h.sync(w); code != 0 {
		t.Fatalf("sync exit %d", code)
	}
	if readFile(t, filepath.Join(w, "src", "x.go")) != "X2\n" {
		t.Fatal("sparse file not updated")
	}
	if _, err := os.Stat(filepath.Join(w, "docs")); err == nil {
		t.Fatal("sync wrote outside the sparse set")
	}
}

func TestSyncFileBecomesDirKeepsLocalEdit(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "A\n", "d": "D\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "d"), "LOCAL D\n")
	baseBefore := readFile(t, filepath.Join(w, ".sg", "base"))
	h.upstream(map[string]string{"d/inner.txt": "I\n"}, "d")
	code, _, errs := h.run(w, "sync")
	// Name what is in the way locally, not the incoming files under it.
	if code != 1 || !strings.Contains(errs, "d (main has a directory there)") || strings.Contains(errs, "inner.txt") {
		t.Fatalf("sync = %d, stderr %q", code, errs)
	}
	if got := readFile(t, filepath.Join(w, "d")); got != "LOCAL D\n" {
		t.Fatalf("local d = %q", got)
	}
	if readFile(t, filepath.Join(w, ".sg", "base")) != baseBefore {
		t.Fatal(".sg/base changed")
	}
}

func TestSyncUntrackedFileBlocksUpstreamDir(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "x"), "MINE\n")
	h.upstream(map[string]string{"x/y.txt": "Y\n"})
	if code, _, errs := h.run(w, "sync"); code != 1 {
		t.Fatalf("sync = %d, stderr %q", code, errs)
	}
	if got := readFile(t, filepath.Join(w, "x")); got != "MINE\n" {
		t.Fatalf("x = %q", got)
	}
}

func TestSyncIgnoredFileNotSilentlyOverwritten(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "A\n", ".gitignore": "*.local\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "cfg.local"), "mine")
	h.upstream(map[string]string{"cfg.local": "theirs"})
	if code, _, errs := h.run(w, "sync"); code != 1 {
		t.Fatalf("sync = %d, stderr %q", code, errs)
	}
	if got := readFile(t, filepath.Join(w, "cfg.local")); got != "mine" {
		t.Fatalf("cfg.local = %q", got)
	}
	// Converged variant: upstream lands the same content.
	h.upstream(map[string]string{"cfg.local": "mine"})
	if code, s := h.sync(w); code != 0 {
		t.Fatalf("converged sync = %d %+v", code, s)
	}
	if got := readFile(t, filepath.Join(w, "cfg.local")); got != "mine" {
		t.Fatalf("cfg.local = %q", got)
	}
}

func (h *harness) upstreamMode(path, content, mode string) {
	h.t.Helper()
	sha := h.env.Upload(h.env.AdminToken, "web", []byte(content))[0]
	testenv.Must(h.env.Project(h.env.AdminToken).CommitAndLand(context.Background(), &sgitv1.CommitAndLandRequest{
		Project: "web", Base: h.mainSHA(), Message: "mode",
		Changes: []*sgitv1.Change{{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_MODIFY, BlobSha: sha, Mode: mode}},
	}))
}

func isExec(t *testing.T, p string) bool {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()&0o111 != 0
}

func TestSyncKeepsUpstreamModeChangeOnMerge(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"run.sh": "1\n2\n3\n4\n5\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "run.sh"), "one\n2\n3\n4\n5\n")
	h.upstreamMode("run.sh", "1\n2\n3\n4\nfive\n", "100755")
	if code, s := h.sync(w); code != 0 || !contains(s.Merged, "run.sh") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	p := filepath.Join(w, "run.sh")
	if !isExec(t, p) {
		t.Fatal("upstream mode change lost")
	}
	if got := readFile(t, p); got != "one\n2\n3\n4\nfive\n" {
		t.Fatalf("merged = %q", got)
	}
	if _, m := h.status(w); m["run.sh"] != "modified" || len(m) != 1 {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncUpstreamModeOnlyWithLocalSameContent(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"run.sh": "echo\n"})
	w := h.checkout(t.TempDir())
	h.upstreamMode("run.sh", "echo\n", "100755")
	if code, s := h.sync(w); code != 0 {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if !isExec(t, filepath.Join(w, "run.sh")) {
		t.Fatal("mode not applied")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncDirBecomesFile(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "A\n", "d/inner.txt": "I\n"})
	w := h.checkout(t.TempDir())
	h.upstream(map[string]string{"d": "now a file\n"}, "d/inner.txt")
	if code, s := h.sync(w); code != 0 {
		t.Fatalf("sync = %d %+v", code, s)
	}
	fi, err := os.Lstat(filepath.Join(w, "d"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("d: %v %v", fi, err)
	}
	if got := readFile(t, filepath.Join(w, "d")); got != "now a file\n" {
		t.Fatalf("d = %q", got)
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncDirBecomesFileBlockedByUntracked(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "A\n", "d/inner.txt": "I\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "d", "mine.txt"), "MINE\n")
	h.upstream(map[string]string{"d": "now a file\n"}, "d/inner.txt")
	if code, _, errs := h.run(w, "sync"); code != 1 || !strings.Contains(errs, "d (main has a file there)") {
		t.Fatalf("sync = %d, stderr %q", code, errs)
	}
	if readFile(t, filepath.Join(w, "d", "mine.txt")) != "MINE\n" || readFile(t, filepath.Join(w, "d", "inner.txt")) != "I\n" {
		t.Fatal("local files touched")
	}
}

func TestSyncFileBecomesDirUnchanged(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "A\n", "d": "D\n"})
	w := h.checkout(t.TempDir())
	h.upstream(map[string]string{"d/x.txt": "X\n"}, "d")
	if code, s := h.sync(w); code != 0 {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if readFile(t, filepath.Join(w, "d", "x.txt")) != "X\n" {
		t.Fatal("d/x.txt missing")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncRefusesWhileLocked(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	base := readFile(t, filepath.Join(w, ".sg", "base"))
	h.upstream(map[string]string{"a.txt": "A2\n"})
	lw := testenv.Must(wc.Open(w))
	unlock := testenv.Must(lw.Lock())
	defer unlock()
	code, _, errs := h.run(w, "sync")
	if code != 1 || !strings.Contains(errs, "another sg command is running in this working copy") {
		t.Fatalf("sync while locked: exit %d %q", code, errs)
	}
	if readFile(t, filepath.Join(w, "a.txt")) != "A\n" || readFile(t, filepath.Join(w, ".sg", "base")) != base {
		t.Fatal("locked working copy was changed")
	}
	writeFile(t, filepath.Join(w, "a.txt"), "edited\n")
	if _, m := h.status(w); m["a.txt"] != "modified" {
		t.Fatalf("status must work while locked: %v", m)
	}
	if code, _, errs := h.run(w, "commit", "-m", "x"); code != 1 || !strings.Contains(errs, "another sg command") {
		t.Fatalf("commit while locked: exit %d %q", code, errs)
	}
}

func TestSyncRecordsConflictsBeforeApplying(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a/f.txt": "1\n2\n3\n", "b/g.txt": "1\n2\n3\n4\n5\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a", "f.txt"), "1\nL\n3\n")
	writeFile(t, filepath.Join(w, "b", "g.txt"), "L1\n2\n3\n4\n5\n")
	h.upstream(map[string]string{"a/f.txt": "1\nU\n3\n", "b/g.txt": "1\n2\n3\n4\nU5\n"})
	bdir := filepath.Join(w, "b")
	if err := os.Chmod(bdir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bdir, 0o755) })
	if code, _, errs := h.run(w, "sync"); code != 1 {
		t.Fatalf("sync with an unwritable merge target: exit %d %q", code, errs)
	}
	if !strings.Contains(readFile(t, filepath.Join(w, "a", "f.txt")), "<<<<<<< main") {
		t.Fatal("expected the conflicted merge to be written first")
	}
	if got := readFile(t, filepath.Join(w, ".sg", "conflicts")); got != "a/f.txt\n" {
		t.Fatalf(".sg/conflicts = %q", got)
	}
}

func TestCommitAllowsMarkerLinesInUnlistedFiles(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	doc := "How to read a conflict:\n<<<<<<< ours\nmine\n=======\ntheirs\n>>>>>>> theirs\n"
	writeFile(t, filepath.Join(w, "docs", "conflicts.md"), doc)
	h.ok(w, "commit", "-m", "document conflict markers", "--land")
	if h.env.Files("web", "main")["docs/conflicts.md"] != doc {
		t.Fatal("file with intentional marker lines was not committed")
	}
}

// Files that sync removes are labelled deleted:, not updated:.
func TestSyncLabelsDeletions(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	h.upstream(map[string]string{"a.txt": "A2\n"}, "docs/d.md")
	out := h.ok(w, "sync")
	if !strings.Contains(out, "deleted:  docs/d.md") || strings.Contains(out, "updated:  docs/d.md") || !strings.Contains(out, "updated:  a.txt") {
		t.Fatalf("sync output = %q", out)
	}
}
