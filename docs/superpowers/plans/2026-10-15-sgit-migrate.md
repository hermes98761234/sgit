# sgit migrate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `sg migrate in` (git repo → sgit repo or monorepo folder, plus a project) and `sg migrate out` (project → standalone git repo).

**Architecture:** Two new Admin RPCs, `MigrateIn` and `MigrateOut`, in a new `internal/server/migrate.go`. They compose the existing import, project-create and mount paths, plus new gitx helpers: `ReplayUnder` (rewrites a first-parent history under a folder of an existing repo), `FetchMain`, `DeleteRef`, `RemoteBranches` and `PushFF`. A new CLI group `sg migrate` in `internal/cli/migrate.go` calls them. All git work shells out to `git`, as the rest of gitx does.

**Tech Stack:** Go, gRPC/protobuf (plain `protoc` via `make gen`), git CLI, SQLite store.

**Spec:** `docs/superpowers/specs/2026-10-15-sgit-migrate-design.md`

## Global Constraints

- Work directly on `main` in `~/Projects/new/sgit`. Commit identity is the repo-local `No name <hermes98761234@tutamail.com>`; never change git config.
- Every commit message ends with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G
  ```
- Both RPCs are admin only (`a.requireAdmin`). Nothing client-facing (Project service, client docs, sgit-client skill) changes or mentions repos.
- Names follow `^[a-z0-9][a-z0-9._-]{0,63}$` (`ValidRepoName`).
- `--into` migrates `main` only; `release/*` branches are reported as skipped. `migrate out` pushes `main` only.
- Remote URLs go through `gitx.ValidateRemote` (https/ssh only, no credentials in the URL, `file://`/absolute paths only when `AllowLocalRemotes`).
- Client error messages carry no `gitx:`/`store:` prefixes (use `status.Errorf` or `a.s.toStatus`).
- Tests: `go test -race -count=1 ./...`, `go vet ./...`, `staticcheck ./...` all clean. `PATH` needs `$HOME/.local/go/bin:$HOME/.local/protoc/bin:$HOME/go/bin` (the Makefile exports it; `make test` works).
- Publishing (last task only): `gh auth setup-git` then `git push origin main`.

## Review Focus

1. **Lock leak on a failed `--into`.** A refused or failed replay must release the target repo's land lock; the next migrate or land into that repo must not hang. Test: Task 4, `TestMigrateIntoLockReleased`.
2. **Project name already taken.** `migrate in` without `--project` must fail with AlreadyExists *before* importing anything, so no orphan repo is left. Test: Task 3, `TestMigrateInNameTaken`.
3. **Source without `main`.** A repo that only has `master` must fail with InvalidArgument and leave nothing behind (no repo, no project, no temp ref). Tests: Task 3 (plain) and Task 4 (`--into`).
4. **URL shapes for the default name.** Trailing `/`, `.git` suffix and scp-style `git@host:org/repo` must all give the repo name; a URL with no path segment must ask for `<name>`. Test: Task 6, `TestNameFromURL`.
5. **Bad target URL for `migrate out`.** Credentials in the URL or a non-https/ssh scheme must be refused with InvalidArgument without pushing. Test: Task 5, in `TestMigrateOut`.

---

### Task 1: gitx `ReplayUnder`

**Files:**
- Modify: `internal/gitx/commit.go` (add `Encoding` to `Commit`, parse it in `parseCommit`)
- Modify: `internal/gitx/project.go:356-379` (`WriteCommit` delegates to a new `writeCommit` that can write an `encoding` header)
- Create: `internal/gitx/migrate.go`
- Test: `internal/gitx/migrate_test.go`

**Interfaces:**
- Consumes: `(*Repo).ReadCommit`, `FirstParents`, `SpliceTree`, `ReadObject`, `run` (all existing).
- Produces:
  - `var ErrFolderNotEmpty = errors.New("gitx: folder not empty")`
  - `func (r *Repo) ReplayUnder(ctx context.Context, base, tip, folder string) (newTip string, n int, err error)`
  - `Commit.Encoding string`

- [ ] **Step 1: Write the failing tests**

Create `internal/gitx/migrate_test.go`:

```go
package gitx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// sideCommit writes a commit on top of parent without moving main.
func sideCommit(t *testing.T, r *Repo, parent string, changes []Change, msg string, sig Signature) string {
	t.Helper()
	ctx := context.Background()
	tree, err := r.BuildTree(ctx, parent, changes)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	c, err := r.CommitTree(ctx, tree, []string{parent}, msg, sig, sig)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReplayUnder(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	root, _ := r.ReadRef(ctx, MainRef)
	base := commitChanges(t, r, []Change{{Path: "keep.txt", Op: OpAdd, BlobSHA: blob(t, r, "k")}})

	ann := Signature{Name: "Ann", Email: "ann@example.com", When: time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3*3600))}
	s1 := sideCommit(t, r, root, []Change{{Path: "a.go", Op: OpAdd, BlobSHA: blob(t, r, "a1")}}, "one\n", ann)
	s2 := sideCommit(t, r, s1, []Change{{Path: "b/c.go", Op: OpAdd, BlobSHA: blob(t, r, "c1")}}, "two\n", ann)
	x := sideCommit(t, r, s1, []Change{{Path: "side.txt", Op: OpAdd, BlobSHA: blob(t, r, "s")}}, "side\n", ann)
	mtree, err := r.BuildTree(ctx, s2, []Change{{Path: "side.txt", Op: OpAdd, BlobSHA: blob(t, r, "s")}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.CommitTree(ctx, mtree, []string{s2, x}, "merge\n", ann, ann)
	if err != nil {
		t.Fatal(err)
	}

	tip, n, err := r.ReplayUnder(ctx, base, m, "svc/api")
	if err != nil {
		t.Fatalf("ReplayUnder: %v", err)
	}
	orig, _ := r.FirstParents(ctx, m, "")
	if n != len(orig) || n != 4 { // root, s1, s2, m; x is off the first-parent line
		t.Fatalf("n = %d, first parents = %d", n, len(orig))
	}
	copies, err := r.FirstParents(ctx, tip, base)
	if err != nil || len(copies) != n {
		t.Fatalf("copies = %v, %v", copies, err)
	}
	bc, _ := r.ReadCommit(ctx, base)
	prev := base
	for i := range copies {
		o, _ := r.ReadCommit(ctx, orig[i].SHA)
		c, _ := r.ReadCommit(ctx, copies[i].SHA)
		if c.AuthorRaw != o.AuthorRaw || c.CommitterRaw != o.CommitterRaw || c.Message != o.Message {
			t.Fatalf("copy %d header/message differ: %+v vs %+v", i, c, o)
		}
		if len(c.Parents) != 1 || c.Parents[0] != prev {
			t.Fatalf("copy %d parents = %v, want [%s]", i, c.Parents, prev)
		}
		want, _ := r.SpliceTree(ctx, bc.Tree, map[string]string{"svc/api": o.Tree})
		if c.Tree != want {
			t.Fatalf("copy %d tree = %s, want %s", i, c.Tree, want)
		}
		prev = c.SHA
	}
	files, err := r.ListFiles(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"keep.txt", "svc/api/a.go", "svc/api/b/c.go", "svc/api/side.txt"} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing %s in %v", p, files)
		}
	}
	if len(files) != 4 {
		t.Errorf("files = %v", files)
	}
	if got, _ := r.ReadRef(ctx, MainRef); got != base {
		t.Fatalf("ReplayUnder moved main to %s", got)
	}
}

func TestReplayUnderKeepsEncodingDropsSignature(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, _ := r.ReadRef(ctx, MainRef)
	bc, _ := r.ReadCommit(ctx, base)
	raw := "tree " + bc.Tree + "\n" +
		"author A U <a@x.org> 1700000000 +0300\n" +
		"committer C <c@x.org> 1700000001 -0500\n" +
		"encoding ISO-8859-1\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n" +
		"\nsigned msg\n"
	out, err := r.run(ctx, strings.NewReader(raw), nil, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	signed := strings.TrimSpace(string(out))
	tip, n, err := r.ReplayUnder(ctx, base, signed, "x")
	if err != nil || n != 1 {
		t.Fatalf("ReplayUnder = %d, %v", n, err)
	}
	obj, err := r.ReadObject(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	got := string(obj.Data)
	for _, s := range []string{
		"\nauthor A U <a@x.org> 1700000000 +0300\ncommitter C <c@x.org> 1700000001 -0500\nencoding ISO-8859-1\n\nsigned msg\n",
	} {
		if !strings.Contains(got, s) {
			t.Fatalf("copy %q lacks %q", got, s)
		}
	}
	if strings.Contains(got, "gpgsig") {
		t.Fatalf("copy kept the signature: %q", got)
	}
}

func TestReplayUnderRefusesNonEmptyFolder(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	root, _ := r.ReadRef(ctx, MainRef)
	base := commitChanges(t, r, []Change{
		{Path: "svc/x.txt", Op: OpAdd, BlobSHA: blob(t, r, "x")},
		{Path: "file", Op: OpAdd, BlobSHA: blob(t, r, "f")},
	})
	tip := sideCommit(t, r, root, []Change{{Path: "a", Op: OpAdd, BlobSHA: blob(t, r, "a")}}, "a\n", testSig)
	for _, folder := range []string{"svc", "file"} {
		if _, _, err := r.ReplayUnder(ctx, base, tip, folder); !errors.Is(err, ErrFolderNotEmpty) {
			t.Errorf("folder %q: err = %v, want ErrFolderNotEmpty", folder, err)
		}
	}
	if _, n, err := r.ReplayUnder(ctx, base, tip, "svc/new"); err != nil || n != 2 {
		t.Errorf("nested new folder: n=%d err=%v", n, err)
	}
	for _, folder := range []string{"", ".", "/abs", "a/"} {
		if _, _, err := r.ReplayUnder(ctx, base, tip, folder); !errors.Is(err, ErrInvalid) {
			t.Errorf("folder %q: err = %v, want ErrInvalid", folder, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gitx -run ReplayUnder -count=1`
Expected: FAIL to compile with `r.ReplayUnder undefined` and `undefined: ErrFolderNotEmpty`.

- [ ] **Step 3: Parse the encoding header**

In `internal/gitx/commit.go`, add a field to `Commit` after `CommitterRaw`:

```go
	Encoding     string // value of the "encoding" header, "" when absent
```

and a case in `parseCommit`'s switch:

```go
		case "encoding":
			c.Encoding = v
```

- [ ] **Step 4: Let commits be written with an encoding header**

In `internal/gitx/project.go`, replace `WriteCommit` with:

```go
// WriteCommit writes a commit object from raw parts (ident lines as stored
// after "author "/"committer "), so a copy of a commit is byte-identical.
func (r *Repo) WriteCommit(ctx context.Context, tree string, parents []string, author, committer, message string) (string, error) {
	return r.writeCommit(ctx, tree, parents, author, committer, "", message)
}

// writeCommit is WriteCommit with an optional encoding header ("" omits it).
func (r *Repo) writeCommit(ctx context.Context, tree string, parents []string, author, committer, encoding, message string) (string, error) {
	if !IsSHA(tree) || strings.ContainsAny(author+committer+encoding, "\n\x00") || author == "" || committer == "" {
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
	fmt.Fprintf(&b, "author %s\ncommitter %s\n", author, committer)
	if encoding != "" {
		fmt.Fprintf(&b, "encoding %s\n", encoding)
	}
	b.WriteString("\n")
	b.WriteString(message)
	if err := r.ensureEmptyTree(ctx); err != nil {
		return "", err
	}
	out, err := r.run(ctx, strings.NewReader(b.String()), nil, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
```

- [ ] **Step 5: Implement `ReplayUnder`**

Create `internal/gitx/migrate.go`:

```go
package gitx

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrFolderNotEmpty means a replay target folder already holds files.
var ErrFolderNotEmpty = errors.New("gitx: folder not empty")

// ReplayUnder copies the first-parent chain of tip, oldest first, on top of
// base. Each copy's tree is base's tree with folder replaced by the original
// commit's root tree. Author, committer, encoding and message are kept byte
// for byte; signatures (gpgsig, mergetag) are dropped because they no longer
// match. folder must be absent from base, or an empty tree there. tip's
// objects must already be in r. Refs are not touched.
func (r *Repo) ReplayUnder(ctx context.Context, base, tip, folder string) (string, int, error) {
	if !IsSHA(base) || !IsSHA(tip) || folder == "" || folder == "." ||
		strings.HasPrefix(folder, "/") || strings.HasSuffix(folder, "/") || strings.ContainsAny(folder, "\x00\n") {
		return "", 0, fmt.Errorf("%w: replay %q onto %q under %q", ErrInvalid, tip, base, folder)
	}
	bc, err := r.ReadCommit(ctx, base)
	if err != nil {
		return "", 0, err
	}
	if err := r.folderFree(ctx, bc.Tree, folder); err != nil {
		return "", 0, err
	}
	list, err := r.FirstParents(ctx, tip, "")
	if err != nil {
		return "", 0, err
	}
	parent := base
	for _, fp := range list {
		c, err := r.ReadCommit(ctx, fp.SHA)
		if err != nil {
			return "", 0, err
		}
		tree, err := r.SpliceTree(ctx, bc.Tree, map[string]string{folder: c.Tree})
		if err != nil {
			return "", 0, err
		}
		parent, err = r.writeCommit(ctx, tree, []string{parent}, c.AuthorRaw, c.CommitterRaw, c.Encoding, c.Message)
		if err != nil {
			return "", 0, err
		}
	}
	return parent, len(list), nil
}

// folderFree returns ErrFolderNotEmpty unless folder is absent from tree or
// is an empty tree there.
func (r *Repo) folderFree(ctx context.Context, tree, folder string) error {
	out, err := r.run(ctx, nil, nil, "ls-tree", "-z", tree, "--", folder)
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok || name != folder {
			continue
		}
		if f := strings.Fields(meta); len(f) == 3 && f[1] == "tree" && f[2] == EmptyTree {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrFolderNotEmpty, folder)
	}
	return nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/gitx -count=1 -race`
Expected: PASS (all gitx tests, including the existing `WriteCommit` users).

- [ ] **Step 7: Commit**

```bash
git add internal/gitx/commit.go internal/gitx/project.go internal/gitx/migrate.go internal/gitx/migrate_test.go
git commit -m "gitx: ReplayUnder rewrites a history under a folder of another repo" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

---

### Task 2: gitx remote helpers (`FetchMain`, `DeleteRef`, `RemoteBranches`, `PushFF`)

**Files:**
- Modify: `internal/gitx/remote.go` (add the four functions; factor the push-rejection mapping into `pushErr`)
- Test: `internal/gitx/remote_test.go` (append)

**Interfaces:**
- Consumes: `ValidateRemote`, `remoteEnv`, `runGit`, `(*Repo).run`, `ReadRef`, `ErrRejected` (existing).
- Produces:
  - `func (r *Repo) FetchMain(ctx context.Context, remote, ref string, allowLocal bool) (string, error)` — fetches remote's `main` into `ref` (full ref name, overwritten), returns its SHA; no `main` → error wrapping `ErrInvalid` ("… has no main branch").
  - `func (r *Repo) DeleteRef(ctx context.Context, ref string) error` — missing ref is not an error.
  - `func RemoteBranches(ctx context.Context, remote, prefix string, allowLocal bool) ([]string, error)` — sorted branch names (without `refs/heads/`) starting with prefix.
  - `func (r *Repo) PushFF(ctx context.Context, remote, sha string, allowLocal bool) error` — non-forced push of sha to remote `main`; non-fast-forward → error wrapping `ErrRejected`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/gitx/remote_test.go` (add `"reflect"` to its imports if missing):

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gitx -run 'FetchMain|RemoteBranches|PushFF' -count=1`
Expected: FAIL to compile (`dst.FetchMain undefined`, `undefined: RemoteBranches`, `src.PushFF undefined`).

- [ ] **Step 3: Implement the helpers**

In `internal/gitx/remote.go` (add `"sort"` to imports), replace the tail of `Push` with a shared helper and add the new functions:

```go
func (r *Repo) Push(ctx context.Context, remote, sha, expect string, allowLocal bool) error {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return err
	}
	if !IsSHA(sha) || (expect != "" && !IsSHA(expect)) {
		return fmt.Errorf("%w: push %q over %q", ErrInvalid, sha, expect)
	}
	out, err := r.run(ctx, nil, remoteEnv(allowLocal), "push", "--porcelain", "--no-verify",
		"--force-with-lease="+MainRef+":"+expect, remote, sha+":"+MainRef)
	return pushErr(out, err)
}

// PushFF sets remote's main to sha without forcing: it creates main or
// fast-forwards it. A remote main that is not an ancestor of sha returns an
// error wrapping ErrRejected and leaves the remote untouched.
func (r *Repo) PushFF(ctx context.Context, remote, sha string, allowLocal bool) error {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return err
	}
	if !IsSHA(sha) {
		return fmt.Errorf("%w: push %q", ErrInvalid, sha)
	}
	out, err := r.run(ctx, nil, remoteEnv(allowLocal), "push", "--porcelain", "--no-verify", remote, sha+":"+MainRef)
	return pushErr(out, err)
}

// pushErr turns a "[rejected]" push into ErrRejected.
func pushErr(out []byte, err error) error {
	var ge *GitError
	if errors.As(err, &ge) && (strings.Contains(string(out), "[rejected]") || strings.Contains(ge.Stderr, "[rejected]")) {
		return fmt.Errorf("%w: %s", ErrRejected, strings.Join(strings.Fields(string(out)), " "))
	}
	return err
}

// FetchMain fetches remote's main (no tags) into ref, a full ref name that
// is overwritten, and returns the fetched SHA.
func (r *Repo) FetchMain(ctx context.Context, remote, ref string, allowLocal bool) (string, error) {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return "", err
	}
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, " :\x00\n") {
		return "", fmt.Errorf("%w: fetch into %q", ErrInvalid, ref)
	}
	_, err := r.run(ctx, nil, remoteEnv(allowLocal), "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote,
		"+"+MainRef+":"+ref)
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref "+MainRef) {
			return "", fmt.Errorf("%w: %s has no main branch", ErrInvalid, remote)
		}
		return "", err
	}
	return r.ReadRef(ctx, ref)
}

// DeleteRef removes ref. A missing ref is not an error.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("%w: delete %q", ErrInvalid, ref)
	}
	_, err := r.run(ctx, nil, nil, "update-ref", "-d", ref)
	return err
}

// RemoteBranches lists remote's branches whose name (without refs/heads/)
// starts with prefix, sorted.
func RemoteBranches(ctx context.Context, remote, prefix string, allowLocal bool) ([]string, error) {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return nil, err
	}
	out, err := runGit(ctx, nil, append(remoteEnv(allowLocal), "GIT_DIR="+os.DevNull), "ls-remote", "--refs", remote)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		name, isHead := strings.CutPrefix(ref, "refs/heads/")
		if ok && isHead && strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
```

If `git update-ref -d` on a missing ref turns out to fail with your git version, make `DeleteRef` return nil when `ReadRef` then reports `ErrNotFound`; the test pins the behaviour either way.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gitx -count=1 -race`
Expected: PASS, including the existing `TestPushLease`.

- [ ] **Step 5: Commit**

```bash
git add internal/gitx/remote.go internal/gitx/remote_test.go
git commit -m "gitx: FetchMain, DeleteRef, RemoteBranches and a fast-forward-only push" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

---

### Task 3: Proto + `MigrateIn` plain form with the project step

**Files:**
- Modify: `proto/sgit/v1/sgit.proto` (Admin service + 4 messages)
- Regenerate: `gen/sgit/v1/sgit.pb.go`, `gen/sgit/v1/sgit_grpc.pb.go` (`make gen`)
- Create: `internal/server/migrate.go`
- Test: `internal/server/migrate_test.go`

**Interfaces:**
- Consumes: `a.requireAdmin`, `a.resolveProject`, `a.CreateProject`, `a.MountProject`, `a.DeleteProject`, `a.s.repos.importRepo`, `a.s.repos.rollbackNew`, `a.s.repos.path`, `a.s.store.ResolveProject`, `validProjectName`, `validMountPath`, `validatePath`, `gitx.Open`, `(*gitx.Repo).FirstParents`.
- Produces (used by Tasks 4–6):
  - Proto: `MigrateInRequest{from_url, name, into_repo, into_folder, project, path, mirror_url}`, `MigrateInResponse{repo, folder, project, path, main, commits int32, skipped_branches []string, project_head}`, `MigrateOutRequest{project, to_url, force bool}`, `MigrateOutResponse{project, pushed, up_to_date bool}`.
  - `func (a *adminService) MigrateIn(ctx, *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error)`
  - `func (a *adminService) migrateMount(ctx context.Context, req *sgitv1.MigrateInRequest, resp *sgitv1.MigrateInResponse, repo, folder string) error`
  - Test helpers in `migrate_test.go`: `allowLocal(*server.Config)`, `srcRepo(t, commits ...map[string]string) (dir, main string)`.

- [ ] **Step 1: Add the proto definitions**

In `proto/sgit/v1/sgit.proto`, add to `service Admin` after `DeleteProject`:

```proto
  rpc MigrateIn(MigrateInRequest) returns (MigrateInResponse);
  rpc MigrateOut(MigrateOutRequest) returns (MigrateOutResponse);
```

and after `DeleteProjectResponse`:

```proto
// MigrateIn imports from_url as repo `name`, or with into_repo replays its
// main under into_folder of that repo; then mounts it in a new project
// `name`, or in `project` at `path`.
message MigrateInRequest {
  string from_url = 1;
  string name = 2;
  string into_repo = 3;
  string into_folder = 4;
  string project = 5;
  string path = 6;
  string mirror_url = 7;
}
message MigrateInResponse {
  string repo = 1;
  string folder = 2;        // "." for a whole-repo import
  string project = 3;       // canonical project name
  string path = 4;          // mount path in the project
  string main = 5;          // repo main after the migration
  int32 commits = 6;        // commits imported or replayed
  repeated string skipped_branches = 7;
  string project_head = 8;
}
// MigrateOut pushes a project's main to to_url (fast-forward unless force).
message MigrateOutRequest { string project = 1; string to_url = 2; bool force = 3; }
message MigrateOutResponse { string project = 1; string pushed = 2; bool up_to_date = 3; }
```

Run: `make gen`
Expected: `gen/sgit/v1/*.pb.go` regenerated. Then run `go build ./...` — Expected: success (`adminService` embeds `UnimplementedAdminServer`, so the new methods default to Unimplemented).

- [ ] **Step 2: Write the failing tests**

Create `internal/server/migrate_test.go`:

```go
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
```

(`DeleteRef` is from Task 2.)

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/server -run MigrateIn -count=1`
Expected: FAIL with `code = Unimplemented desc = method MigrateIn not implemented`.

- [ ] **Step 4: Implement `MigrateIn` (plain form) and the project step**

Create `internal/server/migrate.go`:

```go
package server

import (
	"context"
	"strconv"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MigrateIn brings a git repo into sgit and mounts it in a project
// (docs/superpowers/specs/2026-10-15-sgit-migrate-design.md §3).
func (a *adminService) MigrateIn(ctx context.Context, req *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := checkMigrateIn(req); err != nil {
		return nil, err
	}
	if err := a.checkMigrateProject(ctx, req); err != nil {
		return nil, err
	}
	if req.IntoRepo != "" {
		return a.migrateInto(ctx, req)
	}
	main, err := a.s.repos.importRepo(ctx, req.Name, req.FromUrl, req.MirrorUrl)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	dir := a.s.repos.path(req.Name)
	g := gitx.Open(dir)
	fps, err := g.FirstParents(ctx, main, "")
	g.Close()
	if err != nil {
		a.s.repos.rollbackNew(req.Name, dir)
		return nil, a.s.toStatus(ctx, err)
	}
	resp := &sgitv1.MigrateInResponse{Repo: req.Name, Folder: ".", Main: main, Commits: int32(len(fps))}
	if err := a.migrateMount(ctx, req, resp, req.Name, "."); err != nil {
		a.s.repos.rollbackNew(req.Name, dir)
		return nil, err
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return resp, nil
}

// checkMigrateIn validates the request shape before anything is fetched.
func checkMigrateIn(req *sgitv1.MigrateInRequest) error {
	if req.FromUrl == "" {
		return status.Error(codes.InvalidArgument, "migrate in needs a source URL")
	}
	if err := validProjectName("name", req.Name); err != nil {
		return err
	}
	if (req.Project == "") != (req.Path == "") {
		return status.Error(codes.InvalidArgument, "--project and --path go together")
	}
	if req.Path != "" {
		if err := validMountPath(req.Path); err != nil {
			return err
		}
	}
	if req.IntoRepo == "" {
		if req.IntoFolder != "" {
			return status.Error(codes.InvalidArgument, "a folder needs --into <repo>:<folder>")
		}
		return nil
	}
	if req.MirrorUrl != "" {
		return status.Error(codes.InvalidArgument, "--mirror cannot be used with --into; set the mirror on the target repo")
	}
	if req.IntoFolder == "" || req.IntoFolder == "." {
		return status.Error(codes.InvalidArgument, "--into needs a folder: <repo>:<folder>")
	}
	return validatePath(req.IntoFolder)
}

// checkMigrateProject fails early, before any import, when the project step
// cannot succeed: --project must exist, and the default project name must be free.
func (a *adminService) checkMigrateProject(ctx context.Context, req *sgitv1.MigrateInRequest) error {
	if req.Project != "" {
		_, err := a.resolveProject(ctx, req.Project)
		return err
	}
	_, err := a.s.store.ResolveProject(ctx, req.Name)
	switch {
	case err == nil:
		return status.Errorf(codes.AlreadyExists, "project %q exists; pass another name or use --project", req.Name)
	case errors.Is(err, store.ErrNotFound):
		return nil
	default:
		return a.s.toStatus(ctx, err)
	}
}

// migrateMount mounts repo:folder in a new project req.Name at ".", or in
// req.Project at req.Path, and fills the project fields of resp. A project
// it created is deleted again when the mount fails.
func (a *adminService) migrateMount(ctx context.Context, req *sgitv1.MigrateInRequest, resp *sgitv1.MigrateInResponse, repo, folder string) error {
	project, path, created := req.Project, req.Path, false
	if project == "" {
		info, err := a.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: req.Name})
		if err != nil {
			return err
		}
		project, path, created = "#"+strconv.FormatInt(info.Id, 10), ".", true
	}
	info, err := a.MountProject(ctx, &sgitv1.MountProjectRequest{Project: project, Path: path, Repo: repo, Folder: folder})
	if err != nil {
		if created {
			if _, derr := a.DeleteProject(context.WithoutCancel(ctx), &sgitv1.DeleteProjectRequest{Project: project}); derr != nil {
				a.s.log.ErrorContext(ctx, "migrate: removing the new project failed", "project", project, "err", derr)
			}
		}
		return err
	}
	resp.Project, resp.Path, resp.ProjectHead = info.Name, path, info.Head
	return nil
}
```

Add `"errors"` to the imports. Add a temporary stub so the package builds until Task 4:

```go
func (a *adminService) migrateInto(ctx context.Context, req *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error) {
	return nil, status.Error(codes.Unimplemented, "--into is not implemented yet")
}
```

The `mirror with into`, `into root` and `folder without repo` cases fail in `checkMigrateIn`, before the stub is reached.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/server -run MigrateIn -count=1 -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add proto gen internal/server/migrate.go internal/server/migrate_test.go
git commit -m "server: MigrateIn imports a git repo and mounts it in a project" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

---

### Task 4: `MigrateIn --into <repo>:<folder>`

**Files:**
- Modify: `internal/server/migrate.go` (replace the `migrateInto` stub; add `replayInto`, `migrateRef`, `remoteErr`)
- Test: `internal/server/migrate_test.go` (append)

**Interfaces:**
- Consumes: Task 1 `ReplayUnder`, `ErrFolderNotEmpty`; Task 2 `FetchMain`, `DeleteRef`, `RemoteBranches`; Task 3 `migrateMount`, `srcRepo`, `allowLocal`; existing `a.s.repos.get`, `repoHandle.landMu`, `store.RecordLand`, `UpdateRef`, `mirror.Notify`.
- Produces: `func (a *adminService) migrateInto(ctx, req) (*sgitv1.MigrateInResponse, error)`; `func (s *Server) remoteErr(ctx context.Context, what, url string, err error) error` (also used by Task 5).

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/migrate_test.go` (add `"os/exec"`, `"slices"`, `"strings"` to imports):

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server -run 'MigrateInInto|MigrateInto' -count=1`
Expected: FAIL with `--into is not implemented yet`.

- [ ] **Step 3: Implement `--into`**

In `internal/server/migrate.go`, replace the `migrateInto` stub with the code below and add imports `"crypto/rand"`, `"encoding/hex"`, `"slices"`.

```go
// migrateInto replays from_url's main under into_folder of into_repo
// (spec §3.2), then mounts that folder.
func (a *adminService) migrateInto(ctx context.Context, req *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error) {
	h, err := a.s.repos.get(ctx, req.IntoRepo)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	allow := a.s.repos.allowLocal
	if err := gitx.ValidateRemote(req.FromUrl, allow); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	skipped, err := gitx.RemoteBranches(ctx, req.FromUrl, "release/", allow)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "import from", req.FromUrl, err)
	}
	ref, err := migrateRef()
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	defer func() {
		if err := h.git.DeleteRef(context.WithoutCancel(ctx), ref); err != nil {
			a.s.log.ErrorContext(ctx, "migrate: removing temporary ref failed", "repo", h.name, "ref", ref, "err", err)
		}
	}()
	tip, err := h.git.FetchMain(ctx, req.FromUrl, ref, allow)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "import from", req.FromUrl, err)
	}
	main, n, err := a.replayInto(ctx, h, tip, req.IntoFolder)
	if err != nil {
		return nil, err
	}
	resp := &sgitv1.MigrateInResponse{Repo: h.name, Folder: req.IntoFolder, Main: main, Commits: int32(n), SkippedBranches: skipped}
	if err := a.migrateMount(ctx, req, resp, h.name, req.IntoFolder); err != nil {
		st := status.Convert(err)
		return nil, status.Errorf(st.Code(), "%s (the code landed at %s:%s; mount it with sg project mount)", st.Message(), h.name, req.IntoFolder)
	}
	return resp, nil
}

// replayInto rewrites tip's history under folder on top of the repo's main
// and moves main, holding the repo's land lock throughout.
func (a *adminService) replayInto(ctx context.Context, h *repoHandle, tip, folder string) (string, int, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()
	base, err := h.git.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		return "", 0, a.s.toStatus(ctx, err)
	}
	main, n, err := h.git.ReplayUnder(ctx, base, tip, folder)
	if errors.Is(err, gitx.ErrFolderNotEmpty) {
		return "", 0, status.Errorf(codes.FailedPrecondition, "folder %q is not empty in repo %q", folder, h.name)
	}
	if err != nil {
		return "", 0, a.s.toStatus(ctx, err)
	}
	// Once main moves the bookkeeping must finish; the sequence row goes
	// first, as in landSource.
	bg := context.WithoutCancel(ctx)
	if err := a.s.store.RecordLand(bg, h.name, main, a.s.cfg.Now()); err != nil {
		return "", 0, status.Error(codes.Internal, "recording the land sequence failed")
	}
	if err := h.git.UpdateRef(bg, gitx.MainRef, main, base); err != nil {
		if errors.Is(err, gitx.ErrRefMoved) {
			return "", 0, status.Errorf(codes.Aborted, "main of repo %q moved during the migration; retry", h.name)
		}
		return "", 0, a.s.toStatus(ctx, err)
	}
	a.s.mirror.Notify(h.name)
	return main, n, nil
}

// migrateRef returns a fresh temporary ref for fetched history.
func migrateRef() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "refs/sgit/migrate/" + hex.EncodeToString(b[:]), nil
}

// remoteErr maps a failed fetch, ls-remote or push to a client status that
// carries git's own one-line reason.
func (s *Server) remoteErr(ctx context.Context, what, url string, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	var ge *gitx.GitError
	if errors.As(err, &ge) && slices.ContainsFunc([]string{"fetch", "ls-remote", "push"}, func(c string) bool { return slices.Contains(ge.Args, c) }) {
		return status.Errorf(codes.FailedPrecondition, "%s %s failed: %s", what, url, gitx.ErrSummary(err))
	}
	return s.toStatus(ctx, err)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server -run Migrate -count=1 -race`
Expected: PASS (Task 3 tests still pass).

- [ ] **Step 5: Commit**

```bash
git add internal/server/migrate.go internal/server/migrate_test.go
git commit -m "server: migrate in --into replays a repo's history under a monorepo folder" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

---

### Task 5: `MigrateOut`

**Files:**
- Modify: `internal/server/migrate.go` (add `MigrateOut`)
- Test: `internal/server/migrate_test.go` (append)

**Interfaces:**
- Consumes: Task 2 `PushFF`; Task 4 `remoteErr`; existing `a.resolveProject`, `a.s.projects.Get`, `a.s.projects.CatchUp`, `gitx.RemoteRef`, `(*gitx.Repo).Push`, `gitx.ErrRejected`, `auth.ProjectNotFound`.
- Produces: `func (a *adminService) MigrateOut(ctx, *sgitv1.MigrateOutRequest) (*sgitv1.MigrateOutResponse, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/migrate_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server -run 'MigrateOut|MigrateNeedsAdmin' -count=1`
Expected: FAIL with `method MigrateOut not implemented` (`TestMigrateNeedsAdmin`'s MigrateIn half already passes).

- [ ] **Step 3: Implement `MigrateOut`**

Append to `internal/server/migrate.go` (add `"github.com/hermes98761234/sgit/internal/auth"` to imports):

```go
// MigrateOut pushes a project's main to a standalone git repo (spec §3.4).
// sgit itself is not changed.
func (a *adminService) MigrateOut(ctx context.Context, req *sgitv1.MigrateOutRequest) (*sgitv1.MigrateOutResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.ToUrl == "" {
		return nil, status.Error(codes.InvalidArgument, "migrate out needs a target URL")
	}
	allow := a.s.repos.allowLocal
	if err := gitx.ValidateRemote(req.ToUrl, allow); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	p, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	h, err := a.s.projects.Get(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, auth.ProjectNotFound(req.Project)
	}
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	head, err := a.s.projects.CatchUp(ctx, h)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if head == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "project %q has no commits", p.Name)
	}
	remote, err := gitx.RemoteRef(ctx, req.ToUrl, gitx.MainRef, allow)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "reading", req.ToUrl, err)
	}
	resp := &sgitv1.MigrateOutResponse{Project: p.Name, Pushed: head}
	if remote == head {
		resp.UpToDate = true
		return resp, nil
	}
	if req.Force {
		err = h.Git.Push(ctx, req.ToUrl, head, remote, allow)
	} else {
		err = h.Git.PushFF(ctx, req.ToUrl, head, allow)
	}
	if errors.Is(err, gitx.ErrRejected) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"main of %s has diverged from project %q; use --force to overwrite it", req.ToUrl, p.Name)
	}
	if err != nil {
		return nil, a.s.remoteErr(ctx, "pushing to", req.ToUrl, err)
	}
	return resp, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server -count=1 -race`
Expected: PASS (whole server package).

- [ ] **Step 5: Commit**

```bash
git add internal/server/migrate.go internal/server/migrate_test.go
git commit -m "server: MigrateOut pushes a project's main to a standalone git repo" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

---

### Task 6: CLI `sg migrate`

**Files:**
- Create: `internal/cli/migrate.go`
- Modify: `internal/cli/cli.go:49-75` (register `migrate` in `commandTable` after `acl`, and in `groupCommands`)
- Test: `internal/cli/migrate_test.go` (`package cli_test`), `internal/cli/migrate_internal_test.go` (`package cli`)

**Interfaces:**
- Consumes: `a.newFlags`, `parse`, `usageErr`, `exitf`, `ExitError`, `a.admin`, `importTimeout`, `a.emit`, `short` (existing); proto types from Task 3.
- Produces: `cmdMigrate`, `migrateUsage`, `nameFromURL(string) string`.

- [ ] **Step 1: Write the failing tests**

The CLI tests are `package cli_test` (they call `cli.Run` through `harness_test.go`; `decode` lives in `admin_test.go`). `nameFromURL` is unexported, so its test goes in an internal test file. Create `internal/cli/migrate_internal_test.go`:

```go
package cli

import "testing"

func TestNameFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/api.git": "api",
		"https://github.com/acme/api/":    "api",
		"git@github.com:acme/api.git":     "api",
		"ssh://git@example.com/srv/web":   "web",
		"file:///tmp/x/api.git":           "api",
		"/srv/git/tool.git/":              "tool",
		"https://example.com/":            "example.com",
		"":                                "",
	} {
		if got := nameFromURL(in); got != want {
			t.Errorf("nameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
```

Create `internal/cli/migrate_test.go`:

```go
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
	if out := h.ok(dir, "migrate", "in", "file://"+src, "lib", "--project", "web", "--path", "vendor"); !strings.Contains(out, "mounted at vendor/ of project web\n") {
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
```

`decode` matches JSON keys case-insensitively (Go's `encoding/json` matches `Repo` to `"repo"` case-insensitively, so only the snake_case fields need tags).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli -run 'Migrate|NameFromURL' -count=1`
Expected: FAIL to compile (`undefined: nameFromURL`), and once that compiles, `unknown command "migrate"`.

- [ ] **Step 3: Implement the CLI**

Create `internal/cli/migrate.go`:

```go
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

const migrateUsage = "migrate in <url> [name] [--into <repo>:<folder>] [--project P --path DIR] [--mirror URL] | " +
	"migrate out <project> --to URL [--force]  [--server S]"

func cmdMigrate(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "in":
			return migrateIn(ctx, a, args[1:])
		case "out":
			return migrateOut(ctx, a, args[1:])
		}
	}
	return usageErr(migrateUsage)
}

func migrateIn(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("migrate in")
	into := fs.String("into", "", "replay main under <repo>:<folder> of an existing repo instead of creating a repo")
	project := fs.String("project", "", "mount into this existing project (needs --path)")
	path := fs.String("path", "", "mount path in --project")
	mirror := fs.String("mirror", "", "push main to this git URL after every land (not with --into)")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 || (*project == "") != (*path == "") {
		return usageErr(migrateUsage)
	}
	req := &sgitv1.MigrateInRequest{FromUrl: pos[0], Project: *project, Path: *path, MirrorUrl: *mirror}
	if len(pos) == 2 {
		req.Name = pos[1]
	} else if req.Name = nameFromURL(pos[0]); req.Name == "" {
		return exitf(ExitError, "cannot derive a name from %s; pass one: sg migrate in <url> <name>", pos[0])
	}
	if *into != "" {
		repo, folder, ok := strings.Cut(*into, ":")
		if !ok || repo == "" || folder == "" {
			return usageErr(migrateUsage)
		}
		req.IntoRepo, req.IntoFolder = repo, folder
	}
	var resp *sgitv1.MigrateInResponse
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.MigrateIn(ctx, req)
		return err
	})
	if err != nil {
		return err
	}
	skipped := resp.SkippedBranches
	if skipped == nil {
		skipped = []string{}
	}
	return a.emit(map[string]any{
		"repo": resp.Repo, "folder": resp.Folder, "project": resp.Project, "path": resp.Path,
		"main": resp.Main, "commits": resp.Commits, "skipped_branches": skipped, "project_head": resp.ProjectHead,
	}, func(w io.Writer) {
		target := resp.Repo
		if resp.Folder != "." {
			target += ":" + resp.Folder
		}
		noun := "commits"
		if resp.Commits == 1 {
			noun = "commit"
		}
		fmt.Fprintf(w, "migrated %s into %s: %d %s, main %s\n", pos[0], target, resp.Commits, noun, short(resp.Main))
		at := resp.Path + "/"
		if resp.Path == "." {
			at = "the project root"
		}
		fmt.Fprintf(w, "mounted at %s of project %s\n", at, resp.Project)
		if len(skipped) > 0 {
			fmt.Fprintf(w, "warning: branches not migrated: %s\n", strings.Join(skipped, ", "))
		}
	})
}

func migrateOut(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("migrate out")
	to := fs.String("to", "", "git URL to push the project's main to (required)")
	force := fs.Bool("force", false, "overwrite the target's main when it has diverged")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" {
		return usageErr(migrateUsage)
	}
	var resp *sgitv1.MigrateOutResponse
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: pos[0], ToUrl: *to, Force: *force})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"project": resp.Project, "pushed": resp.Pushed, "up_to_date": resp.UpToDate}, func(w io.Writer) {
		if resp.UpToDate {
			fmt.Fprintf(w, "%s is up to date with project %s (main %s)\n", *to, resp.Project, short(resp.Pushed))
			return
		}
		fmt.Fprintf(w, "pushed project %s to %s (main %s)\n", resp.Project, *to, short(resp.Pushed))
	})
}

// nameFromURL returns the last path segment of a git URL without ".git":
// https://host/acme/api.git and git@host:acme/api both give "api".
func nameFromURL(u string) string {
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return strings.TrimSuffix(u, ".git")
}
```

In `internal/cli/cli.go`, add after the `acl` entry of `commandTable`:

```go
		{"migrate", migrateUsage, cmdMigrate},
```

and add `"migrate": true` to `groupCommands`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli -count=1 -race`
Expected: PASS. If an existing help/usage test enumerates commands, update its expected list to include `migrate`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/migrate.go internal/cli/migrate_test.go internal/cli/migrate_internal_test.go internal/cli/cli.go
git commit -m "cli: sg migrate in and sg migrate out" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

---

### Task 7: Docs, spec notes, full verification, publish

**Files:**
- Modify: `docs/server.md` (new "Migrating repos in and out" section next to the import/mirror docs)
- Modify: `skills/sgit-admin/SKILL.md` (`.claude/skills/sgit-admin` is a symlink to it)
- Modify: `README.md` (one line in the admin overview)
- Modify: `docs/superpowers/specs/2026-10-15-sgit-migrate-design.md` (add §7 Implementation notes; set Status to "Implemented")

- [ ] **Step 1: Write `docs/server.md` section**

Add, in the same voice and heading level as the existing import/mirror section:

````markdown
## Migrating repos in and out

`sg migrate` moves code between plain git repos and the server. Both commands are admin only.

### In

```
sg migrate in <url> [name] [--into <repo>:<folder>] [--project P --path DIR] [--mirror URL]
```

- Without `--into`, the repo is imported as a new repo `name` (its `main` and `release/*` keep their SHAs), like `sg repo import`.
- With `--into mono:services/api`, the history of `main` is replayed under `services/api/` of the existing repo `mono`: one new commit per original first-parent commit, with the original author, committer, date and message. Commit SHAs change. Signatures are dropped and merges are flattened. The folder must be absent or empty. Lands into `mono` wait while the replay runs. Other branches are not migrated and are listed as skipped.
- `name` defaults to the last segment of the URL without `.git`.
- The code is then mounted at the root of a new project `name`, or with `--project P --path DIR` at `DIR` in the existing project `P`.
- If the project step fails, a plain import is rolled back. After `--into` the replayed commits stay; mount them with `sg project mount`.

### Out

```
sg migrate out <project> --to <url> [--force]
```

Pushes the project's `main` to `url`. The project is not changed; run it again to update the target. A project that mounts one whole repo at `.` keeps the repo's commit SHAs; other projects export their project history. The push is fast-forward only; `--force` overwrites a target whose `main` has diverged.
````

- [ ] **Step 2: Update the sgit-admin skill**

Add a "Migrate" subsection to the skill's command reference with the two usage lines and three examples:

```
sg migrate in https://github.com/acme/api.git                       # repo api + project api
sg migrate in https://github.com/acme/api.git --into mono:services/api  # into the monorepo
sg migrate out api --to git@github.com:acme/api-export.git
```

Also add "migrate a repo in or out" to the skill's `description:` trigger list in the frontmatter.

- [ ] **Step 3: README line**

In the admin overview list, add: `sg migrate in/out — bring a git repo in (as its own repo or into a monorepo folder) or push a project out as a git repo.`

- [ ] **Step 4: Spec implementation notes**

Append to the spec:

```markdown
## 7. Implementation notes

- `--into` fetches the source `main` straight into the target repo under a temporary `refs/sgit/migrate/<rand>` ref instead of a staging repo under `.incoming/`; the ref is deleted on every path. Unreferenced objects from a failed fetch are left for git's gc.
- `migrate out` does not fetch the target to test ancestry. It pushes without force, and git refuses a non-fast-forward; `--force` pushes with a lease on the `main` read by `ls-remote`.
- The default name is derived by the CLI; the server validates it like any repo or project name.
```

Set `**Status:**` to `Implemented`.

- [ ] **Step 5: Full verification**

Run: `make test && go vet ./... && staticcheck ./...` (or the Makefile's `vet`/`lint` targets)
Expected: all PASS, no vet or staticcheck findings.

Smoke test by hand against a dev server is optional; the integration tests cover the flows.

- [ ] **Step 6: Commit**

```bash
git add docs README.md skills
git commit -m "docs: sg migrate" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01XJvpCimfXurbmUfXpNXm2G"
```

- [ ] **Step 7: Publish**

```bash
git log --format='%h %an <%ae> | %cn <%ce> %s' origin/main..main   # every line must be No name <hermes98761234@tutamail.com>
gh auth setup-git
git push origin main
gh api repos/hermes98761234/sgit/branches/main --jq .commit.sha    # must equal: git rev-parse main
```
