package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func TestMigrateCommands(t *testing.T) {
	h := newHarness(t, func(c *server.Config) { c.AllowLocalRemotes = true })
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "api.git")
	sig := gitx.Signature{Name: "Ann", Email: "ann@example.com", When: time.Unix(1700000000, 0)}
	r := testenv.Must(gitx.Init(ctx, src, sig))
	root := testenv.Must(r.ReadRef(ctx, gitx.MainRef))
	tree := testenv.Must(r.BuildTree(ctx, root, []gitx.Change{{Path: "main.go", Op: gitx.OpAdd, BlobSHA: testenv.Must(r.WriteBlob(ctx, []byte("package main\n")))}}))
	main := testenv.Must(r.CommitTree(ctx, tree, []string{root}, "add main\n", sig, sig))
	if err := r.UpdateRef(ctx, gitx.MainRef, main, root); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// plain: name from the URL
	out := h.ok(dir, "migrate", "in", "file://"+src)
	if !strings.HasPrefix(out, "migrated file://"+src+" into api: 2 commits, main ") ||
		!strings.Contains(out, "mounted at the project root of project api\n") {
		t.Fatalf("migrate in printed %q", out)
	}

	// --into, JSON
	h.ok(dir, "repo", "create", "mono")
	var in struct {
		Repo, Folder, Project, Path, Main string
		Commits                           int
		SkippedBranches                   []string `json:"skipped_branches"`
	}
	decode(t, h.ok(dir, "migrate", "in", "file://"+src, "web", "--into", "mono:svc/api", "--json"), &in)
	if in.Repo != "mono" || in.Folder != "svc/api" || in.Project != "web" || in.Path != "." || in.Commits != 2 || len(in.Main) != 40 {
		t.Fatalf("migrate in --into --json = %+v", in)
	}
	h.ok(dir, "project", "create", "shelf")
	if out := h.ok(dir, "migrate", "in", "file://"+src, "lib", "--project", "shelf", "--path", "vendor"); !strings.Contains(out, "mounted at vendor/ of project shelf\n") {
		t.Fatalf("migrate in --project printed %q", out)
	}

	// usage errors
	for _, args := range [][]string{
		{"migrate"},
		{"migrate", "in"},
		{"migrate", "in", "file://" + src, "x", "--project", "web"},
		{"migrate", "in", "file://" + src, "x", "--into", "mono"},
		{"migrate", "out", "api"},
	} {
		if code, _, errs := h.run(dir, args...); code != cli.ExitError || !strings.Contains(errs, "usage: sg migrate") {
			t.Errorf("sg %s: exit %d, stderr %q", strings.Join(args, " "), code, errs)
		}
	}

	// a derived name must satisfy the repo-name rule; this fails before dialing
	if code, _, errs := h.run(dir, "migrate", "in", "https://github.com/acme/MyApp.git"); code != cli.ExitError || !strings.Contains(errs, "cannot derive a name") {
		t.Errorf("uppercase derived name: exit %d, stderr %q", code, errs)
	}

	// out
	dst := filepath.Join(t.TempDir(), "out.git")
	if err := gitx.InitBare(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if out := h.ok(dir, "migrate", "out", "api", "--to", "file://"+dst); !strings.HasPrefix(out, "pushed project api to file://"+dst+" (main ") {
		t.Fatalf("migrate out printed %q", out)
	}
	if out := h.ok(dir, "migrate", "out", "api", "--to", "file://"+dst); !strings.Contains(out, "is up to date with project api") {
		t.Fatalf("second migrate out printed %q", out)
	}
	var o struct {
		Project, Pushed string
		UpToDate        bool `json:"up_to_date"`
	}
	decode(t, h.ok(dir, "migrate", "out", "api", "--to", "file://"+dst, "--json"), &o)
	if o.Project != "api" || o.Pushed != main || !o.UpToDate {
		t.Fatalf("migrate out --json = %+v", o)
	}
}
