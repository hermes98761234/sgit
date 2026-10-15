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
	before := map[string]res{}
	for _, c := range cmds {
		before[strings.Join(c, " ")] = run(c...)
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
