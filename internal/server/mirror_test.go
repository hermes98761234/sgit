package server_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func mirrorEnv(t *testing.T) *testenv.Env {
	return testenv.Start(t, func(c *server.Config) {
		c.AllowLocalRemotes = true
		c.MirrorBackoffMin = 5 * time.Millisecond
		c.MirrorBackoffMax = 50 * time.Millisecond
	})
}

// bareRemote makes an empty bare repo standing in for GitHub.
func bareRemote(t *testing.T) (path, url string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "remote.git")
	if err := gitx.InitBare(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	return path, "file://" + path
}

// sourceRepo makes a repo with main, release/1.0, feature/x and tag v1, and
// returns its path and main.
func sourceRepo(t *testing.T) (path, main string) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "src.git")
	r := testenv.Must(gitx.Init(ctx, path, gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000000, 0)}))
	defer r.Close()
	main = testenv.Must(r.ReadRef(ctx, gitx.MainRef))
	for _, ref := range []string{"refs/heads/release/1.0", "refs/heads/feature/x", "refs/tags/v1"} {
		if err := r.UpdateRef(ctx, ref, main, ""); err != nil {
			t.Fatal(err)
		}
	}
	return path, main
}

func remoteMain(t *testing.T, path string) string {
	t.Helper()
	g := gitx.Open(path)
	defer g.Close()
	sha, err := g.ReadRef(context.Background(), gitx.MainRef)
	if err != nil {
		return ""
	}
	return sha
}

func waitStatus(t *testing.T, ac sgitv1.AdminClient, repo string, cond func(*sgitv1.RepoInfo) bool) *sgitv1.RepoInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := ac.RepoStatus(context.Background(), &sgitv1.RepoStatusRequest{Name: repo})
		if err != nil {
			t.Fatal(err)
		}
		if cond(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last status %+v", info)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func landFile(t *testing.T, e *testenv.Env, repo, base, path, content string) string {
	t.Helper()
	shas := e.Upload(e.AdminToken, repo, []byte(content))
	resp := testenv.Must(e.Repo(e.AdminToken).CommitAndLand(context.Background(), &sgitv1.CommitAndLandRequest{
		Repo: repo, Base: base, Changes: []*sgitv1.Change{testenv.Add(path, shas[0])}, Message: "add " + path,
	}))
	return resp.Commit
}

func TestLandIsMirrored(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	path, url := bareRemote(t)
	cr := testenv.Must(ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "web", MirrorUrl: url}))
	waitStatus(t, ac, "web", func(s *sgitv1.RepoInfo) bool { return s.MirrorLast == cr.Main && !s.MirrorPending })

	c := landFile(t, e, "web", cr.Main, "a.txt", "hello\n")
	info := waitStatus(t, ac, "web", func(s *sgitv1.RepoInfo) bool { return s.MirrorLast == c && !s.MirrorPending })
	if info.Main != c || info.MirrorUrl != url || info.MirrorError != "" || info.MirrorAttemptUnix == 0 || info.CreatedUnix == 0 {
		t.Fatalf("status = %+v", info)
	}
	if got := remoteMain(t, path); got != c {
		t.Fatalf("mirror main = %s, want %s", got, c)
	}
}

func TestImportKeepsMainAndReleases(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	src, main := sourceRepo(t)
	resp := testenv.Must(ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: "file://" + src}))
	if resp.Main != main {
		t.Fatalf("imported main = %s, want %s", resp.Main, main)
	}
	g := e.Git("app")
	defer g.Close()
	if sha, err := g.ReadRef(ctx, "refs/heads/release/1.0"); err != nil || sha != main {
		t.Errorf("release/1.0 = %q, %v", sha, err)
	}
	for _, ref := range []string{"refs/heads/feature/x", "refs/tags/v1"} {
		if _, err := g.ReadRef(ctx, ref); !errors.Is(err, gitx.ErrNotFound) {
			t.Errorf("%s imported (err %v)", ref, err)
		}
	}
	info := testenv.Must(ac.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: "app"}))
	if info.Main != main || info.MirrorUrl != "" || info.MirrorPending {
		t.Fatalf("status = %+v", info)
	}
	// The imported repo works like any other: land on top of it.
	landFile(t, e, "app", main, "b.txt", "b\n")
}

func TestImportFromItsMirrorIsInSync(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	src, main := sourceRepo(t)
	url := "file://" + src
	testenv.Must(ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: url, MirrorUrl: url}))
	info := testenv.Must(ac.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: "app"}))
	if info.MirrorLast != main || info.MirrorPending {
		t.Fatalf("status right after import = %+v, want mirror_last %s and not pending", info, main)
	}
	c := landFile(t, e, "app", main, "c.txt", "c\n")
	waitStatus(t, ac, "app", func(s *sgitv1.RepoInfo) bool { return s.MirrorLast == c && s.MirrorError == "" })
	if got := remoteMain(t, src); got != c {
		t.Fatalf("source main = %s, want %s", got, c)
	}
}

func TestImportRejectsBadURLs(t *testing.T) {
	e := testenv.Start(t) // local remotes not allowed
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	cases := []struct{ from, mirror string }{
		{"", ""},
		{"file:///tmp/src.git", ""},
		{"/tmp/src.git", ""},
		{"https://u:pw@example.com/r.git", ""},
		{"ext::sh", ""},
		{"-uhelp", ""},
		{"https://example.com/r.git", "https://tok@example.com/m.git"},
		{"https://example.com/r.git", "/tmp/m.git"},
	}
	for _, c := range cases {
		_, err := ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: c.from, MirrorUrl: c.mirror})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("ImportRepo(from=%q, mirror=%q) = %v, want InvalidArgument", c.from, c.mirror, err)
		}
	}
	_, err := ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "web", MirrorUrl: "file:///tmp/m.git"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateRepo with a local mirror = %v, want InvalidArgument", err)
	}
	if list := testenv.Must(ac.ListRepos(ctx, &sgitv1.ListReposRequest{})); len(list.Repos) != 0 {
		t.Fatalf("rejected requests created repos: %+v", list.Repos)
	}
}

func TestFailedImportLeavesNothing(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	dir := filepath.Join(e.DataDir, "repos", "app.git")
	assertGone := func(what string) {
		t.Helper()
		if _, err := ac.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: "app"}); status.Code(err) != codes.NotFound {
			t.Fatalf("%s: RepoStatus = %v, want NotFound", what, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %s left behind (stat err %v)", what, dir, err)
		}
	}

	missing := "file://" + filepath.Join(t.TempDir(), "missing.git")
	_, err := ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: missing})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("import from a missing repo = %v, want FailedPrecondition", err)
	}
	assertGone("missing source")

	empty, _ := bareRemote(t) // a repo with no main
	_, err = ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: "file://" + empty})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("import from a repo without main = %v, want InvalidArgument", err)
	}
	assertGone("no main")

	src, main := sourceRepo(t)
	resp := testenv.Must(ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: "file://" + src}))
	if resp.Main != main {
		t.Fatalf("retry import main = %s, want %s", resp.Main, main)
	}
}

func TestMirrorErrorIsReported(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	missing := "file://" + filepath.Join(t.TempDir(), "missing.git")
	testenv.Must(ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "web", MirrorUrl: missing}))
	waitStatus(t, ac, "web", func(s *sgitv1.RepoInfo) bool {
		return s.MirrorError != "" && s.MirrorPending && s.MirrorAttemptUnix != 0 && s.MirrorLast == ""
	})
}

func TestListRepos(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	_, url := bareRemote(t)
	testenv.Must(ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "b"}))
	testenv.Must(ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "a", MirrorUrl: url}))
	list := testenv.Must(ac.ListRepos(ctx, &sgitv1.ListReposRequest{}))
	if len(list.Repos) != 2 || list.Repos[0].Name != "a" || list.Repos[1].Name != "b" {
		t.Fatalf("ListRepos = %+v, want [a b]", list.Repos)
	}
	if list.Repos[0].MirrorUrl != url || list.Repos[1].MirrorUrl != "" || len(list.Repos[1].Main) != 40 {
		t.Fatalf("ListRepos = %+v", list.Repos)
	}
}

func TestRepoAdminRPCsNeedAdmin(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	e.CreateRepo("web")
	bob := e.Admin(e.NewUser("bob", "web", "write"))
	src, _ := sourceRepo(t)
	calls := map[string]error{}
	_, calls["ImportRepo"] = bob.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: "file://" + src})
	_, calls["RepoStatus"] = bob.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: "web"})
	_, calls["ListRepos"] = bob.ListRepos(ctx, &sgitv1.ListReposRequest{})
	_, calls["CreateRepo"] = bob.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "x"})
	for name, err := range calls {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s as bob = %v, want PermissionDenied", name, err)
		}
	}
}

func TestConcurrentCreateOneWins(t *testing.T) {
	e := mirrorEnv(t)
	ac := e.Admin(e.AdminToken)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = ac.CreateRepo(context.Background(), &sgitv1.CreateRepoRequest{Name: "race"})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch status.Code(err) {
		case codes.OK:
			ok++
		case codes.AlreadyExists:
		default:
			t.Errorf("CreateRepo = %v, want OK or AlreadyExists", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d creates succeeded, want exactly 1", ok)
	}
}

func TestImportNameConflicts(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	src, _ := sourceRepo(t)
	from := "file://" + src

	e.CreateRepo("web")
	_, err := ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "web", FromUrl: from})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("import onto existing repo = %v, want AlreadyExists", err)
	}

	stray := filepath.Join(e.DataDir, "repos", "app.git")
	if err := os.MkdirAll(stray, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stray, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err = ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "app", FromUrl: from})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("import onto stray dir = %v, want AlreadyExists", err)
	}
	_, err = ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "app"})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("create onto stray dir = %v, want AlreadyExists", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker file gone: %v", err)
	}

	_, err = ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "Bad/Name", FromUrl: from})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("import with invalid name = %v, want InvalidArgument", err)
	}
}

func TestListReposSurvivesBrokenRepo(t *testing.T) {
	e := mirrorEnv(t)
	ctx := context.Background()
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "a"}))
	testenv.Must(ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "b"}))
	if err := os.RemoveAll(filepath.Join(e.DataDir, "repos", "b.git")); err != nil {
		t.Fatal(err)
	}
	list := testenv.Must(ac.ListRepos(ctx, &sgitv1.ListReposRequest{}))
	if len(list.Repos) != 2 {
		t.Fatalf("ListRepos = %+v", list.Repos)
	}
	a, b := list.Repos[0], list.Repos[1]
	if a.Name != "a" || a.Error != "" || len(a.Main) != 40 {
		t.Errorf("a = %+v", a)
	}
	if b.Name != "b" || b.Error == "" || b.Main != "" {
		t.Errorf("b = %+v", b)
	}
}
