package server_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	if files := e.Files("app", "main"); files["a.txt"] != "a" {
		t.Fatalf("re-imported app files = %v", files)
	}
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

// tempRefs lists refs under refs/sgit/ in a server repo.
func tempRefs(t *testing.T, e *testenv.Env, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", filepath.Join(e.DataDir, "repos", repo+".git"), "for-each-ref", "refs/sgit/").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestMigrateInInto(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepo("mono")
	before := e.CommitDirect("mono", map[string]string{"README": "r"}, "seed")
	src, main := srcRepo(t, map[string]string{"a.txt": "a"}, map[string]string{"b/c.txt": "c"})
	r := gitx.Open(src)
	if err := r.UpdateRef(ctx, "refs/heads/release/1", main, ""); err != nil {
		t.Fatal(err)
	}
	r.Close()

	resp, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{
		FromUrl: "file://" + src, Name: "app", IntoRepo: "mono", IntoFolder: "svc/app"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Repo != "mono" || resp.Folder != "svc/app" || resp.Project != "app" || resp.Path != "." ||
		resp.Commits != 3 || !slices.Equal(resp.SkippedBranches, []string{"release/1"}) || resp.ProjectHead == "" {
		t.Fatalf("MigrateIn = %+v", resp)
	}
	g := e.Git("mono")
	defer g.Close()
	if got := testenv.Must(g.ReadRef(ctx, gitx.MainRef)); got != resp.Main {
		t.Fatalf("mono main = %s, resp.Main = %s", got, resp.Main)
	}
	if fps := testenv.Must(g.FirstParents(ctx, resp.Main, before)); len(fps) != 3 {
		t.Fatalf("mono gained %d commits, want 3", len(fps))
	}
	files := e.Files("mono", "main")
	if files["README"] != "r" || files["svc/app/a.txt"] != "a" || files["svc/app/b/c.txt"] != "c" || len(files) != 3 {
		t.Fatalf("mono files = %v", files)
	}
	if pf := e.ProjectFiles("app", "main"); pf["a.txt"] != "a" || pf["b/c.txt"] != "c" || len(pf) != 2 {
		t.Fatalf("project files = %v", pf)
	}
	// every original commit that touched the folder is in the project history
	pg := e.ProjectGit(e.ProjectID("app"))
	defer pg.Close()
	var msgs []string
	for _, fp := range testenv.Must(pg.FirstParents(ctx, resp.ProjectHead, "")) {
		msgs = append(msgs, testenv.Must(pg.ReadCommit(ctx, fp.SHA)).Message)
	}
	if !slices.Contains(msgs, "commit 1\n") || !slices.Contains(msgs, "commit 2\n") {
		t.Fatalf("project history messages = %q", msgs)
	}
	if refs := tempRefs(t, e, "mono"); refs != "" {
		t.Fatalf("temporary refs left: %s", refs)
	}
}

func TestMigrateIntoRefusesNonEmptyFolder(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepo("mono")
	before := e.CommitDirect("mono", map[string]string{"svc/app/x": "x"}, "seed")
	src, _ := srcRepo(t, map[string]string{"a.txt": "a"})
	_, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{
		FromUrl: "file://" + src, Name: "app", IntoRepo: "mono", IntoFolder: "svc/app"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "not empty") {
		t.Fatalf("err = %v, want FailedPrecondition not empty", err)
	}
	g := e.Git("mono")
	defer g.Close()
	if got := testenv.Must(g.ReadRef(ctx, gitx.MainRef)); got != before {
		t.Fatalf("mono main moved to %s", got)
	}
	if _, err := e.Store.ResolveProject(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project app created: %v", err)
	}
	if refs := tempRefs(t, e, "mono"); refs != "" {
		t.Fatalf("temporary refs left: %s", refs)
	}
}

func TestMigrateIntoLockReleased(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepo("mono")
	e.CommitDirect("mono", map[string]string{"svc/x": "x"}, "seed")
	src, _ := srcRepo(t, map[string]string{"a.txt": "a"})
	ac := e.Admin(e.AdminToken)
	if _, err := ac.MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "one", IntoRepo: "mono", IntoFolder: "svc"}); err == nil {
		t.Fatal("first MigrateIn succeeded; want not empty")
	}
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := ac.MigrateIn(ctx2, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "two", IntoRepo: "mono", IntoFolder: "lib/two"}); err != nil {
		t.Fatalf("second MigrateIn after a refused one: %v", err)
	}
}

func TestMigrateIntoErrors(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepo("mono")
	ac := e.Admin(e.AdminToken)
	src, main := srcRepo(t, map[string]string{"a.txt": "a"})
	if _, err := ac.MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "x", IntoRepo: "nope", IntoFolder: "f"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown repo: %v", err)
	}
	noMain := filepath.Join(t.TempDir(), "m.git")
	if err := gitx.InitBare(ctx, noMain); err != nil {
		t.Fatal(err)
	}
	r := gitx.Open(src)
	if err := r.Push(ctx, noMain, main, "", true); err != nil {
		t.Fatal(err)
	}
	r.Close()
	m := gitx.Open(noMain)
	if err := m.UpdateRef(ctx, "refs/heads/master", main, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteRef(ctx, gitx.MainRef); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, err := ac.MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + noMain, Name: "x", IntoRepo: "mono", IntoFolder: "f"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no main: %v", err)
	}
	if refs := tempRefs(t, e, "mono"); refs != "" {
		t.Fatalf("temporary refs left: %s", refs)
	}
	if _, err := e.Store.ResolveProject(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project x created: %v", err)
	}
}

func TestMigrateIntoMountFailureKeepsCommits(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	e.CreateRepo("mono")
	e.CreateRepoProject("core")
	src, _ := srcRepo(t, map[string]string{"a.txt": "a"})
	_, err := e.Admin(e.AdminToken).MigrateIn(ctx, &sgitv1.MigrateInRequest{
		FromUrl: "file://" + src, Name: "app", IntoRepo: "mono", IntoFolder: "svc/app", Project: "core", Path: "lib"})
	if err == nil || !strings.Contains(status.Convert(err).Message(), "the code landed at mono:svc/app") {
		t.Fatalf("err = %v, want the code landed at mono:svc/app", err)
	}
	if files := e.Files("mono", "main"); files["svc/app/a.txt"] != "a" {
		t.Fatalf("mono files = %v", files)
	}
	if _, err := e.Store.ResolveProject(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project app exists: %v", err)
	}
	if refs := tempRefs(t, e, "mono"); refs != "" {
		t.Fatalf("temporary refs left: %s", refs)
	}
}

func TestMigrateOut(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	ac := e.Admin(e.AdminToken)
	src, main := srcRepo(t, map[string]string{"a.txt": "a"})
	testenv.Must(ac.MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "file://" + src, Name: "app"}))
	dst := filepath.Join(t.TempDir(), "out.git")
	if err := gitx.InitBare(ctx, dst); err != nil {
		t.Fatal(err)
	}
	remoteMain := func() string { return testenv.Must(gitx.RemoteRef(ctx, dst, gitx.MainRef, true)) }

	// round trip: the target gets the original history, SHAs unchanged
	out := testenv.Must(ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: "app", ToUrl: "file://" + dst}))
	if out.Project != "app" || out.Pushed != main || out.UpToDate || remoteMain() != main {
		t.Fatalf("first MigrateOut = %+v, remote %s", out, remoteMain())
	}
	if out = testenv.Must(ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: "app", ToUrl: "file://" + dst})); !out.UpToDate || out.Pushed != main {
		t.Fatalf("second MigrateOut = %+v", out)
	}
	// a new land is caught up and fast-forwarded
	next := e.CommitDirect("app", map[string]string{"n.txt": "n"}, "next")
	if out = testenv.Must(ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: "app", ToUrl: "file://" + dst})); out.Pushed != next || remoteMain() != next {
		t.Fatalf("after land: %+v, remote %s", out, remoteMain())
	}
	// the target diverges: refused, then forced
	d := gitx.Open(dst)
	tree := testenv.Must(d.BuildTree(ctx, next, []gitx.Change{{Path: "z", Op: gitx.OpAdd, BlobSHA: testenv.Must(d.WriteBlob(ctx, []byte("z")))}}))
	sig := gitx.Signature{Name: "Zed", Email: "z@example.com", When: time.Unix(1800000000, 0)}
	theirs := testenv.Must(d.CommitTree(ctx, tree, []string{next}, "theirs\n", sig, sig))
	if err := d.UpdateRef(ctx, gitx.MainRef, theirs, next); err != nil {
		t.Fatal(err)
	}
	d.Close()
	again := e.CommitDirect("app", map[string]string{"m.txt": "m"}, "again")
	_, err := ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: "app", ToUrl: "file://" + dst})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "--force") {
		t.Fatalf("diverged: %v", err)
	}
	if remoteMain() != theirs {
		t.Fatalf("a refused push changed the target")
	}
	if out = testenv.Must(ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: "app", ToUrl: "file://" + dst, Force: true})); out.Pushed != again || remoteMain() != again {
		t.Fatalf("forced: %+v, remote %s", out, remoteMain())
	}
}

func TestMigrateOutErrors(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	ac := e.Admin(e.AdminToken)
	e.CreateProject("empty")
	dst := filepath.Join(t.TempDir(), "out.git")
	if err := gitx.InitBare(ctx, dst); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  *sgitv1.MigrateOutRequest
		code codes.Code
	}{
		{"empty project", &sgitv1.MigrateOutRequest{Project: "empty", ToUrl: "file://" + dst}, codes.FailedPrecondition},
		{"unknown project", &sgitv1.MigrateOutRequest{Project: "nope", ToUrl: "file://" + dst}, codes.NotFound},
		{"no url", &sgitv1.MigrateOutRequest{Project: "empty"}, codes.InvalidArgument},
		{"credentials", &sgitv1.MigrateOutRequest{Project: "empty", ToUrl: "https://u:p@example.com/x.git"}, codes.InvalidArgument},
		{"scheme", &sgitv1.MigrateOutRequest{Project: "empty", ToUrl: "ftp://example.com/x.git"}, codes.InvalidArgument},
	}
	for _, c := range cases {
		if _, err := ac.MigrateOut(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
	if got := testenv.Must(gitx.RemoteRef(ctx, dst, gitx.MainRef, true)); got != "" {
		t.Fatalf("target got main %s", got)
	}
}

func TestMigrateNeedsAdmin(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t, allowLocal)
	ac := e.Admin(e.NewUser("bob", "", ""))
	if _, err := ac.MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: "https://example.com/x.git", Name: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("MigrateIn as non-admin = %v", err)
	}
	if _, err := ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: "x", ToUrl: "https://example.com/x.git"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("MigrateOut as non-admin = %v", err)
	}
}
