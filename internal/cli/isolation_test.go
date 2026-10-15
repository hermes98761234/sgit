package cli_test

import (
	"strconv"
	"strings"
	"testing"
)

func TestCLINotFoundIsByteIdentical(t *testing.T) {
	h, _ := coreProject(t)
	type res struct {
		code     int
		out, err string
	}
	run := func(args ...string) res {
		code, out, errs := h.runAs("bob", t.TempDir(), args...)
		return res{code, out, errs}
	}
	// The id the next project gets (ids are never reused).
	nextID := "#" + strconv.FormatInt(h.env.ProjectID("web")+1, 10)
	cmds := [][]string{
		{"log", "--project", "api"}, {"log", "--project", "api", "--json"},
		{"cat", "main:x", "--project", "api"}, {"drafts", "--project", "api", "--json"},
		{"checkout", "api"}, {"checkout", "api", "--json"},
		{"log", "--project", nextID}, {"checkout", nextID, "--json"},
	}
	// Control: bob's credentials work, so exit 2 below is the project lookup.
	if r := run("log", "--project", "web"); r.code != 0 || !strings.Contains(r.out, "seed") {
		t.Fatalf("control: bob log --project web = %+v", r)
	}
	before := map[string]res{}
	for _, c := range cmds {
		r := run(c...)
		before[strings.Join(c, " ")] = r
		if !strings.Contains(r.out+r.err, " not found or no access") {
			t.Errorf("%v: %+v, want the not-found-or-no-access error", c, r)
		}
	}
	h.env.CreateProject("api")
	h.env.Mount("api", "lib", "zz-src", "private", false)
	if got := "#" + strconv.FormatInt(h.env.ProjectID("api"), 10); got != nextID {
		t.Fatalf("api got id %s, want %s", got, nextID)
	}
	for _, c := range cmds {
		k := strings.Join(c, " ")
		if got := run(c...); got != before[k] {
			t.Errorf("%s: unknown %+v vs no access %+v", k, before[k], got)
		}
		if before[k].code != 2 {
			t.Errorf("%s: exit %d, want 2", k, before[k].code)
		}
	}
}

func TestCLIOutputNeverNamesTheRepo(t *testing.T) {
	h, _ := coreProject(t)
	root := t.TempDir()
	outs := []string{h.okAs("bob", root, "checkout", "web", "--json"), h.okAs("bob", root, "projects", "--json")}
	w := root + "/web"
	writeFile(t, w+"/lib/n.txt", "n\n")
	outs = append(outs,
		h.okAs("bob", w, "status", "--json"),
		h.okAs("bob", w, "commit", "-m", "n", "--land", "--json"),
		h.okAs("bob", w, "log", "--json"),
		h.okAs("bob", w, "drafts", "--json", "--status", "all"),
		h.okAs("bob", w, "blame", "lib/a.txt", "--json"))
	for _, o := range outs {
		if strings.Contains(o, "zz-src") || strings.Contains(o, "private") || strings.Contains(o, "src/") {
			t.Fatalf("CLI output names a source: %s", o)
		}
	}
}

func TestCLICatCannotReachOutsideTheMounts(t *testing.T) {
	h, _ := coreProject(t)
	dir := t.TempDir()
	if got := h.okAs("bob", dir, "cat", "main:lib/a.txt", "--project", "web"); got != "a\n" {
		t.Fatalf("control: cat main:lib/a.txt = %q", got)
	}
	for _, spec := range []string{
		"main:private/p.txt", "main:src/a.txt", "main:lib/../private/p.txt", "main:../private/p.txt",
		"main:/private/p.txt", ":private/p.txt", "main:lib/../../zz-src.git/HEAD",
	} {
		code, out, errs := h.runAs("bob", dir, "cat", spec, "--project", "web")
		if code != 1 || out != "" || !strings.Contains(errs, "no such file at main") {
			t.Errorf("cat %s: exit %d, stdout %q", spec, code, out)
		}
		if strings.Contains(errs, "zz-src") && !strings.Contains(spec, "zz-src") {
			t.Errorf("cat %s: error names the repo: %s", spec, errs)
		}
	}
}
