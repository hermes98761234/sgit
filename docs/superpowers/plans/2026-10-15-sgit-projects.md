# sgit projects Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace per-repo client access with **projects**: named views made of folders from one or more repos, with per-project `read`/`write` grants, aliases, read-only mounts, and a hard isolation boundary. Clients only ever see projects; repo names, source paths and source commit SHAs never reach them.

**Architecture:**
- Every project has a derived bare repo `<data>/projects/<id>.git` with its own object store. A new `internal/project` engine translates first-parent `main` commits of the mounted repos into linear project commits (unchanged subtree objects, copied metadata), ordered across repos by a per-repo land sequence, and catches up lazily under a per-project lock before every project RPC.
- All client RPCs (`sgit.v1.Project`, the renamed `Repo` service) are served from the project repo. Writes are drafts in the project repo; landing maps project paths to `<repo>:<folder>` paths, builds one source commit per repo on the recorded source base, lands it with the existing CAS/rebase loop, and catches the project up. Cross-repo lands are best effort, repo by repo.
- `auth.Authorize` decides per (user, project). Unknown and inaccessible projects produce one byte-identical NotFound. sgd keys its access memo by (token, project) and confirms every cache hit with one batched `HasObjects(project, shas)`.
- SQLite gains `projects`, `project_aliases`, `project_mounts`, `project_commits`, `project_commit_sources`, `project_cursors` and `land_seq`; `acl` and `drafts` become per project.

**Tech Stack:** Go 1.27, git plumbing (`rev-list --objects`, `pack-objects`, `index-pack`, `mktree`, `hash-object -t commit`, `cat-file --batch-check`, `diff-tree`), gRPC/protobuf (`make gen`), SQLite (`modernc.org/sqlite`), zstd, Docker.

**Spec:** `docs/superpowers/specs/2026-10-15-sgit-projects-design.md` (builds on `docs/superpowers/specs/2026-10-14-sgit-design.md`).

## Global Constraints

- **Shell setup.** Every command assumes `cd <repo checkout> && export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/protoc/bin`. If a build fails with ENOSPC, use `export GOCACHE=/dev/shm/gocache` and remove it afterwards.
- **Git checkout.** The project dir is a git checkout on `main`. Each task ends with `git add -A && git commit -m "<message from the task>"`. The commit identity `No name <hermes98761234@tutamail.com>` is already configured; do not change it. Do not push.
- **No real names anywhere** (code, tests, docs, commit messages, LICENSE). Test users are `admin`, `alice`, `bob`, `carol`, `eve`; test signatures use `<name>@sgit.local`.
- **Names.** Project names, aliases and repo names all match `^[a-z0-9][a-z0-9._-]{0,63}$`. Project names and aliases share one namespace.
- **Project reference** in any request: a name, an alias, or a decimal id (`strconv.FormatInt(id, 10) == ref`). Resolution order: name, then alias, then id.
- **Existence hiding.** An unknown project and one the caller cannot read return the identical error `status.Errorf(codes.NotFound, "project %q not found or no access", ref)` (`auth.ProjectNotFound(ref)`); the CLI maps it to exit code 2. PermissionDenied only goes to callers who can read the project.
- **Write errors.** Path outside every mount, or equal to a mount path: InvalidArgument `path outside project: <path>`. Path inside a read-only mount: PermissionDenied `read-only path: <path>` (admins included).
- **Storage.** Derived repo `<data>/projects/<id>.git`, its own object store (no alternates, no shared packs). Project ids use `INTEGER PRIMARY KEY AUTOINCREMENT` so a deleted project's id is never reused.
- **Exit codes** (unchanged): 0 ok, 1 error, 2 auth/permission/not-found-or-no-access, 3 conflict (including a partial cross-repo land), 4 CAS retries exhausted.
- **Client-facing messages** never carry a repo name, a source path or a source commit SHA. Errors raised by source-repo operations go through `Server.hide`, which logs them and returns `Internal "internal error (request <id>)"`.
- **Determinism.** Translated commits are written with `hash-object -t commit` from the source commit's raw `author`/`committer` lines and message, so rebuilding a project repo yields byte-identical SHAs.
- **Lint and tests.** `gofmt -l .` prints nothing; `go vet ./...`, `staticcheck ./...` and `go test -race -count=1 ./...` are clean before every commit.
- **testenv.Must** is `Must(f())`; never call it inside a goroutine.

### Decisions beyond the spec (for review)

- **`project_commits` stores the derivation recipe**: `kind` (`source` or `layout`), `repo` and `source_sha` (for `source`), `layout` (JSON list of `{path, repo, folder}` in effect), and for layout commits `actor_name`, `actor_email`, `actor_when`, `message`. Without it, a rebuild cannot reproduce forward-only layout commits (§5.1 requires byte-identical SHAs).
- **Land sequence table** is `land_seq(seq INTEGER PRIMARY KEY AUTOINCREMENT, repo, sha, at, UNIQUE(repo, sha))`. The row is written **before** the CAS on `main`, so a concurrent catch-up never sees a landed commit without its sequence number. Rows for commits that lost the CAS are harmless (only commits on first-parent `main` are looked up). Import records its imported `main`; create records its initial commit.
- **Ordering comparator** (§5.3): two commits that both have a sequence compare by sequence; otherwise by committer date, then repo name, then first-parent position. The merge is a k-way merge of per-repo first-parent queues, so per-repo order is always preserved.
- **An initial mount that finds no commit touching the folder** writes one layout commit ("Project layout: added `<path>/`", empty content) so the project has a `main` that can be checked out.
- **An empty folder tree counts as absent** for any mount except `.`, so an empty source repo mounted at `x` adds no `x/` entry and its empty initial commit is skipped (git trees never hold empty directories).
- **`.` mounts of a whole repo** reproduce the source history exactly (same trees, metadata and parents), so project SHAs equal source SHAs there. This is harmless: such a project contains exactly that repo. Isolation tests use folder mounts.
- **Draft refs stay** `refs/sg/drafts/<id>/head` and `/v<n>` (the spec's `refs/drafts/<id>` is descriptive). A draft's base must be a project commit (`project_commits`), otherwise InvalidArgument `base commit <sha> not found`.
- **Rebuild** happens when `<data>/projects/<id>.git` is missing but the project has commits. Open drafts of that project are marked abandoned (their objects lived only in the deleted repo).
- **A cross-repo land that fails with a non-conflict error after at least one repo landed** returns the same Aborted + `ConflictDetail{partial: true, landed, files: []}` as a conflict, so the client still rebases onto the new head and exits 3. If nothing landed, the original error is returned.
- **`LandResponse.parent`** is the new project head's parent only when the head's tree equals the draft's tree; otherwise it is empty, which makes the CLI do a full sync.
- **`ResolveRevResponse` gains `project_id` and `project_name`**, so `sg checkout` resolves an alias to an id and learns the canonical name in the existing round trip. `.sg/config` stores `project = <id>` plus a display-only `name` cache refreshed by `checkout` and `sync`.
- **`ResolveRev`** accepts any git revision syntax, but only against the project repo, plus a draft id (8 base32 chars with an existing draft head ref). Source SHAs are absent from that object store, so they give NotFound. `Log`, `Blame` and `Diff` resolve their revisions with the same function first, so a foreign SHA gives NotFound rather than Internal.
- **`HasObjects` with a foreign SHA returns `present=false`** (it never errors per SHA); `GetObjects` returns NotFound.
- **`sg project mount` on an existing path** with the same `repo:folder` only sets the read-only flag to the value given (`--read-only` present or absent), with no commit. A different source is AlreadyExists.
- **`sg project rename`** to one of the project's own aliases swaps it: the alias row is removed and the old name becomes an alias.
- **ListProjects level** for admins is `admin`.
- **CLI `--json` for a partial land** keeps the existing boolean `"landed": false` and adds `"partial": true` and `"landed_mounts": [...]` (the spec's `landed` list lives in `ConflictDetail`; reusing the JSON key would break agents that read the boolean).
- **The not-found error echoes the caller's reference** (`project "<ref>" …`), so the two cases are byte-identical for the same reference; invalid names get the same shape. The CLI maps NotFound with the suffix ` not found or no access` to exit 2.
- **Existing databases** with the old per-repo `acl` or `drafts.repo` are migrated by dropping those two tables on `Open` (no production data exists).

## Review Focus

These five failure modes would hurt users most; each is pinned by a test in the named task.
1. **A warm sgd cache serves another project's object.** Pinned in Task 11 (`TestCacheHitNeedsProjectConfirmation`) and Task 12 (`TestIsolationThroughWarmDaemon`).
2. **A project RPC reaches an object or commit outside the mounts** (a source SHA, an unmounted folder's blob, a commit that only touched unmounted folders). Pinned in Task 4 (`TestProjectRepoHoldsOnlyMountedObjects`) and Task 12 (`TestForeignSHAsAreNotFound`, `TestUnmountedOnlyCommitNotInLog`).
3. **Existence leaks through error differences** between unknown, no-access, invalid-name and decimal-id references, on reads, writes and uploads. Pinned in Task 7 (`TestUnknownAndNoAccessAreIdentical`) and Task 12 (`TestCLINotFoundIsByteIdentical`).
4. **A partial cross-repo land lands a mount twice or drops changes on retry.** Pinned in Task 8 (`TestPartialLandThenRetryLandsRest`, `TestConcurrentLandsThroughTwoProjects`).
5. **A draft made before a revoke, or touching a read-only mount, still lands.** Pinned in Task 7 (`TestRevokedGrantBlocksLand`, `TestReadOnlyPathRejected`).

---

## File Structure

```
internal/gitx/project.go            (Task 1) Subtrees, FirstParents, ComposeTree, CopyObjects, WriteCommit, FormatIdent, DiffTree, MountTree, FPCommit
internal/gitx/commit.go             (Task 1) Commit.AuthorRaw / CommitterRaw
internal/gitx/project_test.go       (Task 1)
internal/store/store.go             (Task 2, Task 7) schema, migrate
internal/store/projects.go          (Task 2) projects, aliases, mounts, project commits, cursors, land_seq
internal/store/projects_test.go     (Task 2)
internal/store/acl.go               (Task 7) per-project ACL and drafts (moved out of store.go)
internal/server/land.go             (Task 3, 7, 8) land sequence, landSource, project land, partial land
internal/server/repos.go            (Task 3, 6) create/import record land_seq; Source(); delete refused while mounted
internal/project/engine.go          (Task 4) Engine, Handle, Get, CatchUp, Mount, Unmount, Rebuild, Delete
internal/project/order.go           (Task 4) k-way merge
internal/project/engine_test.go     (Task 4)
proto/sgit/v1/sgit.proto            (Task 5) Project service, project fields, project admin RPCs; gen/ regenerated
internal/server/admin_projects.go   (Task 6) project admin RPCs
internal/server/admin_projects_test.go (Task 6)
internal/auth/auth.go               (Task 7) Authorize per project, ErrNoAccess, ProjectNotFound
internal/server/server.go           (Task 6, 7) projects engine wiring, open(), hide()
internal/server/paths.go            (Task 7) mount mapping
internal/server/read.go, drafts.go  (Task 7)
internal/server/projects_test.go    (Task 7)
internal/server/landmulti_test.go   (Task 8)
internal/testenv/testenv.go         (Task 5, 6, 7) Project client, CreateProject, Mount, CreateRepoProject, ProjectGit
internal/cli/project.go             (Task 9) sg project …, sg acl … by project
internal/cli/projects.go            (Task 10) sg projects
internal/cli/*.go                   (Task 10) --project, config id, partial land output, exit 2 mapping
internal/wc/wc.go                   (Task 10) Config{Server, Project int64, Name, Sparse}
internal/daemon/objects.go          (Task 11) memo by project, batched confirmation
internal/server/isolation_test.go   (Task 12)
internal/cli/isolation_test.go      (Task 12)
internal/smoke/docker_smoke_test.go, scripts/docker-smoke.sh (Task 13)
internal/cli/monkey_test.go, internal/cli/bench_test.go       (Task 13)
README.md, docs/*.md, skills/*/SKILL.md                       (Task 14)
```

---

### Task 1: gitx helpers for projection

**Files:**
- Create: `internal/gitx/project.go`, `internal/gitx/project_test.go`
- Modify: `internal/gitx/commit.go` (raw identity lines)

**Interfaces:**
- Consumes: `(*Repo).run`, `runGit`, `ReadCommit`, `ObjectInfo`, `ErrNotFound`, `ErrInvalid`, `EmptyTree`, test helpers `newRepo`, `commitChanges`, `blob`, `testSig`.
- Produces:
  ```go
  type FPCommit struct{ SHA string; CommitterUnix int64 }
  type MountTree struct{ Path, Tree string } // Path "." or clean relative path; Tree "" = absent
  func (r *Repo) FirstParents(ctx context.Context, tip, stop string) ([]FPCommit, error)   // oldest first; stop "" = full history
  func (r *Repo) Subtrees(ctx context.Context, commits []string, folder string) ([]string, error) // "" where missing or not a tree
  func (r *Repo) ComposeTree(ctx context.Context, mounts []MountTree) (string, error)
  func CopyObjects(ctx context.Context, src, dst *Repo, want, have []string) (int, error)
  func (r *Repo) WriteCommit(ctx context.Context, tree string, parents []string, author, committer, message string) (string, error)
  func FormatIdent(s Signature) string // "Name <email> <unix> +0000"
  func (r *Repo) DiffTree(ctx context.Context, a, b string) ([]Change, error)
  // Commit gains: AuthorRaw, CommitterRaw string (text after "author " / "committer ")
  ```

- [ ] **Step 1: Write the failing tests** in `internal/gitx/project_test.go`:

```go
package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFirstParentsAndSubtrees(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c0, _ := r.ReadRef(ctx, MainRef)
	c1 := commitChanges(t, r, []Change{{Path: "a/x.txt", Op: OpAdd, BlobSHA: blob(t, r, "x1")}})
	c2 := commitChanges(t, r, []Change{{Path: "b/y.txt", Op: OpAdd, BlobSHA: blob(t, r, "y1")}})
	c3 := commitChanges(t, r, []Change{{Path: "a/x.txt", Op: OpModify, BlobSHA: blob(t, r, "x2")}})

	all, err := r.FirstParents(ctx, c3, "")
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for _, c := range all {
		shas = append(shas, c.SHA)
		if c.CommitterUnix != testSig.When.Unix() {
			t.Fatalf("committer time %d", c.CommitterUnix)
		}
	}
	if !reflect.DeepEqual(shas, []string{c0, c1, c2, c3}) {
		t.Fatalf("FirstParents = %v", shas)
	}
	tail, err := r.FirstParents(ctx, c3, c1)
	if err != nil || len(tail) != 2 || tail[0].SHA != c2 || tail[1].SHA != c3 {
		t.Fatalf("FirstParents(c3, c1) = %v, %v", tail, err)
	}
	if none, err := r.FirstParents(ctx, c3, c3); err != nil || len(none) != 0 {
		t.Fatalf("FirstParents(c3, c3) = %v, %v", none, err)
	}

	trees, err := r.Subtrees(ctx, []string{c0, c1, c2, c3}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if trees[0] != "" || trees[1] == "" || trees[2] != trees[1] || trees[3] == trees[2] {
		t.Fatalf("Subtrees(a) = %v", trees)
	}
	root, err := r.Subtrees(ctx, []string{c0}, ".")
	if err != nil || root[0] != EmptyTree {
		t.Fatalf("Subtrees(.) = %v, %v", root, err)
	}
	// A file is not a folder.
	if f, err := r.Subtrees(ctx, []string{c3}, "a/x.txt"); err != nil || f[0] != "" {
		t.Fatalf("Subtrees(file) = %v, %v", f, err)
	}
}

func TestComposeTree(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c := commitChanges(t, r, []Change{
		{Path: "src/a.go", Op: OpAdd, BlobSHA: blob(t, r, "a")},
		{Path: "docs/r.md", Op: OpAdd, BlobSHA: blob(t, r, "r")},
	})
	src := must(r.Subtrees(ctx, []string{c}, "src"))[0]
	docs := must(r.Subtrees(ctx, []string{c}, "docs"))[0]
	tree, err := r.ComposeTree(ctx, []MountTree{{Path: "lib/core", Tree: src}, {Path: "lib/doc", Tree: docs}, {Path: "gone", Tree: ""}})
	if err != nil {
		t.Fatal(err)
	}
	pc := must(r.CommitTree(ctx, tree, nil, "p", testSig, testSig))
	files := must(r.ListFiles(ctx, pc))
	if len(files) != 2 || files["lib/core/a.go"].SHA == "" || files["lib/doc/r.md"].SHA == "" {
		t.Fatalf("composed files = %v", files)
	}
	if got := must(r.ComposeTree(ctx, []MountTree{{Path: ".", Tree: src}})); got != src {
		t.Fatalf(". mount = %s, want %s", got, src)
	}
	if got := must(r.ComposeTree(ctx, []MountTree{{Path: ".", Tree: ""}})); got != EmptyTree {
		t.Fatalf("missing . mount = %s, want empty tree", got)
	}
	if got := must(r.ComposeTree(ctx, nil)); got != EmptyTree {
		t.Fatalf("no mounts = %s, want empty tree", got)
	}
}

func TestCopyObjectsCopiesOnlyWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	c1 := commitChanges(t, src, []Change{
		{Path: "pub/a.txt", Op: OpAdd, BlobSHA: blob(t, src, "public a")},
		{Path: "secret/k.txt", Op: OpAdd, BlobSHA: blob(t, src, "secret k")},
	})
	t1 := must(src.Subtrees(ctx, []string{c1}, "pub"))[0]
	dstDir := filepath.Join(t.TempDir(), "p.git")
	if err := InitBare(ctx, dstDir); err != nil {
		t.Fatal(err)
	}
	dst := Open(dstDir)
	defer dst.Close()
	n, err := CopyObjects(ctx, src, dst, []string{t1}, nil)
	if err != nil || n != 2 { // tree + one blob
		t.Fatalf("CopyObjects = %d, %v; want 2", n, err)
	}
	secret := must(src.Subtrees(ctx, []string{c1}, "secret"))[0]
	if _, _, err := dst.ObjectInfo(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unmounted tree copied: %v", err)
	}
	c2 := commitChanges(t, src, []Change{{Path: "pub/b.txt", Op: OpAdd, BlobSHA: blob(t, src, "public b")}})
	t2 := must(src.Subtrees(ctx, []string{c2}, "pub"))[0]
	n, err = CopyObjects(ctx, src, dst, []string{t2}, []string{t1})
	if err != nil || n != 2 { // new pub tree + b.txt; a.txt excluded by have
		t.Fatalf("incremental CopyObjects = %d, %v; want 2", n, err)
	}
	if n, err := CopyObjects(ctx, src, dst, []string{t2}, nil); err != nil || n != 0 {
		t.Fatalf("repeat CopyObjects = %d, %v; want 0", n, err)
	}
	if n, err := CopyObjects(ctx, src, dst, nil, nil); err != nil || n != 0 {
		t.Fatalf("empty CopyObjects = %d, %v", n, err)
	}
	// Blobs can be copied directly (used to move uploaded blobs to a source repo).
	b := blob(t, dst, "uploaded")
	if n, err := CopyObjects(ctx, dst, src, []string{b}, nil); err != nil || n != 1 {
		t.Fatalf("blob CopyObjects = %d, %v", n, err)
	}
}

func TestWriteCommitIsDeterministic(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c1 := commitChanges(t, r, []Change{{Path: "a", Op: OpAdd, BlobSHA: blob(t, r, "a")}})
	sc := must(r.ReadCommit(ctx, c1))
	if !strings.HasPrefix(sc.AuthorRaw, "alice <alice@sgit.local> 1700000000 ") {
		t.Fatalf("AuthorRaw = %q", sc.AuthorRaw)
	}
	// Re-writing the same commit from its raw parts yields the same SHA.
	again, err := r.WriteCommit(ctx, sc.Tree, sc.Parents, sc.AuthorRaw, sc.CommitterRaw, sc.Message)
	if err != nil || again != c1 {
		t.Fatalf("WriteCommit = %s, %v; want %s", again, err, c1)
	}
	sig := Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000500, 0).In(time.FixedZone("x", 3600))}
	if got := FormatIdent(sig); got != "admin <admin@sgit.local> 1700000500 +0000" {
		t.Fatalf("FormatIdent = %q", got)
	}
	if _, err := r.WriteCommit(ctx, sc.Tree, nil, "bad\nident", sc.CommitterRaw, "m\n"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("newline in ident: %v", err)
	}
}

func TestDiffTree(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base := commitChanges(t, r, []Change{
		{Path: "keep", Op: OpAdd, BlobSHA: blob(t, r, "k")},
		{Path: "mod", Op: OpAdd, BlobSHA: blob(t, r, "m1")},
		{Path: "del", Op: OpAdd, BlobSHA: blob(t, r, "d")},
	})
	m2 := blob(t, r, "m2")
	n := blob(t, r, "n")
	head := commitChanges(t, r, []Change{
		{Path: "mod", Op: OpModify, BlobSHA: m2, Mode: "100755"},
		{Path: "del", Op: OpDelete},
		{Path: "dir/new", Op: OpAdd, BlobSHA: n},
	})
	got, err := r.DiffTree(ctx, base, head)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "del", Op: OpDelete},
		{Path: "dir/new", Op: OpAdd, BlobSHA: n, Mode: "100644"},
		{Path: "mod", Op: OpModify, BlobSHA: m2, Mode: "100755"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffTree = %+v", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gitx/ -run 'FirstParents|ComposeTree|CopyObjects|WriteCommit|DiffTree' -count=1`
Expected: FAIL to compile (`r.FirstParents undefined`, `undefined: MountTree`, `sc.AuthorRaw undefined`, …).

- [ ] **Step 3: Add raw identity lines to `Commit`.** In `internal/gitx/commit.go`, add two fields and set them in `parseCommit` where the `author`/`committer` header lines are parsed (the value is the text after the first space, exactly as stored):

```go
type Commit struct {
	SHA          string
	Tree         string
	Parents      []string
	Author       Signature
	Committer    Signature
	AuthorRaw    string // header value after "author ", byte for byte
	CommitterRaw string // header value after "committer ", byte for byte
	Message      string
}
```

```go
		case "author":
			c.AuthorRaw = v
			if c.Author, err = parseSig(v); err != nil {
				return Commit{}, err
			}
		case "committer":
			c.CommitterRaw = v
			if c.Committer, err = parseSig(v); err != nil {
				return Commit{}, err
			}
```

- [ ] **Step 4: Implement `internal/gitx/project.go`:**

```go
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FPCommit is one commit on a first-parent chain.
type FPCommit struct {
	SHA           string
	CommitterUnix int64
}

// MountTree places Tree at Path in a composed root tree. Tree "" is absent.
type MountTree struct {
	Path string
	Tree string
}

// FirstParents lists the first-parent chain of tip, oldest first, stopping
// before stop and its ancestors. stop "" lists the whole chain.
func (r *Repo) FirstParents(ctx context.Context, tip, stop string) ([]FPCommit, error) {
	if !IsSHA(tip) || (stop != "" && !IsSHA(stop)) {
		return nil, fmt.Errorf("%w: first-parents %q %q", ErrInvalid, tip, stop)
	}
	args := []string{"log", "--first-parent", "--reverse", "--no-show-signature", "--format=%H %ct", tip}
	if stop != "" {
		args = append(args, "^"+stop)
	}
	out, err := r.run(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var list []FPCommit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		sha, ts, ok := strings.Cut(line, " ")
		n, perr := strconv.ParseInt(ts, 10, 64)
		if !ok || perr != nil || !IsSHA(sha) {
			return nil, fmt.Errorf("gitx: bad log line %q", line)
		}
		list = append(list, FPCommit{SHA: sha, CommitterUnix: n})
	}
	return list, nil
}

// Subtrees returns, for each commit, the tree at folder ("." = root tree),
// or "" when folder is missing or not a directory. One git process serves
// the whole batch.
func (r *Repo) Subtrees(ctx context.Context, commits []string, folder string) ([]string, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	if strings.ContainsAny(folder, "\n\x00") {
		return nil, fmt.Errorf("%w: folder %q", ErrInvalid, folder)
	}
	var in bytes.Buffer
	for _, c := range commits {
		if !IsSHA(c) {
			return nil, fmt.Errorf("%w: commit %q", ErrInvalid, c)
		}
		if folder == "." {
			fmt.Fprintf(&in, "%s^{tree}\n", c)
		} else {
			fmt.Fprintf(&in, "%s:%s\n", c, folder)
		}
	}
	out, err := r.run(ctx, &in, nil, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(commits) {
		return nil, fmt.Errorf("gitx: cat-file returned %d lines for %d commits", len(lines), len(commits))
	}
	res := make([]string, len(commits))
	for i, l := range lines {
		if f := strings.Fields(l); len(f) == 2 && f[1] == "tree" && IsSHA(f[0]) {
			res[i] = f[0]
		}
	}
	return res, nil
}

type dirNode struct {
	tree     string // set when a mount sits exactly here
	children map[string]*dirNode
}

// ComposeTree builds a root tree from mounts: every non-empty Tree is placed
// unchanged at its Path; intermediate directories are created with mktree.
// A "." mount is the root itself. All trees must already exist in r.
func (r *Repo) ComposeTree(ctx context.Context, mounts []MountTree) (string, error) {
	root := &dirNode{children: map[string]*dirNode{}}
	for _, m := range mounts {
		if m.Path == "." {
			if m.Tree == "" {
				return r.emptyTree(ctx)
			}
			return m.Tree, nil
		}
		if m.Tree == "" {
			continue
		}
		n := root
		for _, seg := range strings.Split(m.Path, "/") {
			c, ok := n.children[seg]
			if !ok {
				c = &dirNode{children: map[string]*dirNode{}}
				n.children[seg] = c
			}
			n = c
		}
		n.tree = m.Tree
	}
	return r.writeNode(ctx, root)
}

func (r *Repo) writeNode(ctx context.Context, n *dirNode) (string, error) {
	if n.tree != "" {
		return n.tree, nil
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	var in bytes.Buffer
	for _, name := range names {
		sha, err := r.writeNode(ctx, n.children[name])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&in, "040000 tree %s\t%s\x00", sha, name)
	}
	out, err := r.run(ctx, &in, nil, "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) emptyTree(ctx context.Context) (string, error) {
	out, err := r.run(ctx, strings.NewReader(""), nil, "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CopyObjects copies into dst every object reachable from want (commits,
// trees or blobs) that is not reachable from have and that dst lacks. have
// must exist in src. It returns the number of objects copied.
func CopyObjects(ctx context.Context, src, dst *Repo, want, have []string) (int, error) {
	if len(want) == 0 {
		return 0, nil
	}
	args := []string{"rev-list", "--objects", "--no-object-names"}
	for _, w := range want {
		if !IsSHA(w) {
			return 0, fmt.Errorf("%w: want %q", ErrInvalid, w)
		}
		args = append(args, w)
	}
	if len(have) > 0 {
		args = append(args, "--not")
		for _, h := range have {
			if !IsSHA(h) {
				return 0, fmt.Errorf("%w: have %q", ErrInvalid, h)
			}
			args = append(args, h)
		}
	}
	out, err := src.run(ctx, nil, nil, args...)
	if err != nil {
		return 0, err
	}
	var list bytes.Buffer
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		sha := strings.TrimSpace(sc.Text())
		if sha == "" {
			continue
		}
		switch _, _, err := dst.ObjectInfo(ctx, sha); {
		case err == nil:
			continue
		case !errors.Is(err, ErrNotFound):
			return 0, err
		}
		list.WriteString(sha + "\n")
		n++
	}
	if n == 0 {
		return 0, nil
	}
	pack, err := src.run(ctx, &list, nil, "pack-objects", "--stdout", "-q")
	if err != nil {
		return 0, err
	}
	if _, err := dst.run(ctx, bytes.NewReader(pack), nil, "index-pack", "--stdin", "-q"); err != nil {
		return 0, err
	}
	return n, nil
}

// FormatIdent renders a signature as a raw ident line in UTC.
func FormatIdent(s Signature) string {
	return fmt.Sprintf("%s <%s> %d +0000", s.Name, s.Email, s.When.Unix())
}

// WriteCommit writes a commit object from raw parts (ident lines as stored
// after "author "/"committer "), so a copy of a commit is byte-identical.
func (r *Repo) WriteCommit(ctx context.Context, tree string, parents []string, author, committer, message string) (string, error) {
	if !IsSHA(tree) || strings.ContainsAny(author+committer, "\n\x00") || author == "" || committer == "" {
		return "", fmt.Errorf("%w: commit header", ErrInvalid)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		if !IsSHA(p) {
			return "", fmt.Errorf("%w: parent %q", ErrInvalid, p)
		}
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s", author, committer, message)
	out, err := r.run(ctx, strings.NewReader(b.String()), nil, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DiffTree lists file-level changes from commit a to commit b, sorted by
// path, as Changes that BuildTree can apply on a.
func (r *Repo) DiffTree(ctx context.Context, a, b string) ([]Change, error) {
	if !IsSHA(a) || !IsSHA(b) {
		return nil, fmt.Errorf("%w: diff-tree %q %q", ErrInvalid, a, b)
	}
	out, err := r.run(ctx, nil, nil, "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", a, b)
	if err != nil {
		return nil, err
	}
	recs := strings.Split(string(out), "\x00")
	var res []Change
	for i := 0; i+1 < len(recs); i += 2 {
		meta, p := recs[i], recs[i+1]
		f := strings.Fields(strings.TrimPrefix(meta, ":")) // oldmode newmode oldsha newsha status
		if len(f) != 5 {
			return nil, fmt.Errorf("gitx: bad diff-tree record %q", meta)
		}
		switch f[4][0] {
		case 'D':
			res = append(res, Change{Path: p, Op: OpDelete})
		case 'A':
			res = append(res, Change{Path: p, Op: OpAdd, BlobSHA: f[3], Mode: f[1]})
		default: // M, T
			res = append(res, Change{Path: p, Op: OpModify, BlobSHA: f[3], Mode: f[1]})
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Path < res[j].Path })
	return res, nil
}
```

- [ ] **Step 5: Run the gitx tests**

Run: `go test -race -count=1 ./internal/gitx/`
Expected: PASS.

- [ ] **Step 6: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "gitx: subtrees, first-parent lists, tree composition, object copy, raw commits, diff-tree"
```

---

### Task 2: Store: project tables

**Files:**
- Create: `internal/store/projects.go`, `internal/store/projects_test.go`
- Modify: `internal/store/store.go` (append the new tables to `schema`)

**Interfaces:**
- Consumes: `Store.db`, `isUnique`, `ErrNotFound`, `ErrExists`, the tx pattern (`BeginTx` + `defer tx.Rollback() //nolint:errcheck`).
- Produces:
  ```go
  var ErrMounted = errors.New("store: repo is mounted by a project")
  type Project struct{ ID int64; Name string; Aliases []string; CreatedAt time.Time }
  type Mount struct{ Path, Repo, Folder string; ReadOnly bool; AddedAt time.Time }
  type LayoutMount struct{ Path string `json:"path"`; Repo string `json:"repo"`; Folder string `json:"folder"` }
  type ProjectCommit struct {
  	SHA string; Seq int64; Kind string /* "source" | "layout" */
  	Repo, SourceSHA string             // kind source
  	Layout []LayoutMount
  	ActorName, ActorEmail string; ActorWhen int64; Message string // kind layout
  	Sources map[string]string // repo -> source main at this commit (every mounted repo)
  }
  type MountChange struct{ Add *Mount; Remove string }
  const KindSource, KindLayout = "source", "layout"
  func (s *Store) CreateProject(ctx context.Context, name string, at time.Time) (Project, error)
  func (s *Store) ResolveProject(ctx context.Context, ref string) (Project, error)
  func (s *Store) GetProject(ctx context.Context, id int64) (Project, error)
  func (s *Store) ListProjects(ctx context.Context) ([]Project, error)
  func (s *Store) RenameProject(ctx context.Context, id int64, newName string) error
  func (s *Store) AddProjectAlias(ctx context.Context, id int64, alias string) error
  func (s *Store) RemoveProjectAlias(ctx context.Context, alias string) (int64, error)
  func (s *Store) DeleteProjectRows(ctx context.Context, id int64) error   // Task 7 extends with acl + drafts
  func (s *Store) Mounts(ctx context.Context, id int64) ([]Mount, error)   // ordered by path
  func (s *Store) SetMountReadOnly(ctx context.Context, id int64, path string, ro bool) error
  func (s *Store) ProjectsMounting(ctx context.Context, repo string) ([]string, error) // project names
  func (s *Store) RecordProjectCommits(ctx context.Context, id int64, commits []ProjectCommit, cursors map[string]string, mc *MountChange) error
  func (s *Store) ProjectHead(ctx context.Context, id int64) (sha string, seq int64, err error) // "", 0 when none
  func (s *Store) ProjectCommits(ctx context.Context, id int64) ([]ProjectCommit, error)        // by seq
  func (s *Store) ProjectSource(ctx context.Context, id int64, projectSHA, repo string) (string, error)
  func (s *Store) IsProjectCommit(ctx context.Context, id int64, sha string) (bool, error)
  func (s *Store) ProjectCursors(ctx context.Context, id int64) (map[string]string, error)
  func (s *Store) RecordLand(ctx context.Context, repo, sha string, at time.Time) error
  func (s *Store) LandSeqs(ctx context.Context, repo string, shas []string) (map[string]int64, error)
  ```

- [ ] **Step 1: Write the failing tests** in `internal/store/projects_test.go` (package `store`, reuses `open(t)` from `store_test.go`):

```go
package store

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"
)

var t0 = time.Unix(1700000000, 0).UTC()

func TestProjectNamesAndAliasesShareANamespace(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	web, err := s.CreateProject(ctx, "web", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, "web", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := s.AddProjectAlias(ctx, web.ID, "site"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, "site", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("name equal to an alias: %v", err)
	}
	api, _ := s.CreateProject(ctx, "api", t0)
	if err := s.AddProjectAlias(ctx, api.ID, "web"); !errors.Is(err, ErrExists) {
		t.Fatalf("alias equal to a name: %v", err)
	}
	for _, ref := range []string{"web", "site", strconv.FormatInt(web.ID, 10)} {
		p, err := s.ResolveProject(ctx, ref)
		if err != nil || p.ID != web.ID || p.Name != "web" || !reflect.DeepEqual(p.Aliases, []string{"site"}) {
			t.Fatalf("ResolveProject(%q) = %+v, %v", ref, p, err)
		}
	}
	for _, ref := range []string{"nope", "0" + strconv.FormatInt(web.ID, 10), "+1", ""} {
		if _, err := s.ResolveProject(ctx, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ResolveProject(%q) = %v, want ErrNotFound", ref, err)
		}
	}
	// Rename keeps the old name as an alias; renaming onto an own alias swaps.
	if err := s.RenameProject(ctx, web.ID, "www"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx, web.ID)
	if p.Name != "www" || !reflect.DeepEqual(p.Aliases, []string{"site", "web"}) {
		t.Fatalf("after rename: %+v", p)
	}
	if err := s.RenameProject(ctx, web.ID, "site"); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProject(ctx, web.ID)
	if p.Name != "site" || !reflect.DeepEqual(p.Aliases, []string{"web", "www"}) {
		t.Fatalf("after swap: %+v", p)
	}
	if err := s.RenameProject(ctx, web.ID, "api"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto another project: %v", err)
	}
	if id, err := s.RemoveProjectAlias(ctx, "www"); err != nil || id != web.ID {
		t.Fatalf("RemoveProjectAlias = %d, %v", id, err)
	}
	if _, err := s.RemoveProjectAlias(ctx, "site"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing a canonical name as alias: %v", err)
	}
}

func TestProjectIDsAreNeverReused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	a, _ := s.CreateProject(ctx, "a", t0)
	if err := s.DeleteProjectRows(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateProject(ctx, "b", t0)
	if b.ID == a.ID {
		t.Fatalf("project id %d reused after delete", a.ID)
	}
	if _, err := s.GetProject(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted project still readable: %v", err)
	}
}

func TestProjectCommitsCursorsAndMounts(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CreateRepo(ctx, "core", ""); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "web", t0)
	m := Mount{Path: "lib", Repo: "core", Folder: "src", AddedAt: t0}
	c1 := ProjectCommit{SHA: sha('1'), Seq: 1, Kind: KindSource, Repo: "core", SourceSHA: sha('a'),
		Layout: []LayoutMount{{Path: "lib", Repo: "core", Folder: "src"}}, Sources: map[string]string{"core": sha('a')}}
	if err := s.RecordProjectCommits(ctx, p.ID, []ProjectCommit{c1}, map[string]string{"core": sha('a')}, &MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	c2 := ProjectCommit{SHA: sha('2'), Seq: 2, Kind: KindLayout, Layout: nil, ActorName: "admin", ActorEmail: "admin@sgit.local",
		ActorWhen: 1700000100, Message: "Project layout: removed `lib/`\n", Sources: map[string]string{}}
	if err := s.RecordProjectCommits(ctx, p.ID, []ProjectCommit{c2}, map[string]string{}, &MountChange{Remove: "lib"}); err != nil {
		t.Fatal(err)
	}
	if head, seq, err := s.ProjectHead(ctx, p.ID); err != nil || head != sha('2') || seq != 2 {
		t.Fatalf("ProjectHead = %s %d %v", head, seq, err)
	}
	if src, err := s.ProjectSource(ctx, p.ID, sha('1'), "core"); err != nil || src != sha('a') {
		t.Fatalf("ProjectSource = %s, %v", src, err)
	}
	if _, err := s.ProjectSource(ctx, p.ID, sha('2'), "core"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ProjectSource after unmount: %v", err)
	}
	if ok, _ := s.IsProjectCommit(ctx, p.ID, sha('1')); !ok {
		t.Fatal("IsProjectCommit(c1) = false")
	}
	all, err := s.ProjectCommits(ctx, p.ID)
	if err != nil || len(all) != 2 || !reflect.DeepEqual(all[0], c1) || all[1].Message != c2.Message || all[1].Kind != KindLayout {
		t.Fatalf("ProjectCommits = %+v, %v", all, err)
	}
	if cur, _ := s.ProjectCursors(ctx, p.ID); len(cur) != 0 {
		t.Fatalf("cursors after unmount = %v", cur)
	}
	if ms, _ := s.Mounts(ctx, p.ID); len(ms) != 0 {
		t.Fatalf("mounts after unmount = %v", ms)
	}
	// A duplicate seq or SHA rolls back the whole batch.
	if err := s.RecordProjectCommits(ctx, p.ID, []ProjectCommit{{SHA: sha('3'), Seq: 2, Kind: KindLayout, Sources: map[string]string{}}}, nil, nil); err == nil {
		t.Fatal("duplicate seq accepted")
	}
}

func TestMountsAndRepoDeleteGuard(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_ = s.CreateRepo(ctx, "core", "")
	p, _ := s.CreateProject(ctx, "web", t0)
	m := Mount{Path: "lib", Repo: "core", Folder: "src", AddedAt: t0}
	if err := s.RecordProjectCommits(ctx, p.ID, nil, map[string]string{"core": sha('a')}, &MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMountReadOnly(ctx, p.ID, "lib", true); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Mounts(ctx, p.ID)
	if len(ms) != 1 || !ms[0].ReadOnly || ms[0].Folder != "src" {
		t.Fatalf("Mounts = %+v", ms)
	}
	if names, _ := s.ProjectsMounting(ctx, "core"); !reflect.DeepEqual(names, []string{"web"}) {
		t.Fatalf("ProjectsMounting = %v", names)
	}
	if err := s.DeleteRepo(ctx, "core"); !errors.Is(err, ErrMounted) {
		t.Fatalf("DeleteRepo while mounted = %v, want ErrMounted", err)
	}
	if err := s.SetMountReadOnly(ctx, p.ID, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetMountReadOnly(missing) = %v", err)
	}
}

func TestLandSequence(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i, c := range []byte{'a', 'b', 'c'} {
		if err := s.RecordLand(ctx, "core", sha(c), t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordLand(ctx, "core", sha('a'), t0); err != nil { // idempotent
		t.Fatal(err)
	}
	seqs, err := s.LandSeqs(ctx, "core", []string{sha('c'), sha('a'), sha('z')})
	if err != nil || len(seqs) != 2 || seqs[sha('a')] >= seqs[sha('c')] {
		t.Fatalf("LandSeqs = %v, %v", seqs, err)
	}
	if other, _ := s.LandSeqs(ctx, "other", []string{sha('a')}); len(other) != 0 {
		t.Fatalf("LandSeqs leaked across repos: %v", other)
	}
}

func sha(c byte) string {
	b := make([]byte, 40)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
```

`sha('1')`, `sha('a')` etc. are syntactically valid 40-char strings; the store does not validate hex.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'Project|Mounts|LandSequence' -count=1`
Expected: FAIL to compile (`s.CreateProject undefined`, …).

- [ ] **Step 3: Add the tables** to the end of the `schema` constant in `internal/store/store.go`:

```sql
CREATE TABLE IF NOT EXISTS projects (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT UNIQUE NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS project_aliases (
  alias TEXT PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id)
);
CREATE TABLE IF NOT EXISTS project_mounts (
  project_id INTEGER NOT NULL REFERENCES projects(id),
  mount_path TEXT NOT NULL,
  repo TEXT NOT NULL REFERENCES repos(name),
  folder TEXT NOT NULL,
  read_only INTEGER NOT NULL DEFAULT 0,
  added_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, mount_path)
);
CREATE INDEX IF NOT EXISTS project_mounts_repo ON project_mounts(repo);
CREATE TABLE IF NOT EXISTS project_commits (
  project_id INTEGER NOT NULL,
  project_sha TEXT NOT NULL,
  seq INTEGER NOT NULL,
  kind TEXT NOT NULL,
  repo TEXT NOT NULL DEFAULT '',
  source_sha TEXT NOT NULL DEFAULT '',
  layout TEXT NOT NULL DEFAULT '[]',
  actor_name TEXT NOT NULL DEFAULT '',
  actor_email TEXT NOT NULL DEFAULT '',
  actor_when INTEGER NOT NULL DEFAULT 0,
  message TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, project_sha),
  UNIQUE (project_id, seq)
);
CREATE TABLE IF NOT EXISTS project_commit_sources (
  project_id INTEGER NOT NULL,
  project_sha TEXT NOT NULL,
  repo TEXT NOT NULL,
  source_sha TEXT NOT NULL,
  PRIMARY KEY (project_id, project_sha, repo)
);
CREATE TABLE IF NOT EXISTS project_cursors (
  project_id INTEGER NOT NULL,
  repo TEXT NOT NULL,
  source_sha TEXT NOT NULL,
  PRIMARY KEY (project_id, repo)
);
CREATE TABLE IF NOT EXISTS land_seq (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  repo TEXT NOT NULL,
  sha TEXT NOT NULL,
  at INTEGER NOT NULL,
  UNIQUE (repo, sha)
);
```

- [ ] **Step 4: Implement `internal/store/projects.go`.** Key parts (write the remaining simple CRUD in the same style as `store.go`):

```go
// nameTaken reports whether name is a project name or alias (inside tx).
func nameTaken(ctx context.Context, tx *sql.Tx, name string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name=?1
		UNION ALL SELECT project_id FROM project_aliases WHERE alias=?1 LIMIT 1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (s *Store) CreateProject(ctx context.Context, name string, at time.Time) (Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, taken, err := nameTaken(ctx, tx, name); err != nil {
		return Project{}, err
	} else if taken {
		return Project{}, fmt.Errorf("%w: project %q", ErrExists, name)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO projects(name, created_at) VALUES(?,?)`, name, at.Unix())
	if err != nil {
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	return Project{ID: id, Name: name, Aliases: []string{}, CreatedAt: time.Unix(at.Unix(), 0).UTC()}, tx.Commit()
}

// ResolveProject finds a project by name, then alias, then canonical decimal id.
func (s *Store) ResolveProject(ctx context.Context, ref string) (Project, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE name=?1
		UNION ALL SELECT project_id FROM project_aliases WHERE alias=?1 LIMIT 1`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		n, perr := strconv.ParseInt(ref, 10, 64)
		if perr != nil || n <= 0 || strconv.FormatInt(n, 10) != ref {
			return Project{}, fmt.Errorf("%w: project %q", ErrNotFound, ref)
		}
		id = n
	} else if err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, id)
}
```

`GetProject` reads the row plus `SELECT alias FROM project_aliases WHERE project_id=? ORDER BY alias` (Aliases is `[]string{}` when none). `ListProjects` orders by name. `RenameProject` in one tx: `nameTaken(newName)`; if taken by another id → `ErrExists`; if it is one of this project's aliases, delete that alias row; insert the old name into `project_aliases`; update `projects.name`. `AddProjectAlias` in one tx: project must exist (`ErrNotFound`), `nameTaken` → `ErrExists`. `RemoveProjectAlias` returns the owner id, `ErrNotFound` when no alias row.

`RecordProjectCommits` is the single writer of derived state; everything happens in one tx:

```go
func (s *Store) RecordProjectCommits(ctx context.Context, id int64, commits []ProjectCommit, cursors map[string]string, mc *MountChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if mc != nil && mc.Add != nil {
		m := mc.Add
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_mounts(project_id,mount_path,repo,folder,read_only,added_at) VALUES(?,?,?,?,?,?)`,
			id, m.Path, m.Repo, m.Folder, m.ReadOnly, m.AddedAt.Unix()); err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: mount %q", ErrExists, m.Path)
			}
			return err
		}
	}
	if mc != nil && mc.Remove != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_mounts WHERE project_id=? AND mount_path=?`, id, mc.Remove); err != nil {
			return err
		}
	}
	for _, c := range commits {
		layout, err := json.Marshal(c.Layout)
		if err != nil {
			return err
		}
		if c.Layout == nil {
			layout = []byte("[]")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_commits(project_id,project_sha,seq,kind,repo,source_sha,layout,actor_name,actor_email,actor_when,message)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, c.SHA, c.Seq, c.Kind, c.Repo, c.SourceSHA, string(layout), c.ActorName, c.ActorEmail, c.ActorWhen, c.Message); err != nil {
			return err
		}
		for repo, src := range c.Sources {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_commit_sources(project_id,project_sha,repo,source_sha) VALUES(?,?,?,?)`, id, c.SHA, repo, src); err != nil {
				return err
			}
		}
	}
	if cursors != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_cursors WHERE project_id=?`, id); err != nil {
			return err
		}
		for repo, src := range cursors {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_cursors(project_id,repo,source_sha) VALUES(?,?,?)`, id, repo, src); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
```

`cursors == nil` leaves cursors untouched; an empty non-nil map clears them. `ProjectCommits` decodes `layout` JSON (an empty array decodes to a nil slice: normalize `[]LayoutMount{}` → `nil` so `reflect.DeepEqual` with the inserted value holds) and loads `Sources` from `project_commit_sources` (an empty map, never nil). `ProjectHead` is `SELECT project_sha, seq FROM project_commits WHERE project_id=? ORDER BY seq DESC LIMIT 1` (no rows → `"", 0, nil`).

```go
func (s *Store) RecordLand(ctx context.Context, repo, sha string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO land_seq(repo,sha,at) VALUES(?,?,?) ON CONFLICT(repo,sha) DO NOTHING`, repo, sha, at.Unix())
	return err
}

func (s *Store) LandSeqs(ctx context.Context, repo string, shas []string) (map[string]int64, error) {
	out := make(map[string]int64, len(shas))
	for start := 0; start < len(shas); start += 500 {
		chunk := shas[start:min(start+500, len(shas))]
		q := `SELECT sha, seq FROM land_seq WHERE repo=? AND sha IN (?` + strings.Repeat(",?", len(chunk)-1) + `)`
		args := []any{repo}
		for _, s := range chunk {
			args = append(args, s)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sha string
			var seq int64
			if err := rows.Scan(&sha, &seq); err != nil {
				rows.Close()
				return nil, err
			}
			out[sha] = seq
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
```

`DeleteProjectRows` deletes, in one tx, from `project_aliases`, `project_mounts`, `project_commits`, `project_commit_sources`, `project_cursors`, then `projects` (`ErrNotFound` if no project row). `DeleteRepo` (in `store.go`) gains a first step inside its tx: `SELECT COUNT(*) FROM project_mounts WHERE repo=?` → if > 0 return `fmt.Errorf("%w: %q", ErrMounted, name)`; it also deletes `land_seq` rows of the repo.

- [ ] **Step 5: Run the store tests**

Run: `go test -race -count=1 ./internal/store/`
Expected: PASS (old tests unchanged).

- [ ] **Step 6: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "store: projects, aliases, mounts, derived commits, cursors and land sequence"
```

---

### Task 3: Record the land sequence

**Files:**
- Modify: `internal/server/land.go` (record before the CAS), `internal/server/repos.go` (create and import record their `main`)
- Create: `internal/server/landseq_test.go`

**Interfaces:**
- Consumes: `store.RecordLand`, `store.LandSeqs` (Task 2); `repoService.land`, `repoManager.create`, `repoManager.importRepo`; test helpers `testenv.Start`, `e.CreateRepo`, `e.Upload`, `commitAndLand`, `mirrorEnv`, `sourceRepo`.
- Produces: every commit that reaches `main` through a land, create or import has a `land_seq` row, written before `main` moves.

- [ ] **Step 1: Write the failing test** `internal/server/landseq_test.go`:

```go
package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func TestLandSequenceIsRecordedBeforeMainMoves(t *testing.T) {
	ctx := context.Background()
	var seenAtCAS int
	var e *testenv.Env
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeCAS = func(repo string) {
			// The row for the commit about to land must already exist.
			var n int
			if err := e.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM land_seq WHERE repo=?`, repo).Scan(&n); err != nil {
				t.Error(err)
			}
			seenAtCAS = n
		}
	})
	main := e.CreateRepo("web")
	seqs := testenv.Must(e.Store.LandSeqs(ctx, "web", []string{main}))
	if len(seqs) != 1 {
		t.Fatalf("create did not record main: %v", seqs)
	}
	sha := e.Upload(e.AdminToken, "web", []byte("a\n"))[0]
	resp := testenv.Must(commitAndLand(e.Repo(e.AdminToken), main, "a.txt", sha, "add a"))
	if seenAtCAS != 2 {
		t.Fatalf("land_seq rows at CAS time = %d, want 2", seenAtCAS)
	}
	seqs = testenv.Must(e.Store.LandSeqs(ctx, "web", []string{main, resp.Commit}))
	if len(seqs) != 2 || seqs[main] >= seqs[resp.Commit] {
		t.Fatalf("land seqs = %v", seqs)
	}
}

func TestImportRecordsMain(t *testing.T) {
	ctx := context.Background()
	e := mirrorEnv(t)
	src, main := sourceRepo(t)
	got := testenv.Must(e.Admin(e.AdminToken).ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "imp", FromUrl: "file://" + src}))
	if got.Main != main {
		t.Fatalf("import main = %s", got.Main)
	}
	if seqs := testenv.Must(e.Store.LandSeqs(ctx, "imp", []string{main})); len(seqs) != 1 {
		t.Fatalf("import did not record main: %v", seqs)
	}
}
```

This needs a test-only accessor on the store. Add to `internal/store/store.go`:

```go
// DB exposes the database handle for tests that inspect raw tables.
func (s *Store) DB() *sql.DB { return s.db }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run 'LandSequence|ImportRecordsMain' -count=1`
Expected: FAIL (`create did not record main`).

- [ ] **Step 3: Implement.** In `repoService.land` (`internal/server/land.go`), right after `newC` is created and before `BeforeCAS`/`UpdateRef`:

```go
		bg := context.WithoutCancel(ctx)
		// Recorded before main moves, so a catch-up never sees this commit
		// on main without its sequence number. A row for a commit that loses
		// the CAS is never looked up.
		if err := r.s.store.RecordLand(bg, h.name, newC, r.s.cfg.Now()); err != nil {
			return "", "", err
		}
		if r.s.cfg.BeforeCAS != nil {
			r.s.cfg.BeforeCAS(h.name)
		}
		err = h.git.UpdateRef(bg, gitx.MainRef, newC, mainOld)
```

(remove the old `bg :=` line further down). In `repoManager.create`, after `m.st.CreateRepo` succeeds: `if err := m.st.RecordLand(ctx, name, main, time.Now()); err != nil { return "", err }`. In `repoManager.importRepo`, after `CreateRepoWithMirror` succeeds: `if err := m.st.RecordLand(context.WithoutCancel(ctx), name, main, time.Now()); err != nil { return "", err }`.

- [ ] **Step 4: Run the server tests**

Run: `go test -race -count=1 ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "server: record the land sequence for lands, creates and imports"
```

---

### Task 4: The projection engine (`internal/project`)

**Files:**
- Create: `internal/project/engine.go`, `internal/project/order.go`, `internal/project/engine_test.go`

**Interfaces:**
- Consumes: Task 1 gitx helpers; Task 2 store functions; `gitx.InitBare`, `gitx.Open`, `gitx.Init`, `(*gitx.Repo).Log`, `ReadRef`, `UpdateRef`, `BuildTree`, `CommitTree`, `WriteBlob`.
- Produces:
  ```go
  type Sources interface {
  	Source(ctx context.Context, repo string) (*gitx.Repo, error)
  }
  type Config struct {
  	Root         string // <data>/projects
  	Store        *store.Store
  	Sources      Sources
  	Now          func() time.Time
  	AfterRebuild func(ctx context.Context, id int64) error // optional
  }
  type Handle struct {
  	ID     int64
  	Git    *gitx.Repo
  	LandMu sync.Mutex // drafts and lands through this project (Task 7)
  	mu     sync.Mutex // translation: catch-up, mount, unmount
  }
  func New(cfg Config) *Engine
  func (e *Engine) Path(id int64) string
  func (e *Engine) Get(ctx context.Context, id int64) (*Handle, error)
  func (e *Engine) CatchUp(ctx context.Context, h *Handle) (string, error)
  func (e *Engine) Mount(ctx context.Context, h *Handle, m store.Mount, actor gitx.Signature) (string, error)
  func (e *Engine) Unmount(ctx context.Context, h *Handle, path string, actor gitx.Signature) (string, error)
  func (e *Engine) Delete(id int64) error
  func (e *Engine) CloseAll()
  func LayoutMessage(verb, path string) string // "Project layout: added `docs/`\n"
  ```
  Errors: overlapping mounts wrap `gitx.ErrInvalid`; a different source on an existing mount path wraps `store.ErrExists`; unmounting a missing path wraps `store.ErrNotFound`. Messages name only mount paths.

- [ ] **Step 1: Write the failing tests** `internal/project/engine_test.go`:

```go
package project

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

// Local helpers: importing testenv here would be an import cycle (testenv -> server -> project).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(err)
	}
	return a, b
}

type srcs map[string]*gitx.Repo

func (s srcs) Source(_ context.Context, repo string) (*gitx.Repo, error) {
	r, ok := s[repo]
	if !ok {
		return nil, fmt.Errorf("no repo %q", repo)
	}
	return r, nil
}

type env struct {
	t       *testing.T
	st      *store.Store
	srcs    srcs
	eng     *Engine
	clock   time.Time
	rebuilt []int64
}

var admin = gitx.Signature{Name: "admin", Email: "admin@sgit.local"}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st := must(store.Open(filepath.Join(dir, "sg.db")))
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, srcs: srcs{}, clock: time.Unix(1700000000, 0).UTC()}
	e.eng = New(Config{Root: filepath.Join(dir, "projects"), Store: st, Sources: e.srcs, Now: e.now,
		AfterRebuild: func(_ context.Context, id int64) error { e.rebuilt = append(e.rebuilt, id); return nil }})
	t.Cleanup(e.eng.CloseAll)
	return e
}

func (e *env) now() time.Time { e.clock = e.clock.Add(time.Second); return e.clock }

func (e *env) repo(name string) {
	ctx := context.Background()
	sig := admin
	sig.When = e.now()
	r := must(gitx.Init(ctx, filepath.Join(e.t.TempDir(), name+".git"), sig))
	e.t.Cleanup(func() { r.Close() })
	e.srcs[name] = r
	if err := e.st.CreateRepo(ctx, name, ""); err != nil {
		e.t.Fatal(err)
	}
}

// commit puts files on main of repo; seq records a land sequence row.
func (e *env) commit(repo, msg string, files map[string]string, seq bool) string {
	e.t.Helper()
	ctx := context.Background()
	r := e.srcs[repo]
	main := must(r.ReadRef(ctx, gitx.MainRef))
	var ch []gitx.Change
	for p, c := range files {
		op := gitx.OpAdd
		if c == "" {
			ch = append(ch, gitx.Change{Path: p, Op: gitx.OpDelete})
			continue
		}
		ch = append(ch, gitx.Change{Path: p, Op: op, BlobSHA: must(r.WriteBlob(ctx, []byte(c)))})
	}
	sig := gitx.Signature{Name: "alice", Email: "alice@sgit.local", When: e.now()}
	c := must(r.CommitTree(ctx, must(r.BuildTree(ctx, main, ch)), []string{main}, msg, sig, sig))
	if seq {
		if err := e.st.RecordLand(ctx, repo, c, sig.When); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) project(name string) *Handle {
	ctx := context.Background()
	p := must(e.st.CreateProject(ctx, name, e.now()))
	return must(e.eng.Get(ctx, p.ID))
}

func (e *env) mount(h *Handle, path, repo, folder string) string {
	e.t.Helper()
	sig := admin
	sig.When = e.now()
	return must(e.eng.Mount(context.Background(), h, store.Mount{Path: path, Repo: repo, Folder: folder}, sig))
}

func (e *env) subjects(h *Handle) []string {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.eng.CatchUp(ctx, h); err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, l := range must(h.Git.Log(ctx, "main", "", 1000)) {
		out = append(out, l.Subject)
	}
	return out
}

func (e *env) files(h *Handle) map[string]string {
	ctx := context.Background()
	out := map[string]string{}
	for p, fe := range must(h.Git.ListFiles(ctx, "main")) {
		out[p] = string(must(h.Git.ReadObject(ctx, fe.SHA)).Data)
	}
	return out
}

func TestFolderMountTranslatesOnlyTouchingCommits(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "add both", map[string]string{"src/a.go": "a1", "docs/x.md": "x1"}, true)
	e.commit("core", "docs only", map[string]string{"docs/x.md": "x2"}, true)
	c3 := e.commit("core", "src change", map[string]string{"src/a.go": "a2"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	if got := e.subjects(h); !reflect.DeepEqual(got, []string{"src change", "add both"}) {
		t.Fatalf("project log = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"lib/a.go": "a2"}) {
		t.Fatalf("project files = %v", f)
	}
	ctx := context.Background()
	head := must(h.Git.ReadCommit(ctx, must(h.Git.ReadRef(ctx, gitx.MainRef))))
	sc := must(e.srcs["core"].ReadCommit(ctx, c3))
	if head.AuthorRaw != sc.AuthorRaw || head.CommitterRaw != sc.CommitterRaw || head.Message != sc.Message {
		t.Fatalf("metadata not copied: %+v vs %+v", head, sc)
	}
	if head.SHA == c3 {
		t.Fatal("folder mount reproduced the source SHA")
	}
	if src := must(e.st.ProjectSource(ctx, h.ID, head.SHA, "core")); src != c3 {
		t.Fatalf("ProjectSource = %s, want %s", src, c3)
	}
}

func TestDotMountReproducesTheRepo(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	c1 := e.commit("core", "one", map[string]string{"a": "1"}, true)
	h := e.project("web")
	e.mount(h, ".", "core", ".")
	if got := e.subjects(h); !reflect.DeepEqual(got, []string{"one", "Initial commit"}) {
		t.Fatalf("log = %v", got)
	}
	if head := must(h.Git.ReadRef(context.Background(), gitx.MainRef)); head != c1 {
		t.Fatalf(". mount head %s != source %s (identical history must give identical SHAs)", head, c1)
	}
}

func TestTwoReposInterleaveByLandSequence(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	h := e.project("web")
	e.mount(h, "x", "x", ".")
	e.mount(h, "y", "y", ".")
	e.commit("x", "x1", map[string]string{"f": "1"}, true)
	e.commit("y", "y1", map[string]string{"g": "1"}, true)
	e.commit("x", "x2", map[string]string{"f": "2"}, true)
	got := e.subjects(h)
	if !reflect.DeepEqual(got[:3], []string{"x2", "y1", "x1"}) || got[3] != "Project layout: added `y/`" {
		t.Fatalf("log = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"x/f": "2", "y/g": "1"}) {
		t.Fatalf("files = %v", f)
	}
}

func TestSequenceBeatsCommitterDate(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	h := e.project("web")
	e.mount(h, "x", "x", ".")
	e.mount(h, "y", "y", ".")
	// y1 is committed (dated) first but landed (sequenced) second.
	ctx := context.Background()
	yc := e.commit("y", "y1", map[string]string{"g": "1"}, false)
	e.commit("x", "x1", map[string]string{"f": "1"}, true)
	if err := e.st.RecordLand(ctx, "y", yc, e.now()); err != nil {
		t.Fatal(err)
	}
	if got := e.subjects(h); !reflect.DeepEqual(got[:2], []string{"y1", "x1"}) {
		t.Fatalf("log = %v, want y1 after x1", got)
	}
}

func TestUnsequencedCommitsOrderByDateThenRepo(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	h := e.project("web")
	e.mount(h, "x", "x", ".")
	e.mount(h, "y", "y", ".")
	e.commit("y", "y1", map[string]string{"g": "1"}, false)
	e.commit("x", "x1", map[string]string{"f": "1"}, false)
	if got := e.subjects(h); !reflect.DeepEqual(got[:2], []string{"x1", "y1"}) {
		t.Fatalf("log = %v, want y1 then x1 by date", got)
	}
}

func TestForwardOnlyLayout(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "docs history", map[string]string{"docs/old.md": "o"}, true)
	e.commit("core", "src", map[string]string{"src/a": "a"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	e.mount(h, "docs", "core", "docs")
	got := e.subjects(h)
	if !reflect.DeepEqual(got, []string{"Project layout: added `docs/`", "src"}) {
		t.Fatalf("log after add = %v (older docs history must not be imported)", got)
	}
	if f := e.files(h); f["docs/old.md"] != "o" || f["lib/a"] != "a" {
		t.Fatalf("files = %v", f)
	}
	sig := admin
	sig.When = e.now()
	if _, err := e.eng.Unmount(context.Background(), h, "lib", sig); err != nil {
		t.Fatal(err)
	}
	if got := e.subjects(h); got[0] != "Project layout: removed `lib/`" {
		t.Fatalf("log after remove = %v", got)
	}
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"docs/old.md": "o"}) {
		t.Fatalf("files after remove = %v", f)
	}
	ctx := context.Background()
	head := must(h.Git.ReadCommit(ctx, must(h.Git.ReadRef(ctx, gitx.MainRef))))
	if !strings.HasPrefix(head.AuthorRaw, "admin <admin@sgit.local> ") || strings.Contains(head.Message, "core") {
		t.Fatalf("layout commit = %+v", head)
	}
}

func TestMountValidation(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	ctx := context.Background()
	for _, m := range []store.Mount{
		{Path: "lib/sub", Repo: "core", Folder: "x"},
		{Path: ".", Repo: "core", Folder: "."},
		{Path: "lib", Repo: "core", Folder: "other"},
	} {
		if _, err := e.eng.Mount(ctx, h, m, admin); err == nil {
			t.Errorf("Mount(%+v) accepted", m)
		}
	}
	before := e.subjects(h)
	if _, err := e.eng.Mount(ctx, h, store.Mount{Path: "lib", Repo: "core", Folder: "src", ReadOnly: true}, admin); err != nil {
		t.Fatal(err)
	}
	if ms := must(e.st.Mounts(ctx, h.ID)); !ms[0].ReadOnly {
		t.Fatal("read-only flag not set")
	}
	if after := e.subjects(h); !reflect.DeepEqual(before, after) {
		t.Fatal("changing read-only created a commit")
	}
	if _, err := e.eng.Unmount(ctx, h, "nope", admin); err == nil {
		t.Fatal("unmount of a missing path accepted")
	}
}

func TestMissingFolderAppearsLater(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	h := e.project("web")
	e.mount(h, "lib", "core", "later")
	if got := e.subjects(h); !reflect.DeepEqual(got, []string{"Project layout: added `lib/`"}) {
		t.Fatalf("log = %v", got)
	}
	e.commit("core", "create later", map[string]string{"later/f": "1"}, true)
	if f := e.files(h); !reflect.DeepEqual(f, map[string]string{"lib/f": "1"}) {
		t.Fatalf("files = %v", f)
	}
}

func TestCatchUpWithoutMovesWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	e.commit("core", "one", map[string]string{"src/a": "1"}, true)
	h := e.project("web")
	e.mount(h, "lib", "core", "src")
	ctx := context.Background()
	h1, s1 := must2(e.st.ProjectHead(ctx, h.ID))
	e.commit("core", "unrelated", map[string]string{"docs/z": "1"}, true)
	head := must(e.eng.CatchUp(ctx, h))
	h2, s2 := must2(e.st.ProjectHead(ctx, h.ID))
	if head != h1 || h2 != h1 || s2 != s1 {
		t.Fatalf("unrelated commit changed the project: %s/%d -> %s/%d", h1, s1, h2, s2)
	}
	cur := must(e.st.ProjectCursors(ctx, h.ID))
	if cur["core"] != must(e.srcs["core"].ReadRef(ctx, gitx.MainRef)) {
		t.Fatal("cursor did not advance past the skipped commit")
	}
}

func TestRebuildGivesSameSHAs(t *testing.T) {
	e := newEnv(t)
	e.repo("x")
	e.repo("y")
	e.commit("x", "x0", map[string]string{"src/a": "1", "other/z": "z"}, true)
	h := e.project("web")
	e.mount(h, "a", "x", "src")
	e.mount(h, "b", "y", ".")
	e.commit("y", "y1", map[string]string{"g": "1"}, true)
	e.commit("x", "x1", map[string]string{"src/a": "2"}, true)
	sig := admin
	sig.When = e.now()
	if _, err := e.eng.Unmount(context.Background(), h, "b", sig); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want := must(h.Git.Log(ctx, "main", "", 100))
	e.eng.CloseAll()
	if err := os.RemoveAll(e.eng.Path(h.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := must(e.eng.Get(ctx, h.ID))
	got := must(h2.Git.Log(ctx, "main", "", 100))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt log differs:\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(e.rebuilt, []int64{h.ID}) {
		t.Fatalf("AfterRebuild calls = %v", e.rebuilt)
	}
}

func TestProjectRepoHoldsOnlyMountedObjects(t *testing.T) {
	e := newEnv(t)
	e.repo("core")
	src := e.commit("core", "both", map[string]string{"pub/a": "public", "secret/k": "top secret"}, true)
	h := e.project("web")
	e.mount(h, "pub", "core", "pub")
	ctx := context.Background()
	secretTree := must(e.srcs["core"].Subtrees(ctx, []string{src}, "secret"))[0]
	secretBlob := must(e.srcs["core"].WriteBlob(ctx, []byte("top secret"))) // same SHA as committed
	out, err := exec.Command("git", "--git-dir="+e.eng.Path(h.ID), "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{src, secretTree, secretBlob} {
		if strings.Contains(string(out), bad) {
			t.Fatalf("project repo holds foreign object %s", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(e.eng.Path(h.ID), "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("project repo has alternates: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/project/ -count=1`
Expected: FAIL to compile (`undefined: New`, `undefined: Config`, …).

- [ ] **Step 3: Implement `internal/project/order.go`** (the §5.3 comparator and k-way merge):

```go
package project

import "github.com/hermes98761234/sgit/internal/gitx"

// queue is the pending first-parent commits of one mounted repo.
type queue struct {
	repo    string
	src     *gitx.Repo
	commits []gitx.FPCommit
	seqs    map[string]int64    // land sequence by commit SHA
	trees   map[string][]string // mount path -> subtree per commit
	pos     int
}

type key struct {
	hasSeq bool
	seq    int64
	when   int64
	repo   string
	pos    int
}

func (q *queue) key() key {
	c := q.commits[q.pos]
	s, ok := q.seqs[c.SHA]
	return key{hasSeq: ok, seq: s, when: c.CommitterUnix, repo: q.repo, pos: q.pos}
}

// less orders two queue heads: by land sequence when both have one,
// otherwise by committer date, then repo name, then first-parent position.
func less(a, b key) bool {
	if a.hasSeq && b.hasSeq {
		return a.seq < b.seq
	}
	if a.when != b.when {
		return a.when < b.when
	}
	if a.repo != b.repo {
		return a.repo < b.repo
	}
	return a.pos < b.pos
}

// next returns the queue whose head comes first, or nil when all are drained.
func next(qs []*queue) *queue {
	var best *queue
	for _, q := range qs {
		if q.pos >= len(q.commits) {
			continue
		}
		if best == nil || less(q.key(), best.key()) {
			best = q
		}
	}
	return best
}
```

- [ ] **Step 4: Implement `internal/project/engine.go`.** The translation core, exact:

```go
// Package project derives each project's bare repo from the mounted folders
// of its source repos (spec §5) and keeps it up to date.
package project

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

type Engine struct {
	cfg  Config
	mu   sync.Mutex
	open map[int64]*Handle
}

func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{cfg: cfg, open: map[int64]*Handle{}}
}

func (e *Engine) Path(id int64) string {
	return filepath.Join(e.cfg.Root, strconv.FormatInt(id, 10)+".git")
}

func LayoutMessage(verb, path string) string {
	return fmt.Sprintf("Project layout: %s `%s/`\n", verb, path)
}

// Get opens the project repo, creating it when the project has no commits
// and rebuilding it from the recorded history when it is missing.
func (e *Engine) Get(ctx context.Context, id int64) (*Handle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.open[id]; ok {
		return h, nil
	}
	dir := e.Path(id)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		head, _, err := e.cfg.Store.ProjectHead(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(e.cfg.Root, 0o750); err != nil {
			return nil, err
		}
		if head == "" {
			if err := gitx.InitBare(ctx, dir); err != nil {
				return nil, err
			}
		} else if err := e.rebuild(ctx, id, dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	h := &Handle{ID: id, Git: gitx.Open(dir)}
	e.open[id] = h
	return h, nil
}

// CatchUp translates new source commits. When no mounted repo's main moved
// it costs one ref read per mounted repo and takes no lock.
func (e *Engine) CatchUp(ctx context.Context, h *Handle) (string, error) {
	mounts, err := e.cfg.Store.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	cursors, err := e.cfg.Store.ProjectCursors(ctx, h.ID)
	if err != nil {
		return "", err
	}
	moved := false
	for _, repo := range reposOf(mounts) {
		src, err := e.cfg.Sources.Source(ctx, repo)
		if err != nil {
			return "", err
		}
		main, err := src.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", err
		}
		if main != cursors[repo] {
			moved = true
			break
		}
	}
	if !moved {
		head, _, err := e.cfg.Store.ProjectHead(ctx, h.ID)
		return head, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return e.catchUpLocked(ctx, h)
}

func (e *Engine) catchUpLocked(ctx context.Context, h *Handle) (string, error) {
	st := e.cfg.Store
	head, seq, err := st.ProjectHead(ctx, h.ID)
	if err != nil {
		return "", err
	}
	if err := e.moveRef(ctx, h, head); err != nil { // the DB is the truth; repair a lagging ref
		return "", err
	}
	mounts, err := st.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	cursors, err := st.ProjectCursors(ctx, h.ID)
	if err != nil {
		return "", err
	}
	byRepo := map[string][]store.Mount{}
	for _, m := range mounts {
		byRepo[m.Repo] = append(byRepo[m.Repo], m)
	}
	cur := map[string]string{} // mount path -> current subtree ("" = absent)
	var queues []*queue
	for _, repo := range reposOf(mounts) {
		src, err := e.cfg.Sources.Source(ctx, repo)
		if err != nil {
			return "", err
		}
		main, err := src.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", err
		}
		c := cursors[repo]
		if c != "" {
			for _, m := range byRepo[repo] {
				t, err := src.Subtrees(ctx, []string{c}, m.Folder)
				if err != nil {
					return "", err
				}
				cur[m.Path] = norm(m.Path, t[0])
			}
		}
		if main == c {
			continue
		}
		list, err := src.FirstParents(ctx, main, c)
		if err != nil {
			return "", err
		}
		shas := make([]string, len(list))
		for i, fc := range list {
			shas[i] = fc.SHA
		}
		q := &queue{repo: repo, src: src, commits: list, trees: map[string][]string{}}
		for _, m := range byRepo[repo] {
			if q.trees[m.Path], err = src.Subtrees(ctx, shas, m.Folder); err != nil {
				return "", err
			}
		}
		if q.seqs, err = st.LandSeqs(ctx, repo, shas); err != nil {
			return "", err
		}
		queues = append(queues, q)
	}
	if len(queues) == 0 {
		return head, nil
	}
	sources := maps.Clone(cursors)
	if sources == nil {
		sources = map[string]string{}
	}
	layout := layoutOf(mounts)
	var out []store.ProjectCommit
	parent := head
	for q := next(queues); q != nil; q = next(queues) {
		c := q.commits[q.pos]
		var want, have []string
		changed := false
		for _, m := range byRepo[q.repo] {
			t := norm(m.Path, q.trees[m.Path][q.pos])
			if t == cur[m.Path] {
				continue
			}
			changed = true
			if t != "" {
				want = append(want, t)
			}
			if cur[m.Path] != "" {
				have = append(have, cur[m.Path])
			}
			cur[m.Path] = t
		}
		sources[q.repo] = c.SHA
		q.pos++
		if !changed {
			continue
		}
		if _, err := gitx.CopyObjects(ctx, q.src, h.Git, want, have); err != nil {
			return "", err
		}
		tree, err := h.Git.ComposeTree(ctx, mountTrees(mounts, cur))
		if err != nil {
			return "", err
		}
		sc, err := q.src.ReadCommit(ctx, c.SHA)
		if err != nil {
			return "", err
		}
		sha, err := h.Git.WriteCommit(ctx, tree, parents(parent), sc.AuthorRaw, sc.CommitterRaw, sc.Message)
		if err != nil {
			return "", err
		}
		seq++
		out = append(out, store.ProjectCommit{SHA: sha, Seq: seq, Kind: store.KindSource, Repo: q.repo,
			SourceSHA: c.SHA, Layout: layout, Sources: maps.Clone(sources)})
		parent = sha
	}
	if err := st.RecordProjectCommits(ctx, h.ID, out, sources, nil); err != nil {
		return "", err
	}
	if err := e.moveRef(ctx, h, parent); err != nil {
		return "", err
	}
	return parent, nil
}

// moveRef points the project's main at sha (no-op when sha is "" or current).
func (e *Engine) moveRef(ctx context.Context, h *Handle, sha string) error {
	if sha == "" {
		return nil
	}
	cur, err := h.Git.ReadRef(ctx, gitx.MainRef)
	if errors.Is(err, gitx.ErrNotFound) {
		cur = ""
	} else if err != nil {
		return err
	}
	if cur == sha {
		return nil
	}
	return h.Git.UpdateRef(ctx, gitx.MainRef, sha, cur)
}

func parents(p string) []string {
	if p == "" {
		return nil
	}
	return []string{p}
}

func reposOf(mounts []store.Mount) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mounts {
		if !seen[m.Repo] {
			seen[m.Repo] = true
			out = append(out, m.Repo)
		}
	}
	sort.Strings(out)
	return out
}

func layoutOf(mounts []store.Mount) []store.LayoutMount {
	var out []store.LayoutMount
	for _, m := range mounts {
		out = append(out, store.LayoutMount{Path: m.Path, Repo: m.Repo, Folder: m.Folder})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func mountTrees(mounts []store.Mount, cur map[string]string) []gitx.MountTree {
	out := make([]gitx.MountTree, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, gitx.MountTree{Path: m.Path, Tree: cur[m.Path]})
	}
	return out
}

// norm treats an empty folder tree as absent, except for a "." mount, so
// project trees never hold empty directories (an empty source repo's root
// mounted at a sub-path shows up as no entry at all).
func norm(path, tree string) string {
	if path != "." && tree == gitx.EmptyTree {
		return ""
	}
	return tree
}

func overlaps(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
```

Mount, unmount and the shared layout writer:

```go
// Mount adds m. The first mount of a project without commits imports the
// history of the folder (§5.4); later mounts write one layout commit (§5.5).
// Re-mounting the same path with the same source only sets the read-only flag.
func (e *Engine) Mount(ctx context.Context, h *Handle, m store.Mount, actor gitx.Signature) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	head, err := e.catchUpLocked(ctx, h)
	if err != nil {
		return "", err
	}
	mounts, err := e.cfg.Store.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	for _, old := range mounts {
		if old.Path == m.Path {
			if old.Repo == m.Repo && old.Folder == m.Folder {
				return head, e.cfg.Store.SetMountReadOnly(ctx, h.ID, m.Path, m.ReadOnly)
			}
			return "", fmt.Errorf("%w: mount %q already exists; unmount it first", store.ErrExists, m.Path)
		}
		if overlaps(old.Path, m.Path) {
			return "", fmt.Errorf("%w: mount path %q overlaps %q", gitx.ErrInvalid, m.Path, old.Path)
		}
	}
	m.AddedAt = e.cfg.Now()
	if head == "" {
		if err := e.cfg.Store.RecordProjectCommits(ctx, h.ID, nil, nil, &store.MountChange{Add: &m}); err != nil {
			return "", err
		}
		if head, err = e.catchUpLocked(ctx, h); err != nil || head != "" {
			return head, err
		}
		return e.layoutLocked(ctx, h, actor, "added", m.Path, nil)
	}
	return e.layoutLocked(ctx, h, actor, "added", m.Path, &store.MountChange{Add: &m})
}

func (e *Engine) Unmount(ctx context.Context, h *Handle, path string, actor gitx.Signature) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := e.catchUpLocked(ctx, h); err != nil {
		return "", err
	}
	mounts, err := e.cfg.Store.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	for _, m := range mounts {
		if m.Path == path {
			return e.layoutLocked(ctx, h, actor, "removed", path, &store.MountChange{Remove: path})
		}
	}
	return "", fmt.Errorf("%w: mount %q", store.ErrNotFound, path)
}

// layoutLocked writes one layout commit holding the current content of every
// mount after applying mc. New repos start at their current main.
func (e *Engine) layoutLocked(ctx context.Context, h *Handle, actor gitx.Signature, verb, path string, mc *store.MountChange) (string, error) {
	st := e.cfg.Store
	head, seq, err := st.ProjectHead(ctx, h.ID)
	if err != nil {
		return "", err
	}
	mounts, err := st.Mounts(ctx, h.ID)
	if err != nil {
		return "", err
	}
	cursors, err := st.ProjectCursors(ctx, h.ID)
	if err != nil {
		return "", err
	}
	if mc != nil && mc.Add != nil {
		mounts = append(mounts, *mc.Add)
	}
	if mc != nil && mc.Remove != "" {
		kept := mounts[:0]
		for _, m := range mounts {
			if m.Path != mc.Remove {
				kept = append(kept, m)
			}
		}
		mounts = kept
	}
	newCursors := map[string]string{}
	cur := map[string]string{}
	for _, m := range mounts {
		src, err := e.cfg.Sources.Source(ctx, m.Repo)
		if err != nil {
			return "", err
		}
		c, ok := newCursors[m.Repo]
		if !ok {
			if c, ok = cursors[m.Repo]; !ok || c == "" {
				if c, err = src.ReadRef(ctx, gitx.MainRef); err != nil {
					return "", err
				}
			}
			newCursors[m.Repo] = c
		}
		t, err := src.Subtrees(ctx, []string{c}, m.Folder)
		if err != nil {
			return "", err
		}
		tt := norm(m.Path, t[0])
		if tt != "" {
			if _, err := gitx.CopyObjects(ctx, src, h.Git, []string{tt}, nil); err != nil {
				return "", err
			}
		}
		cur[m.Path] = tt
	}
	tree, err := h.Git.ComposeTree(ctx, mountTrees(mounts, cur))
	if err != nil {
		return "", err
	}
	actor.When = time.Unix(actor.When.Unix(), 0).UTC()
	ident := gitx.FormatIdent(actor)
	msg := LayoutMessage(verb, path)
	sha, err := h.Git.WriteCommit(ctx, tree, parents(head), ident, ident, msg)
	if err != nil {
		return "", err
	}
	pc := store.ProjectCommit{SHA: sha, Seq: seq + 1, Kind: store.KindLayout, Layout: layoutOf(mounts),
		ActorName: actor.Name, ActorEmail: actor.Email, ActorWhen: actor.When.Unix(), Message: msg, Sources: maps.Clone(newCursors)}
	if err := st.RecordProjectCommits(ctx, h.ID, []store.ProjectCommit{pc}, newCursors, mc); err != nil {
		return "", err
	}
	return sha, e.moveRef(ctx, h, sha)
}
```

Rebuild, delete and close:

```go
// rebuild recreates dir from the recorded derivation and checks every SHA.
func (e *Engine) rebuild(ctx context.Context, id int64, dir string) error {
	commits, err := e.cfg.Store.ProjectCommits(ctx, id)
	if err != nil {
		return err
	}
	stage := dir + ".rebuild"
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := gitx.InitBare(ctx, stage); err != nil {
		return err
	}
	g := gitx.Open(stage)
	defer g.Close()
	copied := map[string]string{} // repo \x00 folder -> last tree copied
	parent := ""
	for _, pc := range commits {
		var mts []gitx.MountTree
		for _, lm := range pc.Layout {
			src, err := e.cfg.Sources.Source(ctx, lm.Repo)
			if err != nil {
				return err
			}
			t, err := src.Subtrees(ctx, []string{pc.Sources[lm.Repo]}, lm.Folder)
			if err != nil {
				return err
			}
			tt := norm(lm.Path, t[0])
			k := lm.Repo + "\x00" + lm.Folder
			if tt != "" && copied[k] != tt {
				var have []string
				if copied[k] != "" {
					have = []string{copied[k]}
				}
				if _, err := gitx.CopyObjects(ctx, src, g, []string{tt}, have); err != nil {
					return err
				}
				copied[k] = tt
			}
			mts = append(mts, gitx.MountTree{Path: lm.Path, Tree: tt})
		}
		tree, err := g.ComposeTree(ctx, mts)
		if err != nil {
			return err
		}
		var author, committer, msg string
		if pc.Kind == store.KindSource {
			src, err := e.cfg.Sources.Source(ctx, pc.Repo)
			if err != nil {
				return err
			}
			sc, err := src.ReadCommit(ctx, pc.SourceSHA)
			if err != nil {
				return err
			}
			author, committer, msg = sc.AuthorRaw, sc.CommitterRaw, sc.Message
		} else {
			ident := gitx.FormatIdent(gitx.Signature{Name: pc.ActorName, Email: pc.ActorEmail, When: time.Unix(pc.ActorWhen, 0)})
			author, committer, msg = ident, ident, pc.Message
		}
		sha, err := g.WriteCommit(ctx, tree, parents(parent), author, committer, msg)
		if err != nil {
			return err
		}
		if sha != pc.SHA {
			return fmt.Errorf("project %d: rebuilt commit %d is %s, recorded %s", id, pc.Seq, sha, pc.SHA)
		}
		parent = sha
	}
	if parent != "" {
		if err := g.UpdateRef(ctx, gitx.MainRef, parent, ""); err != nil {
			return err
		}
	}
	g.Close()
	if err := os.Rename(stage, dir); err != nil {
		return err
	}
	if e.cfg.AfterRebuild != nil {
		return e.cfg.AfterRebuild(ctx, id)
	}
	return nil
}

// Delete closes and removes a project's derived repo (the DB rows are the caller's).
func (e *Engine) Delete(id int64) error {
	e.mu.Lock()
	h := e.open[id]
	delete(e.open, id)
	e.mu.Unlock()
	if h != nil {
		h.mu.Lock() // wait for a running catch-up
		h.Git.Close()
		h.mu.Unlock()
	}
	return os.RemoveAll(e.Path(id))
}

func (e *Engine) CloseAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, h := range e.open {
		h.Git.Close()
		delete(e.open, id)
	}
}
```

Note `rebuild` runs under `e.mu` (from `Get`), so other projects wait for it; it only runs after an operator deleted a project repo.

- [ ] **Step 5: Run the engine tests**

Run: `go test -race -count=1 ./internal/project/`
Expected: PASS. If `TestUnsequencedCommitsOrderByDateThenRepo` fails, check that `e.now()` advances between the two commits (it does: one second per call).

- [ ] **Step 6: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "project: derive project repos from mounted folders, catch up, layout commits, rebuild"
```

---

### Task 5: Proto: the `Project` service and project fields (mechanical rename)

This task changes the wire API and renames every consumer, **without changing behavior**: the server still treats the `project` field as a repo name until Task 7. The new RPCs are declared here and return Unimplemented until Tasks 6 and 7.

**Files:**
- Modify: `proto/sgit/v1/sgit.proto`; regenerate `gen/sgit/v1/*.pb.go` with `make gen`
- Modify (mechanical): `internal/server/*.go`, `internal/daemon/*.go`, `internal/daemontest/daemontest.go`, `internal/fetch/fetch.go`, `internal/rpc/rpc.go`, `internal/cli/*.go`, `internal/testenv/testenv.go`, `internal/smoke/docker_smoke_test.go`, and every `_test.go` that uses them

**Interfaces:**
- Produces (generated): `sgitv1.ProjectClient`, `sgitv1.NewProjectClient`, `sgitv1.RegisterProjectServer`, `sgitv1.UnimplementedProjectServer`; request field `Project` everywhere `Repo` was; `Draft.Project`; `ConflictDetail.Partial`, `ConflictDetail.Landed`; `ResolveRevResponse.ProjectId`, `.ProjectName`; messages `ListProjectsRequest/Response`, `ProjectSummary`, `ProjectInfo`, `MountInfo`, `GrantInfo`, `CreateProjectRequest`, `MountProjectRequest`, `UnmountProjectRequest`, `AddProjectAliasRequest`, `RemoveProjectAliasRequest`, `RenameProjectRequest`, `AdminListProjectsRequest/Response`, `GetProjectRequest`, `DeleteProjectRequest/Response`; ACL messages with `project` instead of `repo`.
- `rpc.UploadBlobs(ctx context.Context, c sgitv1.ProjectClient, project string, blobs [][]byte) ([]string, error)`
- `fetch.Tree(ctx context.Context, c sgitv1.ProjectClient, project, commit string, sparse []string)`, `fetch.Blobs(ctx context.Context, c sgitv1.ProjectClient, project string, shas []string)`
- `testenv.(*Env).Project(token string) sgitv1.ProjectClient` (replaces `Repo`); `daemontest.(*Env).Project(token string) sgitv1.ProjectClient` (replaces `Repo`).

- [ ] **Step 1: Rewrite `proto/sgit/v1/sgit.proto`** (complete file; field numbers of existing fields are kept, so only names change):

```proto
syntax = "proto3";

package sgit.v1;

option go_package = "github.com/hermes98761234/sgit/gen/sgit/v1;sgitv1";

// Project is the client API, served by both sg-server and sgd. Every request
// names a project by name, alias or decimal id.
service Project {
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
  // ListProjects lists the projects the caller can access (admins: all).
  rpc ListProjects(ListProjectsRequest) returns (ListProjectsResponse);
}

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
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse);
  rpc SetUserAdmin(SetUserAdminRequest) returns (SetUserAdminResponse);
  rpc DeleteUser(DeleteUserRequest) returns (DeleteUserResponse);
  rpc ListTokens(ListTokensRequest) returns (ListTokensResponse);
  rpc ListACL(ListACLRequest) returns (ListACLResponse);
  rpc DeleteRepo(DeleteRepoRequest) returns (DeleteRepoResponse);
  rpc CreateProject(CreateProjectRequest) returns (ProjectInfo);
  rpc MountProject(MountProjectRequest) returns (ProjectInfo);
  rpc UnmountProject(UnmountProjectRequest) returns (ProjectInfo);
  rpc AddProjectAlias(AddProjectAliasRequest) returns (ProjectInfo);
  rpc RemoveProjectAlias(RemoveProjectAliasRequest) returns (ProjectInfo);
  rpc RenameProject(RenameProjectRequest) returns (ProjectInfo);
  rpc ListProjects(AdminListProjectsRequest) returns (AdminListProjectsResponse);
  rpc GetProject(GetProjectRequest) returns (ProjectInfo);
  rpc DeleteProject(DeleteProjectRequest) returns (DeleteProjectResponse);
}

message ResolveRevRequest { string project = 1; string rev = 2; }
// project_id and project_name identify the resolved project (canonical name).
message ResolveRevResponse { string sha = 1; int64 project_id = 2; string project_name = 3; }

message GetObjectsRequest { string project = 1; repeated string shas = 2; }
message Object {
  string sha = 1;
  string type = 2;      // blob | tree | commit
  uint64 size = 3;      // uncompressed size
  bytes zstd_data = 4;  // zstd-compressed raw object content
}
message HasObjectsRequest { string project = 1; repeated string shas = 2; }
message HasObjectsResponse { repeated bool present = 1; }

// BlobChunk streams blobs in <= 1 MiB chunks; project is required on the first chunk.
message BlobChunk { string project = 1; bytes data = 2; bool end_of_blob = 3; }
message UploadBlobsResponse { repeated string shas = 1; }

enum ChangeOp {
  CHANGE_OP_UNSPECIFIED = 0;
  CHANGE_OP_ADD = 1;
  CHANGE_OP_MODIFY = 2;
  CHANGE_OP_DELETE = 3;
}
// path is relative to the project root.
message Change { string path = 1; ChangeOp op = 2; string blob_sha = 3; string mode = 4; }

message CreateDraftRequest { string project = 1; string base = 2; repeated Change changes = 3; string title = 4; string message = 5; }
message UpdateDraftRequest { string project = 1; string id = 2; string base = 3; repeated Change changes = 4; string message = 5; }
message GetDraftRequest { string project = 1; string id = 2; }
message ListDraftsRequest { string project = 1; bool mine = 2; string status = 3; }
message ListDraftsResponse { repeated Draft drafts = 1; }
message AbandonDraftRequest { string project = 1; string id = 2; }
message Draft {
  string id = 1;
  string project = 2;  // canonical project name
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

message LandRequest { string project = 1; string id = 2; }
message CommitAndLandRequest { string project = 1; string draft_id = 2; string base = 3; repeated Change changes = 4; string title = 5; string message = 6; }
// commit is the new project head. parent is set only when that head's tree
// equals the draft's tree; it is then the commit the draft landed on.
message LandResponse { string commit = 1; string draft_id = 2; string parent = 3; }

// Attached as a status detail to ABORTED errors from Land/CommitAndLand.
// "ours" is main, "theirs" is the draft. main is the project head to rebase
// onto. partial is set when some mounts landed before the stop; landed lists
// their mount paths, and the draft now holds only what did not land.
message ConflictFile { string path = 1; string base_sha = 2; string ours_sha = 3; string theirs_sha = 4; }
message ConflictDetail { string draft_id = 1; string main = 2; repeated ConflictFile files = 3; bool partial = 4; repeated string landed = 5; }

message LogRequest { string project = 1; string rev = 2; string path = 3; int32 n = 4; }
message LogEntry { string sha = 1; string author_name = 2; string author_email = 3; int64 time = 4; string subject = 5; }
message LogResponse { repeated LogEntry entries = 1; }

message BlameRequest { string project = 1; string rev = 2; string path = 3; }
message BlameLine { string sha = 1; string author = 2; int32 line = 3; bytes text = 4; }
message BlameResponse { repeated BlameLine lines = 1; }

message DiffRequest { string project = 1; string a = 2; string b = 3; repeated string paths = 4; }
message DiffResponse { bytes patch = 1; }

message MergeFileRequest { string project = 1; string base_sha = 2; string ours_sha = 3; string theirs_sha = 4; string ours_label = 5; string theirs_label = 6; }
message MergeFileResponse { bytes content = 1; bool conflicted = 2; }

message ListProjectsRequest {}
// level is read, write, or admin (for admins, who see every project).
message ProjectSummary { int64 id = 1; string name = 2; repeated string aliases = 3; string level = 4; }
message ListProjectsResponse { repeated ProjectSummary projects = 1; }

message WhoAmIRequest {}
message WhoAmIResponse { string user = 1; string email = 2; bool admin = 3; }
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
  string error = 9;               // set when the repo itself could not be read
}
message CreateTokenRequest { string user = 1; string email = 2; bool admin = 3; }
message CreateTokenResponse { string id = 1; string token = 2; string user = 3; }
message RevokeTokenRequest { string id = 1; }
message RevokeTokenResponse {}
message GrantACLRequest { string user = 1; string project = 2; string level = 3; }
message GrantACLResponse {}
message RevokeACLRequest { string user = 1; string project = 2; }
message RevokeACLResponse {}

message UserInfo {
  string name = 1;
  string email = 2;
  bool admin = 3;
  int32 active_tokens = 4;  // tokens that are not revoked
  int64 created_unix = 5;
}
message ListUsersRequest {}
message ListUsersResponse { repeated UserInfo users = 1; }
message SetUserAdminRequest { string user = 1; bool admin = 2; }
message SetUserAdminResponse {}
message DeleteUserRequest { string user = 1; }
message DeleteUserResponse { int32 tokens_revoked = 1; int32 acls_removed = 2; int32 drafts_abandoned = 3; }
// TokenInfo never carries the secret, only the public id.
message TokenInfo { string id = 1; string user = 2; int64 created_unix = 3; int64 revoked_unix = 4; }
// ListTokensRequest.user filters to one user; empty lists everyone's.
message ListTokensRequest { string user = 1; }
message ListTokensResponse { repeated TokenInfo tokens = 1; }
message ACLEntry { string user = 1; string project = 2; string level = 3; }
// ListACLRequest.user and .project are optional filters.
message ListACLRequest { string user = 1; string project = 2; }
message ListACLResponse { repeated ACLEntry entries = 1; }
// DeleteRepo moves the bare repo to <data>/deleted; moved_to is its new path.
// It is refused while any project mounts the repo.
message DeleteRepoRequest { string name = 1; }
message DeleteRepoResponse { string moved_to = 1; }

// Project administration (admin only). project accepts a name, alias or id.
message MountInfo { string path = 1; string repo = 2; string folder = 3; bool read_only = 4; int64 added_unix = 5; }
message GrantInfo { string user = 1; string level = 2; }
message ProjectInfo {
  int64 id = 1;
  string name = 2;
  repeated string aliases = 3;
  repeated MountInfo mounts = 4;
  repeated GrantInfo grants = 5;
  string head = 6;  // "" while the project has no commits
  int64 created_unix = 7;
}
message CreateProjectRequest { string name = 1; }
message MountProjectRequest { string project = 1; string path = 2; string repo = 3; string folder = 4; bool read_only = 5; }
message UnmountProjectRequest { string project = 1; string path = 2; }
message AddProjectAliasRequest { string project = 1; string alias = 2; }
message RemoveProjectAliasRequest { string alias = 1; }
message RenameProjectRequest { string project = 1; string new_name = 2; }
message AdminListProjectsRequest {}
message AdminListProjectsResponse { repeated ProjectInfo projects = 1; }
message GetProjectRequest { string project = 1; }
message DeleteProjectRequest { string project = 1; }
message DeleteProjectResponse { int32 drafts_removed = 1; int32 acls_removed = 2; }

// Daemon is served only by sgd, so clients can check which server it fronts
// before sending it a token.
service Daemon {
  rpc Info(DaemonInfoRequest) returns (DaemonInfoResponse);
}

message DaemonInfoRequest {}
message DaemonInfoResponse {
  string upstream = 1;
  string version = 2;
  uint64 cache_bytes = 3;
  uint64 cache_max_bytes = 4;
}
```

- [ ] **Step 2: Regenerate and see the build break**

Run: `make gen && go build ./... 2>&1 | head -40`
Expected: compile errors (`undefined: sgitv1.RepoClient`, `unknown field Repo in struct literal`, …).

- [ ] **Step 3: Rename the generated identifiers everywhere:**

```bash
grep -rl --include=*.go -e 'RepoClient\|RepoServer\|sgit\.v1\.Repo/' internal cmd | xargs sed -i \
  -e 's/UnimplementedRepoServer/UnimplementedProjectServer/g' \
  -e 's/sgitv1\.RepoClient/sgitv1.ProjectClient/g' \
  -e 's/NewRepoClient/NewProjectClient/g' \
  -e 's/RegisterRepoServer/RegisterProjectServer/g' \
  -e 's#/sgit\.v1\.Repo/#/sgit.v1.Project/#g'
grep -rl --include=*.go '\.Repo(' internal | xargs sed -i 's/\.Repo(/.Project(/g'
```

Then rename the methods in `internal/testenv/testenv.go` and `internal/daemontest/daemontest.go`:

```go
func (e *Env) Project(token string) sgitv1.ProjectClient { return sgitv1.NewProjectClient(e.Conn(token)) }
```

- [ ] **Step 4: Fix the remaining field names until `go build ./... && go vet ./...` pass.** The rule is mechanical: in every `sgitv1.*Request` literal and every `BlobChunk`, `Repo:` becomes `Project:`; every read of `req.Repo` / `c.Repo` / `r.Repo` / `msg.Repo` on a proto message becomes `.Project`; `ACLEntry{… Repo: …}` and `e.Repo` in the CLI's `aclList` become `Project`; `draftPB` sets `Project: d.Repo`. Do **not** rename `store.Draft.Repo`, `store.ACLRow.Repo` or `repoHandle.name` (they change meaning in Task 7). In `rpc.UploadBlobs` and `fetch` rename the parameter `repo` to `project`. Use `go build ./... 2>&1 | head` and `go vet ./... 2>&1 | head` (which also compiles tests) to find each site.

New RPCs are not implemented yet; the embedded `Unimplemented*Server` types satisfy the interfaces.

- [ ] **Step 5: Run everything**

Run: `gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: PASS with no behavior change. `grep -rn 'sgit.v1.Repo\b\|RepoClient' --include=*.go internal cmd` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "proto: Project service, project fields and project admin RPCs (rename only)"
```

---

### Task 6: Server: project administration RPCs

**Files:**
- Create: `internal/server/admin_projects.go`, `internal/server/admin_projects_test.go`
- Modify: `internal/server/server.go` (engine wiring), `internal/server/repos.go` (`Source`), `internal/server/admin.go` (`DeleteRepo` refusal), `internal/auth/auth.go` (`ProjectNotFound`), `internal/testenv/testenv.go` (project helpers)

**Interfaces:**
- Consumes: `project.New`, `Engine.Get/Mount/Unmount/Delete/CloseAll` (Task 4), store project functions (Task 2), `validatePath`, `ValidRepoName`, `requireAdmin`, `toStatus`, `notFound`.
- Produces:
  ```go
  // auth
  func ProjectNotFound(ref string) error // status NotFound `project "<ref>" not found or no access`
  // server
  func (m *repoManager) Source(ctx context.Context, repo string) (*gitx.Repo, error)
  func (a *adminService) resolveProject(ctx context.Context, ref string) (store.Project, *project.Handle, error)
  func (a *adminService) projectInfo(ctx context.Context, p store.Project) (*sgitv1.ProjectInfo, error)
  // testenv
  func (e *Env) CreateProject(name string) int64
  func (e *Env) Mount(projectRef, path, repo, folder string, readOnly bool) string // returns the project head
  func (e *Env) ProjectGit(id int64) *gitx.Repo                                    // caller closes
  ```
  `toStatus` additionally maps `store.ErrMounted` to FailedPrecondition.

- [ ] **Step 1: Write the failing tests** `internal/server/admin_projects_test.go`:

```go
package server_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProjectAdminLifecycle(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/a.go": "a", "docs/b.md": "b"}, "seed")
	ac := e.Admin(e.AdminToken)

	info := testenv.Must(ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "web"}))
	if info.Name != "web" || info.Id == 0 || info.Head != "" || len(info.Mounts) != 0 {
		t.Fatalf("CreateProject = %+v", info)
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "projects", strconv.FormatInt(info.Id, 10)+".git")); err != nil {
		t.Fatalf("project repo missing: %v", err)
	}
	info = testenv.Must(ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "src"}))
	if len(info.Mounts) != 1 || info.Mounts[0].Path != "lib" || info.Mounts[0].Repo != "core" || info.Mounts[0].Folder != "src" || info.Head == "" {
		t.Fatalf("MountProject = %+v", info)
	}
	g := e.ProjectGit(info.Id)
	defer g.Close()
	files := testenv.Must(g.ListFiles(ctx, "main"))
	if len(files) != 1 || files["lib/a.go"].SHA == "" {
		t.Fatalf("project files = %v", files)
	}
	testenv.Must(ac.AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: "web", Alias: "site"}))
	info = testenv.Must(ac.RenameProject(ctx, &sgitv1.RenameProjectRequest{Project: "site", NewName: "www"}))
	if info.Name != "www" || !reflect.DeepEqual(info.Aliases, []string{"site", "web"}) {
		t.Fatalf("RenameProject = %+v", info)
	}
	byID := testenv.Must(ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: strconv.FormatInt(info.Id, 10)}))
	if byID.Name != "www" {
		t.Fatalf("GetProject by id = %+v", byID)
	}
	list := testenv.Must(ac.ListProjects(ctx, &sgitv1.AdminListProjectsRequest{}))
	if len(list.Projects) != 1 || list.Projects[0].Name != "www" {
		t.Fatalf("ListProjects = %+v", list)
	}
	info = testenv.Must(ac.RemoveProjectAlias(ctx, &sgitv1.RemoveProjectAliasRequest{Alias: "site"}))
	if !reflect.DeepEqual(info.Aliases, []string{"web"}) {
		t.Fatalf("RemoveProjectAlias = %+v", info)
	}
	info = testenv.Must(ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "www", Path: "lib"}))
	if len(info.Mounts) != 0 {
		t.Fatalf("UnmountProject = %+v", info)
	}
	testenv.Must(ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "www"}))
	if _, err := ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: "www"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "projects", strconv.FormatInt(info.Id, 10)+".git")); !os.IsNotExist(err) {
		t.Fatalf("project repo not removed: %v", err)
	}
}

func TestMountProjectValidation(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "web"}))
	testenv.Must(ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "src"}))
	cases := []struct {
		req  *sgitv1.MountProjectRequest
		code codes.Code
	}{
		{&sgitv1.MountProjectRequest{Project: "web", Path: "../x", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "/abs", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "a/.git", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "x", Repo: "core", Folder: "a//b"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "lib/sub", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: ".", Repo: "core", Folder: "."}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "other"}, codes.AlreadyExists},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "y", Repo: "nope", Folder: "a"}, codes.NotFound},
		{&sgitv1.MountProjectRequest{Project: "nope", Path: "y", Repo: "core", Folder: "a"}, codes.NotFound},
	}
	for _, c := range cases {
		if _, err := ac.MountProject(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("MountProject(%+v) = %v, want %v", c.req, err, c.code)
		}
	}
	for _, name := range []string{"Web", "-x", "", "a b"} {
		if _, err := ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: name}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateProject(%q) = %v", name, err)
		}
	}
	if _, err := ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "web"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate CreateProject = %v", err)
	}
}

func TestRepoDeleteRefusedWhileMounted(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	ac := e.Admin(e.AdminToken)
	_, err := ac.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "core"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != `repo "core" is mounted by project(s) web; unmount it first` {
		t.Fatalf("DeleteRepo while mounted = %v", err)
	}
	testenv.Must(ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "web", Path: "lib"}))
	testenv.Must(ac.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "core"}))
}

func TestProjectAdminNeedsAdmin(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	bob := e.NewUser("bob", "", "")
	ac := e.Admin(bob)
	calls := map[string]func() error{
		"CreateProject":      func() error { _, err := ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "x"}); return err },
		"MountProject":       func() error { _, err := ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "x"}); return err },
		"UnmountProject":     func() error { _, err := ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "x"}); return err },
		"AddProjectAlias":    func() error { _, err := ac.AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: "x"}); return err },
		"RemoveProjectAlias": func() error { _, err := ac.RemoveProjectAlias(ctx, &sgitv1.RemoveProjectAliasRequest{Alias: "x"}); return err },
		"RenameProject":      func() error { _, err := ac.RenameProject(ctx, &sgitv1.RenameProjectRequest{Project: "x"}); return err },
		"ListProjects":       func() error { _, err := ac.ListProjects(ctx, &sgitv1.AdminListProjectsRequest{}); return err },
		"GetProject":         func() error { _, err := ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: "x"}); return err },
		"DeleteProject":      func() error { _, err := ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "x"}); return err },
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s as non-admin = %v", name, err)
		}
	}
}
```

`e.NewUser("bob", "", "")` creates a user without a grant (the grant branch is skipped for an empty level).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run 'ProjectAdmin|MountProject|RepoDeleteRefused' -count=1`
Expected: FAIL to compile (`e.CreateProject undefined`) and, after adding the testenv helpers, Unimplemented errors.

- [ ] **Step 3: testenv helpers** in `internal/testenv/testenv.go`:

```go
// CreateProject creates an empty project and returns its id.
func (e *Env) CreateProject(name string) int64 {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).CreateProject(context.Background(), &sgitv1.CreateProjectRequest{Name: name})).Id
}

// Mount mounts repo:folder at path in project and returns the project head.
func (e *Env) Mount(project, path, repo, folder string, readOnly bool) string {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).MountProject(context.Background(), &sgitv1.MountProjectRequest{
		Project: project, Path: path, Repo: repo, Folder: folder, ReadOnly: readOnly})).Head
}

// ProjectGit opens a project's derived repo directly (caller closes).
func (e *Env) ProjectGit(id int64) *gitx.Repo {
	return gitx.Open(filepath.Join(e.DataDir, "projects", strconv.FormatInt(id, 10)+".git"))
}
```

- [ ] **Step 4: Wire the engine.** In `internal/server/server.go` add `projects *project.Engine` to `Server`, create it in `New` after `s.repos`:

```go
	s.projects = project.New(project.Config{
		Root:    filepath.Join(cfg.DataDir, "projects"),
		Store:   st,
		Sources: s.repos,
		Now:     cfg.Now,
	})
```

and call `s.projects.CloseAll()` in `Close` before `s.repos.closeAll()`. In `toStatus` add `case errors.Is(err, store.ErrMounted): return status.Error(codes.FailedPrecondition, err.Error())`. In `internal/server/repos.go`:

```go
// Source opens a source repo for the projection engine.
func (m *repoManager) Source(ctx context.Context, repo string) (*gitx.Repo, error) {
	h, err := m.get(ctx, repo)
	if err != nil {
		return nil, err
	}
	return h.git, nil
}
```

In `internal/auth/auth.go`:

```go
// ProjectNotFound is the one error for a project that does not exist and a
// project the caller cannot read; the two must be indistinguishable (§8).
func ProjectNotFound(ref string) error {
	return status.Errorf(codes.NotFound, "project %q not found or no access", ref)
}
```

- [ ] **Step 5: Implement `internal/server/admin_projects.go`:**

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/project"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (a *adminService) resolveProject(ctx context.Context, ref string) (store.Project, *project.Handle, error) {
	p, err := a.s.store.ResolveProject(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return p, nil, auth.ProjectNotFound(ref)
	}
	if err != nil {
		return p, nil, a.s.toStatus(ctx, err)
	}
	h, err := a.s.projects.Get(ctx, p.ID)
	if err != nil {
		return p, nil, a.s.toStatus(ctx, err)
	}
	return p, h, nil
}

func (a *adminService) projectInfo(ctx context.Context, p store.Project) (*sgitv1.ProjectInfo, error) {
	p, err := a.s.store.GetProject(ctx, p.ID) // fresh name and aliases
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	head, _, err := a.s.store.ProjectHead(ctx, p.ID)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	mounts, err := a.s.store.Mounts(ctx, p.ID)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	info := &sgitv1.ProjectInfo{Id: p.ID, Name: p.Name, Aliases: p.Aliases, Head: head, CreatedUnix: p.CreatedAt.Unix()}
	for _, m := range mounts {
		info.Mounts = append(info.Mounts, &sgitv1.MountInfo{Path: m.Path, Repo: m.Repo, Folder: m.Folder, ReadOnly: m.ReadOnly, AddedUnix: m.AddedAt.Unix()})
	}
	return info, nil
}

func validMountPath(p string) error {
	if p == "." {
		return nil
	}
	return validatePath(p)
}

func (a *adminService) CreateProject(ctx context.Context, req *sgitv1.CreateProjectRequest) (*sgitv1.ProjectInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if !ValidRepoName(req.Name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid project name %q", req.Name)
	}
	p, err := a.s.store.CreateProject(ctx, req.Name, a.s.cfg.Now())
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if _, err := a.s.projects.Get(ctx, p.ID); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) MountProject(ctx context.Context, req *sgitv1.MountProjectRequest) (*sgitv1.ProjectInfo, error) {
	u, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	if err := validMountPath(req.Path); err != nil {
		return nil, err
	}
	if err := validMountPath(req.Folder); err != nil {
		return nil, err
	}
	p, h, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	if _, err := a.s.store.GetRepo(ctx, req.Repo); err != nil {
		return nil, a.s.toStatus(ctx, notFound(err, "repo %q not found", req.Repo))
	}
	m := store.Mount{Path: req.Path, Repo: req.Repo, Folder: req.Folder, ReadOnly: req.ReadOnly}
	if _, err := a.s.projects.Mount(ctx, h, m, a.s.sig(u)); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}
```

`UnmountProject` (`projects.Unmount`), `AddProjectAlias` (`ValidRepoName(alias)`, then `store.AddProjectAlias`), `RemoveProjectAlias` (`store.RemoveProjectAlias` → `projectInfo` of the returned id; `ErrNotFound` → NotFound `alias %q not found`), `RenameProject` (`ValidRepoName(new_name)`, `store.RenameProject`), `ListProjects` (every `store.ListProjects` row through `projectInfo`), and `GetProject` follow the same shape: `requireAdmin`, `resolveProject`, store/engine call through `toStatus`, `projectInfo`. Every engine error (`gitx.ErrInvalid` overlap, `store.ErrExists`, `store.ErrNotFound`) already maps in `toStatus`.

```go
func (a *adminService) DeleteProject(ctx context.Context, req *sgitv1.DeleteProjectRequest) (*sgitv1.DeleteProjectResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, h, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	h.LandMu.Lock() // no draft change or land is mid-flight
	defer h.LandMu.Unlock()
	if err := a.s.store.DeleteProjectRows(ctx, p.ID); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if err := a.s.projects.Delete(p.ID); err != nil {
		a.s.log.ErrorContext(ctx, "project rows deleted but repo not removed", "project", p.ID, "err", err)
	}
	return &sgitv1.DeleteProjectResponse{}, nil
}
```

(`DeleteProjectResponse` counts are filled in Task 7, when ACLs and drafts become per project.)

In `adminService.DeleteRepo`, before calling `deleteRepo`:

```go
	if names, err := a.s.store.ProjectsMounting(ctx, req.Name); err != nil {
		return nil, a.s.toStatus(ctx, err)
	} else if len(names) > 0 {
		sort.Strings(names)
		return nil, status.Errorf(codes.FailedPrecondition, "repo %q is mounted by project(s) %s; unmount it first", req.Name, strings.Join(names, ", "))
	}
```

Remove `fmt` from the imports if unused.

- [ ] **Step 6: Run the server tests**

Run: `go test -race -count=1 ./internal/server/`
Expected: PASS.

- [ ] **Step 7: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "server: project administration RPCs; repo delete refused while mounted"
```

---

### Task 7: Server: every client RPC goes through a project

This is the switch-over. After it, ACLs and drafts are per project, `Server.open` resolves projects with existence hiding, all reads come from the project repo after a catch-up, and writes are mapped through the mounts and landed into the source repo (single repo; Task 8 adds cross-repo lands).

**Files:**
- Modify: `internal/store/store.go` (schema for `acl` and `drafts`, `migrate`, `DeleteRepo`, `DeleteUser` unchanged in behavior), move ACL and draft functions to `internal/store/acl.go`; update `internal/store/store_test.go`, `internal/store/admin_test.go`
- Modify: `internal/auth/auth.go`, `internal/auth/auth_test.go`
- Create: `internal/server/paths.go`, `internal/server/paths_internal_test.go`, `internal/server/projects_test.go`
- Modify: `internal/server/server.go`, `read.go`, `drafts.go`, `land.go`, `validate.go`, `admin.go`, `admin_projects.go`
- Modify: `internal/testenv/testenv.go`; mechanical updates in `internal/server/*_test.go`, `internal/daemon/*_test.go`, `internal/cli/*_test.go`, `internal/fetch/fetch_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2, 4, 5, 6.
- Produces:
  ```go
  // store
  type Draft struct{ ID string; ProjectID int64; OwnerID int64; OwnerName, Title, Base, Head string; Version int; Status, LandedAs string; CreatedAt, UpdatedAt time.Time }
  type ACLRow struct{ User, Project string; Level Level } // Project = canonical name
  type Access struct{ Project Project; Level Level }
  func (s *Store) SetACL(ctx context.Context, userID, projectID int64, l Level) error
  func (s *Store) ACL(ctx context.Context, userID, projectID int64) (Level, error)
  func (s *Store) ListACL(ctx context.Context, user string, projectID int64) ([]ACLRow, error) // "" / 0 = no filter
  func (s *Store) ListAccess(ctx context.Context, userID int64) ([]Access, error)             // projects with a grant, by name
  func (s *Store) GetDraft(ctx context.Context, projectID int64, id string) (Draft, error)
  func (s *Store) ListDrafts(ctx context.Context, projectID, ownerID int64, status string) ([]Draft, error)
  func (s *Store) AbandonOpenDrafts(ctx context.Context, projectID int64) (int, error)
  func (s *Store) DeleteProjectRows(ctx context.Context, id int64) (drafts, acls int, err error) // signature change from Task 2
  // auth
  var ErrNoAccess = errors.New("auth: no access")
  type ACLReader interface{ ACL(ctx context.Context, userID, projectID int64) (store.Level, error) }
  func Authorize(ctx context.Context, acl ACLReader, u store.User, projectID int64, a Action) error
  // server
  type projectService struct{ sgitv1.UnimplementedProjectServer; s *Server } // renamed from repoService
  type proj struct{ h *project.Handle; p store.Project; ref string; mounts []store.Mount }
  func (s *Server) open(ctx context.Context, ref string, a auth.Action) (*proj, store.User, error)
  func (s *Server) hide(ctx context.Context, err error) error
  func mountFor(mounts []store.Mount, p string) (store.Mount, bool)
  func sourcePath(m store.Mount, p string) string
  func projectPath(mounts []store.Mount, repo, sp string) (string, bool)
  func checkPaths(mounts []store.Mount, paths []string) error
  func (s *Server) landSource(ctx context.Context, h *repoHandle, base, head string) (newC string, conflicts []gitx.ConflictFile, err error)
  var errAlreadyOnMain = errors.New("changes already on main")
  // testenv
  func (e *Env) NewUser(name, project, level string) string        // level granted on project
  func (e *Env) CreateRepoProject(name string) string               // repo + project `name` mounted at "."; returns head
  func (e *Env) ProjectID(ref string) int64
  ```

- [ ] **Step 1: Write the failing tests.** `internal/server/paths_internal_test.go` (package `server`):

```go
package server

import (
	"testing"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMountMapping(t *testing.T) {
	ms := []store.Mount{
		{Path: "lib", Repo: "core", Folder: "src"},
		{Path: "lib/x", Repo: "other", Folder: "."}, // nesting is refused by Mount; mapping still picks the longest
		{Path: "docs", Repo: "core", Folder: "doc/en", ReadOnly: true},
	}
	cases := []struct{ p, repo, src string }{
		{"lib/a.go", "core", "src/a.go"},
		{"lib/x/y", "other", "y"},
		{"docs/r.md", "core", "doc/en/r.md"},
	}
	for _, c := range cases {
		m, ok := mountFor(ms, c.p)
		if !ok || m.Repo != c.repo || sourcePath(m, c.p) != c.src {
			t.Errorf("map %q = %+v %v -> %q", c.p, m, ok, sourcePath(m, c.p))
		}
	}
	for _, p := range []string{"lib", "other.txt", "libx/a", "docs"} {
		if _, ok := mountFor(ms, p); ok {
			t.Errorf("%q mapped to a mount", p)
		}
	}
	dot := []store.Mount{{Path: ".", Repo: "core", Folder: "src"}}
	if m, ok := mountFor(dot, "a/b"); !ok || sourcePath(m, "a/b") != "src/a/b" {
		t.Errorf(". mount mapping = %+v %v", m, ok)
	}
	if p, ok := projectPath(ms, "core", "doc/en/r.md"); !ok || p != "docs/r.md" {
		t.Errorf("projectPath = %q %v", p, ok)
	}
	if _, ok := projectPath(ms, "core", "secret/k"); ok {
		t.Error("projectPath mapped an unmounted source path")
	}
	err := checkPaths(ms, []string{"lib/a.go", "other.txt"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "path outside project: other.txt" {
		t.Errorf("outside = %v", err)
	}
	err = checkPaths(ms, []string{"docs/r.md"})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "read-only path: docs/r.md" {
		t.Errorf("read-only = %v", err)
	}
}
```

`internal/server/projects_test.go` (package `server_test`):

```go
package server_test

import (
	"context"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// coreWeb makes repo "core" (src/a.go, secret/k.txt) and project "web"
// mounting core:src at lib, and returns the env and the source main.
func coreWeb(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	e := testenv.Start(t)
	e.CreateRepo("core")
	main := e.CommitDirect("core", map[string]string{"src/a.go": "package a\n", "secret/k.txt": "top secret\n"}, "seed")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	return e, main
}

func TestProjectReadsServeOnlyMounts(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	bob := e.NewUser("bob", "web", "read")
	c := e.Project(bob)
	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}))
	if rr.Sha == srcMain || rr.ProjectName != "web" || rr.ProjectId != e.ProjectID("web") {
		t.Fatalf("ResolveRev = %+v", rr)
	}
	objs := testenv.Must(fetch(t, c, "web", rr.Sha))
	if len(objs) != 1 || objs[0].Type != "commit" {
		t.Fatalf("commit fetch = %+v", objs)
	}
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	files := testenv.Must(g.ListFiles(ctx, rr.Sha))
	if len(files) != 1 || files["lib/a.go"].SHA == "" {
		t.Fatalf("project files = %v", files)
	}
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web"}))
	if len(lg.Entries) != 1 || lg.Entries[0].Subject != "seed" || lg.Entries[0].Sha != rr.Sha {
		t.Fatalf("log = %+v", lg.Entries)
	}
	bl := testenv.Must(c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "lib/a.go"}))
	if len(bl.Lines) != 1 || bl.Lines[0].Sha != rr.Sha {
		t.Fatalf("blame = %+v", bl.Lines)
	}
	for _, rev := range []string{srcMain, srcMain[:12]} {
		if _, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: rev}); status.Code(err) != codes.NotFound {
			t.Fatalf("ResolveRev(source sha %s) = %v", rev, err)
		}
	}
	list := testenv.Must(c.ListProjects(ctx, &sgitv1.ListProjectsRequest{}))
	if len(list.Projects) != 1 || list.Projects[0].Name != "web" || list.Projects[0].Level != "read" {
		t.Fatalf("ListProjects = %+v", list)
	}
}

// statusBytes is the full wire form of a gRPC error (code, message, details).
func statusBytes(t *testing.T, err error) []byte {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return testenv.Must(proto.Marshal(status.Convert(err).Proto()))
}

func TestUnknownAndNoAccessAreIdentical(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	bob := e.NewUser("bob", "", "")
	c := e.Project(bob)
	sha := strings.Repeat("a", 40)
	calls := map[string]func(ref string) error{
		"ResolveRev": func(ref string) error { _, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: ref}); return err },
		"GetObjects": func(ref string) error { _, err := fetch(t, c, ref, sha); return err },
		"HasObjects": func(ref string) error { _, err := c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: ref}); return err },
		"UploadBlobs": func(ref string) error {
			_, err := rpc.UploadBlobs(ctx, c, ref, [][]byte{[]byte("x")})
			return err
		},
		"CreateDraft": func(ref string) error {
			_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: ref, Base: sha, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}})
			return err
		},
		"UpdateDraft":   func(ref string) error { _, err := c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: ref, Id: "aaaaaaaa"}); return err },
		"GetDraft":      func(ref string) error { _, err := c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: ref, Id: "aaaaaaaa"}); return err },
		"ListDrafts":    func(ref string) error { _, err := c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: ref}); return err },
		"AbandonDraft":  func(ref string) error { _, err := c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: ref, Id: "aaaaaaaa"}); return err },
		"Land":          func(ref string) error { _, err := c.Land(ctx, &sgitv1.LandRequest{Project: ref, Id: "aaaaaaaa"}); return err },
		"CommitAndLand": func(ref string) error { _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: ref, Base: sha, Message: "m"}); return err },
		"Log":           func(ref string) error { _, err := c.Log(ctx, &sgitv1.LogRequest{Project: ref}); return err },
		"Blame":         func(ref string) error { _, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: ref, Path: "a"}); return err },
		"Diff":          func(ref string) error { _, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: ref, A: sha, B: sha}); return err },
		"MergeFile":     func(ref string) error { _, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: ref}); return err },
	}
	refs := []string{"web", "1", "site"}
	before := map[string][]byte{}
	for name, call := range calls {
		for _, ref := range refs {
			err := call(ref)
			if status.Code(err) != codes.NotFound || status.Convert(err).Message() != `project "`+ref+`" not found or no access` {
				t.Fatalf("%s(%s) before create = %v", name, ref, err)
			}
			before[name+ref] = statusBytes(t, err)
		}
	}
	id := e.CreateProject("web") // id 1, the first project
	if id != 1 {
		t.Fatalf("first project id = %d", id)
	}
	e.Mount("web", "lib", "core", "src", false)
	testenv.Must(e.Admin(e.AdminToken).AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: "web", Alias: "site"}))
	for name, call := range calls {
		for _, ref := range refs {
			if got := statusBytes(t, call(ref)); string(got) != string(before[name+ref]) {
				t.Errorf("%s(%s): no-access error differs from unknown-project error", name, ref)
			}
		}
	}
	// Invalid names get the same shape too.
	if err := calls["Log"]("Bad Name!"); status.Convert(err).Message() != `project "Bad Name!" not found or no access` {
		t.Errorf("invalid name = %v", err)
	}
}

func TestWriteWithReadAccessIsPermissionDenied(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	bob := e.NewUser("bob", "web", "read")
	c := e.Project(bob)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if _, err := rpc.UploadBlobs(ctx, c, "web", [][]byte{[]byte("x")}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("UploadBlobs with read = %v", err)
	}
	if _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CommitAndLand with read = %v", err)
	}
}

func TestPathOutsideProjectRejected(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	for _, p := range []string{"other.txt", "lib", "secret/k.txt"} {
		_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add(p, sha)}})
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "path outside project: "+p {
			t.Errorf("CreateDraft(%s) = %v", p, err)
		}
	}
}

func TestReadOnlyPathRejected(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}}))
	e.Mount("web", "lib", "core", "src", true) // flip to read-only after the draft exists
	for _, tok := range []string{alice, e.AdminToken} {
		_, err := e.Project(tok).Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "read-only path: lib/x" {
			t.Fatalf("Land on read-only mount = %v", err)
		}
		_, err = e.Project(tok).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/y", sha)}})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("CreateDraft on read-only mount = %v", err)
		}
	}
}

func TestRevokedGrantBlocksLand(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}}))
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "alice", Project: "web", Level: "read"}))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Land after downgrade = %v", err)
	}
	testenv.Must(ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "alice", Project: "web"}))
	_, err := c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != `project "web" not found or no access` {
		t.Fatalf("Land after revoke = %v", err)
	}
	if files := e.Files("core", "main"); files["src/x"] != "" {
		t.Fatal("revoked draft reached the source repo")
	}
}

func TestLandThroughProject(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("new\n"))[0]
	resp := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "add b",
		Changes: []*sgitv1.Change{testenv.Add("lib/b.go", sha)}}))
	if resp.Parent != head {
		t.Fatalf("LandResponse.Parent = %s, want %s (landed on its base)", resp.Parent, head)
	}
	if got := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha; got != resp.Commit {
		t.Fatalf("project main = %s, land returned %s", got, resp.Commit)
	}
	files := e.Files("core", "main")
	if files["src/b.go"] != "new\n" || files["secret/k.txt"] != "top secret\n" || len(files) != 3 {
		t.Fatalf("source files = %v", files)
	}
	g := e.Git("core")
	defer g.Close()
	sm := testenv.Must(g.ReadCommit(ctx, testenv.Must(g.ReadRef(ctx, "refs/heads/main"))))
	if sm.Parents[0] != srcMain || sm.Author.Name != "alice" || sm.Message != "add b\n" {
		t.Fatalf("source commit = %+v", sm)
	}
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: resp.DraftId}))
	if d.Status != "landed" || d.LandedAs != resp.Commit || d.Project != "web" {
		t.Fatalf("draft = %+v", d)
	}
}

func TestLandConflictMapsPathsBack(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	e.CommitDirect("core", map[string]string{"src/a.go": "package theirs\n"}, "upstream edit")
	sha := e.Upload(alice, "web", []byte("package ours\n"))[0]
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "edit",
		Changes: []*sgitv1.Change{testenv.Modify("lib/a.go", sha)}})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("conflict = %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	now := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if det == nil || len(det.Files) != 1 || det.Files[0].Path != "lib/a.go" || det.Main != now || det.Partial {
		t.Fatalf("detail = %+v (project main %s)", det, now)
	}
	// The three blobs are readable through the project for the client's merge.
	for _, s := range []string{det.Files[0].BaseSha, det.Files[0].OursSha, det.Files[0].TheirsSha} {
		if _, err := fetch(t, c, "web", s); err != nil {
			t.Fatalf("conflict blob %s not readable: %v", s, err)
		}
	}
}

func TestCatchUpAfterLandThroughOtherProject(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	e.CreateProject("api")
	e.Mount("api", "code", "core", "src", false)
	ac := e.Project(e.AdminToken)
	head := testenv.Must(ac.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "api"})).Sha
	sha := e.Upload(e.AdminToken, "api", []byte("shared\n"))[0]
	testenv.Must(ac.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "api", Base: head, Message: "via api",
		Changes: []*sgitv1.Change{testenv.Add("code/s.go", sha)}}))
	lg := testenv.Must(ac.Log(ctx, &sgitv1.LogRequest{Project: "web", N: 1}))
	if lg.Entries[0].Subject != "via api" {
		t.Fatalf("web did not catch up: %+v", lg.Entries)
	}
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	if f := testenv.Must(g.ListFiles(ctx, "main")); f["lib/s.go"].SHA != sha {
		t.Fatalf("web files = %v", f)
	}
}

func TestDraftBaseMustBeAProjectCommit(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	sha := e.Upload(e.AdminToken, "web", []byte("x\n"))[0]
	_, err := e.Project(e.AdminToken).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: srcMain, Message: "m",
		Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "base commit") {
		t.Fatalf("CreateDraft on a source base = %v", err)
	}
}
```

Update `internal/auth/auth_test.go`: `fakeACL` becomes `map[int64]store.Level` with `ACL(_ context.Context, _ int64, project int64)`; `TestAuthorize` asserts: admin always nil; no grant → `errors.Is(err, ErrNoAccess)`; read grant + write action → PermissionDenied; `ActionAdmin` for a non-admin → PermissionDenied `admin role required`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ ./internal/auth/ -count=1 2>&1 | head -30`
Expected: FAIL to compile (`undefined: mountFor`, `e.ProjectID undefined`, `ErrNoAccess` undefined, …).

- [ ] **Step 3: Store.** In `schema` replace the `acl` and `drafts` tables and the drafts index with:

```sql
CREATE TABLE IF NOT EXISTS acl (
  user_id INTEGER NOT NULL REFERENCES users(id),
  project_id INTEGER NOT NULL,
  level INTEGER NOT NULL,
  PRIMARY KEY (user_id, project_id)
);
CREATE TABLE IF NOT EXISTS drafts (
  id TEXT PRIMARY KEY,
  project_id INTEGER NOT NULL,
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
CREATE INDEX IF NOT EXISTS drafts_project_status ON drafts(project_id, status);
```

`Open` must drop old tables **before** the schema runs (otherwise `CREATE TABLE IF NOT EXISTS` keeps the old shape). Add `preMigrate(db)` called before `db.Exec(schema)`:

```go
// preMigrate drops the per-repo acl and drafts tables of databases created
// before projects (spec §1: no migration, no production data).
func preMigrate(db *sql.DB) error {
	for _, t := range []string{"acl", "drafts"} {
		has, err := hasColumn(db, t, "repo")
		if err != nil {
			return err
		}
		if has {
			if _, err := db.Exec(`DROP INDEX IF EXISTS drafts_repo_status; DROP TABLE ` + t); err != nil {
				return err
			}
		}
	}
	return nil
}
```

Move the ACL and draft functions to `internal/store/acl.go` with the new signatures (Interfaces above). Key SQL:

```go
const draftCols = `d.id,d.project_id,d.owner_id,u.name,d.title,d.base,d.head,d.version,d.status,d.landed_as,d.created_at,d.updated_at`

// GetDraft: WHERE d.project_id=? AND d.id=?; not found → fmt.Errorf("%w: draft %q", ErrNotFound, id) (no project or repo in the text).
// UpdateDraft: ... WHERE project_id=? AND id=?
// ListACL:
q := `SELECT u.name, p.name, a.level FROM acl a JOIN users u ON u.id=a.user_id JOIN projects p ON p.id=a.project_id
      WHERE u.deleted_at IS NULL`
// + optional AND u.name=? / AND a.project_id=?; ORDER BY u.name, p.name
// ListAccess:
q := `SELECT a.project_id, a.level FROM acl a JOIN projects p ON p.id=a.project_id WHERE a.user_id=? ORDER BY p.name`
// then GetProject for each row (aliases included).
func (s *Store) AbandonOpenDrafts(ctx context.Context, projectID int64) (int, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE drafts SET status=?, updated_at=? WHERE project_id=? AND status=?`,
		DraftAbandoned, time.Now().Unix(), projectID, DraftOpen)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}
```

`DeleteProjectRows` now also deletes `acl` and `drafts` rows of the project (returning both counts). `DeleteRepo` no longer deletes `acl`/`drafts` rows (they are per project now). Update `store_test.go` and `admin_test.go`: create projects with `s.CreateProject` and use their ids in `SetACL`, `ACL`, `InsertDraft{ProjectID: p.ID}`, `GetDraft(ctx, p.ID, id)`, `ListDrafts(ctx, p.ID, …)`, `ListACL(ctx, "alice", p.ID)` and `ACLRow{User: "alice", Project: "r2", Level: LevelWrite}`. Add to `projects_test.go`:

```go
func TestOldPerRepoTablesAreDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sg.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE acl (user_id INTEGER, repo TEXT, level INTEGER, PRIMARY KEY (user_id, repo));
		CREATE TABLE drafts (id TEXT PRIMARY KEY, repo TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tc := range []struct{ table, col string }{{"acl", "project_id"}, {"drafts", "project_id"}} {
		if ok, err := hasColumn(s.db, tc.table, tc.col); err != nil || !ok {
			t.Fatalf("%s.%s missing after Open: %v", tc.table, tc.col, err)
		}
	}
}
```

- [ ] **Step 4: Auth.** Replace `ACLReader` and `Authorize` in `internal/auth/auth.go`:

```go
// ErrNoAccess means the caller cannot read the project. Callers must turn it
// into ProjectNotFound, so that it looks exactly like an unknown project.
var ErrNoAccess = errors.New("auth: no access")

type ACLReader interface {
	ACL(ctx context.Context, userID, projectID int64) (store.Level, error)
}

// Authorize is the single permission decision point (spec §8).
func Authorize(ctx context.Context, acl ACLReader, u store.User, projectID int64, a Action) error {
	if u.Admin {
		return nil
	}
	switch a {
	case ActionAdmin:
		return status.Error(codes.PermissionDenied, "admin role required")
	case ActionRead, ActionWrite:
	default:
		return status.Error(codes.PermissionDenied, "unknown action")
	}
	lvl, err := acl.ACL(ctx, u.ID, projectID)
	if err != nil {
		return status.Errorf(codes.Internal, "acl lookup: %v", err)
	}
	if lvl < store.LevelRead {
		return ErrNoAccess
	}
	if a == ActionWrite && lvl < store.LevelWrite {
		return status.Error(codes.PermissionDenied, "write access denied")
	}
	return nil
}
```

`requireAdmin` calls `auth.Authorize(ctx, a.s.store, u, 0, auth.ActionAdmin)`.

- [ ] **Step 5: Mount mapping** `internal/server/paths.go`:

```go
package server

import (
	"strings"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mountFor returns the mount holding project path p: the longest mount path
// that is a proper prefix of p ("." holds everything). A path equal to a
// mount path is the mount itself, not a file inside it.
func mountFor(mounts []store.Mount, p string) (store.Mount, bool) {
	best, ok := store.Mount{}, false
	for _, m := range mounts {
		if m.Path == "." || strings.HasPrefix(p, m.Path+"/") {
			if !ok || len(m.Path) > len(best.Path) || best.Path == "." {
				best, ok = m, true
			}
		}
	}
	return best, ok
}

// sourcePath maps project path p (inside m) to its path in m.Repo.
func sourcePath(m store.Mount, p string) string {
	rest := p
	if m.Path != "." {
		rest = strings.TrimPrefix(p, m.Path+"/")
	}
	if m.Folder == "." {
		return rest
	}
	return m.Folder + "/" + rest
}

// projectPath maps source path sp of repo back into the project (the mount
// with the longest matching folder wins).
func projectPath(mounts []store.Mount, repo, sp string) (string, bool) {
	best, ok := store.Mount{}, false
	for _, m := range mounts {
		if m.Repo != repo || !(m.Folder == "." || strings.HasPrefix(sp, m.Folder+"/")) {
			continue
		}
		if !ok || len(m.Folder) > len(best.Folder) || best.Folder == "." {
			best, ok = m, true
		}
	}
	if !ok {
		return "", false
	}
	rest := sp
	if best.Folder != "." {
		rest = strings.TrimPrefix(sp, best.Folder+"/")
	}
	if best.Path == "." {
		return rest, true
	}
	return best.Path + "/" + rest, true
}

// checkPaths enforces §6.1 for every path a write touches.
func checkPaths(mounts []store.Mount, paths []string) error {
	for _, p := range paths {
		m, ok := mountFor(mounts, p)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "path outside project: %s", p)
		}
		if m.ReadOnly {
			return status.Errorf(codes.PermissionDenied, "read-only path: %s", p)
		}
	}
	return nil
}
```

- [ ] **Step 6: `Server.open` and `hide`** in `internal/server/server.go` (replace the old `open`):

```go
// proj is an opened, authorized and caught-up project for one RPC.
type proj struct {
	h      *project.Handle
	p      store.Project
	ref    string // what the caller sent; used in errors
	mounts []store.Mount
}

// open resolves ref, authorizes the caller and catches the project up.
// Unknown projects and projects the caller cannot read give the identical
// ProjectNotFound error.
func (s *Server) open(ctx context.Context, ref string, a auth.Action) (*proj, store.User, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, u, status.Error(codes.Unauthenticated, "no user")
	}
	p, err := s.store.ResolveProject(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, u, auth.ProjectNotFound(ref)
	}
	if err != nil {
		return nil, u, s.hide(ctx, err)
	}
	if err := auth.Authorize(ctx, s.store, u, p.ID, a); err != nil {
		if errors.Is(err, auth.ErrNoAccess) {
			return nil, u, auth.ProjectNotFound(ref)
		}
		return nil, u, err
	}
	h, err := s.projects.Get(ctx, p.ID)
	if err != nil {
		return nil, u, s.hide(ctx, err)
	}
	if _, err := s.projects.CatchUp(ctx, h); err != nil {
		return nil, u, s.hide(ctx, err)
	}
	mounts, err := s.store.Mounts(ctx, p.ID)
	if err != nil {
		return nil, u, s.hide(ctx, err)
	}
	return &proj{h: h, p: p, ref: ref, mounts: mounts}, u, nil
}

// hide logs err and returns a generic Internal error. Every failure inside a
// source repo goes through it: such messages can name repos, source paths or
// source SHAs, none of which may reach a client.
func (s *Server) hide(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	id := auth.RequestID(ctx)
	s.log.ErrorContext(ctx, "project operation failed", "request_id", id, "err", err)
	return status.Errorf(codes.Internal, "internal error (request %s)", id)
}
```

Wire `AfterRebuild` in `New`: `AfterRebuild: func(ctx context.Context, id int64) error { _, err := st.AbandonOpenDrafts(ctx, id); return err }`. Rename `repoService` to `projectService` (`Register` uses `sgitv1.RegisterProjectServer(g, &projectService{s: s})`).

- [ ] **Step 7: Reads** (`internal/server/read.go`). `open` is the **first** thing every handler does, before validating any other request field, so the error for an unknown or inaccessible project never depends on the rest of the request. Every handler calls `pr, _, err := r.s.open(ctx, req.Project, auth.ActionRead)` (UploadBlobs: `ActionWrite` on the first chunk) and uses `pr.h.Git` where it used `h.git`. Errors from the project repo still go through `toStatus` (they only mention project objects). Add the shared resolver and use it in `ResolveRev`, `Log`, `Blame` and both sides of `Diff`:

```go
// resolve accepts any revision of the project repo, plus a draft id.
func (r *projectService) resolve(ctx context.Context, pr *proj, rev string) (string, error) {
	if rev == "" {
		rev = "main"
	}
	if draftIDRe.MatchString(rev) {
		if sha, err := pr.h.Git.ReadRef(ctx, headRef(rev)); err == nil {
			return sha, nil
		}
	}
	sha, err := pr.h.Git.ResolveRev(ctx, rev)
	if errors.Is(err, gitx.ErrNotFound) {
		return "", status.Errorf(codes.NotFound, "revision %q not found", rev)
	}
	if err != nil {
		return "", r.s.toStatus(ctx, err)
	}
	return sha, nil
}

func (r *projectService) ResolveRev(ctx context.Context, req *sgitv1.ResolveRevRequest) (*sgitv1.ResolveRevResponse, error) {
	pr, _, err := r.s.open(ctx, req.Project, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	sha, err := r.resolve(ctx, pr, req.Rev)
	if err != nil {
		return nil, err
	}
	return &sgitv1.ResolveRevResponse{Sha: sha, ProjectId: pr.p.ID, ProjectName: pr.p.Name}, nil
}

func (r *projectService) ListProjects(ctx context.Context, _ *sgitv1.ListProjectsRequest) (*sgitv1.ListProjectsResponse, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no user")
	}
	resp := &sgitv1.ListProjectsResponse{}
	if u.Admin {
		ps, err := r.s.store.ListProjects(ctx)
		if err != nil {
			return nil, r.s.toStatus(ctx, err)
		}
		for _, p := range ps {
			resp.Projects = append(resp.Projects, &sgitv1.ProjectSummary{Id: p.ID, Name: p.Name, Aliases: p.Aliases, Level: "admin"})
		}
		return resp, nil
	}
	acc, err := r.s.store.ListAccess(ctx, u.ID)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	for _, a := range acc {
		lvl := "read"
		if a.Level == store.LevelWrite {
			lvl = "write"
		}
		resp.Projects = append(resp.Projects, &sgitv1.ProjectSummary{Id: a.Project.ID, Name: a.Project.Name, Aliases: a.Project.Aliases, Level: lvl})
	}
	return resp, nil
}
```

`Log`/`Blame` call `r.resolve(ctx, pr, rev)` and pass the SHA to gitx; `Diff` resolves `req.A` and `req.B`.

- [ ] **Step 8: Drafts** (`internal/server/drafts.go`): every function takes `pr *proj` instead of `h *repoHandle`, stores `ProjectID: pr.p.ID`, reads with `GetDraft(ctx, pr.p.ID, id)`, and locks `pr.h.LandMu` where it locked `h.landMu`. `draftPB(d store.Draft, project string)` sets `Project: project` (the canonical `pr.p.Name`). `buildDraftCommit` gains the project checks:

```go
	if ok, err := r.s.store.IsProjectCommit(ctx, pr.p.ID, base); err != nil {
		return "", r.s.toStatus(ctx, err)
	} else if !ok {
		return "", status.Errorf(codes.InvalidArgument, "base commit %s not found", base)
	}
	gc, err := r.s.convertChanges(ctx, pr.h.Git, changes) // convertChanges now takes *gitx.Repo
	if err != nil {
		return "", err
	}
	paths := make([]string, len(gc))
	for i, c := range gc {
		paths[i] = c.Path
	}
	if err := checkPaths(pr.mounts, paths); err != nil {
		return "", err
	}
```

- [ ] **Step 9: Land** (`internal/server/land.go`). Extract the CAS loop into `landSource` (the body of today's `land` from `for attempt` on, unchanged except: no draft bookkeeping, `errAlreadyOnMain` instead of the FailedPrecondition, conflicts returned instead of `conflictStatus`):

```go
var errAlreadyOnMain = errors.New("changes already on main")

// landSource lands head (a commit whose parent is base) onto main of a
// source repo: compare-and-swap, with a merge-tree rebase when main moved.
func (s *Server) landSource(ctx context.Context, h *repoHandle, base, head string) (string, []gitx.ConflictFile, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()
	dc, err := h.git.ReadCommit(ctx, head)
	if err != nil {
		return "", nil, err
	}
	for attempt := 0; attempt < s.cfg.LandRetries; attempt++ {
		mainOld, err := h.git.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", nil, err
		}
		tree := dc.Tree
		if mainOld != base {
			mr, err := h.git.MergeTree(ctx, base, mainOld, head)
			if err != nil {
				return "", nil, err
			}
			if !mr.Clean {
				return "", mr.Conflicts, nil
			}
			tree = mr.Tree
		}
		mc, err := h.git.ReadCommit(ctx, mainOld)
		if err != nil {
			return "", nil, err
		}
		if tree == mc.Tree {
			return mainOld, nil, errAlreadyOnMain
		}
		committer := dc.Committer
		committer.When = s.cfg.Now()
		newC, err := h.git.CommitTree(ctx, tree, []string{mainOld}, dc.Message, dc.Author, committer)
		if err != nil {
			return "", nil, err
		}
		bg := context.WithoutCancel(ctx)
		if err := s.store.RecordLand(bg, h.name, newC, s.cfg.Now()); err != nil {
			return "", nil, err
		}
		if s.cfg.BeforeCAS != nil {
			s.cfg.BeforeCAS(h.name)
		}
		err = h.git.UpdateRef(bg, gitx.MainRef, newC, mainOld)
		if errors.Is(err, gitx.ErrRefMoved) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		s.mirror.Notify(h.name)
		return newC, nil, nil
	}
	return "", nil, status.Errorf(codes.ResourceExhausted, "main kept moving; gave up after %d attempts", s.cfg.LandRetries)
}
```

The project land. `Land` is `open(ctx, req.Project, auth.ActionWrite)` followed by `return r.landDraft(ctx, pr, u, req.Id)`; `CommitAndLand` opens the same way, creates or updates the draft exactly as today (with `pr`), then calls `r.landDraft(ctx, pr, u, d.ID)`:

```go
// repoChanges is the part of a draft that lands in one source repo.
type repoChanges struct {
	repo    string
	mounts  []string      // mount paths touched
	project []gitx.Change // project paths (for rebuilding a partial draft)
	source  []gitx.Change // source paths
}

// splitByRepo maps project changes to source changes, grouped by repo and
// sorted by repo name, enforcing §6.1 again (mounts may have changed).
func splitByRepo(mounts []store.Mount, changes []gitx.Change) ([]*repoChanges, error) {
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	if err := checkPaths(mounts, paths); err != nil {
		return nil, err
	}
	by := map[string]*repoChanges{}
	for _, c := range changes {
		m, _ := mountFor(mounts, c.Path)
		g := by[m.Repo]
		if g == nil {
			g = &repoChanges{repo: m.Repo}
			by[m.Repo] = g
		}
		if !slices.Contains(g.mounts, m.Path) {
			g.mounts = append(g.mounts, m.Path)
		}
		g.project = append(g.project, c)
		sc := c
		sc.Path = sourcePath(m, c.Path)
		g.source = append(g.source, sc)
	}
	out := make([]*repoChanges, 0, len(by))
	for _, g := range by {
		sort.Strings(g.mounts)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].repo < out[j].repo })
	return out, nil
}

// landDraft implements §6.3 (and, from Task 8, §6.4) for an owned open draft.
func (r *projectService) landDraft(ctx context.Context, pr *proj, u store.User, id string) (*sgitv1.LandResponse, error) {
	pr.h.LandMu.Lock()
	defer pr.h.LandMu.Unlock()
	// 1. Catch up (open did) and 2. re-check access now: a grant revoked or
	// downgraded after the draft was made blocks the land.
	if err := auth.Authorize(ctx, r.s.store, u, pr.p.ID, auth.ActionWrite); err != nil {
		if errors.Is(err, auth.ErrNoAccess) {
			return nil, auth.ProjectNotFound(pr.ref)
		}
		return nil, err
	}
	d, err := r.loadOwnedOpenDraft(ctx, pr, u, id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	if cur, err := pr.h.Git.ReadRef(ctx, headRef(d.ID)); err != nil || cur != d.Head {
		return nil, status.Errorf(codes.Aborted, "draft %s is inconsistent (head ref moved); update it and retry", id)
	}
	dc, err := pr.h.Git.ReadCommit(ctx, d.Head)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	// 3. Diff against the project base and map to mounts.
	changes, err := pr.h.Git.DiffTree(ctx, d.Base, d.Head)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	groups, err := splitByRepo(pr.mounts, changes)
	if err != nil {
		return nil, err
	}
	if len(groups) > 1 {
		return nil, status.Error(codes.FailedPrecondition, "this draft spans several mounts from different sources; land them separately") // removed in Task 8
	}
	landedAny := false
	for _, g := range groups {
		conflicts, err := r.landGroup(ctx, pr, d, dc, g)
		if errors.Is(err, errAlreadyOnMain) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 {
			head, cerr := r.s.projects.CatchUp(ctx, pr.h)
			if cerr != nil {
				return nil, r.s.hide(ctx, cerr)
			}
			return nil, conflictStatus(d.ID, head, conflicts, false, nil)
		}
		landedAny = true
	}
	if !landedAny {
		return nil, status.Errorf(codes.FailedPrecondition, "draft %s changes are already on main", id)
	}
	// 5. Catch up and answer with the new project head.
	head, err := r.s.projects.CatchUp(ctx, pr.h)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	bg := context.WithoutCancel(ctx)
	d.Status, d.LandedAs, d.UpdatedAt = store.DraftLanded, head, r.s.cfg.Now()
	if err := r.s.store.UpdateDraft(bg, d); err != nil {
		r.s.log.ErrorContext(bg, "landed but failed to mark draft", "draft", d.ID, "err", err)
	}
	parent := ""
	if hc, err := pr.h.Git.ReadCommit(ctx, head); err == nil && hc.Tree == dc.Tree && len(hc.Parents) == 1 {
		parent = hc.Parents[0]
	}
	return &sgitv1.LandResponse{Commit: head, DraftId: d.ID, Parent: parent}, nil
}

// landGroup builds the source commit for one repo on the source base recorded
// for the draft's project base (§6.3 step 4) and lands it. Conflicts come
// back with project paths. Source-side failures are hidden.
func (r *projectService) landGroup(ctx context.Context, pr *proj, d store.Draft, dc gitx.Commit, g *repoChanges) ([]gitx.ConflictFile, error) {
	srcBase, err := r.s.store.ProjectSource(ctx, pr.p.ID, d.Base, g.repo)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.FailedPrecondition, "draft %s was made before mount %s/ was added; sync and commit again", d.ID, g.mounts[0])
	}
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	rh, err := r.s.repos.get(ctx, g.repo)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	var blobs []string
	for _, c := range g.source {
		if c.Op != gitx.OpDelete {
			blobs = append(blobs, c.BlobSHA)
		}
	}
	if _, err := gitx.CopyObjects(ctx, pr.h.Git, rh.git, blobs, nil); err != nil {
		return nil, r.s.hide(ctx, err)
	}
	tree, err := rh.git.BuildTree(ctx, srcBase, g.source)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	sc, err := rh.git.CommitTree(ctx, tree, []string{srcBase}, dc.Message, dc.Author, dc.Committer)
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	_, conflicts, err := r.s.landSource(ctx, rh, srcBase, sc)
	if errors.Is(err, errAlreadyOnMain) {
		return nil, err
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted {
		return nil, err // our own message, safe
	}
	if err != nil {
		return nil, r.s.hide(ctx, err)
	}
	for i := range conflicts {
		p, ok := projectPath(pr.mounts, g.repo, conflicts[i].Path)
		if !ok { // a conflict outside the touched folders is impossible; never leak it
			return nil, r.s.hide(ctx, fmt.Errorf("conflict outside mounts in %s", g.repo))
		}
		conflicts[i].Path = p
	}
	return conflicts, nil
}
```

`conflictStatus` gains the partial fields: `func conflictStatus(draftID, main string, files []gitx.ConflictFile, partial bool, landed []string) error` (message `conflict in %d file(s)`; detail sets `Partial` and `Landed`). The blob SHAs in `ConflictFile` are readable through the project: base is in the draft base's mount trees, theirs was uploaded, ours is on source main and was copied by the catch-up that runs before `conflictStatus`.

- [ ] **Step 10: Admin ACL by project** (`internal/server/admin.go`): `setACL(ctx, user, ref string, lvl)` resolves the project with `a.s.store.ResolveProject` (NotFound → `auth.ProjectNotFound(ref)`) and calls `SetACL(ctx, u.ID, p.ID, lvl)`. `ListACL` resolves `req.Project` when set and passes its id; entries carry `Project: row.Project`. `projectInfo` fills `Grants` from `ListACL(ctx, "", p.ID)`. `DeleteProject` returns the two counts from `DeleteProjectRows`.

- [ ] **Step 11: testenv** (`internal/testenv/testenv.go`):

```go
// NewUser creates a user and returns its token. If level is "read" or
// "write", it is granted on project.
func (e *Env) NewUser(name, project, level string) string {
	e.t.Helper()
	ctx := context.Background()
	adm := e.Admin(e.AdminToken)
	tok := Must(adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: name}))
	if level != "" {
		Must(adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: name, Project: project, Level: level}))
	}
	return tok.Token
}

// CreateRepoProject creates repo name and project name mounting the whole
// repo at ".", and returns the project head (equal to the repo's main).
func (e *Env) CreateRepoProject(name string) string {
	e.t.Helper()
	e.CreateRepo(name)
	e.CreateProject(name)
	return e.Mount(name, ".", name, ".", false)
}

func (e *Env) ProjectID(ref string) int64 {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).GetProject(context.Background(), &sgitv1.GetProjectRequest{Project: ref})).Id
}
```

- [ ] **Step 12: Mechanical test updates.** Client-side tests that set up a repo and then talk to it as a client switch to a whole-repo project, which keeps every SHA the same (a `.` mount reproduces the source history):
  - `internal/server/*_test.go`, `internal/daemon/*_test.go`, `internal/fetch/fetch_test.go`, `internal/cli/*_test.go` (via `harness.seed`): replace `e.CreateRepo("X")` by `e.CreateRepoProject("X")` wherever the test then calls a `Project` RPC on `"X"`. Admin-only tests (repo create/import/status/list/delete, mirror) keep `CreateRepo`.
  - `harness.seed` becomes `h.env.CreateRepoProject("web"); return h.env.CommitDirect("web", files, "seed")`.
  - Tests that read the land result from the source repo (`e.Files("web", "main")`, `assertLinear`) keep working for `.` mounts.
  - `TestAuthorizationMatrix`: the `stranger` cases now expect NotFound `project "web" not found or no access` instead of PermissionDenied; the `read-only` cases stay PermissionDenied.
  - CLI admin tests that run `sg acl grant <user> web …` call `h.env.CreateProject("web")` and `h.env.Mount("web", ".", "web", ".", false)` first (Task 9 replaces this with `sg project` commands). Tests that `sg repo delete` a repo that is now mounted delete the project first with `h.env.Admin(h.env.AdminToken).DeleteProject`.
  - `store.DeleteUser` tests: create a project and grant on its id.

- [ ] **Step 13: Run everything**

Run: `go test -race -count=1 ./...`
Expected: PASS. Then `grep -rn '"repo %q\|in repo %q' internal/server internal/store` must show only admin-only code paths (repo admin RPCs and `repoManager`).

- [ ] **Step 14: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./...
git add -A && git commit -m "server: per-project access, reads from project repos, writes mapped through mounts"
```

---

### Task 8: Cross-repo best-effort land

**Files:**
- Modify: `internal/server/land.go`
- Create: `internal/server/landmulti_test.go`

**Interfaces:**
- Consumes: `landDraft`, `landGroup`, `splitByRepo`, `conflictStatus`, `headRef`, `versionRef`, `store.UpdateDraft`, `Engine.CatchUp`.
- Produces: `func (r *projectService) rebuildDraft(ctx context.Context, pr *proj, d store.Draft, dc gitx.Commit, rest []*repoChanges, head string) (store.Draft, error)`; `func (s *Server) safeMessage(ctx context.Context, err error) string`. A land touching several repos lands them one by one in repo-name order and stops at the first conflict or error, answering Aborted + `ConflictDetail{partial: true, landed: [...]}` when something landed.

- [ ] **Step 1: Write the failing tests** `internal/server/landmulti_test.go`:

```go
package server_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// twoRepos makes repos "a" and "b" (each with f.txt) and project "web"
// mounting them at a/ and b/.
func twoRepos(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	e := testenv.Start(t)
	for _, r := range []string{"a", "b"} {
		e.CreateRepo(r)
		e.CommitDirect(r, map[string]string{"f.txt": "base " + r + "\n"}, "seed "+r)
	}
	e.CreateProject("web")
	e.Mount("web", "a", "a", ".", false)
	return e, e.Mount("web", "b", "b", ".", false)
}

func countSubject(t *testing.T, e *testenv.Env, repo, subject string) int {
	t.Helper()
	g := e.Git(repo)
	defer g.Close()
	n := 0
	for _, l := range testenv.Must(g.Log(context.Background(), "main", "", 100)) {
		if l.Subject == subject {
			n++
		}
	}
	return n
}

func TestCrossRepoLand(t *testing.T) {
	ctx := context.Background()
	e, head := twoRepos(t)
	c := e.Project(e.AdminToken)
	shas := e.Upload(e.AdminToken, "web", []byte("A\n"), []byte("B\n"))
	resp := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}}))
	if e.Files("a", "main")["f.txt"] != "A\n" || e.Files("b", "main")["f.txt"] != "B\n" {
		t.Fatal("not landed in both repos")
	}
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web", N: 3}))
	if lg.Entries[0].Sha != resp.Commit || lg.Entries[0].Subject != "both" || lg.Entries[1].Subject != "both" {
		t.Fatalf("project log = %+v (one commit per repo)", lg.Entries)
	}
}

func TestPartialLandThenRetryLandsRest(t *testing.T) {
	ctx := context.Background()
	e, head := twoRepos(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	e.CommitDirect("b", map[string]string{"f.txt": "theirs b\n"}, "upstream b")
	shas := e.Upload(alice, "web", []byte("ours a\n"), []byte("ours b\n"))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("partial land = %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	now := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if det == nil || !det.Partial || !reflect.DeepEqual(det.Landed, []string{"a"}) || det.Main != now ||
		len(det.Files) != 1 || det.Files[0].Path != "b/f.txt" {
		t.Fatalf("detail = %+v", det)
	}
	if e.Files("a", "main")["f.txt"] != "ours a\n" {
		t.Fatal("repo a did not land")
	}
	// The draft now holds only what did not land, on the new head, same id.
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: det.DraftId}))
	if d.Base != now || d.Status != "open" {
		t.Fatalf("draft after partial = %+v", d)
	}
	patch := string(testenv.Must(c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: d.Base, B: d.Head})).Patch)
	if !strings.Contains(patch, "b/f.txt") || strings.Contains(patch, "a/f.txt") {
		t.Fatalf("draft diff = %s", patch)
	}
	// Resolve and retry: only b lands; a is never landed twice.
	fixed := e.Upload(alice, "web", []byte("merged b\n"))[0]
	testenv.Must(c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: d.Id, Base: now, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("b/f.txt", fixed)}}))
	testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}))
	if e.Files("b", "main")["f.txt"] != "merged b\n" {
		t.Fatal("retry did not land b")
	}
	if n := countSubject(t, e, "a", "both"); n != 1 {
		t.Fatalf("repo a has %d commits from the draft, want 1", n)
	}
}

func TestFirstRepoConflictIsNotPartial(t *testing.T) {
	ctx := context.Background()
	e, head := twoRepos(t)
	c := e.Project(e.AdminToken)
	e.CommitDirect("a", map[string]string{"f.txt": "theirs a\n"}, "upstream a")
	shas := e.Upload(e.AdminToken, "web", []byte("ours a\n"), []byte("ours b\n"))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}})
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	if det == nil || det.Partial || len(det.Landed) != 0 {
		t.Fatalf("detail = %+v", det)
	}
	if e.Files("b", "main")["f.txt"] != "base b\n" {
		t.Fatal("repo b landed after repo a conflicted")
	}
}

func TestConcurrentLandsThroughTwoProjects(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/seed": "s"}, "seed")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	e.CreateProject("api")
	e.Mount("api", "code", "core", "src", false)
	const n = 10
	c := e.Project(e.AdminToken) // one client, shared by the goroutines
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		for _, pp := range []struct{ project, dir string }{{"web", "lib"}, {"api", "code"}} {
			wg.Add(1)
			go func(i int, project, dir string) {
				defer wg.Done()
				rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: project})
				if err != nil {
					errs <- err
					return
				}
				name := fmt.Sprintf("%s-%d", project, i)
				shas, err := rpc.UploadBlobs(ctx, c, project, [][]byte{[]byte(name)})
				if err != nil {
					errs <- err
					return
				}
				_, err = c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: project, Base: rr.Sha, Message: name,
					Changes: []*sgitv1.Change{testenv.Add(dir+"/"+name, shas[0])}})
				errs <- err
			}(i, pp.project, pp.dir)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent land: %v", err)
		}
	}
	files := e.Files("core", "main")
	if len(files) != 2*n+1 {
		t.Fatalf("source has %d files, want %d", len(files), 2*n+1)
	}
	for _, p := range []string{"web", "api"} {
		g := e.ProjectGit(e.ProjectID(p))
		testenv.Must(e.Project(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: p})) // catch up
		if got := len(testenv.Must(g.ListFiles(ctx, "main"))); got != 2*n+1 {
			t.Errorf("project %s has %d files", p, got)
		}
		g.Close()
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run 'CrossRepo|PartialLand|FirstRepoConflict|ConcurrentLandsThroughTwo' -count=1`
Expected: FAIL (`spans several mounts from different sources`).

- [ ] **Step 3: Implement.** In `landDraft`, delete the `len(groups) > 1` guard and replace the loop with:

```go
	var landed []string // mount paths that landed
	landedAny := false
	for i, g := range groups {
		conflicts, err := r.landGroup(ctx, pr, d, dc, g)
		if errors.Is(err, errAlreadyOnMain) {
			continue
		}
		if err == nil && len(conflicts) == 0 {
			landed = append(landed, g.mounts...)
			landedAny = true
			continue
		}
		// Stop at the first conflict or error (§6.4).
		head, cerr := r.s.projects.CatchUp(ctx, pr.h)
		if cerr != nil {
			return nil, r.s.hide(ctx, cerr)
		}
		if !landedAny {
			if err != nil {
				return nil, err
			}
			return nil, conflictStatus(d.ID, head, conflicts, false, nil)
		}
		if _, rerr := r.rebuildDraft(ctx, pr, d, dc, groups[i:], head); rerr != nil {
			return nil, r.s.hide(ctx, rerr)
		}
		if err != nil {
			st := status.New(codes.Aborted, fmt.Sprintf("landed %s; the rest did not land: %s", strings.Join(landed, ", "), r.s.safeMessage(ctx, err)))
			withDet, derr := st.WithDetails(&sgitv1.ConflictDetail{DraftId: d.ID, Main: head, Partial: true, Landed: landed})
			if derr != nil {
				return nil, st.Err()
			}
			return nil, withDet.Err()
		}
		return nil, conflictStatus(d.ID, head, conflicts, true, landed)
	}
```

```go
// rebuildDraft re-creates the draft on head with only the changes of the
// groups that did not land, keeping its id (§6.4).
func (r *projectService) rebuildDraft(ctx context.Context, pr *proj, d store.Draft, dc gitx.Commit, rest []*repoChanges, head string) (store.Draft, error) {
	files, err := pr.h.Git.ListFiles(ctx, head)
	if err != nil {
		return d, err
	}
	var changes []gitx.Change
	for _, g := range rest {
		for _, c := range g.project {
			if c.Op == gitx.OpDelete {
				if _, ok := files[c.Path]; !ok {
					continue // already gone upstream
				}
			}
			changes = append(changes, c)
		}
	}
	tree, err := pr.h.Git.BuildTree(ctx, head, changes)
	if err != nil {
		return d, err
	}
	committer := dc.Committer
	committer.When = r.s.cfg.Now()
	c, err := pr.h.Git.CommitTree(ctx, tree, []string{head}, dc.Message, dc.Author, committer)
	if err != nil {
		return d, err
	}
	bg := context.WithoutCancel(ctx)
	if err := pr.h.Git.UpdateRef(bg, headRef(d.ID), c, d.Head); err != nil {
		return d, err
	}
	d.Version++
	if err := pr.h.Git.UpdateRef(bg, versionRef(d.ID, d.Version), c, ""); err != nil {
		return d, err
	}
	d.Base, d.Head, d.UpdatedAt = head, c, r.s.cfg.Now()
	return d, r.s.store.UpdateDraft(bg, d)
}

// safeMessage returns err's message when it is one of our own status errors
// (safe for clients), otherwise logs it and returns a request-id reference.
func (s *Server) safeMessage(ctx context.Context, err error) string {
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Code() != codes.Internal {
		return st.Message()
	}
	return status.Convert(s.hide(ctx, err)).Message()
}
```

`landGroup` already hides source-side errors, so `err` reaching `safeMessage` is either one of our statuses (ResourceExhausted, FailedPrecondition) or a hidden Internal.

- [ ] **Step 4: Run the server tests**

Run: `go test -race -count=1 ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "server: best-effort cross-repo land with partial results and draft rebuild"
```

---

### Task 9: CLI admin commands: `sg project …` and `sg acl …` by project

**Files:**
- Create: `internal/cli/project.go`, `internal/cli/project_test.go`
- Modify: `internal/cli/admin.go` (`aclUsage`, `cmdACL`, `aclList`, `repoDelete` message), `internal/cli/cli.go` (command table, usage footer); CLI admin tests from Task 7 switch to `sg project` commands

**Interfaces:**
- Consumes: `(a *app) admin(ctx, server, timeout, fn)`, `a.newFlags`, `parse`, `usageErr`, `a.emit`, `short`, `exitf`; Admin project RPCs (Tasks 6, 7).
- Produces: `func cmdProject(ctx context.Context, a *app, args []string) error`; `const projectUsage`; JSON shapes:
  ```go
  type mountJSON struct{ Path, Repo, Folder string; ReadOnly bool }        // json: path, repo, folder, read_only
  type grantJSON struct{ User, Level string }                              // json: user, level
  type projectJSON struct{ ID int64; Name string; Aliases []string; Head string; Mounts []mountJSON; Grants []grantJSON } // json: id, name, aliases, head, mounts, grants
  ```

```go
const projectUsage = "project create <name> | project mount <project> <path> <repo>:<folder> [--read-only] | project unmount <project> <path> | " +
	"project alias add <project> <alias> | project alias rm <alias> | project rename <old> <new> | project list | project show <project> | " +
	"project delete <project> --yes  [--server S]"
const aclUsage = "acl grant <user> <project> read|write | acl revoke <user> <project> | acl list [--user U] [--project P]  [--server S]"
```

Text output, exactly:
- `project create web` → `created project web (id 3)`
- `project mount web lib core:src [--read-only]` → `mounted core:src at lib/ in web` (+ ` (read-only)`)
- `project unmount web lib` → `unmounted lib/ from web`
- `project alias add web site` → `added alias site for web`; `project alias rm site` → `removed alias site (was web)`
- `project rename web www` → `renamed web to www (web is kept as an alias)`
- `project list` → tabwriter table `NAME  ALIASES  MOUNTS`; aliases comma-joined or `-`; mounts as `path=repo:folder` comma-joined, sorted by path, read-only ones suffixed `(ro)`, or `-`
- `project show www` → lines `project: www (id 3)`, `aliases: site, web` (or `-`), `head:    <short sha>` (or `-`), `mounts:` then `  <path>/  <repo>:<folder>[  read-only]` per mount, `grants:` then `  <user>  <level>` per grant (or `  -`)
- `project delete www --yes` → `deleted project www (2 drafts, 1 grants removed)`; without `--yes`: exit 1 `refusing to delete project "www" without --yes (this removes its aliases, mounts, grants, drafts and derived repo)`
- `acl grant bob web write` → `granted bob write access to web`; `acl revoke bob web` → `revoked bob's access to web`; `acl list` → `USER  PROJECT  LEVEL`

- [ ] **Step 1: Write the failing tests** `internal/cli/project_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'ProjectCommands' -count=1`
Expected: FAIL (`unknown command "project"`).

- [ ] **Step 3: Implement `internal/cli/project.go`.** Dispatch like `cmdRepo`:

```go
func cmdProject(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return projectCreate(ctx, a, args[1:])
		case "mount":
			return projectMount(ctx, a, args[1:])
		case "unmount":
			return projectUnmount(ctx, a, args[1:])
		case "alias":
			return projectAlias(ctx, a, args[1:])
		case "rename":
			return projectRename(ctx, a, args[1:])
		case "list":
			return projectList(ctx, a, args[1:])
		case "show":
			return projectShow(ctx, a, args[1:])
		case "delete":
			return projectDelete(ctx, a, args[1:])
		}
	}
	return usageErr(projectUsage)
}

func projectMount(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("project mount")
	ro := fs.Bool("read-only", false, "refuse writes to this mount, from everyone")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		return usageErr(projectUsage)
	}
	repo, folder, ok := strings.Cut(pos[2], ":")
	if !ok || repo == "" || folder == "" {
		return usageErr(projectUsage)
	}
	var info *sgitv1.ProjectInfo
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		info, err = ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: pos[0], Path: pos[1], Repo: repo, Folder: folder, ReadOnly: *ro})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(toProjectJSON(info), func(w io.Writer) {
		suffix := ""
		if *ro {
			suffix = " (read-only)"
		}
		fmt.Fprintf(w, "mounted %s:%s at %s/ in %s%s\n", repo, folder, pos[1], info.Name, suffix)
	})
}
```

The other subcommands follow `repoCreate`'s shape (one RPC inside `a.admin`, `toProjectJSON` for `--json`, the exact text above). `project alias` dispatches `add` (2 positionals) and `rm` (1 positional). Add `{"project", projectUsage, cmdProject}` to `commandTable` after `repo`. `cmdACL` sends `Project: pos[1]`; `aclList` takes `--project` (and no longer `--repo`) and prints `USER PROJECT LEVEL`. The usage footer becomes: `Admin commands need an admin token. acl list shows explicit grants only; admins have implicit access to every project.`

- [ ] **Step 4: Replace the Task 7 stopgaps** in CLI admin tests (`h.env.CreateProject`, `h.env.Mount` before `acl grant`) with `h.ok(dir, "project", "create", …)` / `h.ok(dir, "project", "mount", …)`, and change `--repo` to `--project` in `acl list` assertions (`admin_manage_test.go`).

- [ ] **Step 5: Run the CLI tests**

Run: `go test -race -count=1 ./internal/cli/`
Expected: PASS.

- [ ] **Step 6: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "cli: sg project commands; sg acl by project"
```

---

### Task 10: CLI client: projects only

**Files:**
- Create: `internal/cli/projects.go`, `internal/cli/projects_test.go`
- Modify: `internal/wc/wc.go`, `internal/wc/wc_test.go`, `internal/cli/session.go`, `cli.go`, `checkout.go`, `status.go`, `sync.go`, `land.go`, `commit.go`, `drafts.go`, `history.go`, `sparse.go`, `diff.go`; CLI tests that pass `--repo`

**Interfaces:**
- Consumes: `ResolveRevResponse.ProjectId/ProjectName`, `ListProjects`, `ConflictDetail.Partial/Landed` (Task 5); `auth.ProjectNotFound` message shape.
- Produces:
  ```go
  // wc
  type Config struct {
  	Server  string   `toml:"server"`
  	Project int64    `toml:"project"`
  	Name    string   `toml:"name,omitempty"` // display cache of the canonical name
  	Sparse  []string `toml:"sparse,omitempty"`
  }
  // cli
  type session struct{ conn, cached *grpc.ClientConn; proj sgitv1.ProjectClient; admin sgitv1.AdminClient; server, project string; wc *wc.WC }
  type target struct{ server, project string; needWC, needProject, adminOnly bool }
  func cmdProjects(ctx context.Context, a *app, args []string) error
  ```
  `session.project` is the reference sent to the server: the decimal id from `.sg/config` inside a working copy, otherwise the `--project` value.

- [ ] **Step 1: Write the failing tests** `internal/cli/projects_test.go`:

```go
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/testenv"
)

// coreProject: repo "zz-src" (lib code under src/, private/ unmounted) and
// project "web" (alias "site") mounting zz-src:src at lib, bob with write.
func coreProject(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.env.CreateRepo("zz-src")
	h.env.CommitDirect("zz-src", map[string]string{"src/a.txt": "a\n", "private/p.txt": "p\n"}, "seed")
	h.env.CreateProject("web")
	h.env.Mount("web", "lib", "zz-src", "src", false)
	testenv.Must(h.env.Admin(h.env.AdminToken).AddProjectAlias(t.Context(), &sgitv1.AddProjectAliasRequest{Project: "web", Alias: "site"}))
	bob := h.env.NewUser("bob", "web", "write")
	h.loginAs("bob", bob)
	return h, bob
}

func TestCheckoutByAliasStoresProjectID(t *testing.T) {
	h, _ := coreProject(t)
	root := t.TempDir()
	var res map[string]any
	if err := json.Unmarshal([]byte(h.okAs("bob", root, "checkout", "site", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if res["project"] != "web" || res["dir"] != filepath.Join(root, "web") {
		t.Fatalf("checkout json = %v", res)
	}
	w := filepath.Join(root, "web")
	cfg := readFile(t, filepath.Join(w, ".sg", "config"))
	id := h.env.ProjectID("web")
	if !strings.Contains(cfg, "project = "+strconv.FormatInt(id, 10)) || strings.Contains(cfg, "zz-src") {
		t.Fatalf(".sg/config = %q", cfg)
	}
	if readFile(t, filepath.Join(w, "lib", "a.txt")) != "a\n" {
		t.Fatal("file missing")
	}
	if _, err := os.Stat(filepath.Join(w, "private")); err == nil {
		t.Fatal("unmounted folder checked out")
	}
	h.ok(t.TempDir(), "project", "rename", "web", "www")
	writeFile(t, filepath.Join(w, "lib", "b.txt"), "b\n")
	h.okAs("bob", w, "commit", "-m", "add b", "--land")
	h.okAs("bob", w, "sync")
	if out := h.okAs("bob", w, "status"); !strings.HasPrefix(out, "On www@") {
		t.Fatalf("status after rename = %q", out)
	}
	if h.env.Files("zz-src", "main")["src/b.txt"] != "b\n" {
		t.Fatal("land did not reach the source")
	}
}

func TestProjectsCommand(t *testing.T) {
	h, _ := coreProject(t)
	h.env.CreateProject("other")
	var res struct {
		Projects []struct {
			ID      int64    `json:"id"`
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
			Level   string   `json:"level"`
		} `json:"projects"`
	}
	decode(t, h.okAs("bob", t.TempDir(), "projects", "--json"), &res)
	if len(res.Projects) != 1 || res.Projects[0].Name != "web" || res.Projects[0].Level != "write" || res.Projects[0].Aliases[0] != "site" {
		t.Fatalf("bob projects = %+v", res)
	}
	decode(t, h.ok(t.TempDir(), "projects", "--json"), &res)
	if len(res.Projects) != 2 || res.Projects[0].Level != "admin" {
		t.Fatalf("admin projects = %+v", res)
	}
	out := h.okAs("bob", t.TempDir(), "projects")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || strings.Join(strings.Fields(lines[1]), " ") != "web site write" {
		t.Fatalf("projects text = %q", out)
	}
}

func TestNoAccessExitsTwo(t *testing.T) {
	h, _ := coreProject(t)
	h.env.CreateProject("api")
	for _, p := range []string{"api", "nope"} {
		code, _, errs := h.runAs("bob", t.TempDir(), "log", "--project", p)
		if code != cli.ExitAuth || errs != "sg: project \""+p+"\" not found or no access\n" {
			t.Errorf("log --project %s: exit %d, %q", p, code, errs)
		}
	}
	if code, _, _ := h.runAs("bob", t.TempDir(), "checkout", "api"); code != cli.ExitAuth {
		t.Errorf("checkout api: exit %d", code)
	}
}

func TestPartialLandCLI(t *testing.T) {
	h := newHarness(t)
	for _, r := range []string{"a", "b"} {
		h.env.CreateRepo(r)
		h.env.CommitDirect(r, map[string]string{"f.txt": "base\n"}, "seed")
	}
	h.env.CreateProject("web")
	h.env.Mount("web", "a", "a", ".", false)
	h.env.Mount("web", "b", "b", ".", false)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	h.env.CommitDirect("b", map[string]string{"f.txt": "theirs\n"}, "upstream")
	writeFile(t, filepath.Join(w, "a", "f.txt"), "ours a\n")
	writeFile(t, filepath.Join(w, "b", "f.txt"), "ours b\n")
	code, out, _ := h.run(w, "commit", "-m", "both", "--land", "--json")
	if code != cli.ExitConflict {
		t.Fatalf("partial land exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res["partial"] != true || res["landed"] != false {
		t.Fatalf("partial json = %v", res)
	}
	if lm, _ := res["landed_mounts"].([]any); len(lm) != 1 || lm[0] != "a" {
		t.Fatalf("landed_mounts = %v", res["landed_mounts"])
	}
	if readFile(t, filepath.Join(w, "a", "f.txt")) != "ours a\n" || !strings.Contains(readFile(t, filepath.Join(w, "b", "f.txt")), "<<<<<<<") {
		t.Fatal("working copy not rebased with markers in b only")
	}
	writeFile(t, filepath.Join(w, "b", "f.txt"), "merged\n")
	h.ok(w, "commit", "-m", "both", "--land")
	if h.env.Files("b", "main")["f.txt"] != "merged\n" || h.env.Files("a", "main")["f.txt"] != "ours a\n" {
		t.Fatal("retry did not land the rest")
	}
}

func TestPreProjectWorkingCopyIsRejected(t *testing.T) {
	h, _ := coreProject(t)
	w := t.TempDir()
	if err := os.MkdirAll(filepath.Join(w, ".sg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w, ".sg", "config"), "server = \""+h.env.Addr+"\"\nrepo = \"web\"\n")
	writeFile(t, filepath.Join(w, ".sg", "base"), strings.Repeat("a", 40)+"\n")
	code, _, errs := h.runAs("bob", w, "sync")
	if code != cli.ExitError || !strings.Contains(errs, "predates projects") {
		t.Fatalf("old working copy: exit %d, %q", code, errs)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'CheckoutByAlias|ProjectsCommand|NoAccessExitsTwo|PartialLandCLI|PreProject' -count=1`
Expected: FAIL (`unknown command "projects"`, `flag provided but not defined: -project`, …).

- [ ] **Step 3: Working copy config** (`internal/wc/wc.go`): replace `Repo string` with `Project int64 \`toml:"project"\`` and `Name string \`toml:"name,omitempty"\``. Update `wc_test.go` fixtures (`Config{Server: "s", Project: 1, Name: "web"}`).

- [ ] **Step 4: Session.** In `internal/cli/session.go` rename `repo`→`proj`, `name`→`project`, `target.repo`→`project`, `needRepo`→`needProject`. In `connect`:

```go
	server, ref := t.server, t.project
	if w != nil {
		if server == "" {
			server = w.Config.Server
		}
		if ref == "" {
			if w.Config.Project == 0 {
				return nil, exitf(ExitError, "%s: this working copy predates projects; check it out again with `sg checkout <project>`", w.Root)
			}
			ref = strconv.FormatInt(w.Config.Project, 10)
		}
	}
	…
	if t.needProject && ref == "" {
		return nil, exitf(ExitError, "no project: run inside a working copy or pass --project")
	}
```

`ownsDraft` compares `strconv.FormatInt(s.wc.Config.Project, 10) == s.project`. Every request literal uses `Project: s.project`; every `s.repo.` call becomes `s.proj.`.

- [ ] **Step 5: Commands.**
  - `cli.go`: commands `land`, `drafts`, `show`, `abandon`, `log`, `blame`, `cat` show `[--project P]` instead of `[--repo R]`; `checkout <project|alias> [dir] …`; add `{"projects", "projects [--server S]", cmdProjects}` after `checkout`. In `exitCode`, before the switch default:

    ```go
    	case codes.NotFound:
    		if strings.HasSuffix(st.Message(), " not found or no access") {
    			return ExitAuth
    		}
    ```
  - every `fs.String("repo", …)` becomes `fs.String("project", "", "project name, alias or id (default: the working copy's)")`.
  - `checkout.go`: the positional is the project reference; `dir` defaults to `rr.ProjectName` (resolved first, so the default dir is the canonical name even for an alias or id); `wc.Config{Server: s.server, Project: rr.ProjectId, Name: rr.ProjectName, Sparse: sparse}`; fetch calls use `strconv.FormatInt(rr.ProjectId, 10)`; JSON `{"project": rr.ProjectName, "dir", "commit", "files"}`; text `checked out <name>@<short> (<n> files) into <dir>`.
  - `status.go`: JSON field `project` (`w.Config.Name`); text `On <name>@<short>`.
  - `sync.go`: after `ResolveRev`, `w.Config.Name = rr.ProjectName` before `syncTo` (saved with the sync).
  - `history.go`: `repoPath` becomes `projectPath` and checks `s.ownsWC()` (`s.wc != nil && strconv.FormatInt(s.wc.Config.Project, 10) == s.project`).
  - `land.go` `finishLand`, conflict branch:

    ```go
    		res, serr := syncTo(ctx, s, det.Main)
    		if serr != nil {
    			return exitf(ExitConflict, "landing conflicted on main %s (draft %s kept); could not rebase the working copy: %v", short(det.Main), det.DraftId, serr)
    		}
    		landed := det.Landed
    		if landed == nil {
    			landed = []string{}
    		}
    		if err := a.emit(map[string]any{"landed": false, "partial": det.Partial, "landed_mounts": landed, "draft": det.DraftId, "main": det.Main, "sync": res}, func(out io.Writer) {
    			if det.Partial {
    				fmt.Fprintf(out, "partially landed: %s/ landed; the rest stays in draft %s\n", strings.Join(det.Landed, "/, "), det.DraftId)
    			}
    			fmt.Fprintf(out, "conflict: draft %s does not apply cleanly to main %s; working copy rebased onto it\n", det.DraftId, short(det.Main))
    			printSync(out, res)
    		}); err != nil {
    			return err
    		}
    ```
  - `projects.go`:

    ```go
    func cmdProjects(ctx context.Context, a *app, args []string) error {
    	fs := a.newFlags("projects")
    	server := fs.String("server", "", "server (default: from sg login)")
    	pos, err := parse(fs, args)
    	if err != nil {
    		return err
    	}
    	if len(pos) != 0 {
    		return usageErr("projects [--server S]")
    	}
    	s, err := a.connect(target{server: *server})
    	if err != nil {
    		return err
    	}
    	defer s.Close()
    	resp, err := s.proj.ListProjects(ctx, &sgitv1.ListProjectsRequest{})
    	if err != nil {
    		return err
    	}
    	type pj struct {
    		ID      int64    `json:"id"`
    		Name    string   `json:"name"`
    		Aliases []string `json:"aliases"`
    		Level   string   `json:"level"`
    	}
    	out := make([]pj, 0, len(resp.Projects))
    	for _, p := range resp.Projects {
    		al := p.Aliases
    		if al == nil {
    			al = []string{}
    		}
    		out = append(out, pj{p.Id, p.Name, al, p.Level})
    	}
    	return a.emit(map[string]any{"projects": out}, func(w io.Writer) {
    		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
    		fmt.Fprintln(tw, "NAME\tALIASES\tLEVEL")
    		for _, p := range out {
    			al := strings.Join(p.Aliases, ",")
    			if al == "" {
    				al = "-"
    			}
    			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, al, p.Level)
    		}
    		tw.Flush()
    	})
    }
    ```

  The `ListProjects` RPC goes through sgd like any `Project` RPC (pass-through, added in Task 11).

- [ ] **Step 6: Update existing CLI tests**: `--repo web` → `--project web` (`history_test.go`, `admin_test.go`, `land_test.go` RPC literals already renamed in Task 5); checkout JSON assertions read `project` instead of `repo` (`TestCheckoutFullAndSparse`: `res.Project != "web"`); `statusJSON` gains `Project string \`json:"project"\`` instead of `Repo`.

- [ ] **Step 7: Run the CLI and wc tests**

Run: `go test -race -count=1 ./internal/cli/ ./internal/wc/`
Expected: PASS. `grep -rn '"repo"\|--repo' internal/cli/*.go | grep -v _test.go` shows only admin `repo` commands.

- [ ] **Step 8: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "cli: projects only: sg projects, --project, project id in .sg/config, partial land output"
```

---

### Task 11: sgd: access memo by project, confirmed cache hits

**Files:**
- Modify: `internal/daemon/objects.go`, `internal/daemon/proxy.go`, `internal/daemon/objects_test.go`

**Interfaces:**
- Consumes: `forward`, `d.up` (`sgitv1.ProjectClient`), `d.cached`, `d.fetch`, `toPB`, `daemontest.Start`, `testenv` helpers (Task 7).
- Produces: `aclKey{token [32]byte; project string}`; `func (d *Daemon) confirm(ctx context.Context, project string, shas []string) (map[string]bool, error)`; `proxy.ListProjects` pass-through. In-flight keys become `project + "\x00" + sha`.

- [ ] **Step 1: Write the failing test** (append to `internal/daemon/objects_test.go`):

```go
func getObjects(c sgitv1.ProjectClient, project string, shas ...string) ([]*sgitv1.Object, error) {
	stream, err := c.GetObjects(context.Background(), &sgitv1.GetObjectsRequest{Project: project, Shas: shas})
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

func TestCacheHitNeedsProjectConfirmation(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"pub/a.txt": "public\n", "secret/k.txt": "top secret\n"}, "seed")
	e.CreateProject("pub")
	e.Mount("pub", "p", "core", "pub", false)
	e.CreateProject("sec")
	e.Mount("sec", "s", "core", "secret", false)
	bob := e.NewUser("bob", "pub", "read")
	d := daemontest.Start(t, e.Addr)
	secret := objfmt.BlobSHA([]byte("top secret\n"))
	public := objfmt.BlobSHA([]byte("public\n"))

	// The admin warms the shared cache with the secret blob.
	if objs, err := getObjects(d.Project(e.AdminToken), "sec", secret); err != nil || len(objs) != 1 {
		t.Fatalf("admin fetch = %v, %v", objs, err)
	}
	if _, ok := d.Cache.Get(secret); !ok {
		t.Fatal("secret blob not cached")
	}
	before := d.Counter.Total()
	// bob asks for it through his own project: nothing may be served.
	objs, err := getObjects(d.Project(bob), "pub", secret, public)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("bob fetch of a foreign cached object = %v", err)
	}
	for _, o := range objs {
		if o.Sha == secret {
			t.Fatal("sgd served another project's cached object")
		}
	}
	// bob's own objects still work, and a warm hit is confirmed, not refetched.
	if objs, err := getObjects(d.Project(bob), "pub", public); err != nil || len(objs) != 1 {
		t.Fatalf("bob own fetch = %v, %v", objs, err)
	}
	mid := d.Counter.Total()
	if objs, err := getObjects(d.Project(bob), "pub", public); err != nil || len(objs) != 1 || d.Counter.Total() != mid {
		t.Fatalf("warm fetch = %v, %v; upstream GetObjects SHAs %d -> %d (before %d)", objs, err, mid, d.Counter.Total(), before)
	}
}
```

(imports: `io`, `objfmt`, `codes`, `status`, `daemontest` if not already there).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run CacheHitNeedsProjectConfirmation -count=1`
Expected: FAIL (`sgd served another project's cached object`).

- [ ] **Step 3: Implement** in `internal/daemon/objects.go`: rename `repo` to `project` throughout (`aclKey.project`, in-flight key `project + "\x00" + s`), and add:

```go
// confirm asks the server which of shas the caller can read in project. A
// cache hit is served only after this check, so the shared cache never
// leaks an object across projects (spec §9).
func (d *Daemon) confirm(ctx context.Context, project string, shas []string) (map[string]bool, error) {
	ok := make(map[string]bool, len(shas))
	if len(shas) == 0 {
		return ok, nil
	}
	resp, err := d.up.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: project, Shas: shas})
	if err != nil {
		return nil, err
	}
	if len(resp.Present) != len(shas) {
		return nil, status.Errorf(codes.Internal, "upstream answered %d of %d objects", len(resp.Present), len(shas))
	}
	for i, s := range shas {
		ok[s] = resp.Present[i]
	}
	return ok, nil
}
```

In `proxy.GetObjects` (`internal/daemon/proxy.go`), replace the serve-while-scanning loop:

```go
	ctx, err := p.d.authorize(stream.Context(), req.Project)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(req.Shas))
	hits := map[string]cache.Object{}
	var hitList, misses []string
	for _, s := range req.Shas {
		if seen[s] {
			continue
		}
		seen[s] = true
		if o, ok := p.d.cached(s); ok {
			hits[s] = o
			hitList = append(hitList, s)
			continue
		}
		misses = append(misses, s)
	}
	confirmed, err := p.d.confirm(ctx, req.Project, hitList)
	if err != nil {
		return err
	}
	for _, s := range hitList {
		if !confirmed[s] {
			misses = append(misses, s) // upstream decides: it answers NotFound
			continue
		}
		if err := stream.Send(toPB(s, hits[s])); err != nil {
			return err
		}
	}
	if len(misses) == 0 {
		return nil
	}
	objs, err := p.d.fetch(ctx, req.Project, misses)
	…
```

Add `func (p *proxy) ListProjects(ctx context.Context, r *sgitv1.ListProjectsRequest) (*sgitv1.ListProjectsResponse, error) { return pass(ctx, r, p.d.up.ListProjects) }`. Note the error for bob's request comes from the upstream `GetObjects` of the unconfirmed SHA (NotFound from the project repo), and that `public` is confirmed in the same batch, so a warm request costs one `HasObjects` and no `GetObjects`. Update `docs/sgd.md` in Task 14.

- [ ] **Step 4: Run the daemon tests**

Run: `go test -race -count=1 ./internal/daemon/ ./internal/cli/`
Expected: PASS (`TestCheckoutUsesDaemonCache` still sees zero upstream `GetObjects` on a warm checkout).

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "sgd: access memo per project; every cache hit confirmed with one batched HasObjects"
```

---

### Task 12: Isolation suite

**Files:**
- Create: `internal/server/isolation_test.go`, `internal/cli/isolation_test.go`

**Interfaces:**
- Consumes: everything above; `fetch` (server tests), `getObjects` is daemon-only so the server test uses `fetch`; `daemontest.Start`; harness helpers.
- Produces: tests only. Any failure here is a bug in the owning task; fix it there (in this task's commit) and keep the test.

- [ ] **Step 1: Write the tests** `internal/server/isolation_test.go`:

```go
package server_test

import (
	"context"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

const hiddenRepo = "zz-hidden-src"

// isoEnv: repo zz-hidden-src with pub/ and secret/, project "web" mounting
// pub at "app", bob with write on web. Returns env, bob's token, and every
// source commit SHA.
func isoEnv(t *testing.T) (*testenv.Env, string, []string) {
	t.Helper()
	e := testenv.Start(t)
	e.CreateRepo(hiddenRepo)
	e.CommitDirect(hiddenRepo, map[string]string{"pub/a.txt": "public a\n", "secret/k.txt": "top secret\n"}, "seed")
	e.CommitDirect(hiddenRepo, map[string]string{"secret/k.txt": "rotated secret\n"}, "rotate secret only")
	e.CreateProject("web")
	e.Mount("web", "app", hiddenRepo, "pub", false)
	bob := e.NewUser("bob", "web", "write")
	return e, bob, sourceSHAs(t, e)
}

func sourceSHAs(t *testing.T, e *testenv.Env) []string {
	t.Helper()
	g := e.Git(hiddenRepo)
	defer g.Close()
	var out []string
	for _, l := range testenv.Must(g.Log(context.Background(), "main", "", 1000)) {
		out = append(out, l.SHA)
	}
	return out
}

// assertClean fails if a client-facing message mentions the hidden repo, a
// source path outside the mount, or a source commit SHA.
func assertClean(t *testing.T, what string, m proto.Message, srcSHAs []string) {
	t.Helper()
	text := prototext.Format(m)
	for _, bad := range append([]string{hiddenRepo, "secret/", "pub/"}, srcSHAs...) {
		if strings.Contains(text, bad) {
			t.Fatalf("%s leaks %q:\n%s", what, bad, text)
		}
	}
}

func TestProjectRootListsOnlyMounts(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	c := e.Project(bob)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	root := testenv.Must(g.Subtrees(ctx, []string{head}, "."))[0]
	// The root tree, as a client fetches it, has exactly one entry: the mount.
	obj := testenv.Must(fetch(t, c, "web", root))[0]
	if obj.Type != "tree" {
		t.Fatalf("root = %+v", obj)
	}
	files := testenv.Must(g.ListFiles(ctx, head))
	if len(files) != 1 || files["app/a.txt"].SHA == "" {
		t.Fatalf("project files = %v, want only app/a.txt", files)
	}
	if app := testenv.Must(g.Subtrees(ctx, []string{head}, "app"))[0]; app == "" {
		t.Fatal("mount path missing from the root")
	}
}

func TestForeignSHAsAreNotFound(t *testing.T) {
	ctx := context.Background()
	e, bob, src := isoEnv(t)
	c := e.Project(bob)
	g := e.Git(hiddenRepo)
	defer g.Close()
	secretTree := testenv.Must(g.Subtrees(ctx, []string{src[0]}, "secret"))[0]
	secretBlob := objfmt.BlobSHA([]byte("rotated secret\n"))
	for _, sha := range append([]string{secretTree, secretBlob}, src...) {
		if _, err := fetch(t, c, "web", sha); status.Code(err) != codes.NotFound {
			t.Errorf("GetObjects(%s) = %v", sha, err)
		}
		has := testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: []string{sha}}))
		if has.Present[0] {
			t.Errorf("HasObjects(%s) = true", sha)
		}
		for name, err := range map[string]error{
			"ResolveRev": func() error { _, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: sha}); return err }(),
			"Log":        func() error { _, err := c.Log(ctx, &sgitv1.LogRequest{Project: "web", Rev: sha}); return err }(),
			"Diff":       func() error { _, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: sha, B: "main"}); return err }(),
			"Blame":      func() error { _, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Rev: sha, Path: "app/a.txt"}); return err }(),
			"MergeFile":  func() error { _, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: sha}); return err }(),
		} {
			if status.Code(err) != codes.NotFound {
				t.Errorf("%s(%s) = %v", name, sha[:8], err)
			}
		}
	}
}

func TestUnmountedOnlyCommitNotInLog(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	lg := testenv.Must(e.Project(bob).Log(ctx, &sgitv1.LogRequest{Project: "web"}))
	for _, l := range lg.Entries {
		if l.Subject == "rotate secret only" {
			t.Fatal("a commit touching only unmounted folders is in the project log")
		}
	}
}

func TestResponsesNeverLeakSources(t *testing.T) {
	ctx := context.Background()
	e, bob, src := isoEnv(t)
	c := e.Project(bob)
	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}))
	assertClean(t, "ResolveRev", rr, src)
	assertClean(t, "Log", testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web"})), src)
	assertClean(t, "Blame", testenv.Must(c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "app/a.txt"})), src)
	assertClean(t, "ListProjects", testenv.Must(c.ListProjects(ctx, &sgitv1.ListProjectsRequest{})), src)
	sha := e.Upload(bob, "web", []byte("new\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: rr.Sha, Message: "m",
		Changes: []*sgitv1.Change{testenv.Add("app/n.txt", sha)}}))
	assertClean(t, "CreateDraft", d, src)
	assertClean(t, "Diff", testenv.Must(c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: d.Base, B: d.Head})), src)
	resp := testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}))
	assertClean(t, "Land", resp, append(src, sourceSHAs(t, e)...))
	assertClean(t, "ListDrafts", testenv.Must(c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: "web"})), sourceSHAs(t, e))
	// A conflict detail carries project paths and the project head only.
	e.CommitDirect(hiddenRepo, map[string]string{"pub/a.txt": "upstream\n"}, "upstream")
	ours := e.Upload(bob, "web", []byte("mine\n"))[0]
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: resp.Commit, Message: "m",
		Changes: []*sgitv1.Change{testenv.Modify("app/a.txt", ours)}})
	assertClean(t, "conflict", status.Convert(err).Proto(), sourceSHAs(t, e))
}

func TestIsolationThroughWarmDaemon(t *testing.T) {
	e, bob, _ := isoEnv(t)
	e.CreateProject("sec")
	e.Mount("sec", "s", hiddenRepo, "secret", false)
	d := daemontest.Start(t, e.Addr)
	secret := objfmt.BlobSHA([]byte("rotated secret\n"))
	if _, err := fetch(t, d.Project(e.AdminToken), "sec", secret); err != nil {
		t.Fatal(err)
	}
	objs, err := fetch(t, d.Project(bob), "web", secret)
	if status.Code(err) != codes.NotFound || len(objs) != 0 {
		t.Fatalf("warm sgd served a foreign object: %v %v", objs, err)
	}
}
```

`internal/cli/isolation_test.go`:

```go
package cli_test

import (
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
	cmds := [][]string{
		{"log", "--project", "api"}, {"log", "--project", "api", "--json"},
		{"cat", "main:x", "--project", "api"}, {"drafts", "--project", "api", "--json"},
		{"checkout", "api"}, {"checkout", "api", "--json"},
	}
	before := map[string]res{}
	for _, c := range cmds {
		before[strings.Join(c, " ")] = run(c...)
	}
	h.env.CreateProject("api")
	h.env.Mount("api", "lib", "zz-src", "private", false)
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
```

- [ ] **Step 2: Run the isolation tests**

Run: `go test -race -count=1 ./internal/server/ ./internal/cli/ -run 'Isolation|Foreign|UnmountedOnly|NeverLeak|RootListsOnly|ByteIdentical|NeverNames'`
Expected: PASS. If one fails, fix the owning code (Task 7 for server leaks, Task 10 for CLI output, Task 11 for sgd) and re-run.

- [ ] **Step 3: Lint and commit**

```bash
gofmt -l . && go vet ./... && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "test: project isolation suite (foreign SHAs, logs, errors, responses, warm sgd)"
```

---

### Task 13: End to end: Docker smoke, monkey run, benchmarks

**Files:**
- Modify: `internal/smoke/docker_smoke_test.go`, `scripts/docker-smoke.sh`
- Create: `internal/cli/monkey_test.go` (build tag `monkey`), `internal/cli/bench_test.go`
- Modify: `README.md` (Performance section numbers)

**Interfaces:**
- Consumes: the whole stack; harness helpers; `daemontest.Start`.
- Produces: `SG_MONKEY_STEPS` (default 400) and `SG_MONKEY_SEED` env knobs for the monkey test; benchmarks `BenchmarkProjectCheckout`, `BenchmarkProjectLand`, `BenchmarkProjectLog`.

- [ ] **Step 1: Docker smoke over projects.** In `internal/smoke/docker_smoke_test.go`, after `CreateRepo(smoke)`:

```go
	testenv.Must(admin.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "smoke"}))
	testenv.Must(admin.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "smoke", Path: "app", Repo: "smoke", Folder: "app"}))
	pc := sgitv1.NewProjectClient(conn)
	head := testenv.Must(pc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "smoke"})).Sha
	shas := testenv.Must(rpc.UploadBlobs(ctx, pc, "smoke", [][]byte{[]byte("hello from docker\n")}))
	landed := testenv.Must(pc.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{
		Project: "smoke", Base: head, Changes: []*sgitv1.Change{testenv.Add("app/hello.txt", shas[0])}, Message: "smoke commit",
	}))
	lg := testenv.Must(pc.Log(ctx, &sgitv1.LogRequest{Project: "smoke", N: 5}))
	if len(lg.Entries) != 2 || lg.Entries[0].Sha != landed.Commit || lg.Entries[0].Subject != "smoke commit" {
		t.Fatalf("log = %+v", lg.Entries)
	}
```

(the second entry is the layout commit "Project layout: added `app/`"). In `scripts/docker-smoke.sh`, extend the in-container block:

```bash
LIST=$(docker exec "$NAME" sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)" >/dev/null \
  && sg repo create smoke-admin >/dev/null && sg project create smoke-admin >/dev/null \
  && sg project mount smoke-admin . smoke-admin:. >/dev/null && sg project list')
grep -q '^smoke-admin ' <<<"$LIST"
```

Run: `scripts/docker-smoke.sh`
Expected: `in-container sg and askpass OK` then `docker smoke OK`. (Needs Docker; if Docker is unavailable in the environment, say so in the task report instead of claiming success.)

- [ ] **Step 2: Monkey run** `internal/cli/monkey_test.go`:

```go
//go:build monkey

package cli_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestMonkeyProjects drives random sg calls over two projects that share a
// repo and one that spans two repos, then checks the invariants.
// Run: go test -tags monkey -run Monkey -count=1 ./internal/cli/ (SG_MONKEY_STEPS, SG_MONKEY_SEED).
func TestMonkeyProjects(t *testing.T) {
	steps, _ := strconv.Atoi(os.Getenv("SG_MONKEY_STEPS"))
	if steps == 0 {
		steps = 400
	}
	seed, _ := strconv.ParseUint(os.Getenv("SG_MONKEY_SEED"), 10, 64)
	if seed == 0 {
		seed = 1
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	h := newHarness(t)
	for _, r := range []string{"core", "docs"} {
		h.env.CreateRepo(r)
		h.env.CommitDirect(r, map[string]string{"src/seed.txt": "seed\n"}, "seed")
	}
	h.env.CreateProject("web")
	h.env.Mount("web", "lib", "core", "src", false)
	h.env.CreateProject("api")
	h.env.Mount("api", "code", "core", "src", false)
	h.env.Mount("api", "doc", "docs", "src", false)
	root := t.TempDir()
	type wcT struct{ dir, mount string }
	var wcs []wcT
	for i, p := range []struct{ project, mount string }{{"web", "lib"}, {"web", "lib"}, {"api", "code"}, {"api", "doc"}} {
		dir := filepath.Join(root, fmt.Sprintf("w%d", i))
		h.ok(root, "checkout", p.project, dir)
		wcs = append(wcs, wcT{dir, p.mount})
	}
	// step runs one sg call; any exit code outside the documented 0-4 is a crash.
	step := func(dir string, args ...string) int {
		code, out, errs := h.run(dir, args...)
		if code < 0 || code > 4 {
			t.Fatalf("sg %v in %s: exit %d\n%s%s", args, dir, code, out, errs)
		}
		return code
	}
	for i := 0; i < steps; i++ {
		w := wcs[rng.IntN(len(wcs))]
		switch rng.IntN(6) {
		case 0, 1:
			name := fmt.Sprintf("f%d.txt", rng.IntN(20))
			writeFile(t, filepath.Join(w.dir, w.mount, name), fmt.Sprintf("step %d\n", i))
		case 2:
			step(w.dir, "commit", "-m", fmt.Sprintf("step %d", i), "--land")
		case 3:
			step(w.dir, "sync")
		case 4:
			step(w.dir, "log", "-n", "5")
		case 5:
			step(w.dir, "status")
		}
	}
	// Settle: every working copy syncs and reports a status (clean, or with
	// recorded conflicts) without crashing.
	for _, w := range wcs {
		step(w.dir, "sync")
		step(w.dir, "status", "--json")
	}
	for _, r := range []string{"core", "docs"} {
		g := h.env.Git(r)
		if _, err := g.Log(t.Context(), "main", "", 10000); err != nil {
			t.Fatalf("%s log: %v", r, err)
		}
		g.Close()
	}
}
```

Also run `git --git-dir=<data>/repos/core.git fsck --strict` and `git --git-dir=<data>/projects/<id>.git fsck --strict` at the end via `exec.Command` and fail on a non-zero exit (add the `os/exec` import). Run:

Run: `go test -tags monkey -run Monkey -count=1 ./internal/cli/ && SG_MONKEY_STEPS=3000 SG_MONKEY_SEED=7 go test -tags monkey -run Monkey -count=1 ./internal/cli/`
Expected: PASS for both.

- [ ] **Step 3: Benchmarks** `internal/cli/bench_test.go` (no build tag; benchmarks only run with `-bench`):

```go
package cli_test

import (
	"fmt"
	"path/filepath"
	"testing"
)

// benchRepo builds the 20,000-file layout from the README (20 dirs × 1,000
// files of about 2.7 KB) as repo "big" and project "big" mounting d0..d19 at ".".
func benchRepo(b *testing.B) *harness {
	b.Helper()
	h := newHarnessTB(b)
	h.env.CreateRepo("big")
	files := map[string]string{}
	body := string(make([]byte, 2700))
	for d := 0; d < 20; d++ {
		for f := 0; f < 1000; f++ {
			files[fmt.Sprintf("d%d/f%04d.txt", d, f)] = fmt.Sprintf("%d/%d\n", d, f) + body
		}
	}
	h.env.CommitDirect("big", files, "seed")
	h.env.CreateProject("big")
	h.env.Mount("big", ".", "big", ".", false)
	return h
}

func BenchmarkProjectCheckout(b *testing.B) {
	h := benchRepo(b)
	for i := 0; b.Loop(); i++ {
		h.ok(b.TempDir(), "checkout", "big", filepath.Join(b.TempDir(), "w"))
	}
}

func BenchmarkProjectLand(b *testing.B) {
	h := benchRepo(b)
	root := b.TempDir()
	h.ok(root, "checkout", "big")
	w := filepath.Join(root, "big")
	for i := 0; b.Loop(); i++ {
		writeFile(b, filepath.Join(w, "d0", "f0000.txt"), fmt.Sprintf("edit %d\n", i))
		h.ok(w, "commit", "-m", fmt.Sprintf("edit %d", i), "--land")
	}
}

func BenchmarkProjectLog(b *testing.B) {
	h := benchRepo(b)
	for b.Loop() {
		h.ok(b.TempDir(), "log", "--project", "big", "-n", "100")
	}
}
```

This needs the harness to accept `testing.TB`: change `harness.t` to `testing.TB`, add `func newHarnessTB(tb testing.TB, opts ...func(*server.Config)) *harness` (the body of `newHarness`, which now calls it), and make `readFile`/`writeFile` take `testing.TB`.

Run: `go test -run '^$' -bench 'BenchmarkProject' -benchtime 5x ./internal/cli/`
Expected: three results. Record them, together with the v0.2.0 rows they correspond to (`sg checkout` full 3.7 s, `sg commit --land` 1 file 0.48 s, `sg log -n 100` 50–70 ms), in the README Performance section as a new sub-table "Projects (in-process, same 20,000-file layout)". Do not edit the v0.2.0 rows.

- [ ] **Step 4: Lint and commit**

```bash
gofmt -l . && go vet ./... && go vet -tags monkey ./internal/cli/ && staticcheck ./... && go test -race -count=1 ./...
git add -A && git commit -m "test: projects in the docker smoke test, a monkey run and benchmarks"
```

---

### Task 14: Docs and skills

**Files:**
- Modify: `README.md`, `docs/server.md`, `docs/client.md`, `docs/sgd.md`, `skills/sgit-admin/SKILL.md`, `skills/sgit-client/SKILL.md`, `docs/superpowers/specs/2026-10-15-sgit-projects-design.md` (implementation notes section only)

**Interfaces:**
- Consumes: final command names and outputs from Tasks 9 and 10.
- Produces: documentation only.

- [ ] **Step 1: README.md.**
  - Features: replace "per-repo `read`/`write` access" with "projects: per-folder views of one or more repos, with per-project `read`/`write` access, aliases and read-only mounts; clients never see repos".
  - Architecture diagram: `sg-server` gains `└─ <data-dir>/projects/<id>.git (derived per-project repos)`; auth line becomes `tokens → user → per-project read/write, admin role`.
  - Server setup walk-through: after `sg repo create web`, add `sg project create web`, `sg project mount web . web:.`, `sg acl grant bob web write`.
  - Client setup: `sg projects`, `sg checkout <project>`.
  - Skills example prompts: "Create project site from web:public, give bob write access".
- [ ] **Step 2: docs/server.md** "Administration": a new "Projects" subsection with every `sg project …` command and its output, the mount rules (clean path or `.`; no nesting; `.` only alone; a missing folder shows up empty), forward-only history (layout commits), read-only mounts, aliases and rename, ACLs per project, `repo delete` refused while mounted, the `<data>/projects/<id>.git` cache (safe to delete; rebuilt with identical SHAs; open drafts of that project are abandoned), and the accepted limits from spec §1 (copied commit messages, non-atomic cross-repo lands, mirrors publish whole repos).
- [ ] **Step 3: docs/client.md**: commands use `--project`; `sg projects`; `.sg/config` holds the project id; exit code 2 also covers `project "<name>" not found or no access`; exit 3 covers partial cross-repo lands (`--json`: `"partial": true, "landed_mounts": [...]`) and how a retry lands only the rest.
- [ ] **Step 4: docs/sgd.md** "Security": the access memo is per (token, project), and every cache hit is confirmed with one batched `HasObjects` before it is served, so the shared cache never crosses project boundaries.
- [ ] **Step 5: skills/sgit-admin/SKILL.md**: Model section (repos are admin-only; users get projects); onboarding becomes `token create` → `project create` → `project mount` → `acl grant <user> <project>`; command reference for `sg project …` and `sg acl … <project>`; Safety: `project delete --yes`, `repo delete` refused while mounted.
- [ ] **Step 6: skills/sgit-client/SKILL.md**: rewrite every mention of a repo as a project (`sg projects`, `sg checkout <project>`, `--project`); the word "repo" must not appear: `grep -ci repo skills/sgit-client/SKILL.md` prints `0`. Add the partial-land exit-3 case to "Exit codes - handle them".
- [ ] **Step 7: Spec notes.** Append a "## 12. Implementation notes" section to the projects spec listing the "Decisions beyond the spec" from this plan.
- [ ] **Step 8: Check and commit**

Run: `grep -rn -- '--repo' README.md docs/client.md skills/sgit-client/SKILL.md` prints nothing; `go test -race -count=1 ./...` passes.

```bash
git add -A && git commit -m "docs: projects in README, server/client/sgd guides and both skills"
```

---

## Self-review

- **Spec coverage.** §2 concepts and mount rules: Tasks 4, 6. §3 data model: Tasks 2, 7 (plus the recorded derivation columns, see decisions). §4 admin commands: Task 9; client commands: Task 10. §5.1 storage and rebuild: Task 4. §5.2 translation: Task 4. §5.3 land sequence and ordering: Tasks 3, 4. §5.4 initial build: Task 4. §5.5 forward-only mounts: Task 4. §5.6 catch-up: Tasks 4, 7. §6.1 path mapping: Task 7. §6.2 drafts: Task 7. §6.3 single-repo land with access re-check: Task 7. §6.4 cross-repo best effort: Task 8. §6.5 CommitAndLand: Tasks 7, 8. §7 reads, `ResolveRev`, the `Project` service and admin RPCs: Tasks 5, 6, 7. §8 errors and existence hiding: Tasks 6, 7, 10, 12. §9 sgd: Task 11. §10 tests: isolation (Task 12), translation (Task 4, Task 7 catch-up through another project), writes (Tasks 7, 8), end to end (Task 13). §11 docs: Task 14.
- **Type consistency.** `store.Mount`, `store.LayoutMount`, `store.ProjectCommit`, `store.MountChange` are defined in Task 2 and used unchanged by Tasks 4, 6, 7. `project.Handle.LandMu` (Task 4) is locked by `DeleteProject` (Task 6) and `landDraft` (Task 7). `auth.ProjectNotFound` (Task 6) is used by Tasks 7 and 10's exit-code mapping (message suffix ` not found or no access`). `Store.DeleteProjectRows` changes from `error` (Task 2) to `(drafts, acls int, err error)` (Task 7); Task 6's `DeleteProject` is updated in Task 7 Step 10. `conflictStatus` gains `partial`/`landed` parameters in Task 7 and is used that way in Task 8. `testenv.NewUser(name, project, level)` keeps its Task-5 signature (the second argument changes meaning in Task 7). `rpc.UploadBlobs` and `fetch.*` take `sgitv1.ProjectClient` from Task 5 on.
