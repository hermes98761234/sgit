package cli_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/cli"
)

type projectJSON struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Head    string   `json:"head"`
	Mounts  []struct {
		Path     string `json:"path"`
		Repo     string `json:"repo"`
		Folder   string `json:"folder"`
		ReadOnly bool   `json:"read_only"`
	} `json:"mounts"`
	Grants []struct {
		User  string `json:"user"`
		Level string `json:"level"`
	} `json:"grants"`
}

func TestProjectCommands(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.ok(dir, "repo", "create", "core")
	h.env.CommitDirect("core", map[string]string{"src/a.go": "a\n", "docs/r.md": "r\n"}, "seed")
	if out := h.ok(dir, "project", "create", "web"); !strings.HasPrefix(out, "created project web (id ") {
		t.Fatalf("create = %q", out)
	}
	if out := h.ok(dir, "project", "mount", "web", "lib", "core:src"); out != "mounted core:src at lib/ in web\n" {
		t.Fatalf("mount = %q", out)
	}
	if out := h.ok(dir, "project", "mount", "web", "docs", "core:docs", "--read-only"); out != "mounted core:docs at docs/ in web (read-only)\n" {
		t.Fatalf("mount ro = %q", out)
	}
	h.ok(dir, "project", "alias", "add", "web", "site")
	if out := h.ok(dir, "project", "rename", "web", "www"); out != "renamed web to www (web is kept as an alias)\n" {
		t.Fatalf("rename = %q", out)
	}
	lines := strings.Split(strings.TrimSpace(h.ok(dir, "project", "list")), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "NAME ALIASES MOUNTS" ||
		strings.Join(strings.Fields(lines[1]), " ") != "www site,web docs=core:docs(ro),lib=core:src" {
		t.Fatalf("list = %q", lines)
	}
	h.ok(dir, "token", "create", "bob")
	h.ok(dir, "acl", "grant", "bob", "site", "write")
	var show projectJSON
	decode(t, h.ok(dir, "project", "show", "www", "--json"), &show)
	if show.Name != "www" || !reflect.DeepEqual(show.Aliases, []string{"site", "web"}) || len(show.Mounts) != 2 ||
		!show.Mounts[0].ReadOnly || len(show.Grants) != 1 || show.Grants[0].User != "bob" || len(show.Head) != 40 {
		t.Fatalf("show = %+v", show)
	}
	acl := strings.Split(strings.TrimSpace(h.ok(dir, "acl", "list", "--project", "www")), "\n")
	if len(acl) != 2 || strings.Join(strings.Fields(acl[1]), " ") != "bob www write" {
		t.Fatalf("acl list = %q", acl)
	}
	if code, _, errs := h.run(dir, "repo", "delete", "core", "--yes"); code != cli.ExitError || !strings.Contains(errs, "mounted by project(s) www") {
		t.Fatalf("repo delete while mounted: exit %d, %q", code, errs)
	}
	h.ok(dir, "project", "unmount", "www", "docs")
	if out := h.ok(dir, "project", "alias", "rm", "site"); out != "removed alias site (was www)\n" {
		t.Fatalf("alias rm = %q", out)
	}
	if code, _, _ := h.run(dir, "project", "delete", "www"); code != cli.ExitError {
		t.Fatalf("delete without --yes: exit %d", code)
	}
	if out := h.ok(dir, "project", "delete", "www", "--yes"); !strings.HasPrefix(out, "deleted project www (0 drafts, 1 grants removed)") {
		t.Fatalf("delete = %q", out)
	}
	h.ok(dir, "repo", "delete", "core", "--yes")
}

func TestProjectCommandsNeedAdminAndValidate(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	bob := h.ok(dir, "token", "create", "bob")
	h.loginAs("bob", strings.TrimSpace(bob))
	for _, args := range [][]string{{"project", "list"}, {"project", "create", "x"}, {"acl", "list"}} {
		if code, _, _ := h.runAs("bob", dir, args...); code != cli.ExitAuth {
			t.Errorf("bob %v: exit %d", args, code)
		}
	}
	for _, args := range [][]string{
		{"project"}, {"project", "mount", "web", "lib", "core"}, {"project", "mount", "web", "lib", "core:"},
		{"project", "alias"}, {"project", "alias", "add", "web"}, {"project", "rename", "web"}, {"acl", "grant", "bob", "web"},
	} {
		if code, _, errs := h.run(dir, args...); code != cli.ExitError || !strings.Contains(errs, "usage: sg") {
			t.Errorf("%v: exit %d, %q", args, code, errs)
		}
	}
}
