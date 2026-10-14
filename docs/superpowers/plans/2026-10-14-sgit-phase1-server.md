# sgit Phase 1: Server Core + Docker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `sg-server`, a gRPC server over bare git repos, and ship it as a Docker image. It covers token auth, per-repo ACLs, blob upload and object fetch, drafts, server-side land with compare-and-swap on `main`, and conflict reporting.

**Architecture:** Go module `github.com/hermes98761234/sgit`.
- `internal/gitx` wraps git plumbing: persistent `cat-file --batch-command` processes, temp-index tree building, `merge-tree --write-tree`, and `update-ref` CAS.
- `internal/store` is SQLite (modernc, pure Go) and holds users, tokens, ACLs, repos and drafts.
- `internal/auth` does token hashing, `Authorize`, and gRPC interceptors.
- `internal/server` implements the `sgit.v1.Repo` and `sgit.v1.Admin` services.
- `internal/servercmd` provides the `init/serve/gen-cert/health` subcommands.
- A multi-stage Dockerfile and compose file run it with a `/data` volume.

**Tech Stack:** Go ≥1.23, git ≥2.40, gRPC + protobuf (protoc, protoc-gen-go, protoc-gen-go-grpc), `modernc.org/sqlite`, `github.com/klauspost/compress/zstd`, Docker, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-10-14-sgit-design.md` (phase 1 = spec §12 item 1, plus the Docker server image from spec §13).

## Global Constraints

- **Shell setup.** Every shell command assumes this was run first:
  ```bash
  cd /home/vkopytsia/Projects/new/sgit
  export PATH=$HOME/.local/go/bin:$HOME/.local/protoc/bin:$HOME/go/bin:$PATH
  PUSH=/tmp/claude-1000/-home-vkopytsia-Projects-new/1891487f-5dc9-4cb5-80f6-b0b8e3cb203f/scratchpad/ghpush-nopages.mjs
  ```
- **No raw git for publishing.** The project dir is not a git repo. "Commit" means `node "$PUSH" . sgit "<message>"`. That command replaces the remote tree with the local dir, ignoring `node_modules`, `dist`, `.git`, `.tmp`, `coverage` and `.vite`. If `$PUSH` is missing, rebuild it: copy `/home/vkopytsia/Projects/new/_tools/ghpush.mjs`, delete the "Pages via Actions" block, remove both `homepage:` fields, and set the `Claude-Session` trailer to the current session.
- **Real name.** Never write the user's real name anywhere. The LICENSE holder is `The sgit authors`, and test identities are `admin`, `alice`, `bob` and `direct`.
- **Versions.**
  - Module path: `github.com/hermes98761234/sgit`. Go directive ≥ 1.23.
  - Server git is ≥ 2.40, because `merge-tree --write-tree --merge-base` is used. Local git is 2.43; Alpine 3.20 ships 2.45.
- **Proto.**
  - Package `sgit.v1`, Go package `github.com/hermes98761234/sgit/gen/sgit/v1` (alias `sgitv1`).
  - Generated code lives in `gen/` and is committed.
- **Default port.** `7419`.
- **Data dir layout.** `<data>/sg.db`, `<data>/repos/<name>.git`, `<data>/admin-token`, and `<data>/tls/{cert,key}.pem` (Docker self-signed).
- **Refs.**
  - `refs/heads/main` (`gitx.MainRef`)
  - draft head: `refs/sg/drafts/<id>/head`
  - draft versions: `refs/sg/drafts/<id>/v<n>`
  - Draft ids match `^[a-z2-7]{8}$`.
- **Tokens.** `sgt_` + lowercase unpadded base32 of 32 random bytes. The DB stores only the SHA-256. Token ids match `^[a-z2-7]{8}$`.
- **Names.**
  - Repo names: `^[a-z0-9][a-z0-9._-]{0,63}$`, not ending in `.git`, containing no `..`.
  - User names: `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`.
- **Path rules.**
  - A path must be non-empty, ≤4096 bytes, valid UTF-8, contain no NUL/CR/LF, not start with `/`, satisfy `path.Clean(p)==p`, and have no `.` or `..` segment.
  - No segment equal (case-insensitive) to `.git` or `.sg`. `.sg` is the client metadata dir.
  - Modes are `100644` (the default when empty), `100755` and `120000`.
- **Limits** (`server.Config` defaults): `MaxBlobBytes` = 100 MiB, `MaxChanges` = 10000, `LandRetries` = 5. Max SHAs per `GetObjects`/`HasObjects` request is 10000. `rpc.MaxMsgSize` = 256 MiB. Upload chunks are ≤ 1 MiB.
- **Server env vars.** `SG_DATA_DIR`, `SG_LISTEN`, `SG_TLS_CERT`, `SG_TLS_KEY`, `SG_DEV_INSECURE`, `SG_TLS_SELF_SIGNED`, `SG_TLS_HOSTS`, `SG_LOG_LEVEL`, `SG_MAX_BLOB_MB`.
- **Image.** `ghcr.io/hermes98761234/sgit-server`. The registry publishing workflow is phase 4. This phase builds locally and in CI only.
- **git isolation.** Every git invocation runs with `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0` and `LC_ALL=C`.
- **Race detector.** `go test -race` needs cgo/gcc. If `gcc` is missing locally, run `go test ./...` locally. CI always runs with `-race`.
- **Out of scope here:** CLI, daemon, mirror, import, and `RepoStatus` (phases 2–4). Do not add stubs for them.

## Review Focus

These are the conditions the spec implies but no feature test naturally exercises, most likely first. Each one is pinned by a test in the task named.

1. **Blob larger than gRPC's 4 MiB default message size.** A 10 MiB random file must upload and fetch intact. Pinned in Task 9 (`TestLargeBlobRoundTrip`).
2. **Paths with spaces, Unicode, or a file replacing a directory.** These must commit and list exactly. Pinned in Task 3 (`TestBuildTreeUnicodeAndReplace`) and Task 10 (`TestCreateDraftUnicodePath`).
3. **Another user touching someone's draft.** Update, land or abandon by a non-owner who has write access must return `PermissionDenied`, while admin may. Pinned in Task 10 (`TestDraftOwnership`) and Task 11 (`TestLandByNonOwner`).
4. **Landing a draft that is already landed or abandoned.** It must return `FailedPrecondition` and never create a second commit on main. Pinned in Task 11 (`TestLandTwiceAndAbandoned`).
5. **`main` moved by an external writer between merge and CAS.** The server must retry and still land, or after `LandRetries` give up with `ResourceExhausted` and leave the draft open. Pinned in Task 11 (`TestLandRetriesWhenMainMoves`, `TestLandGivesUpWhenMainKeepsMoving`).

---

## File Structure

```
go.mod, go.sum, Makefile, doc.go, README.md, LICENSE
.github/workflows/ci.yml
proto/sgit/v1/sgit.proto           service + message definitions
gen/sgit/v1/*.pb.go                generated (committed)
internal/objfmt/objfmt.go          git blob hashing + raw tree parsing (no git binary; reused by client in phase 2)
internal/gitx/run.go               errors, constants, runGit, env isolation
internal/gitx/repo.go              Repo, Init, Open, Close, ReadRef, ResolveRev, UpdateRef, WriteBlob
internal/gitx/catfile.go           cat-file --batch-command pool; ReadObject, ObjectInfo
internal/gitx/commit.go            Signature, Commit, CommitTree, ReadCommit
internal/gitx/tree.go              Change, BuildTree, ListFiles
internal/gitx/merge.go             MergeTree, MergeFile
internal/gitx/history.go           Log, Blame, Diff
internal/store/store.go            SQLite schema + all queries
internal/auth/auth.go              tokens, Authorize, interceptors, request id
internal/rpc/rpc.go                shared gRPC options, token credentials, UploadBlobs helper
internal/server/server.go          Config, Server, New, NewGRPC, Register, error mapping
internal/server/repos.go           repo name validation, repoManager
internal/server/validate.go        path/mode validation, change conversion
internal/server/admin.go           Admin service
internal/server/read.go            ResolveRev, GetObjects, HasObjects, UploadBlobs, Log, Blame, Diff, MergeFile
internal/server/drafts.go          Create/Update/Get/List/AbandonDraft
internal/server/land.go            Land, CommitAndLand, CAS loop, conflict detail
internal/testenv/testenv.go        in-process server harness for tests (reused by phases 2–3)
internal/tlsutil/tlsutil.go        self-signed cert generation
internal/servercmd/servercmd.go    init / serve / gen-cert / health
cmd/sg-server/main.go              thin main
internal/smoke/docker_smoke_test.go  build tag `docker`
Dockerfile, .dockerignore, docker-entrypoint.sh, docker-compose.yml, scripts/docker-smoke.sh
```

---

### Task 1: Toolchain and module scaffold

**Files:**
- Create: `go.mod`, `doc.go`, `Makefile`, `.github/workflows/ci.yml`

**Interfaces:**
- Produces: a buildable module `github.com/hermes98761234/sgit`, plus `make gen|test|vet|lint|build`.

- [ ] **Step 1: Install Go (no sudo)**

```bash
mkdir -p ~/.local
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VER=$(curl -fsSL 'https://go.dev/dl/?mode=json' | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["version"])')
curl -fsSL "https://go.dev/dl/${VER}.linux-${ARCH}.tar.gz" | tar -C ~/.local -xz
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
go version
```
Expected: `go version go1.2x.y linux/amd64`, at 1.23 or newer.

- [ ] **Step 2: Install protoc, the plugins and staticcheck**

```bash
PVER=$(gh release view -R protocolbuffers/protobuf --json tagName --jq .tagName | sed 's/^v//')
PARCH=$(uname -m | sed 's/aarch64/aarch_64/')
curl -fsSL -o /tmp/protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v${PVER}/protoc-${PVER}-linux-${PARCH}.zip"
rm -rf ~/.local/protoc && mkdir -p ~/.local/protoc && unzip -q /tmp/protoc.zip -d ~/.local/protoc && rm /tmp/protoc.zip
export PATH=$HOME/.local/protoc/bin:$PATH
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
protoc --version && protoc-gen-go --version && protoc-gen-go-grpc --version && git --version && (gcc --version | head -1 || echo "no gcc: run tests without -race locally")
```
Expected: all versions print, and git is 2.43.0.

- [ ] **Step 3: Initialize the module and create the files**

```bash
go mod init github.com/hermes98761234/sgit
```

`doc.go`:
```go
// Package sgit is Scion-style source control on top of git: thin clients
// without .git, server-side commits and lands with compare-and-swap on main,
// and a shared per-node object cache. See docs/superpowers/specs.
package sgit
```

`Makefile` (recipe lines are indented with a TAB):
```make
export PATH := $(HOME)/.local/go/bin:$(HOME)/.local/protoc/bin:$(HOME)/go/bin:$(PATH)
MODULE := github.com/hermes98761234/sgit

.PHONY: gen test vet lint build docker smoke

gen:
	protoc -I proto \
	  --go_out=. --go_opt=module=$(MODULE) \
	  --go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
	  proto/sgit/v1/*.proto

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	staticcheck ./...

build:
	CGO_ENABLED=0 go build -trimpath -o dist/ ./cmd/...

docker:
	docker build -t sgit-server:dev .

smoke:
	./scripts/docker-smoke.sh
```

`.github/workflows/ci.yml`:
```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: git --version
      - run: go vet ./...
      - run: go run honnef.co/go/tools/cmd/staticcheck@latest ./...
      - run: go test -race -count=1 ./...
```

- [ ] **Step 4: Verify**

Run: `go vet ./... && go build ./...`
Expected: no output, exit 0.

- [ ] **Step 5: Commit**

```bash
node "$PUSH" . sgit "chore: go module scaffold, Makefile, CI"
gh run list -R hermes98761234/sgit -L 1
```
Expected: push prints `pushed N files`, and a `ci` run appears (it will pass with no tests).

---

### Task 2: gitx core (run, repo, cat-file pool, refs, blobs, commits)

**Files:**
- Create: `internal/gitx/run.go`, `internal/gitx/repo.go`, `internal/gitx/catfile.go`, `internal/gitx/commit.go`
- Test: `internal/gitx/repo_test.go`

**Interfaces:**
- Produces:
  - Errors: `gitx.ErrNotFound`, `gitx.ErrInvalid`, `gitx.ErrRefMoved`, `*gitx.GitError{Args []string; ExitCode int; Stderr string}`
  - Constants: `gitx.ZeroSHA`, `gitx.EmptyTree`, `gitx.MainRef`
  - `gitx.IsSHA(string) bool`
  - Lifecycle: `gitx.Init(ctx, dir string, sig Signature) (*Repo, error)` creates a bare repo whose `main` is a single "Initial commit\n" with the empty tree. `gitx.Open(dir string) *Repo`, `(*Repo).Close() error`, `(*Repo).Dir() string`
  - Objects: `(*Repo).ReadObject(ctx, sha) (Object{SHA, Type string; Data []byte}, error)`, `(*Repo).ObjectInfo(ctx, sha) (typ string, size int64, err error)`, `(*Repo).WriteBlob(ctx, data []byte) (string, error)`
  - Refs: `(*Repo).ReadRef(ctx, ref) (string, error)`, `(*Repo).ResolveRev(ctx, rev) (string, error)`, `(*Repo).UpdateRef(ctx, ref, newSHA, oldSHA string) error` (where `oldSHA==""` means the ref must not exist, and a mismatch returns `ErrRefMoved`)
  - Commits: `gitx.Signature{Name, Email string; When time.Time}`, `gitx.Commit{SHA, Tree string; Parents []string; Author, Committer Signature; Message string}`, `(*Repo).CommitTree(ctx, tree string, parents []string, msg string, author, committer Signature) (string, error)`, `(*Repo).ReadCommit(ctx, sha) (Commit, error)`

- [ ] **Step 1: Write the failing tests**

`internal/gitx/repo_test.go`:
```go
package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var testSig = Signature{Name: "alice", Email: "alice@sgit.local", When: time.Unix(1700000000, 0).UTC()}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(context.Background(), filepath.Join(t.TempDir(), "r.git"), testSig)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestInitCreatesMain(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	main, err := r.ReadRef(ctx, MainRef)
	if err != nil || !IsSHA(main) {
		t.Fatalf("ReadRef main = %q, %v", main, err)
	}
	c, err := r.ReadCommit(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tree != EmptyTree || len(c.Parents) != 0 || c.Message != "Initial commit\n" {
		t.Fatalf("unexpected initial commit: %+v", c)
	}
	if c.Author.Name != "alice" || c.Author.Email != "alice@sgit.local" || !c.Author.When.Equal(testSig.When) {
		t.Fatalf("author = %+v", c.Author)
	}
}

func TestWriteAndReadBlob(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	sha, err := r.WriteBlob(ctx, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sha != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("sha = %s", sha)
	}
	obj, err := r.ReadObject(ctx, sha)
	if err != nil || obj.Type != "blob" || string(obj.Data) != "hello\n" {
		t.Fatalf("ReadObject = %+v, %v", obj, err)
	}
	typ, size, err := r.ObjectInfo(ctx, sha)
	if err != nil || typ != "blob" || size != 6 {
		t.Fatalf("ObjectInfo = %s %d %v", typ, size, err)
	}
}

func TestReadObjectMissingKeepsPoolHealthy(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	missing := "1111111111111111111111111111111111111111"
	for i := 0; i < 10; i++ {
		if _, err := r.ReadObject(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if _, err := r.ReadObject(ctx, "nothex"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := r.ReadObject(ctx, EmptyTree); err != nil {
		t.Fatalf("pool broken after misses: %v", err)
	}
}

func TestResolveRev(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	main, _ := r.ReadRef(ctx, MainRef)
	if got, err := r.ResolveRev(ctx, "main"); err != nil || got != main {
		t.Fatalf("ResolveRev main = %s, %v", got, err)
	}
	if got, err := r.ResolveRev(ctx, main); err != nil || got != main {
		t.Fatalf("ResolveRev sha = %s, %v", got, err)
	}
	if _, err := r.ResolveRev(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	for _, bad := range []string{"", "-x", "a b"} {
		if _, err := r.ResolveRev(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ResolveRev(%q): want ErrInvalid, got %v", bad, err)
		}
	}
}

func TestUpdateRefCAS(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	main, _ := r.ReadRef(ctx, MainRef)
	c2, err := r.CommitTree(ctx, EmptyTree, []string{main}, "second", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, MainRef, c2, c2); !errors.Is(err, ErrRefMoved) {
		t.Fatalf("stale old: want ErrRefMoved, got %v", err)
	}
	if err := r.UpdateRef(ctx, MainRef, c2, main); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/sg/drafts/abcdefgh/head", c2, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/sg/drafts/abcdefgh/head", c2, ""); !errors.Is(err, ErrRefMoved) {
		t.Fatalf("create existing: want ErrRefMoved, got %v", err)
	}
	if _, err := r.ReadRef(ctx, "refs/heads/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ref: want ErrNotFound, got %v", err)
	}
	c, _ := r.ReadCommit(ctx, c2)
	if c.Message != "second\n" || len(c.Parents) != 1 || c.Parents[0] != main {
		t.Fatalf("commit = %+v", c)
	}
}

func TestConcurrentReads(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	sha, _ := r.WriteBlob(ctx, []byte("x"))
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obj, err := r.ReadObject(ctx, sha)
			if err == nil && string(obj.Data) != "x" {
				err = errors.New("bad data")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/gitx/`
Expected: FAIL to compile (`undefined: Init`, …).

- [ ] **Step 3: Implement**

`internal/gitx/run.go`:
```go
// Package gitx wraps the git plumbing commands sg-server needs. It never
// touches a working tree: all repos are bare and all trees are built through
// temporary index files.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var (
	ErrNotFound = errors.New("gitx: not found")
	ErrInvalid  = errors.New("gitx: invalid argument")
	ErrRefMoved = errors.New("gitx: ref moved")
)

const (
	ZeroSHA   = "0000000000000000000000000000000000000000"
	EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	MainRef   = "refs/heads/main"
)

// GitError is a git command that exited non-zero.
type GitError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.ExitCode, strings.TrimSpace(e.Stderr))
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsSHA reports whether s is a full lowercase hex SHA-1.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// baseEnv isolates git from host system/global config (hooks, signing, helpers).
func baseEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LC_ALL=C",
	)
}

// runGit runs git and returns stdout. On a non-zero exit it returns stdout
// together with a *GitError, because some commands (merge-tree) report
// results through their exit code.
func runGit(ctx context.Context, stdin io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(baseEnv(), extraEnv...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &GitError{Args: args, ExitCode: ee.ExitCode(), Stderr: stderr.String()}
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func exitCode(err error) int {
	var ge *GitError
	if errors.As(err, &ge) {
		return ge.ExitCode
	}
	return -1
}
```

`internal/gitx/repo.go`:
```go
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const catFilePoolSize = 4

// Repo is a handle to one bare repository. It is safe for concurrent use.
type Repo struct {
	dir    string
	pool   chan *catFile
	mu     sync.Mutex
	procs  []*catFile
	closed bool
}

// Open returns a handle to an existing bare repo. Processes start lazily.
func Open(dir string) *Repo {
	return &Repo{dir: dir, pool: make(chan *catFile, catFilePoolSize)}
}

// Init creates a bare repo at dir whose main points at an initial empty commit.
func Init(ctx context.Context, dir string, sig Signature) (*Repo, error) {
	if _, err := runGit(ctx, nil, nil, "init", "--bare", "--quiet", "--initial-branch=main", dir); err != nil {
		return nil, err
	}
	r := Open(dir)
	out, err := r.run(ctx, strings.NewReader(""), nil, "hash-object", "-w", "-t", "tree", "--stdin")
	if err != nil {
		return nil, err
	}
	if got := strings.TrimSpace(string(out)); got != EmptyTree {
		return nil, fmt.Errorf("gitx: unexpected empty tree id %q", got)
	}
	c, err := r.CommitTree(ctx, EmptyTree, nil, "Initial commit\n", sig, sig)
	if err != nil {
		return nil, err
	}
	if err := r.UpdateRef(ctx, MainRef, c, ""); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Repo) Dir() string { return r.dir }

// Close stops all cat-file processes.
func (r *Repo) Close() error {
	r.mu.Lock()
	r.closed = true
	procs := r.procs
	r.procs = nil
	r.mu.Unlock()
	for _, p := range procs {
		p.close()
	}
	return nil
}

func (r *Repo) run(ctx context.Context, stdin io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	return runGit(ctx, stdin, extraEnv, append([]string{"--git-dir=" + r.dir}, args...)...)
}

// ReadRef returns the SHA a full ref name points to, or ErrNotFound.
func (r *Repo) ReadRef(ctx context.Context, ref string) (string, error) {
	out, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		if exitCode(err) == 1 {
			return "", ErrNotFound
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveRev resolves a revision expression (branch name, SHA, ...) to a commit SHA.
func (r *Repo) ResolveRev(ctx context.Context, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, " \t\r\n\x00") || len(rev) > 256 {
		return "", fmt.Errorf("%w: invalid revision %q", ErrInvalid, rev)
	}
	out, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		if exitCode(err) == 1 {
			return "", fmt.Errorf("%w: revision %q", ErrNotFound, rev)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// UpdateRef atomically moves ref from oldSHA to newSHA. oldSHA == "" means the
// ref must not exist yet. A mismatch (or lock contention) returns ErrRefMoved.
func (r *Repo) UpdateRef(ctx context.Context, ref, newSHA, oldSHA string) error {
	if oldSHA == "" {
		oldSHA = ZeroSHA
	}
	_, err := r.run(ctx, nil, nil, "update-ref", "--no-deref", ref, newSHA, oldSHA)
	if err == nil {
		return nil
	}
	var ge *GitError
	if !errors.As(err, &ge) {
		return err
	}
	cur, rerr := r.ReadRef(ctx, ref)
	switch {
	case errors.Is(rerr, ErrNotFound):
		cur = ZeroSHA
	case rerr != nil:
		return err
	}
	if cur != oldSHA || strings.Contains(ge.Stderr, ".lock") {
		return ErrRefMoved
	}
	return err
}

// WriteBlob stores data as a blob and returns its SHA.
func (r *Repo) WriteBlob(ctx context.Context, data []byte) (string, error) {
	out, err := r.run(ctx, bytes.NewReader(data), nil, "hash-object", "-w", "-t", "blob", "--no-filters", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
```

`internal/gitx/catfile.go`:
```go
package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Object is a raw git object.
type Object struct {
	SHA  string
	Type string
	Data []byte
}

type catFile struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	once sync.Once
}

func startCatFile(dir string) (*catFile, error) {
	cmd := exec.Command("git", "--git-dir="+dir, "cat-file", "--batch-command")
	cmd.Env = baseEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 64<<10)}, nil
}

func (c *catFile) close() {
	c.once.Do(func() {
		_ = c.in.Close()
		_ = c.cmd.Wait()
	})
}

// request sends "<command> <sha>" and parses the reply. command is "info" or "contents".
func (c *catFile) request(command, sha string, withBody bool) (typ string, size int64, body []byte, err error) {
	if _, err = fmt.Fprintf(c.in, "%s %s\n", command, sha); err != nil {
		return "", 0, nil, err
	}
	var line string
	line, err = c.out.ReadString('\n')
	if err != nil {
		return "", 0, nil, err
	}
	f := strings.Fields(line)
	if len(f) == 2 && f[1] == "missing" {
		return "", 0, nil, fmt.Errorf("%w: object %s", ErrNotFound, sha)
	}
	if len(f) != 3 {
		return "", 0, nil, fmt.Errorf("gitx: unexpected cat-file header %q", line)
	}
	size, err = strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return "", 0, nil, err
	}
	if !withBody {
		return f[1], size, nil, nil
	}
	buf := make([]byte, size+1) // body + trailing LF
	if _, err = io.ReadFull(c.out, buf); err != nil {
		return "", 0, nil, err
	}
	return f[1], size, buf[:size], nil
}

func (r *Repo) acquire(ctx context.Context) (*catFile, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("gitx: repo closed")
	}
	select {
	case c := <-r.pool:
		r.mu.Unlock()
		return c, nil
	default:
	}
	if len(r.procs) < catFilePoolSize {
		c, err := startCatFile(r.dir)
		if err == nil {
			r.procs = append(r.procs, c)
		}
		r.mu.Unlock()
		return c, err
	}
	r.mu.Unlock()
	select {
	case c := <-r.pool:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Repo) release(c *catFile, healthy bool) {
	r.mu.Lock()
	if healthy && !r.closed {
		r.mu.Unlock()
		r.pool <- c // never blocks: len(procs) <= cap(pool)
		return
	}
	for i, p := range r.procs {
		if p == c {
			r.procs = append(r.procs[:i], r.procs[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	c.close()
}

func (r *Repo) withCatFile(ctx context.Context, fn func(*catFile) error) error {
	c, err := r.acquire(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	r.release(c, err == nil || errors.Is(err, ErrNotFound))
	return err
}

// ReadObject returns the object's type and content, or ErrNotFound.
func (r *Repo) ReadObject(ctx context.Context, sha string) (Object, error) {
	if !IsSHA(sha) {
		return Object{}, fmt.Errorf("%w: bad object id %q", ErrInvalid, sha)
	}
	var obj Object
	err := r.withCatFile(ctx, func(c *catFile) error {
		typ, _, body, err := c.request("contents", sha, true)
		obj = Object{SHA: sha, Type: typ, Data: body}
		return err
	})
	return obj, err
}

// ObjectInfo returns the object's type and size without reading its content.
func (r *Repo) ObjectInfo(ctx context.Context, sha string) (string, int64, error) {
	if !IsSHA(sha) {
		return "", 0, fmt.Errorf("%w: bad object id %q", ErrInvalid, sha)
	}
	var typ string
	var size int64
	err := r.withCatFile(ctx, func(c *catFile) error {
		var err error
		typ, size, _, err = c.request("info", sha, false)
		return err
	})
	return typ, size, err
}
```

`internal/gitx/commit.go`:
```go
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signature is an author or committer identity.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

// Commit is a parsed commit object.
type Commit struct {
	SHA       string
	Tree      string
	Parents   []string
	Author    Signature
	Committer Signature
	Message   string
}

func gitDate(t time.Time) string { return fmt.Sprintf("%d %s", t.Unix(), t.Format("-0700")) }

// CommitTree creates a commit object (no ref is moved) and returns its SHA.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, msg string, author, committer Signature) (string, error) {
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	args := []string{"-c", "commit.gpgSign=false", "commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-")
	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email, "GIT_AUTHOR_DATE=" + gitDate(author.When),
		"GIT_COMMITTER_NAME=" + committer.Name, "GIT_COMMITTER_EMAIL=" + committer.Email, "GIT_COMMITTER_DATE=" + gitDate(committer.When),
	}
	out, err := r.run(ctx, strings.NewReader(msg), env, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadCommit reads and parses a commit object.
func (r *Repo) ReadCommit(ctx context.Context, sha string) (Commit, error) {
	obj, err := r.ReadObject(ctx, sha)
	if err != nil {
		return Commit{}, err
	}
	if obj.Type != "commit" {
		return Commit{}, fmt.Errorf("%w: %s is a %s, not a commit", ErrInvalid, sha, obj.Type)
	}
	return parseCommit(sha, obj.Data)
}

func parseCommit(sha string, data []byte) (Commit, error) {
	header, msg, _ := bytes.Cut(data, []byte("\n\n"))
	c := Commit{SHA: sha, Message: string(msg)}
	for _, line := range strings.Split(string(header), "\n") {
		k, v, _ := strings.Cut(line, " ")
		var err error
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.Author, err = parseSig(v)
		case "committer":
			c.Committer, err = parseSig(v)
		}
		if err != nil {
			return Commit{}, fmt.Errorf("gitx: commit %s: %w", sha, err)
		}
	}
	return c, nil
}

// parseSig parses "Name <email> 1700000000 +0100".
func parseSig(v string) (Signature, error) {
	lt, gt := strings.LastIndex(v, "<"), strings.LastIndex(v, ">")
	if lt < 0 || gt < lt {
		return Signature{}, fmt.Errorf("bad signature %q", v)
	}
	rest := strings.Fields(v[gt+1:])
	if len(rest) != 2 || len(rest[1]) != 5 {
		return Signature{}, fmt.Errorf("bad signature date %q", v)
	}
	secs, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return Signature{}, err
	}
	hh, err1 := strconv.Atoi(rest[1][1:3])
	mm, err2 := strconv.Atoi(rest[1][3:5])
	if err1 != nil || err2 != nil {
		return Signature{}, fmt.Errorf("bad tz %q", rest[1])
	}
	off := hh*3600 + mm*60
	if rest[1][0] == '-' {
		off = -off
	}
	return Signature{
		Name:  strings.TrimSpace(v[:lt]),
		Email: v[lt+1 : gt],
		When:  time.Unix(secs, 0).In(time.FixedZone(rest[1], off)),
	}, nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race ./internal/gitx/` (drop `-race` if there's no gcc)
Expected: `ok  github.com/hermes98761234/sgit/internal/gitx`

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./...
node "$PUSH" . sgit "feat(gitx): repo init, cat-file pool, refs CAS, blobs, commits"
```

---

### Task 3: objfmt plus tree building

**Files:**
- Create: `internal/objfmt/objfmt.go`, `internal/gitx/tree.go`
- Test: `internal/objfmt/objfmt_test.go`, `internal/gitx/tree_test.go`

**Interfaces:**
- Consumes: Task 2 (`Repo`, `WriteBlob`, `ReadObject`, `CommitTree`, `ReadRef`, `ErrInvalid`).
- Produces:
  - `objfmt.BlobSHA(data []byte) string`
  - `objfmt.TreeEntry{Mode, Name, SHA string}` with `IsTree() bool`
  - `objfmt.ParseTree(data []byte) ([]TreeEntry, error)`
  - `gitx.ChangeOp` (`OpAdd`, `OpModify`, `OpDelete`), `gitx.Change{Path string; Op ChangeOp; BlobSHA, Mode string}`
  - `(*Repo).BuildTree(ctx, baseCommit string, changes []Change) (treeSHA string, err error)`: add and modify upsert, delete of a non-file in base returns `ErrInvalid`, a file can replace a directory and vice versa.
  - `gitx.FileEntry{Mode, SHA string}`, `(*Repo).ListFiles(ctx, rev string) (map[string]FileEntry, error)`

- [ ] **Step 1: Write the failing tests**

`internal/objfmt/objfmt_test.go`:
```go
package objfmt

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestBlobSHA(t *testing.T) {
	if got := BlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("got %s", got)
	}
	if got := BlobSHA(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty: got %s", got)
	}
}

func TestParseTree(t *testing.T) {
	a, _ := hex.DecodeString("ce013625030ba8dba906f756967f9e9ca394464a")
	d, _ := hex.DecodeString("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	var raw bytes.Buffer
	raw.WriteString("100644 a b.txt\x00")
	raw.Write(a)
	raw.WriteString("40000 dir\x00")
	raw.Write(d)
	got, err := ParseTree(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []TreeEntry{
		{Mode: "100644", Name: "a b.txt", SHA: "ce013625030ba8dba906f756967f9e9ca394464a"},
		{Mode: "40000", Name: "dir", SHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] || got[0].IsTree() || !got[1].IsTree() {
		t.Fatalf("got %+v", got)
	}
	if _, err := ParseTree([]byte("100644 truncated\x00abc")); err == nil {
		t.Fatal("want error for truncated tree")
	}
}
```

`internal/gitx/tree_test.go`:
```go
package gitx

import (
	"context"
	"errors"
	"testing"

	"github.com/hermes98761234/sgit/internal/objfmt"
)

// commitChanges builds a tree from base+changes, commits it and moves main.
func commitChanges(t *testing.T, r *Repo, changes []Change) string {
	t.Helper()
	ctx := context.Background()
	main, err := r.ReadRef(ctx, MainRef)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.BuildTree(ctx, main, changes)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	c, err := r.CommitTree(ctx, tree, []string{main}, "c", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, MainRef, c, main); err != nil {
		t.Fatal(err)
	}
	return c
}

func blob(t *testing.T, r *Repo, s string) string {
	t.Helper()
	sha, err := r.WriteBlob(context.Background(), []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestBuildTreeAddModifyDelete(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	a, b := blob(t, r, "a\n"), blob(t, r, "b\n")
	commitChanges(t, r, []Change{
		{Path: "a.txt", Op: OpAdd, BlobSHA: a},
		{Path: "x/y/b.sh", Op: OpAdd, BlobSHA: b, Mode: "100755"},
	})
	files, err := r.ListFiles(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if files["a.txt"] != (FileEntry{Mode: "100644", SHA: a}) || files["x/y/b.sh"] != (FileEntry{Mode: "100755", SHA: b}) || len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	commitChanges(t, r, []Change{
		{Path: "a.txt", Op: OpModify, BlobSHA: b},
		{Path: "x/y/b.sh", Op: OpDelete},
	})
	files, _ = r.ListFiles(ctx, "main")
	if len(files) != 1 || files["a.txt"].SHA != b {
		t.Fatalf("after modify/delete: %+v", files)
	}
	main, _ := r.ReadRef(ctx, MainRef)
	if _, err := r.BuildTree(ctx, main, []Change{{Path: "nope.txt", Op: OpDelete}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete missing: want ErrInvalid, got %v", err)
	}
}

func TestBuildTreeUnicodeAndReplace(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	a := blob(t, r, "a\n")
	commitChanges(t, r, []Change{
		{Path: "dir with space/файл.txt", Op: OpAdd, BlobSHA: a},
		{Path: "d/inner.txt", Op: OpAdd, BlobSHA: a},
	})
	files, _ := r.ListFiles(ctx, "main")
	if _, ok := files["dir with space/файл.txt"]; !ok {
		t.Fatalf("unicode path missing: %+v", files)
	}
	// File replaces directory "d".
	commitChanges(t, r, []Change{{Path: "d", Op: OpAdd, BlobSHA: a}})
	files, _ = r.ListFiles(ctx, "main")
	if _, ok := files["d/inner.txt"]; ok || files["d"].SHA != a {
		t.Fatalf("file did not replace dir: %+v", files)
	}
	// Directory replaces file "d".
	commitChanges(t, r, []Change{{Path: "d/again.txt", Op: OpAdd, BlobSHA: a}})
	files, _ = r.ListFiles(ctx, "main")
	if _, ok := files["d"]; ok || files["d/again.txt"].SHA != a {
		t.Fatalf("dir did not replace file: %+v", files)
	}
}

func TestRawTreeParsesWithObjfmt(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	a := blob(t, r, "a\n")
	c := commitChanges(t, r, []Change{{Path: "a.txt", Op: OpAdd, BlobSHA: a}, {Path: "sub/b.txt", Op: OpAdd, BlobSHA: a}})
	cm, _ := r.ReadCommit(ctx, c)
	obj, err := r.ReadObject(ctx, cm.Tree)
	if err != nil || obj.Type != "tree" {
		t.Fatalf("tree read: %v %s", err, obj.Type)
	}
	entries, err := objfmt.ParseTree(obj.Data)
	if err != nil || len(entries) != 2 || entries[0].Name != "a.txt" || entries[0].SHA != a || !entries[1].IsTree() {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if objfmt.BlobSHA([]byte("a\n")) != a {
		t.Fatal("objfmt.BlobSHA disagrees with git")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/objfmt/ ./internal/gitx/`
Expected: FAIL to compile (`undefined: BlobSHA`, `undefined: Change`, …).

- [ ] **Step 3: Implement**

`internal/objfmt/objfmt.go`:
```go
// Package objfmt implements the parts of git's object format that clients need
// without a git binary: blob hashing and raw tree parsing.
package objfmt

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
)

// BlobSHA returns the git object id of data stored as a blob.
func BlobSHA(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// TreeEntry is one entry of a raw git tree object.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "40000", "160000"
	Name string
	SHA  string
}

// IsTree reports whether the entry is a subdirectory.
func (e TreeEntry) IsTree() bool { return e.Mode == "40000" }

var errTruncated = errors.New("objfmt: truncated tree")

// ParseTree parses the binary content of a tree object.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errTruncated
		}
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errTruncated
		}
		nameEnd := sp + 1 + nul
		if len(data) < nameEnd+1+20 {
			return nil, errTruncated
		}
		out = append(out, TreeEntry{
			Mode: string(data[:sp]),
			Name: string(data[sp+1 : nameEnd]),
			SHA:  hex.EncodeToString(data[nameEnd+1 : nameEnd+21]),
		})
		data = data[nameEnd+21:]
	}
	return out, nil
}
```

`internal/gitx/tree.go`:
```go
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChangeOp is the kind of a single path change.
type ChangeOp int

const (
	OpAdd ChangeOp = iota + 1
	OpModify
	OpDelete
)

// Change is one path change relative to a base commit. Add and Modify are
// upserts; Delete requires the path to be a file in the base.
type Change struct {
	Path    string
	Op      ChangeOp
	BlobSHA string
	Mode    string // "" means 100644
}

// FileEntry is a file in a recursive tree listing.
type FileEntry struct {
	Mode string
	SHA  string
}

// ListFiles lists every file (recursively) in rev's tree.
func (r *Repo) ListFiles(ctx context.Context, rev string) (map[string]FileEntry, error) {
	out, err := r.run(ctx, nil, nil, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	files := make(map[string]FileEntry)
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta) // mode type sha
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("gitx: bad ls-tree record %q", rec)
		}
		files[p] = FileEntry{Mode: f[0], SHA: f[2]}
	}
	return files, nil
}

// BuildTree applies changes on top of baseCommit's tree using a temporary
// index and returns the new tree SHA. No refs or working trees are touched.
func (r *Repo) BuildTree(ctx context.Context, baseCommit string, changes []Change) (string, error) {
	var deletes []string
	var info bytes.Buffer
	for _, c := range changes {
		switch c.Op {
		case OpAdd, OpModify:
			mode := c.Mode
			if mode == "" {
				mode = "100644"
			}
			fmt.Fprintf(&info, "%s %s\t%s\n", mode, c.BlobSHA, c.Path)
		case OpDelete:
			deletes = append(deletes, c.Path)
			fmt.Fprintf(&info, "0 %s\t%s\n", ZeroSHA, c.Path)
		default:
			return "", fmt.Errorf("%w: unknown op %d for %q", ErrInvalid, c.Op, c.Path)
		}
	}
	if len(deletes) > 0 {
		files, err := r.ListFiles(ctx, baseCommit)
		if err != nil {
			return "", err
		}
		for _, p := range deletes {
			if _, ok := files[p]; !ok {
				return "", fmt.Errorf("%w: delete %q: no such file in base", ErrInvalid, p)
			}
		}
	}
	tmp, err := os.MkdirTemp("", "sgit-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := r.run(ctx, nil, env, "read-tree", baseCommit); err != nil {
		return "", err
	}
	if _, err := r.run(ctx, &info, env, "update-index", "--replace", "--index-info"); err != nil {
		return "", err
	}
	out, err := r.run(ctx, nil, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race ./internal/objfmt/ ./internal/gitx/`
Expected: both `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./...
node "$PUSH" . sgit "feat(gitx): temp-index tree building, ls-tree; objfmt blob hash + tree parser"
```

---

### Task 4: gitx merge and history

**Files:**
- Create: `internal/gitx/merge.go`, `internal/gitx/history.go`
- Test: `internal/gitx/merge_test.go`, `internal/gitx/history_test.go`

**Interfaces:**
- Consumes: Task 2/3 (`commitChanges` and `blob` test helpers from `tree_test.go`, same package).
- Produces:
  - Merge: `gitx.ConflictFile{Path, Base, Ours, Theirs string}` (blob SHAs, `""` if absent), `gitx.MergeResult{Tree string; Clean bool; Conflicts []ConflictFile}`, `(*Repo).MergeTree(ctx, mergeBase, ours, theirs string) (MergeResult, error)`, and `gitx.MergeFile(ctx, base, ours, theirs []byte, oursLabel, baseLabel, theirsLabel string) (merged []byte, conflicted bool, err error)` (a package func; binary input returns ours and `conflicted=true`)
  - Log: `gitx.LogEntry{SHA, AuthorName, AuthorEmail string; Time time.Time; Subject string}`, `(*Repo).Log(ctx, rev, path string, n int) ([]LogEntry, error)` (newest first)
  - Blame: `gitx.BlameLine{SHA, Author string; Line int; Text string}`, `(*Repo).Blame(ctx, rev, path string) ([]BlameLine, error)` (a missing path returns `ErrNotFound`)
  - Diff: `(*Repo).Diff(ctx, a, b string, paths []string) ([]byte, error)`

- [ ] **Step 1: Write the failing tests**

`internal/gitx/merge_test.go`:
```go
package gitx

import (
	"bytes"
	"context"
	"testing"
)

// fork returns (base, ours, theirs) commits: ours and theirs both start at base.
func fork(t *testing.T, r *Repo, baseContent, oursContent, theirsContent string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	base := commitChanges(t, r, []Change{{Path: "f.txt", Op: OpAdd, BlobSHA: blob(t, r, baseContent)}})
	ours := commitChanges(t, r, []Change{{Path: "f.txt", Op: OpModify, BlobSHA: blob(t, r, oursContent)}})
	tree, err := r.BuildTree(ctx, base, []Change{{Path: "f.txt", Op: OpModify, BlobSHA: blob(t, r, theirsContent)}})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := r.CommitTree(ctx, tree, []string{base}, "theirs", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	return base, ours, theirs
}

func TestMergeTreeClean(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, ours, theirs := fork(t, r, "a\nb\nc\n", "A\nb\nc\n", "a\nb\nC\n")
	res, err := r.MergeTree(ctx, base, ours, theirs)
	if err != nil || !res.Clean || len(res.Conflicts) != 0 || !IsSHA(res.Tree) {
		t.Fatalf("res = %+v, %v", res, err)
	}
	c, _ := r.CommitTree(ctx, res.Tree, []string{ours}, "m", testSig, testSig)
	files, _ := r.ListFiles(ctx, c)
	obj, _ := r.ReadObject(ctx, files["f.txt"].SHA)
	if string(obj.Data) != "A\nb\nC\n" {
		t.Fatalf("merged = %q", obj.Data)
	}
}

func TestMergeTreeConflict(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, ours, theirs := fork(t, r, "a\nb\nc\n", "a\nB1\nc\n", "a\nB2\nc\n")
	res, err := r.MergeTree(ctx, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean || len(res.Conflicts) != 1 {
		t.Fatalf("res = %+v", res)
	}
	cf := res.Conflicts[0]
	if cf.Path != "f.txt" || !IsSHA(cf.Base) || !IsSHA(cf.Ours) || !IsSHA(cf.Theirs) {
		t.Fatalf("conflict = %+v", cf)
	}
	o, _ := r.ReadObject(ctx, cf.Ours)
	if string(o.Data) != "a\nB1\nc\n" {
		t.Fatalf("ours blob = %q", o.Data)
	}
}

func TestMergeFile(t *testing.T) {
	ctx := context.Background()
	out, conflicted, err := MergeFile(ctx, []byte("a\nb\nc\n"), []byte("A\nb\nc\n"), []byte("a\nb\nC\n"), "ours", "base", "theirs")
	if err != nil || conflicted || string(out) != "A\nb\nC\n" {
		t.Fatalf("clean: %q %v %v", out, conflicted, err)
	}
	out, conflicted, err = MergeFile(ctx, []byte("a\nb\n"), []byte("a\nX\n"), []byte("a\nY\n"), "main", "base", "local")
	if err != nil || !conflicted || !bytes.Contains(out, []byte("<<<<<<< main")) || !bytes.Contains(out, []byte(">>>>>>> local")) {
		t.Fatalf("conflict: %q %v %v", out, conflicted, err)
	}
	out, conflicted, err = MergeFile(ctx, []byte("a\x00"), []byte("b\x00"), []byte("c\x00"), "ours", "base", "theirs")
	if err != nil || !conflicted || string(out) != "b\x00" {
		t.Fatalf("binary: %q %v %v", out, conflicted, err)
	}
}
```

`internal/gitx/history_test.go`:
```go
package gitx

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestLogBlameDiff(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c1 := commitChanges(t, r, []Change{{Path: "f.txt", Op: OpAdd, BlobSHA: blob(t, r, "one\n")}})
	c2 := commitChanges(t, r, []Change{
		{Path: "f.txt", Op: OpModify, BlobSHA: blob(t, r, "one\ntwo\n")},
		{Path: "g.txt", Op: OpAdd, BlobSHA: blob(t, r, "g\n")},
	})

	all, err := r.Log(ctx, "main", "", 10)
	if err != nil || len(all) != 3 || all[0].SHA != c2 || all[1].SHA != c1 || all[0].Subject != "c" || all[0].AuthorName != "alice" {
		t.Fatalf("log = %+v, %v", all, err)
	}
	onlyG, _ := r.Log(ctx, "main", "g.txt", 10)
	if len(onlyG) != 1 || onlyG[0].SHA != c2 {
		t.Fatalf("path log = %+v", onlyG)
	}
	limited, _ := r.Log(ctx, "main", "", 1)
	if len(limited) != 1 {
		t.Fatalf("n=1 log = %+v", limited)
	}

	lines, err := r.Blame(ctx, "main", "f.txt")
	if err != nil || len(lines) != 2 {
		t.Fatalf("blame = %+v, %v", lines, err)
	}
	if lines[0].SHA != c1 || lines[0].Line != 1 || lines[0].Text != "one" || lines[0].Author != "alice" ||
		lines[1].SHA != c2 || lines[1].Line != 2 || lines[1].Text != "two" {
		t.Fatalf("blame lines = %+v", lines)
	}
	if _, err := r.Blame(ctx, "main", "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blame missing: want ErrNotFound, got %v", err)
	}

	patch, err := r.Diff(ctx, c1, c2, nil)
	if err != nil || !bytes.Contains(patch, []byte("+two")) || !bytes.Contains(patch, []byte("g.txt")) {
		t.Fatalf("diff = %s, %v", patch, err)
	}
	patch, _ = r.Diff(ctx, c1, c2, []string{"g.txt"})
	if bytes.Contains(patch, []byte("+two")) {
		t.Fatalf("path-limited diff leaked f.txt: %s", patch)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/gitx/`
Expected: FAIL to compile (`undefined: MergeTree`, `undefined: Log`, …).

- [ ] **Step 3: Implement**

`internal/gitx/merge.go`:
```go
package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConflictFile describes one conflicted path. "Ours" is the first side passed
// to MergeTree (main when landing), "Theirs" the second (the draft).
type ConflictFile struct {
	Path   string
	Base   string
	Ours   string
	Theirs string
}

// MergeResult is the outcome of an in-memory 3-way merge.
type MergeResult struct {
	Tree      string
	Clean     bool
	Conflicts []ConflictFile
}

// MergeTree runs `git merge-tree --write-tree` (git >= 2.40) without a worktree.
func (r *Repo) MergeTree(ctx context.Context, mergeBase, ours, theirs string) (MergeResult, error) {
	out, err := r.run(ctx, nil, nil, "merge-tree", "--write-tree", "-z", "--merge-base="+mergeBase, ours, theirs)
	clean := true
	if err != nil {
		if exitCode(err) != 1 {
			return MergeResult{}, err
		}
		clean = false
	}
	res, perr := parseMergeTree(out)
	if perr != nil {
		return MergeResult{}, perr
	}
	res.Clean = clean
	return res, nil
}

// parseMergeTree parses -z output: "<tree>\0" then "<mode> <sha> <stage>\t<path>\0"
// entries, then an empty token, then informational messages (ignored).
func parseMergeTree(out []byte) (MergeResult, error) {
	toks := strings.Split(string(out), "\x00")
	if len(toks) == 0 || !IsSHA(toks[0]) {
		return MergeResult{}, fmt.Errorf("gitx: unexpected merge-tree output %q", out)
	}
	res := MergeResult{Tree: toks[0]}
	byPath := map[string]*ConflictFile{}
	var order []string
	for _, tok := range toks[1:] {
		if tok == "" {
			break
		}
		meta, p, ok := strings.Cut(tok, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			break
		}
		cf := byPath[p]
		if cf == nil {
			cf = &ConflictFile{Path: p}
			byPath[p] = cf
			order = append(order, p)
		}
		switch f[2] {
		case "1":
			cf.Base = f[1]
		case "2":
			cf.Ours = f[1]
		case "3":
			cf.Theirs = f[1]
		}
	}
	for _, p := range order {
		res.Conflicts = append(res.Conflicts, *byPath[p])
	}
	return res, nil
}

// MergeFile 3-way merges file contents with `git merge-file`. On conflict the
// result contains conflict markers labelled with the given labels. Binary
// inputs cannot be merged: ours is returned with conflicted=true.
func MergeFile(ctx context.Context, base, ours, theirs []byte, oursLabel, baseLabel, theirsLabel string) ([]byte, bool, error) {
	dir, err := os.MkdirTemp("", "sgit-merge-*")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	paths := [3]string{filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs")}
	for i, data := range [][]byte{ours, base, theirs} {
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			return nil, false, err
		}
	}
	out, err := runGit(ctx, nil, nil, "merge-file", "-p", "-L", oursLabel, "-L", baseLabel, "-L", theirsLabel, paths[0], paths[1], paths[2])
	if err == nil {
		return out, false, nil
	}
	var ge *GitError
	if errors.As(err, &ge) {
		if ge.ExitCode > 0 && ge.ExitCode < 128 {
			return out, true, nil
		}
		if strings.Contains(strings.ToLower(ge.Stderr), "binary") {
			return ours, true, nil
		}
	}
	return nil, false, err
}
```

`internal/gitx/history.go`:
```go
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LogEntry is one commit in a history listing.
type LogEntry struct {
	SHA         string
	AuthorName  string
	AuthorEmail string
	Time        time.Time
	Subject     string
}

// Log lists up to n commits reachable from rev (newest first), optionally
// limited to those touching path.
func (r *Repo) Log(ctx context.Context, rev, path string, n int) ([]LogEntry, error) {
	sha, err := r.ResolveRev(ctx, rev)
	if err != nil {
		return nil, err
	}
	args := []string{"log", "-z", "--format=%H%x1f%an%x1f%ae%x1f%at%x1f%s", "-n", strconv.Itoa(n), sha}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := r.run(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []LogEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		rec = strings.TrimPrefix(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) != 5 {
			return nil, fmt.Errorf("gitx: bad log record %q", rec)
		}
		secs, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, LogEntry{SHA: f[0], AuthorName: f[1], AuthorEmail: f[2], Time: time.Unix(secs, 0).UTC(), Subject: f[4]})
	}
	return entries, nil
}

// BlameLine attributes one line of a file to a commit.
type BlameLine struct {
	SHA    string
	Author string
	Line   int
	Text   string
}

// Blame attributes each line of path at rev.
func (r *Repo) Blame(ctx context.Context, rev, path string) ([]BlameLine, error) {
	sha, err := r.ResolveRev(ctx, rev)
	if err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, nil, "blame", "--porcelain", sha, "--", path)
	if err != nil {
		if ge, ok := err.(*GitError); ok && strings.Contains(ge.Stderr, "no such path") {
			return nil, fmt.Errorf("%w: path %q at %s", ErrNotFound, path, rev)
		}
		return nil, err
	}
	return parseBlame(out), nil
}

func parseBlame(out []byte) []BlameLine {
	authors := map[string]string{}
	var lines []BlameLine
	var cur BlameLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "\t") {
			cur.Text = l[1:]
			cur.Author = authors[cur.SHA]
			lines = append(lines, cur)
			continue
		}
		if f := strings.Fields(l); len(f) >= 3 && IsSHA(f[0]) {
			n, _ := strconv.Atoi(f[2])
			cur = BlameLine{SHA: f[0], Line: n}
			continue
		}
		if a, ok := strings.CutPrefix(l, "author "); ok {
			authors[cur.SHA] = a
		}
	}
	return lines
}

// Diff returns a unified diff between revisions a and b, optionally limited to paths.
func (r *Repo) Diff(ctx context.Context, a, b string, paths []string) ([]byte, error) {
	sa, err := r.ResolveRev(ctx, a)
	if err != nil {
		return nil, err
	}
	sb, err := r.ResolveRev(ctx, b)
	if err != nil {
		return nil, err
	}
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", sa, sb, "--"}, paths...)
	return r.run(ctx, nil, nil, args...)
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race ./internal/gitx/`
Expected: `ok`. If `TestMergeFile` binary case fails because git merged the NUL content as text, check the exit code and stderr, keep the "binary → ours, conflicted" contract, and adjust the detection branch in `MergeFile`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./...
node "$PUSH" . sgit "feat(gitx): merge-tree, merge-file, log, blame, diff"
```

---

### Task 5: SQLite store

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - Errors: `store.ErrNotFound`, `store.ErrExists`
  - Levels: `store.Level` (`LevelNone=0`, `LevelRead=1`, `LevelWrite=2`)
  - Draft statuses: `store.DraftOpen="open"`, `DraftLanded="landed"`, `DraftAbandoned="abandoned"`
  - Types: `store.User{ID int64; Name, Email string; Admin bool}`, `store.RepoRow{Name, MirrorURL string; CreatedAt time.Time}`, `store.Draft{ID, Repo string; OwnerID int64; OwnerName, Title, Base, Head string; Version int; Status, LandedAs string; CreatedAt, UpdatedAt time.Time}`
  - Lifecycle: `store.Open(path string) (*Store, error)`, `(*Store).Close() error`
  - Users: `CreateUser(ctx, name, email string, admin bool) (User, error)`, `UserByName(ctx, name) (User, error)`, `CountUsers(ctx) (int, error)`
  - Tokens: `CreateToken(ctx, userID int64, id string, hash []byte) error`, `UserByTokenHash(ctx, hash []byte) (User, error)`, `RevokeToken(ctx, id string) error`
  - ACLs: `SetACL(ctx, userID int64, repo string, l Level) error` (`LevelNone` deletes), `ACL(ctx, userID int64, repo string) (Level, error)`
  - Repos: `CreateRepo(ctx, name, mirrorURL string) error`, `GetRepo(ctx, name) (RepoRow, error)`, `ListRepos(ctx) ([]RepoRow, error)`
  - Drafts: `InsertDraft(ctx, d Draft) error`, `GetDraft(ctx, repo, id string) (Draft, error)`, `UpdateDraft(ctx, d Draft) error`, `ListDrafts(ctx, repo string, ownerID int64, status string) ([]Draft, error)` (`ownerID 0` and `status ""` mean no filter)

- [ ] **Step 1: Write the failing tests**

`internal/store/store_test.go`:
```go
package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsersAndTokens(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("CountUsers = %d", n)
	}
	u, err := s.CreateUser(ctx, "alice", "alice@sgit.local", false)
	if err != nil || u.ID == 0 || u.Name != "alice" {
		t.Fatalf("CreateUser = %+v, %v", u, err)
	}
	if _, err := s.CreateUser(ctx, "alice", "x", false); !errors.Is(err, ErrExists) {
		t.Fatalf("dup user: want ErrExists, got %v", err)
	}
	if got, err := s.UserByName(ctx, "alice"); err != nil || got != u {
		t.Fatalf("UserByName = %+v, %v", got, err)
	}
	if _, err := s.UserByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := s.CreateToken(ctx, u.ID, "tokid001", hash); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserByTokenHash(ctx, hash); err != nil || got != u {
		t.Fatalf("UserByTokenHash = %+v, %v", got, err)
	}
	if err := s.RevokeToken(ctx, "tokid001"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByTokenHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token: want ErrNotFound, got %v", err)
	}
	if err := s.RevokeToken(ctx, "tokid001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: want ErrNotFound, got %v", err)
	}
}

func TestReposAndACL(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u, _ := s.CreateUser(ctx, "bob", "bob@sgit.local", false)
	if err := s.CreateRepo(ctx, "web", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRepo(ctx, "web", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("dup repo: want ErrExists, got %v", err)
	}
	if r, err := s.GetRepo(ctx, "web"); err != nil || r.Name != "web" || r.CreatedAt.IsZero() {
		t.Fatalf("GetRepo = %+v, %v", r, err)
	}
	if _, err := s.GetRepo(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if rs, _ := s.ListRepos(ctx); len(rs) != 1 {
		t.Fatalf("ListRepos = %+v", rs)
	}
	if l, _ := s.ACL(ctx, u.ID, "web"); l != LevelNone {
		t.Fatalf("default ACL = %d", l)
	}
	_ = s.SetACL(ctx, u.ID, "web", LevelRead)
	_ = s.SetACL(ctx, u.ID, "web", LevelWrite)
	if l, _ := s.ACL(ctx, u.ID, "web"); l != LevelWrite {
		t.Fatalf("upserted ACL = %d", l)
	}
	_ = s.SetACL(ctx, u.ID, "web", LevelNone)
	if l, _ := s.ACL(ctx, u.ID, "web"); l != LevelNone {
		t.Fatalf("revoked ACL = %d", l)
	}
}

func TestDrafts(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	alice, _ := s.CreateUser(ctx, "alice", "a@sgit.local", false)
	bob, _ := s.CreateUser(ctx, "bob", "b@sgit.local", false)
	now := time.Unix(1700000000, 0).UTC()
	d := Draft{ID: "aaaaaaaa", Repo: "web", OwnerID: alice.ID, Title: "t", Base: "b", Head: "h", Version: 1, Status: DraftOpen, CreatedAt: now, UpdatedAt: now}
	if err := s.InsertDraft(ctx, d); err != nil {
		t.Fatal(err)
	}
	_ = s.InsertDraft(ctx, Draft{ID: "bbbbbbbb", Repo: "web", OwnerID: bob.ID, Title: "u", Base: "b", Head: "h", Version: 1, Status: DraftOpen, CreatedAt: now, UpdatedAt: now.Add(time.Second)})
	got, err := s.GetDraft(ctx, "web", "aaaaaaaa")
	if err != nil || got.OwnerName != "alice" || got.Head != "h" || !got.CreatedAt.Equal(now) {
		t.Fatalf("GetDraft = %+v, %v", got, err)
	}
	if _, err := s.GetDraft(ctx, "other", "aaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong repo: want ErrNotFound, got %v", err)
	}
	got.Status, got.LandedAs, got.Version = DraftLanded, "c", 2
	if err := s.UpdateDraft(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDraft(ctx, "web", "aaaaaaaa")
	if got.Status != DraftLanded || got.LandedAs != "c" || got.Version != 2 {
		t.Fatalf("after update = %+v", got)
	}
	all, _ := s.ListDrafts(ctx, "web", 0, "")
	mine, _ := s.ListDrafts(ctx, "web", bob.ID, "")
	openDrafts, _ := s.ListDrafts(ctx, "web", 0, DraftOpen)
	if len(all) != 2 || all[0].ID != "bbbbbbbb" || len(mine) != 1 || mine[0].ID != "bbbbbbbb" || len(openDrafts) != 1 {
		t.Fatalf("list: all=%v mine=%v open=%v", all, mine, openDrafts)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go get modernc.org/sqlite@latest && go test ./internal/store/`
Expected: FAIL to compile (`undefined: Open`).

- [ ] **Step 3: Implement**

`internal/store/store.go`:
```go
// Package store persists users, tokens, ACLs, repos and drafts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
)

// Level is a per-repo access level. Write implies read.
type Level int

const (
	LevelNone  Level = 0
	LevelRead  Level = 1
	LevelWrite Level = 2
)

const (
	DraftOpen      = "open"
	DraftLanded    = "landed"
	DraftAbandoned = "abandoned"
)

type User struct {
	ID    int64
	Name  string
	Email string
	Admin bool
}

type RepoRow struct {
	Name      string
	MirrorURL string
	CreatedAt time.Time
}

type Draft struct {
	ID        string
	Repo      string
	OwnerID   int64
	OwnerName string // filled on reads
	Title     string
	Base      string
	Head      string
	Version   int
	Status    string
	LandedAs  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  email TEXT NOT NULL,
  admin INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
  id TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  hash BLOB UNIQUE NOT NULL,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS repos (
  name TEXT PRIMARY KEY,
  mirror_url TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS acl (
  user_id INTEGER NOT NULL REFERENCES users(id),
  repo TEXT NOT NULL,
  level INTEGER NOT NULL,
  PRIMARY KEY (user_id, repo)
);
CREATE TABLE IF NOT EXISTS drafts (
  id TEXT PRIMARY KEY,
  repo TEXT NOT NULL,
  owner_id INTEGER NOT NULL REFERENCES users(id),
  title TEXT NOT NULL,
  base TEXT NOT NULL,
  head TEXT NOT NULL,
  version INTEGER NOT NULL,
  status TEXT NOT NULL,
  landed_as TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS drafts_repo_status ON drafts(repo, status);
`

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serializing avoids SQLITE_BUSY on upgrades.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func isUnique(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY"))
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) CreateUser(ctx context.Context, name, email string, admin bool) (User, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users(name,email,admin,created_at) VALUES(?,?,?,?)`, name, email, admin, time.Now().Unix())
	if isUnique(err) {
		return User{}, fmt.Errorf("%w: user %q", ErrExists, name)
	}
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	return User{ID: id, Name: name, Email: email, Admin: admin}, err
}

func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id,name,email,admin FROM users WHERE name=?`, name).Scan(&u.ID, &u.Name, &u.Email, &u.Admin)
	return u, notFound(err)
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateToken(ctx context.Context, userID int64, id string, hash []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens(id,user_id,hash,created_at) VALUES(?,?,?,?)`, id, userID, hash, time.Now().Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: token %q", ErrExists, id)
	}
	return err
}

func (s *Store) UserByTokenHash(ctx context.Context, hash []byte) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.name,u.email,u.admin FROM tokens t JOIN users u ON u.id=t.user_id
		WHERE t.hash=? AND t.revoked_at IS NULL`, hash).Scan(&u.ID, &u.Name, &u.Email, &u.Admin)
	return u, notFound(err)
}

func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: token %q", ErrNotFound, id)
	}
	return nil
}

func (s *Store) SetACL(ctx context.Context, userID int64, repo string, l Level) error {
	if l == LevelNone {
		_, err := s.db.ExecContext(ctx, `DELETE FROM acl WHERE user_id=? AND repo=?`, userID, repo)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO acl(user_id,repo,level) VALUES(?,?,?)
		ON CONFLICT(user_id,repo) DO UPDATE SET level=excluded.level`, userID, repo, int(l))
	return err
}

func (s *Store) ACL(ctx context.Context, userID int64, repo string) (Level, error) {
	var l int
	err := s.db.QueryRowContext(ctx, `SELECT level FROM acl WHERE user_id=? AND repo=?`, userID, repo).Scan(&l)
	if errors.Is(err, sql.ErrNoRows) {
		return LevelNone, nil
	}
	return Level(l), err
}

func (s *Store) CreateRepo(ctx context.Context, name, mirrorURL string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repos(name,mirror_url,created_at) VALUES(?,?,?)`, name, mirrorURL, time.Now().Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: repo %q", ErrExists, name)
	}
	return err
}

func (s *Store) GetRepo(ctx context.Context, name string) (RepoRow, error) {
	var r RepoRow
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT name,mirror_url,created_at FROM repos WHERE name=?`, name).Scan(&r.Name, &r.MirrorURL, &created)
	r.CreatedAt = time.Unix(created, 0).UTC()
	return r, notFound(err)
}

func (s *Store) ListRepos(ctx context.Context) ([]RepoRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,mirror_url,created_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoRow
	for rows.Next() {
		var r RepoRow
		var created int64
		if err := rows.Scan(&r.Name, &r.MirrorURL, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InsertDraft(ctx context.Context, d Draft) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO drafts(id,repo,owner_id,title,base,head,version,status,landed_as,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, d.ID, d.Repo, d.OwnerID, d.Title, d.Base, d.Head, d.Version, d.Status, d.LandedAs, d.CreatedAt.Unix(), d.UpdatedAt.Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: draft %q", ErrExists, d.ID)
	}
	return err
}

const draftCols = `d.id,d.repo,d.owner_id,u.name,d.title,d.base,d.head,d.version,d.status,d.landed_as,d.created_at,d.updated_at`

type scanner interface{ Scan(...any) error }

func scanDraft(sc scanner) (Draft, error) {
	var d Draft
	var created, updated int64
	err := sc.Scan(&d.ID, &d.Repo, &d.OwnerID, &d.OwnerName, &d.Title, &d.Base, &d.Head, &d.Version, &d.Status, &d.LandedAs, &created, &updated)
	d.CreatedAt, d.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return d, err
}

func (s *Store) GetDraft(ctx context.Context, repo, id string) (Draft, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+draftCols+` FROM drafts d JOIN users u ON u.id=d.owner_id WHERE d.repo=? AND d.id=?`, repo, id)
	d, err := scanDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, fmt.Errorf("%w: draft %q in repo %q", ErrNotFound, id, repo)
	}
	return d, err
}

func (s *Store) UpdateDraft(ctx context.Context, d Draft) error {
	res, err := s.db.ExecContext(ctx, `UPDATE drafts SET title=?,base=?,head=?,version=?,status=?,landed_as=?,updated_at=? WHERE repo=? AND id=?`,
		d.Title, d.Base, d.Head, d.Version, d.Status, d.LandedAs, d.UpdatedAt.Unix(), d.Repo, d.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: draft %q", ErrNotFound, d.ID)
	}
	return nil
}

func (s *Store) ListDrafts(ctx context.Context, repo string, ownerID int64, status string) ([]Draft, error) {
	q := `SELECT ` + draftCols + ` FROM drafts d JOIN users u ON u.id=d.owner_id WHERE d.repo=?`
	args := []any{repo}
	if ownerID != 0 {
		q += ` AND d.owner_id=?`
		args = append(args, ownerID)
	}
	if status != "" {
		q += ` AND d.status=?`
		args = append(args, status)
	}
	q += ` ORDER BY d.updated_at DESC, d.id LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race ./internal/store/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./...
node "$PUSH" . sgit "feat(store): sqlite users, tokens, acl, repos, drafts"
```

---

### Task 6: Auth (tokens, Authorize, interceptors)

**Files:**
- Create: `internal/auth/auth.go`
- Test: `internal/auth/auth_test.go`

**Interfaces:**
- Consumes: `store.User`, `store.Level*`, `store.ErrNotFound` (Task 5).
- Produces:
  - Tokens: `auth.TokenPrefix = "sgt_"`, `auth.NewToken() (id, token string, hash []byte, err error)`, `auth.Hash(token string) []byte`
  - Actions: `auth.Action` (`ActionRead`, `ActionWrite`, `ActionAdmin`) with `String()`
  - Interfaces: `auth.ACLReader` (`ACL(ctx, userID int64, repo string) (store.Level, error)`), `auth.TokenLookup` (`UserByTokenHash(ctx, hash []byte) (store.User, error)`)
  - `auth.Authorize(ctx, acl ACLReader, u store.User, repo string, a Action) error` returns gRPC `PermissionDenied`
  - Interceptors: `auth.UnaryInterceptor(TokenLookup) grpc.UnaryServerInterceptor`, `auth.StreamInterceptor(TokenLookup) grpc.StreamServerInterceptor`. Both skip `/grpc.health.v1.Health/*`.
  - Context: `auth.UserFrom(ctx) (store.User, bool)`, `auth.WithUser(ctx, u) context.Context`, `auth.RequestID(ctx) string`

- [ ] **Step 1: Write the failing tests**

`internal/auth/auth_test.go`:
```go
package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeACL map[string]store.Level

func (f fakeACL) ACL(_ context.Context, _ int64, repo string) (store.Level, error) { return f[repo], nil }

type fakeTokens map[string]store.User

func (f fakeTokens) UserByTokenHash(_ context.Context, hash []byte) (store.User, error) {
	u, ok := f[string(hash)]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func TestNewToken(t *testing.T) {
	id, tok, hash, err := NewToken()
	if err != nil || len(id) != 8 || !strings.HasPrefix(tok, TokenPrefix) || len(tok) < 50 || string(hash) != string(Hash(tok)) || len(hash) != 32 {
		t.Fatalf("NewToken = %q %q %x %v", id, tok, hash, err)
	}
	_, tok2, _, _ := NewToken()
	if tok == tok2 {
		t.Fatal("tokens not random")
	}
}

func TestAuthorize(t *testing.T) {
	ctx := context.Background()
	acl := fakeACL{"r": store.LevelRead, "w": store.LevelWrite}
	user := store.User{ID: 1, Name: "bob"}
	admin := store.User{ID: 2, Name: "admin", Admin: true}
	cases := []struct {
		u    store.User
		repo string
		a    Action
		ok   bool
	}{
		{user, "r", ActionRead, true},
		{user, "r", ActionWrite, false},
		{user, "w", ActionWrite, true},
		{user, "w", ActionRead, true},
		{user, "none", ActionRead, false},
		{user, "", ActionAdmin, false},
		{admin, "none", ActionWrite, true},
		{admin, "", ActionAdmin, true},
	}
	for _, c := range cases {
		err := Authorize(ctx, acl, c.u, c.repo, c.a)
		if c.ok != (err == nil) || (err != nil && status.Code(err) != codes.PermissionDenied) {
			t.Errorf("Authorize(%s,%s,%s) = %v", c.u.Name, c.repo, c.a, err)
		}
	}
}

func TestUnaryInterceptor(t *testing.T) {
	_, tok, hash, _ := NewToken()
	lk := fakeTokens{string(hash): {ID: 7, Name: "alice"}}
	icpt := UnaryInterceptor(lk)
	var seen store.User
	var reqID string
	handler := func(ctx context.Context, _ any) (any, error) {
		seen, _ = UserFrom(ctx)
		reqID = RequestID(ctx)
		return "ok", nil
	}
	call := func(md metadata.MD, method string) error {
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := icpt(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		return err
	}
	if err := call(metadata.Pairs("authorization", "Bearer "+tok), "/sgit.v1.Repo/ResolveRev"); err != nil || seen.Name != "alice" || len(reqID) != 12 {
		t.Fatalf("valid token: err=%v user=%+v reqID=%q", err, seen, reqID)
	}
	for name, md := range map[string]metadata.MD{
		"missing":   metadata.MD{},
		"malformed": metadata.Pairs("authorization", "Basic xyz"),
		"unknown":   metadata.Pairs("authorization", "Bearer sgt_doesnotexist"),
	} {
		if err := call(md, "/sgit.v1.Repo/ResolveRev"); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: want Unauthenticated, got %v", name, err)
		}
	}
	if err := call(metadata.MD{}, "/grpc.health.v1.Health/Check"); err != nil {
		t.Fatalf("health must skip auth: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go get google.golang.org/grpc@latest && go test ./internal/auth/`
Expected: FAIL to compile (`undefined: NewToken`).

- [ ] **Step 3: Implement**

`internal/auth/auth.go`:
```go
// Package auth implements API tokens, the single Authorize decision point and
// the gRPC interceptors that attach the calling user to the context.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const TokenPrefix = "sgt_"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func randB32(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(b32.EncodeToString(b)), nil
}

// NewToken returns a public token id, the secret token, and the hash to store.
func NewToken() (id, token string, hash []byte, err error) {
	if id, err = randB32(5); err != nil {
		return
	}
	var secret string
	if secret, err = randB32(32); err != nil {
		return
	}
	token = TokenPrefix + secret
	return id, token, Hash(token), nil
}

// Hash is the value stored in the database for a token.
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

type Action int

const (
	ActionRead Action = iota + 1
	ActionWrite
	ActionAdmin
)

func (a Action) String() string {
	switch a {
	case ActionRead:
		return "read"
	case ActionWrite:
		return "write"
	case ActionAdmin:
		return "admin"
	}
	return "unknown"
}

type ACLReader interface {
	ACL(ctx context.Context, userID int64, repo string) (store.Level, error)
}

type TokenLookup interface {
	UserByTokenHash(ctx context.Context, hash []byte) (store.User, error)
}

// Authorize is the single permission decision point. Per-directory ACLs
// would be added here.
func Authorize(ctx context.Context, acl ACLReader, u store.User, repo string, a Action) error {
	if u.Admin {
		return nil
	}
	if a == ActionAdmin {
		return status.Error(codes.PermissionDenied, "admin role required")
	}
	lvl, err := acl.ACL(ctx, u.ID, repo)
	if err != nil {
		return status.Errorf(codes.Internal, "acl lookup: %v", err)
	}
	need := store.LevelRead
	if a == ActionWrite {
		need = store.LevelWrite
	}
	if lvl < need {
		return status.Errorf(codes.PermissionDenied, "%s access to repo %q denied", a, repo)
	}
	return nil
}

type ctxKey int

const (
	userKey ctxKey = iota
	reqIDKey
)

func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// RequestID returns the per-call id used to correlate client errors with logs.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey).(string)
	return id
}

func withRequestID(ctx context.Context) context.Context {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return context.WithValue(ctx, reqIDKey, hex.EncodeToString(b))
}

const healthPrefix = "/grpc.health.v1.Health/"

func authenticate(ctx context.Context, lk TokenLookup) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing token")
	}
	tok, ok := strings.CutPrefix(vals[0], "Bearer ")
	if !ok || !strings.HasPrefix(tok, TokenPrefix) {
		return nil, status.Error(codes.Unauthenticated, "malformed authorization header")
	}
	u, err := lk.UserByTokenHash(ctx, Hash(tok))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.Unauthenticated, "invalid or revoked token")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "token lookup: %v", err)
	}
	return WithUser(ctx, u), nil
}

func UnaryInterceptor(lk TokenLookup) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = withRequestID(ctx)
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(ctx, req)
		}
		ctx, err := authenticate(ctx, lk)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func StreamInterceptor(lk TokenLookup) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := withRequestID(ss.Context())
		if !strings.HasPrefix(info.FullMethod, healthPrefix) {
			var err error
			if ctx, err = authenticate(ctx, lk); err != nil {
				return err
			}
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race ./internal/auth/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./...
node "$PUSH" . sgit "feat(auth): tokens, Authorize, gRPC interceptors with request ids"
```

---

### Task 7: Proto, generated code, rpc helpers

**Files:**
- Create: `proto/sgit/v1/sgit.proto`, `gen/sgit/v1/sgit.pb.go` and `gen/sgit/v1/sgit_grpc.pb.go` (generated), `internal/rpc/rpc.go`
- Test: `internal/rpc/rpc_test.go`

**Interfaces:**
- Produces:
  - All `sgitv1.*` message and service types below.
  - `rpc.MaxMsgSize` (256 MiB) and `rpc.ChunkSize` (1 MiB)
  - `rpc.ServerOptions() []grpc.ServerOption`
  - `rpc.DialOptions(token string, tlsCfg *tls.Config) []grpc.DialOption`, where `tlsCfg == nil` means plaintext
  - `rpc.TokenCreds{Token string; Secure bool}`, which implements `credentials.PerRPCCredentials`
  - `rpc.UploadBlobs(ctx, c sgitv1.RepoClient, repo string, blobs [][]byte) ([]string, error)`

- [ ] **Step 1: Write the proto**

`proto/sgit/v1/sgit.proto`:
```proto
syntax = "proto3";

package sgit.v1;

option go_package = "github.com/hermes98761234/sgit/gen/sgit/v1;sgitv1";

// Repo is served by both sg-server and sgd (phase 3).
service Repo {
  rpc ResolveRev(ResolveRevRequest) returns (ResolveRevResponse);
  rpc GetObjects(GetObjectsRequest) returns (stream Object);
  rpc HasObjects(HasObjectsRequest) returns (HasObjectsResponse);
  rpc UploadBlobs(stream BlobChunk) returns (UploadBlobsResponse);
  rpc CreateDraft(CreateDraftRequest) returns (Draft);
  rpc UpdateDraft(UpdateDraftRequest) returns (Draft);
  rpc GetDraft(GetDraftRequest) returns (Draft);
  rpc ListDrafts(ListDraftsRequest) returns (ListDraftsResponse);
  rpc AbandonDraft(AbandonDraftRequest) returns (Draft);
  rpc Land(LandRequest) returns (LandResponse);
  rpc CommitAndLand(CommitAndLandRequest) returns (LandResponse);
  rpc Log(LogRequest) returns (LogResponse);
  rpc Blame(BlameRequest) returns (BlameResponse);
  rpc Diff(DiffRequest) returns (DiffResponse);
  rpc MergeFile(MergeFileRequest) returns (MergeFileResponse);
}

// Admin is only served by sg-server; clients dial it directly.
service Admin {
  rpc WhoAmI(WhoAmIRequest) returns (WhoAmIResponse);
  rpc CreateRepo(CreateRepoRequest) returns (CreateRepoResponse);
  rpc CreateToken(CreateTokenRequest) returns (CreateTokenResponse);
  rpc RevokeToken(RevokeTokenRequest) returns (RevokeTokenResponse);
  rpc GrantACL(GrantACLRequest) returns (GrantACLResponse);
  rpc RevokeACL(RevokeACLRequest) returns (RevokeACLResponse);
}

message ResolveRevRequest { string repo = 1; string rev = 2; }
message ResolveRevResponse { string sha = 1; }

message GetObjectsRequest { string repo = 1; repeated string shas = 2; }
message Object {
  string sha = 1;
  string type = 2;      // blob | tree | commit
  uint64 size = 3;      // uncompressed size
  bytes zstd_data = 4;  // zstd-compressed raw object content
}
message HasObjectsRequest { string repo = 1; repeated string shas = 2; }
message HasObjectsResponse { repeated bool present = 1; }

// BlobChunk streams blobs in <= 1 MiB chunks; repo is required on the first chunk.
message BlobChunk { string repo = 1; bytes data = 2; bool end_of_blob = 3; }
message UploadBlobsResponse { repeated string shas = 1; }

enum ChangeOp {
  CHANGE_OP_UNSPECIFIED = 0;
  CHANGE_OP_ADD = 1;
  CHANGE_OP_MODIFY = 2;
  CHANGE_OP_DELETE = 3;
}
message Change { string path = 1; ChangeOp op = 2; string blob_sha = 3; string mode = 4; }

message CreateDraftRequest { string repo = 1; string base = 2; repeated Change changes = 3; string title = 4; string message = 5; }
message UpdateDraftRequest { string repo = 1; string id = 2; string base = 3; repeated Change changes = 4; string message = 5; }
message GetDraftRequest { string repo = 1; string id = 2; }
message ListDraftsRequest { string repo = 1; bool mine = 2; string status = 3; }
message ListDraftsResponse { repeated Draft drafts = 1; }
message AbandonDraftRequest { string repo = 1; string id = 2; }
message Draft {
  string id = 1;
  string repo = 2;
  string owner = 3;
  string title = 4;
  string base = 5;
  string head = 6;
  int32 version = 7;
  string status = 8;
  string landed_as = 9;
  int64 created_at = 10;
  int64 updated_at = 11;
}

message LandRequest { string repo = 1; string id = 2; }
message CommitAndLandRequest { string repo = 1; string draft_id = 2; string base = 3; repeated Change changes = 4; string title = 5; string message = 6; }
message LandResponse { string commit = 1; string draft_id = 2; }

// Attached as a status detail to ABORTED errors from Land/CommitAndLand.
// "ours" is main, "theirs" is the draft.
message ConflictFile { string path = 1; string base_sha = 2; string ours_sha = 3; string theirs_sha = 4; }
message ConflictDetail { string draft_id = 1; string main = 2; repeated ConflictFile files = 3; }

message LogRequest { string repo = 1; string rev = 2; string path = 3; int32 n = 4; }
message LogEntry { string sha = 1; string author_name = 2; string author_email = 3; int64 time = 4; string subject = 5; }
message LogResponse { repeated LogEntry entries = 1; }

message BlameRequest { string repo = 1; string rev = 2; string path = 3; }
message BlameLine { string sha = 1; string author = 2; int32 line = 3; bytes text = 4; }
message BlameResponse { repeated BlameLine lines = 1; }

message DiffRequest { string repo = 1; string a = 2; string b = 3; repeated string paths = 4; }
message DiffResponse { bytes patch = 1; }

message MergeFileRequest { string repo = 1; string base_sha = 2; string ours_sha = 3; string theirs_sha = 4; string ours_label = 5; string theirs_label = 6; }
message MergeFileResponse { bytes content = 1; bool conflicted = 2; }

message WhoAmIRequest {}
message WhoAmIResponse { string user = 1; string email = 2; bool admin = 3; }
message CreateRepoRequest { string name = 1; }
message CreateRepoResponse { string main = 1; }
message CreateTokenRequest { string user = 1; string email = 2; bool admin = 3; }
message CreateTokenResponse { string id = 1; string token = 2; string user = 3; }
message RevokeTokenRequest { string id = 1; }
message RevokeTokenResponse {}
message GrantACLRequest { string user = 1; string repo = 2; string level = 3; }
message GrantACLResponse {}
message RevokeACLRequest { string user = 1; string repo = 2; }
message RevokeACLResponse {}
```

- [ ] **Step 2: Generate**

Run: `go get google.golang.org/protobuf@latest && make gen && go build ./gen/...`
Expected: `gen/sgit/v1/sgit.pb.go` and `gen/sgit/v1/sgit_grpc.pb.go` exist, and the build is OK.

- [ ] **Step 3: Write the failing rpc test**

`internal/rpc/rpc_test.go`:
```go
package rpc

import (
	"context"
	"testing"
)

func TestTokenCreds(t *testing.T) {
	md, err := TokenCreds{Token: "sgt_x", Secure: true}.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer sgt_x" {
		t.Fatalf("md = %v, %v", md, err)
	}
	if !(TokenCreds{Secure: true}).RequireTransportSecurity() || (TokenCreds{}).RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity mismatch")
	}
}

func TestChunks(t *testing.T) {
	data := make([]byte, 2*ChunkSize+5)
	got := chunks(data)
	if len(got) != 3 || len(got[0]) != ChunkSize || len(got[2]) != 5 {
		t.Fatalf("chunks lens = %d", len(got))
	}
	if got := chunks(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("empty blob must yield one empty chunk, got %d", len(got))
	}
}
```

Run: `go test ./internal/rpc/`
Expected: FAIL to compile (`undefined: TokenCreds`).

- [ ] **Step 4: Implement**

`internal/rpc/rpc.go`:
```go
// Package rpc holds gRPC options and helpers shared by server, daemon and CLI.
package rpc

import (
	"context"
	"crypto/tls"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	MaxMsgSize = 256 << 20
	ChunkSize  = 1 << 20
)

func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(MaxMsgSize), grpc.MaxSendMsgSize(MaxMsgSize)}
}

// TokenCreds sends "authorization: Bearer <token>" on every call.
type TokenCreds struct {
	Token  string
	Secure bool
}

func (c TokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.Token}, nil
}

func (c TokenCreds) RequireTransportSecurity() bool { return c.Secure }

// DialOptions builds client options. tlsCfg == nil dials plaintext (tests, unix socket).
func DialOptions(token string, tlsCfg *tls.Config) []grpc.DialOption {
	opts := []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMsgSize), grpc.MaxCallSendMsgSize(MaxMsgSize))}
	if tlsCfg != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(TokenCreds{Token: token, Secure: tlsCfg != nil}))
	}
	return opts
}

func chunks(data []byte) [][]byte {
	if len(data) == 0 {
		return [][]byte{nil}
	}
	var out [][]byte
	for len(data) > ChunkSize {
		out = append(out, data[:ChunkSize])
		data = data[ChunkSize:]
	}
	return append(out, data)
}

// UploadBlobs streams blobs to the server and returns their SHAs in order.
func UploadBlobs(ctx context.Context, c sgitv1.RepoClient, repo string, blobs [][]byte) ([]string, error) {
	stream, err := c.UploadBlobs(ctx)
	if err != nil {
		return nil, err
	}
	first := true
	for _, b := range blobs {
		parts := chunks(b)
		for i, part := range parts {
			msg := &sgitv1.BlobChunk{Data: part, EndOfBlob: i == len(parts)-1}
			if first {
				msg.Repo, first = repo, false
			}
			if err := stream.Send(msg); err != nil {
				_, rerr := stream.CloseAndRecv() // surface the server's status
				if rerr != nil {
					return nil, rerr
				}
				return nil, err
			}
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	return resp.Shas, nil
}
```

- [ ] **Step 5: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race ./internal/rpc/ && go vet ./...`
Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
gofmt -l . ; go vet ./...
node "$PUSH" . sgit "feat(proto): sgit.v1 Repo/Admin services, generated code, rpc helpers"
```

---

### Task 8: Server skeleton, Admin service, test harness

**Files:**
- Create: `internal/server/server.go`, `internal/server/repos.go`, `internal/server/admin.go`, `internal/testenv/testenv.go`
- Test: `internal/server/admin_test.go`

**Interfaces:**
- Consumes: gitx (Tasks 2–4), store (5), auth (6), sgitv1 and rpc (7).
- Produces:
  - Config: `server.Config{DataDir string; MaxBlobBytes int64; MaxChanges, LandRetries int; Now func() time.Time; BeforeCAS func(repo string)}`. `BeforeCAS` is a test hook that runs just before the CAS on main.
  - Server: `server.New(cfg Config, st *store.Store, log *slog.Logger) (*Server, error)`, `(*Server).Register(g *grpc.Server)` (Repo, Admin, health), `(*Server).Close()`
  - `server.NewGRPC(st *store.Store, extra ...grpc.ServerOption) *grpc.Server` (rpc options + auth interceptors)
  - `server.ValidRepoName(string) bool`
  - Internal: `(*Server).toStatus(ctx, err) error`, `(*Server).sig(u store.User) gitx.Signature`, `repoManager.get(ctx, name) (*repoHandle, error)`, and `repoHandle{name string; git *gitx.Repo; landMu sync.Mutex}`
  - `testenv.Start(t testing.TB, opts ...func(*server.Config)) *testenv.Env`. `Env` has fields `Addr`, `AdminToken`, `DataDir`, `Store`, `Server` and methods:
    - clients: `Conn(token) *grpc.ClientConn`, `Repo(token) sgitv1.RepoClient`, `Admin(token) sgitv1.AdminClient`
    - setup: `CreateRepo(name) string` (returns the main SHA), `NewUser(name, repo, level string) string` (returns a token; empty level means no ACL), `Upload(token, repo string, blobs ...[]byte) []string`
    - direct git access: `Git(repo) *gitx.Repo`, `Files(repo, rev) map[string]string` (path → content), `CommitDirect(repo string, files map[string]string, msg string) string` (moves main directly via gitx)
  - Change helpers: `testenv.Add(path, sha) *sgitv1.Change`, `testenv.Modify(path, sha)`, `testenv.Delete(path)`
  - `testenv.Must[T any](t testing.TB, v T, err error) T`

- [ ] **Step 1: Write the failing tests**

`internal/server/admin_test.go`:
```go
package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestWhoAmIAndHealth(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	who := testenv.Must(t, e.Admin(e.AdminToken).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
	if who.User != "admin" || !who.Admin {
		t.Fatalf("WhoAmI = %+v", who)
	}
	h := testenv.Must(t, healthpb.NewHealthClient(e.Conn("")).Check(ctx, &healthpb.HealthCheckRequest{}))
	if h.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v", h.Status)
	}
	if _, err := e.Admin("").WhoAmI(ctx, &sgitv1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: want Unauthenticated, got %v", err)
	}
	if _, err := e.Admin("sgt_bogus").WhoAmI(ctx, &sgitv1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: want Unauthenticated, got %v", err)
	}
}

func TestCreateRepo(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	main := e.CreateRepo("web")
	if len(main) != 40 {
		t.Fatalf("main = %q", main)
	}
	if len(e.Files("web", "main")) != 0 {
		t.Fatal("new repo must be empty")
	}
	adm := e.Admin(e.AdminToken)
	if _, err := adm.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "web"}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("dup: want AlreadyExists, got %v", err)
	}
	for _, bad := range []string{"", "Web", "a/b", "x.git", "a..b", "-x"} {
		if _, err := adm.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateRepo(%q): want InvalidArgument, got %v", bad, err)
		}
	}
	bob := e.NewUser("bob", "", "")
	if _, err := e.Admin(bob).CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "other"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestTokensAndACL(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	e.CreateRepo("web")
	adm := e.Admin(e.AdminToken)
	tok := testenv.Must(t, adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "carol"}))
	if tok.User != "carol" || len(tok.Id) != 8 {
		t.Fatalf("CreateToken = %+v", tok)
	}
	who := testenv.Must(t, e.Admin(tok.Token).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
	if who.User != "carol" || who.Admin || who.Email != "carol@sgit.local" {
		t.Fatalf("WhoAmI = %+v", who)
	}
	if _, err := adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "carol", Repo: "web", Level: "admin"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad level: want InvalidArgument, got %v", err)
	}
	if _, err := adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "carol", Repo: "nope", Level: "read"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown repo: want NotFound, got %v", err)
	}
	if _, err := adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "nobody", Repo: "web", Level: "read"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown user: want NotFound, got %v", err)
	}
	testenv.Must(t, adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "carol", Repo: "web", Level: "read"}))
	testenv.Must(t, e.Repo(tok.Token).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web", Rev: "main"}))
	testenv.Must(t, adm.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "carol", Repo: "web"}))
	if _, err := e.Repo(tok.Token).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web", Rev: "main"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("after revoke ACL: want PermissionDenied, got %v", err)
	}
	testenv.Must(t, adm.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: tok.Id}))
	if _, err := e.Admin(tok.Token).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked token: want Unauthenticated, got %v", err)
	}
	if _, err := adm.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: tok.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("revoke twice: want NotFound, got %v", err)
	}
}
```

`TestTokensAndACL` uses `ResolveRev`, which this task implements in `read.go` as part of the skeleton. Task 9 adds the other read RPCs.

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/server/`
Expected: FAIL to compile (`undefined: testenv.Start`).

- [ ] **Step 3: Implement**

`internal/server/server.go`:
```go
// Package server implements the sgit.v1 Repo and Admin gRPC services over
// bare git repositories.
package server

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/store"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type Config struct {
	DataDir      string
	MaxBlobBytes int64            // default 100 MiB
	MaxChanges   int              // default 10000
	LandRetries  int              // default 5
	Now          func() time.Time // default time.Now
	// BeforeCAS is a test hook called right before main is compare-and-swapped.
	BeforeCAS func(repo string)
}

func (c *Config) defaults() {
	if c.MaxBlobBytes == 0 {
		c.MaxBlobBytes = 100 << 20
	}
	if c.MaxChanges == 0 {
		c.MaxChanges = 10000
	}
	if c.LandRetries == 0 {
		c.LandRetries = 5
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

const maxBatch = 10000

type Server struct {
	cfg   Config
	store *store.Store
	log   *slog.Logger
	repos *repoManager
	zenc  *zstd.Encoder
}

func New(cfg Config, st *store.Store, log *slog.Logger) (*Server, error) {
	cfg.defaults()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:   cfg,
		store: st,
		log:   log,
		repos: newRepoManager(filepath.Join(cfg.DataDir, "repos"), st),
		zenc:  enc,
	}, nil
}

// NewGRPC returns a gRPC server with sgit's message limits and auth interceptors.
func NewGRPC(st *store.Store, extra ...grpc.ServerOption) *grpc.Server {
	opts := append(rpc.ServerOptions(),
		grpc.ChainUnaryInterceptor(auth.UnaryInterceptor(st)),
		grpc.ChainStreamInterceptor(auth.StreamInterceptor(st)),
	)
	return grpc.NewServer(append(opts, extra...)...)
}

func (s *Server) Register(g *grpc.Server) {
	sgitv1.RegisterRepoServer(g, &repoService{s: s})
	sgitv1.RegisterAdminServer(g, &adminService{s: s})
	healthpb.RegisterHealthServer(g, health.NewServer())
}

func (s *Server) Close() {
	s.repos.closeAll()
	_ = s.zenc.Close()
}

func (s *Server) sig(u store.User) gitx.Signature {
	return gitx.Signature{Name: u.Name, Email: u.Email, When: s.cfg.Now()}
}

// toStatus maps internal errors to gRPC status errors. Unknown errors are
// logged with the request id and returned as Internal without details.
func (s *Server) toStatus(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, gitx.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, gitx.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, store.ErrExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	id := auth.RequestID(ctx)
	s.log.ErrorContext(ctx, "internal error", "request_id", id, "err", err)
	return status.Errorf(codes.Internal, "internal error (request %s)", id)
}

// open authenticates, authorizes and opens the repo for a Repo RPC.
func (s *Server) open(ctx context.Context, repo string, a auth.Action) (*repoHandle, store.User, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, u, status.Error(codes.Unauthenticated, "no user")
	}
	if err := auth.Authorize(ctx, s.store, u, repo, a); err != nil {
		return nil, u, err
	}
	h, err := s.repos.get(ctx, repo)
	if err != nil {
		return nil, u, s.toStatus(ctx, err)
	}
	return h, u, nil
}
```

`internal/server/repos.go`:
```go
package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var repoNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidRepoName reports whether name is an acceptable repo name.
func ValidRepoName(name string) bool {
	return repoNameRe.MatchString(name) && !strings.HasSuffix(name, ".git") && !strings.Contains(name, "..")
}

type repoHandle struct {
	name   string
	git    *gitx.Repo
	landMu sync.Mutex // serializes lands per repo; correctness comes from CAS
}

type repoManager struct {
	root string
	st   *store.Store
	mu   sync.Mutex
	open map[string]*repoHandle
}

func newRepoManager(root string, st *store.Store) *repoManager {
	return &repoManager{root: root, st: st, open: map[string]*repoHandle{}}
}

func (m *repoManager) path(name string) string { return filepath.Join(m.root, name+".git") }

func (m *repoManager) get(ctx context.Context, name string) (*repoHandle, error) {
	if !ValidRepoName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repo name %q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.open[name]; ok {
		return h, nil
	}
	if _, err := m.st.GetRepo(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "repo %q not found", name)
		}
		return nil, err
	}
	h := &repoHandle{name: name, git: gitx.Open(m.path(name))}
	m.open[name] = h
	return h, nil
}

func (m *repoManager) create(ctx context.Context, name string, sig gitx.Signature) (string, error) {
	if !ValidRepoName(name) {
		return "", status.Errorf(codes.InvalidArgument, "invalid repo name %q", name)
	}
	if _, err := m.st.GetRepo(ctx, name); err == nil {
		return "", status.Errorf(codes.AlreadyExists, "repo %q exists", name)
	}
	p := m.path(name)
	if _, err := os.Stat(p); err == nil {
		return "", status.Errorf(codes.AlreadyExists, "repo dir for %q exists", name)
	}
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return "", err
	}
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
	if err := m.st.CreateRepo(ctx, name, ""); err != nil {
		os.RemoveAll(p)
		return "", err
	}
	return main, nil
}

func (m *repoManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.open {
		h.git.Close()
	}
	m.open = map[string]*repoHandle{}
}
```

`internal/server/admin.go`:
```go
package server

import (
	"context"
	"errors"
	"regexp"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var userNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type adminService struct {
	sgitv1.UnimplementedAdminServer
	s *Server
}

func (a *adminService) requireAdmin(ctx context.Context) (store.User, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return u, status.Error(codes.Unauthenticated, "no user")
	}
	return u, auth.Authorize(ctx, a.s.store, u, "", auth.ActionAdmin)
}

func (a *adminService) WhoAmI(ctx context.Context, _ *sgitv1.WhoAmIRequest) (*sgitv1.WhoAmIResponse, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no user")
	}
	return &sgitv1.WhoAmIResponse{User: u.Name, Email: u.Email, Admin: u.Admin}, nil
}

func (a *adminService) CreateRepo(ctx context.Context, req *sgitv1.CreateRepoRequest) (*sgitv1.CreateRepoResponse, error) {
	u, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	main, err := a.s.repos.create(ctx, req.Name, a.s.sig(u))
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.CreateRepoResponse{Main: main}, nil
}

func (a *adminService) CreateToken(ctx context.Context, req *sgitv1.CreateTokenRequest) (*sgitv1.CreateTokenResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if !userNameRe.MatchString(req.User) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user name %q", req.User)
	}
	u, err := a.s.store.UserByName(ctx, req.User)
	if errors.Is(err, store.ErrNotFound) {
		email := req.Email
		if email == "" {
			email = req.User + "@sgit.local"
		}
		// --admin only applies when the user is created.
		u, err = a.s.store.CreateUser(ctx, req.User, email, req.Admin)
	}
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	id, tok, hash, err := auth.NewToken()
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if err := a.s.store.CreateToken(ctx, u.ID, id, hash); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.CreateTokenResponse{Id: id, Token: tok, User: u.Name}, nil
}

func (a *adminService) RevokeToken(ctx context.Context, req *sgitv1.RevokeTokenRequest) (*sgitv1.RevokeTokenResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := a.s.store.RevokeToken(ctx, req.Id); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.RevokeTokenResponse{}, nil
}

func (a *adminService) setACL(ctx context.Context, user, repo string, lvl store.Level) error {
	if _, err := a.requireAdmin(ctx); err != nil {
		return err
	}
	u, err := a.s.store.UserByName(ctx, user)
	if err != nil {
		return a.s.toStatus(ctx, err)
	}
	if _, err := a.s.store.GetRepo(ctx, repo); err != nil {
		return a.s.toStatus(ctx, err)
	}
	return a.s.toStatus(ctx, a.s.store.SetACL(ctx, u.ID, repo, lvl))
}

func (a *adminService) GrantACL(ctx context.Context, req *sgitv1.GrantACLRequest) (*sgitv1.GrantACLResponse, error) {
	var lvl store.Level
	switch req.Level {
	case "read":
		lvl = store.LevelRead
	case "write":
		lvl = store.LevelWrite
	default:
		return nil, status.Errorf(codes.InvalidArgument, "level must be read or write, got %q", req.Level)
	}
	if err := a.setACL(ctx, req.User, req.Repo, lvl); err != nil {
		return nil, err
	}
	return &sgitv1.GrantACLResponse{}, nil
}

func (a *adminService) RevokeACL(ctx context.Context, req *sgitv1.RevokeACLRequest) (*sgitv1.RevokeACLResponse, error) {
	if err := a.setACL(ctx, req.User, req.Repo, store.LevelNone); err != nil {
		return nil, err
	}
	return &sgitv1.RevokeACLResponse{}, nil
}
```

`internal/server/read.go`. This starts with the service type and `ResolveRev`; Task 9 extends it.
```go
package server

import (
	"context"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
)

type repoService struct {
	sgitv1.UnimplementedRepoServer
	s *Server
}

func (r *repoService) ResolveRev(ctx context.Context, req *sgitv1.ResolveRevRequest) (*sgitv1.ResolveRevResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	rev := req.Rev
	if rev == "" {
		rev = "main"
	}
	sha, err := h.git.ResolveRev(ctx, rev)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.ResolveRevResponse{Sha: sha}, nil
}
```

`internal/testenv/testenv.go`:
```go
// Package testenv starts an in-process sg-server for tests.
package testenv

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc"
)

type Env struct {
	t          testing.TB
	Addr       string
	AdminToken string
	DataDir    string
	Store      *store.Store
	Server     *server.Server
}

// Must fails the test on err and returns v.
func Must[T any](t testing.TB, v T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return v
}

func Start(t testing.TB, opts ...func(*server.Config)) *Env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st := Must(t, store.Open(filepath.Join(dir, "sg.db")))
	admin := Must(t, st.CreateUser(ctx, "admin", "admin@sgit.local", true))
	id, tok, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateToken(ctx, admin.ID, id, hash); err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{DataDir: dir}
	for _, o := range opts {
		o(&cfg)
	}
	srv := Must(t, server.New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	g := server.NewGRPC(st)
	srv.Register(g)
	lis := Must(t, net.Listen("tcp", "127.0.0.1:0"))
	go g.Serve(lis)
	t.Cleanup(func() {
		g.Stop()
		srv.Close()
		st.Close()
	})
	return &Env{t: t, Addr: lis.Addr().String(), AdminToken: tok, DataDir: dir, Store: st, Server: srv}
}

func (e *Env) Conn(token string) *grpc.ClientConn {
	conn := Must(e.t, grpc.NewClient(e.Addr, rpc.DialOptions(token, nil)...))
	e.t.Cleanup(func() { conn.Close() })
	return conn
}

func (e *Env) Repo(token string) sgitv1.RepoClient   { return sgitv1.NewRepoClient(e.Conn(token)) }
func (e *Env) Admin(token string) sgitv1.AdminClient { return sgitv1.NewAdminClient(e.Conn(token)) }

func (e *Env) CreateRepo(name string) string {
	e.t.Helper()
	return Must(e.t, e.Admin(e.AdminToken).CreateRepo(context.Background(), &sgitv1.CreateRepoRequest{Name: name})).Main
}

// NewUser creates a user and returns its token. If level is "read" or "write",
// it is granted on repo.
func (e *Env) NewUser(name, repo, level string) string {
	e.t.Helper()
	ctx := context.Background()
	adm := e.Admin(e.AdminToken)
	tok := Must(e.t, adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: name}))
	if level != "" {
		Must(e.t, adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: name, Repo: repo, Level: level}))
	}
	return tok.Token
}

func (e *Env) Upload(token, repo string, blobs ...[]byte) []string {
	e.t.Helper()
	return Must(e.t, rpc.UploadBlobs(context.Background(), e.Repo(token), repo, blobs))
}

// Git opens the repo's bare git dir directly (caller closes).
func (e *Env) Git(repo string) *gitx.Repo {
	return gitx.Open(filepath.Join(e.DataDir, "repos", repo+".git"))
}

// Files returns path -> content of every file at rev.
func (e *Env) Files(repo, rev string) map[string]string {
	e.t.Helper()
	ctx := context.Background()
	r := e.Git(repo)
	defer r.Close()
	out := map[string]string{}
	for p, fe := range Must(e.t, r.ListFiles(ctx, rev)) {
		out[p] = string(Must(e.t, r.ReadObject(ctx, fe.SHA)).Data)
	}
	return out
}

// CommitDirect commits files on top of main through gitx, bypassing the server
// (simulates an external writer). Safe to call from a BeforeCAS hook.
func (e *Env) CommitDirect(repo string, files map[string]string, msg string) string {
	ctx := context.Background()
	r := e.Git(repo)
	defer r.Close()
	main := Must(e.t, r.ReadRef(ctx, gitx.MainRef))
	var ch []gitx.Change
	for p, content := range files {
		ch = append(ch, gitx.Change{Path: p, Op: gitx.OpAdd, BlobSHA: Must(e.t, r.WriteBlob(ctx, []byte(content)))})
	}
	tree := Must(e.t, r.BuildTree(ctx, main, ch))
	sig := gitx.Signature{Name: "direct", Email: "direct@sgit.local", When: time.Now()}
	c := Must(e.t, r.CommitTree(ctx, tree, []string{main}, msg, sig, sig))
	if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
		e.t.Fatalf("CommitDirect: %v", err)
	}
	return c
}

func Add(path, sha string) *sgitv1.Change {
	return &sgitv1.Change{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_ADD, BlobSha: sha}
}

func Modify(path, sha string) *sgitv1.Change {
	return &sgitv1.Change{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_MODIFY, BlobSha: sha}
}

func Delete(path string) *sgitv1.Change {
	return &sgitv1.Change{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_DELETE}
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go get github.com/klauspost/compress@latest && go mod tidy && go test -race ./internal/server/`
Expected: `ok` (all three tests).

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./... ; staticcheck ./...
node "$PUSH" . sgit "feat(server): skeleton, Admin service, ResolveRev, testenv harness"
```

---

### Task 9: Object and history read RPCs plus blob upload

**Files:**
- Modify: `internal/server/read.go` (append the RPCs below)
- Create: `internal/server/validate.go`
- Test: `internal/server/read_test.go`, `internal/server/validate_test.go`

**Interfaces:**
- Consumes: Task 8 (`repoService`, `Server.open`, `toStatus`, `testenv`), `rpc.UploadBlobs`, `objfmt.BlobSHA`.
- Produces:
  - RPCs: `GetObjects`, `HasObjects`, `UploadBlobs`, `Log` (default n=50, max 1000, default rev `main`), `Blame`, `Diff` and `MergeFile` (labels default to `ours`/`theirs` and `base`)
  - Validation: `validatePath(p string) error`, `validMode(m string) bool`, `(*Server).convertChanges(ctx, h *repoHandle, in []*sgitv1.Change) ([]gitx.Change, error)`

- [ ] **Step 1: Write the failing tests**

`internal/server/validate_test.go`:
```go
package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidatePath(t *testing.T) {
	good := []string{"a", "a/b.txt", "dir with space/файл.txt", ".gitignore", ".github/workflows/ci.yml", "a/.sgx"}
	bad := []string{"", "/abs", "a/", "./a", "a/../b", "..", ".", "a//b", ".git/config", "x/.GIT/y", ".sg/base", "a\nb", "a\x00b", string([]byte{0xff, 'a'})}
	for _, p := range good {
		if err := validatePath(p); err != nil {
			t.Errorf("validatePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range bad {
		if err := validatePath(p); status.Code(err) != codes.InvalidArgument {
			t.Errorf("validatePath(%q) = %v, want InvalidArgument", p, err)
		}
	}
}
```

`internal/server/read_test.go`:
```go
package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fetch(t *testing.T, c sgitv1.RepoClient, repo string, shas ...string) ([]*sgitv1.Object, error) {
	t.Helper()
	stream, err := c.GetObjects(context.Background(), &sgitv1.GetObjectsRequest{Repo: repo, Shas: shas})
	if err != nil {
		return nil, err
	}
	var out []*sgitv1.Object
	for {
		o, err := stream.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
}

func TestUploadHasGet(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("hello\n"), nil)
	if shas[0] != objfmt.BlobSHA([]byte("hello\n")) || shas[1] != objfmt.BlobSHA(nil) {
		t.Fatalf("shas = %v", shas)
	}
	missing := "1111111111111111111111111111111111111111"
	has := testenv.Must(t, c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Repo: "web", Shas: []string{shas[0], missing}}))
	if len(has.Present) != 2 || !has.Present[0] || has.Present[1] {
		t.Fatalf("HasObjects = %v", has.Present)
	}
	testenv.Must(t, c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Repo: "web"})) // empty list = ACL probe
	objs := testenv.Must(t, fetch(t, c, "web", shas[0]))
	dec, _ := zstd.NewReader(nil)
	data, err := dec.DecodeAll(objs[0].ZstdData, nil)
	if err != nil || string(data) != "hello\n" || objs[0].Type != "blob" || objs[0].Size != 6 {
		t.Fatalf("object = %+v data=%q err=%v", objs[0], data, err)
	}
	if _, err := fetch(t, c, "web", missing); status.Code(err) != codes.NotFound {
		t.Fatalf("missing object: want NotFound, got %v", err)
	}
	if _, err := fetch(t, c, "web", "zz"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad sha: want InvalidArgument, got %v", err)
	}
}

func TestLargeBlobRoundTrip(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	big := make([]byte, 10<<20)
	_, _ = rand.Read(big)
	shas := e.Upload(e.AdminToken, "web", big)
	objs := testenv.Must(t, fetch(t, e.Repo(e.AdminToken), "web", shas[0]))
	dec, _ := zstd.NewReader(nil)
	data, err := dec.DecodeAll(objs[0].ZstdData, nil)
	if err != nil || !bytes.Equal(data, big) {
		t.Fatalf("10 MiB round trip failed: err=%v len=%d", err, len(data))
	}
}

func TestBlobSizeLimit(t *testing.T) {
	e := testenv.Start(t, func(c *server.Config) { c.MaxBlobBytes = 1 << 20 })
	e.CreateRepo("web")
	_, err := rpc.UploadBlobs(context.Background(), e.Repo(e.AdminToken), "web", [][]byte{make([]byte, 2<<20)})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized blob: want InvalidArgument, got %v", err)
	}
}

func TestReadPermissions(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	ctx := context.Background()
	reader := e.NewUser("rita", "web", "read")
	stranger := e.NewUser("sam", "", "")
	testenv.Must(t, e.Repo(reader).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}))
	if _, err := rpc.UploadBlobs(ctx, e.Repo(reader), "web", [][]byte{[]byte("x")}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reader upload: want PermissionDenied, got %v", err)
	}
	if _, err := e.Repo(stranger).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stranger: want PermissionDenied, got %v", err)
	}
	if _, err := e.Repo(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown repo: want NotFound, got %v", err)
	}
	if _, err := e.Repo(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web", Rev: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown rev: want NotFound, got %v", err)
	}
	if _, err := e.Repo(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "Bad/Name"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad repo name: want InvalidArgument, got %v", err)
	}
}

func TestLogBlameDiffMergeFile(t *testing.T) {
	e := testenv.Start(t)
	first := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	c1 := e.CommitDirect("web", map[string]string{"f.txt": "one\n"}, "add f")
	c2 := e.CommitDirect("web", map[string]string{"f.txt": "one\ntwo\n"}, "extend f")

	lg := testenv.Must(t, c.Log(ctx, &sgitv1.LogRequest{Repo: "web"}))
	if len(lg.Entries) != 3 || lg.Entries[0].Sha != c2 || lg.Entries[0].Subject != "extend f" || lg.Entries[2].Sha != first {
		t.Fatalf("log = %+v", lg.Entries)
	}
	bl := testenv.Must(t, c.Blame(ctx, &sgitv1.BlameRequest{Repo: "web", Path: "f.txt"}))
	if len(bl.Lines) != 2 || bl.Lines[0].Sha != c1 || string(bl.Lines[1].Text) != "two" || bl.Lines[1].Author != "direct" {
		t.Fatalf("blame = %+v", bl.Lines)
	}
	if _, err := c.Blame(ctx, &sgitv1.BlameRequest{Repo: "web", Path: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("blame missing: want NotFound, got %v", err)
	}
	df := testenv.Must(t, c.Diff(ctx, &sgitv1.DiffRequest{Repo: "web", A: c1, B: c2}))
	if !bytes.Contains(df.Patch, []byte("+two")) {
		t.Fatalf("diff = %s", df.Patch)
	}

	// git treats edits on adjacent lines as conflicting, so the clean case edits lines 1 and 3.
	shas := e.Upload(e.AdminToken, "web", []byte("a\nb\nc\n"), []byte("a\nX\nc\n"), []byte("a\nY\nc\n"), []byte("A\nb\nc\n"), []byte("a\nb\nC\n"))
	clean := testenv.Must(t, c.MergeFile(ctx, &sgitv1.MergeFileRequest{Repo: "web", BaseSha: shas[0], OursSha: shas[3], TheirsSha: shas[4]}))
	if clean.Conflicted || string(clean.Content) != "A\nb\nC\n" {
		t.Fatalf("clean merge = %q %v", clean.Content, clean.Conflicted)
	}
	conf := testenv.Must(t, c.MergeFile(ctx, &sgitv1.MergeFileRequest{Repo: "web", BaseSha: shas[0], OursSha: shas[1], TheirsSha: shas[2], OursLabel: "main", TheirsLabel: "local"}))
	if !conf.Conflicted || !bytes.Contains(conf.Content, []byte("<<<<<<< main")) {
		t.Fatalf("conflict merge = %q", conf.Content)
	}
	addAdd := testenv.Must(t, c.MergeFile(ctx, &sgitv1.MergeFileRequest{Repo: "web", OursSha: shas[1], TheirsSha: shas[1]}))
	if addAdd.Conflicted || string(addAdd.Content) != "a\nX\nc\n" {
		t.Fatalf("identical add/add = %q %v", addAdd.Content, addAdd.Conflicted)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/server/`
Expected: FAIL to compile (`undefined: validatePath`). After `validate.go` is created, the RPC tests fail with `Unimplemented` (`method GetObjects not implemented`, …).

- [ ] **Step 3: Implement (append to `internal/server/read.go` and extend its imports)**

Replace the import block of `internal/server/read.go` with:
```go
import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)
```

Append:
```go
func checkBatch(shas []string) error {
	if len(shas) > maxBatch {
		return status.Errorf(codes.InvalidArgument, "too many objects: %d > %d", len(shas), maxBatch)
	}
	for _, s := range shas {
		if !gitx.IsSHA(s) {
			return status.Errorf(codes.InvalidArgument, "invalid object id %q", s)
		}
	}
	return nil
}

func (r *repoService) GetObjects(req *sgitv1.GetObjectsRequest, stream grpc.ServerStreamingServer[sgitv1.Object]) error {
	ctx := stream.Context()
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return err
	}
	if err := checkBatch(req.Shas); err != nil {
		return err
	}
	for _, sha := range req.Shas {
		obj, err := h.git.ReadObject(ctx, sha)
		if err != nil {
			return r.s.toStatus(ctx, err)
		}
		msg := &sgitv1.Object{Sha: sha, Type: obj.Type, Size: uint64(len(obj.Data)), ZstdData: r.s.zenc.EncodeAll(obj.Data, nil)}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

func (r *repoService) HasObjects(ctx context.Context, req *sgitv1.HasObjectsRequest) (*sgitv1.HasObjectsResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if err := checkBatch(req.Shas); err != nil {
		return nil, err
	}
	present := make([]bool, len(req.Shas))
	for i, sha := range req.Shas {
		_, _, err := h.git.ObjectInfo(ctx, sha)
		switch {
		case err == nil:
			present[i] = true
		case errors.Is(err, gitx.ErrNotFound):
		default:
			return nil, r.s.toStatus(ctx, err)
		}
	}
	return &sgitv1.HasObjectsResponse{Present: present}, nil
}

func (r *repoService) UploadBlobs(stream grpc.ClientStreamingServer[sgitv1.BlobChunk, sgitv1.UploadBlobsResponse]) error {
	ctx := stream.Context()
	var h *repoHandle
	var buf bytes.Buffer
	var shas []string
	pending := false
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			if pending {
				return status.Error(codes.InvalidArgument, "stream ended inside a blob (missing end_of_blob)")
			}
			return stream.SendAndClose(&sgitv1.UploadBlobsResponse{Shas: shas})
		}
		if err != nil {
			return err
		}
		if h == nil {
			if h, _, err = r.s.open(ctx, c.Repo, auth.ActionWrite); err != nil {
				return err
			}
		}
		if int64(buf.Len()+len(c.Data)) > r.s.cfg.MaxBlobBytes {
			return status.Errorf(codes.InvalidArgument, "blob exceeds %d bytes", r.s.cfg.MaxBlobBytes)
		}
		buf.Write(c.Data)
		pending = true
		if c.EndOfBlob {
			sha, err := h.git.WriteBlob(ctx, buf.Bytes())
			if err != nil {
				return r.s.toStatus(ctx, err)
			}
			shas = append(shas, sha)
			buf.Reset()
			pending = false
		}
	}
}

func clean(s string) string { return strings.ToValidUTF8(s, "�") }

func (r *repoService) Log(ctx context.Context, req *sgitv1.LogRequest) (*sgitv1.LogResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	rev, n := req.Rev, int(req.N)
	if rev == "" {
		rev = "main"
	}
	if n <= 0 {
		n = 50
	}
	if n > 1000 {
		n = 1000
	}
	entries, err := h.git.Log(ctx, rev, req.Path, n)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.LogResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &sgitv1.LogEntry{
			Sha: e.SHA, AuthorName: clean(e.AuthorName), AuthorEmail: clean(e.AuthorEmail), Time: e.Time.Unix(), Subject: clean(e.Subject),
		})
	}
	return resp, nil
}

func (r *repoService) Blame(ctx context.Context, req *sgitv1.BlameRequest) (*sgitv1.BlameResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if err := validatePath(req.Path); err != nil {
		return nil, err
	}
	rev := req.Rev
	if rev == "" {
		rev = "main"
	}
	lines, err := h.git.Blame(ctx, rev, req.Path)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.BlameResponse{}
	for _, l := range lines {
		resp.Lines = append(resp.Lines, &sgitv1.BlameLine{Sha: l.SHA, Author: clean(l.Author), Line: int32(l.Line), Text: []byte(l.Text)})
	}
	return resp, nil
}

func (r *repoService) Diff(ctx context.Context, req *sgitv1.DiffRequest) (*sgitv1.DiffResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	for _, p := range req.Paths {
		if err := validatePath(p); err != nil {
			return nil, err
		}
	}
	patch, err := h.git.Diff(ctx, req.A, req.B, req.Paths)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.DiffResponse{Patch: patch}, nil
}

func (r *repoService) readBlobOrEmpty(ctx context.Context, h *repoHandle, sha string) ([]byte, error) {
	if sha == "" {
		return nil, nil
	}
	obj, err := h.git.ReadObject(ctx, sha)
	if err != nil {
		return nil, err
	}
	if obj.Type != "blob" {
		return nil, status.Errorf(codes.InvalidArgument, "%s is a %s, not a blob", sha, obj.Type)
	}
	return obj.Data, nil
}

func (r *repoService) MergeFile(ctx context.Context, req *sgitv1.MergeFileRequest) (*sgitv1.MergeFileResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	var parts [3][]byte
	for i, sha := range []string{req.BaseSha, req.OursSha, req.TheirsSha} {
		if parts[i], err = r.readBlobOrEmpty(ctx, h, sha); err != nil {
			return nil, r.s.toStatus(ctx, err)
		}
	}
	ol, tl := req.OursLabel, req.TheirsLabel
	if ol == "" {
		ol = "ours"
	}
	if tl == "" {
		tl = "theirs"
	}
	out, conflicted, err := gitx.MergeFile(ctx, parts[0], parts[1], parts[2], ol, "base", tl)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.MergeFileResponse{Content: out, Conflicted: conflicted}, nil
}
```

Also create the validation helpers. `read.go` uses `validatePath`, and Task 10 uses `convertChanges`.

`internal/server/validate.go`:
```go
package server

import (
	"context"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validatePath(p string) error {
	bad := func(why string) error { return status.Errorf(codes.InvalidArgument, "invalid path %q: %s", p, why) }
	switch {
	case p == "":
		return bad("empty")
	case len(p) > 4096:
		return bad("too long")
	case !utf8.ValidString(p):
		return bad("not UTF-8")
	case strings.ContainsAny(p, "\x00\r\n"):
		return bad("control character")
	case strings.HasPrefix(p, "/"):
		return bad("absolute")
	case path.Clean(p) != p:
		return bad("not normalized")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return bad("dot segment")
		}
		if strings.EqualFold(seg, ".git") || strings.EqualFold(seg, ".sg") {
			return bad("reserved name " + seg)
		}
	}
	return nil
}

func validMode(m string) bool {
	switch m {
	case "", "100644", "100755", "120000":
		return true
	}
	return false
}

// convertChanges validates a client change list and checks referenced blobs exist.
func (s *Server) convertChanges(ctx context.Context, h *repoHandle, in []*sgitv1.Change) ([]gitx.Change, error) {
	if len(in) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty change list")
	}
	if len(in) > s.cfg.MaxChanges {
		return nil, status.Errorf(codes.InvalidArgument, "too many changes: %d > %d", len(in), s.cfg.MaxChanges)
	}
	seen := make(map[string]bool, len(in))
	upserts := map[string]bool{}
	out := make([]gitx.Change, 0, len(in))
	for _, c := range in {
		if err := validatePath(c.Path); err != nil {
			return nil, err
		}
		if seen[c.Path] {
			return nil, status.Errorf(codes.InvalidArgument, "duplicate path %q", c.Path)
		}
		seen[c.Path] = true
		switch c.Op {
		case sgitv1.ChangeOp_CHANGE_OP_DELETE:
			out = append(out, gitx.Change{Path: c.Path, Op: gitx.OpDelete})
		case sgitv1.ChangeOp_CHANGE_OP_ADD, sgitv1.ChangeOp_CHANGE_OP_MODIFY:
			if !validMode(c.Mode) {
				return nil, status.Errorf(codes.InvalidArgument, "invalid mode %q for %q", c.Mode, c.Path)
			}
			if !gitx.IsSHA(c.BlobSha) {
				return nil, status.Errorf(codes.InvalidArgument, "invalid blob id %q for %q", c.BlobSha, c.Path)
			}
			typ, _, err := h.git.ObjectInfo(ctx, c.BlobSha)
			if errors.Is(err, gitx.ErrNotFound) || (err == nil && typ != "blob") {
				return nil, status.Errorf(codes.InvalidArgument, "blob %s for %q not uploaded", c.BlobSha, c.Path)
			}
			if err != nil {
				return nil, s.toStatus(ctx, err)
			}
			op := gitx.OpAdd
			if c.Op == sgitv1.ChangeOp_CHANGE_OP_MODIFY {
				op = gitx.OpModify
			}
			upserts[c.Path] = true
			out = append(out, gitx.Change{Path: c.Path, Op: op, BlobSHA: c.BlobSha, Mode: c.Mode})
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown op for %q", c.Path)
		}
	}
	for p := range upserts {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if upserts[d] {
				return nil, status.Errorf(codes.InvalidArgument, "path %q conflicts with file %q in the same change list", p, d)
			}
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race ./internal/server/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./... ; staticcheck ./...
node "$PUSH" . sgit "feat(server): GetObjects/HasObjects/UploadBlobs, Log/Blame/Diff/MergeFile"
```

---

### Task 10: Drafts

**Files:**
- Create: `internal/server/drafts.go`
- Test: `internal/server/drafts_test.go`

**Interfaces:**
- Consumes: Task 8/9 (including `convertChanges` from `validate.go`).
- Produces:
  - RPCs: `CreateDraft`, `UpdateDraft`, `GetDraft`, `ListDrafts`, `AbandonDraft`
  - Refs: `headRef(id) string`, `versionRef(id string, v int) string`, `draftIDRe`
  - Used by Task 11: `(*repoService).createDraft(ctx, h, u, base, changes, title, message) (store.Draft, error)`, `(*repoService).updateDraft(ctx, h, u, id, base, changes, message) (store.Draft, error)`, `(*repoService).loadOwnedOpenDraft(ctx, h, u, id) (store.Draft, error)`, `draftPB(store.Draft) *sgitv1.Draft`

- [ ] **Step 1: Write the failing tests**

`internal/server/drafts_test.go`:
```go
package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateAndUpdateDraft(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Repo(alice)
	ctx := context.Background()
	shas := e.Upload(alice, "web", []byte("v1\n"), []byte("v2\n"))

	d := testenv.Must(t, c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "add a\n\nbody"}))
	if len(d.Id) != 8 || d.Owner != "alice" || d.Title != "add a" || d.Version != 1 || d.Status != "open" || d.Base != main {
		t.Fatalf("draft = %+v", d)
	}
	g := e.Git("web")
	defer g.Close()
	head := testenv.Must(t, g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/head"))
	v1 := testenv.Must(t, g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v1"))
	if head != d.Head || v1 != d.Head {
		t.Fatalf("refs head=%s v1=%s draft.head=%s", head, v1, d.Head)
	}
	if got := e.Files("web", d.Head); got["a.txt"] != "v1\n" {
		t.Fatalf("draft files = %v", got)
	}
	if got := e.Files("web", "main"); len(got) != 0 {
		t.Fatal("creating a draft must not touch main")
	}
	commit := testenv.Must(t, g.ReadCommit(ctx, d.Head))
	if commit.Author.Name != "alice" || len(commit.Parents) != 1 || commit.Parents[0] != main {
		t.Fatalf("draft commit = %+v", commit)
	}

	d2 := testenv.Must(t, c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[1])}, Message: "add a v2"}))
	if d2.Version != 2 || d2.Head == d.Head {
		t.Fatalf("updated = %+v", d2)
	}
	if testenv.Must(t, g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v1")) != d.Head {
		t.Fatal("v1 ref must be preserved")
	}
	if e.Files("web", d2.Head)["a.txt"] != "v2\n" {
		t.Fatal("update must be a full delta vs base")
	}
	got := testenv.Must(t, c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}))
	if got.Head != d2.Head {
		t.Fatalf("GetDraft = %+v", got)
	}
	list := testenv.Must(t, c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Repo: "web", Mine: true, Status: "open"}))
	if len(list.Drafts) != 1 {
		t.Fatalf("ListDrafts = %+v", list.Drafts)
	}
	if _, err := c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Repo: "web", Status: "weird"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad status filter: want InvalidArgument, got %v", err)
	}
	ab := testenv.Must(t, c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}))
	if ab.Status != "abandoned" {
		t.Fatalf("abandon = %+v", ab)
	}
	if _, err := c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "m"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("update abandoned: want FailedPrecondition, got %v", err)
	}
}

func TestCreateDraftValidation(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("x"))[0]
	missing := "1111111111111111111111111111111111111111"
	cases := map[string]*sgitv1.CreateDraftRequest{
		"empty changes": {Repo: "web", Base: main, Message: "m"},
		"no message":    {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}},
		"bad base":      {Repo: "web", Base: "xyz", Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"},
		"unknown base":  {Repo: "web", Base: missing, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"},
		"missing blob":  {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", missing)}, Message: "m"},
		"bad path":      {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("../a", sha)}, Message: "m"},
		"git dir":       {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add(".git/hooks/x", sha)}, Message: "m"},
		"bad mode":      {Repo: "web", Base: main, Changes: []*sgitv1.Change{{Path: "a", Op: sgitv1.ChangeOp_CHANGE_OP_ADD, BlobSha: sha, Mode: "040000"}}, Message: "m"},
		"no op":         {Repo: "web", Base: main, Changes: []*sgitv1.Change{{Path: "a", BlobSha: sha}}, Message: "m"},
		"duplicate":     {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha), testenv.Add("a", sha)}, Message: "m"},
		"file and dir":  {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha), testenv.Add("a/b", sha)}, Message: "m"},
		"delete absent": {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Delete("nope")}, Message: "m"},
	}
	for name, req := range cases {
		if _, err := c.CreateDraft(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", name, err)
		}
	}
}

func TestCreateDraftUnicodePath(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	sha := e.Upload(e.AdminToken, "web", []byte("x\n"))[0]
	d := testenv.Must(t, c.CreateDraft(context.Background(), &sgitv1.CreateDraftRequest{
		Repo: "web", Base: main, Message: "unicode",
		Changes: []*sgitv1.Change{testenv.Add("docs/my notes/файл ü.md", sha)},
	}))
	if e.Files("web", d.Head)["docs/my notes/файл ü.md"] != "x\n" {
		t.Fatalf("files = %v", e.Files("web", d.Head))
	}
}

func TestDraftOwnership(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	bob := e.NewUser("bob", "web", "write")
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("x"))[0]
	d := testenv.Must(t, e.Repo(alice).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}))
	b := e.Repo(bob)
	if _, err := b.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob update: want PermissionDenied, got %v", err)
	}
	if _, err := b.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob abandon: want PermissionDenied, got %v", err)
	}
	testenv.Must(t, b.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id})) // reading is fine
	testenv.Must(t, e.Repo(e.AdminToken).AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}))
	if _, err := b.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: "zzzzzzzz"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown draft: want NotFound, got %v", err)
	}
	if _, err := b.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: "../x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad draft id: want InvalidArgument, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/server/ -run Draft`
Expected: FAIL with `Unimplemented` (`method CreateDraft not implemented`).

- [ ] **Step 3: Implement**

`internal/server/drafts.go`:
```go
package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var draftIDRe = regexp.MustCompile(`^[a-z2-7]{8}$`)

func headRef(id string) string           { return "refs/sg/drafts/" + id + "/head" }
func versionRef(id string, v int) string { return fmt.Sprintf("refs/sg/drafts/%s/v%d", id, v) }

func newDraftID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l)
}

func draftPB(d store.Draft) *sgitv1.Draft {
	return &sgitv1.Draft{
		Id: d.ID, Repo: d.Repo, Owner: d.OwnerName, Title: d.Title, Base: d.Base, Head: d.Head,
		Version: int32(d.Version), Status: d.Status, LandedAs: d.LandedAs,
		CreatedAt: d.CreatedAt.Unix(), UpdatedAt: d.UpdatedAt.Unix(),
	}
}

// buildDraftCommit creates (without any ref) a commit = base + changes.
func (r *repoService) buildDraftCommit(ctx context.Context, h *repoHandle, u store.User, base string, changes []*sgitv1.Change, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", status.Error(codes.InvalidArgument, "commit message required")
	}
	if !gitx.IsSHA(base) {
		return "", status.Errorf(codes.InvalidArgument, "invalid base %q", base)
	}
	bc, err := h.git.ReadCommit(ctx, base)
	if errors.Is(err, gitx.ErrNotFound) || errors.Is(err, gitx.ErrInvalid) {
		return "", status.Errorf(codes.InvalidArgument, "base commit %s not found", base)
	}
	if err != nil {
		return "", err
	}
	gc, err := r.s.convertChanges(ctx, h, changes)
	if err != nil {
		return "", err
	}
	tree, err := h.git.BuildTree(ctx, base, gc)
	if err != nil {
		return "", err
	}
	if tree == bc.Tree {
		return "", status.Error(codes.InvalidArgument, "changes produce no difference from base")
	}
	sig := r.s.sig(u)
	return h.git.CommitTree(ctx, tree, []string{base}, message, sig, sig)
}

func (r *repoService) createDraft(ctx context.Context, h *repoHandle, u store.User, base string, changes []*sgitv1.Change, title, message string) (store.Draft, error) {
	c, err := r.buildDraftCommit(ctx, h, u, base, changes, message)
	if err != nil {
		return store.Draft{}, err
	}
	id, err := newDraftID()
	if err != nil {
		return store.Draft{}, err
	}
	if err := h.git.UpdateRef(ctx, headRef(id), c, ""); err != nil {
		return store.Draft{}, err
	}
	if err := h.git.UpdateRef(ctx, versionRef(id, 1), c, ""); err != nil {
		return store.Draft{}, err
	}
	if title == "" {
		title = firstLine(message)
	}
	now := r.s.cfg.Now()
	d := store.Draft{ID: id, Repo: h.name, OwnerID: u.ID, OwnerName: u.Name, Title: title, Base: base, Head: c,
		Version: 1, Status: store.DraftOpen, CreatedAt: now, UpdatedAt: now}
	return d, r.s.store.InsertDraft(ctx, d)
}

func (r *repoService) loadOwnedOpenDraft(ctx context.Context, h *repoHandle, u store.User, id string) (store.Draft, error) {
	if !draftIDRe.MatchString(id) {
		return store.Draft{}, status.Errorf(codes.InvalidArgument, "invalid draft id %q", id)
	}
	d, err := r.s.store.GetDraft(ctx, h.name, id)
	if err != nil {
		return d, err
	}
	if d.OwnerID != u.ID && !u.Admin {
		return d, status.Errorf(codes.PermissionDenied, "draft %s belongs to %s", id, d.OwnerName)
	}
	if d.Status != store.DraftOpen {
		return d, status.Errorf(codes.FailedPrecondition, "draft %s is %s", id, d.Status)
	}
	return d, nil
}

func (r *repoService) updateDraft(ctx context.Context, h *repoHandle, u store.User, id, base string, changes []*sgitv1.Change, message string) (store.Draft, error) {
	d, err := r.loadOwnedOpenDraft(ctx, h, u, id)
	if err != nil {
		return d, err
	}
	c, err := r.buildDraftCommit(ctx, h, u, base, changes, message)
	if err != nil {
		return d, err
	}
	if err := h.git.UpdateRef(ctx, headRef(id), c, d.Head); err != nil {
		if errors.Is(err, gitx.ErrRefMoved) {
			return d, status.Errorf(codes.Aborted, "draft %s was updated concurrently", id)
		}
		return d, err
	}
	d.Version++
	if err := h.git.UpdateRef(ctx, versionRef(id, d.Version), c, ""); err != nil {
		return d, err
	}
	d.Base, d.Head, d.UpdatedAt = base, c, r.s.cfg.Now()
	return d, r.s.store.UpdateDraft(ctx, d)
}

func (r *repoService) CreateDraft(ctx context.Context, req *sgitv1.CreateDraftRequest) (*sgitv1.Draft, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.createDraft(ctx, h, u, req.Base, req.Changes, req.Title, req.Message)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}

func (r *repoService) UpdateDraft(ctx context.Context, req *sgitv1.UpdateDraftRequest) (*sgitv1.Draft, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.updateDraft(ctx, h, u, req.Id, req.Base, req.Changes, req.Message)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}

func (r *repoService) GetDraft(ctx context.Context, req *sgitv1.GetDraftRequest) (*sgitv1.Draft, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if !draftIDRe.MatchString(req.Id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid draft id %q", req.Id)
	}
	d, err := r.s.store.GetDraft(ctx, h.name, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}

func (r *repoService) ListDrafts(ctx context.Context, req *sgitv1.ListDraftsRequest) (*sgitv1.ListDraftsResponse, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	switch req.Status {
	case "", store.DraftOpen, store.DraftLanded, store.DraftAbandoned:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid status filter %q", req.Status)
	}
	var owner int64
	if req.Mine {
		owner = u.ID
	}
	ds, err := r.s.store.ListDrafts(ctx, h.name, owner, req.Status)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.ListDraftsResponse{}
	for _, d := range ds {
		resp.Drafts = append(resp.Drafts, draftPB(d))
	}
	return resp, nil
}

func (r *repoService) AbandonDraft(ctx context.Context, req *sgitv1.AbandonDraftRequest) (*sgitv1.Draft, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.loadOwnedOpenDraft(ctx, h, u, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	d.Status, d.UpdatedAt = store.DraftAbandoned, r.s.cfg.Now()
	if err := r.s.store.UpdateDraft(ctx, d); err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race ./internal/server/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./... ; staticcheck ./...
node "$PUSH" . sgit "feat(server): drafts with versioned hidden refs, change validation"
```

---

### Task 11: Land, CommitAndLand, CAS loop, conflicts

**Files:**
- Create: `internal/server/land.go`
- Test: `internal/server/land_test.go`

**Interfaces:**
- Consumes: Task 10 (`createDraft`, `updateDraft`, `loadOwnedOpenDraft`), and `gitx.MergeTree`, `gitx.UpdateRef`, `gitx.ErrRefMoved`.
- Produces:
  - RPCs: `Land` and `CommitAndLand`, both returning `LandResponse{commit, draft_id}`.
  - Errors:
    - A conflict returns `codes.Aborted` with the detail `*sgitv1.ConflictDetail{DraftId, Main, Files}`.
    - Exhausted retries return `codes.ResourceExhausted` and leave the draft open.
    - A landed or abandoned draft returns `codes.FailedPrecondition`.

- [ ] **Step 1: Write the failing tests**

`internal/server/land_test.go`:
```go
package server_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func commitAndLand(c sgitv1.RepoClient, base string, path, sha, msg string) (*sgitv1.LandResponse, error) {
	return c.CommitAndLand(context.Background(), &sgitv1.CommitAndLandRequest{
		Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Add(path, sha)}, Message: msg,
	})
}

// assertLinear checks that every commit on main has exactly one parent except the root.
func assertLinear(t *testing.T, e *testenv.Env, wantCommits int) {
	t.Helper()
	ctx := context.Background()
	g := e.Git("web")
	defer g.Close()
	log := testenv.Must(t, g.Log(ctx, "main", "", 1000))
	if len(log) != wantCommits {
		t.Fatalf("main has %d commits, want %d", len(log), wantCommits)
	}
	for i, le := range log {
		c := testenv.Must(t, g.ReadCommit(ctx, le.SHA))
		want := 1
		if i == len(log)-1 {
			want = 0
		}
		if len(c.Parents) != want {
			t.Fatalf("commit %s has %d parents", le.SHA, len(c.Parents))
		}
	}
}

func TestLandFastPath(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Repo(alice)
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("hello\n"))[0]
	resp := testenv.Must(t, commitAndLand(c, main, "a.txt", sha, "add a"))
	if e.Files("web", "main")["a.txt"] != "hello\n" {
		t.Fatal("file not on main")
	}
	rr := testenv.Must(t, c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}))
	if rr.Sha != resp.Commit {
		t.Fatalf("main = %s, landed %s", rr.Sha, resp.Commit)
	}
	d := testenv.Must(t, c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: resp.DraftId}))
	if d.Status != "landed" || d.LandedAs != resp.Commit {
		t.Fatalf("draft = %+v", d)
	}
	g := e.Git("web")
	defer g.Close()
	lc := testenv.Must(t, g.ReadCommit(ctx, resp.Commit))
	if lc.Author.Name != "alice" || lc.Message != "add a\n" {
		t.Fatalf("landed commit = %+v", lc)
	}
	assertLinear(t, e, 2)
}

func TestLandRebasesOntoMovedMain(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("a\n"), []byte("b\n"))
	d1 := testenv.Must(t, c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "a"}))
	d2 := testenv.Must(t, c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("b.txt", shas[1])}, Message: "b"}))
	testenv.Must(t, c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d1.Id}))
	testenv.Must(t, c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d2.Id}))
	files := e.Files("web", "main")
	if files["a.txt"] != "a\n" || files["b.txt"] != "b\n" {
		t.Fatalf("files = %v", files)
	}
	assertLinear(t, e, 3)
}

func TestLandConflict(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	base := e.CommitDirect("web", map[string]string{"f.txt": "a\nb\nc\n"}, "base")
	shas := e.Upload(e.AdminToken, "web", []byte("a\nB1\nc\n"), []byte("a\nB2\nc\n"))
	first := testenv.Must(t, c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[0])}, Message: "one"}))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[1])}, Message: "two"})
	st := status.Convert(err)
	if st.Code() != codes.Aborted {
		t.Fatalf("want Aborted, got %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range st.Details() {
		if cd, ok := d.(*sgitv1.ConflictDetail); ok {
			det = cd
		}
	}
	if det == nil || det.Main != first.Commit || len(det.DraftId) != 8 || len(det.Files) != 1 {
		t.Fatalf("detail = %+v", det)
	}
	f := det.Files[0]
	if f.Path != "f.txt" || f.OursSha != shas[0] || f.TheirsSha != shas[1] || f.BaseSha == "" {
		t.Fatalf("conflict file = %+v", f)
	}
	d := testenv.Must(t, c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: det.DraftId}))
	if d.Status != "open" {
		t.Fatalf("conflicted draft must stay open: %+v", d)
	}
	if e.Files("web", "main")["f.txt"] != "a\nB1\nc\n" {
		t.Fatal("main changed by a conflicting land")
	}
	// Resolve: update the draft against the new main and land again.
	fixed := e.Upload(e.AdminToken, "web", []byte("a\nB1+B2\nc\n"))[0]
	testenv.Must(t, c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", DraftId: det.DraftId, Base: first.Commit, Changes: []*sgitv1.Change{testenv.Modify("f.txt", fixed)}, Message: "two (resolved)"}))
	if e.Files("web", "main")["f.txt"] != "a\nB1+B2\nc\n" {
		t.Fatal("resolved land missing")
	}
}

func TestLandTwiceAndAbandoned(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("x"))[0]
	resp := testenv.Must(t, commitAndLand(c, main, "x", sha, "x"))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: resp.DraftId}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("land twice: want FailedPrecondition, got %v", err)
	}
	d := testenv.Must(t, c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: resp.Commit, Changes: []*sgitv1.Change{testenv.Delete("x")}, Message: "rm"}))
	testenv.Must(t, c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("land abandoned: want FailedPrecondition, got %v", err)
	}
	assertLinear(t, e, 2)
}

func TestConcurrentLands(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	const n = 50
	var blobs [][]byte
	for i := 0; i < n; i++ {
		blobs = append(blobs, []byte(fmt.Sprintf("content %d\n", i)))
	}
	shas := e.Upload(e.AdminToken, "web", blobs...)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := commitAndLand(c, main, fmt.Sprintf("f%02d.txt", i), shas[i], fmt.Sprintf("agent %d", i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent land: %v", err)
		}
	}
	if got := len(e.Files("web", "main")); got != n {
		t.Fatalf("main has %d files, want %d", got, n)
	}
	assertLinear(t, e, n+1)
}

func TestLandRetriesWhenMainMoves(t *testing.T) {
	var e *testenv.Env
	var moved atomic.Bool
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeCAS = func(repo string) {
			if moved.CompareAndSwap(false, true) {
				e.CommitDirect(repo, map[string]string{"external.txt": "ext\n"}, "external writer")
			}
		}
	})
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	sha := e.Upload(e.AdminToken, "web", []byte("mine\n"))[0]
	testenv.Must(t, commitAndLand(c, main, "mine.txt", sha, "mine"))
	files := e.Files("web", "main")
	if files["external.txt"] != "ext\n" || files["mine.txt"] != "mine\n" {
		t.Fatalf("files = %v", files)
	}
	assertLinear(t, e, 3)
}

func TestLandGivesUpWhenMainKeepsMoving(t *testing.T) {
	var e *testenv.Env
	var n atomic.Int32
	e = testenv.Start(t, func(c *server.Config) {
		c.LandRetries = 3
		c.BeforeCAS = func(repo string) {
			i := n.Add(1)
			e.CommitDirect(repo, map[string]string{fmt.Sprintf("ext%d.txt", i): "x"}, "external")
		}
	})
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("mine\n"))[0]
	d := testenv.Must(t, c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("mine.txt", sha)}, Message: "mine"}))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", n.Load())
	}
	got := testenv.Must(t, c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}))
	if got.Status != "open" {
		t.Fatalf("draft must stay open: %+v", got)
	}
	if _, ok := e.Files("web", "main")["mine.txt"]; ok {
		t.Fatal("draft landed despite exhaustion")
	}
}

func TestLandByNonOwner(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	bob := e.NewUser("bob", "web", "write")
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("x"))[0]
	d := testenv.Must(t, e.Repo(alice).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}))
	if _, err := e.Repo(bob).Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob land: want PermissionDenied, got %v", err)
	}
	if _, err := e.Repo(bob).CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", DraftId: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob commit-and-land: want PermissionDenied, got %v", err)
	}
	testenv.Must(t, e.Repo(e.AdminToken).Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id})) // admin may land
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/server/ -run 'Land|Concurrent'`
Expected: FAIL with `Unimplemented` (`method CommitAndLand not implemented`).

- [ ] **Step 3: Implement**

`internal/server/land.go`:
```go
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (r *repoService) Land(ctx context.Context, req *sgitv1.LandRequest) (*sgitv1.LandResponse, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.loadOwnedOpenDraft(ctx, h, u, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	commit, err := r.land(ctx, h, d.ID)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.LandResponse{Commit: commit, DraftId: d.ID}, nil
}

func (r *repoService) CommitAndLand(ctx context.Context, req *sgitv1.CommitAndLandRequest) (*sgitv1.LandResponse, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	var d store.Draft
	if req.DraftId == "" {
		d, err = r.createDraft(ctx, h, u, req.Base, req.Changes, req.Title, req.Message)
	} else {
		d, err = r.updateDraft(ctx, h, u, req.DraftId, req.Base, req.Changes, req.Message)
	}
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	commit, err := r.land(ctx, h, d.ID)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.LandResponse{Commit: commit, DraftId: d.ID}, nil
}

// land rebases the draft onto main as a single linear commit and moves main
// with compare-and-swap, retrying if main moved underneath.
func (r *repoService) land(ctx context.Context, h *repoHandle, id string) (string, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()

	// Re-read under the lock: a concurrent Land of the same draft may have won.
	d, err := r.s.store.GetDraft(ctx, h.name, id)
	if err != nil {
		return "", err
	}
	if d.Status != store.DraftOpen {
		return "", status.Errorf(codes.FailedPrecondition, "draft %s is %s", id, d.Status)
	}
	dc, err := h.git.ReadCommit(ctx, d.Head)
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < r.s.cfg.LandRetries; attempt++ {
		mainOld, err := h.git.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", err
		}
		tree := dc.Tree
		if mainOld != d.Base {
			mr, err := h.git.MergeTree(ctx, d.Base, mainOld, d.Head)
			if err != nil {
				return "", err
			}
			if !mr.Clean {
				return "", conflictStatus(d.ID, mainOld, mr.Conflicts)
			}
			tree = mr.Tree
		}
		committer := dc.Committer
		committer.When = r.s.cfg.Now()
		newC, err := h.git.CommitTree(ctx, tree, []string{mainOld}, dc.Message, dc.Author, committer)
		if err != nil {
			return "", err
		}
		if r.s.cfg.BeforeCAS != nil {
			r.s.cfg.BeforeCAS(h.name)
		}
		err = h.git.UpdateRef(ctx, gitx.MainRef, newC, mainOld)
		if errors.Is(err, gitx.ErrRefMoved) {
			continue
		}
		if err != nil {
			return "", err
		}
		d.Status, d.LandedAs, d.UpdatedAt = store.DraftLanded, newC, r.s.cfg.Now()
		if err := r.s.store.UpdateDraft(ctx, d); err != nil {
			// main already moved; report success but log the bookkeeping failure.
			r.s.log.ErrorContext(ctx, "landed but failed to mark draft", "draft", d.ID, "commit", newC, "err", err)
		}
		return newC, nil
	}
	return "", status.Errorf(codes.ResourceExhausted, "main kept moving; gave up after %d attempts", r.s.cfg.LandRetries)
}

func conflictStatus(draftID, main string, files []gitx.ConflictFile) error {
	st := status.New(codes.Aborted, fmt.Sprintf("conflict in %d file(s)", len(files)))
	det := &sgitv1.ConflictDetail{DraftId: draftID, Main: main}
	for _, f := range files {
		det.Files = append(det.Files, &sgitv1.ConflictFile{
			Path: strings.ToValidUTF8(f.Path, "�"), BaseSha: f.Base, OursSha: f.Ours, TheirsSha: f.Theirs,
		})
	}
	withDet, err := st.WithDetails(det)
	if err != nil {
		return st.Err()
	}
	return withDet.Err()
}
```

- [ ] **Step 4: Run all server tests to confirm they pass**

Run: `go test -race -count=1 ./internal/server/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./... ; staticcheck ./...
node "$PUSH" . sgit "feat(server): land and commit-and-land with CAS retry and conflict details"
```

---

### Task 12: `sg-server` binary (init, serve, gen-cert, health)

**Files:**
- Create: `internal/tlsutil/tlsutil.go`, `internal/servercmd/servercmd.go`, `cmd/sg-server/main.go`
- Test: `internal/tlsutil/tlsutil_test.go`, `internal/servercmd/servercmd_test.go`

**Interfaces:**
- Consumes: `server.New`, `server.NewGRPC`, `store.Open`, `auth.NewToken`, `rpc.DialOptions`.
- Produces:
  - `tlsutil.GenerateSelfSigned(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error)`. The certificate is ECDSA P-256 with `IsCA` set, valid for 10 years, with IP or DNS SANs.
  - `servercmd.Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`, with exit codes 0 ok, 1 runtime error, 2 usage or config error.
  - Subcommands:
    - `init [--data DIR] [--admin-name NAME]`: writes `DIR/admin-token` (0600) and prints the token once. If already initialized it prints `already initialized` and exits 0.
    - `serve [--data] [--listen] [--tls-cert] [--tls-key] [--dev-insecure] [--max-blob-mb] [--log-level]`: `--dev-insecure` is only allowed on a loopback listen address.
    - `gen-cert --hosts a,b --out-cert F --out-key F`
    - `health [--addr] [--dev-insecure] [--timeout]`: the TLS probe skips verification because it only targets the local server. It prints `SERVING` and exits 0, otherwise exits 1.
  - Flag defaults come from the `SG_*` env vars listed in Global Constraints.

- [ ] **Step 1: Write the failing tests**

`internal/tlsutil/tlsutil_test.go`:
```go
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	certPEM, keyPEM, err := GenerateSelfSigned([]string{"localhost", "127.0.0.1", "sg.example"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("keypair: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.IPAddresses) != 1 || len(cert.DNSNames) != 2 || !cert.IsCA {
		t.Fatalf("SANs ip=%v dns=%v ca=%v", cert.IPAddresses, cert.DNSNames, cert.IsCA)
	}
	if err := cert.VerifyHostname("sg.example"); err != nil {
		t.Fatal(err)
	}
}
```

`internal/servercmd/servercmd_test.go`:
```go
package servercmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"google.golang.org/grpc"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	code, out, errs := run(t, "init", "--data", dir)
	if code != 0 || !strings.Contains(out, "sgt_") {
		t.Fatalf("init: code=%d out=%q err=%q", code, out, errs)
	}
	st, err := os.Stat(filepath.Join(dir, "admin-token"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("admin-token: %v %v", st, err)
	}
	code, out, _ = run(t, "init", "--data", dir)
	if code != 0 || !strings.Contains(out, "already initialized") {
		t.Fatalf("second init: code=%d out=%q", code, out)
	}
}

func TestServeRefusesUnsafeConfig(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	if code, _, errs := run(t, "serve", "--data", dir, "--listen", "127.0.0.1:0"); code != 2 || !strings.Contains(errs, "tls") {
		t.Fatalf("no TLS: code=%d err=%q", code, errs)
	}
	if code, _, errs := run(t, "serve", "--data", dir, "--listen", "0.0.0.0:0", "--dev-insecure"); code != 2 || !strings.Contains(errs, "loopback") {
		t.Fatalf("insecure non-loopback: code=%d err=%q", code, errs)
	}
	if code, _, errs := run(t, "serve", "--data", t.TempDir(), "--listen", "127.0.0.1:0", "--dev-insecure"); code != 1 || !strings.Contains(errs, "init") {
		t.Fatalf("uninitialized: code=%d err=%q", code, errs)
	}
	if code, _, _ := run(t, "bogus"); code != 2 {
		t.Fatalf("unknown subcommand: code=%d", code)
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServeTLSAndHealth(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if code, _, errs := run(t, "gen-cert", "--hosts", "localhost,127.0.0.1", "--out-cert", cert, "--out-key", key); code != 0 {
		t.Fatalf("gen-cert: %s", errs)
	}
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		var out, errb bytes.Buffer
		done <- Run(ctx, []string{"serve", "--data", dir, "--listen", addr, "--tls-cert", cert, "--tls-key", key}, &out, &errb)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, out, _ := run(t, "health", "--addr", addr)
		if code == 0 && strings.Contains(out, "SERVING") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became healthy")
		}
		time.Sleep(100 * time.Millisecond)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, "admin-token"))
	pemBytes, _ := os.ReadFile(cert)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	conn, err := grpc.NewClient(addr, rpc.DialOptions(strings.TrimSpace(string(tok)), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	who, err := sgitv1.NewAdminClient(conn).WhoAmI(context.Background(), &sgitv1.WhoAmIRequest{})
	if err != nil || who.User != "admin" || !who.Admin {
		t.Fatalf("WhoAmI over TLS = %+v, %v", who, err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/tlsutil/ ./internal/servercmd/`
Expected: FAIL to compile (`undefined: GenerateSelfSigned`, `undefined: Run`).

- [ ] **Step 3: Implement**

`internal/tlsutil/tlsutil.go`:
```go
// Package tlsutil generates self-signed certificates for sg-server.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"time"
)

// GenerateSelfSigned returns a PEM certificate and EC key valid for hosts.
// The certificate is its own CA so clients can pin it as a root.
func GenerateSelfSigned(hosts []string, now time.Time) ([]byte, []byte, error) {
	if len(hosts) == 0 {
		return nil, nil, errors.New("tlsutil: at least one host required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "sgit self-signed"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}
```

`internal/servercmd/servercmd.go`:
```go
// Package servercmd implements the sg-server command line.
package servercmd

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/store"
	"github.com/hermes98761234/sgit/internal/tlsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const usage = `usage: sg-server <command> [flags]

commands:
  init       create data dir, database and initial admin token
  serve      run the gRPC server
  gen-cert   write a self-signed TLS certificate and key
  health     probe a local server (exit 0 when SERVING)
`

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool { return os.Getenv(key) == "1" || strings.EqualFold(os.Getenv(key), "true") }

// Run executes one sg-server subcommand and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "gen-cert":
		return runGenCert(args[1:], stdout, stderr)
	case "health":
		return runHealth(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", env("SG_DATA_DIR", "./sgdata"), "data directory")
	adminName := fs.String("admin-name", "admin", "name of the initial admin user")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := os.MkdirAll(filepath.Join(*data, "repos"), 0o750); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	st, err := store.Open(filepath.Join(*data, "sg.db"))
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	defer st.Close()
	n, err := st.CountUsers(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	if n > 0 {
		fmt.Fprintln(stdout, "already initialized")
		return 0
	}
	u, err := st.CreateUser(ctx, *adminName, *adminName+"@sgit.local", true)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	id, tok, hash, err := auth.NewToken()
	if err == nil {
		err = st.CreateToken(ctx, u.ID, id, hash)
	}
	tokenPath := filepath.Join(*data, "admin-token")
	if err == nil {
		err = os.WriteFile(tokenPath, []byte(tok+"\n"), 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	fmt.Fprintf(stdout, "initialized %s\nadmin token (shown once, also saved to %s):\n%s\n", *data, tokenPath, tok)
	return 0
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func runServe(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", env("SG_DATA_DIR", "./sgdata"), "data directory")
	listen := fs.String("listen", env("SG_LISTEN", ":7419"), "listen address")
	certFile := fs.String("tls-cert", env("SG_TLS_CERT", ""), "TLS certificate (PEM)")
	keyFile := fs.String("tls-key", env("SG_TLS_KEY", ""), "TLS key (PEM)")
	insecureDev := fs.Bool("dev-insecure", envBool("SG_DEV_INSECURE"), "plaintext; loopback listen address only")
	maxBlobMB, _ := strconv.Atoi(env("SG_MAX_BLOB_MB", "100"))
	fs.IntVar(&maxBlobMB, "max-blob-mb", maxBlobMB, "max blob size in MiB")
	logLevel := fs.String("log-level", env("SG_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))

	var extra []grpc.ServerOption
	if *insecureDev {
		if !isLoopback(*listen) {
			fmt.Fprintln(stderr, "serve: --dev-insecure is only allowed on a loopback listen address")
			return 2
		}
	} else {
		if *certFile == "" || *keyFile == "" {
			fmt.Fprintln(stderr, "serve: --tls-cert and --tls-key are required (or --dev-insecure on loopback)")
			return 2
		}
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			fmt.Fprintln(stderr, "serve: load tls:", err)
			return 2
		}
		extra = append(extra, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})))
	}

	dbPath := filepath.Join(*data, "sg.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "serve: %s not initialized; run `sg-server init --data %s`\n", *data, *data)
		return 1
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	defer st.Close()
	srv, err := server.New(server.Config{DataDir: *data, MaxBlobBytes: int64(maxBlobMB) << 20}, st, log)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	defer srv.Close()
	g := server.NewGRPC(st, extra...)
	srv.Register(g)
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	log.Info("listening", "addr", lis.Addr().String(), "tls", !*insecureDev, "data", *data)
	if err := g.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

func runGenCert(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen-cert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hosts := fs.String("hosts", env("SG_TLS_HOSTS", "localhost,127.0.0.1"), "comma-separated DNS names / IPs")
	outCert := fs.String("out-cert", "", "certificate output path")
	outKey := fs.String("out-key", "", "key output path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outCert == "" || *outKey == "" {
		fmt.Fprintln(stderr, "gen-cert: --out-cert and --out-key are required")
		return 2
	}
	var hs []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hs = append(hs, h)
		}
	}
	certPEM, keyPEM, err := tlsutil.GenerateSelfSigned(hs, time.Now())
	if err == nil {
		err = os.WriteFile(*outCert, certPEM, 0o644)
	}
	if err == nil {
		err = os.WriteFile(*outKey, keyPEM, 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "gen-cert:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s and %s for %s\n", *outCert, *outKey, strings.Join(hs, ","))
	return 0
}

// healthAddr turns a listen address like ":7419" into a dialable local address.
func healthAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:7419"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func runHealth(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", healthAddr(env("SG_LISTEN", ":7419")), "server address")
	insecureDev := fs.Bool("dev-insecure", envBool("SG_DEV_INSECURE"), "plaintext")
	timeout := fs.Duration("timeout", 3*time.Second, "timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var tlsCfg *tls.Config
	if !*insecureDev {
		// Local liveness probe only: certificate identity is not checked here.
		tlsCfg = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	}
	conn, err := grpc.NewClient(*addr, rpc.DialOptions("", tlsCfg)...)
	if err != nil {
		fmt.Fprintln(stderr, "health:", err)
		return 1
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		fmt.Fprintln(stderr, "health:", err)
		return 1
	}
	fmt.Fprintln(stdout, resp.Status.String())
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		return 1
	}
	return 0
}
```

`cmd/sg-server/main.go`:
```go
// Command sg-server is the sgit gRPC server over bare git repositories.
package main

import (
	"context"
	"os"

	"github.com/hermes98761234/sgit/internal/servercmd"
)

func main() {
	os.Exit(servercmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
```

- [ ] **Step 4: Run all tests to confirm they pass, then a manual check**

Run: `go test -race -count=1 ./... && make build && ./dist/sg-server`
Expected: all packages `ok`. The final command prints the usage text and exits 2.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; go vet ./... ; staticcheck ./...
node "$PUSH" . sgit "feat(cmd): sg-server init/serve/gen-cert/health with TLS"
```

---

### Task 13: Docker image, compose, smoke test, CI, README

**Files:**
- Create: `Dockerfile`, `.dockerignore`, `docker-entrypoint.sh` (mode 0755), `docker-compose.yml`, `scripts/docker-smoke.sh` (mode 0755), `internal/smoke/docker_smoke_test.go`
- Modify: `.github/workflows/ci.yml` (add the `docker` job), `README.md` (add the quickstart)

**Interfaces:**
- Consumes: the `sg-server` binary (Task 12), `rpc.DialOptions`, `rpc.UploadBlobs`, `testenv.Add`.
- Produces: the image `sgit-server` (local tag `sgit-server:dev`/`:smoke`), and `make docker` / `make smoke`.

- [ ] **Step 1: Write the smoke test (fails until the image exists)**

`internal/smoke/docker_smoke_test.go`:
```go
//go:build docker

// Package smoke runs an end-to-end check against a running sg-server container.
// Run via scripts/docker-smoke.sh.
package smoke

import (
	"context"
	"crypto/tls"
	"os"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc"
)

func TestDockerSmoke(t *testing.T) {
	addr, tok := os.Getenv("SG_SMOKE_ADDR"), os.Getenv("SG_SMOKE_TOKEN")
	if addr == "" || tok == "" {
		t.Skip("SG_SMOKE_ADDR / SG_SMOKE_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The container uses a self-signed certificate; identity is not under test here.
	conn := testenv.Must(t, grpc.NewClient(addr, rpc.DialOptions(tok, &tls.Config{InsecureSkipVerify: true})...))
	defer conn.Close()
	admin := sgitv1.NewAdminClient(conn)
	repo := sgitv1.NewRepoClient(conn)

	who := testenv.Must(t, admin.WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
	if !who.Admin {
		t.Fatalf("WhoAmI = %+v", who)
	}
	main := testenv.Must(t, admin.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "smoke"})).Main
	shas := testenv.Must(t, rpc.UploadBlobs(ctx, repo, "smoke", [][]byte{[]byte("hello from docker\n")}))
	landed := testenv.Must(t, repo.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{
		Repo: "smoke", Base: main, Changes: []*sgitv1.Change{testenv.Add("hello.txt", shas[0])}, Message: "smoke commit",
	}))
	lg := testenv.Must(t, repo.Log(ctx, &sgitv1.LogRequest{Repo: "smoke", N: 5}))
	if len(lg.Entries) != 2 || lg.Entries[0].Sha != landed.Commit || lg.Entries[0].Subject != "smoke commit" {
		t.Fatalf("log = %+v", lg.Entries)
	}
}
```

- [ ] **Step 2: Write the Docker files**

`Dockerfile` (set `GO_VERSION` to the major.minor of the `go` directive in `go.mod`):
```dockerfile
# syntax=docker/dockerfile:1
ARG GO_VERSION=1.23
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sg-server ./cmd/sg-server

FROM alpine:3.20
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 10001 -h /home/sg sg \
 && mkdir -p /data && chown sg:sg /data
COPY --from=build /out/sg-server /usr/local/bin/sg-server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
USER sg
ENV SG_DATA_DIR=/data SG_LISTEN=:7419
VOLUME /data
EXPOSE 7419
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=3 CMD ["sg-server", "health"]
ENTRYPOINT ["docker-entrypoint.sh"]
```

Then pin `GO_VERSION`:
```bash
GOV=$(awk '/^go /{print $2}' go.mod | cut -d. -f1,2)
sed -i "s/^ARG GO_VERSION=.*/ARG GO_VERSION=${GOV}/" Dockerfile && grep '^ARG GO_VERSION' Dockerfile
```

`.dockerignore`:
```
dist
.github
docs
**/*_test.go
```

The smoke test is built on the host, not in the image, so excluding test files is safe.

`docker-entrypoint.sh`:
```sh
#!/bin/sh
set -eu
DATA="${SG_DATA_DIR:-/data}"

if [ ! -f "$DATA/sg.db" ]; then
  sg-server init --data "$DATA"
fi

if [ -z "${SG_TLS_CERT:-}" ] && [ "${SG_TLS_SELF_SIGNED:-0}" = "1" ]; then
  mkdir -p "$DATA/tls"
  if [ ! -f "$DATA/tls/cert.pem" ]; then
    sg-server gen-cert --hosts "${SG_TLS_HOSTS:-localhost,127.0.0.1}" \
      --out-cert "$DATA/tls/cert.pem" --out-key "$DATA/tls/key.pem"
  fi
  export SG_TLS_CERT="$DATA/tls/cert.pem" SG_TLS_KEY="$DATA/tls/key.pem"
fi

exec sg-server serve "$@"
```

`docker-compose.yml`:
```yaml
services:
  sg-server:
    build: .
    image: ghcr.io/hermes98761234/sgit-server:latest
    ports:
      - "7419:7419"
    environment:
      SG_TLS_SELF_SIGNED: "1"
      SG_TLS_HOSTS: "localhost,127.0.0.1"
      SG_LOG_LEVEL: info
      # Or bring your own certificate:
      # SG_TLS_CERT: /data/tls/cert.pem
      # SG_TLS_KEY: /data/tls/key.pem
    volumes:
      - sgdata:/data
    restart: unless-stopped
volumes:
  sgdata: {}
```

`scripts/docker-smoke.sh`:
```bash
#!/usr/bin/env bash
# Builds the sg-server image, runs it with a self-signed cert, and runs the
# end-to-end smoke test against it from the host.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-sgit-server:smoke}
NAME=sgit-smoke-$$

docker build -t "$IMAGE" .
docker run -d --name "$NAME" -e SG_TLS_SELF_SIGNED=1 -p 127.0.0.1::7419 "$IMAGE" >/dev/null
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

status=starting
for _ in $(seq 1 30); do
  status=$(docker inspect -f '{{.State.Health.Status}}' "$NAME")
  [ "$status" = healthy ] && break
  sleep 2
done
if [ "$status" != healthy ]; then
  echo "container not healthy: $status" >&2
  docker logs "$NAME" >&2
  exit 1
fi

PORT=$(docker port "$NAME" 7419/tcp | head -1 | sed 's/.*://')
TOKEN=$(docker exec "$NAME" cat /data/admin-token)
SG_SMOKE_ADDR="127.0.0.1:${PORT}" SG_SMOKE_TOKEN="${TOKEN}" go test -tags docker -count=1 ./internal/smoke/
echo "docker smoke OK"
```

```bash
chmod 0755 docker-entrypoint.sh scripts/docker-smoke.sh
```

- [ ] **Step 3: Run the smoke test**

Run: `./scripts/docker-smoke.sh`
Expected: the image builds, the container reports `healthy`, `ok  github.com/hermes98761234/sgit/internal/smoke`, and the script prints `docker smoke OK`.

Also confirm the normal test run still skips the smoke package: `go test ./internal/smoke/` should report `no test files`, because of the build tag.

- [ ] **Step 4: CI docker job and README**

Append to `.github/workflows/ci.yml`, under `jobs:` at the same indentation as `test:`:
```yaml
  docker:
    runs-on: ubuntu-24.04
    needs: test
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: ./scripts/docker-smoke.sh
```

Replace `README.md` with:
````markdown
# sgit

Scion-style source control on top of git:

- Thin, always-online clients with no `.git`.
- Commits and lands run on the server, with compare-and-swap on `main`.
- A shared object cache on each machine for parallel agents.
- Bare git repos on the server are the source of truth.

Inspired by Anthropic's [Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control).

> **Status:** phase 1 (server core and Docker) is implemented. The CLI (`sg`), daemon (`sgd`) and GitHub mirroring are next. See the [design spec](docs/superpowers/specs/2026-10-14-sgit-design.md).

## Run the server with Docker

```bash
docker compose up -d
docker compose exec sg-server cat /data/admin-token   # initial admin token
docker compose exec sg-server sg-server health        # SERVING
```

- The server listens on `:7419` (gRPC over TLS).
- With `SG_TLS_SELF_SIGNED=1`, a certificate for `SG_TLS_HOSTS` is generated into the volume at `/data/tls/cert.pem`. Clients should pin this certificate.
- To use your own certificate instead, set `SG_TLS_CERT` and `SG_TLS_KEY` and mount the files.

| Variable | Default | Meaning |
|---|---|---|
| `SG_DATA_DIR` | `/data` | repos, `sg.db`, `admin-token`, `tls/` |
| `SG_LISTEN` | `:7419` | listen address |
| `SG_TLS_CERT` / `SG_TLS_KEY` | — | PEM certificate and key |
| `SG_TLS_SELF_SIGNED` | `0` | `1` = generate a self-signed certificate on first start |
| `SG_TLS_HOSTS` | `localhost,127.0.0.1` | SANs for the self-signed certificate |
| `SG_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `SG_MAX_BLOB_MB` | `100` | max size of one file |

All users on a machine share the object cache, which is fine for a trusted team (see spec §7).

## Run without Docker

Requires git ≥ 2.40.

```bash
make build
./dist/sg-server init --data ./sgdata
./dist/sg-server gen-cert --out-cert sgdata/cert.pem --out-key sgdata/key.pem
./dist/sg-server serve --data ./sgdata --tls-cert sgdata/cert.pem --tls-key sgdata/key.pem
```

## Development

```bash
make gen     # regenerate protobuf code (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
make test    # go test -race ./...
make smoke   # build the image and run the end-to-end test against it
```

## License

MIT
````

- [ ] **Step 5: Final verification**

Run:
```bash
gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./... && ./scripts/docker-smoke.sh
```
Expected: no gofmt output, vet and staticcheck are clean, all packages `ok`, and the script prints `docker smoke OK`.

- [ ] **Step 6: Commit and check CI**

```bash
node "$PUSH" . sgit "feat(docker): server image, compose, entrypoint with self-signed TLS, smoke test, CI"
sleep 5; gh run list -R hermes98761234/sgit -L 1
gh run watch -R hermes98761234/sgit "$(gh run list -R hermes98761234/sgit -L 1 --json databaseId --jq '.[0].databaseId')" --exit-status
```
Expected: both the `test` and `docker` jobs succeed.
