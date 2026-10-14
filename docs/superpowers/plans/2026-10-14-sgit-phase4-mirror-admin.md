# sgit Phase 4: Mirror, Import and Admin Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish sgit v1:
- `sg-server` can import a repo from a git URL.
- It mirrors every repo's `main` to an optional `mirror_url` (GitHub) in the background, and never overwrites commits made on the mirror outside sgit.
- It reports mirror health.
- `sg` gains the admin commands `repo`, `token` and `acl`.
- The Docker image ships `sg` and a GIT_ASKPASS helper, and a release workflow publishes multi-arch images to ghcr.

**Architecture:**
- `internal/gitx` gains remote URL validation, `InitBare`, `Import`, `Push` (lease-protected) and `RemoteRef`.
- `internal/store` gains a `mirror_state` table.
- A new `internal/mirror` package runs one worker goroutine per mirrored repo. It is kicked by `Notify(repo)` after each land, create or import, and by a catch-up pass at server start.
- `internal/server` wires the manager in and adds the `ImportRepo`, `RepoStatus` and `ListRepos` Admin RPCs. `CreateRepo` gains `mirror_url`.
- The CLI admin commands call the Admin service directly and never use sgd.

**Tech Stack:** Go, git CLI (`fetch`, `push --force-with-lease`, `ls-remote`), gRPC/protobuf, SQLite (`modernc.org/sqlite`), Docker buildx, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-10-14-sgit-design.md`. Phase 4 is §12 item 4. Mirror and import are §8, the admin CLI is §4, and Docker is §13 (mirror credentials, ghcr publishing, `sg` in the image).

## Global Constraints

- **Shell setup.** Every shell command assumes:
  ```bash
  cd /home/vkopytsia/Projects/new/sgit
  export PATH=$HOME/.local/go/bin:$HOME/.local/protoc/bin:$HOME/go/bin:$PATH
  ```
  If a build fails with ENOSPC, use `export GOCACHE=/dev/shm/gocache`, and `rm -rf /dev/shm/gocache` when you are done.
- **No git in the project dir.** The project dir is not a git repo. Never run `git init/add/commit/push` there; tests create their own git repos under `t.TempDir()`. Implementers do not push; the controller publishes after review. The "Commit" steps below mean "stop and report".
- **Real names.** Never write anyone's real name. Test users are `admin`, `bob`, `carol` and `eve`. Commit signatures in tests use `admin <admin@sgit.local>`.
- **Remote URLs** (import `from` and `mirror_url`) are checked by `gitx.ValidateRemote` before git ever sees them.
  - Accepted: `https://host/...`, `ssh://[user@]host[:port]/...` and scp-style `user@host:path`.
  - `file://` URLs and absolute paths are accepted only when `server.Config.AllowLocalRemotes` is true. That setting is for tests and has no CLI flag.
  - Refused: credentials in a URL (any userinfo on https, a password on ssh), a leading `-`, whitespace or control characters, `::` remote-helper syntax, and any other scheme (including `http`).
  - git always runs with `GIT_ALLOW_PROTOCOL=https:ssh`, plus `:file` when local remotes are allowed.
- **Import** fetches exactly `refs/heads/main` and `refs/heads/release/*`, with no tags, into a new bare repo. A source without `main` is an `InvalidArgument` error. A failed import leaves no repo row and no directory.
- **Mirror push.**
  - Command: `git push --porcelain --no-verify --force-with-lease=refs/heads/main:<last_mirrored> <url> <main-sha>:refs/heads/main`. An empty `last_mirrored` means the mirror must not have `main` yet.
  - One worker per repo.
  - Backoff after a failure: 1 s, doubling, capped at 5 min, reset by a success.
  - Each push has a 10 min timeout.
  - A server start queues every repo that has a `mirror_url`.
- **Mirror status.** `pending` as reported by `RepoStatus` means `main != last_mirrored`. The mirror error strings are exactly the ones in Task 3's `describe`.
- **Authorization.** Every new Admin RPC requires an admin (`requireAdmin`). The CLI's `repo`, `token` and `acl` commands never dial sgd.
- **Image.** `ghcr.io/hermes98761234/sgit-server`, platforms `linux/amd64,linux/arm64`, published on tags `v*`.
- **Lint and tests.** `gofmt -l .` must print nothing, and `go vet ./...`, `staticcheck ./...` and `go test -race -count=1 ./...` must all be clean before a task is done.
- **testenv.Must** is called as `Must(f())`. It fails the test from the calling goroutine, so it must never be called inside a goroutine.

### Decisions beyond the spec (for review)
- **Import uses `init --bare` plus a refspec fetch,** not `clone --bare --single-branch`. `--single-branch` can take only one branch. The refspec fetch takes exactly `main` and `release/*`, with no tags, which is what §8 asks for.
- **Mirror state lives in a new `mirror_state` table** created with `CREATE TABLE IF NOT EXISTS`. The store has no migration machinery, and existing databases gain the table on their next `Open`. The `mirror_url` column already exists on `repos`.
- **`pending` is computed** as `main != last_mirrored` when status is read. The stored `pending` column (spec §8) is the worker's own view and can lag after a crash. The computed value cannot.
- **The worker pushes a SHA snapshot** (`<sha>:refs/heads/main`), not the `main` ref. What gets recorded as `last_mirrored` is then exactly what was pushed, even if `main` moves during the push.
- **Stale-lease recovery.** When a push is rejected, the worker runs `ls-remote`. If the mirror's `main` already equals our `main`, it records success. This covers a crash between a successful push and the database update, which would otherwise wedge the mirror forever.
- **A new mirror must start empty** (no `main`). There is one exception: `repo import X --from U --mirror U` records `last_mirrored = imported main`, so mirroring back to the source just works.
- **No URL credentials, ever.** They would sit in SQLite and appear in `repo status`. HTTPS mirrors use a `GIT_ASKPASS` helper (`sg-git-askpass`, shipped in the image) that reads a token from the environment or a file.
- **Concurrent create/import of the same name** is serialized by a per-name reservation in `repoManager`. Before this change, two creates could both pass the existence check and race in the same directory.
- **Deferred:** changing or removing a repo's `mirror_url` after creation. This needs a new RPC and a lease reset, and is not in spec §4.
- **`RepoStatus` and `ListRepos` are admin-only,** matching §4's "(admin)" marker.
- **`sg token create` prints only the token on stdout,** with the token id on stderr, so `TOKEN=$(sg token create bob)` works.
- **The image adds `sg` but not `sgd`.** sgd is a per-user workstation daemon (spec §13).
- **The release workflow** re-runs the tests before building. `latest` is moved only for tags without a `-` (no prereleases).

## Review Focus

These are the five conditions most likely to hurt a user if broken, each pinned by a test in the task named:
1. **A mirror whose `main` was changed outside sgit, or which was not empty, is never overwritten.** The error is visible in `repo status`. Pinned in Task 1 (`TestPushLease`), and in Task 3 (`TestLeaseMismatchIsNotOverwritten`, `TestNonEmptyMirrorIsNotOverwritten`).
2. **A push that succeeded but was never recorded (a crash) must not wedge the mirror.** Pinned in Task 3 (`TestAlreadyPushedIsRecorded`).
3. **Hostile or careless URLs** never reach git: credentials, local paths, `ext::` helpers, `-o…` option injection. Pinned in Task 1 (`TestValidateRemote`) and Task 4 (`TestImportRejectsBadURLs`).
4. **A failed import (unreachable source, no `main`) leaves nothing behind,** and the name can be reused. Pinned in Task 4 (`TestFailedImportLeavesNothing`).
5. **Server shutdown is not held up by a mirror in backoff,** and a remote that comes back is caught up. Pinned in Task 3 (`TestCloseDuringBackoff`, `TestRetriesUntilMirrorExists`, `TestStartCatchesUp`).

---

## File Structure

```
internal/gitx/remote.go            (Task 1) ValidateRemote, InitBare, Import, Push, RemoteRef, ErrRejected, ErrSummary
internal/gitx/repo.go              (Task 1) Init uses InitBare
internal/gitx/remote_test.go       (Task 1)
internal/store/store.go            (Task 2) mirror_state table, MirrorState, CreateRepoWithMirror, Set/Record methods
internal/store/mirror_test.go      (Task 2)
internal/mirror/mirror.go          (Task 3) Config, Manager, New, Start, Notify, Close, worker
internal/mirror/mirror_test.go     (Task 3)
proto/sgit/v1/sgit.proto           (Task 4) ImportRepo, RepoStatus, ListRepos, RepoInfo, CreateRepo.mirror_url; gen/ regenerated
internal/server/server.go          (Task 4) Config mirror fields, Server.mirror wiring
internal/server/repos.go           (Task 4) reserve, create(mirror), importRepo
internal/server/admin.go           (Task 4) CreateRepo(mirror), ImportRepo, RepoStatus, ListRepos, repoInfo
internal/server/land.go            (Task 4) Notify after main moves
internal/server/mirror_test.go     (Task 4)
internal/cli/admin.go              (Task 5) repo/token/acl commands
internal/cli/cli.go                (Task 5) commandTable entries
internal/cli/session.go            (Task 5) target.adminOnly skips sgd
internal/cli/admin_test.go         (Task 5)
Dockerfile                         (Task 6) build sg too, cross-compile, ship sg-git-askpass
contrib/sg-git-askpass             (Task 6)
scripts/docker-smoke.sh            (Task 6) in-container sg + askpass checks
.github/workflows/release.yml      (Task 6)
docker-compose.yml                 (Task 6) commented mirror-credential examples
README.md                          (Task 6) Administration, Importing and mirroring
docs/superpowers/specs/2026-10-14-sgit-design.md (Task 6) phase-4 notes under §8
```

---

### Task 1: gitx remotes: validation, import, lease-protected push

**Files:**
- Create: `internal/gitx/remote.go`
- Modify: `internal/gitx/repo.go` (`Init` calls `InitBare`)
- Test: `internal/gitx/remote_test.go`

**Interfaces:**
- Consumes (existing): `runGit(ctx, stdin, extraEnv, args...) ([]byte, error)`, `(*Repo).run`, `Open`, `ReadRef`, `UpdateRef`, `CommitTree`, `IsSHA`, `*GitError{Args, ExitCode, Stderr}`, `ErrInvalid`, `ErrNotFound`, `MainRef`, `EmptyTree`, and the test helpers `newRepo(t)` and `testSig` from `repo_test.go`.
- Produces:
  - `var ErrRejected error`
  - `func ValidateRemote(raw string, allowLocal bool) error`: errors wrap `ErrInvalid`.
  - `func InitBare(ctx context.Context, dir string) error`
  - `func Import(ctx context.Context, dir, remote string, allowLocal bool) (main string, err error)`
  - `func (r *Repo) Push(ctx context.Context, remote, sha, expect string, allowLocal bool) error`
  - `func RemoteRef(ctx context.Context, remote, ref string, allowLocal bool) (string, error)`: returns `""` when the ref is absent.
  - `func ErrSummary(err error) string`

- [ ] **Step 1: Write the failing tests** in `internal/gitx/remote_test.go`:

```go
package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRemote(t *testing.T) {
	good := []string{
		"https://github.com/acme/web.git",
		"ssh://git@github.com/acme/web.git",
		"ssh://git@host.example:2222/web",
		"git@github.com:acme/web.git",
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gitx/ -run 'Remote|Import|Push|ErrSummary' -count=1`
Expected: FAIL to compile (`undefined: ValidateRemote`, `InitBare`, …).

- [ ] **Step 3: Implement** `internal/gitx/remote.go`:

```go
package gitx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrRejected means the remote refused a push, typically because its main no
// longer matched the --force-with-lease expectation.
var ErrRejected = errors.New("gitx: push rejected")

var scpRemoteRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9][A-Za-z0-9.-]*:[^:]+$`)

// ValidateRemote checks a URL that import or mirroring will hand to git. It
// accepts https://, ssh:// and user@host:path remotes, and file:// URLs or
// absolute paths only when allowLocal is set. Credentials in the URL are
// refused: the URL is stored in the database and shown by repo status.
func ValidateRemote(raw string, allowLocal bool) error {
	bad := func(why string) error { return fmt.Errorf("%w: remote %q: %s", ErrInvalid, raw, why) }
	if raw == "" || len(raw) > 2048 {
		return bad("must be 1-2048 bytes")
	}
	if strings.HasPrefix(raw, "-") {
		return bad("must not start with '-'")
	}
	for _, c := range raw {
		if c <= ' ' || c == 0x7f {
			return bad("must not contain spaces or control characters")
		}
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return bad("not a valid URL")
		}
		switch u.Scheme {
		case "https":
			if u.User != nil {
				return bad("credentials in the URL are not allowed; use GIT_ASKPASS or an SSH key")
			}
		case "ssh":
			if _, ok := u.User.Password(); ok {
				return bad("passwords in the URL are not allowed; use an SSH key")
			}
			if strings.HasPrefix(u.User.Username(), "-") {
				return bad("invalid user name")
			}
		case "file":
			if !allowLocal {
				return bad("local remotes are disabled")
			}
			return nil
		default:
			return bad("scheme must be https or ssh")
		}
		if h := u.Hostname(); h == "" || strings.HasPrefix(h, "-") {
			return bad("missing or invalid host")
		}
		return nil
	}
	if strings.Contains(raw, "::") {
		return bad("remote helpers are not allowed")
	}
	if filepath.IsAbs(raw) {
		if !allowLocal {
			return bad("local remotes are disabled")
		}
		return nil
	}
	if scpRemoteRe.MatchString(raw) {
		return nil
	}
	return bad("use https://, ssh:// or user@host:path")
}

// remoteEnv limits the transports git may use for a remote, including any
// redirects it follows.
func remoteEnv(allowLocal bool) []string {
	p := "https:ssh"
	if allowLocal {
		p += ":file"
	}
	return []string{"GIT_ALLOW_PROTOCOL=" + p}
}

// InitBare creates an empty bare repo at dir whose HEAD names main.
func InitBare(ctx context.Context, dir string) error {
	_, err := runGit(ctx, nil, nil, "init", "--bare", "--quiet", "--initial-branch=main", dir)
	return err
}

// Import creates a bare repo at dir holding remote's main and release/*
// branches (no tags) and returns main. On error the caller removes dir.
func Import(ctx context.Context, dir, remote string, allowLocal bool) (string, error) {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return "", err
	}
	if err := InitBare(ctx, dir); err != nil {
		return "", err
	}
	r := Open(dir)
	defer r.Close()
	_, err := r.run(ctx, nil, remoteEnv(allowLocal), "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote,
		"+"+MainRef+":"+MainRef, "+refs/heads/release/*:refs/heads/release/*")
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref "+MainRef) {
			return "", fmt.Errorf("%w: %s has no main branch", ErrInvalid, remote)
		}
		return "", err
	}
	return r.ReadRef(ctx, MainRef)
}

// Push sets remote's main to sha, but only while the remote's main is still
// expect ("" = main must not exist yet). A stale expectation returns an error
// wrapping ErrRejected and leaves the remote untouched.
func (r *Repo) Push(ctx context.Context, remote, sha, expect string, allowLocal bool) error {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return err
	}
	if !IsSHA(sha) || (expect != "" && !IsSHA(expect)) {
		return fmt.Errorf("%w: push %q over %q", ErrInvalid, sha, expect)
	}
	out, err := r.run(ctx, nil, remoteEnv(allowLocal), "push", "--porcelain", "--no-verify",
		"--force-with-lease="+MainRef+":"+expect, remote, sha+":"+MainRef)
	var ge *GitError
	if errors.As(err, &ge) && (strings.Contains(string(out), "[rejected]") || strings.Contains(ge.Stderr, "[rejected]")) {
		return fmt.Errorf("%w: %s", ErrRejected, strings.Join(strings.Fields(string(out)), " "))
	}
	return err
}

// RemoteRef returns the SHA that ref (a full ref name) points to on remote,
// or "" when the remote has no such ref.
func RemoteRef(ctx context.Context, remote, ref string, allowLocal bool) (string, error) {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return "", err
	}
	out, err := runGit(ctx, nil, remoteEnv(allowLocal), "ls-remote", "--refs", remote, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && name == ref && IsSHA(sha) {
			return sha, nil
		}
	}
	return "", nil
}

// ErrSummary is a one-line description of err for operators: git's stderr
// for a *GitError, err.Error() otherwise, with whitespace collapsed and the
// result capped at 500 bytes.
func ErrSummary(err error) string {
	msg := err.Error()
	var ge *GitError
	if errors.As(err, &ge) && strings.TrimSpace(ge.Stderr) != "" {
		msg = ge.Stderr
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:500], "") + "…"
	}
	return msg
}
```

In `internal/gitx/repo.go`, make `Init` call the new helper, so there is one `git init` invocation:

```go
func Init(ctx context.Context, dir string, sig Signature) (*Repo, error) {
	if err := InitBare(ctx, dir); err != nil {
		return nil, err
	}
	r := Open(dir)
	// ... rest unchanged
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/gitx/ -count=1`
Expected: PASS, the whole package. If `TestPushLease` fails because git reports rejection differently, print `out` and `ge.Stderr` from `Push` and fix the detection. Keep the test as written: the behavior it pins is the requirement.

- [ ] **Step 5: Lint, then report**

Run: `gofmt -l . && go vet ./internal/gitx/ && staticcheck ./internal/gitx/`
Expected: no output. Then write the report. Do not run git commit.

---

### Task 2: Store: mirror state

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/mirror_test.go`

**Interfaces:**
- Consumes (existing): `Store.db`, `isUnique`, `ErrExists`, `CreateRepo(ctx, name, mirrorURL)`.
- Produces:
  - `type MirrorState struct { Repo, LastMirrored, LastError string; LastAttempt time.Time; Pending bool }`
  - `func (s *Store) CreateRepoWithMirror(ctx context.Context, name, mirrorURL, lastMirrored string) error`. `CreateRepo` now delegates to it with `lastMirrored == ""`.
  - `func (s *Store) MirrorState(ctx context.Context, repo string) (MirrorState, error)`. With no row it returns `MirrorState{Repo: repo}` and a nil error.
  - `func (s *Store) SetMirrorPending(ctx context.Context, repo string, pending bool) error`
  - `func (s *Store) RecordMirrorSuccess(ctx context.Context, repo, sha string, at time.Time) error`
  - `func (s *Store) RecordMirrorFailure(ctx context.Context, repo, msg string, at time.Time) error`

- [ ] **Step 1: Write the failing tests** in `internal/store/mirror_test.go`:

```go
package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/store"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func openMirrorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMirrorStateDefaultsToEmpty(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.CreateRepo(ctx, "web", "https://example.com/web.git"); err != nil {
		t.Fatal(err)
	}
	got, err := st.MirrorState(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got != (store.MirrorState{Repo: "web"}) {
		t.Fatalf("MirrorState = %+v, want empty", got)
	}
}

func TestMirrorStateTransitions(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.CreateRepo(ctx, "web", "https://example.com/web.git"); err != nil {
		t.Fatal(err)
	}
	t1 := time.Unix(1700000000, 0).UTC()
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)

	if err := st.SetMirrorPending(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MirrorState(ctx, "web"); !got.Pending || got.LastMirrored != "" || !got.LastAttempt.IsZero() {
		t.Fatalf("after SetMirrorPending: %+v", got)
	}
	if err := st.RecordMirrorFailure(ctx, "web", "boom", t1); err != nil {
		t.Fatal(err)
	}
	want := store.MirrorState{Repo: "web", LastError: "boom", LastAttempt: t1, Pending: true}
	if got, _ := st.MirrorState(ctx, "web"); got != want {
		t.Fatalf("after failure: %+v, want %+v", got, want)
	}
	if err := st.RecordMirrorSuccess(ctx, "web", shaA, t2); err != nil {
		t.Fatal(err)
	}
	want = store.MirrorState{Repo: "web", LastMirrored: shaA, LastAttempt: t2}
	if got, _ := st.MirrorState(ctx, "web"); got != want {
		t.Fatalf("after success: %+v, want %+v", got, want)
	}
	if err := st.RecordMirrorFailure(ctx, "web", "again", t3); err != nil {
		t.Fatal(err)
	}
	want = store.MirrorState{Repo: "web", LastMirrored: shaA, LastError: "again", LastAttempt: t3, Pending: true}
	if got, _ := st.MirrorState(ctx, "web"); got != want {
		t.Fatalf("a failure must keep last_mirrored: %+v, want %+v", got, want)
	}
	if err := st.SetMirrorPending(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MirrorState(ctx, "web"); got.Pending || got.LastError != "again" {
		t.Fatalf("SetMirrorPending(false) must only clear pending: %+v", got)
	}
}

func TestCreateRepoWithMirrorSeedsState(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.CreateRepoWithMirror(ctx, "app", "https://example.com/app.git", shaA); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetRepo(ctx, "app")
	if err != nil || row.MirrorURL != "https://example.com/app.git" {
		t.Fatalf("GetRepo = %+v, %v", row, err)
	}
	if got, _ := st.MirrorState(ctx, "app"); got.LastMirrored != shaA || got.Pending {
		t.Fatalf("MirrorState = %+v, want last_mirrored %s", got, shaA)
	}
	err = st.CreateRepoWithMirror(ctx, "app", "https://example.com/other.git", shaB)
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate = %v, want ErrExists", err)
	}
	if got, _ := st.MirrorState(ctx, "app"); got.LastMirrored != shaA {
		t.Fatalf("a failed duplicate changed mirror state: %+v", got)
	}
}

func TestMirrorStateNeedsRepo(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.SetMirrorPending(ctx, "ghost", true); err == nil {
		t.Fatal("SetMirrorPending for a missing repo succeeded; want a foreign-key error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run Mirror -count=1`
Expected: FAIL to compile (`undefined: store.MirrorState`, …).

- [ ] **Step 3: Implement.** In `internal/store/store.go`:

(a) Append this to the `schema` constant, after the `drafts_repo_status` index line:

```sql
CREATE TABLE IF NOT EXISTS mirror_state (
  repo TEXT PRIMARY KEY REFERENCES repos(name),
  last_mirrored TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  last_attempt INTEGER NOT NULL DEFAULT 0,
  pending INTEGER NOT NULL DEFAULT 0
);
```

(b) Add the type next to `RepoRow`:

```go
// MirrorState is the background mirror's bookkeeping for one repo (spec §8).
type MirrorState struct {
	Repo         string
	LastMirrored string    // last commit pushed to the mirror; "" = never
	LastError    string    // last push error; "" after a success
	LastAttempt  time.Time // zero = never attempted
	Pending      bool      // the worker saw main ahead of LastMirrored
}
```

(c) Replace `CreateRepo` and add the mirror methods:

```go
func (s *Store) CreateRepo(ctx context.Context, name, mirrorURL string) error {
	return s.CreateRepoWithMirror(ctx, name, mirrorURL, "")
}

// CreateRepoWithMirror inserts a repo. A non-empty lastMirrored records that
// the mirror already holds that commit (a repo imported from its own mirror).
func (s *Store) CreateRepoWithMirror(ctx context.Context, name, mirrorURL, lastMirrored string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO repos(name,mirror_url,created_at) VALUES(?,?,?)`, name, mirrorURL, time.Now().Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: repo %q", ErrExists, name)
	}
	if err != nil {
		return err
	}
	if lastMirrored != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mirror_state(repo,last_mirrored) VALUES(?,?)`, name, lastMirrored); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MirrorState returns repo's mirror bookkeeping; a repo never mirrored has
// the zero state.
func (s *Store) MirrorState(ctx context.Context, repo string) (MirrorState, error) {
	m := MirrorState{Repo: repo}
	var attempt, pending int64
	err := s.db.QueryRowContext(ctx, `SELECT last_mirrored,last_error,last_attempt,pending FROM mirror_state WHERE repo=?`, repo).
		Scan(&m.LastMirrored, &m.LastError, &attempt, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if attempt != 0 {
		m.LastAttempt = time.Unix(attempt, 0).UTC()
	}
	m.Pending = pending != 0
	return m, nil
}

func (s *Store) SetMirrorPending(ctx context.Context, repo string, pending bool) error {
	p := 0
	if pending {
		p = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO mirror_state(repo,pending) VALUES(?,?)
ON CONFLICT(repo) DO UPDATE SET pending=excluded.pending`, repo, p)
	return err
}

// RecordMirrorSuccess notes that the mirror's main is now sha.
func (s *Store) RecordMirrorSuccess(ctx context.Context, repo, sha string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mirror_state(repo,last_mirrored,last_error,last_attempt,pending) VALUES(?,?,'',?,0)
ON CONFLICT(repo) DO UPDATE SET last_mirrored=excluded.last_mirrored,last_error='',last_attempt=excluded.last_attempt,pending=0`,
		repo, sha, at.Unix())
	return err
}

// RecordMirrorFailure notes a failed push; last_mirrored is kept.
func (s *Store) RecordMirrorFailure(ctx context.Context, repo, msg string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mirror_state(repo,last_error,last_attempt,pending) VALUES(?,?,?,1)
ON CONFLICT(repo) DO UPDATE SET last_error=excluded.last_error,last_attempt=excluded.last_attempt,pending=1`,
		repo, msg, at.Unix())
	return err
}
```

`database/sql` and `errors` are already imported by `store.go`. Check the import block and add anything missing.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/store/ -count=1`
Expected: PASS, including every existing store test.

- [ ] **Step 5: Lint, then report**

Run: `gofmt -l . && go vet ./internal/store/ && staticcheck ./internal/store/`
Expected: no output. Then report.

---

### Task 3: The mirror worker (`internal/mirror`)

**Files:**
- Create: `internal/mirror/mirror.go`
- Test: `internal/mirror/mirror_test.go`

**Interfaces:**
- Consumes:
  - From Task 1: `gitx.Open`, `(*gitx.Repo).ReadRef`, `(*gitx.Repo).Push`, `gitx.RemoteRef`, `gitx.ErrRejected`, `gitx.ErrSummary`, `gitx.InitBare`, `gitx.Init` and `gitx.MainRef`.
  - From Task 2: `store.GetRepo`, `store.MirrorState`, `store.SetMirrorPending`, `store.RecordMirrorSuccess`, `store.RecordMirrorFailure` and `store.ErrNotFound`.
- Produces (Task 4 uses these):
  ```go
  type Config struct {
  	RepoDir    func(repo string) string
  	AllowLocal bool
  	BackoffMin time.Duration // default 1s
  	BackoffMax time.Duration // default 5m
  	Timeout    time.Duration // per push, default 10m
  	Now        func() time.Time
  }
  func New(cfg Config, st *store.Store, log *slog.Logger) *Manager
  func (m *Manager) Start(ctx context.Context) error // queue every repo with a mirror_url
  func (m *Manager) Notify(repo string)             // never blocks; no-op after Close
  func (m *Manager) Close()                          // idempotent; waits for workers
  ```

**Concurrency design** (the reviewer checks against this):
- **One goroutine per repo.** Each is created lazily by the first `Notify` and owns a 1-slot kick channel. Further `Notify` calls coalesce in that slot.
- **The worker loop.** On a kick, call `sync` until it reports "caught up". Each successful push loops again, because `main` may have moved during the push. On error, wait out the backoff, then retry. Kicks that arrive during a backoff do not cut it short.
- **Shutdown.** `Close` cancels the manager context, which kills a running `git push` and ends any timer wait, then waits on the WaitGroup. `Notify` after `Close` does nothing.

- [ ] **Step 1: Write the failing tests** in `internal/mirror/mirror_test.go`:

```go
package mirror_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/mirror"
	"github.com/hermes98761234/sgit/internal/store"
)

var sig = gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000000, 0)}

type fixture struct {
	t      *testing.T
	st     *store.Store
	root   string     // repos dir
	src    *gitx.Repo // the sgit repo "web"
	remote string     // bare repo standing in for GitHub
	url    string
}

func newFixture(t *testing.T, makeRemote bool) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st, root: filepath.Join(dir, "repos"), remote: filepath.Join(dir, "remote.git")}
	f.url = "file://" + f.remote
	src, err := gitx.Init(ctx, filepath.Join(f.root, "web.git"), sig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	f.src = src
	if err := st.CreateRepo(ctx, "web", f.url); err != nil {
		t.Fatal(err)
	}
	if makeRemote {
		f.makeRemote()
	}
	return f
}

func (f *fixture) makeRemote() {
	f.t.Helper()
	if err := gitx.InitBare(context.Background(), f.remote); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) start(backoffMax time.Duration) *mirror.Manager {
	f.t.Helper()
	m := mirror.New(mirror.Config{
		RepoDir:    func(name string) string { return filepath.Join(f.root, name+".git") },
		AllowLocal: true,
		BackoffMin: 5 * time.Millisecond,
		BackoffMax: backoffMax,
	}, f.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(m.Close)
	return m
}

func (f *fixture) main() string {
	f.t.Helper()
	sha, err := f.src.ReadRef(context.Background(), gitx.MainRef)
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *fixture) remoteMain() string {
	f.t.Helper()
	sha, err := gitx.RemoteRef(context.Background(), f.url, gitx.MainRef, true)
	if err != nil {
		return "error: " + err.Error()
	}
	return sha
}

func (f *fixture) state() store.MirrorState {
	f.t.Helper()
	s, err := f.st.MirrorState(context.Background(), "web")
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// commit advances r's main by one empty-tree commit and returns it.
func commit(t *testing.T, r *gitx.Repo, msg string) string {
	t.Helper()
	ctx := context.Background()
	parent, err := r.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitTree(ctx, gitx.EmptyTree, []string{parent}, msg+"\n", sig, sig)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, gitx.MainRef, c, parent); err != nil {
		t.Fatal(err)
	}
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartCatchesUp(t *testing.T) {
	f := newFixture(t, true)
	f.start(50 * time.Millisecond) // no Notify: Start alone must queue the repo
	main := f.main()
	waitFor(t, "initial mirror", func() bool {
		s := f.state()
		return f.remoteMain() == main && s.LastMirrored == main && !s.Pending && s.LastError == ""
	})
}

func TestNotifyPushesNewCommits(t *testing.T) {
	f := newFixture(t, true)
	m := f.start(50 * time.Millisecond)
	first := f.main()
	waitFor(t, "initial mirror", func() bool { return f.state().LastMirrored == first })
	for i, msg := range []string{"two", "three"} {
		c := commit(t, f.src, msg)
		m.Notify("web")
		waitFor(t, "commit "+msg, func() bool { return f.remoteMain() == c && f.state().LastMirrored == c })
		if s := f.state(); s.Pending || s.LastError != "" || s.LastAttempt.IsZero() {
			t.Fatalf("after push %d: state %+v", i, s)
		}
	}
}

func TestRetriesUntilMirrorExists(t *testing.T) {
	f := newFixture(t, false) // the mirror does not exist yet
	f.start(20 * time.Millisecond)
	waitFor(t, "a recorded failure", func() bool {
		s := f.state()
		return s.LastError != "" && s.Pending && !s.LastAttempt.IsZero()
	})
	if s := f.state(); !strings.HasPrefix(s.LastError, "push failed: ") {
		t.Fatalf("LastError = %q, want a \"push failed: \" message", s.LastError)
	}
	f.makeRemote()
	main := f.main()
	waitFor(t, "recovery", func() bool {
		s := f.state()
		return f.remoteMain() == main && s.LastMirrored == main && s.LastError == "" && !s.Pending
	})
}

func TestLeaseMismatchIsNotOverwritten(t *testing.T) {
	f := newFixture(t, true)
	m := f.start(20 * time.Millisecond)
	first := f.main()
	waitFor(t, "initial mirror", func() bool { return f.state().LastMirrored == first })

	remote := gitx.Open(f.remote)
	defer remote.Close()
	stray := commit(t, remote, "made on the mirror")
	commit(t, f.src, "made in sgit")
	m.Notify("web")

	waitFor(t, "lease error", func() bool { return strings.Contains(f.state().LastError, "refusing to overwrite") })
	s := f.state()
	if want := "mirror main is no longer " + first + " (changed outside sgit); refusing to overwrite it"; s.LastError != want {
		t.Fatalf("LastError = %q, want %q", s.LastError, want)
	}
	if !s.Pending || s.LastMirrored != first {
		t.Fatalf("state = %+v, want pending with last_mirrored %s", s, first)
	}
	time.Sleep(100 * time.Millisecond) // several retries
	if got := f.remoteMain(); got != stray {
		t.Fatalf("mirror main = %s, want the stray commit %s kept", got, stray)
	}
}

func TestNonEmptyMirrorIsNotOverwritten(t *testing.T) {
	f := newFixture(t, false)
	other := gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1600000000, 0)}
	r, err := gitx.Init(context.Background(), f.remote, other) // a different initial commit
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := r.ReadRef(context.Background(), gitx.MainRef)
	r.Close()
	f.start(20 * time.Millisecond)
	want := "mirror already has a main branch that sgit did not push; refusing to overwrite it (a mirror must start empty)"
	waitFor(t, "non-empty error", func() bool { return f.state().LastError == want })
	if got := f.remoteMain(); got != theirs {
		t.Fatalf("mirror main = %s, want %s untouched", got, theirs)
	}
}

func TestAlreadyPushedIsRecorded(t *testing.T) {
	f := newFixture(t, true)
	main := f.main()
	// Simulate a crash after a successful push but before it was recorded.
	if err := f.src.Push(context.Background(), f.url, main, "", true); err != nil {
		t.Fatal(err)
	}
	f.start(20 * time.Millisecond)
	waitFor(t, "recorded success", func() bool {
		s := f.state()
		return s.LastMirrored == main && s.LastError == "" && !s.Pending
	})
}

func TestUnknownAndUnmirroredReposAreIgnored(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	if err := f.st.CreateRepo(ctx, "plain", ""); err != nil {
		t.Fatal(err)
	}
	m := f.start(20 * time.Millisecond)
	m.Notify("plain")
	m.Notify("ghost") // not in the store at all
	main := f.main()
	waitFor(t, "web mirrored", func() bool { return f.state().LastMirrored == main })
	time.Sleep(50 * time.Millisecond)
	if s, _ := f.st.MirrorState(ctx, "plain"); !s.LastAttempt.IsZero() || s.Pending {
		t.Fatalf("repo without mirror_url was attempted: %+v", s)
	}
}

func TestCloseDuringBackoff(t *testing.T) {
	f := newFixture(t, false)
	m := f.start(time.Hour)
	waitFor(t, "a failure", func() bool { return f.state().LastError != "" })
	time.Sleep(50 * time.Millisecond) // let the backoff grow past a few retries
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while a worker was backing off")
	}
	m.Notify("web") // after Close: must neither panic nor start a worker
	m.Close()       // idempotent
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/mirror/ -count=1`
Expected: FAIL to compile (package `mirror` does not exist).

- [ ] **Step 3: Implement** `internal/mirror/mirror.go`:

```go
// Package mirror pushes each repo's main to its mirror_url in the background
// (spec §8). One worker per mirrored repo pushes with
// --force-with-lease=main:<last_mirrored>, so commits made on the mirror
// outside sgit are never overwritten. Failed pushes retry with exponential
// backoff; Start queues every mirrored repo so a restart catches up.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

type Config struct {
	RepoDir    func(repo string) string // bare repo directory for a repo name
	AllowLocal bool                     // permit file:// and path mirrors (tests only)
	BackoffMin time.Duration            // first retry delay; default 1s
	BackoffMax time.Duration            // retry delay cap; default 5m
	Timeout    time.Duration            // per push attempt; default 10m
	Now        func() time.Time         // default time.Now
}

type Manager struct {
	cfg    Config
	st     *store.Store
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	kicks  map[string]chan struct{} // one per repo with a running worker
	closed bool
}

func New(cfg Config, st *store.Store, log *slog.Logger) *Manager {
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 5 * time.Minute
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, st: st, log: log, ctx: ctx, cancel: cancel, kicks: map[string]chan struct{}{}}
}

// Start queues every repo that has a mirror_url, so pushes missed while the
// server was down (or whose bookkeeping was lost) are caught up.
func (m *Manager) Start(ctx context.Context) error {
	repos, err := m.st.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("mirror: list repos: %w", err)
	}
	for _, r := range repos {
		if r.MirrorURL != "" {
			m.Notify(r.Name)
		}
	}
	return nil
}

// Notify queues a mirror check for repo. It never blocks.
func (m *Manager) Notify(repo string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	kick, ok := m.kicks[repo]
	if !ok {
		kick = make(chan struct{}, 1)
		m.kicks[repo] = kick
		m.wg.Add(1)
		go m.run(repo, kick)
	}
	select {
	case kick <- struct{}{}:
	default: // a check is already queued
	}
}

// Close stops all workers, killing any push in flight, and waits for them.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) run(repo string, kick <-chan struct{}) {
	defer m.wg.Done()
	g := gitx.Open(m.cfg.RepoDir(repo))
	defer g.Close()
	var delay time.Duration
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-kick:
		}
		for {
			caughtUp, err := m.sync(repo, g)
			if err == nil {
				delay = 0
				if caughtUp {
					break
				}
				continue // pushed; main may have moved meanwhile
			}
			if m.ctx.Err() != nil {
				return
			}
			delay = m.backoff(delay)
			m.log.Warn("mirror push failed", "repo", repo, "retry_in", delay.String(), "err", err)
			t := time.NewTimer(delay)
			select {
			case <-m.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

func (m *Manager) backoff(d time.Duration) time.Duration {
	if d == 0 {
		return m.cfg.BackoffMin
	}
	if d *= 2; d > m.cfg.BackoffMax {
		d = m.cfg.BackoffMax
	}
	return d
}

// sync pushes main to repo's mirror if the mirror is behind. It reports
// whether the mirror is caught up (nothing was pushed).
func (m *Manager) sync(repo string, g *gitx.Repo) (bool, error) {
	ctx := m.ctx
	row, err := m.st.GetRepo(ctx, repo)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if row.MirrorURL == "" {
		return true, nil
	}
	main, err := g.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		return false, err
	}
	state, err := m.st.MirrorState(ctx, repo)
	if err != nil {
		return false, err
	}
	if state.LastMirrored == main {
		if state.Pending {
			return true, m.st.SetMirrorPending(ctx, repo, false)
		}
		return true, nil
	}
	if !state.Pending {
		if err := m.st.SetMirrorPending(ctx, repo, true); err != nil {
			return false, err
		}
	}
	pctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	err = g.Push(pctx, row.MirrorURL, main, state.LastMirrored, m.cfg.AllowLocal)
	if errors.Is(err, gitx.ErrRejected) {
		// The lease is stale. If the mirror already holds main (a push whose
		// bookkeeping was lost to a crash), that is a success, not a conflict.
		if remote, rerr := gitx.RemoteRef(pctx, row.MirrorURL, gitx.MainRef, m.cfg.AllowLocal); rerr == nil && remote == main {
			err = nil
		}
	}
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		msg := describe(err, state.LastMirrored)
		if rerr := m.st.RecordMirrorFailure(ctx, repo, msg, m.cfg.Now()); rerr != nil {
			m.log.Error("mirror: record failure", "repo", repo, "err", rerr)
		}
		return false, errors.New(msg)
	}
	if err := m.st.RecordMirrorSuccess(ctx, repo, main, m.cfg.Now()); err != nil {
		return false, err
	}
	m.log.Info("mirrored", "repo", repo, "commit", main)
	return false, nil
}

// describe turns a push error into the message shown by sg repo status.
func describe(err error, lease string) string {
	switch {
	case errors.Is(err, gitx.ErrRejected) && lease == "":
		return "mirror already has a main branch that sgit did not push; refusing to overwrite it (a mirror must start empty)"
	case errors.Is(err, gitx.ErrRejected):
		return fmt.Sprintf("mirror main is no longer %s (changed outside sgit); refusing to overwrite it", lease)
	default:
		return "push failed: " + gitx.ErrSummary(err)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/mirror/ -count=3`
Expected: PASS on all three runs. Using `-count=3` shakes out timing flakes. A flaky test is a bug in the code or in the test; never fix it by lengthening sleeps beyond the 10 s `waitFor` deadline.

- [ ] **Step 5: Lint, then report**

Run: `gofmt -l . && go vet ./internal/mirror/ && staticcheck ./internal/mirror/`
Expected: no output. Then report.

---

### Task 4: Server: import, mirror wiring, repo status RPCs

**Files:**
- Modify: `proto/sgit/v1/sgit.proto`, then regenerate `gen/sgit/v1/*.pb.go` with `make gen`
- Modify: `internal/server/server.go`, `internal/server/repos.go`, `internal/server/admin.go`, `internal/server/land.go`
- Test: `internal/server/mirror_test.go`

**Interfaces:**
- Consumes:
  - From Task 1: `gitx.ValidateRemote`, `gitx.Import`, `gitx.InitBare`, `gitx.ErrSummary`, `gitx.GitError`.
  - From Task 2: `store.CreateRepoWithMirror`, `store.MirrorState`.
  - From Task 3: `mirror.New`, `Start`, `Notify`, `Close` and `mirror.Config`.
  - Test helpers: `testenv.Start`, `Env.Admin`, `Env.Repo`, `Env.Upload`, `Env.Git`, `Env.NewUser`, `testenv.Add` and `testenv.Must`.
- Produces (Task 5 uses these):
  - Admin RPCs:
    - `ImportRepo(ImportRepoRequest{Name, FromUrl, MirrorUrl}) → CreateRepoResponse{Main}`
    - `RepoStatus(RepoStatusRequest{Name}) → RepoInfo`
    - `ListRepos(ListReposRequest{}) → ListReposResponse{Repos []*RepoInfo}`
    - `CreateRepoRequest` gains `MirrorUrl`.
  - `RepoInfo` Go fields: `Name, Main string; CreatedUnix int64; MirrorUrl, MirrorLast string; MirrorPending bool; MirrorError string; MirrorAttemptUnix int64`.
  - `server.Config` gains `AllowLocalRemotes bool` and `MirrorBackoffMin`, `MirrorBackoffMax`, `MirrorTimeout time.Duration`.

- [ ] **Step 1: Update the proto and regenerate.** In `proto/sgit/v1/sgit.proto`, change the `Admin` service to:

```proto
// Admin is only served by sg-server; clients dial it directly.
service Admin {
  rpc WhoAmI(WhoAmIRequest) returns (WhoAmIResponse);
  rpc CreateRepo(CreateRepoRequest) returns (CreateRepoResponse);
  rpc ImportRepo(ImportRepoRequest) returns (CreateRepoResponse);
  rpc RepoStatus(RepoStatusRequest) returns (RepoInfo);
  rpc ListRepos(ListReposRequest) returns (ListReposResponse);
  rpc CreateToken(CreateTokenRequest) returns (CreateTokenResponse);
  rpc RevokeToken(RevokeTokenRequest) returns (RevokeTokenResponse);
  rpc GrantACL(GrantACLRequest) returns (GrantACLResponse);
  rpc RevokeACL(RevokeACLRequest) returns (RevokeACLResponse);
}
```

Replace the `CreateRepoRequest` line with the following, and add the new messages right after `CreateRepoResponse`:

```proto
message CreateRepoRequest { string name = 1; string mirror_url = 2; }
message CreateRepoResponse { string main = 1; }
// ImportRepo copies main and release/* (no tags) from from_url.
message ImportRepoRequest { string name = 1; string from_url = 2; string mirror_url = 3; }
message RepoStatusRequest { string name = 1; }
message ListReposRequest {}
message ListReposResponse { repeated RepoInfo repos = 1; }
// RepoInfo describes a repo and its mirror; mirror_* fields other than
// mirror_url are zero when mirror_url is empty.
message RepoInfo {
  string name = 1;
  string main = 2;
  int64 created_unix = 3;
  string mirror_url = 4;
  string mirror_last = 5;         // last commit pushed to the mirror; "" = never
  bool mirror_pending = 6;        // main has commits the mirror lacks
  string mirror_error = 7;        // last push error; "" after a success
  int64 mirror_attempt_unix = 8;  // last push attempt; 0 = never
}
```

Run: `make gen && go build ./...`
Expected: the build fails only in `internal/server`, because `adminService` does not yet implement the new methods (or because `UnimplementedAdminServer` hides them, in which case the build passes). Any other failure means the proto edit is wrong.

- [ ] **Step 2: Write the failing tests** in `internal/server/mirror_test.go`:

```go
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
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run 'Mirror|Import|ListRepos|RepoAdmin|ConcurrentCreate' -count=1`
Expected: FAIL. Either the build fails (`unknown field AllowLocalRemotes`), or the RPCs return `Unimplemented`.

- [ ] **Step 4: Implement the server changes.**

(a) **`internal/server/server.go`.** Add these fields to `Config`, after `LandRetries`:

```go
	// AllowLocalRemotes permits file:// and path remotes for import and
	// mirroring. Tests only; sg-server has no flag for it.
	AllowLocalRemotes bool
	MirrorBackoffMin  time.Duration // first mirror retry delay; default 1s
	MirrorBackoffMax  time.Duration // mirror retry delay cap; default 5m
	MirrorTimeout     time.Duration // per mirror push; default 10m
```

The mirror package applies those defaults, so `defaults()` does not change. Add `mirror *mirror.Manager` to `Server`, import `"github.com/hermes98761234/sgit/internal/mirror"`, and replace the body of `New` after `enc` is created:

```go
	s := &Server{
		cfg:   cfg,
		store: st,
		log:   log,
		repos: newRepoManager(filepath.Join(cfg.DataDir, "repos"), st, cfg.AllowLocalRemotes),
		zenc:  enc,
	}
	s.mirror = mirror.New(mirror.Config{
		RepoDir:    s.repos.path,
		AllowLocal: cfg.AllowLocalRemotes,
		BackoffMin: cfg.MirrorBackoffMin,
		BackoffMax: cfg.MirrorBackoffMax,
		Timeout:    cfg.MirrorTimeout,
		Now:        cfg.Now,
	}, st, log)
	if err := s.mirror.Start(context.Background()); err != nil {
		s.mirror.Close()
		_ = enc.Close()
		return nil, err
	}
	return s, nil
```

Then make `Close` stop the mirror workers first:

```go
func (s *Server) Close() {
	s.mirror.Close()
	s.repos.closeAll()
	_ = s.zenc.Close()
}
```

(b) **`internal/server/repos.go`.** Add the reservation, and give create a mirror URL and an import sibling. Replace the `repoManager` struct, `newRepoManager` and `create` with the code below, and add `reserve` and `importRepo`:

```go
type repoManager struct {
	root       string
	st         *store.Store
	allowLocal bool // permit file:// and path remotes (tests)
	mu         sync.Mutex
	open       map[string]*repoHandle
	busy       map[string]bool // names with a create or import in flight
}

func newRepoManager(root string, st *store.Store, allowLocal bool) *repoManager {
	return &repoManager{root: root, st: st, allowLocal: allowLocal, open: map[string]*repoHandle{}, busy: map[string]bool{}}
}

// reserve claims name for one create or import at a time. Call the returned
// release once the repo row exists or the attempt has been cleaned up.
func (m *repoManager) reserve(ctx context.Context, name string) (func(), error) {
	if !ValidRepoName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repo name %q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[name] {
		return nil, status.Errorf(codes.AlreadyExists, "repo %q is being created", name)
	}
	if _, err := m.st.GetRepo(ctx, name); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "repo %q exists", name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, err := os.Stat(m.path(name)); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "repo dir for %q exists", name)
	}
	m.busy[name] = true
	return func() {
		m.mu.Lock()
		delete(m.busy, name)
		m.mu.Unlock()
	}, nil
}

func (m *repoManager) create(ctx context.Context, name, mirrorURL string, sig gitx.Signature) (string, error) {
	if mirrorURL != "" {
		if err := gitx.ValidateRemote(mirrorURL, m.allowLocal); err != nil {
			return "", err
		}
	}
	release, err := m.reserve(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return "", err
	}
	p := m.path(name)
	r, err := gitx.Init(ctx, p, sig)
	if err != nil {
		os.RemoveAll(p)
		return "", err
	}
	defer r.Close()
	main, err := r.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		os.RemoveAll(p)
		return "", err
	}
	if err := m.st.CreateRepo(ctx, name, mirrorURL); err != nil {
		os.RemoveAll(p)
		return "", err
	}
	return main, nil
}

// importRepo creates name from the main and release/* branches of from. When
// the mirror is the source itself, the mirror is recorded as holding main.
func (m *repoManager) importRepo(ctx context.Context, name, from, mirrorURL string) (string, error) {
	if from == "" {
		return "", status.Error(codes.InvalidArgument, "import needs a source URL")
	}
	if err := gitx.ValidateRemote(from, m.allowLocal); err != nil {
		return "", err
	}
	if mirrorURL != "" {
		if err := gitx.ValidateRemote(mirrorURL, m.allowLocal); err != nil {
			return "", err
		}
	}
	release, err := m.reserve(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return "", err
	}
	p := m.path(name)
	main, err := gitx.Import(ctx, p, from, m.allowLocal)
	if err != nil {
		os.RemoveAll(p)
		var ge *gitx.GitError
		if errors.As(err, &ge) {
			return "", status.Errorf(codes.FailedPrecondition, "import from %s failed: %s", from, gitx.ErrSummary(err))
		}
		return "", err
	}
	seeded := ""
	if mirrorURL == from {
		seeded = main
	}
	// The fetch may have taken minutes; record the result even if the caller gave up.
	if err := m.st.CreateRepoWithMirror(context.WithoutCancel(ctx), name, mirrorURL, seeded); err != nil {
		os.RemoveAll(p)
		return "", err
	}
	return main, nil
}
```

(c) **`internal/server/admin.go`.** Import `gitx`, change `CreateRepo`, and add the three new RPCs plus their helper:

```go
func (a *adminService) CreateRepo(ctx context.Context, req *sgitv1.CreateRepoRequest) (*sgitv1.CreateRepoResponse, error) {
	u, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	main, err := a.s.repos.create(ctx, req.Name, req.MirrorUrl, a.s.sig(u))
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return &sgitv1.CreateRepoResponse{Main: main}, nil
}

func (a *adminService) ImportRepo(ctx context.Context, req *sgitv1.ImportRepoRequest) (*sgitv1.CreateRepoResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	main, err := a.s.repos.importRepo(ctx, req.Name, req.FromUrl, req.MirrorUrl)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return &sgitv1.CreateRepoResponse{Main: main}, nil
}

func (a *adminService) RepoStatus(ctx context.Context, req *sgitv1.RepoStatusRequest) (*sgitv1.RepoInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	row, err := a.s.store.GetRepo(ctx, req.Name)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	info, err := a.repoInfo(ctx, row)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return info, nil
}

func (a *adminService) ListRepos(ctx context.Context, _ *sgitv1.ListReposRequest) (*sgitv1.ListReposResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := a.s.store.ListRepos(ctx)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	out := &sgitv1.ListReposResponse{}
	for _, row := range rows {
		info, err := a.repoInfo(ctx, row)
		if err != nil {
			return nil, a.s.toStatus(ctx, err)
		}
		out.Repos = append(out.Repos, info)
	}
	return out, nil
}

// repoInfo reports a repo's main and mirror health. Pending is computed from
// main and last_mirrored, so it is right even if the worker's view lags.
func (a *adminService) repoInfo(ctx context.Context, row store.RepoRow) (*sgitv1.RepoInfo, error) {
	h, err := a.s.repos.get(ctx, row.Name)
	if err != nil {
		return nil, err
	}
	main, err := h.git.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		return nil, err
	}
	info := &sgitv1.RepoInfo{Name: row.Name, Main: main, CreatedUnix: row.CreatedAt.Unix(), MirrorUrl: row.MirrorURL}
	if row.MirrorURL == "" {
		return info, nil
	}
	ms, err := a.s.store.MirrorState(ctx, row.Name)
	if err != nil {
		return nil, err
	}
	info.MirrorLast = ms.LastMirrored
	info.MirrorPending = ms.LastMirrored != main
	info.MirrorError = ms.LastError
	if !ms.LastAttempt.IsZero() {
		info.MirrorAttemptUnix = ms.LastAttempt.Unix()
	}
	return info, nil
}
```

(d) **`internal/server/land.go`.** Every code path that moves `refs/heads/main` must call `Notify`. Run `grep -n 'UpdateRef(.*MainRef' internal/server/*.go`; today the only hit is the CAS loop at land.go:~109. In that loop, add the call immediately before `return newC, nil`, after the `UpdateDraft` bookkeeping:

```go
		r.s.mirror.Notify(h.name)
		return newC, nil
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/server/ -count=1 && go test -race ./... -count=1`
Expected: PASS. The first command runs the new tests together with every existing server test, and the second runs the whole module.

- [ ] **Step 6: Lint, then report**

Run: `gofmt -l . && go vet ./... && staticcheck ./...`
Expected: no output. Then report.

---

### Task 5: `sg repo`, `sg token`, `sg acl`

**Files:**
- Create: `internal/cli/admin.go`
- Modify: `internal/cli/cli.go` (`commandTable`), `internal/cli/session.go` (`target.adminOnly`)
- Test: `internal/cli/admin_test.go`

**Interfaces:**
- Consumes:
  - From Task 4: the Admin RPCs `CreateRepo` (with `MirrorUrl`), `ImportRepo`, `RepoStatus` and `ListRepos`, plus the existing `CreateToken`, `RevokeToken`, `GrantACL` and `RevokeACL`, and the `RepoInfo` fields.
  - Existing CLI pieces: `a.newFlags`, `parse`, `usageErr`, `a.emit`, `short(sha)`, `a.connect(target)`, `session.admin`.
  - Test harness: `newHarness(t, opts...)`, `h.ok`, `h.okAs`, `h.run`, `h.runAs`, `h.loginAs`, `h.seed`.
- Produces: the commands below. Each one also accepts `--json` and `--server S`.
  - `sg repo create <name> [--mirror URL]`
  - `sg repo import <name> --from URL [--mirror URL]`
  - `sg repo status <name>`
  - `sg repo list`
  - `sg token create <user> [--admin] [--email E]`
  - `sg token revoke <id>`
  - `sg acl grant <user> <repo> read|write`
  - `sg acl revoke <user> <repo>`

**Output contract:**

| Command | Text output | `--json` output |
|---|---|---|
| `repo create` | `created web (main 1a2b3c4)` | `{"repo","main"}` |
| `repo import` | `imported app from URL (main 1a2b3c4)` | `{"repo","main"}` |
| `repo status` | the block shown in `printRepoStatus` | one `repoJSON` |
| `repo list` | a tab-aligned `NAME MAIN MIRROR` table; MIRROR is `-`, `ok`, `pending` or `error` | `{"repos":[repoJSON...]}` |
| `token create` | the token alone on stdout, plus `token <id> for <user> (shown only once):` on stderr | `{"id","user","token"}` |

- [ ] **Step 1: Write the failing tests** in `internal/cli/admin_test.go`:

```go
package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/server"
)

type repoStatusJSON struct {
	Name          string `json:"name"`
	Main          string `json:"main"`
	Created       string `json:"created"`
	MirrorURL     string `json:"mirror_url"`
	MirrorLast    string `json:"mirror_last"`
	MirrorPending bool   `json:"mirror_pending"`
	MirrorError   string `json:"mirror_error"`
	MirrorAttempt string `json:"mirror_attempt"`
}

func decode(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}
}

func TestRepoCreateStatusList(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	if out := h.ok(dir, "repo", "create", "web"); !strings.HasPrefix(out, "created web (main ") {
		t.Fatalf("repo create printed %q", out)
	}
	var st repoStatusJSON
	decode(t, h.ok(dir, "repo", "status", "web", "--json"), &st)
	if st.Name != "web" || len(st.Main) != 40 || st.Created == "" || st.MirrorURL != "" || st.MirrorPending {
		t.Fatalf("repo status = %+v", st)
	}
	if out := h.ok(dir, "repo", "status", "web"); !strings.Contains(out, "mirror:   none") {
		t.Fatalf("repo status text = %q", out)
	}
	out := h.ok(dir, "repo", "list")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.HasPrefix(lines[1], "web") || !strings.HasSuffix(lines[1], "-") {
		t.Fatalf("repo list = %q", out)
	}
	var list struct{ Repos []repoStatusJSON }
	decode(t, h.ok(dir, "repo", "list", "--json"), &list)
	if len(list.Repos) != 1 || list.Repos[0].Name != "web" {
		t.Fatalf("repo list --json = %+v", list)
	}
	if code, _, _ := h.run(dir, "repo", "create", "web"); code != cli.ExitError {
		t.Fatalf("duplicate create exit = %d, want %d", code, cli.ExitError)
	}
}

func TestRepoImportWithMirror(t *testing.T) {
	h := newHarness(t, func(c *server.Config) {
		c.AllowLocalRemotes = true
		c.MirrorBackoffMin = 5 * time.Millisecond
		c.MirrorBackoffMax = 50 * time.Millisecond
	})
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.git")
	r, err := gitx.Init(ctx, src, gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	main, _ := r.ReadRef(ctx, gitx.MainRef)
	r.Close()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	if err := gitx.InitBare(ctx, mirror); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := h.ok(dir, "repo", "import", "app", "--from", "file://"+src, "--mirror", "file://"+mirror)
	if !strings.HasPrefix(out, "imported app from file://"+src+" (main ") {
		t.Fatalf("repo import printed %q", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var st repoStatusJSON
		decode(t, h.ok(dir, "repo", "status", "app", "--json"), &st)
		if st.MirrorLast == main && !st.MirrorPending && st.MirrorError == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror never caught up: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := h.ok(dir, "repo", "status", "app")
	for _, want := range []string{"mirror:   file://" + mirror, "  last:    " + main, "  pending: no"} {
		if !strings.Contains(text, want) {
			t.Errorf("repo status text lacks %q:\n%s", want, text)
		}
	}
	if out := h.ok(dir, "repo", "list"); !strings.Contains(out, "ok") {
		t.Errorf("repo list = %q, want mirror column ok", out)
	}
}

func TestTokenAndACLCommands(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "hi\n"})
	dir := t.TempDir()

	var tok struct{ ID, User, Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	if tok.User != "bob" || tok.ID == "" || !strings.HasPrefix(tok.Token, "sgt_") {
		t.Fatalf("token create = %+v", tok)
	}
	h.loginAs("bob", tok.Token)
	bobLog := func() int {
		code, _, _ := h.runAs("bob", dir, "log", "--repo", "web")
		return code
	}
	if code := bobLog(); code != cli.ExitAuth {
		t.Fatalf("log without ACL exit = %d, want %d", code, cli.ExitAuth)
	}
	h.ok(dir, "acl", "grant", "bob", "web", "read")
	if code := bobLog(); code != cli.ExitOK {
		t.Fatalf("log with read exit = %d", code)
	}
	h.ok(dir, "acl", "revoke", "bob", "web")
	if code := bobLog(); code != cli.ExitAuth {
		t.Fatalf("log after acl revoke exit = %d, want %d", code, cli.ExitAuth)
	}
	h.ok(dir, "acl", "grant", "bob", "web", "write")
	if out := h.ok(dir, "token", "revoke", tok.ID); !strings.Contains(out, tok.ID) {
		t.Fatalf("token revoke printed %q", out)
	}
	if code := bobLog(); code != cli.ExitAuth {
		t.Fatalf("log after token revoke exit = %d, want %d", code, cli.ExitAuth)
	}

	code, out, errs := h.run(dir, "token", "create", "carol")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "sgt_") {
		t.Fatalf("token create text: exit %d stdout %q", code, out)
	}
	if !strings.Contains(errs, "for carol (shown only once)") {
		t.Fatalf("token create stderr = %q", errs)
	}
}

func TestAdminCommandsNeedAdmin(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	var tok struct{ Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	h.loginAs("bob", tok.Token)
	for _, args := range [][]string{
		{"repo", "create", "x"},
		{"repo", "list"},
		{"token", "create", "eve"},
		{"acl", "grant", "bob", "x", "read"},
	} {
		if code, _, _ := h.runAs("bob", dir, args...); code != cli.ExitAuth {
			t.Errorf("sg %v as bob: exit %d, want %d", args, code, cli.ExitAuth)
		}
	}
}

func TestAdminUsageErrors(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"repo"},
		{"repo", "frob"},
		{"repo", "create"},
		{"repo", "import", "x"},
		{"repo", "status"},
		{"token"},
		{"token", "create"},
		{"token", "revoke"},
		{"acl", "grant", "bob", "web"},
		{"acl", "revoke", "bob"},
	} {
		code, _, errs := h.run(dir, args...)
		if code != cli.ExitError || !strings.Contains(errs, "usage:") {
			t.Errorf("sg %v: exit %d stderr %q, want usage error", args, code, errs)
		}
	}
	if code, _, _ := h.run(dir, "acl", "grant", "bob", "web", "admin"); code != cli.ExitError {
		t.Errorf("acl grant with a bad level: exit %d, want %d", code, cli.ExitError)
	}
}

func TestAdminCommandsSkipDaemon(t *testing.T) {
	h := newHarness(t)
	sock := filepath.Join(t.TempDir(), "sgd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil { // exists, but is not a socket
		t.Fatal(err)
	}
	t.Setenv("SG_SOCKET", sock)
	code, _, errs := h.run(t.TempDir(), "repo", "list")
	if code != 0 || strings.Contains(errs, "sgd") {
		t.Fatalf("repo list: exit %d stderr %q; admin commands must not touch sgd", code, errs)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'Repo|Token|Admin' -count=1`
Expected: FAIL, with `sg: unknown command "repo"` and the matching exits.

- [ ] **Step 3: Implement.**

(a) **`internal/cli/session.go`.** Add a field to `target`:

```go
	adminOnly bool // Admin RPCs only: never dial sgd
```

In `connect`, change the daemon condition to `if os.Getenv("SG_NO_DAEMON") == "" && !t.adminOnly {`.

(b) **`internal/cli/cli.go`.** Append these to `commandTable()`, after `sparse`:

```go
		{"repo", repoUsage, cmdRepo},
		{"token", tokenUsage, cmdToken},
		{"acl", aclUsage, cmdACL},
```

(c) **Create `internal/cli/admin.go`:**

```go
package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

const (
	repoUsage  = "repo create <name> [--mirror URL] | repo import <name> --from URL [--mirror URL] | repo status <name> | repo list  [--server S]"
	tokenUsage = "token create <user> [--admin] [--email E] | token revoke <id>  [--server S]"
	aclUsage   = "acl grant <user> <repo> read|write | acl revoke <user> <repo>  [--server S]"
)

const (
	adminTimeout  = 30 * time.Second
	importTimeout = 30 * time.Minute
)

// admin dials the server directly (never sgd) and runs fn with a deadline.
func (a *app) admin(ctx context.Context, server string, timeout time.Duration, fn func(context.Context, sgitv1.AdminClient) error) error {
	s, err := a.connect(target{server: server, adminOnly: true})
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(ctx, s.admin)
}

func cmdRepo(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return repoCreate(ctx, a, args[1:])
		case "import":
			return repoImport(ctx, a, args[1:])
		case "status":
			return repoStatus(ctx, a, args[1:])
		case "list":
			return repoList(ctx, a, args[1:])
		}
	}
	return usageErr(repoUsage)
}

func repoCreate(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo create")
	mirror := fs.String("mirror", "", "push main to this git URL after every land")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(repoUsage)
	}
	var resp *sgitv1.CreateRepoResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: pos[0], MirrorUrl: *mirror})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"repo": pos[0], "main": resp.Main}, func(w io.Writer) {
		fmt.Fprintf(w, "created %s (main %s)\n", pos[0], short(resp.Main))
	})
}

func repoImport(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo import")
	from := fs.String("from", "", "git URL to import main and release/* from (required)")
	mirror := fs.String("mirror", "", "push main to this git URL after every land")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *from == "" {
		return usageErr(repoUsage)
	}
	var resp *sgitv1.CreateRepoResponse
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: pos[0], FromUrl: *from, MirrorUrl: *mirror})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"repo": pos[0], "main": resp.Main}, func(w io.Writer) {
		fmt.Fprintf(w, "imported %s from %s (main %s)\n", pos[0], *from, short(resp.Main))
	})
}

func repoStatus(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo status")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(repoUsage)
	}
	var info *sgitv1.RepoInfo
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		info, err = ac.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: pos[0]})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(toRepoJSON(info), func(w io.Writer) { printRepoStatus(w, info) })
}

func repoList(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo list")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr(repoUsage)
	}
	var resp *sgitv1.ListReposResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ListRepos(ctx, &sgitv1.ListReposRequest{})
		return err
	})
	if err != nil {
		return err
	}
	repos := make([]repoJSON, 0, len(resp.Repos))
	for _, r := range resp.Repos {
		repos = append(repos, toRepoJSON(r))
	}
	return a.emit(map[string]any{"repos": repos}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMAIN\tMIRROR")
		for _, r := range resp.Repos {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, short(r.Main), mirrorSummary(r))
		}
		tw.Flush()
	})
}

type repoJSON struct {
	Name          string `json:"name"`
	Main          string `json:"main"`
	Created       string `json:"created"`
	MirrorURL     string `json:"mirror_url,omitempty"`
	MirrorLast    string `json:"mirror_last,omitempty"`
	MirrorPending bool   `json:"mirror_pending"`
	MirrorError   string `json:"mirror_error,omitempty"`
	MirrorAttempt string `json:"mirror_attempt,omitempty"`
}

func toRepoJSON(r *sgitv1.RepoInfo) repoJSON {
	return repoJSON{
		Name: r.Name, Main: r.Main, Created: unixTime(r.CreatedUnix),
		MirrorURL: r.MirrorUrl, MirrorLast: r.MirrorLast, MirrorPending: r.MirrorPending,
		MirrorError: r.MirrorError, MirrorAttempt: unixTime(r.MirrorAttemptUnix),
	}
}

func unixTime(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func mirrorSummary(r *sgitv1.RepoInfo) string {
	switch {
	case r.MirrorUrl == "":
		return "-"
	case r.MirrorError != "":
		return "error"
	case r.MirrorPending:
		return "pending"
	default:
		return "ok"
	}
}

func printRepoStatus(w io.Writer, r *sgitv1.RepoInfo) {
	fmt.Fprintf(w, "repo:     %s\n", r.Name)
	fmt.Fprintf(w, "main:     %s\n", r.Main)
	fmt.Fprintf(w, "created:  %s\n", unixTime(r.CreatedUnix))
	if r.MirrorUrl == "" {
		fmt.Fprintln(w, "mirror:   none")
		return
	}
	fmt.Fprintf(w, "mirror:   %s\n", r.MirrorUrl)
	last := r.MirrorLast
	if last == "" {
		last = "never"
	}
	fmt.Fprintf(w, "  last:    %s\n", last)
	pending := "no"
	if r.MirrorPending {
		pending = "yes"
	}
	fmt.Fprintf(w, "  pending: %s\n", pending)
	if r.MirrorAttemptUnix != 0 {
		fmt.Fprintf(w, "  attempt: %s\n", unixTime(r.MirrorAttemptUnix))
	}
	if r.MirrorError != "" {
		fmt.Fprintf(w, "  error:   %s\n", r.MirrorError)
	}
}

func cmdToken(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return tokenCreate(ctx, a, args[1:])
		case "revoke":
			return tokenRevoke(ctx, a, args[1:])
		}
	}
	return usageErr(tokenUsage)
}

func tokenCreate(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("token create")
	admin := fs.Bool("admin", false, "make a new user an admin (ignored if the user exists)")
	email := fs.String("email", "", "email for a new user (default <user>@sgit.local; ignored if the user exists)")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(tokenUsage)
	}
	var resp *sgitv1.CreateTokenResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: pos[0], Email: *email, Admin: *admin})
		return err
	})
	if err != nil {
		return err
	}
	if !a.json {
		fmt.Fprintf(a.io.Stderr, "token %s for %s (shown only once):\n", resp.Id, resp.User)
	}
	return a.emit(map[string]any{"id": resp.Id, "user": resp.User, "token": resp.Token}, func(w io.Writer) {
		fmt.Fprintln(w, resp.Token)
	})
}

func tokenRevoke(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("token revoke")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(tokenUsage)
	}
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) error {
		_, err := ac.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: pos[0]})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"revoked": pos[0]}, func(w io.Writer) {
		fmt.Fprintf(w, "revoked token %s\n", pos[0])
	})
}

func cmdACL(ctx context.Context, a *app, args []string) error {
	if len(args) == 0 || (args[0] != "grant" && args[0] != "revoke") {
		return usageErr(aclUsage)
	}
	verb := args[0]
	fs := a.newFlags("acl " + verb)
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	want := 2
	if verb == "grant" {
		want = 3
	}
	if len(pos) != want {
		return usageErr(aclUsage)
	}
	user, repo := pos[0], pos[1]
	level := ""
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) error {
		if verb == "grant" {
			level = pos[2]
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: user, Repo: repo, Level: level})
			return err
		}
		_, err := ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: user, Repo: repo})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"user": user, "repo": repo, "level": level}, func(w io.Writer) {
		if verb == "grant" {
			fmt.Fprintf(w, "granted %s %s access to %s\n", user, level, repo)
		} else {
			fmt.Fprintf(w, "revoked %s's access to %s\n", user, repo)
		}
	})
}
```

The functions use named results, as in `func(ctx, ac) (err error) { resp, err = ...; return err }`. Assigning `resp` inside the closure with `=` sets the outer variable; do not change it to `:=`, which would shadow it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/cli/ -count=1`
Expected: PASS, including every existing CLI test. Then run `go run ./cmd/sg help`; the three new commands must be listed.

- [ ] **Step 5: Lint, then report**

Run: `gofmt -l . && go vet ./... && staticcheck ./...`
Expected: no output. Then report.

---

### Task 6: Image, release workflow, mirror-credential docs

**Files:**
- Modify: `Dockerfile`, `scripts/docker-smoke.sh`, `docker-compose.yml`, `README.md`, `docs/superpowers/specs/2026-10-14-sgit-design.md`
- Create: `contrib/sg-git-askpass`, `.github/workflows/release.yml`

**Interfaces:**
- Consumes: the `sg` commands from Task 5 (`login`, `repo create`, `repo list`) and the image layout from phase 1. That layout is:
  - user `sg`, uid 10001, with `HOME=/home/sg`;
  - `/data/admin-token`;
  - `/data/tls/cert.pem` when `SG_TLS_SELF_SIGNED=1`.
- Produces:
  - An image with `/usr/local/bin/sg` and `/usr/local/bin/sg-git-askpass`.
  - A tag-triggered multi-arch publish to `ghcr.io/hermes98761234/sgit-server`.

- [ ] **Step 1: The askpass helper.** Create `contrib/sg-git-askpass` with mode 0755:

```sh
#!/bin/sh
# GIT_ASKPASS helper for sg-server mirrors and imports over https.
# Username: $SG_MIRROR_USER (default x-access-token, which GitHub accepts
# with a personal access token). Password: the contents of
# $SG_MIRROR_TOKEN_FILE if set, else $SG_MIRROR_TOKEN.
case "$1" in
Username*)
  printf '%s\n' "${SG_MIRROR_USER:-x-access-token}"
  ;;
*)
  if [ -n "${SG_MIRROR_TOKEN_FILE:-}" ]; then
    tr -d '\r\n' <"$SG_MIRROR_TOKEN_FILE"
    echo
  else
    printf '%s\n' "${SG_MIRROR_TOKEN:-}"
  fi
  ;;
esac
```

Check it:
```bash
[ "$(sh contrib/sg-git-askpass "Username for 'https://github.com': ")" = x-access-token ] && echo user-ok
[ "$(SG_MIRROR_TOKEN=s3cret sh contrib/sg-git-askpass "Password for 'https://x-access-token@github.com': ")" = s3cret ] && echo env-ok
printf 's3cret\r\n' > /tmp/claude-askpass-tok && [ "$(SG_MIRROR_TOKEN_FILE=/tmp/claude-askpass-tok sh contrib/sg-git-askpass "Password: ")" = s3cret ] && echo file-ok; rm -f /tmp/claude-askpass-tok
```
Expected: `user-ok`, `env-ok` and `file-ok`.

- [ ] **Step 2: The Dockerfile.** Replace the build stage and the binary `COPY` with the following, so the build cross-compiles natively instead of running Go under QEMU:

```dockerfile
# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/sg-server ./cmd/sg

FROM alpine:3.20
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 10001 -h /home/sg sg \
 && mkdir -p /data && chown sg:sg /data
COPY --from=build /out/sg-server /out/sg /usr/local/bin/
COPY --chmod=0755 contrib/sg-git-askpass /usr/local/bin/sg-git-askpass
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
```

Keep everything from `USER sg` onward unchanged. `.dockerignore` excludes `docs`, `dist`, `.github` and `*_test.go`, so `contrib/` is included. Verify that it still is.

- [ ] **Step 3: Smoke checks for the new image contents.** In `scripts/docker-smoke.sh`, insert this block after the `TOKEN=...` line and before the `go test` line:

```bash
# The image ships sg for admin work inside the container.
LIST=$(docker exec "$NAME" sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)" >/dev/null \
  && sg repo create smoke-admin >/dev/null && sg repo list')
grep -q '^smoke-admin ' <<<"$LIST"
# sg-git-askpass answers git's https prompts from the environment.
[ "$(docker exec "$NAME" sg-git-askpass "Username for 'https://github.com': ")" = x-access-token ]
[ "$(docker exec -e SG_MIRROR_TOKEN=s3cret "$NAME" sg-git-askpass "Password for 'https://x-access-token@github.com': ")" = s3cret ]
echo "in-container sg and askpass OK"
```

Run: `./scripts/docker-smoke.sh`
Expected: it ends with `in-container sg and askpass OK` and then `docker smoke OK`. Afterwards, free disk with `docker image rm sgit-server:smoke`. Do not prune other images or the builder cache.

If `docker buildx ls` lists `linux/arm64` for a builder, also run `docker buildx build --platform linux/arm64 -t sgit-server:arm64-check .` and then `docker image rm sgit-server:arm64-check`. If arm64 is not available, say so in the report and do not install QEMU.

- [ ] **Step 4: The release workflow.** Create `.github/workflows/release.yml`:

```yaml
name: release
on:
  push:
    tags: ['v*']
permissions:
  contents: read
  packages: write
jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race -count=1 ./...
  image:
    runs-on: ubuntu-24.04
    needs: test
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-qemu-action@v3
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: meta
        uses: docker/metadata-action@v5
        with:
          images: ghcr.io/hermes98761234/sgit-server
          tags: |
            type=semver,pattern={{version}}
            type=semver,pattern={{major}}.{{minor}}
            type=raw,value=latest,enable=${{ !contains(github.ref_name, '-') }}
      - uses: docker/build-push-action@v6
        with:
          context: .
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
```

QEMU is only needed for the runtime stage's `apk add`, because Go cross-compiles.

- [ ] **Step 5: docker-compose mirror examples.** In `docker-compose.yml`, add these commented lines to the service's `environment` block:

```yaml
        # Mirror/import over https with a token (see README "Mirror credentials"):
        # GIT_ASKPASS: /usr/local/bin/sg-git-askpass
        # SG_MIRROR_TOKEN_FILE: /run/secrets/github_token
```

Add these to `volumes`:

```yaml
        # Mirror/import over ssh: id_ed25519 (0600, owned by uid 10001) + known_hosts
        # - ./mirror-ssh:/home/sg/.ssh:ro
```

Then add these at the end of the file:

```yaml
# secrets:
#   github_token:
#     file: ./github_token
```

Also add the comment `# secrets: [github_token]` under the service. Run `docker compose config -q` to check that the file still parses.

- [ ] **Step 6: README.** Insert two sections after "Run the cache daemon (`sgd`)" and before "Run without Docker". Use this text verbatim:

````markdown
## Administration

Admin commands need an admin token. They always talk to the server directly, never through `sgd`.

```bash
sg token create bob --email bob@example.com   # prints the token once; give it to bob
sg acl grant bob web write                    # or: read
sg acl revoke bob web
sg token revoke <id>                          # the id printed by token create
sg repo create web
sg repo list
sg repo status web
```

`--admin` and `--email` take effect only when `token create` creates the user.

The server image ships `sg` too, so you can administer from inside the container:

```bash
docker compose exec sg-server sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)"'
docker compose exec sg-server sg repo list
```

## Importing and mirroring

`sg repo import web --from https://github.com/acme/web.git` creates `web` from the source's `main` and `release/*` branches. Other branches and tags are not copied, and the source must have a `main` branch.

`--mirror URL` on `repo create` or `repo import` makes the server push `main` to that URL after every land, in the background. Each push uses `--force-with-lease=main:<last mirrored commit>`:

- **The mirror is never overwritten.** If someone changes the mirror's `main` outside sgit, the push is refused and `sg repo status` shows the error. To resume, reset the mirror's `main` to the `last:` commit that `sg repo status` prints.
- **A new mirror must be empty** (no `main` branch). The exception is the URL you imported from: `sg repo import web --from URL --mirror URL` mirrors back to the source.
- **Failed pushes retry automatically,** with exponential backoff up to 5 minutes. A server restart catches up any missed pushes.
- **Allowed URLs** are `https://`, `ssh://` and `user@host:path`. A URL containing credentials is rejected, because it would be stored in the server database.

### Mirror credentials

The server runs git with an empty global config, so credential helpers in `~/.gitconfig` are not used. The same credentials serve both imports and mirrors. Choose one of the following.

**SSH deploy key.** Add a deploy key with write access to the GitHub repo. Then put the key in a directory that you mount read-only at `/home/sg/.ssh`:

```bash
mkdir mirror-ssh && ssh-keygen -t ed25519 -N '' -f mirror-ssh/id_ed25519
ssh-keyscan github.com > mirror-ssh/known_hosts      # check it against GitHub's published fingerprints
sudo chown -R 10001 mirror-ssh && chmod 600 mirror-ssh/id_ed25519
```

```yaml
    volumes:
      - sgdata:/data
      - ./mirror-ssh:/home/sg/.ssh:ro
```

Then use `--mirror git@github.com:acme/web.git`.

**HTTPS token.** The image includes `sg-git-askpass`, which answers git's prompts from the environment:

```yaml
services:
  sg-server:
    environment:
      GIT_ASKPASS: /usr/local/bin/sg-git-askpass
      SG_MIRROR_TOKEN_FILE: /run/secrets/github_token   # or SG_MIRROR_TOKEN: <token>
    secrets: [github_token]
secrets:
  github_token:
    file: ./github_token   # fine-grained token with Contents: read and write
```

Then use `--mirror https://github.com/acme/web.git`. The username defaults to `x-access-token`; set `SG_MIRROR_USER` to change it.

### Releases

Pushing a tag `vX.Y.Z` runs the tests, then publishes `ghcr.io/hermes98761234/sgit-server:X.Y.Z`, `:X.Y` and `:latest` for linux/amd64 and linux/arm64. Prerelease tags such as `v1.0.0-rc1` do not move `latest`.
````

Also add the three admin commands to the command list in "Use the CLI", if that section lists commands.

- [ ] **Step 7: Spec notes.** In the spec, append a subsection at the end of §8, matching the style of the existing "### Phase 3 implementation notes" under §7:

```markdown
### Phase 4 implementation notes

- **Import.** Import is `git init --bare` followed by a refspec fetch of `main` and `release/*`, with no tags. `clone --single-branch` cannot take both. A source without `main` is rejected, and a failed import leaves nothing behind.
- **Mirror state.** It lives in the `mirror_state` table: `last_mirrored`, `last_error`, `last_attempt` and `pending`. `sg repo status` computes `pending` as `main != last_mirrored`.
- **Pushes.** The worker pushes a SHA snapshot with a lease. An empty `last_mirrored` means the mirror must not have `main`, which is why new mirrors must start empty. The exception is a repo imported with `--mirror` equal to `--from`, which starts with `last_mirrored` = the imported main.
- **Rejected pushes.** When a push is rejected, `ls-remote` checks whether the mirror already has our `main`. If it does (a lost bookkeeping write), that counts as success.
- **URLs** must be https, ssh or scp-style, and must not contain credentials. git runs with `GIT_ALLOW_PROTOCOL=https:ssh`. Credentials come from an SSH key mounted at `/home/sg/.ssh` or from `GIT_ASKPASS=sg-git-askpass`; see the README.
- **Deferred:** changing a repo's `mirror_url` after creation.
```

- [ ] **Step 8: Final checks, then report**

Run: `gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: everything is clean, and the smoke script from Step 3 has passed. Then report.
