package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func TestAgentLoopCommitLand(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "fixed\n")
	out := h.ok(w, "commit", "-m", "fix", "--land")
	if !strings.Contains(out, "landed") {
		t.Fatalf("output = %q", out)
	}
	if h.env.Files("web", "main")["a.txt"] != "fixed\n" {
		t.Fatal("change not on main")
	}
	if st, m := h.status(w); len(m) != 0 || st.Base != h.mainSHA() || h.draftOf(w) != "" {
		t.Fatalf("after land: status %+v draft %q", st, h.draftOf(w))
	}
}

func TestLandRebasesAndBringsUpstreamFiles(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	h.upstream(map[string]string{"u.txt": "U\n"})
	writeFile(t, filepath.Join(w, "a.txt"), "mine\n")
	h.ok(w, "commit", "-m", "mine", "--land")
	files := h.env.Files("web", "main")
	if files["a.txt"] != "mine\n" || files["u.txt"] != "U\n" {
		t.Fatalf("main = %v", files)
	}
	if readFile(t, filepath.Join(w, "u.txt")) != "U\n" {
		t.Fatal("upstream file not brought into the working copy")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestTwoAgentsConflictThenResolve(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n"})
	bob := h.env.NewUser("bob", "web", "write")
	h.loginAs("bob", bob)
	wa := h.checkout(t.TempDir())
	rootB := t.TempDir()
	h.okAs("bob", rootB, "checkout", "web")
	wb := filepath.Join(rootB, "web")

	writeFile(t, filepath.Join(wa, "f.txt"), "1\nA-change\n3\n")
	writeFile(t, filepath.Join(wa, "a-only.txt"), "from A\n")
	h.ok(wa, "commit", "-m", "A", "--land")

	writeFile(t, filepath.Join(wb, "f.txt"), "1\nB-change\n3\n")
	code, out, errs := h.runAs("bob", wb, "commit", "-m", "B", "--land")
	if code != 3 || !strings.Contains(out+errs, "conflict") {
		t.Fatalf("B land: exit %d\n%s\n%s", code, out, errs)
	}
	got := readFile(t, filepath.Join(wb, "f.txt"))
	if !strings.Contains(got, "<<<<<<< main") || !strings.Contains(got, "A-change") || !strings.Contains(got, "B-change") {
		t.Fatalf("conflict file = %q", got)
	}
	if readFile(t, filepath.Join(wb, "a-only.txt")) != "from A\n" {
		t.Fatal("non-conflicting upstream file must arrive with the conflict sync")
	}
	draft := h.draftOf(wb)
	if draft == "" {
		t.Fatal("draft id must be kept after a conflict")
	}
	if code, _, _ := h.runAs("bob", wb, "commit", "-m", "B", "--land"); code != 3 {
		t.Fatalf("land with markers still present: exit %d", code)
	}
	writeFile(t, filepath.Join(wb, "f.txt"), "1\nA-change+B-change\n3\n")
	h.okAs("bob", wb, "commit", "-m", "B resolved", "--land")
	if h.env.Files("web", "main")["f.txt"] != "1\nA-change+B-change\n3\n" {
		t.Fatal("resolved content not on main")
	}
	if h.draftOf(wb) != "" || h.getDraft(draft).Status != "landed" {
		t.Fatal("the same draft must have landed and been cleared")
	}
}

func TestSgLandAfterCommit(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	if code, _, _ := h.run(w, "land"); code != 1 {
		t.Fatalf("land without a draft: exit %d", code)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "via land\n")
	h.ok(w, "commit", "-m", "draft first")
	h.ok(w, "land")
	if h.env.Files("web", "main")["a.txt"] != "via land\n" || h.draftOf(w) != "" {
		t.Fatal("sg land did not land the working copy's draft")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestLandEditsAfterCommitSurvive(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n4\n5\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "f.txt"), "one\n2\n3\n4\n5\n")
	h.ok(w, "commit", "-m", "v1")
	writeFile(t, filepath.Join(w, "f.txt"), "one\n2\n3\n4\nfive\n") // edited after the commit
	h.ok(w, "land")
	if h.env.Files("web", "main")["f.txt"] != "one\n2\n3\n4\n5\n" {
		t.Fatal("land must land the committed version")
	}
	if readFile(t, filepath.Join(w, "f.txt")) != "one\n2\n3\n4\nfive\n" {
		t.Fatal("uncommitted local edit lost")
	}
	if _, m := h.status(w); m["f.txt"] != "modified" {
		t.Fatalf("status = %v", m)
	}
}

func TestLandExhaustedExit4(t *testing.T) {
	var h *harness
	var n atomic.Int32
	h = newHarness(t, func(c *server.Config) {
		c.LandRetries = 2
		c.BeforeCAS = func(repo string) {
			h.env.CommitDirect(repo, map[string]string{fmt.Sprintf("ext%d.txt", n.Add(1)): "x"}, "external")
		}
	})
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "never lands\n")
	base := readFile(t, filepath.Join(w, ".sg", "base"))
	if code, _, errs := h.run(w, "commit", "-m", "x", "--land"); code != 4 {
		t.Fatalf("exit %d, want 4 (%s)", code, errs)
	}
	if readFile(t, filepath.Join(w, "a.txt")) != "never lands\n" {
		t.Fatal("working copy must be untouched")
	}
	if readFile(t, filepath.Join(w, ".sg", "base")) != base {
		t.Fatal(".sg/base changed")
	}
	draft := h.draftOf(w)
	if draft == "" || h.getDraft(draft).Status != "open" {
		t.Fatalf("draft %q must be kept and open", draft)
	}
	if code, _, errs := h.run(w, "commit", "-m", "x", "--land"); code != 4 {
		t.Fatalf("second attempt: exit %d, want 4 (%s)", code, errs)
	}
	if h.draftOf(w) != draft {
		t.Fatalf("draft changed: %q -> %q", draft, h.draftOf(w))
	}
	l := testenv.Must(h.env.Repo(h.env.AdminToken).ListDrafts(context.Background(), &sgitv1.ListDraftsRequest{Repo: "web", Status: "open"}))
	if len(l.Drafts) != 1 {
		t.Fatalf("open drafts = %d, want 1", len(l.Drafts))
	}
}

func TestLandSucceedsButSyncBlocked(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", asJSON), func(t *testing.T) {
			h := newHarness(t)
			h.seed(seedFiles)
			w := h.checkout(t.TempDir())
			h.upstream(map[string]string{"d/u.txt": "U\n"})
			writeFile(t, filepath.Join(w, "a.txt"), "landed\n")
			h.ok(w, "commit", "-m", "x")
			id := h.draftOf(w)
			// created after the commit, so it is not part of the draft
			writeFile(t, filepath.Join(w, "d"), "local\n")
			args := []string{"land"}
			if asJSON {
				args = append(args, "--json")
			}
			code, out, errs := h.run(w, args...)
			if code != 1 || !strings.Contains(errs, "landed") {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if asJSON {
				var res struct {
					Landed    bool   `json:"landed"`
					Commit    string `json:"commit"`
					Draft     string `json:"draft"`
					SyncError string `json:"sync_error"`
				}
				if err := json.Unmarshal([]byte(out), &res); err != nil {
					t.Fatalf("json %q: %v", out, err)
				}
				if !res.Landed || res.Commit != h.mainSHA() || res.Draft != id || res.SyncError == "" {
					t.Fatalf("json = %+v", res)
				}
			} else if !strings.Contains(out, "landed") {
				t.Fatalf("stdout = %q", out)
			}
			if h.env.Files("web", "main")["a.txt"] != "landed\n" {
				t.Fatal("change not on main")
			}
			if h.draftOf(w) != "" {
				t.Fatal("draft must be cleared after landing")
			}
			if readFile(t, filepath.Join(w, "d")) != "local\n" {
				t.Fatal("local file d clobbered")
			}
			if err := os.Remove(filepath.Join(w, "d")); err != nil {
				t.Fatal(err)
			}
			h.ok(w, "sync")
		})
	}
}

func TestCommitAfterDraftLandedElsewhere(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	landElsewhere := func() string {
		id := h.draftOf(w)
		testenv.Must(h.env.Repo(h.env.AdminToken).Land(context.Background(), &sgitv1.LandRequest{Repo: "web", Id: id}))
		return id
	}
	writeFile(t, filepath.Join(w, "a.txt"), "one\n")
	h.ok(w, "commit", "-m", "one")
	old := landElsewhere()
	writeFile(t, filepath.Join(w, "a.txt"), "two\n")
	h.ok(w, "commit", "-m", "two")
	if nd := h.draftOf(w); nd == "" || nd == old {
		t.Fatalf("commit: expected a new draft, got %q (old %q)", nd, old)
	}

	// The same through commit --land, in a fresh working copy on the new main.
	w = h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "src", "x.go"), "X2\n")
	h.ok(w, "commit", "-m", "x2")
	old = landElsewhere()
	writeFile(t, filepath.Join(w, "new.txt"), "new\n")
	code, out, errs := h.run(w, "commit", "-m", "three", "--land", "--json")
	if code != 0 {
		t.Fatalf("commit --land: exit %d\n%s\n%s", code, out, errs)
	}
	var res struct {
		Landed bool   `json:"landed"`
		Draft  string `json:"draft"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Landed || res.Draft == "" || res.Draft == old {
		t.Fatalf("commit --land = %+v (%v), old draft %q", res, err, old)
	}
	if files := h.env.Files("web", "main"); files["new.txt"] != "new\n" || files["src/x.go"] != "X2\n" {
		t.Fatalf("main = %v", files)
	}
	if h.draftOf(w) != "" {
		t.Fatal("draft must be cleared after landing")
	}
}

func TestLandConflictSyncBlockedKeepsExit3(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n"})
	bob := h.env.NewUser("bob", "web", "write")
	h.loginAs("bob", bob)
	wa := h.checkout(t.TempDir())
	rootB := t.TempDir()
	h.okAs("bob", rootB, "checkout", "web")
	wb := filepath.Join(rootB, "web")

	writeFile(t, filepath.Join(wa, "f.txt"), "1\nA-change\n3\n")
	writeFile(t, filepath.Join(wa, "d", "a-only.txt"), "from A\n")
	h.ok(wa, "commit", "-m", "A", "--land")

	writeFile(t, filepath.Join(wb, "f.txt"), "1\nB-change\n3\n")
	h.okAs("bob", wb, "commit", "-m", "B")
	writeFile(t, filepath.Join(wb, "d"), "local B\n")
	code, out, errs := h.runAs("bob", wb, "land")
	if code != 3 || !strings.Contains(out+errs, "conflict") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if h.draftOf(wb) == "" {
		t.Fatal("draft must be kept")
	}
}
