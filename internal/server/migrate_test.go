package server_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/store"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func allowLocal(c *server.Config) { c.AllowLocalRemotes = true }

// srcRepo creates a bare git repo outside the server: the initial empty
// commit, then one commit per map (paths must be new in each), and returns
// its dir and main.
func srcRepo(t *testing.T, commits ...map[string]string) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "src.git")
	sig := gitx.Signature{Name: "Ann", Email: "ann@example.com", When: time.Unix(1700000000, 0)}
	r := testenv.Must(gitx.Init(ctx, dir, sig))
	defer r.Close()
	main := testenv.Must(r.ReadRef(ctx, gitx.MainRef))
	for i, files := range commits {
		var ch []gitx.Change
		for p, content := range files {
			ch = append(ch, gitx.Change{Path: p, Op: gitx.OpAdd, BlobSHA: testenv.Must(r.WriteBlob(ctx, []byte(content)))})
		}
		tree := testenv.Must(r.BuildTree(ctx, main, ch))
		sig.When = sig.When.Add(time.Minute)
		c := testenv.Must(r.CommitTree(ctx, tree, []string{main}, fmt.Sprintf("commit %d\n", i+1), sig, sig))
		if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
			t.Fatal(err)
		}
		main = c
	}
	return dir, main
}

func TestMigrateInPlain(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	src, main := srcRepo(t, map[string]string{"a.txt": "a"}, map[string]string{"b/c.txt": "c"})
	resp, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Repo != "app" || resp.Folder != "." || resp.Project != "app" || resp.Path != "." ||
		resp.Main != main || resp.Commits != 3 || resp.ProjectHead != main || len(resp.SkippedBranches) != 0 {
		t.Fatalf("MigrateIn = %+v", resp)
	}
	if files := e.ProjectFiles("app", "main"); files["a.txt"] != "a" || files["b/c.txt"] != "c" || len(files) != 2 {
		t.Fatalf("project files = %v", files)
	}
}

func TestMigrateInIntoExistingProject(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/x.go": "x"}, "seed")
	e.CreateProject("web")
	e.Mount("web", "core", "core", "src", false)
	src, _ := srcRepo(t, map[string]string{"a.txt": "a"})
	resp, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{
		FromUrl: "file://" + src, Name: "lib", Project: "web", Path: "vendor/lib"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Repo != "lib" || resp.Project != "web" || resp.Path != "vendor/lib" {
		t.Fatalf("MigrateIn = %+v", resp)
	}
	if files := e.ProjectFiles("web", "main"); files["core/x.go"] != "x" || files["vendor/lib/a.txt"] != "a" {
		t.Fatalf("project files = %v", files)
	}
}

func TestMigrateInRollsBackRepoWhenMountFails(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepoProject("core") // project core mounts core at ".", so no other mount fits
	src, _ := srcRepo(t, map[string]string{"a.txt": "a"})
	_, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{
		FromUrl: "file://" + src, Name: "app", Project: "core", Path: "lib"})
	if err == nil {
		t.Fatal("MigrateIn succeeded; want a mount error")
	}
	if _, err := e.Store.GetRepo(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repo app after rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "repos", "app.git")); !os.IsNotExist(err) {
		t.Fatalf("repo dir after rollback: %v", err)
	}
	// the name is free again
	testenv.Must(e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "app"}))
}

func TestMigrateInNameTaken(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateProject("app")
	src, _ := srcRepo(t, map[string]string{"a.txt": "a"})
	_, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "app"})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
	if _, err := e.Store.GetRepo(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repo app was created: %v", err)
	}
}

func TestMigrateInValidation(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	ac := e.Admin(e.AdminToken)
	src, main := srcRepo(t)
	noMain := filepath.Join(t.TempDir(), "m.git")
	if err := gitx.InitBare(ctx, noMain); err != nil {
		t.Fatal(err)
	}
	r := gitx.Open(src)
	if err := r.Push(ctx, noMain, main, "", true); err != nil { // gives noMain a main...
		t.Fatal(err)
	}
	r.Close()
	m := gitx.Open(noMain)
	if err := m.UpdateRef(ctx, "refs/heads/master", main, ""); err != nil { // ...then move it to master
		t.Fatal(err)
	}
	if err := m.DeleteRef(ctx, gitx.MainRef); err != nil {
		t.Fatal(err)
	}
	m.Close()
	cases := []struct {
		name string
		req  *sgitv1.MigrateInRequest
		code codes.Code
	}{
		{"no url", &sgitv1.MigrateInRequest{Name: "x"}, codes.InvalidArgument},
		{"bad name", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "Bad Name"}, codes.InvalidArgument},
		{"project without path", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", Project: "p"}, codes.InvalidArgument},
		{"path without project", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", Path: "p"}, codes.InvalidArgument},
		{"unknown project", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", Project: "nope", Path: "p"}, codes.NotFound},
		{"mirror with into", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", IntoRepo: "r", IntoFolder: "f", MirrorUrl: "file://" + src}, codes.InvalidArgument},
		{"into root", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", IntoRepo: "r", IntoFolder: "."}, codes.InvalidArgument},
		{"folder without repo", &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", IntoFolder: "f"}, codes.InvalidArgument},
		{"credentials", &sgitv1.MigrateInRequest{FromUrl: "https://u:p@example.com/x.git", Name: "x"}, codes.InvalidArgument},
		{"no main", &sgitv1.MigrateInRequest{FromUrl: "file://" + noMain, Name: "x"}, codes.InvalidArgument},
	}
	for _, c := range cases {
		if _, err := ac.MigrateIn(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
	if _, err := e.Store.GetRepo(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a failed MigrateIn left repo x: %v", err)
	}
	if _, err := e.Store.ResolveProject(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a failed MigrateIn left project x: %v", err)
	}
}
