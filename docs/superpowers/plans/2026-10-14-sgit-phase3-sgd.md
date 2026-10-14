# sgit Phase 3: `sgd` Cache Daemon Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `sgd`, a per-machine daemon. It listens on a Unix socket in front of one `sg-server`, serves `GetObjects` from a verified, content-addressed disk cache with LRU eviction, and passes every other Repo RPC through with the caller's token. `sg` uses it automatically when it is running and connects directly otherwise.

**Architecture:**
- `internal/cache` is the disk store.
- `internal/daemon` holds the gRPC proxy, with Repo pass-through, the cached `GetObjects`, per-token and per-repo read checks, and in-flight dedup.
- `internal/daemoncmd` and `cmd/sgd` provide the binary: flags, socket setup and shutdown.
- `internal/client` gains daemon discovery, and the CLI session uses sgd for Repo calls.
- Admin calls always go straight to the server.
- A new tiny `sgit.v1.Daemon` service has a single `Info` RPC, so the CLI can confirm which server the daemon fronts before sending it a token.

**Tech Stack:** Go, gRPC over Unix sockets, `github.com/klauspost/compress/zstd`, `container/list`, `syscall` (owner and permission checks).

**Spec:** `docs/superpowers/specs/2026-10-14-sgit-design.md`. Phase 3 is §12 item 3, and the daemon is defined in §7. The fetch protocol is §5 and daemon-down fallback is §10.

## Global Constraints

- **Shell setup.** Every shell command assumes:
  ```bash
  cd /home/vkopytsia/Projects/new/sgit
  export PATH=$HOME/.local/go/bin:$HOME/.local/protoc/bin:$HOME/go/bin:$PATH
  ```
- **No git.** The project dir is not a git repo. Never run `git init/add/commit/push` there. Implementers do not push; the controller publishes after review.
- **Real names.** Never write anyone's real name. Test users are `admin`, `bob` and `bob-reader`.
- **Socket path.** `$SG_SOCKET` if set, else `$XDG_RUNTIME_DIR/sg/sgd.sock`, else `<os.TempDir()>/sg-<uid>/sgd.sock`.
  - sgd creates the socket directory with mode `0700` and the socket with mode `0600`. That is one daemon per user.
  - Both sgd and sg refuse a socket, or socket directory, that is not owned by the current uid, or whose directory is group- or world-writable.
- **Environment variables.**
  - `SG_SOCKET`: socket path, used by sg and sgd.
  - `SG_NO_DAEMON=1`: sg never uses sgd.
  - `SGD_UPSTREAM`: default `--upstream`.
  - `SGD_CA`: default `--ca`.
- **Cache.**
  - Default directory: `os.UserCacheDir()/sg/objects`. Directories are `0700` and files `0600`.
  - Layout: `<dir>/<sha[:2]>/<sha[2:]>`. Each file starts with the header `sgc1 <type> <size>\n`, followed by the zstd bytes exactly as received from the server.
  - Default size cap: `10G`. Eviction is LRU, and the recency order survives restarts through file mtime.
  - Keys must match `^[0-9a-f]{40}$`, and anything else is rejected.
- **Integrity.** Every object is checked on the way into the cache and every time it is served from it. The check decompresses the data (bounded by `fetch.MaxObjectSize` = 128 MiB) and confirms that `objfmt.ObjectSHA(type, data) == sha`. A cached entry that fails is deleted and fetched again.
- **Authorization.**
  - sgd forwards the caller's `authorization` header to the server on every upstream call. A call with no header gets `Unauthenticated`.
  - Before it serves cached objects for a repo, sgd probes the server with `HasObjects{Repo, Shas: []}` using the caller's token. A successful probe is remembered for `ACLTTL`, default 60 s, keyed by the SHA-256 of the token plus the repo.
- **Never answered from the cache:** `HasObjects` and `ResolveRev`. Every Repo RPC other than `GetObjects` is passed through unchanged, and status details such as `ConflictDetail` survive.
- **In-flight dedup.** Concurrent requests for the same `(repo, sha)` share one upstream fetch.
  - The shared fetch is detached from any single caller's cancellation and has a 5-minute timeout.
  - A caller whose own context ends stops waiting.
- **CLI.** `sg` uses sgd only if `Daemon.Info().Upstream` equals the session's server name exactly.
  - A socket file that exists but does not answer: sg prints one line, `sg: warning: sgd at <path> is not usable (<err>); connecting directly`, and connects directly.
  - A socket that is missing, or a daemon in front of a different server: sg connects directly and prints nothing.
  - Admin RPCs and `sg login` always dial the server directly.
- **Testing.**
  - Tests use `internal/daemontest` (Task 3) for an in-process daemon on a short temp socket path, because Unix socket paths must stay under 108 bytes.
  - The CLI test harness sets `SG_SOCKET` to a non-existent path, so a developer's real sgd never interferes.
  - `testenv.Must` is called as `Must(f())`. It panics, so it must never be called in goroutines.
- **Lint and tests.** `gofmt -l .` must print nothing, and `go vet ./...`, `staticcheck ./...` and `go test -race -count=1 ./...` must all be clean before a task is done.

### Decisions beyond the spec (for review)
- **Socket mode `0600`, one daemon per user,** instead of the spec's `0660` system-service socket. A shared system socket would need group management and lets any group member read objects cached by other users. Per-user is the safe default. A system mode can be added later behind a flag.
- **`HasObjects` always goes to the server.** An object in the cache may have been fetched for a different repo. If sgd answered "present", the CLI would skip an upload and the commit would fail with "blob not uploaded".
- **New `sgit.v1.Daemon/Info` RPC.** This lets the CLI confirm which server sgd fronts, so a token is never sent to a daemon that would forward it to the wrong place. It is not authenticated: it is a local socket the user owns.
- **The socket owner check exists to stop token theft.** On the `/tmp` fallback path, another local user could otherwise pre-create `/tmp/sg-<uid>/` and run a fake sgd.
- **Shared cache across repos.** If an object is requested by SHA through repo Y, sgd may serve it even though it was cached for repo X. Content addressing means the caller already knows the hash. Spec §7 accepts this for the trusted-team deployment, and the README documents it.

## Review Focus

These are the five conditions most likely to hurt a user if broken, each pinned by a test in the task named:
1. **A token without read access to a repo must not get objects from it through the cache,** even when another user has already cached them, and revoking access must take effect within `ACLTTL`. Pinned in Task 4 (`TestCachedObjectsStillRequireReadAccess`).
2. **A corrupted or tampered cache file** must never be served. It is detected, dropped and fetched again. Pinned in Task 2 (`TestCorruptEntryIsAMiss`) and Task 4 (`TestCorruptCacheEntryIsRefetched`).
3. **A daemon that has died or left a stale socket file** must not break `sg`. sg warns and connects directly, and a new sgd removes the stale file and starts. Pinned in Task 5 (`TestStaleSocketIsReplaced`) and Task 6 (`TestUnusableDaemonWarnsAndFallsBack`).
4. **sgd running in front of a different server** must never receive this server's token; sg silently connects directly. Pinned in Task 6 (`TestDaemonForOtherServerIsIgnored`).
5. **The cache must stay within its size cap,** including across restarts, by evicting least-recently-used entries. Pinned in Task 2 (`TestLRUEviction`, `TestOpenEvictsAndCleans`).

---

## File Structure

```
internal/cli/commit.go            (Task 1) checkConflicts: only listed paths
internal/objfmt/objfmt.go         (Task 2) + ObjectSHA
internal/cache/cache.go           (Task 2) disk LRU cache
proto/sgit/v1/sgit.proto          (Task 3) + Daemon service; gen/ regenerated
internal/daemon/daemon.go         (Task 3) Config, Daemon, New, Register, NewGRPC, forward, Info
internal/daemon/proxy.go          (Task 3) Repo pass-through (+ Task 4 cached GetObjects)
internal/daemon/objects.go        (Task 4) authorize, verify, cached, fetch, fetchUpstream
internal/daemontest/daemontest.go (Task 3) CountingConn, Start (in-process sgd for tests)
internal/daemoncmd/daemoncmd.go   (Task 5) Run, ParseSize, listen, socket checks
cmd/sgd/main.go                   (Task 5)
internal/client/daemon.go         (Task 6) DefaultSocket, CheckSocketOwner, DialDaemon, ErrOtherUpstream
internal/cli/session.go           (Task 6) use sgd for Repo calls
README.md, contrib/sgd.service, docs spec §7 note (Task 7)
```

---

### Task 1: Commit only refuses markers in recorded conflict files

**Files:**
- Modify: `internal/cli/commit.go` (`checkConflicts` and its call site), `internal/cli/sync_test.go` (replace one test), `docs/superpowers/plans/2026-10-14-sgit-phase2-cli.md` (one bullet)

**Interfaces:**
- Changes `checkConflicts(w *wc.WC, changes []wc.Change) error` to `checkConflicts(w *wc.WC) error`. It checks only the paths in `w.Conflicts`. Sync records conflicts before writing markers, which the phase-2 final fix guarantees, so scanning other files is unnecessary, and that scan wrongly blocked files that contain marker lines on purpose.

- [ ] **Step 1: Write the failing test**

In `internal/cli/sync_test.go`, delete the function `TestCommitRefusesMarkersNotListedInConflicts` and add:
```go
func TestCommitAllowsMarkerLinesInUnlistedFiles(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	doc := "How to read a conflict:\n<<<<<<< ours\nmine\n=======\ntheirs\n>>>>>>> theirs\n"
	writeFile(t, filepath.Join(w, "docs", "conflicts.md"), doc)
	h.ok(w, "commit", "-m", "document conflict markers", "--land")
	if h.env.Files("web", "main")["docs/conflicts.md"] != doc {
		t.Fatal("file with intentional marker lines was not committed")
	}
}
```
(`TestCommitBlockedByConflictMarkers` and `TestSyncConflictWritesMarkersAndBlocksCommit` stay as they are and must keep passing: listed conflict files with markers still block.)

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test ./internal/cli/ -run TestCommitAllowsMarkerLinesInUnlistedFiles`
Expected: FAIL with exit 3 ("unresolved conflicts in docs/conflicts.md").

- [ ] **Step 3: Implement**

Replace `checkConflicts` in `internal/cli/commit.go` with:
```go
// checkConflicts drops resolved paths from w.Conflicts and fails while any
// listed file still contains conflict markers. Only paths recorded by sync
// are checked: other files may contain marker-like lines on purpose, and
// sync records its conflicts before writing any markers.
func checkConflicts(w *wc.WC) error {
	var still []string
	for _, p := range w.Conflicts {
		if data, _, err := wc.ReadLocal(w.Root, p); err == nil && hasMarkers(data) {
			still = append(still, p)
		}
	}
	if len(still) != len(w.Conflicts) {
		w.Conflicts = still
		if err := w.Save(); err != nil {
			return err
		}
	}
	if len(still) > 0 {
		return exitf(ExitConflict, "unresolved conflicts in %s (remove the conflict markers first)", strings.Join(still, ", "))
	}
	return nil
}
```
Change the call in `cmdCommit` from `checkConflicts(w, changes)` to `checkConflicts(w)`. If `changes` was computed only for this call, move the `w.Status()` call back below it so it is still used for the upload. Remove any import that becomes unused, such as `sort`.

In `docs/superpowers/plans/2026-10-14-sgit-phase2-cli.md`, replace the bullet that starts with "`sg checkout` into an existing directory that has no `.sg/base` resumes" with:
```markdown
- `sg checkout` resumes only an interrupted checkout (`.sg/` present without `.sg/base`); otherwise the target must be absent or empty (phase-2 final review ruling).
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/cli/`
Expected: `ok`.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 2: `objfmt.ObjectSHA` and the disk cache

**Files:**
- Modify: `internal/objfmt/objfmt.go`, `internal/objfmt/objfmt_test.go`
- Create: `internal/cache/cache.go`, `internal/cache/cache_test.go`

**Interfaces:**
- Produces:
  - `objfmt.ObjectSHA(typ string, data []byte) string`. `BlobSHA` becomes `ObjectSHA("blob", data)`.
  - `cache.Object{Type string; Size uint64; ZstdData []byte}`.
  - `cache.Open(dir string, maxBytes int64) (*cache.Cache, error)`.
  - Methods on `(*Cache)`:
    - `Get(sha string) (Object, bool)`
    - `Put(sha string, o Object) error`
    - `Has(sha string) bool`
    - `Remove(sha string)`
    - `Used() int64`
    - `Max() int64`
  - The cache stores bytes as given. Content verification is the daemon's job (Task 4). The cache only rejects malformed keys and malformed files.

- [ ] **Step 1: Write the failing tests**

Append to `internal/objfmt/objfmt_test.go`:
```go
func TestObjectSHA(t *testing.T) {
	if got := ObjectSHA("blob", []byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("blob: %s", got)
	}
	if got := ObjectSHA("tree", nil); got != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Fatalf("empty tree: %s", got)
	}
}
```

`internal/cache/cache_test.go`:
```go
package cache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sha(i int) string { return fmt.Sprintf("%040x", i+1) }

func obj(n int) Object { return Object{Type: "blob", Size: uint64(n), ZstdData: bytes.Repeat([]byte{'z'}, n)} }

func entrySize(o Object) int64 { return int64(len(encode(o))) }

func TestPutGetPersist(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	o := obj(10)
	if err := c.Put(sha(1), o); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get(sha(1))
	if !ok || got.Type != "blob" || got.Size != 10 || !bytes.Equal(got.ZstdData, o.ZstdData) || !c.Has(sha(1)) {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if c.Used() != entrySize(o) {
		t.Fatalf("Used = %d, want %d", c.Used(), entrySize(o))
	}
	fi, err := os.Stat(filepath.Join(dir, sha(1)[:2], sha(1)[2:]))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi, err)
	}
	c2, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Get(sha(1)); !ok || c2.Used() != c.Used() {
		t.Fatal("entry did not survive reopening")
	}
	if _, ok := c2.Get(sha(2)); ok {
		t.Fatal("unknown key must miss")
	}
}

func TestInvalidKeys(t *testing.T) {
	c, _ := Open(t.TempDir(), 1<<20)
	for _, k := range []string{"", "../etc/passwd", "ABCDEF0123456789abcdef0123456789abcdef01", "abc"} {
		if err := c.Put(k, obj(1)); err == nil {
			t.Errorf("Put(%q) must fail", k)
		}
		if _, ok := c.Get(k); ok || c.Has(k) {
			t.Errorf("Get(%q) must miss", k)
		}
	}
}

func TestLRUEviction(t *testing.T) {
	o := obj(100)
	c, _ := Open(t.TempDir(), 2*entrySize(o))
	_ = c.Put(sha(1), o)
	_ = c.Put(sha(2), o)
	if _, ok := c.Get(sha(1)); !ok { // 1 is now most recent
		t.Fatal("1 missing")
	}
	_ = c.Put(sha(3), o)
	if c.Has(sha(2)) || !c.Has(sha(1)) || !c.Has(sha(3)) {
		t.Fatalf("wrong eviction: has1=%v has2=%v has3=%v", c.Has(sha(1)), c.Has(sha(2)), c.Has(sha(3)))
	}
	if c.Used() > c.Max() {
		t.Fatalf("over cap: %d > %d", c.Used(), c.Max())
	}
}

func TestCorruptEntryIsAMiss(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 1<<20)
	_ = c.Put(sha(1), obj(10))
	p := filepath.Join(dir, sha(1)[:2], sha(1)[2:])
	if err := os.WriteFile(p, []byte("garbage without header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(sha(1)); ok {
		t.Fatal("corrupt entry must miss")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) || c.Has(sha(1)) || c.Used() != 0 {
		t.Fatal("corrupt entry must be removed")
	}
}

func TestOpenEvictsAndCleans(t *testing.T) {
	dir := t.TempDir()
	o := obj(100)
	c, _ := Open(dir, 10*entrySize(o))
	for i := 0; i < 3; i++ {
		_ = c.Put(sha(i), o)
		old := time.Now().Add(time.Duration(i-10) * time.Minute) // sha(0) oldest
		p := filepath.Join(dir, sha(i)[:2], sha(i)[2:])
		_ = os.Chtimes(p, old, old)
	}
	stray := filepath.Join(dir, sha(0)[:2], ".tmp-leftover")
	_ = os.WriteFile(stray, []byte("x"), 0o600)
	c2, err := Open(dir, 2*entrySize(o))
	if err != nil {
		t.Fatal(err)
	}
	if c2.Has(sha(0)) || !c2.Has(sha(1)) || !c2.Has(sha(2)) {
		t.Fatal("Open must evict the least recently used entries down to the cap")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("stray files must be removed on Open")
	}
	if _, err := Open(dir, 0); err == nil {
		t.Fatal("a non-positive cap must be rejected")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/objfmt/ ./internal/cache/`
Expected: FAIL to compile (`undefined: ObjectSHA`, `undefined: Open`).

- [ ] **Step 3: Implement**

In `internal/objfmt/objfmt.go`, replace `BlobSHA` with:
```go
// ObjectSHA returns the git object id of data stored as an object of type
// typ ("blob", "tree", "commit", "tag").
func ObjectSHA(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// BlobSHA returns the git object id of data stored as a blob.
func BlobSHA(data []byte) string { return ObjectSHA("blob", data) }
```

`internal/cache/cache.go`:
```go
// Package cache is sgd's content-addressed on-disk object cache with LRU
// eviction. Entries are immutable and keyed by git object SHA; the cache
// stores what it is given (callers verify content).
package cache

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hermes98761234/sgit/internal/fsutil"
)

// Object is a cached object in the form the server sends it.
type Object struct {
	Type     string
	Size     uint64 // uncompressed size
	ZstdData []byte
}

const magic = "sgc1"

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

type entry struct {
	sha   string
	bytes int64
}

// Cache is safe for concurrent use.
type Cache struct {
	dir   string
	max   int64
	mu    sync.Mutex
	lru   *list.List // front = most recently used
	index map[string]*list.Element
	used  int64
}

func encode(o Object) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s %d\n", magic, o.Type, o.Size)
	b.Write(o.ZstdData)
	return b.Bytes()
}

func decode(data []byte) (Object, error) {
	header, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return Object{}, errors.New("cache: missing header")
	}
	f := strings.Fields(string(header))
	if len(f) != 3 || f[0] != magic {
		return Object{}, errors.New("cache: bad header")
	}
	switch f[1] {
	case "blob", "tree", "commit", "tag":
	default:
		return Object{}, fmt.Errorf("cache: bad object type %q", f[1])
	}
	size, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return Object{}, err
	}
	return Object{Type: f[1], Size: size, ZstdData: body}, nil
}

func (c *Cache) path(sha string) string { return filepath.Join(c.dir, sha[:2], sha[2:]) }

// Open opens (creating if needed) a cache in dir limited to maxBytes. Existing
// entries are indexed in mtime order; stray files are removed and the cache
// is evicted down to the cap.
func Open(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes <= 0 {
		return nil, errors.New("cache: size limit must be positive")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, max: maxBytes, lru: list.New(), index: map[string]*list.Element{}}
	type found struct {
		sha   string
		size  int64
		mtime time.Time
	}
	var all []found
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		key := strings.Replace(rel, "/", "", 1)
		if len(rel) != 41 || rel[2] != '/' || !shaRe.MatchString(key) {
			return os.Remove(p) // temp file or junk
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		all = append(all, found{key, fi.Size(), fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mtime.Before(all[j].mtime) })
	for _, f := range all {
		c.index[f.sha] = c.lru.PushFront(&entry{f.sha, f.size})
		c.used += f.size
	}
	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()
	return c, nil
}

// Has reports whether sha is cached (without reading it).
func (c *Cache) Has(sha string) bool {
	if !shaRe.MatchString(sha) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[sha]
	return ok
}

// Get returns the cached object. Unreadable or malformed entries are removed
// and reported as misses.
func (c *Cache) Get(sha string) (Object, bool) {
	if !shaRe.MatchString(sha) {
		return Object{}, false
	}
	c.mu.Lock()
	el, ok := c.index[sha]
	if ok {
		c.lru.MoveToFront(el)
	}
	c.mu.Unlock()
	if !ok {
		return Object{}, false
	}
	data, err := os.ReadFile(c.path(sha))
	if err != nil {
		c.Remove(sha)
		return Object{}, false
	}
	o, err := decode(data)
	if err != nil {
		c.Remove(sha)
		return Object{}, false
	}
	now := time.Now()
	_ = os.Chtimes(c.path(sha), now, now) // persist recency for the next Open
	return o, true
}

// Put stores o under sha (a no-op if already present) and evicts down to the cap.
func (c *Cache) Put(sha string, o Object) error {
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("cache: invalid key %q", sha)
	}
	if c.Has(sha) {
		c.mu.Lock()
		if el, ok := c.index[sha]; ok {
			c.lru.MoveToFront(el)
		}
		c.mu.Unlock()
		return nil
	}
	data := encode(o)
	p := c.path(sha)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(p, data, 0o600); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.index[sha]; ok { // a concurrent Put of the same object won
		return nil
	}
	c.index[sha] = c.lru.PushFront(&entry{sha, int64(len(data))})
	c.used += int64(len(data))
	c.evictLocked()
	return nil
}

// Remove deletes sha from the cache.
func (c *Cache) Remove(sha string) {
	if !shaRe.MatchString(sha) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[sha]; ok {
		c.removeLocked(el)
		return
	}
	_ = os.Remove(c.path(sha))
}

func (c *Cache) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	c.lru.Remove(el)
	delete(c.index, e.sha)
	c.used -= e.bytes
	_ = os.Remove(c.path(e.sha))
}

func (c *Cache) evictLocked() {
	for c.used > c.max && c.lru.Len() > 0 {
		c.removeLocked(c.lru.Back())
	}
}

// Used is the total size of cached files in bytes.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Max is the size limit in bytes.
func (c *Cache) Max() int64 { return c.max }
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/objfmt/ ./internal/cache/`
Expected: both `ok`.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---
### Task 3: Daemon service, Repo pass-through proxy, test helper

**Files:**
- Modify: `proto/sgit/v1/sgit.proto` (add the `Daemon` service). Regenerate `gen/sgit/v1/` with `make gen`.
- Create: `internal/daemon/daemon.go`, `internal/daemon/proxy.go`, `internal/daemontest/daemontest.go`
- Test: `internal/daemon/proxy_test.go`

**Interfaces:**
- Consumes:
  - `rpc.ServerOptions()`, `rpc.DialOptions(token, nil)`, `rpc.UploadBlobs`.
  - `cache.Open/Used/Max` (Task 2).
  - `testenv.Start/CreateRepo/CommitDirect/Upload/Files/Add/Must`.
  - `fetch.Tree`.
- Produces:
  - Generated `sgitv1.DaemonClient/DaemonServer`, `sgitv1.DaemonInfoRequest{}` and `sgitv1.DaemonInfoResponse{Upstream, Version string; CacheBytes, CacheMaxBytes uint64}`.
  - `daemon.Version`, and `daemon.Config{Upstream string; Conn grpc.ClientConnInterface; Cache *cache.Cache; ACLTTL time.Duration; Log *slog.Logger}`.
  - `daemon.New(Config) (*Daemon, error)`, `(*Daemon).Register(*grpc.Server)`, `(*Daemon).Close()`, `daemon.NewGRPC() *grpc.Server`.
  - In-package: `forward(ctx) (out context.Context, token string, err error)`, `type proxy`, `pass[...]`.
  - `daemontest.Counter`, with `Calls() int` (GetObjects streams opened), `Requested(sha string) int` and `Total() int` (SHAs requested upstream).
  - `daemontest.CountingConn(t, addr) (*grpc.ClientConn, *Counter)`.
  - `daemontest.Start(t, upstreamAddr string, opts ...func(*daemon.Config)) *daemontest.Env`. The Env has the fields `Socket`, `CacheDir`, `Cache`, `Daemon` and `Counter`, and the methods `Conn(token)`, `Repo(token) sgitv1.RepoClient` and `Info() *sgitv1.DaemonInfoResponse`.

- [ ] **Step 1: Add the proto service and regenerate**

Append to `proto/sgit/v1/sgit.proto`:
```proto
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
Run: `make gen && go build ./gen/...`
Expected: the build succeeds, and `gen/sgit/v1/sgit_grpc.pb.go` contains `DaemonServer`.

- [ ] **Step 2: Write the failing tests**

`internal/daemon/proxy_test.go`:
```go
package daemon_test

import (
	"bytes"
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPassThrough(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	d := daemontest.Start(t, e.Addr)
	c := d.Repo(e.AdminToken)
	ctx := context.Background()

	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web", Rev: "main"}))
	if rr.Sha != main {
		t.Fatalf("ResolveRev via sgd = %s, want %s", rr.Sha, main)
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 200<<10) // 3.2 MiB, multi-chunk
	shas := testenv.Must(rpc.UploadBlobs(ctx, c, "web", [][]byte{big, []byte("x\n")}))
	if shas[0] != objfmt.BlobSHA(big) || shas[1] != objfmt.BlobSHA([]byte("x\n")) {
		t.Fatalf("upload via sgd = %v", shas)
	}
	has := testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Repo: "web", Shas: shas}))
	if len(has.Present) != 2 || !has.Present[0] || !has.Present[1] {
		t.Fatalf("HasObjects via sgd = %v", has.Present)
	}
	land := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{
		Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("big.bin", shas[0])}, Message: "via sgd",
	}))
	if e.Files("web", "main")["big.bin"] != string(big) {
		t.Fatal("land via sgd did not reach main")
	}
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Repo: "web", N: 1}))
	if len(lg.Entries) != 1 || lg.Entries[0].Sha != land.Commit {
		t.Fatalf("log via sgd = %+v", lg.Entries)
	}
	tree := testenv.Must(fetch.Tree(ctx, c, "web", land.Commit, nil))
	if tree["big.bin"].SHA != shas[0] {
		t.Fatalf("tree via sgd = %+v", tree)
	}
}

func TestConflictDetailSurvivesProxy(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	base := e.CommitDirect("web", map[string]string{"f.txt": "1\n2\n3\n"}, "base")
	d := daemontest.Start(t, e.Addr)
	c := d.Repo(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("1\nA\n3\n"), []byte("1\nB\n3\n"))
	testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[0])}, Message: "a"}))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[1])}, Message: "b"})
	st := status.Convert(err)
	if st.Code() != codes.Aborted {
		t.Fatalf("want Aborted, got %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, x := range st.Details() {
		if cd, ok := x.(*sgitv1.ConflictDetail); ok {
			det = cd
		}
	}
	if det == nil || len(det.Files) != 1 || det.Files[0].Path != "f.txt" {
		t.Fatalf("ConflictDetail lost through sgd: %+v", det)
	}
}

func TestDaemonRequiresToken(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	d := daemontest.Start(t, e.Addr)
	ctx := context.Background()
	if _, err := d.Repo("").ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: want Unauthenticated, got %v", err)
	}
	if _, err := d.Repo("sgt_nope").ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: want Unauthenticated, got %v", err)
	}
}

func TestInfo(t *testing.T) {
	e := testenv.Start(t)
	d := daemontest.Start(t, e.Addr)
	info := d.Info()
	if info.Upstream != e.Addr || info.Version == "" || info.CacheMaxBytes == 0 {
		t.Fatalf("Info = %+v", info)
	}
}
```

- [ ] **Step 3: Run the tests to confirm they fail**

Run: `go test ./internal/daemon/`
Expected: FAIL to compile (no `internal/daemontest`, no `daemon` package).

- [ ] **Step 4: Implement**

`internal/daemon/daemon.go`:
```go
// Package daemon implements sgd: a per-user gRPC proxy in front of one
// sg-server. GetObjects is served from a verified disk cache; every other
// Repo RPC is forwarded with the caller's token.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Version is reported by Daemon/Info.
const Version = "0.3.0"

// Config configures a Daemon.
type Config struct {
	Upstream string                   // upstream server address, reported by Info
	Conn     grpc.ClientConnInterface // connection to the upstream (no token attached)
	Cache    *cache.Cache
	ACLTTL   time.Duration // how long a successful read probe is trusted (default 60s)
	Log      *slog.Logger
}

// Daemon is the sgd service.
type Daemon struct {
	cfg Config
	up  sgitv1.RepoClient
}

// New validates cfg and returns a Daemon.
func New(cfg Config) (*Daemon, error) {
	if cfg.Conn == nil || cfg.Cache == nil {
		return nil, errors.New("daemon: Conn and Cache are required")
	}
	if cfg.ACLTTL == 0 {
		cfg.ACLTTL = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Daemon{cfg: cfg, up: sgitv1.NewRepoClient(cfg.Conn)}, nil
}

// NewGRPC returns a gRPC server with sgit's message limits.
func NewGRPC() *grpc.Server { return grpc.NewServer(rpc.ServerOptions()...) }

// Register adds the Repo, Daemon and health services to g.
func (d *Daemon) Register(g *grpc.Server) {
	sgitv1.RegisterRepoServer(g, &proxy{d: d})
	sgitv1.RegisterDaemonServer(g, &infoServer{d: d})
	healthpb.RegisterHealthServer(g, health.NewServer())
}

// Close releases resources.
func (d *Daemon) Close() {}

// forward copies the caller's authorization header onto a context for the
// upstream call. Calls without one are rejected here.
func forward(ctx context.Context) (context.Context, string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 || vals[0] == "" {
		return nil, "", status.Error(codes.Unauthenticated, "missing token")
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", vals[0])), vals[0], nil
}

type infoServer struct {
	sgitv1.UnimplementedDaemonServer
	d *Daemon
}

func (s *infoServer) Info(context.Context, *sgitv1.DaemonInfoRequest) (*sgitv1.DaemonInfoResponse, error) {
	return &sgitv1.DaemonInfoResponse{
		Upstream: s.d.cfg.Upstream, Version: Version,
		CacheBytes: uint64(s.d.cfg.Cache.Used()), CacheMaxBytes: uint64(s.d.cfg.Cache.Max()),
	}, nil
}
```

`internal/daemon/proxy.go`:
```go
package daemon

import (
	"context"
	"io"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc"
)

type proxy struct {
	sgitv1.UnimplementedRepoServer
	d *Daemon
}

// pass forwards one unary call upstream with the caller's token. Upstream
// errors (including status details such as ConflictDetail) are returned as is.
func pass[Req, Resp any](ctx context.Context, req Req, call func(context.Context, Req, ...grpc.CallOption) (Resp, error)) (Resp, error) {
	out, _, err := forward(ctx)
	if err != nil {
		var zero Resp
		return zero, err
	}
	return call(out, req)
}

func (p *proxy) ResolveRev(ctx context.Context, r *sgitv1.ResolveRevRequest) (*sgitv1.ResolveRevResponse, error) {
	return pass(ctx, r, p.d.up.ResolveRev)
}

func (p *proxy) HasObjects(ctx context.Context, r *sgitv1.HasObjectsRequest) (*sgitv1.HasObjectsResponse, error) {
	return pass(ctx, r, p.d.up.HasObjects)
}

func (p *proxy) CreateDraft(ctx context.Context, r *sgitv1.CreateDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.CreateDraft)
}

func (p *proxy) UpdateDraft(ctx context.Context, r *sgitv1.UpdateDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.UpdateDraft)
}

func (p *proxy) GetDraft(ctx context.Context, r *sgitv1.GetDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.GetDraft)
}

func (p *proxy) ListDrafts(ctx context.Context, r *sgitv1.ListDraftsRequest) (*sgitv1.ListDraftsResponse, error) {
	return pass(ctx, r, p.d.up.ListDrafts)
}

func (p *proxy) AbandonDraft(ctx context.Context, r *sgitv1.AbandonDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.AbandonDraft)
}

func (p *proxy) Land(ctx context.Context, r *sgitv1.LandRequest) (*sgitv1.LandResponse, error) {
	return pass(ctx, r, p.d.up.Land)
}

func (p *proxy) CommitAndLand(ctx context.Context, r *sgitv1.CommitAndLandRequest) (*sgitv1.LandResponse, error) {
	return pass(ctx, r, p.d.up.CommitAndLand)
}

func (p *proxy) Log(ctx context.Context, r *sgitv1.LogRequest) (*sgitv1.LogResponse, error) {
	return pass(ctx, r, p.d.up.Log)
}

func (p *proxy) Blame(ctx context.Context, r *sgitv1.BlameRequest) (*sgitv1.BlameResponse, error) {
	return pass(ctx, r, p.d.up.Blame)
}

func (p *proxy) Diff(ctx context.Context, r *sgitv1.DiffRequest) (*sgitv1.DiffResponse, error) {
	return pass(ctx, r, p.d.up.Diff)
}

func (p *proxy) MergeFile(ctx context.Context, r *sgitv1.MergeFileRequest) (*sgitv1.MergeFileResponse, error) {
	return pass(ctx, r, p.d.up.MergeFile)
}

// UploadBlobs relays the client stream to the upstream.
func (p *proxy) UploadBlobs(stream grpc.ClientStreamingServer[sgitv1.BlobChunk, sgitv1.UploadBlobsResponse]) error {
	out, _, err := forward(stream.Context())
	if err != nil {
		return err
	}
	up, err := p.d.up.UploadBlobs(out)
	if err != nil {
		return err
	}
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			resp, err := up.CloseAndRecv()
			if err != nil {
				return err
			}
			return stream.SendAndClose(resp)
		}
		if err != nil {
			return err
		}
		if err := up.Send(c); err != nil {
			if _, rerr := up.CloseAndRecv(); rerr != nil {
				return rerr
			}
			return err
		}
	}
}

// GetObjects relays the server stream. (Task 4 serves it from the cache.)
func (p *proxy) GetObjects(req *sgitv1.GetObjectsRequest, stream grpc.ServerStreamingServer[sgitv1.Object]) error {
	out, _, err := forward(stream.Context())
	if err != nil {
		return err
	}
	up, err := p.d.up.GetObjects(out, req)
	if err != nil {
		return err
	}
	for {
		o, err := up.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(o); err != nil {
			return err
		}
	}
}
```

`internal/daemontest/daemontest.go`:
```go
// Package daemontest runs an in-process sgd for tests, in front of an
// upstream sg-server (usually testenv), with a counting upstream connection.
package daemontest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/daemon"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc"
)

// Counter counts GetObjects traffic the daemon sends upstream.
type Counter struct {
	mu    sync.Mutex
	calls int
	shas  map[string]int
}

func (c *Counter) add(shas []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	for _, s := range shas {
		c.shas[s]++
	}
}

// Calls is the number of upstream GetObjects streams opened.
func (c *Counter) Calls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

// Requested is how often sha was requested upstream.
func (c *Counter) Requested(sha string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.shas[sha] }

// Total is the number of SHAs requested upstream.
func (c *Counter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.shas {
		n += v
	}
	return n
}

type countingStream struct {
	grpc.ClientStream
	c *Counter
}

func (s *countingStream) SendMsg(m any) error {
	if r, ok := m.(*sgitv1.GetObjectsRequest); ok {
		s.c.add(r.Shas)
	}
	return s.ClientStream.SendMsg(m)
}

// CountingConn dials addr (plaintext, no token) and counts GetObjects requests.
func CountingConn(t testing.TB, addr string) (*grpc.ClientConn, *Counter) {
	t.Helper()
	c := &Counter{shas: map[string]int{}}
	si := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err == nil && method == "/sgit.v1.Repo/GetObjects" {
			return &countingStream{ClientStream: cs, c: c}, nil
		}
		return cs, err
	}
	conn := testenv.Must(grpc.NewClient(addr, append(rpc.DialOptions("", nil), grpc.WithStreamInterceptor(si))...))
	t.Cleanup(func() { conn.Close() })
	return conn, c
}

// Env is a running in-process sgd.
type Env struct {
	t        testing.TB
	Socket   string
	CacheDir string
	Cache    *cache.Cache
	Daemon   *daemon.Daemon
	Counter  *Counter
}

// Start runs sgd on a short temp socket in front of upstreamAddr. opts may
// adjust the config (e.g. ACLTTL, Upstream, Cache).
func Start(t testing.TB, upstreamAddr string, opts ...func(*daemon.Config)) *Env {
	t.Helper()
	conn, counter := CountingConn(t, upstreamAddr)
	cacheDir := t.TempDir()
	c := testenv.Must(cache.Open(cacheDir, 1<<30))
	cfg := daemon.Config{Upstream: upstreamAddr, Conn: conn, Cache: c}
	for _, o := range opts {
		o(&cfg)
	}
	d := testenv.Must(daemon.New(cfg))
	dir := testenv.Must(os.MkdirTemp("", "sgd")) // short path: unix sockets are limited to ~108 bytes
	sock := filepath.Join(dir, "s.sock")
	lis := testenv.Must(net.Listen("unix", sock))
	g := daemon.NewGRPC()
	d.Register(g)
	go g.Serve(lis)
	t.Cleanup(func() {
		g.Stop()
		d.Close()
		os.RemoveAll(dir)
	})
	return &Env{t: t, Socket: sock, CacheDir: cacheDir, Cache: cfg.Cache, Daemon: d, Counter: counter}
}

// Conn dials the daemon socket with token.
func (e *Env) Conn(token string) *grpc.ClientConn {
	conn := testenv.Must(grpc.NewClient("unix://"+e.Socket, rpc.DialOptions(token, nil)...))
	e.t.Cleanup(func() { conn.Close() })
	return conn
}

func (e *Env) Repo(token string) sgitv1.RepoClient { return sgitv1.NewRepoClient(e.Conn(token)) }

// Info calls Daemon/Info.
func (e *Env) Info() *sgitv1.DaemonInfoResponse {
	return testenv.Must(sgitv1.NewDaemonClient(e.Conn("")).Info(context.Background(), &sgitv1.DaemonInfoRequest{}))
}
```

- [ ] **Step 5: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race -count=1 ./internal/daemon/`
Expected: `ok`.

- [ ] **Step 6: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 4: Cached `GetObjects`: verification, read probes, in-flight dedup

**Files:**
- Modify: `internal/daemon/daemon.go` (the `Daemon` struct, `New` and `Close`), `internal/daemon/proxy.go` (replace `GetObjects`)
- Create: `internal/daemon/objects.go`
- Test: `internal/daemon/objects_test.go`

**Interfaces:**
- Consumes: `cache.Get/Put/Remove` (Task 2), `objfmt.ObjectSHA`, `fetch.MaxObjectSize`, `fetch.Batch`, and `daemontest` (Task 3).
- Produces:
  - In package `daemon`: `authorize(ctx, repo) (context.Context, error)`, `verify(sha, cache.Object) error`, `cached(sha) (cache.Object, bool)`, `fetch(ctx, repo, shas) (map[string]cache.Object, error)`, `fetchUpstream(ctx, repo, shas)` and `toPB(sha, cache.Object) *sgitv1.Object`.
  - `GetObjects` now runs `authorize` (the probe, cached for ACLTTL), serves verified cache hits, and fetches misses through the shared in-flight map, keyed by `repo+"\x00"+sha`.

- [ ] **Step 1: Write the failing tests**

`internal/daemon/objects_test.go`:
```go
package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/daemon"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func seeded(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	e := testenv.Start(t)
	e.CreateRepo("web")
	return e, e.CommitDirect("web", map[string]string{"a.txt": "A\n", "src/x.go": "X\n", "docs/d.md": "D\n"}, "seed")
}

func readAll(t *testing.T, c sgitv1.RepoClient, commit string) map[string]fetch.File {
	t.Helper()
	ctx := context.Background()
	tree := testenv.Must(fetch.Tree(ctx, c, "web", commit, nil))
	var shas []string
	for _, f := range tree {
		shas = append(shas, f.SHA)
	}
	testenv.Must(fetch.Blobs(ctx, c, "web", shas))
	return tree
}

func TestSecondFetchServedFromCache(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	readAll(t, d.Repo(e.AdminToken), commit)
	first := d.Counter.Total()
	if first == 0 {
		t.Fatal("cold fetch must go upstream")
	}
	readAll(t, d.Repo(e.AdminToken), commit)
	if d.Counter.Total() != first {
		t.Fatalf("warm fetch went upstream: %d -> %d", first, d.Counter.Total())
	}
	// A restarted daemon on the same cache directory serves from disk.
	reopened := testenv.Must(cache.Open(d.CacheDir, 1<<30))
	d2 := daemontest.Start(t, e.Addr, func(c *daemon.Config) { c.Cache = reopened })
	readAll(t, d2.Repo(e.AdminToken), commit)
	if d2.Counter.Total() != 0 {
		t.Fatalf("restarted daemon went upstream for %d objects", d2.Counter.Total())
	}
}

func TestConcurrentRequestsShareOneUpstreamFetch(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	ctx := context.Background()
	tree := testenv.Must(fetch.Tree(ctx, e.Repo(e.AdminToken), "web", commit, nil)) // directly, not via sgd
	sha := tree["a.txt"].SHA
	c := d.Repo(e.AdminToken)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			b, err := fetch.Blobs(ctx, c, "web", []string{sha})
			if err == nil && string(b[sha]) != "A\n" {
				err = errors.New("wrong content")
			}
			errs <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := d.Counter.Requested(sha); n != 1 {
		t.Fatalf("blob requested upstream %d times, want 1", n)
	}
}

func TestCachedObjectsStillRequireReadAccess(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr, func(c *daemon.Config) { c.ACLTTL = 50 * time.Millisecond })
	ctx := context.Background()
	readAll(t, d.Repo(e.AdminToken), commit) // cache is warm now
	nobody := e.NewUser("bob", "", "")
	if _, err := fetch.Tree(ctx, d.Repo(nobody), "web", commit, nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("user without access got cached objects: %v", err)
	}
	reader := e.NewUser("bob-reader", "web", "read")
	testenv.Must(fetch.Tree(ctx, d.Repo(reader), "web", commit, nil))
	testenv.Must(e.Admin(e.AdminToken).RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "bob-reader", Repo: "web"}))
	time.Sleep(150 * time.Millisecond)
	if _, err := fetch.Tree(ctx, d.Repo(reader), "web", commit, nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked access still served after ACLTTL: %v", err)
	}
}

func TestCorruptCacheEntryIsRefetched(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	c := d.Repo(e.AdminToken)
	tree := readAll(t, c, commit)
	sha := tree["a.txt"].SHA
	before := d.Counter.Requested(sha)
	enc, _ := zstd.NewWriter(nil)
	forged := append([]byte("sgc1 blob 2\n"), enc.EncodeAll([]byte("B\n"), nil)...) // well-formed, wrong content
	if err := os.WriteFile(filepath.Join(d.CacheDir, sha[:2], sha[2:]), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	b := testenv.Must(fetch.Blobs(context.Background(), c, "web", []string{sha}))
	if string(b[sha]) != "A\n" {
		t.Fatalf("served forged content: %q", b[sha])
	}
	if d.Counter.Requested(sha) != before+1 {
		t.Fatal("corrupt entry must be refetched from upstream")
	}
}

func TestGetObjectsRejectsBadSHA(t *testing.T) {
	e, _ := seeded(t)
	d := daemontest.Start(t, e.Addr)
	stream, err := d.Repo(e.AdminToken).GetObjects(context.Background(), &sgitv1.GetObjectsRequest{Repo: "web", Shas: []string{"../../etc/passwd"}})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/daemon/ -run 'Cache|Concurrent|ReadAccess|Corrupt|BadSHA'`
Expected: FAIL. With pass-through, the warm fetch still goes upstream, the counts are wrong, and the bad SHA is reported as `InvalidArgument` by the server only after the probe. The exact failures depend on the test.

- [ ] **Step 3: Implement**

In `internal/daemon/daemon.go`, replace the `Daemon` struct, `New` and `Close` with the following, and add `"sync"`, `"github.com/hermes98761234/sgit/internal/fetch"` and `"github.com/klauspost/compress/zstd"` to the imports:
```go
// Daemon is the sgd service.
type Daemon struct {
	cfg      Config
	up       sgitv1.RepoClient
	dec      *zstd.Decoder
	mu       sync.Mutex
	acl      map[aclKey]time.Time // successful read probes, until expiry
	inflight map[string]*call     // repo+"\x00"+sha -> shared upstream fetch
}

// New validates cfg and returns a Daemon.
func New(cfg Config) (*Daemon, error) {
	if cfg.Conn == nil || cfg.Cache == nil {
		return nil, errors.New("daemon: Conn and Cache are required")
	}
	if cfg.ACLTTL == 0 {
		cfg.ACLTTL = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(fetch.MaxObjectSize))
	if err != nil {
		return nil, err
	}
	return &Daemon{
		cfg: cfg, up: sgitv1.NewRepoClient(cfg.Conn), dec: dec,
		acl: map[aclKey]time.Time{}, inflight: map[string]*call{},
	}, nil
}

// Close releases resources.
func (d *Daemon) Close() { d.dec.Close() }
```

`internal/daemon/objects.go`:
```go
package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"regexp"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxBatch = 10000

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

type aclKey struct {
	token [32]byte
	repo  string
}

// call is one upstream fetch of an object that other requests may wait on.
type call struct {
	done chan struct{}
	obj  cache.Object
	err  error
}

// authorize checks (or recalls, for ACLTTL) that the caller may read repo by
// probing the upstream with an empty HasObjects using the caller's token.
func (d *Daemon) authorize(ctx context.Context, repo string) (context.Context, error) {
	out, tok, err := forward(ctx)
	if err != nil {
		return nil, err
	}
	key := aclKey{sha256.Sum256([]byte(tok)), repo}
	now := time.Now()
	d.mu.Lock()
	exp, ok := d.acl[key]
	d.mu.Unlock()
	if ok && now.Before(exp) {
		return out, nil
	}
	if _, err := d.up.HasObjects(out, &sgitv1.HasObjectsRequest{Repo: repo}); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if len(d.acl) > 1024 {
		for k, e := range d.acl {
			if now.After(e) {
				delete(d.acl, k)
			}
		}
	}
	d.acl[key] = now.Add(d.cfg.ACLTTL)
	d.mu.Unlock()
	return out, nil
}

// verify decompresses o and checks that its content hashes to sha.
func (d *Daemon) verify(sha string, o cache.Object) error {
	if o.Size > fetch.MaxObjectSize {
		return fmt.Errorf("object %s too large (%d bytes)", sha, o.Size)
	}
	data, err := d.dec.DecodeAll(o.ZstdData, make([]byte, 0, o.Size))
	if err != nil {
		return fmt.Errorf("object %s: %w", sha, err)
	}
	if uint64(len(data)) != o.Size || objfmt.ObjectSHA(o.Type, data) != sha {
		return fmt.Errorf("object %s failed verification", sha)
	}
	return nil
}

// cached returns a verified cache entry; bad entries are dropped.
func (d *Daemon) cached(sha string) (cache.Object, bool) {
	o, ok := d.cfg.Cache.Get(sha)
	if !ok {
		return cache.Object{}, false
	}
	if err := d.verify(sha, o); err != nil {
		d.cfg.Log.Warn("dropping bad cache entry", "sha", sha, "err", err)
		d.cfg.Cache.Remove(sha)
		return cache.Object{}, false
	}
	return o, true
}

func toPB(sha string, o cache.Object) *sgitv1.Object {
	return &sgitv1.Object{Sha: sha, Type: o.Type, Size: o.Size, ZstdData: o.ZstdData}
}

// fetch returns the objects for shas from upstream, sharing in-flight fetches
// of the same (repo, sha) with concurrent requests.
func (d *Daemon) fetch(ctx context.Context, repo string, shas []string) (map[string]cache.Object, error) {
	d.mu.Lock()
	calls := make(map[string]*call, len(shas))
	var mine []string
	for _, s := range shas {
		key := repo + "\x00" + s
		if c, ok := d.inflight[key]; ok {
			calls[s] = c
			continue
		}
		c := &call{done: make(chan struct{})}
		d.inflight[key] = c
		calls[s] = c
		mine = append(mine, s)
	}
	d.mu.Unlock()
	if len(mine) > 0 {
		// Detached from this caller's cancellation: other requests may be
		// waiting on these objects. The caller's token travels in the values.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		d.fetchUpstream(fctx, repo, mine)
		cancel()
	}
	out := make(map[string]cache.Object, len(shas))
	for s, c := range calls {
		select {
		case <-c.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c.err != nil {
			return nil, c.err
		}
		out[s] = c.obj
	}
	return out, nil
}

// fetchUpstream fetches shas from upstream, verifies and caches them, and
// completes their in-flight calls.
func (d *Daemon) fetchUpstream(ctx context.Context, repo string, shas []string) {
	want := make(map[string]bool, len(shas))
	for _, s := range shas {
		want[s] = true
	}
	got := make(map[string]cache.Object, len(shas))
	var ferr error
	for start := 0; start < len(shas) && ferr == nil; start += fetch.Batch {
		end := min(start+fetch.Batch, len(shas))
		stream, err := d.up.GetObjects(ctx, &sgitv1.GetObjectsRequest{Repo: repo, Shas: shas[start:end]})
		if err != nil {
			ferr = err
			break
		}
		for {
			o, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				ferr = err
				break
			}
			if !want[o.Sha] {
				ferr = status.Errorf(codes.Internal, "upstream returned unrequested object %s", o.Sha)
				break
			}
			obj := cache.Object{Type: o.Type, Size: o.Size, ZstdData: o.ZstdData}
			if err := d.verify(o.Sha, obj); err != nil {
				ferr = status.Errorf(codes.Internal, "%v", err)
				break
			}
			if err := d.cfg.Cache.Put(o.Sha, obj); err != nil {
				d.cfg.Log.Warn("cache put failed", "sha", o.Sha, "err", err)
			}
			got[o.Sha] = obj
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range shas {
		key := repo + "\x00" + s
		c := d.inflight[key]
		delete(d.inflight, key)
		switch o, ok := got[s]; {
		case ok:
			c.obj = o
		case ferr != nil:
			c.err = ferr
		default:
			c.err = status.Errorf(codes.Internal, "upstream did not return object %s", s)
		}
		close(c.done)
	}
}
```

In `internal/daemon/proxy.go`, replace the `GetObjects` method with the following, and add the imports `"google.golang.org/grpc/codes"` and `"google.golang.org/grpc/status"`. Keep `"io"` only if it is still used; it is still used by UploadBlobs.
```go
// GetObjects serves verified cache hits and fetches the rest upstream, after
// checking that the caller may read the repo.
func (p *proxy) GetObjects(req *sgitv1.GetObjectsRequest, stream grpc.ServerStreamingServer[sgitv1.Object]) error {
	if len(req.Shas) > maxBatch {
		return status.Errorf(codes.InvalidArgument, "too many objects: %d > %d", len(req.Shas), maxBatch)
	}
	for _, s := range req.Shas {
		if !shaRe.MatchString(s) {
			return status.Errorf(codes.InvalidArgument, "invalid object id %q", s)
		}
	}
	ctx, err := p.d.authorize(stream.Context(), req.Repo)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(req.Shas))
	var misses []string
	for _, s := range req.Shas {
		if seen[s] {
			continue
		}
		seen[s] = true
		if o, ok := p.d.cached(s); ok {
			if err := stream.Send(toPB(s, o)); err != nil {
				return err
			}
			continue
		}
		misses = append(misses, s)
	}
	if len(misses) == 0 {
		return nil
	}
	objs, err := p.d.fetch(ctx, req.Repo, misses)
	if err != nil {
		return err
	}
	for _, s := range misses {
		if err := stream.Send(toPB(s, objs[s])); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/daemon/ && go test -race -count=5 -run Concurrent ./internal/daemon/`
Expected: `ok` both times.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---
### Task 5: The `sgd` binary

**Files:**
- Create: `internal/client/daemon.go` (socket location and owner checks), `internal/daemoncmd/daemoncmd.go`, `cmd/sgd/main.go`
- Test: `internal/daemoncmd/daemoncmd_test.go`, `internal/client/daemon_test.go`

**Interfaces:**
- Consumes: `client.Dial(addr, creds.Server{CAFile, Insecure})` (phase 2; with an empty token no per-call credentials are attached), `cache.Open`, `daemon.New/NewGRPC/Register/Close`, `testenv.Start`.
- Produces:
  - `client.DefaultSocket() string`.
  - `client.CheckOwned(path string, uid int) error`. It errors if the path is a symlink, is not owned by uid, or is a directory writable by group or others.
  - `daemoncmd.Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`. Exit codes: 0 for a clean stop, 1 for a runtime error, 2 for bad flags or config.
  - `daemoncmd.ParseSize(s string) (int64, error)`, which accepts a plain number of bytes or a `K`/`M`/`G`/`T` suffix (powers of 1024).
- Flags (with env defaults):
  - `--upstream` (`$SGD_UPSTREAM`, required)
  - `--ca` (`$SGD_CA`)
  - `--dev-insecure` (plaintext, loopback upstream only)
  - `--socket` (default `client.DefaultSocket()`)
  - `--cache-dir` (default `os.UserCacheDir()/sg/objects`)
  - `--cache-max` (default `10G`)
  - `--log-level` (default `info`)
- Startup order. The flags are parsed first, then:
  1. Dial the upstream (lazily).
  2. Open the cache.
  3. Create the socket dir with mode 0700, and check it is owned by us and not writable by group or others.
  4. Exit 1 if a daemon already answers on the socket. Otherwise remove a stale file there.
  5. Listen, then chmod the socket to 0600.
  6. Serve until SIGINT/SIGTERM or ctx is done, then stop gracefully and remove the socket.

- [ ] **Step 1: Write the failing tests**

`internal/client/daemon_test.go`:
```go
package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultSocket(t *testing.T) {
	t.Setenv("SG_SOCKET", "/x/explicit.sock")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	if got := DefaultSocket(); got != "/x/explicit.sock" {
		t.Fatalf("SG_SOCKET not honoured: %s", got)
	}
	t.Setenv("SG_SOCKET", "")
	if got := DefaultSocket(); got != "/run/user/1/sg/sgd.sock" {
		t.Fatalf("XDG_RUNTIME_DIR: %s", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := DefaultSocket(); !strings.HasSuffix(got, "/sgd.sock") || !strings.Contains(got, "sg-") {
		t.Fatalf("tmp fallback: %s", got)
	}
}

func TestCheckOwned(t *testing.T) {
	dir := t.TempDir()
	uid := os.Getuid()
	if err := CheckOwned(dir, uid); err != nil {
		t.Fatalf("own 0700 dir: %v", err)
	}
	if err := CheckOwned(dir, uid+1); err == nil {
		t.Fatal("wrong owner must be refused")
	}
	open := filepath.Join(dir, "open")
	_ = os.Mkdir(open, 0o700)
	_ = os.Chmod(open, 0o777)
	if err := CheckOwned(open, uid); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("world-writable dir: %v", err)
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(dir, link)
	if err := CheckOwned(link, uid); err == nil {
		t.Fatal("symlink must be refused")
	}
	if err := CheckOwned(filepath.Join(dir, "missing"), uid); err == nil {
		t.Fatal("missing path must be an error")
	}
}
```

`internal/daemoncmd/daemoncmd_test.go`:
```go
package daemoncmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string               { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func shortSocket(t *testing.T) string {
	dir := testenv.Must(os.MkdirTemp("", "sgdc"))
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "s.sock")
}

func start(t *testing.T, args ...string) (<-chan int, context.CancelFunc, *syncBuf) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args, out, out) }()
	t.Cleanup(cancel)
	return done, cancel, out
}

func waitInfo(t *testing.T, sock string) *sgitv1.DaemonInfoResponse {
	t.Helper()
	conn := testenv.Must(grpc.NewClient("unix://"+sock, rpc.DialOptions("", nil)...))
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		info, err := sgitv1.NewDaemonClient(conn).Info(ctx, &sgitv1.DaemonInfoRequest{})
		cancel()
		if err == nil {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("sgd never answered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func baseArgs(e *testenv.Env, sock string, t *testing.T) []string {
	return []string{"--upstream", e.Addr, "--dev-insecure", "--socket", sock, "--cache-dir", t.TempDir(), "--cache-max", "64M"}
}

func TestServeInfoModeAndCleanup(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	done, cancel, out := start(t, baseArgs(e, sock, t)...)
	info := waitInfo(t, sock)
	if info.Upstream != e.Addr || info.CacheMaxBytes != 64<<20 {
		t.Fatalf("Info = %+v", info)
	}
	fi := testenv.Must(os.Stat(sock))
	di := testenv.Must(os.Stat(filepath.Dir(sock)))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes: socket %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	second := Run(context.Background(), baseArgs(e, sock, t), &syncBuf{}, &syncBuf{})
	if second != 1 {
		t.Fatalf("second sgd on the same socket: exit %d", second)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sgd did not stop")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket must be removed on shutdown")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, baseArgs(e, sock, t)...)
	waitInfo(t, sock)
}

func TestRefusesUnsafeSocketDir(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	_ = os.Chmod(filepath.Dir(sock), 0o777)
	var errb syncBuf
	if code := Run(context.Background(), baseArgs(e, sock, t), &syncBuf{}, &errb); code != 1 || !strings.Contains(errb.String(), "writable") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestFlagsValidation(t *testing.T) {
	t.Setenv("SGD_UPSTREAM", "")
	if code := Run(context.Background(), []string{"--socket", shortSocket(t)}, &syncBuf{}, &syncBuf{}); code != 2 {
		t.Fatalf("missing --upstream: exit %d", code)
	}
	if code := Run(context.Background(), []string{"--upstream", "127.0.0.1:1", "--dev-insecure", "--cache-max", "lots", "--socket", shortSocket(t)}, &syncBuf{}, &syncBuf{}); code != 2 {
		t.Fatalf("bad --cache-max: exit %d", code)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"1024": 1024, "1K": 1 << 10, "512M": 512 << 20, "10G": 10 << 30, "2T": 2 << 40, "3g": 3 << 30}
	for in, want := range cases {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-5", "10X", "G"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) must fail", bad)
		}
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/client/ ./internal/daemoncmd/`
Expected: FAIL to compile (`undefined: DefaultSocket`, no `daemoncmd` package).

- [ ] **Step 3: Implement**

`internal/client/daemon.go`:
```go
package client

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultSocket is where sgd listens and sg looks for it: $SG_SOCKET, else
// $XDG_RUNTIME_DIR/sg/sgd.sock, else <tmp>/sg-<uid>/sgd.sock.
func DefaultSocket() string {
	if s := os.Getenv("SG_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sg", "sgd.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("sg-%d", os.Getuid()), "sgd.sock")
}

// CheckOwned verifies that path is not a symlink, is owned by uid and, if it
// is a directory, is not writable by group or others. It guards the sgd
// socket against other local users (who could otherwise collect tokens).
func CheckOwned(path string, uid int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine owner", path)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, uid)
	}
	if fi.IsDir() && fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others", path)
	}
	return nil
}
```

`internal/daemoncmd/daemoncmd.go`:
```go
// Package daemoncmd implements the sgd command line.
package daemoncmd

import (
	"context"
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

	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/daemon"
)

// ParseSize parses "1024", "512M", "10G", ... (powers of 1024).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = 1<<10, s[:n-1]
		case 'M':
			mult, s = 1<<20, s[:n-1]
		case 'G':
			mult, s = 1<<30, s[:n-1]
		case 'T':
			mult, s = 1<<40, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return v * mult, nil
}

func defaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "sg", "objects")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("sg-cache-%d", os.Getuid()))
}

// listen prepares the socket directory (0700, ours), refuses to start when a
// daemon already answers on path, removes a stale file and listens (0600).
func listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := client.CheckOwned(dir, os.Getuid()); err != nil {
		return nil, fmt.Errorf("unsafe socket directory: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
			c.Close()
			return nil, fmt.Errorf("another sgd is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close()
		return nil, err
	}
	return lis, nil
}

// Run executes sgd and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sgd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	upstream := fs.String("upstream", os.Getenv("SGD_UPSTREAM"), "sg-server address host:port (required)")
	ca := fs.String("ca", os.Getenv("SGD_CA"), "PEM file with the server's (self-signed) certificate")
	insecure := fs.Bool("dev-insecure", false, "plaintext connection to a loopback upstream")
	socket := fs.String("socket", client.DefaultSocket(), "unix socket path")
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "object cache directory")
	cacheMax := fs.String("cache-max", "10G", "cache size limit, e.g. 512M or 10G")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *upstream == "" {
		fmt.Fprintln(stderr, "sgd: --upstream (or $SGD_UPSTREAM) is required")
		return 2
	}
	max, err := ParseSize(*cacheMax)
	if err != nil {
		fmt.Fprintln(stderr, "sgd: --cache-max:", err)
		return 2
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(stderr, "sgd: --log-level:", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl}))
	conn, err := client.Dial(*upstream, creds.Server{CAFile: *ca, Insecure: *insecure})
	if err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 2
	}
	defer conn.Close()
	c, err := cache.Open(*cacheDir, max)
	if err != nil {
		fmt.Fprintln(stderr, "sgd: cache:", err)
		return 1
	}
	d, err := daemon.New(daemon.Config{Upstream: *upstream, Conn: conn, Cache: c, Log: log})
	if err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 1
	}
	defer d.Close()
	lis, err := listen(*socket)
	if err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 1
	}
	g := daemon.NewGRPC()
	d.Register(g)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	log.Info("sgd listening", "socket", *socket, "upstream", *upstream, "cache", *cacheDir, "cache_max", max)
	serveErr := g.Serve(lis)
	_ = os.Remove(*socket)
	if serveErr != nil {
		log.Error("serve", "err", serveErr)
		return 1
	}
	log.Info("sgd stopped")
	return 0
}
```

`cmd/sgd/main.go`:
```go
// Command sgd is the sgit per-user object cache daemon.
package main

import (
	"context"
	"os"

	"github.com/hermes98761234/sgit/internal/daemoncmd"
)

func main() {
	os.Exit(daemoncmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/client/ ./internal/daemoncmd/ && make build && ls dist/`
Expected: `ok` twice, and `dist/` lists `sg`, `sg-server` and `sgd`.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 6: `sg` uses `sgd` automatically

**Files:**
- Modify: `internal/client/daemon.go` (add `ErrOtherUpstream` and `DialDaemon`), `internal/cli/session.go` (the session uses sgd for Repo calls), `internal/cli/harness_test.go` (isolate tests from any real sgd)
- Test: `internal/client/daemon_test.go` (extend), `internal/cli/daemon_test.go` (new)

**Interfaces:**
- Consumes: `client.DefaultSocket`, `client.CheckOwned` (Task 5), `daemontest.Start` (Task 3), `sgitv1.NewDaemonClient`.
- Produces:
  - `client.ErrOtherUpstream`.
  - `client.DialDaemon(ctx, socket, server, token string) (*grpc.ClientConn, error)`, which returns one of:
    - `(nil, nil)` when there is no socket file;
    - `(nil, ErrOtherUpstream)` when sgd fronts another server;
    - `(nil, err)` when the socket is unusable or unsafe;
    - otherwise a connection that carries the token.
  - The `Info` probe runs on a connection *without* the token, so the token is only ever sent to a daemon that has confirmed the right upstream.
  - `session.cached *grpc.ClientConn`. `session.repo` uses sgd when one is available, `session.admin` always uses the direct connection, and `Close` closes both.

- [ ] **Step 1: Write the failing tests**

Append to `internal/client/daemon_test.go` (add the imports `"context"`, `"errors"` and `"net"`):
```go
func TestDialDaemonEdgeCases(t *testing.T) {
	ctx := context.Background()
	dir, _ := os.MkdirTemp("", "sgdd")
	t.Cleanup(func() { os.RemoveAll(dir) })
	if conn, err := DialDaemon(ctx, filepath.Join(dir, "none.sock"), "s:1", "sgt_x"); conn != nil || err != nil {
		t.Fatalf("missing socket: %v %v", conn, err)
	}
	file := filepath.Join(dir, "file.sock")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := DialDaemon(ctx, file, "s:1", "sgt_x"); err == nil {
		t.Fatal("a regular file is not a usable socket")
	}
	open := filepath.Join(dir, "open")
	_ = os.Mkdir(open, 0o700)
	l, err := net.Listen("unix", filepath.Join(open, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_ = os.Chmod(open, 0o777)
	if _, err := DialDaemon(ctx, filepath.Join(open, "s.sock"), "s:1", "sgt_x"); err == nil || errors.Is(err, ErrOtherUpstream) {
		t.Fatalf("socket in a world-writable dir must be refused: %v", err)
	}
}
```

In `internal/cli/harness_test.go`, inside `newHarness` right after `t.Setenv("SG_TOKEN", "")`, add:
```go
	t.Setenv("SG_SOCKET", filepath.Join(t.TempDir(), "no-sgd.sock")) // never use a developer's real sgd
	t.Setenv("SG_NO_DAEMON", "")
```

`internal/cli/daemon_test.go`:
```go
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/daemon"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func (h *harness) startDaemon(opts ...func(*daemon.Config)) *daemontest.Env {
	h.t.Helper()
	d := daemontest.Start(h.t, h.env.Addr, opts...)
	h.t.Setenv("SG_SOCKET", d.Socket)
	return d
}

func TestCheckoutUsesDaemonCache(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	d := h.startDaemon()
	root := t.TempDir()
	code, _, errs := h.run(root, "checkout", "web")
	if code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("checkout via sgd: exit %d %q", code, errs)
	}
	cold := d.Counter.Total()
	if cold == 0 {
		t.Fatal("first checkout must fetch through sgd")
	}
	h.ok(root, "checkout", "web", "second")
	if d.Counter.Total() != cold {
		t.Fatalf("second checkout went upstream: %d -> %d", cold, d.Counter.Total())
	}
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "via sgd\n")
	h.ok(w, "commit", "-m", "through the daemon", "--land")
	if h.env.Files("web", "main")["a.txt"] != "via sgd\n" {
		t.Fatal("land through sgd did not reach main")
	}
}

func TestUnusableDaemonWarnsAndFallsBack(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	dir := testenv.Must(os.MkdirTemp("", "sgdf"))
	t.Cleanup(func() { os.RemoveAll(dir) })
	stale := filepath.Join(dir, "s.sock")
	_ = os.WriteFile(stale, nil, 0o600)
	t.Setenv("SG_SOCKET", stale)
	code, _, errs := h.run(t.TempDir(), "checkout", "web")
	if code != 0 || !strings.Contains(errs, "warning") || !strings.Contains(errs, "connecting directly") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestDaemonForOtherServerIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	d := h.startDaemon(func(c *daemon.Config) { c.Upstream = "other.example:7419" })
	code, _, errs := h.run(t.TempDir(), "checkout", "web")
	if code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if d.Counter.Total() != 0 {
		t.Fatal("sgd fronting another server must not be used")
	}
}

func TestNoDaemonEnvDisablesSgd(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	d := h.startDaemon()
	t.Setenv("SG_NO_DAEMON", "1")
	h.ok(t.TempDir(), "checkout", "web")
	if d.Counter.Total() != 0 {
		t.Fatal("SG_NO_DAEMON=1 must bypass sgd")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/client/ ./internal/cli/ -run 'DialDaemon|Daemon'`
Expected: FAIL. `DialDaemon` is undefined, and once it compiles, the CLI tests fail because sg never uses sgd.

- [ ] **Step 3: Implement**

Append to `internal/client/daemon.go` and extend its imports with `"context"`, `"errors"`, `"time"`, `sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"`, `"github.com/hermes98761234/sgit/internal/rpc"` and `"google.golang.org/grpc"`:
```go
// ErrOtherUpstream means sgd is running but fronts a different server.
var ErrOtherUpstream = errors.New("sgd fronts a different server")

// DialDaemon connects to sgd at socket when it fronts server. It returns
// (nil, nil) when no socket exists and ErrOtherUpstream when sgd fronts
// another server; any other error means the socket exists but is unusable
// or unsafe. The Info probe carries no token: the token is only sent once
// sgd has confirmed the upstream.
func DialDaemon(ctx context.Context, socket, server, token string) (*grpc.ClientConn, error) {
	if socket == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(socket)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uid := os.Getuid()
	if err := CheckOwned(filepath.Dir(abs), uid); err != nil {
		return nil, err
	}
	if err := CheckOwned(abs, uid); err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%s is not a socket", abs)
	}
	probe, err := grpc.NewClient("unix://"+abs, rpc.DialOptions("", nil)...)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	info, err := sgitv1.NewDaemonClient(probe).Info(pctx, &sgitv1.DaemonInfoRequest{})
	cancel()
	probe.Close()
	if err != nil {
		return nil, err
	}
	if info.Upstream != server {
		return nil, ErrOtherUpstream
	}
	return grpc.NewClient("unix://"+abs, rpc.DialOptions(token, nil)...)
}
```

In `internal/cli/session.go`:
1. Add the field `cached *grpc.ClientConn // sgd connection; nil when not used` to `session`.
2. Replace `Close` with:
```go
func (s *session) Close() {
	s.conn.Close()
	if s.cached != nil {
		s.cached.Close()
	}
}
```
3. In `connect`, replace the final `return &session{...}` with the following, adding the imports `"context"`, `"fmt"` and `"os"`:
```go
	repoConn := grpc.ClientConnInterface(conn)
	var cached *grpc.ClientConn
	if os.Getenv("SG_NO_DAEMON") == "" {
		sock := client.DefaultSocket()
		dc, err := client.DialDaemon(context.Background(), sock, name, cred.Token)
		switch {
		case err == nil && dc != nil:
			cached, repoConn = dc, dc
		case err != nil && !errors.Is(err, client.ErrOtherUpstream):
			fmt.Fprintf(a.io.Stderr, "sg: warning: sgd at %s is not usable (%v); connecting directly\n", sock, err)
		}
	}
	return &session{
		conn: conn, cached: cached, repo: sgitv1.NewRepoClient(repoConn), admin: sgitv1.NewAdminClient(conn),
		server: name, name: repo, wc: w,
	}, nil
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/client/ ./internal/cli/`
Expected: `ok` twice. Every existing CLI test still passes, because the harness points `SG_SOCKET` at a non-existent path.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 7: Docs: README, systemd user unit, spec note

**Files:**
- Modify: `README.md`, `docs/superpowers/specs/2026-10-14-sgit-design.md`
- Create: `contrib/sgd.service`

**Interfaces:** none (docs only). Verification: `make build`, plus the full test suite still green.

- [ ] **Step 1: Write the docs**

`contrib/sgd.service`:
```ini
# systemd user unit for sgd. Install:
#   mkdir -p ~/.config/systemd/user && cp contrib/sgd.service ~/.config/systemd/user/
#   edit --upstream / --ca below, then: systemctl --user enable --now sgd
[Unit]
Description=sgit object cache daemon (sgd)

[Service]
ExecStart=%h/.local/bin/sgd --upstream sg.example.com:7419 --ca %h/.config/sg/server-ca.pem
Restart=on-failure

[Install]
WantedBy=default.target
```

In `README.md`:
- Replace the `> **Status:** ...` line with:
```markdown
> **Status:** phases 1–3 (server and Docker, `sg` CLI, `sgd` cache daemon) are implemented. GitHub mirroring and admin commands are next. See the [design spec](docs/superpowers/specs/2026-10-14-sgit-design.md).
```
- Insert this section right after the "Use the CLI" section:
````markdown
## Run the cache daemon (`sgd`)

`sgd` keeps a per-user disk cache of git objects in front of one server, so several working copies and agents on the same machine share fetched objects.

```bash
sgd --upstream sg.example.com:7419 --ca cert.pem    # or run it as a systemd user service: contrib/sgd.service
```

- `sg` uses `sgd` automatically when it fronts the server you are logged in to. Otherwise `sg` connects directly. If the socket exists but sgd does not answer, `sg` prints one warning and continues.
- To bypass the daemon, set `SG_NO_DAEMON=1`.
- **Socket:** `$XDG_RUNTIME_DIR/sg/sgd.sock`, mode 0600, one daemon per user. Override it with `--socket` or `$SG_SOCKET`. Both `sg` and `sgd` refuse a socket that you do not own.
- **Cache:** stored in `~/.cache/sg/objects`, with a default size limit of 10 GiB (`--cache-max 10G`) and least-recently-used eviction. Every object is checked against its hash when stored and again each time it is served.
- **Security:**
  - Every request still runs with your own token.
  - Before serving cached objects for a repo, sgd re-checks your read access at least every 60 seconds.
  - Objects cached for one repo can be served for another when requested by their hash. This is acceptable within a trusted team, as spec §7 describes.
````

In `docs/superpowers/specs/2026-10-14-sgit-design.md`, insert the following immediately before the line that starts with `## 8.`:
```markdown
### Phase 3 implementation notes

- **Socket.** sgd runs as one daemon per user. The socket is mode 0600, inside a 0700 directory owned by the user, and sg checks that ownership before connecting. This replaces the 0660 system socket described above.
- **`Daemon/Info`.** This RPC reports the upstream sgd fronts. sg calls it without a token and uses the daemon only when the upstream matches its own server.
- **Which RPCs use the cache.** `HasObjects` and `ResolveRev` always go to the server. Only `GetObjects` is served from the cache: every object is checked on the way in and on every read, and in-flight fetches are deduplicated per (repo, sha).
```

- [ ] **Step 2: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./... && make build && ls dist/`
Expected: clean, all `ok`, and `dist/` lists `sg`, `sg-server` and `sgd`.
