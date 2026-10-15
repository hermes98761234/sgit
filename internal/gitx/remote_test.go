package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateRemote(t *testing.T) {
	good := []string{
		"https://github.com/acme/web.git",
		"ssh://git@github.com/acme/web.git",
		"ssh://git@host.example:2222/web",
		"git@github.com:acme/web.git",
		"ssh://git@[::1]/x",
	}
	bad := []string{
		"",
		"-uhelp",
		"--upload-pack=touch /tmp/x",
		"https://user:pw@github.com/acme/web",
		"https://tok@github.com/acme/web",
		"ssh://u:pw@host/web",
		"ssh://-oProxyCommand=x/web",
		"ssh://-o@host/web",
		"http://github.com/acme/web",
		"ftp://host/web",
		"ext::sh",
		"ext::sh -c touch% /tmp/x",
		"fd::3",
		"https:///web",
		"https://host/a b",
		"git@-oProxy:web",
		"relative/path",
		"file:///tmp/r.git",
		"/tmp/r.git",
		"HTTPS://github.com/a/b",
		"SSH://git@h/x",
		"ssh://%2Do@h/x",
		"ssh://%2Dh/x",
		"ssh://[-o]/x",
		"https::github.com/a",
		"git@host:a::b",
		"https://github.com/a\u202e",
		"https://github.com/a b",
		"https://h/r.git?access_token=x",
		"https://h/r.git#x",
		"ssh://git@h/r.git?x=1",
	}
	for _, u := range good {
		if err := ValidateRemote(u, false); err != nil {
			t.Errorf("ValidateRemote(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range bad {
		if err := ValidateRemote(u, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateRemote(%q) = %v, want ErrInvalid", u, err)
		}
	}
	for _, u := range []string{"file:///tmp/r.git", "/tmp/r.git"} {
		if err := ValidateRemote(u, true); err != nil {
			t.Errorf("ValidateRemote(%q, allowLocal) = %v, want nil", u, err)
		}
	}
	if err := ValidateRemote("relative/path", true); !errors.Is(err, ErrInvalid) {
		t.Errorf("relative path with allowLocal = %v, want ErrInvalid", err)
	}
}

func TestImportTakesMainAndReleases(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	main, err := src.ReadRef(ctx, MainRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/release/1.0", "refs/heads/feature/x", "refs/tags/v1"} {
		if err := src.UpdateRef(ctx, ref, main, ""); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "imp.git")
	got, err := Import(ctx, dir, src.Dir(), true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got != main {
		t.Fatalf("Import main = %s, want %s", got, main)
	}
	dst := Open(dir)
	defer dst.Close()
	if sha, err := dst.ReadRef(ctx, "refs/heads/release/1.0"); err != nil || sha != main {
		t.Errorf("release/1.0 = %q, %v; want %s", sha, err, main)
	}
	for _, ref := range []string{"refs/heads/feature/x", "refs/tags/v1"} {
		if _, err := dst.ReadRef(ctx, ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", ref, err)
		}
	}
}

func TestImportWithoutMainFails(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "empty.git")
	if err := InitBare(ctx, src); err != nil {
		t.Fatal(err)
	}
	_, err := Import(ctx, filepath.Join(t.TempDir(), "imp.git"), src, true)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Import from a repo without main = %v, want ErrInvalid", err)
	}
}

func TestImportRefusesLocalWhenDisallowed(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	dir := filepath.Join(t.TempDir(), "imp.git")
	if _, err := Import(ctx, dir, src.Dir(), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Import = %v, want ErrInvalid", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Import created %s before validating the URL (stat err %v)", dir, err)
	}
}

func TestPushLease(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	c0, err := src.ReadRef(ctx, MainRef)
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := InitBare(ctx, remote); err != nil {
		t.Fatal(err)
	}
	url := "file://" + remote
	if got, err := RemoteRef(ctx, url, MainRef, true); err != nil || got != "" {
		t.Fatalf("RemoteRef on empty remote = %q, %v; want \"\", nil", got, err)
	}
	if err := src.Push(ctx, url, c0, "", true); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if got, _ := RemoteRef(ctx, url, MainRef, true); got != c0 {
		t.Fatalf("remote main = %q, want %s", got, c0)
	}
	c1, err := src.CommitTree(ctx, EmptyTree, []string{c0}, "two\n", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	// The remote has main now, so "must not exist" is a stale lease.
	if err := src.Push(ctx, url, c1, "", true); !errors.Is(err, ErrRejected) {
		t.Fatalf("push with lease \"\" = %v, want ErrRejected", err)
	}
	// A lease naming the wrong commit is stale too.
	if err := src.Push(ctx, url, c1, c1, true); !errors.Is(err, ErrRejected) {
		t.Fatalf("push with wrong lease = %v, want ErrRejected", err)
	}
	if got, _ := RemoteRef(ctx, url, MainRef, true); got != c0 {
		t.Fatalf("rejected push changed remote main to %s", got)
	}
	if err := src.Push(ctx, url, c1, c0, true); err != nil {
		t.Fatalf("push with correct lease: %v", err)
	}
	if got, _ := RemoteRef(ctx, url, MainRef, true); got != c1 {
		t.Fatalf("remote main = %q, want %s", got, c1)
	}
}

func TestPushValidates(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	c0, _ := src.ReadRef(ctx, MainRef)
	if err := src.Push(ctx, "/tmp/x.git", c0, "", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("local push without allowLocal = %v, want ErrInvalid", err)
	}
	if err := src.Push(ctx, "https://example.com/x.git", "main", "", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("push of a non-SHA = %v, want ErrInvalid", err)
	}
}

func TestErrSummary(t *testing.T) {
	ge := &GitError{Args: []string{"push"}, ExitCode: 128, Stderr: "fatal: one\n  two\n"}
	if got := ErrSummary(ge); got != "fatal: one two" {
		t.Errorf("ErrSummary(GitError) = %q", got)
	}
	if got := ErrSummary(errors.New("plain")); got != "plain" {
		t.Errorf("ErrSummary(plain) = %q", got)
	}
}

func TestRemoteEnvSSH(t *testing.T) {
	has := func(env []string, p string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, p) {
				return true
			}
		}
		return false
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	if !has(remoteEnv(false), "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Error("BatchMode not set by default")
	}
	t.Setenv("GIT_SSH_COMMAND", "ssh -i k")
	if has(remoteEnv(false), "GIT_SSH_COMMAND=") {
		t.Error("operator GIT_SSH_COMMAND overridden")
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "/usr/local/bin/my-ssh")
	if has(remoteEnv(false), "GIT_SSH_COMMAND=") {
		t.Error("GIT_SSH_COMMAND set although the operator set GIT_SSH")
	}
}

func TestFetchMainAndDeleteRef(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	main := commitChanges(t, src, []Change{{Path: "a", Op: OpAdd, BlobSHA: blob(t, src, "a")}})
	dst := newRepo(t)
	got, err := dst.FetchMain(ctx, src.Dir(), "refs/sgit/migrate/t1", true)
	if err != nil || got != main {
		t.Fatalf("FetchMain = %q, %v; want %s", got, err, main)
	}
	if _, err := dst.ReadCommit(ctx, main); err != nil {
		t.Fatalf("objects not fetched: %v", err)
	}
	if err := dst.DeleteRef(ctx, "refs/sgit/migrate/t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ReadRef(ctx, "refs/sgit/migrate/t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ref after delete: %v", err)
	}
	if err := dst.DeleteRef(ctx, "refs/sgit/migrate/t1"); err != nil {
		t.Fatalf("deleting a missing ref: %v", err)
	}
	if _, err := dst.FetchMain(ctx, src.Dir(), "refs/sgit/migrate/t2", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("local remote not allowed: %v", err)
	}
	if _, err := dst.FetchMain(ctx, src.Dir(), "main", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short ref name: %v", err)
	}
	// a repo whose only branch is master
	noMain := filepath.Join(t.TempDir(), "m.git")
	if _, err := runGit(ctx, nil, nil, "init", "--bare", "--quiet", "--initial-branch=master", noMain); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, nil, nil, "--git-dir", src.Dir(), "push", "--quiet", noMain, main+":refs/heads/master"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.FetchMain(ctx, noMain, "refs/sgit/migrate/t3", true); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no main branch") {
		t.Fatalf("no main: %v", err)
	}
}

func TestRemoteBranches(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	main, _ := src.ReadRef(ctx, MainRef)
	for _, ref := range []string{"refs/heads/release/2.0", "refs/heads/release/1.0", "refs/heads/feature/x", "refs/tags/release/9"} {
		if err := src.UpdateRef(ctx, ref, main, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RemoteBranches(ctx, src.Dir(), "release/", true)
	if err != nil || !reflect.DeepEqual(got, []string{"release/1.0", "release/2.0"}) {
		t.Fatalf("RemoteBranches = %v, %v", got, err)
	}
	if _, err := RemoteBranches(ctx, src.Dir(), "release/", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("local not allowed: %v", err)
	}
}

func TestPushFF(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	c1 := commitChanges(t, src, []Change{{Path: "a", Op: OpAdd, BlobSHA: blob(t, src, "1")}})
	dir := filepath.Join(t.TempDir(), "dst.git")
	if err := InitBare(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := src.PushFF(ctx, dir, c1, true); err != nil {
		t.Fatalf("PushFF new: %v", err)
	}
	c2 := commitChanges(t, src, []Change{{Path: "a", Op: OpModify, BlobSHA: blob(t, src, "2")}})
	if err := src.PushFF(ctx, dir, c2, true); err != nil {
		t.Fatalf("PushFF fast-forward: %v", err)
	}
	other := sideCommit(t, src, c1, []Change{{Path: "b", Op: OpAdd, BlobSHA: blob(t, src, "b")}}, "b\n", testSig)
	if err := src.PushFF(ctx, dir, other, true); !errors.Is(err, ErrRejected) {
		t.Fatalf("PushFF diverged: %v, want ErrRejected", err)
	}
	if got, _ := RemoteRef(ctx, dir, MainRef, true); got != c2 {
		t.Fatalf("remote main = %s, want %s", got, c2)
	}
	if err := src.PushFF(ctx, "https://user:pw@example.com/x", c2, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("credentials in URL: %v", err)
	}
}
