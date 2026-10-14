package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/testenv"
)

// coreProject: repo "zz-src" (lib code under src/, private/ unmounted) and
// project "web" (alias "site") mounting zz-src:src at lib, bob with write.
func coreProject(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.env.CreateRepo("zz-src")
	h.env.CommitDirect("zz-src", map[string]string{"src/a.txt": "a\n", "private/p.txt": "p\n"}, "seed")
	h.env.CreateProject("web")
	h.env.Mount("web", "lib", "zz-src", "src", false)
	testenv.Must(h.env.Admin(h.env.AdminToken).AddProjectAlias(t.Context(), &sgitv1.AddProjectAliasRequest{Project: "web", Alias: "site"}))
	bob := h.env.NewUser("bob", "web", "write")
	h.loginAs("bob", bob)
	return h, bob
}

func TestCheckoutByAliasStoresProjectID(t *testing.T) {
	h, _ := coreProject(t)
	root := t.TempDir()
	var res map[string]any
	if err := json.Unmarshal([]byte(h.okAs("bob", root, "checkout", "site", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if res["project"] != "web" || res["dir"] != filepath.Join(root, "web") {
		t.Fatalf("checkout json = %v", res)
	}
	w := filepath.Join(root, "web")
	cfg := readFile(t, filepath.Join(w, ".sg", "config"))
	id := h.env.ProjectID("web")
	if !strings.Contains(cfg, "project = "+strconv.FormatInt(id, 10)) || strings.Contains(cfg, "zz-src") {
		t.Fatalf(".sg/config = %q", cfg)
	}
	if readFile(t, filepath.Join(w, "lib", "a.txt")) != "a\n" {
		t.Fatal("file missing")
	}
	if _, err := os.Stat(filepath.Join(w, "private")); err == nil {
		t.Fatal("unmounted folder checked out")
	}
	h.ok(t.TempDir(), "project", "rename", "web", "www")
	writeFile(t, filepath.Join(w, "lib", "b.txt"), "b\n")
	h.okAs("bob", w, "commit", "-m", "add b", "--land")
	h.okAs("bob", w, "sync")
	if out := h.okAs("bob", w, "status"); !strings.HasPrefix(out, "On www@") {
		t.Fatalf("status after rename = %q", out)
	}
	if h.env.Files("zz-src", "main")["src/b.txt"] != "b\n" {
		t.Fatal("land did not reach the source")
	}
}

func TestProjectsCommand(t *testing.T) {
	h, _ := coreProject(t)
	h.env.CreateProject("other")
	var res struct {
		Projects []struct {
			ID      int64    `json:"id"`
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
			Level   string   `json:"level"`
		} `json:"projects"`
	}
	decode(t, h.okAs("bob", t.TempDir(), "projects", "--json"), &res)
	if len(res.Projects) != 1 || res.Projects[0].Name != "web" || res.Projects[0].Level != "write" || res.Projects[0].Aliases[0] != "site" {
		t.Fatalf("bob projects = %+v", res)
	}
	decode(t, h.ok(t.TempDir(), "projects", "--json"), &res)
	if len(res.Projects) != 2 || res.Projects[0].Level != "admin" {
		t.Fatalf("admin projects = %+v", res)
	}
	out := h.okAs("bob", t.TempDir(), "projects")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || strings.Join(strings.Fields(lines[1]), " ") != "web site write" {
		t.Fatalf("projects text = %q", out)
	}
}

func TestNoAccessExitsTwo(t *testing.T) {
	h, _ := coreProject(t)
	h.env.CreateProject("api")
	for _, p := range []string{"api", "nope"} {
		code, _, errs := h.runAs("bob", t.TempDir(), "log", "--project", p)
		if code != cli.ExitAuth || errs != "sg: project \""+p+"\" not found or no access\n" {
			t.Errorf("log --project %s: exit %d, %q", p, code, errs)
		}
	}
	if code, _, _ := h.runAs("bob", t.TempDir(), "checkout", "api"); code != cli.ExitAuth {
		t.Errorf("checkout api: exit %d", code)
	}
}

func TestPartialLandCLI(t *testing.T) {
	h := newHarness(t)
	for _, r := range []string{"a", "b"} {
		h.env.CreateRepo(r)
		h.env.CommitDirect(r, map[string]string{"f.txt": "base\n"}, "seed")
	}
	h.env.CreateProject("web")
	h.env.Mount("web", "a", "a", ".", false)
	h.env.Mount("web", "b", "b", ".", false)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	h.env.CommitDirect("b", map[string]string{"f.txt": "theirs\n"}, "upstream")
	writeFile(t, filepath.Join(w, "a", "f.txt"), "ours a\n")
	writeFile(t, filepath.Join(w, "b", "f.txt"), "ours b\n")
	code, out, _ := h.run(w, "commit", "-m", "both", "--land", "--json")
	if code != cli.ExitConflict {
		t.Fatalf("partial land exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res["partial"] != true || res["landed"] != false {
		t.Fatalf("partial json = %v", res)
	}
	if lm, _ := res["landed_mounts"].([]any); len(lm) != 1 || lm[0] != "a" {
		t.Fatalf("landed_mounts = %v", res["landed_mounts"])
	}
	if readFile(t, filepath.Join(w, "a", "f.txt")) != "ours a\n" || !strings.Contains(readFile(t, filepath.Join(w, "b", "f.txt")), "<<<<<<<") {
		t.Fatal("working copy not rebased with markers in b only")
	}
	writeFile(t, filepath.Join(w, "b", "f.txt"), "merged\n")
	h.ok(w, "commit", "-m", "both", "--land")
	if h.env.Files("b", "main")["f.txt"] != "merged\n" || h.env.Files("a", "main")["f.txt"] != "ours a\n" {
		t.Fatal("retry did not land the rest")
	}
}

func TestPreProjectWorkingCopyIsRejected(t *testing.T) {
	h, _ := coreProject(t)
	w := t.TempDir()
	if err := os.MkdirAll(filepath.Join(w, ".sg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w, ".sg", "config"), "server = \""+h.env.Addr+"\"\nrepo = \"web\"\n")
	writeFile(t, filepath.Join(w, ".sg", "base"), strings.Repeat("a", 40)+"\n")
	code, _, errs := h.runAs("bob", w, "sync")
	if code != cli.ExitError || !strings.Contains(errs, "predates projects") {
		t.Fatalf("old working copy: exit %d, %q", code, errs)
	}
}
