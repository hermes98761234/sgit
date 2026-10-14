# sgit Phase 2: `sg` CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `sg`, the thin client. Its working copy is files plus a `.sg/` folder, with no `.git`. It logs in to `sg-server`, checks out a repo (fully or only some directories), shows status and diffs offline, commits to drafts, lands with `--land`, rebases onto main with `sync`, and returns conflicts as conflict markers with exit code 3.

**Architecture:** The CLI is a set of small packages on top of the phase-1 gRPC API:
- `internal/creds`: saved logins.
- `internal/client`: dialing the server (direct connection; phase 3 adds the `sgd` socket).
- `internal/wc`: the working copy, covering the `.sg/` metadata, the stat-cached index, status and filesystem writes.
- `internal/fetch`: walking trees and fetching blobs, using the Scion-style "ask only for what you need" protocol.
- `internal/cli`: the commands.
- `cmd/sg`: the main binary.

The client never runs git. Every 3-way merge happens on the server through the `MergeFile` RPC.

**Tech Stack:**
- Go (module already at `go 1.27.2`) and gRPC (`gen/sgit/v1`).
- `github.com/BurntSushi/toml` for `.sg/config` and the credentials file.
- `github.com/sabhiram/go-gitignore` for root `.gitignore` support.
- `github.com/pmezard/go-difflib` for unified diffs.
- `github.com/klauspost/compress/zstd`, already a dependency.

**Spec:** `docs/superpowers/specs/2026-10-14-sgit-design.md`. Phase 2 is §12 item 2. The CLI is defined in §4, the working copy in §3, the fetch protocol in §5 and the exit codes in §10.

## Global Constraints

- **Shell setup.** Every shell command assumes:
  ```bash
  cd /home/vkopytsia/Projects/new/sgit
  export PATH=$HOME/.local/go/bin:$HOME/.local/protoc/bin:$HOME/go/bin:$PATH
  ```
- **No git in the project directory.** The project dir is not a git repo. Never run `git init`, `git add`, `git commit` or `git push` there. The controller publishes with the gh-only push helper after review. Implementers do not push.
- **The client never runs the `git` binary.** It hashes blobs and parses trees with `internal/objfmt`, and does merges through the `MergeFile` RPC.
- **Real names.** Never write anyone's real name. Test users are `admin` (from testenv) and `bob`.
- **Exit codes** (spec §4/§10):
  - `0` ok
  - `1` generic error, including usage errors
  - `2` auth/permission: gRPC `Unauthenticated`/`PermissionDenied`, or not logged in
  - `3` conflict: gRPC `Aborted` carrying a `*sgitv1.ConflictDetail`, unresolved conflict markers, or conflicts left by `sync`
  - `4` CAS retries exhausted: gRPC `ResourceExhausted`
- **Working copy layout** (spec §3). Every file is written atomically (temp file plus rename).
  - `.sg/config`: TOML with keys `server`, `repo`, `sparse`.
  - `.sg/base`: commit SHA plus newline. It is written LAST and marks a complete working copy.
  - `.sg/index`: JSON lines `{"path","mode","sha","size","mtime_ns"}`, sorted by path. `size == -1` means "stat unknown, always re-hash".
  - `.sg/draft`: the open draft id. The file is absent when there is no draft.
  - `.sg/conflicts`: one path per line. The file is absent when empty.
- **Credentials** live in `$SG_CONFIG_DIR/credentials`. The default is `os.UserConfigDir()/sg/credentials`, mode `0600`, directory `0700`. The file is TOML, `{default = "host:port", [servers."host:port"] token/ca_file/insecure}`. `SG_TOKEN` supplies the default token for `sg login`.
- **Plaintext connections** happen only with `--dev-insecure`, and only to a loopback host (`localhost`, `127.0.0.0/8`, `::1`). Otherwise the client uses TLS 1.2 or later, with system roots or the pinned `--ca` PEM file.
- **Modes.**
  - Regular file: `100644`, or `100755` if any exec bit is set.
  - Symlink: `120000`. The blob is the link target.
  - Submodules (`160000`) are skipped on checkout.
  - Other file types are ignored.
- **Ignore rules.** Untracked files matching the root `.gitignore` are never reported as added. The `.sg/` directory is always ignored. Tracked files are never ignored.
- **Fetch batching.** At most 1000 SHAs per `GetObjects` or `HasObjects` call. Files are materialized in groups of 500, and uploads are flushed every 32 MiB.
- **Phase-2 scope.** `sg` dials `sg-server` directly; the `sgd` daemon is phase 3. The admin commands (`sg repo|token|acl`) and mirroring are phase 4 and are not implemented here.
- **Test helper.** `testenv.Must` is called as `testenv.Must(f())`, with no `t`. It panics, so never call it inside goroutines.
- **Go style.** Run `gofmt -l .` (prints nothing), `go vet ./...`, `staticcheck ./...` and `go test -race -count=1 ./...`. All must be clean before a task is done.

### Decisions beyond the spec (for review)
- `sg login` adds `--ca FILE` (pins a self-signed server certificate) and `--dev-insecure`. If neither `--token` nor `$SG_TOKEN` is given, the token is read from stdin.
- There is no `sg add`. As in jj, every untracked, non-ignored file inside the sparse set is part of the next commit. Only the root `.gitignore` is honoured.
- `sg checkout` resumes only an interrupted checkout (`.sg/` present without `.sg/base`); otherwise the target must be absent or empty (phase-2 final review ruling).
- `sg sync` resolves one-sided conflicts as follows:
  - A file deleted locally but changed upstream keeps the local deletion, with a warning.
  - A file changed locally but deleted upstream keeps the local file (it becomes "added"), with a warning.
  - Only content conflicts write markers and are listed in `.sg/conflicts`.
- A land conflict reports a `ConflictDetail` with the new `main`. The client then runs `sync` onto that `main`, writing conflict markers. The draft id stays in `.sg/draft`, so a second `sg commit --land` updates the same draft.
- A stale `.sg/draft` (the draft was landed, abandoned or deleted elsewhere) is dropped silently, and the commit starts a new draft.
- `sg sparse rm` refuses to remove the last sparse path, because an empty set means a full checkout. `sg sparse add` refuses on a full checkout.
- Extra flags: `sg log --rev`, `sg drafts --status`, and `--repo`/`--server` on the read commands.

## Review Focus

These are the five conditions the spec implies that most likely hurt a user if broken. Each is pinned by a test in the task named.

1. **Edit within the same mtime tick as a checkout or sync.** A file overwritten with the same size and its mtime restored must still show as modified, which is the racy-stat problem. Pinned in Task 3 (`TestStatusDetectsRacyEdit`).
2. **Re-running an interrupted checkout** into the same directory must succeed and end with correct content. Pinned in Task 6 (`TestCheckoutResumes`).
3. **Running commands from a subdirectory** of the working copy: `status`, `diff <relative path>`, `log <path>`, `blame <path>` must resolve paths against the cwd, not the root. Pinned in Task 6 (`TestDiffFromSubdirectory`) and Task 10 (`TestLogBlameCatFromSubdirectory`).
4. **Exec bits and symlinks must survive** commit and re-checkout, and a local `chmod +x` must show as a change. Pinned in Task 6 (`TestStatusAndDiffShowModeChange`) and Task 7 (`TestCommitSymlinkAndExecRoundTrip`).
5. **A `.sg/draft` that was landed or abandoned elsewhere** must not wedge `sg commit`; it starts a new draft. Pinned in Task 7 (`TestCommitAfterDraftAbandonedElsewhere`).

---

## File Structure

```
cmd/sg/main.go                      thin main (signals, os.Exit)
internal/fsutil/fsutil.go           WriteAtomic
internal/creds/creds.go             credentials file (TOML, 0600)
internal/client/client.go           Dial(addr, creds.Server): TLS/CA/loopback-plaintext
internal/wc/wc.go                   Config, Entry, WC, Create, Open, Save, SortedPaths
internal/wc/sparse.go               NormalizeSparse, InSparse, TreeNeeded
internal/wc/fs.go                   ModeOf, ReadLocal, WriteLocal, RemoveLocal, StatEntry
internal/wc/paths.go                (*WC).RepoPath(cwd, arg)
internal/wc/status.go               Kind, Change, (*WC).Status (+ .gitignore)
internal/fetch/fetch.go             File, Tree, Blobs (batched GetObjects, zstd, integrity)
internal/cli/cli.go                 IO, Run, exit codes, command table, usage, emit
internal/cli/flags.go               stringList, parse (interleaved flags), app.newFlags, app.abs
internal/cli/session.go             session, app.connect, short
internal/cli/login.go               sg login
internal/cli/checkout.go            sg checkout, materialize
internal/cli/status.go              sg status
internal/cli/diff.go                sg diff, writePatch
internal/cli/commit.go              sg commit, uploadChanges, uploadMissing, saveDraft, checkConflicts, isStaleDraft
internal/cli/drafts.go              sg drafts, sg show, sg abandon
internal/cli/sync.go                sg sync, syncTo, writeFromTree
internal/cli/land.go                sg land, app.finishLand, app.commitAndLand
internal/cli/history.go             sg log, sg blame, sg cat, app.repoPath
internal/cli/sparse.go              sg sparse list|add|rm
internal/cli/harness_test.go        shared CLI test harness
```

---

### Task 1: fsutil, credentials, dialing

**Files:**
- Create: `internal/fsutil/fsutil.go`, `internal/creds/creds.go`, `internal/client/client.go`
- Test: `internal/fsutil/fsutil_test.go`, `internal/creds/creds_test.go`, `internal/client/client_test.go`

**Interfaces:**
- Consumes:
  - `rpc.DialOptions(token string, tlsCfg *tls.Config) []grpc.DialOption`. With `tlsCfg == nil` it dials plaintext and the token is still sent.
  - `testenv.Start`, `testenv.Must`, `tlsutil.GenerateSelfSigned(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error)`.
- Produces:
  - `fsutil.WriteAtomic(path string, data []byte, perm os.FileMode) error`
  - `creds.Server{Token, CAFile string; Insecure bool}` and `creds.File{Default string; Servers map[string]Server}`
  - `creds.Dir() (string, error)`, `creds.Load() (File, error)`, `creds.Save(File) error`
  - `(creds.File).Lookup(server string) (name string, s Server, ok bool)`
  - `client.Dial(addr string, cred creds.Server) (*grpc.ClientConn, error)`

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/BurntSushi/toml@latest github.com/sabhiram/go-gitignore@latest github.com/pmezard/go-difflib@latest
```
(`go mod tidy` will drop the unused ones until later tasks import them, so run `go mod tidy` only at the end of each task.)

- [ ] **Step 2: Write the failing tests**

`internal/fsutil/fsutil_test.go`:
```go
package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := WriteAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(data) != "two" || fi.Mode().Perm() != 0o644 {
		t.Fatalf("data=%q mode=%v", data, fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}
```

`internal/creds/creds_test.go`:
```go
package creds

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("SG_CONFIG_DIR", t.TempDir())
	f, err := Load()
	if err != nil || f.Default != "" || len(f.Servers) != 0 || f.Servers == nil {
		t.Fatalf("Load = %+v, %v", f, err)
	}
	if _, _, ok := f.Lookup(""); ok {
		t.Fatal("Lookup on empty file must fail")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "sg")
	t.Setenv("SG_CONFIG_DIR", dir)
	f := File{Default: "127.0.0.1:7419", Servers: map[string]Server{
		"127.0.0.1:7419":    {Token: "sgt_a", Insecure: true},
		"sg.example.com:443": {Token: "sgt_b", CAFile: "/tmp/ca.pem"},
	}}
	if err := Save(f); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode: %v %v", fi, err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	name, s, ok := got.Lookup("")
	if !ok || name != "127.0.0.1:7419" || s.Token != "sgt_a" || !s.Insecure {
		t.Fatalf("default lookup = %q %+v %v", name, s, ok)
	}
	name, s, ok = got.Lookup("sg.example.com:443")
	if !ok || name != "sg.example.com:443" || s.CAFile != "/tmp/ca.pem" || s.Insecure {
		t.Fatalf("named lookup = %q %+v %v", name, s, ok)
	}
	if _, _, ok := got.Lookup("other:1"); ok {
		t.Fatal("unknown server must not be found")
	}
}
```

`internal/client/client_test.go`:
```go
package client

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/hermes98761234/sgit/internal/tlsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestDialInsecureLoopback(t *testing.T) {
	e := testenv.Start(t)
	conn := testenv.Must(Dial(e.Addr, creds.Server{Token: e.AdminToken, Insecure: true}))
	defer conn.Close()
	who := testenv.Must(sgitv1.NewAdminClient(conn).WhoAmI(context.Background(), &sgitv1.WhoAmIRequest{}))
	if who.User != "admin" {
		t.Fatalf("WhoAmI = %+v", who)
	}
}

func TestDialInsecureRefusesRemote(t *testing.T) {
	for _, addr := range []string{"10.1.2.3:7419", "sg.example.com:7419"} {
		if _, err := Dial(addr, creds.Server{Token: "sgt_x", Insecure: true}); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("Dial(%s, insecure) = %v, want loopback error", addr, err)
		}
	}
	if _, err := Dial("no-port", creds.Server{}); err == nil {
		t.Error("address without port must fail")
	}
}

func startTLSHealth(t *testing.T, certPEM, keyPEM []byte) string {
	t.Helper()
	pair := testenv.Must(tls.X509KeyPair(certPEM, keyPEM))
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})))
	healthpb.RegisterHealthServer(g, health.NewServer())
	lis := testenv.Must(net.Listen("tcp", "127.0.0.1:0"))
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	return lis.Addr().String()
}

func TestDialTLSPinnedCA(t *testing.T) {
	certPEM, keyPEM := mustCert(t)
	addr := startTLSHealth(t, certPEM, keyPEM)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := testenv.Must(Dial(addr, creds.Server{CAFile: ca}))
	defer conn.Close()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("pinned CA: %v %v", resp, err)
	}

	otherPEM, _ := mustCert(t)
	wrong := filepath.Join(t.TempDir(), "wrong.pem")
	_ = os.WriteFile(wrong, otherPEM, 0o644)
	conn2 := testenv.Must(Dial(addr, creds.Server{CAFile: wrong}))
	defer conn2.Close()
	if _, err := healthpb.NewHealthClient(conn2).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("wrong CA must fail the TLS handshake")
	}
	if _, err := Dial(addr, creds.Server{CAFile: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Fatal("missing CA file must fail")
	}
}

func mustCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	c, k, err := tlsutil.GenerateSelfSigned([]string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c, k
}
```

- [ ] **Step 3: Run the tests to confirm they fail**

Run: `go test ./internal/fsutil/ ./internal/creds/ ./internal/client/`
Expected: FAIL to compile (`undefined: WriteAtomic`, `undefined: Load`, `undefined: Dial`).

- [ ] **Step 4: Implement**

`internal/fsutil/fsutil.go`:
```go
// Package fsutil holds small filesystem helpers shared by the client packages.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path via a temp file in the same directory and a
// rename, so readers never observe a partial file.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return nil
}
```

`internal/creds/creds.go`:
```go
// Package creds stores sg login credentials in $SG_CONFIG_DIR/credentials
// (default: <user config dir>/sg/credentials) as TOML readable only by the user.
package creds

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/hermes98761234/sgit/internal/fsutil"
)

// Server is how to reach and authenticate to one sg-server.
type Server struct {
	Token    string `toml:"token"`
	CAFile   string `toml:"ca_file,omitempty"`
	Insecure bool   `toml:"insecure,omitempty"` // plaintext; loopback only
}

// File is the whole credentials file.
type File struct {
	Default string            `toml:"default"`
	Servers map[string]Server `toml:"servers"`
}

// Dir returns the sg configuration directory.
func Dir() (string, error) {
	if d := os.Getenv("SG_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sg"), nil
}

func filePath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "credentials"), nil
}

// Load reads the credentials file; a missing file yields an empty File.
func Load() (File, error) {
	p, err := filePath()
	if err != nil {
		return File{}, err
	}
	f := File{}
	if _, err := toml.DecodeFile(p, &f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return File{}, fmt.Errorf("read %s: %w", p, err)
	}
	if f.Servers == nil {
		f.Servers = map[string]Server{}
	}
	return f, nil
}

// Save writes the credentials file atomically with mode 0600.
func Save(f File) error {
	p, err := filePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return err
	}
	return fsutil.WriteAtomic(p, buf.Bytes(), 0o600)
}

// Lookup returns the entry for server, or for the default server when server is "".
func (f File) Lookup(server string) (string, Server, bool) {
	if server == "" {
		server = f.Default
	}
	s, ok := f.Servers[server]
	return server, s, ok && server != ""
}
```

`internal/client/client.go`:
```go
// Package client dials sg-server for the CLI. Phase 3 adds the sgd socket.
package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/rpc"
	"google.golang.org/grpc"
)

// Dial connects to addr ("host:port"). Plaintext (cred.Insecure) is only
// allowed to loopback hosts; otherwise TLS is used with the system roots or
// the pinned CA file.
func Dial(addr string, cred creds.Server) (*grpc.ClientConn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("server address %q must be host:port", addr)
	}
	var tlsCfg *tls.Config
	if cred.Insecure {
		if !isLoopback(host) {
			return nil, fmt.Errorf("insecure (plaintext) connections are only allowed to loopback addresses, not %q", host)
		}
	} else {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		if cred.CAFile != "" {
			pem, err := os.ReadFile(cred.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read CA file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("CA file %s contains no certificates", cred.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
	}
	return grpc.NewClient(addr, rpc.DialOptions(cred.Token, tlsCfg)...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
```

- [ ] **Step 5: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race -count=1 ./internal/fsutil/ ./internal/creds/ ./internal/client/`
Expected: three `ok` lines.

- [ ] **Step 6: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean, with all packages `ok`. Do not push; the controller publishes.

---

### Task 2: Working copy core (metadata, sparse, filesystem, paths)

**Files:**
- Create: `internal/wc/wc.go`, `internal/wc/sparse.go`, `internal/wc/fs.go`, `internal/wc/paths.go`
- Test: `internal/wc/wc_test.go`, `internal/wc/fs_test.go`

**Interfaces:**
- Consumes: `fsutil.WriteAtomic`.
- Produces:
  - Types and errors: `wc.ErrNotWorkingCopy`, `wc.Config{Server, Repo string; Sparse []string}`, `wc.Entry{Path, Mode, SHA string; Size, MtimeNs int64}`, `wc.WC{Root string; Config Config; Base, Draft string; Index map[string]Entry; Conflicts []string}`
  - Lifecycle: `wc.Create(root string, cfg Config) (*WC, error)` creates `.sg/`, and refuses when `.sg/base` exists. `wc.Open(start string) (*WC, error)` walks up from `start`. `(*WC).Save() error`.
  - Index helpers: `(*WC).SortedPaths() []string`, `(*WC).Abs(p string) string`
  - Sparse helpers: `wc.NormalizeSparse([]string) ([]string, error)`, `wc.InSparse(sparse []string, p string) bool`, `wc.TreeNeeded(sparse []string, dir string) bool`
  - Filesystem helpers:
    - `wc.ModeOf(fs.FileInfo) (string, bool)`
    - `wc.ReadLocal(root, p string) ([]byte, string, error)`, which returns data and mode
    - `wc.WriteLocal(root, p string, data []byte, mode string) error`
    - `wc.RemoveLocal(root, p string) error`
    - `wc.StatEntry(root, p, mode, sha string) (Entry, error)`
  - Paths: `(*WC).RepoPath(cwd, arg string) (string, error)`

- [ ] **Step 1: Write the failing tests**

`internal/wc/wc_test.go`:
```go
package wc

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCreateSaveOpen(t *testing.T) {
	root := t.TempDir()
	w, err := Create(root, Config{Server: "127.0.0.1:7419", Repo: "web", Sparse: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrNotWorkingCopy) {
		t.Fatalf("open before first save: want ErrNotWorkingCopy, got %v", err)
	}
	w.Base = "1111111111111111111111111111111111111111"
	w.Draft = "abcdefgh"
	w.Conflicts = []string{"src/b.go", "src/a.go"}
	w.Index["src/a.go"] = Entry{Path: "src/a.go", Mode: "100644", SHA: "2222222222222222222222222222222222222222", Size: 3, MtimeNs: 42}
	w.Index["src/b.go"] = Entry{Path: "src/b.go", Mode: "100755", SHA: "3333333333333333333333333333333333333333", Size: -1}
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Open(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != w.Root || got.Base != w.Base || got.Draft != "abcdefgh" || !reflect.DeepEqual(got.Config, w.Config) ||
		!reflect.DeepEqual(got.Index, w.Index) || !reflect.DeepEqual(got.Conflicts, []string{"src/a.go", "src/b.go"}) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, w)
	}
	got.Draft, got.Conflicts = "", nil
	if err := got.Save(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"draft", "conflicts"} {
		if _, err := os.Stat(filepath.Join(root, ".sg", f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf(".sg/%s must be removed when empty", f)
		}
	}
	again, _ := Open(root)
	if again.Draft != "" || len(again.Conflicts) != 0 {
		t.Fatalf("cleared fields came back: %+v", again)
	}
	if _, err := Create(root, Config{}); err == nil {
		t.Fatal("Create over a complete working copy must fail")
	}
	if got.SortedPaths()[0] != "src/a.go" {
		t.Fatalf("SortedPaths = %v", got.SortedPaths())
	}
}

func TestOpenOutside(t *testing.T) {
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotWorkingCopy) {
		t.Fatalf("want ErrNotWorkingCopy, got %v", err)
	}
}

func TestSparseHelpers(t *testing.T) {
	got, err := NormalizeSparse([]string{"src/", "./docs", "src/sub", "a-b", "a", "a/b"})
	if err != nil || !reflect.DeepEqual(got, []string{"a", "a-b", "docs", "src"}) {
		t.Fatalf("NormalizeSparse = %v, %v", got, err)
	}
	for _, bad := range []string{"", ".", "/abs", "..", "../x"} {
		if _, err := NormalizeSparse([]string{bad}); err == nil {
			t.Errorf("NormalizeSparse(%q) must fail", bad)
		}
	}
	sp := []string{"src", "docs/api"}
	cases := map[string]bool{"src/a.go": true, "src": true, "srcx/a": false, "docs/api/x.md": true, "docs/readme.md": false}
	for p, want := range cases {
		if InSparse(sp, p) != want {
			t.Errorf("InSparse(%q) != %v", p, want)
		}
	}
	if !InSparse(nil, "anything") {
		t.Error("empty sparse set means everything")
	}
	trees := map[string]bool{"": true, "docs": true, "docs/api": true, "docs/api/v1": true, "docs/other": false, "lib": false}
	for d, want := range trees {
		if TreeNeeded(sp, d) != want {
			t.Errorf("TreeNeeded(%q) != %v", d, want)
		}
	}
}

func TestRepoPath(t *testing.T) {
	root := t.TempDir()
	w, _ := Create(root, Config{Repo: "web"})
	w.Base = "1111111111111111111111111111111111111111"
	_ = w.Save()
	w, _ = Open(root)
	sub := filepath.Join(w.Root, "src")
	_ = os.MkdirAll(sub, 0o755)
	cases := []struct{ cwd, arg, want string }{
		{w.Root, "a.txt", "a.txt"},
		{sub, "x.go", "src/x.go"},
		{sub, "../a.txt", "a.txt"},
		{sub, ".", "src"},
		{w.Root, ".", ""},
		{sub, filepath.Join(w.Root, "b.txt"), "b.txt"},
	}
	for _, c := range cases {
		if got, err := w.RepoPath(c.cwd, c.arg); err != nil || got != c.want {
			t.Errorf("RepoPath(%q, %q) = %q, %v; want %q", c.cwd, c.arg, got, err, c.want)
		}
	}
	if _, err := w.RepoPath(sub, "../../outside"); err == nil {
		t.Error("path outside the working copy must fail")
	}
}
```

`internal/wc/fs_test.go`:
```go
package wc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadLocalModes(t *testing.T) {
	root := t.TempDir()
	if err := WriteLocal(root, "bin/run.sh", []byte("#!/bin/sh\n"), "100755"); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(root, "link", []byte("bin/run.sh"), "120000"); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(root, "a.txt", []byte("x"), "100644"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(root, "bin", "run.sh"))
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("exec mode = %v", fi.Mode().Perm())
	}
	data, mode, err := ReadLocal(root, "link")
	if err != nil || mode != "120000" || string(data) != "bin/run.sh" {
		t.Fatalf("symlink = %q %s %v", data, mode, err)
	}
	if _, mode, _ := ReadLocal(root, "bin/run.sh"); mode != "100755" {
		t.Fatalf("exec read mode = %s", mode)
	}
	if _, mode, _ := ReadLocal(root, "a.txt"); mode != "100644" {
		t.Fatalf("plain read mode = %s", mode)
	}
	// Overwrite a symlink with a regular file.
	if err := WriteLocal(root, "link", []byte("now a file"), "100644"); err != nil {
		t.Fatal(err)
	}
	if data, mode, _ := ReadLocal(root, "link"); mode != "100644" || string(data) != "now a file" {
		t.Fatalf("replaced link = %q %s", data, mode)
	}
}

func TestWriteLocalReplacesFileWithDirectory(t *testing.T) {
	root := t.TempDir()
	_ = WriteLocal(root, "d", []byte("file"), "100644")
	if err := WriteLocal(root, "d/inner.txt", []byte("in"), "100644"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "d")); !fi.IsDir() {
		t.Fatal("d must now be a directory")
	}
	if err := WriteLocal(root, "d", []byte("file again"), "100644"); err == nil {
		t.Fatal("a non-empty directory in the way must be an error")
	}
}

func TestRemoveLocalPrunesEmptyDirs(t *testing.T) {
	root := t.TempDir()
	_ = WriteLocal(root, "x/y/z.txt", []byte("z"), "100644")
	_ = WriteLocal(root, "x/keep.txt", []byte("k"), "100644")
	if err := RemoveLocal(root, "x/y/z.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "x", "y")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty x/y must be pruned")
	}
	if _, err := os.Stat(filepath.Join(root, "x", "keep.txt")); err != nil {
		t.Fatal("non-empty x must stay")
	}
	if err := RemoveLocal(root, "missing.txt"); err != nil {
		t.Fatalf("removing a missing file is fine: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root must never be removed")
	}
}

func TestStatEntry(t *testing.T) {
	root := t.TempDir()
	_ = WriteLocal(root, "a.txt", []byte("abc"), "100644")
	e, err := StatEntry(root, "a.txt", "100644", "sha")
	if err != nil || e.Size != 3 || e.MtimeNs == 0 || e.Path != "a.txt" || e.SHA != "sha" {
		t.Fatalf("StatEntry = %+v, %v", e, err)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/wc/`
Expected: FAIL to compile (`undefined: Create`, …).

- [ ] **Step 3: Implement**

`internal/wc/wc.go`:
```go
// Package wc manages an sg working copy: plain files plus the .sg/ metadata
// directory (config, base, index, draft, conflicts). It never runs git.
package wc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/hermes98761234/sgit/internal/fsutil"
)

const metaDir = ".sg"

// ErrNotWorkingCopy means no complete .sg/ directory was found.
var ErrNotWorkingCopy = errors.New("not inside an sg working copy (no .sg directory found)")

// Config is .sg/config.
type Config struct {
	Server string   `toml:"server"`
	Repo   string   `toml:"repo"`
	Sparse []string `toml:"sparse,omitempty"`
}

// Entry is the index record of a tracked file. SHA and Mode describe the file
// as of Base; Size and MtimeNs cache the stat of the working file at the time
// it was known to match (Size -1 = unknown, always re-hash).
type Entry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	SHA     string `json:"sha"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
}

// WC is an open working copy.
type WC struct {
	Root      string
	Config    Config
	Base      string
	Draft     string
	Index     map[string]Entry
	Conflicts []string
}

func realDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Create prepares a new working copy rooted at root (which must exist). It
// refuses when root already holds a complete working copy. Nothing is final
// until Save writes .sg/base.
func Create(root string, cfg Config) (*WC, error) {
	r, err := realDir(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(r, metaDir, "base")); err == nil {
		return nil, fmt.Errorf("%s is already an sg working copy", r)
	}
	if err := os.MkdirAll(filepath.Join(r, metaDir), 0o755); err != nil {
		return nil, err
	}
	return &WC{Root: r, Config: cfg, Index: map[string]Entry{}}, nil
}

// Open finds the working copy containing start by walking up the directories.
func Open(start string) (*WC, error) {
	dir, err := realDir(start)
	if err != nil {
		return nil, err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, metaDir, "base")); err == nil && !fi.IsDir() {
			return load(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotWorkingCopy
		}
		dir = parent
	}
}

func readOptional(p string) (string, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

func load(root string) (*WC, error) {
	m := filepath.Join(root, metaDir)
	w := &WC{Root: root, Index: map[string]Entry{}}
	if _, err := toml.DecodeFile(filepath.Join(m, "config"), &w.Config); err != nil {
		return nil, fmt.Errorf("read .sg/config: %w", err)
	}
	base, err := os.ReadFile(filepath.Join(m, "base"))
	if err != nil {
		return nil, err
	}
	w.Base = strings.TrimSpace(string(base))
	draft, err := readOptional(filepath.Join(m, "draft"))
	if err != nil {
		return nil, err
	}
	w.Draft = strings.TrimSpace(draft)
	conflicts, err := readOptional(filepath.Join(m, "conflicts"))
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(conflicts, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			w.Conflicts = append(w.Conflicts, l)
		}
	}
	f, err := os.Open(filepath.Join(m, "index"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		defer f.Close()
		dec := json.NewDecoder(f)
		for {
			var e Entry
			if err := dec.Decode(&e); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("read .sg/index: %w", err)
			}
			w.Index[e.Path] = e
		}
	}
	return w, nil
}

// SortedPaths returns the tracked paths in order.
func (w *WC) SortedPaths() []string {
	out := make([]string, 0, len(w.Index))
	for p := range w.Index {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Abs returns the absolute filesystem path of repo path p.
func (w *WC) Abs(p string) string { return filepath.Join(w.Root, filepath.FromSlash(p)) }

func (w *WC) writeOrRemove(name string, data []byte) error {
	p := filepath.Join(w.Root, metaDir, name)
	if len(data) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return fsutil.WriteAtomic(p, data, 0o644)
}

// Save writes all metadata. .sg/base is written last: its presence marks a
// complete working copy.
func (w *WC) Save() error {
	m := filepath.Join(w.Root, metaDir)
	if err := os.MkdirAll(m, 0o755); err != nil {
		return err
	}
	var cfg bytes.Buffer
	if err := toml.NewEncoder(&cfg).Encode(w.Config); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(filepath.Join(m, "config"), cfg.Bytes(), 0o644); err != nil {
		return err
	}
	var idx bytes.Buffer
	enc := json.NewEncoder(&idx)
	for _, p := range w.SortedPaths() {
		if err := enc.Encode(w.Index[p]); err != nil {
			return err
		}
	}
	if err := fsutil.WriteAtomic(filepath.Join(m, "index"), idx.Bytes(), 0o644); err != nil {
		return err
	}
	var draft []byte
	if w.Draft != "" {
		draft = []byte(w.Draft + "\n")
	}
	if err := w.writeOrRemove("draft", draft); err != nil {
		return err
	}
	sort.Strings(w.Conflicts)
	var conflicts []byte
	if len(w.Conflicts) > 0 {
		conflicts = []byte(strings.Join(w.Conflicts, "\n") + "\n")
	}
	if err := w.writeOrRemove("conflicts", conflicts); err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(m, "base"), []byte(w.Base+"\n"), 0o644)
}
```

`internal/wc/sparse.go`:
```go
package wc

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// NormalizeSparse cleans sparse paths (slash-separated, relative, no "..")
// and drops entries already covered by another entry.
func NormalizeSparse(paths []string) ([]string, error) {
	var clean []string
	for _, p := range paths {
		c := path.Clean(strings.TrimSuffix(filepath.ToSlash(p), "/"))
		if p == "" || c == "." || strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") {
			return nil, fmt.Errorf("invalid sparse path %q", p)
		}
		clean = append(clean, c)
	}
	sort.Strings(clean)
	var out []string
	for _, c := range clean {
		if len(out) > 0 && InSparse(out, c) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// InSparse reports whether file path p is inside the sparse set; an empty set
// means the whole repository.
func InSparse(sparse []string, p string) bool {
	if len(sparse) == 0 {
		return true
	}
	for _, s := range sparse {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// TreeNeeded reports whether directory dir ("" = root) can contain files of
// the sparse set, i.e. whether a fetch must descend into it.
func TreeNeeded(sparse []string, dir string) bool {
	if len(sparse) == 0 || dir == "" {
		return true
	}
	for _, s := range sparse {
		if dir == s || strings.HasPrefix(dir, s+"/") || strings.HasPrefix(s, dir+"/") {
			return true
		}
	}
	return false
}
```

`internal/wc/fs.go`:
```go
package wc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hermes98761234/sgit/internal/fsutil"
)

// ModeOf maps file info to a git mode; ok is false for unsupported types.
func ModeOf(fi fs.FileInfo) (string, bool) {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "120000", true
	case fi.Mode().IsRegular():
		if fi.Mode().Perm()&0o111 != 0 {
			return "100755", true
		}
		return "100644", true
	}
	return "", false
}

func full(root, p string) string { return filepath.Join(root, filepath.FromSlash(p)) }

// ReadLocal returns the content of repo path p as git would store it (the
// target for symlinks) and its mode.
func ReadLocal(root, p string) ([]byte, string, error) {
	f := full(root, p)
	fi, err := os.Lstat(f)
	if err != nil {
		return nil, "", err
	}
	mode, ok := ModeOf(fi)
	if !ok {
		return nil, "", fmt.Errorf("%s: unsupported file type", p)
	}
	if mode == "120000" {
		t, err := os.Readlink(f)
		return []byte(t), mode, err
	}
	data, err := os.ReadFile(f)
	return data, mode, err
}

// makeParents creates the parent directories of p, replacing files that are
// in the way (a tracked file that became a directory upstream).
func makeParents(root, p string) error {
	dir := root
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil:
			if err := os.Remove(dir); err != nil {
				return err
			}
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// WriteLocal writes data at repo path p with git mode, replacing an existing
// file or symlink. An empty directory in the way is removed; a non-empty one
// is an error.
func WriteLocal(root, p string, data []byte, mode string) error {
	if err := makeParents(root, p); err != nil {
		return err
	}
	f := full(root, p)
	if fi, err := os.Lstat(f); err == nil && fi.IsDir() {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("%s: a directory is in the way", p)
		}
	}
	switch mode {
	case "120000":
		tmp := f + ".sg-tmp-link"
		_ = os.Remove(tmp)
		if err := os.Symlink(string(data), tmp); err != nil {
			return err
		}
		return os.Rename(tmp, f)
	case "100755":
		return fsutil.WriteAtomic(f, data, 0o755)
	default:
		return fsutil.WriteAtomic(f, data, 0o644)
	}
}

// RemoveLocal deletes repo path p and prunes parent directories that became
// empty. The root itself is never removed.
func RemoveLocal(root, p string) error {
	f := full(root, p)
	if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(f); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

// StatEntry builds an index entry for p with the given mode and SHA and the
// file's current stat.
func StatEntry(root, p, mode, sha string) (Entry, error) {
	fi, err := os.Lstat(full(root, p))
	if err != nil {
		return Entry{}, err
	}
	return Entry{Path: p, Mode: mode, SHA: sha, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}, nil
}
```

`internal/wc/paths.go`:
```go
package wc

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RepoPath converts a user-supplied path (relative to cwd, or absolute) into a
// slash-separated repo path. The working copy root itself is "".
func (w *WC) RepoPath(cwd, arg string) (string, error) {
	base, err := realDir(cwd)
	if err != nil {
		return "", err
	}
	f := arg
	if !filepath.IsAbs(f) {
		f = filepath.Join(base, arg)
	}
	rel, err := filepath.Rel(w.Root, filepath.Clean(f))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the working copy", arg)
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race -count=1 ./internal/wc/`
Expected: `ok`. Note: the absolute-path case in `TestRepoPath` uses `w.Root`, which is already symlink-resolved, so it works even when the temp dir is behind a symlink.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 3: Status (stat cache, racy re-hash, .gitignore)

**Files:**
- Create: `internal/wc/status.go`
- Test: `internal/wc/status_test.go`

**Interfaces:**
- Consumes: Task 2 (`WC`, `Entry`, `ReadLocal`, `ModeOf`, `InSparse`, `TreeNeeded`, `StatEntry`), and `objfmt.BlobSHA(data []byte) string`.
- Produces:
  - `wc.Kind` (`wc.Added`, `wc.Modified`, `wc.Deleted`) with `String()` returning `"added"`, `"modified"` or `"deleted"`.
  - `wc.Change{Path string; Kind Kind; Mode, SHA string}`. Mode and SHA are the local file's values for added and modified files, and empty for deleted ones.
  - `(*WC).Status() ([]Change, error)` returns the changes sorted by path. It refreshes the stat cache of unchanged, non-racy entries in `w.Index`; callers may `Save()` to persist that.

- [ ] **Step 1: Write the failing tests**

`internal/wc/status_test.go`:
```go
package wc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/objfmt"
)

// track writes content at p, records it in the index as of Base, and ages the
// file so it is outside the racy window.
func track(t *testing.T, w *WC, p, content, mode string) {
	t.Helper()
	if err := WriteLocal(w.Root, p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if mode != "120000" {
		if err := os.Chtimes(w.Abs(p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	e, err := StatEntry(w.Root, p, mode, objfmt.BlobSHA([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	w.Index[p] = e
}

func newWC(t *testing.T) *WC {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	return &WC{Root: root, Config: Config{Server: "s:1", Repo: "r"}, Base: "b", Index: map[string]Entry{}}
}

func kinds(changes []Change) map[string]Kind {
	m := map[string]Kind{}
	for _, c := range changes {
		m[c.Path] = c.Kind
	}
	return m
}

func TestStatusBasics(t *testing.T) {
	w := newWC(t)
	track(t, w, "same.txt", "same\n", "100644")
	track(t, w, "mod.txt", "old\n", "100644")
	track(t, w, "gone.txt", "bye\n", "100644")
	track(t, w, "run.sh", "#!/bin/sh\n", "100644")
	_ = os.MkdirAll(filepath.Join(w.Root, ".sg"), 0o755)
	_ = os.WriteFile(filepath.Join(w.Root, ".sg", "index"), []byte("meta"), 0o644)
	_ = WriteLocal(w.Root, "mod.txt", []byte("new content\n"), "100644")
	_ = os.Remove(w.Abs("gone.txt"))
	_ = os.Chmod(w.Abs("run.sh"), 0o755)
	_ = WriteLocal(w.Root, "dir/new.txt", []byte("fresh\n"), "100644")
	_ = WriteLocal(w.Root, ".gitignore", []byte("*.log\nbuild/\n"), "100644")
	_ = WriteLocal(w.Root, "debug.log", []byte("noise"), "100644")
	_ = WriteLocal(w.Root, "build/out.bin", []byte("bin"), "100644")

	changes, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(changes)
	want := map[string]Kind{"mod.txt": Modified, "gone.txt": Deleted, "run.sh": Modified, "dir/new.txt": Added, ".gitignore": Added}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v", changes)
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: got %v, want %v", p, got[p], k)
		}
	}
	for _, c := range changes {
		switch c.Path {
		case "mod.txt":
			if c.SHA != objfmt.BlobSHA([]byte("new content\n")) || c.Mode != "100644" {
				t.Errorf("mod.txt change = %+v", c)
			}
		case "run.sh":
			if c.Mode != "100755" {
				t.Errorf("run.sh mode change = %+v", c)
			}
		}
	}
	for i := 1; i < len(changes); i++ {
		if changes[i-1].Path >= changes[i].Path {
			t.Fatal("changes must be sorted by path")
		}
	}
}

func TestStatusTrackedFileIsNeverIgnored(t *testing.T) {
	w := newWC(t)
	track(t, w, "keep.log", "v1\n", "100644")
	_ = WriteLocal(w.Root, ".gitignore", []byte("*.log\n"), "100644")
	track(t, w, ".gitignore", "*.log\n", "100644")
	_ = WriteLocal(w.Root, "keep.log", []byte("v2\n"), "100644")
	changes, _ := w.Status()
	if k := kinds(changes); len(k) != 1 || k["keep.log"] != Modified {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusRespectsSparse(t *testing.T) {
	w := newWC(t)
	w.Config.Sparse = []string{"src"}
	track(t, w, "src/a.go", "a\n", "100644")
	_ = WriteLocal(w.Root, "src/b.go", []byte("b\n"), "100644")
	_ = WriteLocal(w.Root, "docs/outside.md", []byte("x\n"), "100644")
	changes, _ := w.Status()
	if k := kinds(changes); len(k) != 1 || k["src/b.go"] != Added {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusSymlink(t *testing.T) {
	w := newWC(t)
	track(t, w, "target.txt", "t\n", "100644")
	if err := os.Symlink("target.txt", w.Abs("link")); err != nil {
		t.Fatal(err)
	}
	changes, _ := w.Status()
	if len(changes) != 1 || changes[0].Path != "link" || changes[0].Mode != "120000" || changes[0].SHA != objfmt.BlobSHA([]byte("target.txt")) {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStatusDetectsRacyEdit(t *testing.T) {
	w := newWC(t)
	if err := WriteLocal(w.Root, "a.txt", []byte("aaaa"), "100644"); err != nil {
		t.Fatal(err)
	}
	e, _ := StatEntry(w.Root, "a.txt", "100644", objfmt.BlobSHA([]byte("aaaa")))
	w.Index["a.txt"] = e
	fi, _ := os.Lstat(w.Abs("a.txt"))
	// Same size, same mtime: only a re-hash can notice this edit.
	_ = WriteLocal(w.Root, "a.txt", []byte("bbbb"), "100644")
	_ = os.Chtimes(w.Abs("a.txt"), fi.ModTime(), fi.ModTime())
	changes, err := w.Status()
	if err != nil || len(changes) != 1 || changes[0].Kind != Modified {
		t.Fatalf("racy edit not detected: %+v, %v", changes, err)
	}
}

func TestStatusRefreshesStatCache(t *testing.T) {
	w := newWC(t)
	track(t, w, "a.txt", "abcd", "100644")
	e := w.Index["a.txt"]
	e.Size, e.MtimeNs = -1, 0
	w.Index["a.txt"] = e
	changes, _ := w.Status()
	if len(changes) != 0 {
		t.Fatalf("unchanged file reported: %+v", changes)
	}
	if got := w.Index["a.txt"]; got.Size != 4 || got.MtimeNs == 0 {
		t.Fatalf("stat cache not refreshed: %+v", got)
	}
}

func TestStatusFileReplacedByDirectory(t *testing.T) {
	w := newWC(t)
	track(t, w, "d", "file\n", "100644")
	_ = os.Remove(w.Abs("d"))
	_ = WriteLocal(w.Root, "d/inner.txt", []byte("in\n"), "100644")
	got := kinds(testStatus(t, w))
	if got["d"] != Deleted || got["d/inner.txt"] != Added || len(got) != 2 {
		t.Fatalf("changes = %v", got)
	}
}

func testStatus(t *testing.T, w *WC) []Change {
	t.Helper()
	c, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	return c
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/wc/ -run Status`
Expected: FAIL to compile (`w.Status undefined`, `undefined: Modified`).

- [ ] **Step 3: Implement**

`internal/wc/status.go`:
```go
package wc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hermes98761234/sgit/internal/objfmt"
	ignore "github.com/sabhiram/go-gitignore"
)

// Kind is the kind of a working-copy change.
type Kind int

const (
	Added Kind = iota + 1
	Modified
	Deleted
)

func (k Kind) String() string {
	switch k {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Deleted:
		return "deleted"
	}
	return "unknown"
}

// Change is one difference between the working tree and Base. Mode and SHA
// describe the local file (empty for deletions).
type Change struct {
	Path string
	Kind Kind
	Mode string
	SHA  string
}

// racyWindow: files modified this recently are always re-hashed, because an
// edit within the same mtime tick would be invisible to the stat check.
const racyWindow = 2 * time.Second

func loadIgnore(root string) (*ignore.GitIgnore, error) {
	p := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return ignore.CompileIgnoreLines(), nil
	}
	return ignore.CompileIgnoreFile(p)
}

// Status compares the working tree inside the sparse set with the index. It
// refreshes the stat cache of unchanged files outside the racy window in
// w.Index; callers may Save to persist the refresh.
func (w *WC) Status() ([]Change, error) {
	ign, err := loadIgnore(w.Root)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Change
	for _, p := range w.SortedPaths() {
		if !InSparse(w.Config.Sparse, p) {
			continue
		}
		e := w.Index[p]
		fi, err := os.Lstat(w.Abs(p))
		if errors.Is(err, os.ErrNotExist) {
			out = append(out, Change{Path: p, Kind: Deleted})
			continue
		}
		if err != nil {
			return nil, err
		}
		mode, ok := ModeOf(fi)
		if !ok {
			out = append(out, Change{Path: p, Kind: Deleted})
			continue
		}
		mtime := fi.ModTime().UnixNano()
		settled := now.Sub(fi.ModTime()) > racyWindow
		if settled && e.Size >= 0 && fi.Size() == e.Size && mtime == e.MtimeNs && mode == e.Mode {
			continue
		}
		data, _, err := ReadLocal(w.Root, p)
		if err != nil {
			return nil, err
		}
		sha := objfmt.BlobSHA(data)
		if sha == e.SHA && mode == e.Mode {
			if settled {
				e.Size, e.MtimeNs = fi.Size(), mtime
				w.Index[p] = e
			}
			continue
		}
		out = append(out, Change{Path: p, Kind: Modified, Mode: mode, SHA: sha})
	}
	err = filepath.WalkDir(w.Root, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.Root, f)
		if err != nil || rel == "." {
			return err
		}
		p := filepath.ToSlash(rel)
		if d.IsDir() {
			if p == metaDir || !TreeNeeded(w.Config.Sparse, p) || ign.MatchesPath(p+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if _, tracked := w.Index[p]; tracked || !InSparse(w.Config.Sparse, p) || ign.MatchesPath(p) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode, ok := ModeOf(fi)
		if !ok {
			return nil
		}
		data, _, err := ReadLocal(w.Root, p)
		if err != nil {
			return err
		}
		out = append(out, Change{Path: p, Kind: Added, Mode: mode, SHA: objfmt.BlobSHA(data)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race -count=1 ./internal/wc/`
Expected: `ok`.
- If `go-gitignore` matches a directory pattern like `build/` differently than expected, keep the observable contract: `build/out.bin` is not reported. Adjust only the `MatchesPath` arguments, and note it in the report.
- In `TestStatusFileReplacedByDirectory`, the tracked file "d" is now a directory, so `ModeOf` fails and it is reported as Deleted, while the walk reports `d/inner.txt` as Added.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 4: Fetch (trees and blobs)

**Files:**
- Create: `internal/fetch/fetch.go`
- Test: `internal/fetch/fetch_test.go`

**Interfaces:**
- Consumes:
  - From sgitv1: `RepoClient.GetObjects(ctx, *GetObjectsRequest{Repo, Shas}) (grpc.ServerStreamingClient[Object], error)`, where `Object{Sha, Type string; Size uint64; ZstdData []byte}`.
  - From objfmt: `objfmt.ParseTree`, `objfmt.TreeEntry{Mode, Name, SHA}` with `IsTree()`, and `objfmt.BlobSHA`.
  - From wc: `wc.InSparse` and `wc.TreeNeeded`.
  - From testenv: `testenv.CommitDirect(repo, files, msg) string`.
- Produces:
  - `fetch.File{Mode, SHA string}`.
  - `fetch.Tree(ctx, c sgitv1.RepoClient, repo, commit string, sparse []string) (map[string]File, error)`.
  - `fetch.Blobs(ctx, c, repo string, shas []string) (map[string][]byte, error)`. It verifies every blob's SHA.
  - Constant `fetch.Batch = 1000`.

- [ ] **Step 1: Write the failing tests**

`internal/fetch/fetch_test.go`:
```go
package fetch

import (
	"context"
	"fmt"
	"testing"

	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTreeAndBlobs(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	commit := e.CommitDirect("web", map[string]string{
		"a.txt": "A\n", "src/x.go": "X\n", "src/sub/y.go": "Y\n", "docs/z.md": "Z\n",
	}, "seed")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()

	full := testenv.Must(Tree(ctx, c, "web", commit, nil))
	if len(full) != 4 || full["src/sub/y.go"].SHA != objfmt.BlobSHA([]byte("Y\n")) || full["a.txt"].Mode != "100644" {
		t.Fatalf("full tree = %+v", full)
	}
	for sparse, want := range map[string]int{"src": 2, "src/sub": 1, "docs": 1, "nope": 0, "a.txt": 1} {
		got := testenv.Must(Tree(ctx, c, "web", commit, []string{sparse}))
		if len(got) != want {
			t.Errorf("sparse %q: %d files, want %d (%v)", sparse, len(got), want, got)
		}
	}
	blobs := testenv.Must(Blobs(ctx, c, "web", []string{full["a.txt"].SHA, full["docs/z.md"].SHA, full["a.txt"].SHA}))
	if string(blobs[full["a.txt"].SHA]) != "A\n" || string(blobs[full["docs/z.md"].SHA]) != "Z\n" || len(blobs) != 2 {
		t.Fatalf("blobs = %v", blobs)
	}
	if got := testenv.Must(Blobs(ctx, c, "web", nil)); len(got) != 0 {
		t.Fatalf("no shas must fetch nothing: %v", got)
	}
	if _, err := Blobs(ctx, c, "web", []string{"1111111111111111111111111111111111111111"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing blob: want NotFound, got %v", err)
	}
	if _, err := Blobs(ctx, c, "web", []string{commit}); err == nil {
		t.Fatal("a commit is not a blob")
	}
	if _, err := Tree(ctx, c, "web", full["a.txt"].SHA, nil); err == nil {
		t.Fatal("a blob is not a commit")
	}
}

func TestBatching(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	files := map[string]string{}
	for i := 0; i < Batch+150; i++ {
		files[fmt.Sprintf("d%02d/f%04d.txt", i%7, i)] = fmt.Sprintf("content %d\n", i)
	}
	commit := e.CommitDirect("web", files, "many")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	tree := testenv.Must(Tree(ctx, c, "web", commit, nil))
	if len(tree) != len(files) {
		t.Fatalf("tree has %d files, want %d", len(tree), len(files))
	}
	var shas []string
	for _, f := range tree {
		shas = append(shas, f.SHA)
	}
	blobs := testenv.Must(Blobs(ctx, c, "web", shas))
	if len(blobs) != len(files) {
		t.Fatalf("got %d blobs, want %d", len(blobs), len(files))
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/fetch/`
Expected: FAIL to compile (`undefined: Tree`).

- [ ] **Step 3: Implement**

`internal/fetch/fetch.go`:
```go
// Package fetch implements the client side of the Scion-style fetch protocol:
// walk trees level by level, asking the server only for the objects needed
// (honouring the sparse set), then fetch blobs in batches.
package fetch

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
	"github.com/klauspost/compress/zstd"
)

// Batch is the maximum number of SHAs per GetObjects request.
const Batch = 1000

// File is a file in a tree listing.
type File struct {
	Mode string
	SHA  string
}

var (
	decOnce sync.Once
	dec     *zstd.Decoder
	decErr  error
)

func decoder() (*zstd.Decoder, error) {
	decOnce.Do(func() { dec, decErr = zstd.NewReader(nil) })
	return dec, decErr
}

type object struct {
	typ  string
	data []byte
}

func uniq(shas []string) []string {
	seen := make(map[string]bool, len(shas))
	var out []string
	for _, s := range shas {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// objects fetches and decompresses objects, Batch SHAs per request.
func objects(ctx context.Context, c sgitv1.RepoClient, repo string, shas []string) (map[string]object, error) {
	d, err := decoder()
	if err != nil {
		return nil, err
	}
	shas = uniq(shas)
	out := make(map[string]object, len(shas))
	for start := 0; start < len(shas); start += Batch {
		end := min(start+Batch, len(shas))
		stream, err := c.GetObjects(ctx, &sgitv1.GetObjectsRequest{Repo: repo, Shas: shas[start:end]})
		if err != nil {
			return nil, err
		}
		for {
			o, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			data, err := d.DecodeAll(o.ZstdData, make([]byte, 0, o.Size))
			if err != nil {
				return nil, fmt.Errorf("object %s: %w", o.Sha, err)
			}
			if uint64(len(data)) != o.Size {
				return nil, fmt.Errorf("object %s: size mismatch", o.Sha)
			}
			out[o.Sha] = object{typ: o.Type, data: data}
		}
	}
	for _, s := range shas {
		if _, ok := out[s]; !ok {
			return nil, fmt.Errorf("server did not return object %s", s)
		}
	}
	return out, nil
}

func normalizeMode(m string) string {
	switch m {
	case "100755", "120000":
		return m
	}
	return "100644"
}

// Tree lists the files of commit's tree inside the sparse set (nil = all).
// Submodule entries are skipped.
func Tree(ctx context.Context, c sgitv1.RepoClient, repo, commit string, sparse []string) (map[string]File, error) {
	objs, err := objects(ctx, c, repo, []string{commit})
	if err != nil {
		return nil, err
	}
	co := objs[commit]
	if co.typ != "commit" {
		return nil, fmt.Errorf("%s is a %s, not a commit", commit, co.typ)
	}
	first, _, _ := strings.Cut(string(co.data), "\n")
	root, ok := strings.CutPrefix(first, "tree ")
	if !ok {
		return nil, fmt.Errorf("commit %s has no tree", commit)
	}
	type dir struct{ path, sha string }
	files := map[string]File{}
	level := []dir{{"", root}}
	for len(level) > 0 {
		shas := make([]string, 0, len(level))
		for _, d := range level {
			shas = append(shas, d.sha)
		}
		trees, err := objects(ctx, c, repo, shas)
		if err != nil {
			return nil, err
		}
		var next []dir
		for _, d := range level {
			t := trees[d.sha]
			if t.typ != "tree" {
				return nil, fmt.Errorf("%s is a %s, not a tree", d.sha, t.typ)
			}
			entries, err := objfmt.ParseTree(t.data)
			if err != nil {
				return nil, err
			}
			for _, en := range entries {
				p := en.Name
				if d.path != "" {
					p = d.path + "/" + en.Name
				}
				switch {
				case en.IsTree():
					if wc.TreeNeeded(sparse, p) {
						next = append(next, dir{p, en.SHA})
					}
				case en.Mode == "160000":
					// submodule: not materialized
				default:
					if wc.InSparse(sparse, p) {
						files[p] = File{Mode: normalizeMode(en.Mode), SHA: en.SHA}
					}
				}
			}
		}
		level = next
	}
	return files, nil
}

// Blobs fetches blob contents by SHA, verifying each against its SHA.
func Blobs(ctx context.Context, c sgitv1.RepoClient, repo string, shas []string) (map[string][]byte, error) {
	objs, err := objects(ctx, c, repo, shas)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(objs))
	for sha, o := range objs {
		if o.typ != "blob" {
			return nil, fmt.Errorf("%s is a %s, not a blob", sha, o.typ)
		}
		if objfmt.BlobSHA(o.data) != sha {
			return nil, fmt.Errorf("blob %s failed its integrity check", sha)
		}
		out[sha] = o.data
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/fetch/`
Expected: `ok`. `TestBatching` writes 1150 blobs through the server and may take several seconds.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---
### Task 5: CLI skeleton and `sg login`

**Files:**
- Create: `internal/cli/cli.go`, `internal/cli/flags.go`, `internal/cli/session.go`, `internal/cli/login.go`, `cmd/sg/main.go`
- Test: `internal/cli/harness_test.go`, `internal/cli/login_test.go`

**Interfaces:**
- Consumes: `creds.Load/Save/Lookup`, `client.Dial`, `wc.Open`, `wc.ErrNotWorkingCopy`, and `sgitv1.NewRepoClient/NewAdminClient`.
- Produces (later tasks add commands and use these helpers):
  - `cli.IO{Stdin io.Reader; Stdout, Stderr io.Writer; Dir string}`, and `cli.Run(ctx context.Context, args []string, stdio IO) int`.
  - Constants `cli.ExitOK=0`, `ExitError=1`, `ExitAuth=2`, `ExitConflict=3`, `ExitCAS=4`.
  - Internal plumbing:
    - `type app struct{ io IO; json, emitted bool }` and `type command struct{ name, usage string; run func(context.Context, *app, []string) error }`.
    - `commandTable() []command`. Later tasks add rows.
    - `(*app).emit(v any, text func(io.Writer)) error` prints JSON when `--json` is set, and otherwise calls `text`.
  - Errors:
    - `exitf(code int, format string, a ...any) error` and `usageErr(usage string) error`.
    - `exitCode(err error) int`.
    - `conflictDetail(err error) *sgitv1.ConflictDetail`.
  - Flags: `type stringList []string` (implements `flag.Value`), `(*app).newFlags(name string) *flag.FlagSet` (binds `--json`), `parse(fs *flag.FlagSet, args []string) ([]string, error)` (flags interleaved with positionals), `(*app).abs(p string) string`.
  - Session:
    - `type session struct{ conn *grpc.ClientConn; repo sgitv1.RepoClient; admin sgitv1.AdminClient; server, name string; wc *wc.WC }` with `Close()`.
    - `type target struct{ server, repo string; needWC, needRepo bool }`, and `(*app).connect(t target) (*session, error)`.
    - `openWC(a *app) (*wc.WC, error)` and `short(sha string) string`.
  - Test harness, in `harness_test.go`, `package cli_test`:
    - `newHarness(t, opts ...func(*server.Config)) *harness`, which starts testenv and logs in as `admin`.
    - Fields `h.env *testenv.Env` and `h.cfg map[string]string` (user → config dir).
    - Login: `h.loginAs(user, token string)`.
    - Running sg: `h.run(dir string, args ...string) (int, string, string)`, `h.runAs(user, dir string, args ...string)`, `h.runStdin(user, dir, stdin string, args ...string)`, `h.ok(dir, args...) string` and `h.okAs(user, dir, args...) string`.
    - Fixtures: `h.seed(files map[string]string) string` creates repo `web`, commits the files directly, and returns main. `readFile(t, p) string` and `writeFile(t, p, content string)` are file helpers.

- [ ] **Step 1: Write the failing tests**

`internal/cli/harness_test.go`:
```go
package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
)

type harness struct {
	t   *testing.T
	env *testenv.Env
	cfg map[string]string
}

func newHarness(t *testing.T, opts ...func(*server.Config)) *harness {
	t.Helper()
	t.Setenv("SG_TOKEN", "")
	h := &harness{t: t, env: testenv.Start(t, opts...), cfg: map[string]string{}}
	h.loginAs("admin", h.env.AdminToken)
	return h
}

func (h *harness) loginAs(user, token string) {
	h.t.Helper()
	h.cfg[user] = h.t.TempDir()
	if code, _, errs := h.runAs(user, h.t.TempDir(), "login", h.env.Addr, "--token", token, "--dev-insecure"); code != 0 {
		h.t.Fatalf("login %s: exit %d: %s", user, code, errs)
	}
}

func (h *harness) runStdin(user, dir, stdin string, args ...string) (int, string, string) {
	h.t.Setenv("SG_CONFIG_DIR", h.cfg[user])
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), args, cli.IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Dir: dir})
	return code, out.String(), errb.String()
}

func (h *harness) runAs(user, dir string, args ...string) (int, string, string) {
	return h.runStdin(user, dir, "", args...)
}

func (h *harness) run(dir string, args ...string) (int, string, string) {
	return h.runAs("admin", dir, args...)
}

func (h *harness) okAs(user, dir string, args ...string) string {
	h.t.Helper()
	code, out, errs := h.runAs(user, dir, args...)
	if code != 0 {
		h.t.Fatalf("sg %s (as %s): exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), user, code, out, errs)
	}
	return out
}

func (h *harness) ok(dir string, args ...string) string {
	h.t.Helper()
	return h.okAs("admin", dir, args...)
}

// seed creates repo "web" with files committed directly on main and returns main.
func (h *harness) seed(files map[string]string) string {
	h.t.Helper()
	h.env.CreateRepo("web")
	return h.env.CommitDirect("web", files, "seed")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

`internal/cli/login_test.go`:
```go
package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func loadCreds(t *testing.T, dir string) creds.File {
	t.Helper()
	t.Setenv("SG_CONFIG_DIR", dir)
	return testenv.Must(creds.Load())
}

func TestLoginStoresCredentials(t *testing.T) {
	h := newHarness(t)
	f := loadCreds(t, h.cfg["admin"])
	s := f.Servers[h.env.Addr]
	if f.Default != h.env.Addr || s.Token != h.env.AdminToken || !s.Insecure {
		t.Fatalf("creds = %+v", f)
	}
	out := h.ok(t.TempDir(), "login", h.env.Addr, "--token", h.env.AdminToken, "--dev-insecure", "--json")
	var res struct {
		Server string `json:"server"`
		User   string `json:"user"`
		Admin  bool   `json:"admin"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.User != "admin" || !res.Admin || res.Server != h.env.Addr {
		t.Fatalf("json = %q (%v)", out, err)
	}
}

func TestLoginTokenFromStdinAndEnv(t *testing.T) {
	h := newHarness(t)
	bob := h.env.NewUser("bob", "", "")
	h.cfg["bob"] = t.TempDir()
	if code, _, errs := h.runStdin("bob", t.TempDir(), bob+"\n", "login", h.env.Addr, "--dev-insecure"); code != 0 {
		t.Fatalf("stdin login: %d %s", code, errs)
	}
	if loadCreds(t, h.cfg["bob"]).Servers[h.env.Addr].Token != bob {
		t.Fatal("token from stdin not stored")
	}
	h.cfg["bob2"] = t.TempDir()
	t.Setenv("SG_TOKEN", bob)
	if code, _, errs := h.runAs("bob2", t.TempDir(), "login", h.env.Addr, "--dev-insecure"); code != 0 {
		t.Fatalf("env login: %d %s", code, errs)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"sgt_bogus", "bogus"} {
		if code, _, _ := h.run(t.TempDir(), "login", h.env.Addr, "--token", tok, "--dev-insecure"); code != 2 {
			t.Errorf("token %q: exit %d, want 2", tok, code)
		}
	}
	if loadCreds(t, h.cfg["admin"]).Servers[h.env.Addr].Token != h.env.AdminToken {
		t.Fatal("a failed login must not overwrite stored credentials")
	}
}

func TestLoginRefusesInsecureRemote(t *testing.T) {
	h := newHarness(t)
	code, _, errs := h.run(t.TempDir(), "login", "10.9.9.9:7419", "--token", "sgt_x", "--dev-insecure")
	if code != 1 || !strings.Contains(errs, "loopback") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestUnknownCommandAndHelp(t *testing.T) {
	h := newHarness(t)
	if code, _, errs := h.run(t.TempDir(), "frobnicate"); code != 1 || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown: %d %q", code, errs)
	}
	if code, out, _ := h.run(t.TempDir(), "help"); code != 0 || !strings.Contains(out, "sg login") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, _ := h.run(t.TempDir()); code != 1 {
		t.Fatalf("no args: exit %d, want 1", code)
	}
	if code, _, errs := h.run(t.TempDir(), "login"); code != 1 || !strings.Contains(errs, "usage") {
		t.Fatalf("login without server: %d %q", code, errs)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/cli/`
Expected: FAIL to compile (`undefined: cli.Run`).

- [ ] **Step 3: Implement**

`internal/cli/cli.go`:
```go
// Package cli implements the sg command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Exit codes (spec §4/§10).
const (
	ExitOK       = 0
	ExitError    = 1
	ExitAuth     = 2
	ExitConflict = 3
	ExitCAS      = 4
)

// IO is the process environment a command runs in.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string // working directory; "" = os.Getwd()
}

type app struct {
	io      IO
	json    bool
	emitted bool
}

type command struct {
	name  string
	usage string
	run   func(ctx context.Context, a *app, args []string) error
}

func commandTable() []command {
	return []command{
		{"login", "login <host:port> [--token T] [--ca FILE] [--dev-insecure]", cmdLogin},
	}
}

// Run executes one sg command and returns the process exit code.
func Run(ctx context.Context, args []string, stdio IO) int {
	if stdio.Dir == "" {
		if d, err := os.Getwd(); err == nil {
			stdio.Dir = d
		}
	}
	if stdio.Stdin == nil {
		stdio.Stdin = strings.NewReader("")
	}
	a := &app{io: stdio}
	if len(args) == 0 {
		usage(stdio.Stderr)
		return ExitError
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdio.Stdout)
		return ExitOK
	}
	table := commandTable()
	idx := -1
	for i := range table {
		if table[i].name == args[0] {
			idx = i
		}
	}
	if idx < 0 {
		fmt.Fprintf(stdio.Stderr, "sg: unknown command %q\n\n", args[0])
		usage(stdio.Stderr)
		return ExitError
	}
	err := table[idx].run(ctx, a, args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	code := exitCode(err)
	msg := errorMessage(err)
	if a.json && !a.emitted {
		_ = json.NewEncoder(stdio.Stdout).Encode(map[string]any{"error": msg, "exit_code": code})
	}
	fmt.Fprintf(stdio.Stderr, "sg: %s\n", msg)
	return code
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: sg <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, c := range commandTable() {
		fmt.Fprintf(w, "  sg %s\n", c.usage)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command accepts --json. Exit codes: 0 ok, 1 error, 2 auth, 3 conflict, 4 main kept moving.")
}

// emit prints v as JSON when --json was given, otherwise calls text.
func (a *app) emit(v any, text func(w io.Writer)) error {
	a.emitted = true
	if a.json {
		enc := json.NewEncoder(a.io.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if text != nil {
		text(a.io.Stdout)
	}
	return nil
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitf(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

func usageErr(u string) error { return exitf(ExitError, "usage: sg %s", u) }

// conflictDetail extracts the land-conflict detail from a gRPC error.
func conflictDetail(err error) *sgitv1.ConflictDetail {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, d := range st.Details() {
		if cd, ok := d.(*sgitv1.ConflictDetail); ok {
			return cd
		}
	}
	return nil
}

func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	st, ok := status.FromError(err)
	if !ok {
		return ExitError
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		return ExitAuth
	case codes.Aborted:
		if conflictDetail(err) != nil {
			return ExitConflict
		}
	case codes.ResourceExhausted:
		return ExitCAS
	}
	return ExitError
}

func errorMessage(err error) string {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Error()
	}
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}
```

`internal/cli/flags.go`:
```go
package cli

import (
	"errors"
	"flag"
	"path/filepath"
	"strings"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// newFlags returns a FlagSet that reports to stderr and binds --json.
func (a *app) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("sg "+name, flag.ContinueOnError)
	fs.SetOutput(a.io.Stderr)
	fs.BoolVar(&a.json, "json", false, "machine-readable JSON output")
	return fs
}

// parse parses flags that may be interleaved with positional arguments and
// returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &exitError{code: ExitError, err: err}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// abs resolves p against the command's working directory.
func (a *app) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(a.io.Dir, p)
}
```

`internal/cli/session.go`:
```go
package cli

import (
	"errors"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc"
)

// session is a connection to one server plus the working copy (if any).
type session struct {
	conn   *grpc.ClientConn
	repo   sgitv1.RepoClient
	admin  sgitv1.AdminClient
	server string
	name   string // repository name
	wc     *wc.WC
}

func (s *session) Close() { s.conn.Close() }

// target says which server/repo a command needs; empty fields come from the
// working copy, then from the default login.
type target struct {
	server   string
	repo     string
	needWC   bool
	needRepo bool
}

func openWC(a *app) (*wc.WC, error) {
	w, err := wc.Open(a.io.Dir)
	if errors.Is(err, wc.ErrNotWorkingCopy) {
		return nil, exitf(ExitError, "%v", err)
	}
	return w, err
}

func (a *app) connect(t target) (*session, error) {
	w, err := wc.Open(a.io.Dir)
	switch {
	case err == nil:
	case errors.Is(err, wc.ErrNotWorkingCopy):
		if t.needWC {
			return nil, exitf(ExitError, "%v", err)
		}
		w = nil
	default:
		return nil, err
	}
	server, repo := t.server, t.repo
	if w != nil {
		if server == "" {
			server = w.Config.Server
		}
		if repo == "" {
			repo = w.Config.Repo
		}
	}
	f, err := creds.Load()
	if err != nil {
		return nil, err
	}
	name, cred, ok := f.Lookup(server)
	if !ok {
		if name == "" {
			return nil, exitf(ExitAuth, "not logged in; run `sg login <host:port>`")
		}
		return nil, exitf(ExitAuth, "no credentials for %s; run `sg login %s`", name, name)
	}
	if t.needRepo && repo == "" {
		return nil, exitf(ExitError, "no repository: run inside a working copy or pass --repo")
	}
	conn, err := client.Dial(name, cred)
	if err != nil {
		return nil, exitf(ExitError, "%v", err)
	}
	return &session{
		conn: conn, repo: sgitv1.NewRepoClient(conn), admin: sgitv1.NewAdminClient(conn),
		server: name, name: repo, wc: w,
	}, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
```

`internal/cli/login.go`:
```go
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
)

func cmdLogin(ctx context.Context, a *app, args []string) error {
	const u = "login <host:port> [--token T] [--ca FILE] [--dev-insecure]"
	fs := a.newFlags("login")
	token := fs.String("token", os.Getenv("SG_TOKEN"), "API token (default $SG_TOKEN, else read from stdin)")
	ca := fs.String("ca", "", "PEM file with the server's (self-signed) certificate")
	insecure := fs.Bool("dev-insecure", false, "plaintext connection (loopback servers only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(u)
	}
	server := pos[0]
	tok := strings.TrimSpace(*token)
	if tok == "" {
		fmt.Fprint(a.io.Stderr, "token: ")
		line, _ := bufio.NewReader(a.io.Stdin).ReadString('\n')
		if tok = strings.TrimSpace(line); tok == "" {
			return exitf(ExitError, "no token given (use --token, $SG_TOKEN or stdin)")
		}
	}
	cred := creds.Server{Token: tok, Insecure: *insecure}
	if *ca != "" {
		cred.CAFile = a.abs(*ca)
	}
	conn, err := client.Dial(server, cred)
	if err != nil {
		return exitf(ExitError, "%v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	who, err := sgitv1.NewAdminClient(conn).WhoAmI(ctx, &sgitv1.WhoAmIRequest{})
	if err != nil {
		return err
	}
	f, err := creds.Load()
	if err != nil {
		return err
	}
	f.Servers[server] = cred
	f.Default = server
	if err := creds.Save(f); err != nil {
		return err
	}
	return a.emit(map[string]any{"server": server, "user": who.User, "admin": who.Admin}, func(w io.Writer) {
		fmt.Fprintf(w, "logged in to %s as %s\n", server, who.User)
	})
}
```

`cmd/sg/main.go`:
```go
// Command sg is the sgit client.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/hermes98761234/sgit/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], cli.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	stop()
	os.Exit(code)
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/cli/ && go build ./cmd/sg && ./sg; echo "exit=$?"; rm -f sg`
Expected: `ok`, then the usage text and `exit=1`.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 6: `sg checkout`, `sg status`, `sg diff`

**Files:**
- Create: `internal/cli/checkout.go`, `internal/cli/status.go`, `internal/cli/diff.go`
- Modify: `internal/cli/cli.go` (add three rows to `commandTable`)
- Test: `internal/cli/checkout_test.go`

**Interfaces:**
- Consumes:
  - Task 5: app, connect, target, openWC, emit, exitf, usageErr, parse, stringList, short.
  - From wc: `wc.Create/Save/Status/RepoPath`, `wc.NormalizeSparse`, `wc.ReadLocal/WriteLocal/StatEntry`.
  - From fetch: `fetch.Tree/Blobs`, `fetch.File`.
  - `objfmt.BlobSHA`, and `difflib` from `github.com/pmezard/go-difflib/difflib`.
- Produces:
  - `materialize(ctx, c sgitv1.RepoClient, repo, root string, files map[string]fetch.File) (map[string]wc.Entry, error)`. It skips files that already hold the right content, and groups writes 500 at a time.
  - `writePatch(w io.Writer, p string, kind wc.Kind, oldMode, newMode string, old, cur []byte) error`.
  - `matchesAny(filters []string, p string) bool`.

- [ ] **Step 1: Write the failing tests**

`internal/cli/checkout_test.go`:
```go
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type statusJSON struct {
	Repo    string `json:"repo"`
	Base    string `json:"base"`
	Draft   string `json:"draft"`
	Changes []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"changes"`
	Conflicts []string `json:"conflicts"`
}

func (h *harness) status(dir string) (statusJSON, map[string]string) {
	h.t.Helper()
	var s statusJSON
	if err := json.Unmarshal([]byte(h.ok(dir, "status", "--json")), &s); err != nil {
		h.t.Fatal(err)
	}
	m := map[string]string{}
	for _, c := range s.Changes {
		m[c.Path] = c.Status
	}
	return s, m
}

var seedFiles = map[string]string{"a.txt": "A\n", "src/x.go": "X\n", "docs/d.md": "D\n"}

func TestCheckoutFullAndSparse(t *testing.T) {
	h := newHarness(t)
	main := h.seed(seedFiles)
	root := t.TempDir()
	var res struct {
		Repo   string `json:"repo"`
		Dir    string `json:"dir"`
		Commit string `json:"commit"`
		Files  int    `json:"files"`
	}
	if err := json.Unmarshal([]byte(h.ok(root, "checkout", "web", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Commit != main || res.Repo != "web" {
		t.Fatalf("checkout json = %+v", res)
	}
	w := filepath.Join(root, "web")
	if readFile(t, filepath.Join(w, "src", "x.go")) != "X\n" || strings.TrimSpace(readFile(t, filepath.Join(w, ".sg", "base"))) != main {
		t.Fatal("checkout content or base wrong")
	}
	if s, m := h.status(w); len(m) != 0 || s.Base != main {
		t.Fatalf("fresh checkout not clean: %+v", s)
	}

	h.ok(root, "checkout", "web", "part", "--path", "src")
	if readFile(t, filepath.Join(root, "part", "src", "x.go")) != "X\n" {
		t.Fatal("sparse file missing")
	}
	if _, err := os.Stat(filepath.Join(root, "part", "a.txt")); err == nil {
		t.Fatal("file outside the sparse set was checked out")
	}
	if code, _, _ := h.run(root, "checkout", "web", "part"); code != 1 {
		t.Fatalf("checkout over an existing working copy: exit %d", code)
	}
	if code, _, _ := h.run(root, "checkout", "nope"); code != 1 {
		t.Fatalf("unknown repo: exit %d", code)
	}
}

func TestCheckoutResumes(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "A\n")
	writeFile(t, filepath.Join(w, "src", "x.go"), "WRONG half-written\n")
	writeFile(t, filepath.Join(w, "extra.txt"), "mine\n")
	h.ok(root, "checkout", "web")
	if readFile(t, filepath.Join(w, "src", "x.go")) != "X\n" || readFile(t, filepath.Join(w, "docs", "d.md")) != "D\n" {
		t.Fatal("resumed checkout content wrong")
	}
	if readFile(t, filepath.Join(w, "extra.txt")) != "mine\n" {
		t.Fatal("untracked file must be left alone")
	}
	if _, m := h.status(w); len(m) != 1 || m["extra.txt"] != "added" {
		t.Fatalf("status after resume = %v", m)
	}
}

func TestStatusReportsChangesFromAnyDirectory(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(w, "new.txt"), "new\n")
	_ = os.Remove(filepath.Join(w, "docs", "d.md"))
	for _, dir := range []string{w, filepath.Join(w, "src")} {
		_, m := h.status(dir)
		if len(m) != 3 || m["a.txt"] != "modified" || m["new.txt"] != "added" || m["docs/d.md"] != "deleted" {
			t.Fatalf("status from %s = %v", dir, m)
		}
	}
	out := h.ok(w, "status")
	if !strings.Contains(out, "modified: a.txt") || !strings.Contains(out, "new.txt") {
		t.Fatalf("text status = %q", out)
	}
	if code, _, _ := h.run(t.TempDir(), "status"); code != 1 {
		t.Fatalf("status outside a working copy: exit %d", code)
	}
}

func TestStatusAndDiffShowModeChange(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	if err := os.Chmod(filepath.Join(w, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, m := h.status(w); m["a.txt"] != "modified" {
		t.Fatalf("chmod +x not detected: %v", m)
	}
	out := h.ok(w, "diff")
	if !strings.Contains(out, "old mode 100644") || !strings.Contains(out, "new mode 100755") {
		t.Fatalf("diff = %q", out)
	}
}

func TestDiff(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "B\n")
	writeFile(t, filepath.Join(w, "new.txt"), "fresh\n")
	_ = os.Remove(filepath.Join(w, "docs", "d.md"))
	out := h.ok(w, "diff")
	for _, want := range []string{"--- a/a.txt", "+++ b/a.txt", "-A", "+B", "--- /dev/null", "+fresh", "+++ /dev/null", "-D"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff missing %q:\n%s", want, out)
		}
	}
	var js struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(h.ok(w, "diff", "--json")), &js); err != nil || js.Patch != out {
		t.Fatalf("json patch differs: %v", err)
	}
}

func TestDiffFromSubdirectory(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "B\n")
	writeFile(t, filepath.Join(w, "src", "x.go"), "Y\n")
	out := h.ok(filepath.Join(w, "src"), "diff", "x.go")
	if !strings.Contains(out, "src/x.go") || strings.Contains(out, "a.txt") {
		t.Fatalf("path-limited diff = %q", out)
	}
	if code, _, _ := h.run(filepath.Join(w, "src"), "diff", "../../outside"); code != 1 {
		t.Fatalf("path outside the working copy: exit %d", code)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/cli/ -run 'Checkout|Status|Diff'`
Expected: FAIL. The tests compile but exit 1 with `unknown command "checkout"`.

- [ ] **Step 3: Implement**

Add to the `commandTable()` slice in `internal/cli/cli.go`, after the `login` row:
```go
		{"checkout", "checkout <repo> [dir] [--path P]... [--at REV] [--server S]", cmdCheckout},
		{"status", "status", cmdStatus},
		{"diff", "diff [path...]", cmdDiff},
```

`internal/cli/checkout.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
)

const materializeGroup = 500

func cmdCheckout(ctx context.Context, a *app, args []string) error {
	const u = "checkout <repo> [dir] [--path P]... [--at REV] [--server S]"
	fs := a.newFlags("checkout")
	var paths stringList
	fs.Var(&paths, "path", "check out only this directory (repeatable; sparse checkout)")
	at := fs.String("at", "main", "revision to check out")
	server := fs.String("server", "", "server (default: from sg login)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageErr(u)
	}
	repo, dir := pos[0], pos[0]
	if len(pos) == 2 {
		dir = pos[1]
	}
	dir = a.abs(dir)
	sparse, err := wc.NormalizeSparse(paths)
	if err != nil {
		return exitf(ExitError, "%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sg", "base")); err == nil {
		return exitf(ExitError, "%s is already an sg working copy", dir)
	}
	s, err := a.connect(target{server: *server, repo: repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	rr, err := s.repo.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: repo, Rev: *at})
	if err != nil {
		return err
	}
	files, err := fetch.Tree(ctx, s.repo, repo, rr.Sha, sparse)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	w, err := wc.Create(dir, wc.Config{Server: s.server, Repo: repo, Sparse: sparse})
	if err != nil {
		return err
	}
	index, err := materialize(ctx, s.repo, repo, w.Root, files)
	if err != nil {
		return err
	}
	w.Base, w.Index = rr.Sha, index
	if err := w.Save(); err != nil {
		return err
	}
	res := map[string]any{"repo": repo, "dir": w.Root, "commit": rr.Sha, "files": len(index)}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "checked out %s@%s (%d files) into %s\n", repo, short(rr.Sha), len(index), w.Root)
	})
}

// materialize writes files under root and returns their index entries. Files
// that already hold the right content (a resumed checkout) are not rewritten.
func materialize(ctx context.Context, c sgitv1.RepoClient, repo, root string, files map[string]fetch.File) (map[string]wc.Entry, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	index := make(map[string]wc.Entry, len(files))
	for start := 0; start < len(paths); start += materializeGroup {
		chunk := paths[start:min(start+materializeGroup, len(paths))]
		var need []string
		current := map[string]bool{}
		for _, p := range chunk {
			f := files[p]
			if data, mode, err := wc.ReadLocal(root, p); err == nil && mode == f.Mode && objfmt.BlobSHA(data) == f.SHA {
				current[p] = true
				continue
			}
			need = append(need, f.SHA)
		}
		blobs, err := fetch.Blobs(ctx, c, repo, need)
		if err != nil {
			return nil, err
		}
		for _, p := range chunk {
			f := files[p]
			if !current[p] {
				if err := wc.WriteLocal(root, p, blobs[f.SHA], f.Mode); err != nil {
					return nil, err
				}
			}
			e, err := wc.StatEntry(root, p, f.Mode, f.SHA)
			if err != nil {
				return nil, err
			}
			index[p] = e
		}
	}
	return index, nil
}
```

`internal/cli/status.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
)

func cmdStatus(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("status")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("status")
	}
	w, err := openWC(a)
	if err != nil {
		return err
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	if err := w.Save(); err != nil { // persist the refreshed stat cache
		return err
	}
	type item struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	res := struct {
		Repo      string   `json:"repo"`
		Base      string   `json:"base"`
		Draft     string   `json:"draft,omitempty"`
		Changes   []item   `json:"changes"`
		Conflicts []string `json:"conflicts"`
	}{Repo: w.Config.Repo, Base: w.Base, Draft: w.Draft, Changes: []item{}, Conflicts: append([]string{}, w.Conflicts...)}
	for _, c := range changes {
		res.Changes = append(res.Changes, item{c.Path, c.Kind.String()})
	}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "On %s@%s", w.Config.Repo, short(w.Base))
		if w.Draft != "" {
			fmt.Fprintf(out, " (draft %s)", w.Draft)
		}
		fmt.Fprintln(out)
		if len(changes) == 0 && len(w.Conflicts) == 0 {
			fmt.Fprintln(out, "nothing changed")
			return
		}
		for _, c := range changes {
			fmt.Fprintf(out, "  %-9s %s\n", c.Kind.String()+":", c.Path)
		}
		for _, p := range w.Conflicts {
			fmt.Fprintf(out, "  %-9s %s\n", "conflict:", p)
		}
	})
}
```

`internal/cli/diff.go`:
```go
package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/wc"
	"github.com/pmezard/go-difflib/difflib"
)

func cmdDiff(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("diff")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	w, err := openWC(a)
	if err != nil {
		return err
	}
	var filters []string
	for _, p := range pos {
		rp, err := w.RepoPath(a.io.Dir, p)
		if err != nil {
			return exitf(ExitError, "%v", err)
		}
		filters = append(filters, rp)
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	var sel []wc.Change
	var need []string
	for _, c := range changes {
		if len(filters) > 0 && !matchesAny(filters, c.Path) {
			continue
		}
		sel = append(sel, c)
		if c.Kind != wc.Added {
			need = append(need, w.Index[c.Path].SHA)
		}
	}
	var blobs map[string][]byte
	if len(need) > 0 {
		s, err := a.connect(target{needWC: true, needRepo: true})
		if err != nil {
			return err
		}
		defer s.Close()
		if blobs, err = fetch.Blobs(ctx, s.repo, s.name, need); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	for _, c := range sel {
		e := w.Index[c.Path]
		var old, cur []byte
		if c.Kind != wc.Added {
			old = blobs[e.SHA]
		}
		if c.Kind != wc.Deleted {
			if cur, _, err = wc.ReadLocal(w.Root, c.Path); err != nil {
				return err
			}
		}
		if err := writePatch(&buf, c.Path, c.Kind, e.Mode, c.Mode, old, cur); err != nil {
			return err
		}
	}
	return a.emit(map[string]any{"patch": buf.String()}, func(out io.Writer) { out.Write(buf.Bytes()) })
}

func matchesAny(filters []string, p string) bool {
	for _, f := range filters {
		if f == "" || p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

// writePatch writes a unified diff for one change.
func writePatch(w io.Writer, p string, kind wc.Kind, oldMode, newMode string, old, cur []byte) error {
	from, to := "a/"+p, "b/"+p
	fmt.Fprintf(w, "diff --sg a/%s b/%s\n", p, p)
	switch kind {
	case wc.Added:
		from = "/dev/null"
		fmt.Fprintf(w, "new file mode %s\n", newMode)
	case wc.Deleted:
		to = "/dev/null"
		fmt.Fprintf(w, "deleted file mode %s\n", oldMode)
	default:
		if oldMode != newMode {
			fmt.Fprintf(w, "old mode %s\nnew mode %s\n", oldMode, newMode)
		}
	}
	if isBinary(old) || isBinary(cur) {
		_, err := fmt.Fprintf(w, "Binary files %s and %s differ\n", from, to)
		return err
	}
	if bytes.Equal(old, cur) {
		return nil
	}
	return difflib.WriteUnifiedDiff(w, difflib.UnifiedDiff{A: splitLines(old), B: splitLines(cur), FromFile: from, ToFile: to, Context: 3})
}

// splitLines splits text into lines that all end in "\n".
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	} else {
		lines[len(lines)-1] += "\n"
	}
	return lines
}

func isBinary(b []byte) bool { return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 }
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go mod tidy && go test -race -count=1 ./internal/cli/`
Expected: `ok`.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 7: `sg commit` (drafts), `sg drafts`, `sg show`, `sg abandon`

**Files:**
- Create: `internal/cli/commit.go`, `internal/cli/drafts.go`
- Modify: `internal/cli/cli.go` (add four rows to `commandTable`)
- Test: `internal/cli/commit_test.go`

**Interfaces:**
- Consumes:
  - Tasks 5–6 helpers.
  - Repo RPCs `CreateDraft`, `UpdateDraft`, `GetDraft`, `ListDrafts`, `AbandonDraft`, `HasObjects`, `Log` and `Diff`.
  - `rpc.UploadBlobs(ctx, c, repo string, blobs [][]byte) ([]string, error)`.
  - The `sgitv1.ChangeOp_CHANGE_OP_ADD/MODIFY/DELETE` constants.
- Produces:
  - `checkConflicts(w *wc.WC) error`. It drops resolved paths from `w.Conflicts`, and returns `ExitConflict` while any listed file still has markers.
  - `hasMarkers([]byte) bool`.
  - `uploadChanges(ctx, s *session, changes []wc.Change) ([]*sgitv1.Change, error)`.
  - `uploadMissing(ctx, s *session, paths map[string]string) error`. The map is sha → repo path in the working copy.
  - `saveDraft(ctx, s *session, changes []*sgitv1.Change, title, msg string) (*sgitv1.Draft, error)`.
  - `isStaleDraft(err error) bool`.
  - `draftIDRe`.
  - Constant `uploadFlushBytes = 32 << 20`.

- [ ] **Step 1: Write the failing tests**

`internal/cli/commit_test.go`:
```go
package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func (h *harness) draftOf(w string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(w, ".sg", "draft"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *harness) getDraft(id string) *sgitv1.Draft {
	h.t.Helper()
	return testenv.Must(h.env.Repo(h.env.AdminToken).GetDraft(context.Background(), &sgitv1.GetDraftRequest{Repo: "web", Id: id}))
}

func (h *harness) checkout(root string, extra ...string) string {
	h.t.Helper()
	h.ok(root, append([]string{"checkout", "web"}, extra...)...)
	return filepath.Join(root, "web")
}

func TestCommitCreatesAndUpdatesDraft(t *testing.T) {
	h := newHarness(t)
	main := h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(w, "new.txt"), "new\n")
	_ = os.Remove(filepath.Join(w, "docs", "d.md"))

	var res struct {
		Draft   string `json:"draft"`
		Version int    `json:"version"`
		Changes int    `json:"changes"`
	}
	if err := json.Unmarshal([]byte(h.ok(w, "commit", "-m", "first change", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 || res.Changes != 3 || h.draftOf(w) != res.Draft {
		t.Fatalf("commit = %+v, .sg/draft = %q", res, h.draftOf(w))
	}
	d := h.getDraft(res.Draft)
	files := h.env.Files("web", d.Head)
	if files["a.txt"] != "changed\n" || files["new.txt"] != "new\n" || files["docs/d.md"] != "" || d.Base != main || d.Title != "first change" {
		t.Fatalf("draft files = %v, draft = %+v", files, d)
	}
	if h.env.Files("web", "main")["a.txt"] != "A\n" {
		t.Fatal("commit without --land must not touch main")
	}
	writeFile(t, filepath.Join(w, "a.txt"), "changed again\n")
	out := h.ok(w, "commit", "-m", "second")
	if !strings.Contains(out, res.Draft) || !strings.Contains(out, "v2") {
		t.Fatalf("second commit = %q", out)
	}
	if h.env.Files("web", h.getDraft(res.Draft).Head)["a.txt"] != "changed again\n" {
		t.Fatal("draft not updated")
	}
}

func TestCommitNothingAndNoMessage(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	if code, _, errs := h.run(w, "commit", "-m", "x"); code != 1 || !strings.Contains(errs, "nothing to commit") {
		t.Fatalf("nothing: %d %q", code, errs)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "z\n")
	if code, _, _ := h.run(w, "commit"); code != 1 {
		t.Fatalf("missing -m: exit %d", code)
	}
}

func TestCommitSymlinkAndExecRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "run.sh"), "#!/bin/sh\necho hi\n")
	_ = os.Chmod(filepath.Join(w, "run.sh"), 0o755)
	if err := os.Symlink("a.txt", filepath.Join(w, "link")); err != nil {
		t.Fatal(err)
	}
	h.ok(w, "commit", "-m", "modes")
	d := h.getDraft(h.draftOf(w))
	g := h.env.Git("web")
	defer g.Close()
	files := testenv.Must(g.ListFiles(context.Background(), d.Head))
	if files["run.sh"].Mode != "100755" || files["link"].Mode != "120000" {
		t.Fatalf("modes on server = %+v", files)
	}
	root2 := t.TempDir()
	h.ok(root2, "checkout", "web", "copy", "--at", d.Head)
	fi, err := os.Stat(filepath.Join(root2, "copy", "run.sh"))
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exec bit lost: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(root2, "copy", "link")); err != nil || target != "a.txt" {
		t.Fatalf("symlink lost: %q %v", target, err)
	}
}

func TestCommitAfterDraftAbandonedElsewhere(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "one\n")
	h.ok(w, "commit", "-m", "one")
	old := h.draftOf(w)
	testenv.Must(h.env.Repo(h.env.AdminToken).AbandonDraft(context.Background(), &sgitv1.AbandonDraftRequest{Repo: "web", Id: old}))
	writeFile(t, filepath.Join(w, "a.txt"), "two\n")
	h.ok(w, "commit", "-m", "two")
	if nd := h.draftOf(w); nd == "" || nd == old {
		t.Fatalf("expected a new draft, got %q (old %q)", nd, old)
	}
}

func TestDraftsShowAbandon(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "shown\n")
	h.ok(w, "commit", "-m", "show me")
	id := h.draftOf(w)
	var list []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal([]byte(h.ok(w, "drafts", "--mine", "--json")), &list); err != nil || len(list) != 1 || list[0].ID != id || list[0].Title != "show me" {
		t.Fatalf("drafts = %+v (%v)", list, err)
	}
	if out := h.ok(w, "show", id); !strings.Contains(out, "+shown") || !strings.Contains(out, "show me") {
		t.Fatalf("show draft = %q", out)
	}
	if out := h.ok(w, "show", "main"); !strings.Contains(out, "seed") {
		t.Fatalf("show main = %q", out)
	}
	h.ok(w, "abandon")
	if h.draftOf(w) != "" || h.getDraft(id).Status != "abandoned" {
		t.Fatal("abandon must clear .sg/draft and abandon on the server")
	}
	if code, _, _ := h.run(w, "abandon"); code != 1 {
		t.Fatalf("abandon with no draft: exit %d", code)
	}
}

func TestCommitBlockedByConflictMarkers(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "x\n<<<<<<< main\nA\n=======\nB\n>>>>>>> local\n")
	writeFile(t, filepath.Join(w, ".sg", "conflicts"), "a.txt\n")
	if code, _, errs := h.run(w, "commit", "-m", "m"); code != 3 || !strings.Contains(errs, "a.txt") {
		t.Fatalf("markers: exit %d %q", code, errs)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "resolved\n")
	h.ok(w, "commit", "-m", "m")
	if _, err := os.Stat(filepath.Join(w, ".sg", "conflicts")); err == nil {
		t.Fatal(".sg/conflicts must be cleared once resolved")
	}
}

func TestCommitLargeFile(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	big := strings.Repeat("0123456789abcdef", 200<<10) // 3.2 MiB, multi-chunk upload
	writeFile(t, filepath.Join(w, "big.txt"), big)
	h.ok(w, "commit", "-m", "big")
	if h.env.Files("web", h.getDraft(h.draftOf(w)).Head)["big.txt"] != big {
		t.Fatal("big file corrupted")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/cli/ -run 'Commit|Drafts'`
Expected: FAIL with `unknown command "commit"`.

- [ ] **Step 3: Implement**

Add to `commandTable()` after the `diff` row:
```go
		{"commit", "commit -m MSG [--title T]", cmdCommit},
		{"drafts", "drafts [--mine] [--status open|landed|abandoned|all] [--repo R] [--server S]", cmdDrafts},
		{"show", "show <draft-id|rev> [--repo R] [--server S]", cmdShow},
		{"abandon", "abandon [draft-id] [--repo R] [--server S]", cmdAbandon},
```

`internal/cli/commit.go`:
```go
package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const uploadFlushBytes = 32 << 20

func cmdCommit(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("commit")
	msg := fs.String("m", "", "commit message (required)")
	title := fs.String("title", "", "draft title (default: first line of the message)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("commit -m MSG [--title T]")
	}
	if strings.TrimSpace(*msg) == "" {
		return exitf(ExitError, "a commit message is required (-m)")
	}
	s, err := a.connect(target{needWC: true, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	w := s.wc
	if err := checkConflicts(w); err != nil {
		return err
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return exitf(ExitError, "nothing to commit")
	}
	pb, err := uploadChanges(ctx, s, changes)
	if err != nil {
		return err
	}
	d, err := saveDraft(ctx, s, pb, *title, *msg)
	if err != nil {
		return err
	}
	w.Draft = d.Id
	if err := w.Save(); err != nil {
		return err
	}
	return a.emit(map[string]any{"draft": d.Id, "version": d.Version, "changes": len(pb)}, func(out io.Writer) {
		fmt.Fprintf(out, "draft %s v%d (%d changes)\n", d.Id, d.Version, len(pb))
	})
}

func hasMarkers(b []byte) bool {
	return bytes.HasPrefix(b, []byte("<<<<<<< ")) || bytes.Contains(b, []byte("\n<<<<<<< "))
}

// checkConflicts drops resolved paths from w.Conflicts and fails while any
// listed file still contains conflict markers.
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

// uploadChanges uploads local content the server lacks and returns the
// change list (the full delta of the working copy against its base).
func uploadChanges(ctx context.Context, s *session, changes []wc.Change) ([]*sgitv1.Change, error) {
	pb := make([]*sgitv1.Change, 0, len(changes))
	want := map[string]string{}
	for _, ch := range changes {
		c := &sgitv1.Change{Path: ch.Path}
		switch ch.Kind {
		case wc.Added:
			c.Op, c.BlobSha, c.Mode = sgitv1.ChangeOp_CHANGE_OP_ADD, ch.SHA, ch.Mode
			want[ch.SHA] = ch.Path
		case wc.Modified:
			c.Op, c.BlobSha, c.Mode = sgitv1.ChangeOp_CHANGE_OP_MODIFY, ch.SHA, ch.Mode
			want[ch.SHA] = ch.Path
		case wc.Deleted:
			c.Op = sgitv1.ChangeOp_CHANGE_OP_DELETE
		}
		pb = append(pb, c)
	}
	return pb, uploadMissing(ctx, s, want)
}

// uploadMissing uploads the blobs (sha -> working-copy path) the server
// does not have yet, flushing every uploadFlushBytes.
func uploadMissing(ctx context.Context, s *session, paths map[string]string) error {
	shas := make([]string, 0, len(paths))
	for sha := range paths {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	var missing []string
	for start := 0; start < len(shas); start += 1000 {
		chunk := shas[start:min(start+1000, len(shas))]
		resp, err := s.repo.HasObjects(ctx, &sgitv1.HasObjectsRequest{Repo: s.name, Shas: chunk})
		if err != nil {
			return err
		}
		for i, ok := range resp.Present {
			if !ok {
				missing = append(missing, chunk[i])
			}
		}
	}
	var batch [][]byte
	var expect []string
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		got, err := rpc.UploadBlobs(ctx, s.repo, s.name, batch)
		if err != nil {
			return err
		}
		for i := range expect {
			if i >= len(got) || got[i] != expect[i] {
				return fmt.Errorf("server stored an unexpected blob for %s", paths[expect[i]])
			}
		}
		batch, expect, size = nil, nil, 0
		return nil
	}
	for _, sha := range missing {
		data, _, err := wc.ReadLocal(s.wc.Root, paths[sha])
		if err != nil {
			return err
		}
		if objfmt.BlobSHA(data) != sha {
			return exitf(ExitError, "%s changed while committing; run the command again", paths[sha])
		}
		batch, expect, size = append(batch, data), append(expect, sha), size+len(data)
		if size >= uploadFlushBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// isStaleDraft reports whether err says the draft can no longer be updated
// (deleted, landed or abandoned elsewhere).
func isStaleDraft(err error) bool {
	switch status.Code(err) {
	case codes.NotFound:
		return true
	case codes.FailedPrecondition:
		m := status.Convert(err).Message()
		return strings.HasSuffix(m, " is landed") || strings.HasSuffix(m, " is abandoned")
	}
	return false
}

// saveDraft updates the working copy's open draft, or creates one. A stale
// .sg/draft starts a new draft.
func saveDraft(ctx context.Context, s *session, changes []*sgitv1.Change, title, msg string) (*sgitv1.Draft, error) {
	w := s.wc
	if w.Draft != "" {
		d, err := s.repo.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: s.name, Id: w.Draft, Base: w.Base, Changes: changes, Message: msg})
		if err == nil {
			return d, nil
		}
		if !isStaleDraft(err) {
			return nil, err
		}
		w.Draft = ""
	}
	return s.repo.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: s.name, Base: w.Base, Changes: changes, Title: title, Message: msg})
}
```

`internal/cli/drafts.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var draftIDRe = regexp.MustCompile(`^[a-z2-7]{8}$`)

func cmdDrafts(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("drafts")
	mine := fs.Bool("mine", false, "only my drafts")
	st := fs.String("status", "open", "open, landed, abandoned or all")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server (default: the working copy's or the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("drafts [--mine] [--status S] [--repo R] [--server S]")
	}
	want := *st
	if want == "all" {
		want = ""
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	resp, err := s.repo.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Repo: s.name, Mine: *mine, Status: want})
	if err != nil {
		return err
	}
	type item struct {
		ID      string `json:"id"`
		Version int32  `json:"version"`
		Status  string `json:"status"`
		Owner   string `json:"owner"`
		Title   string `json:"title"`
		Head    string `json:"head"`
		Updated string `json:"updated"`
	}
	items := []item{}
	for _, d := range resp.Drafts {
		items = append(items, item{d.Id, d.Version, d.Status, d.Owner, d.Title, d.Head, time.Unix(d.UpdatedAt, 0).UTC().Format(time.RFC3339)})
	}
	return a.emit(items, func(out io.Writer) {
		if len(items) == 0 {
			fmt.Fprintln(out, "no drafts")
		}
		for _, it := range items {
			fmt.Fprintf(out, "%s v%-3d %-9s %-12s %s\n", it.ID, it.Version, it.Status, it.Owner, it.Title)
		}
	})
}

func cmdShow(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("show")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("show <draft-id|rev> [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	arg := pos[0]
	if draftIDRe.MatchString(arg) {
		d, err := s.repo.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: s.name, Id: arg})
		if err == nil {
			df, err := s.repo.Diff(ctx, &sgitv1.DiffRequest{Repo: s.name, A: d.Base, B: d.Head})
			if err != nil {
				return err
			}
			res := map[string]any{"draft": d.Id, "version": d.Version, "status": d.Status, "owner": d.Owner,
				"title": d.Title, "base": d.Base, "head": d.Head, "patch": string(df.Patch)}
			return a.emit(res, func(out io.Writer) {
				fmt.Fprintf(out, "draft %s v%d (%s) by %s\n%s\nbase %s head %s\n\n%s", d.Id, d.Version, d.Status, d.Owner, d.Title, short(d.Base), short(d.Head), df.Patch)
			})
		}
		if status.Code(err) != codes.NotFound {
			return err
		}
	}
	lg, err := s.repo.Log(ctx, &sgitv1.LogRequest{Repo: s.name, Rev: arg, N: 1})
	if err != nil {
		return err
	}
	if len(lg.Entries) == 0 {
		return exitf(ExitError, "no such revision %q", arg)
	}
	e := lg.Entries[0]
	patch := ""
	if df, err := s.repo.Diff(ctx, &sgitv1.DiffRequest{Repo: s.name, A: e.Sha + "^", B: e.Sha}); err == nil {
		patch = string(df.Patch)
	}
	when := time.Unix(e.Time, 0).UTC().Format(time.RFC3339)
	res := map[string]any{"commit": e.Sha, "author": e.AuthorName, "email": e.AuthorEmail, "time": when, "subject": e.Subject, "patch": patch}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "commit %s\nAuthor: %s <%s>\nDate:   %s\n\n    %s\n\n%s", e.Sha, e.AuthorName, e.AuthorEmail, when, e.Subject, patch)
	})
}

func cmdAbandon(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("abandon")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("abandon [draft-id] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	} else if s.wc != nil {
		id = s.wc.Draft
	}
	if id == "" {
		return exitf(ExitError, "no draft to abandon")
	}
	d, err := s.repo.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: s.name, Id: id})
	if err != nil {
		return err
	}
	if s.wc != nil && s.wc.Draft == id {
		s.wc.Draft = ""
		if err := s.wc.Save(); err != nil {
			return err
		}
	}
	return a.emit(map[string]any{"draft": d.Id, "status": d.Status}, func(out io.Writer) {
		fmt.Fprintf(out, "abandoned draft %s\n", d.Id)
	})
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/cli/`
Expected: `ok`.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---
### Task 8: `sg sync` (rebase the working copy onto main)

**Files:**
- Create: `internal/cli/sync.go`
- Modify: `internal/cli/cli.go` (add the `sync` row)
- Test: `internal/cli/sync_test.go`

**Interfaces:**
- Consumes: `fetch.Tree/Blobs`, `wc.Status/WriteLocal/RemoveLocal/StatEntry`, `uploadMissing` (Task 7), and `RepoClient.MergeFile(ctx, *MergeFileRequest{Repo, BaseSha, OursSha, TheirsSha, OursLabel, TheirsLabel}) (*MergeFileResponse{Content []byte; Conflicted bool}, error)`. An empty SHA means an empty file.
- Produces:
  - `type syncResult struct{ From, To string; Updated, Merged, Conflicts, Warnings []string }`, with JSON tags `from`, `to`, `updated`, `merged`, `conflicts` and `warnings`.
  - `syncTo(ctx context.Context, s *session, target string) (syncResult, error)` moves `.sg/base` to `target`. It always calls `Save` and merges `res.Conflicts` into `w.Conflicts`.
  - `printSync(w io.Writer, r syncResult)`.
  - `writeFromTree(ctx, s *session, tree map[string]fetch.File, paths []string) error`.
- Per-path rules. `o` is the old base and `n` is the new target.
  - **Upstream unchanged** (same presence and same File in `o` and `n`): do nothing.
  - **Local unchanged:**
    - `n` has the path: write `n` and refresh the index entry.
    - `n` lacks it: remove the file and the index entry.
    - Either way the path is listed under `Updated`.
  - **Local deleted:**
    - `n` has the path: keep the deletion, set the index entry to `n` with `Size: -1`, and add a warning.
    - `n` lacks it: remove the index entry.
  - **Local content and mode equal to `n`:** set the index entry to `n` with the current stat. The two sides have converged.
  - **Local changed, `n` lacks the path:** keep the file and remove the index entry, so it becomes Added. Add a warning.
  - **Otherwise:**
    - Upload the local blob, then call `MergeFile(base = o.SHA or "", ours = n.SHA, theirs = local SHA, labels "main"/"local")`.
    - Write the result with the local mode, and set the index entry to `n` with `Size: -1`.
    - The path goes under `Conflicts` if the merge conflicted, otherwise under `Merged`.

- [ ] **Step 1: Write the failing tests**

`internal/cli/sync_test.go`:
```go
package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func (h *harness) mainSHA() string {
	h.t.Helper()
	return testenv.Must(h.env.Repo(h.env.AdminToken).ResolveRev(context.Background(), &sgitv1.ResolveRevRequest{Repo: "web", Rev: "main"})).Sha
}

// upstream lands changes on main directly through the API (as another user would).
func (h *harness) upstream(files map[string]string, deletes ...string) string {
	h.t.Helper()
	var ch []*sgitv1.Change
	for p, c := range files {
		ch = append(ch, testenv.Add(p, h.env.Upload(h.env.AdminToken, "web", []byte(c))[0]))
	}
	for _, p := range deletes {
		ch = append(ch, testenv.Delete(p))
	}
	resp := testenv.Must(h.env.Repo(h.env.AdminToken).CommitAndLand(context.Background(),
		&sgitv1.CommitAndLandRequest{Repo: "web", Base: h.mainSHA(), Changes: ch, Message: "upstream"}))
	return resp.Commit
}

type syncJSON struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Updated   []string `json:"updated"`
	Merged    []string `json:"merged"`
	Conflicts []string `json:"conflicts"`
	Warnings  []string `json:"warnings"`
}

func (h *harness) sync(dir string) (int, syncJSON) {
	h.t.Helper()
	code, out, errs := h.run(dir, "sync", "--json")
	var s syncJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		h.t.Fatalf("sync json %q (stderr %q): %v", out, errs, err)
	}
	return code, s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestSyncUpstreamOnly(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	head := h.upstream(map[string]string{"a.txt": "A2\n", "new.txt": "N\n"}, "docs/d.md")
	code, s := h.sync(w)
	if code != 0 || s.To != head || !contains(s.Updated, "a.txt") || !contains(s.Updated, "new.txt") || !contains(s.Updated, "docs/d.md") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if readFile(t, filepath.Join(w, "a.txt")) != "A2\n" || readFile(t, filepath.Join(w, "new.txt")) != "N\n" {
		t.Fatal("upstream changes not written")
	}
	if _, err := os.Stat(filepath.Join(w, "docs")); err == nil {
		t.Fatal("deleted file's now-empty directory must be pruned")
	}
	if st, m := h.status(w); len(m) != 0 || st.Base != head {
		t.Fatalf("after sync status = %+v", st)
	}
	if code, s := h.sync(w); code != 0 || len(s.Updated) != 0 {
		t.Fatalf("second sync must be a no-op: %d %+v", code, s)
	}
}

func TestSyncCleanMerge(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n4\n5\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "f.txt"), "one\n2\n3\n4\n5\n")
	h.upstream(map[string]string{"f.txt": "1\n2\n3\n4\nfive\n"})
	code, s := h.sync(w)
	if code != 0 || !contains(s.Merged, "f.txt") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if got := readFile(t, filepath.Join(w, "f.txt")); got != "one\n2\n3\n4\nfive\n" {
		t.Fatalf("merged = %q", got)
	}
	if _, m := h.status(w); m["f.txt"] != "modified" || len(m) != 1 {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncConflictWritesMarkersAndBlocksCommit(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "f.txt"), "1\nLOCAL\n3\n")
	h.upstream(map[string]string{"f.txt": "1\nUP\n3\n"})
	code, s := h.sync(w)
	if code != 3 || !contains(s.Conflicts, "f.txt") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	got := readFile(t, filepath.Join(w, "f.txt"))
	for _, want := range []string{"<<<<<<< main", "UP", "LOCAL", ">>>>>>> local"} {
		if !strings.Contains(got, want) {
			t.Fatalf("conflict file missing %q:\n%s", want, got)
		}
	}
	if st, _ := h.status(w); !contains(st.Conflicts, "f.txt") {
		t.Fatalf("status conflicts = %v", st.Conflicts)
	}
	if code, _, _ := h.run(w, "commit", "-m", "x"); code != 3 {
		t.Fatalf("commit with markers: exit %d", code)
	}
	writeFile(t, filepath.Join(w, "f.txt"), "1\nUP+LOCAL\n3\n")
	h.ok(w, "commit", "-m", "resolved")
}

func TestSyncOneSidedConflicts(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	_ = os.Remove(filepath.Join(w, "a.txt"))
	writeFile(t, filepath.Join(w, "src", "x.go"), "LOCAL X\n")
	h.upstream(map[string]string{"a.txt": "A2\n"}, "src/x.go")
	code, s := h.sync(w)
	if code != 0 || len(s.Warnings) != 2 {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if _, err := os.Stat(filepath.Join(w, "a.txt")); err == nil {
		t.Fatal("local deletion must be kept")
	}
	if readFile(t, filepath.Join(w, "src", "x.go")) != "LOCAL X\n" {
		t.Fatal("local edit of an upstream-deleted file must be kept")
	}
	if _, m := h.status(w); m["a.txt"] != "deleted" || m["src/x.go"] != "added" {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncConvergedAndAddAdd(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "same.txt"), "S\n")
	writeFile(t, filepath.Join(w, "both.txt"), "L\n")
	h.upstream(map[string]string{"same.txt": "S\n", "both.txt": "U\n"})
	code, s := h.sync(w)
	if code != 3 || !contains(s.Conflicts, "both.txt") || contains(s.Conflicts, "same.txt") {
		t.Fatalf("sync = %d %+v", code, s)
	}
	if _, m := h.status(w); m["same.txt"] != "" || m["both.txt"] != "modified" {
		t.Fatalf("status = %v", m)
	}
}

func TestSyncSparseTouchesOnlySparse(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "src")
	w := filepath.Join(root, "web")
	h.upstream(map[string]string{"src/x.go": "X2\n", "docs/d.md": "D2\n"})
	if code, _ := h.sync(w); code != 0 {
		t.Fatalf("sync exit %d", code)
	}
	if readFile(t, filepath.Join(w, "src", "x.go")) != "X2\n" {
		t.Fatal("sparse file not updated")
	}
	if _, err := os.Stat(filepath.Join(w, "docs")); err == nil {
		t.Fatal("sync wrote outside the sparse set")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/cli/ -run Sync`
Expected: FAIL. `sync --json` prints an `unknown command` error, so the JSON decode fails.

- [ ] **Step 3: Implement**

Add to `commandTable()` after the `abandon` row:
```go
		{"sync", "sync", cmdSync},
```

`internal/cli/sync.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/wc"
)

type syncResult struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Updated   []string `json:"updated"`
	Merged    []string `json:"merged"`
	Conflicts []string `json:"conflicts"`
	Warnings  []string `json:"warnings"`
}

func cmdSync(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("sync")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("sync")
	}
	s, err := a.connect(target{needWC: true, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	rr, err := s.repo.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: s.name, Rev: "main"})
	if err != nil {
		return err
	}
	res, err := syncTo(ctx, s, rr.Sha)
	if err != nil {
		return err
	}
	if err := a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "synced %s -> %s\n", short(res.From), short(res.To))
		printSync(out, res)
	}); err != nil {
		return err
	}
	if len(res.Conflicts) > 0 {
		return exitf(ExitConflict, "%d file(s) have conflicts; edit them, then commit", len(res.Conflicts))
	}
	return nil
}

func printSync(out io.Writer, r syncResult) {
	for _, p := range r.Updated {
		fmt.Fprintf(out, "  updated:  %s\n", p)
	}
	for _, p := range r.Merged {
		fmt.Fprintf(out, "  merged:   %s\n", p)
	}
	for _, p := range r.Conflicts {
		fmt.Fprintf(out, "  CONFLICT: %s\n", p)
	}
	for _, m := range r.Warnings {
		fmt.Fprintf(out, "  warning:  %s\n", m)
	}
}

func newSyncResult(from, to string) syncResult {
	return syncResult{From: from, To: to, Updated: []string{}, Merged: []string{}, Conflicts: []string{}, Warnings: []string{}}
}

// syncTo rebases the working copy from its base onto target, merging local
// changes with upstream ones (3-way merges run on the server).
func syncTo(ctx context.Context, s *session, target string) (syncResult, error) {
	w := s.wc
	res := newSyncResult(w.Base, target)
	if target == w.Base {
		return res, w.Save()
	}
	changes, err := w.Status()
	if err != nil {
		return res, err
	}
	local := make(map[string]wc.Change, len(changes))
	for _, c := range changes {
		local[c.Path] = c
	}
	oldT, err := fetch.Tree(ctx, s.repo, s.name, w.Base, w.Config.Sparse)
	if err != nil {
		return res, err
	}
	newT, err := fetch.Tree(ctx, s.repo, s.name, target, w.Config.Sparse)
	if err != nil {
		return res, err
	}
	all := map[string]bool{}
	for p := range oldT {
		all[p] = true
	}
	for p := range newT {
		all[p] = true
	}
	for p := range local {
		all[p] = true
	}
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	type merge struct {
		path, base string
		local      wc.Change
	}
	var writes []string
	var merges []merge
	for _, p := range paths {
		o, inOld := oldT[p]
		n, inNew := newT[p]
		lc, changed := local[p]
		if inOld == inNew && o == n {
			continue
		}
		if !changed {
			if inNew {
				writes = append(writes, p)
				continue
			}
			if err := wc.RemoveLocal(w.Root, p); err != nil {
				return res, err
			}
			delete(w.Index, p)
			res.Updated = append(res.Updated, p)
			continue
		}
		switch {
		case lc.Kind == wc.Deleted && inNew:
			w.Index[p] = wc.Entry{Path: p, Mode: n.Mode, SHA: n.SHA, Size: -1}
			res.Warnings = append(res.Warnings, p+": deleted locally but changed on main; keeping your deletion")
		case lc.Kind == wc.Deleted:
			delete(w.Index, p)
		case inNew && lc.SHA == n.SHA && lc.Mode == n.Mode:
			e, err := wc.StatEntry(w.Root, p, n.Mode, n.SHA)
			if err != nil {
				return res, err
			}
			w.Index[p] = e
		case !inNew:
			delete(w.Index, p)
			res.Warnings = append(res.Warnings, p+": changed locally but deleted on main; keeping your version")
		default:
			base := ""
			if inOld {
				base = o.SHA
			}
			merges = append(merges, merge{path: p, base: base, local: lc})
		}
	}
	if err := writeFromTree(ctx, s, newT, writes); err != nil {
		return res, err
	}
	res.Updated = append(res.Updated, writes...)
	sort.Strings(res.Updated)
	if len(merges) > 0 {
		want := map[string]string{}
		for _, m := range merges {
			want[m.local.SHA] = m.path
		}
		if err := uploadMissing(ctx, s, want); err != nil {
			return res, err
		}
		for _, m := range merges {
			n := newT[m.path]
			r, err := s.repo.MergeFile(ctx, &sgitv1.MergeFileRequest{
				Repo: s.name, BaseSha: m.base, OursSha: n.SHA, TheirsSha: m.local.SHA, OursLabel: "main", TheirsLabel: "local",
			})
			if err != nil {
				return res, err
			}
			if err := wc.WriteLocal(w.Root, m.path, r.Content, m.local.Mode); err != nil {
				return res, err
			}
			w.Index[m.path] = wc.Entry{Path: m.path, Mode: n.Mode, SHA: n.SHA, Size: -1}
			if r.Conflicted {
				res.Conflicts = append(res.Conflicts, m.path)
			} else {
				res.Merged = append(res.Merged, m.path)
			}
		}
	}
	w.Base = target
	seen := map[string]bool{}
	var conflicts []string
	for _, p := range append(append([]string{}, w.Conflicts...), res.Conflicts...) {
		if !seen[p] {
			seen[p] = true
			conflicts = append(conflicts, p)
		}
	}
	w.Conflicts = conflicts
	return res, w.Save()
}

// writeFromTree writes the given paths with their content from tree and
// refreshes their index entries.
func writeFromTree(ctx context.Context, s *session, tree map[string]fetch.File, paths []string) error {
	w := s.wc
	for start := 0; start < len(paths); start += materializeGroup {
		chunk := paths[start:min(start+materializeGroup, len(paths))]
		shas := make([]string, 0, len(chunk))
		for _, p := range chunk {
			shas = append(shas, tree[p].SHA)
		}
		blobs, err := fetch.Blobs(ctx, s.repo, s.name, shas)
		if err != nil {
			return err
		}
		for _, p := range chunk {
			f := tree[p]
			if err := wc.WriteLocal(w.Root, p, blobs[f.SHA], f.Mode); err != nil {
				return err
			}
			e, err := wc.StatEntry(w.Root, p, f.Mode, f.SHA)
			if err != nil {
				return err
			}
			w.Index[p] = e
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/cli/`
Expected: `ok`. Note on `TestSyncConvergedAndAddAdd`: `both.txt` was added on both sides with different content and an empty base, so `git merge-file` reports a conflict.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 9: `sg land`, `sg commit --land`, land conflicts

**Files:**
- Create: `internal/cli/land.go`
- Modify: `internal/cli/commit.go` (`--land` flag), `internal/cli/cli.go` (`land` row, `commit` usage)
- Test: `internal/cli/land_test.go`

**Interfaces:**
- Consumes:
  - `syncTo`, `printSync` (Task 8), `saveDraft`, `isStaleDraft`, `uploadChanges` (Task 7), `conflictDetail` (Task 5).
  - `RepoClient.Land(ctx, *LandRequest{Repo, Id}) (*LandResponse{Commit, DraftId}, error)`.
  - `RepoClient.CommitAndLand(ctx, *CommitAndLandRequest{Repo, DraftId, Base, Changes, Title, Message})`.
  - Server behaviour:
    - Conflict: `codes.Aborted` with a `*sgitv1.ConflictDetail{DraftId, Main, Files}` detail.
    - Main kept moving: `codes.ResourceExhausted`.
    - Changes already on main: `codes.FailedPrecondition "... changes are already on main"`.
- Produces:
  - `(*app).finishLand(ctx, s *session, resp *sgitv1.LandResponse, err error, touchWC bool) error`.
    - On success: clear `.sg/draft` and `syncTo(resp.Commit)`.
    - On a conflict: store `det.DraftId` in `.sg/draft`, call `syncTo(det.Main)`, emit the result, and return `ExitConflict`.
  - `(*app).commitAndLand(ctx, s, changes, title, msg string) error`. If the error is a stale draft, it retries once without a draft id.

- [ ] **Step 1: Write the failing tests**

`internal/cli/land_test.go`:
```go
package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hermes98761234/sgit/internal/server"
)

func TestAgentLoopCommitLand(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "fixed\n")
	out := h.ok(w, "commit", "-m", "fix", "--land")
	if !strings.Contains(out, "landed") {
		t.Fatalf("output = %q", out)
	}
	if h.env.Files("web", "main")["a.txt"] != "fixed\n" {
		t.Fatal("change not on main")
	}
	if st, m := h.status(w); len(m) != 0 || st.Base != h.mainSHA() || h.draftOf(w) != "" {
		t.Fatalf("after land: status %+v draft %q", st, h.draftOf(w))
	}
}

func TestLandRebasesAndBringsUpstreamFiles(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	h.upstream(map[string]string{"u.txt": "U\n"})
	writeFile(t, filepath.Join(w, "a.txt"), "mine\n")
	h.ok(w, "commit", "-m", "mine", "--land")
	files := h.env.Files("web", "main")
	if files["a.txt"] != "mine\n" || files["u.txt"] != "U\n" {
		t.Fatalf("main = %v", files)
	}
	if readFile(t, filepath.Join(w, "u.txt")) != "U\n" {
		t.Fatal("upstream file not brought into the working copy")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestTwoAgentsConflictThenResolve(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n"})
	bob := h.env.NewUser("bob", "web", "write")
	h.loginAs("bob", bob)
	wa := h.checkout(t.TempDir())
	rootB := t.TempDir()
	h.okAs("bob", rootB, "checkout", "web")
	wb := filepath.Join(rootB, "web")

	writeFile(t, filepath.Join(wa, "f.txt"), "1\nA-change\n3\n")
	writeFile(t, filepath.Join(wa, "a-only.txt"), "from A\n")
	h.ok(wa, "commit", "-m", "A", "--land")

	writeFile(t, filepath.Join(wb, "f.txt"), "1\nB-change\n3\n")
	code, out, errs := h.runAs("bob", wb, "commit", "-m", "B", "--land")
	if code != 3 || !strings.Contains(out+errs, "conflict") {
		t.Fatalf("B land: exit %d\n%s\n%s", code, out, errs)
	}
	got := readFile(t, filepath.Join(wb, "f.txt"))
	if !strings.Contains(got, "<<<<<<< main") || !strings.Contains(got, "A-change") || !strings.Contains(got, "B-change") {
		t.Fatalf("conflict file = %q", got)
	}
	if readFile(t, filepath.Join(wb, "a-only.txt")) != "from A\n" {
		t.Fatal("non-conflicting upstream file must arrive with the conflict sync")
	}
	draft := h.draftOf(wb)
	if draft == "" {
		t.Fatal("draft id must be kept after a conflict")
	}
	if code, _, _ := h.runAs("bob", wb, "commit", "-m", "B", "--land"); code != 3 {
		t.Fatalf("land with markers still present: exit %d", code)
	}
	writeFile(t, filepath.Join(wb, "f.txt"), "1\nA-change+B-change\n3\n")
	h.okAs("bob", wb, "commit", "-m", "B resolved", "--land")
	if h.env.Files("web", "main")["f.txt"] != "1\nA-change+B-change\n3\n" {
		t.Fatal("resolved content not on main")
	}
	if h.draftOf(wb) != "" || h.getDraft(draft).Status != "landed" {
		t.Fatal("the same draft must have landed and been cleared")
	}
}

func TestSgLandAfterCommit(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	if code, _, _ := h.run(w, "land"); code != 1 {
		t.Fatalf("land without a draft: exit %d", code)
	}
	writeFile(t, filepath.Join(w, "a.txt"), "via land\n")
	h.ok(w, "commit", "-m", "draft first")
	h.ok(w, "land")
	if h.env.Files("web", "main")["a.txt"] != "via land\n" || h.draftOf(w) != "" {
		t.Fatal("sg land did not land the working copy's draft")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status = %v", m)
	}
}

func TestLandEditsAfterCommitSurvive(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"f.txt": "1\n2\n3\n4\n5\n"})
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "f.txt"), "one\n2\n3\n4\n5\n")
	h.ok(w, "commit", "-m", "v1")
	writeFile(t, filepath.Join(w, "f.txt"), "one\n2\n3\n4\nfive\n") // edited after the commit
	h.ok(w, "land")
	if h.env.Files("web", "main")["f.txt"] != "one\n2\n3\n4\n5\n" {
		t.Fatal("land must land the committed version")
	}
	if readFile(t, filepath.Join(w, "f.txt")) != "one\n2\n3\n4\nfive\n" {
		t.Fatal("uncommitted local edit lost")
	}
	if _, m := h.status(w); m["f.txt"] != "modified" {
		t.Fatalf("status = %v", m)
	}
}

func TestLandExhaustedExit4(t *testing.T) {
	var h *harness
	var n atomic.Int32
	h = newHarness(t, func(c *server.Config) {
		c.LandRetries = 2
		c.BeforeCAS = func(repo string) {
			h.env.CommitDirect(repo, map[string]string{fmt.Sprintf("ext%d.txt", n.Add(1)): "x"}, "external")
		}
	})
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "never lands\n")
	if code, _, errs := h.run(w, "commit", "-m", "x", "--land"); code != 4 {
		t.Fatalf("exit %d, want 4 (%s)", code, errs)
	}
	if _, err := os.Stat(filepath.Join(w, "a.txt")); err != nil {
		t.Fatal("working copy must be untouched")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/cli/ -run 'Land|Agent'`
Expected: FAIL (`flag provided but not defined: -land`, `unknown command "land"`).

- [ ] **Step 3: Implement**

In `internal/cli/cli.go`, replace the `commit` row and add a `land` row after it:
```go
		{"commit", "commit -m MSG [--title T] [--land]", cmdCommit},
		{"land", "land [draft-id] [--repo R] [--server S]", cmdLand},
```

In `internal/cli/commit.go`, make three edits to `cmdCommit`:

1. After the `title := ...` line, add:
```go
	land := fs.Bool("land", false, "land on main right away (rebase on the server)")
```
2. Change the usage string in `cmdCommit` to `"commit -m MSG [--title T] [--land]"`.
3. Replace the block that starts at `d, err := saveDraft(ctx, s, pb, *title, *msg)` and runs to the end of the function with:
```go
	if *land {
		return a.commitAndLand(ctx, s, pb, *title, *msg)
	}
	d, err := saveDraft(ctx, s, pb, *title, *msg)
	if err != nil {
		return err
	}
	w.Draft = d.Id
	if err := w.Save(); err != nil {
		return err
	}
	return a.emit(map[string]any{"draft": d.Id, "version": d.Version, "changes": len(pb)}, func(out io.Writer) {
		fmt.Fprintf(out, "draft %s v%d (%d changes)\n", d.Id, d.Version, len(pb))
	})
}
```

`internal/cli/land.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

func cmdLand(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("land")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("land [draft-id] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	} else if s.wc != nil {
		id = s.wc.Draft
	}
	if id == "" {
		return exitf(ExitError, "no draft to land (commit first, or pass a draft id)")
	}
	touch := s.wc != nil && s.wc.Config.Repo == s.name && s.wc.Draft == id
	resp, err := s.repo.Land(ctx, &sgitv1.LandRequest{Repo: s.name, Id: id})
	return a.finishLand(ctx, s, resp, err, touch)
}

// commitAndLand creates/updates the working copy's draft and lands it in one
// round trip. A stale .sg/draft is dropped and the call retried once.
func (a *app) commitAndLand(ctx context.Context, s *session, changes []*sgitv1.Change, title, msg string) error {
	w := s.wc
	req := &sgitv1.CommitAndLandRequest{Repo: s.name, DraftId: w.Draft, Base: w.Base, Changes: changes, Title: title, Message: msg}
	resp, err := s.repo.CommitAndLand(ctx, req)
	if err != nil && req.DraftId != "" && isStaleDraft(err) {
		req.DraftId, w.Draft = "", ""
		resp, err = s.repo.CommitAndLand(ctx, req)
	}
	return a.finishLand(ctx, s, resp, err, true)
}

// finishLand updates the working copy after Land/CommitAndLand. touchWC is
// false when the landed draft does not belong to this working copy.
func (a *app) finishLand(ctx context.Context, s *session, resp *sgitv1.LandResponse, err error, touchWC bool) error {
	w := s.wc
	if err != nil {
		det := conflictDetail(err)
		if det == nil || w == nil || !touchWC {
			return err
		}
		w.Draft = det.DraftId
		if err := w.Save(); err != nil {
			return err
		}
		res, err := syncTo(ctx, s, det.Main)
		if err != nil {
			return err
		}
		if err := a.emit(map[string]any{"landed": false, "draft": det.DraftId, "main": det.Main, "sync": res}, func(out io.Writer) {
			fmt.Fprintf(out, "conflict: draft %s does not apply cleanly to main %s; working copy rebased onto it\n", det.DraftId, short(det.Main))
			printSync(out, res)
		}); err != nil {
			return err
		}
		return exitf(ExitConflict, "landing conflicted: fix the conflicted files, then run `sg commit -m MSG --land` again")
	}
	res := newSyncResult("", resp.Commit)
	if w != nil && touchWC {
		w.Draft = ""
		if res, err = syncTo(ctx, s, resp.Commit); err != nil {
			return err
		}
	}
	if err := a.emit(map[string]any{"landed": true, "commit": resp.Commit, "draft": resp.DraftId, "sync": res}, func(out io.Writer) {
		fmt.Fprintf(out, "landed %s (draft %s)\n", short(resp.Commit), resp.DraftId)
		printSync(out, res)
	}); err != nil {
		return err
	}
	if len(res.Conflicts) > 0 {
		return exitf(ExitConflict, "landed, but your uncommitted edits conflict with main in %d file(s)", len(res.Conflicts))
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/cli/`
Expected: `ok`.

How `TestTwoAgentsConflictThenResolve` works:
- After the conflict, `.sg/base` is A's landed commit, and `.sg/draft` is bob's draft.
- The second `commit --land` sends the full delta against that new base (`CommitAndLand` with `DraftId` set). The server updates the same draft and lands it on the fast path.

- [ ] **Step 5: Verify**

Run: `gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./...`
Expected: clean.

---

### Task 10: `sg log`, `sg blame`, `sg cat`, `sg sparse`, README

**Files:**
- Create: `internal/cli/history.go`, `internal/cli/sparse.go`
- Modify: `internal/cli/cli.go` (add five rows), `README.md` (add a CLI section)
- Test: `internal/cli/history_test.go`

**Interfaces:**
- Consumes:
  - RPCs: `RepoClient.Log(*LogRequest{Repo, Rev, Path, N}) → LogResponse{Entries []*LogEntry{Sha, AuthorName, AuthorEmail, Time, Subject}}` and `Blame(*BlameRequest{Repo, Rev, Path}) → BlameResponse{Lines []*BlameLine{Sha, Author, Line, Text []byte}}`.
  - Fetch and working copy: `fetch.Tree/Blobs`, `materialize`, `wc.NormalizeSparse/InSparse/RemoveLocal/Status`.
- Produces:
  - `(*app).repoPath(s *session, arg string) (string, error)`. Inside the matching working copy the path is relative to the cwd; otherwise it is a cleaned repo path.
  - The commands `log`, `blame`, `cat`, `sparse list|add|rm`.

- [ ] **Step 1: Write the failing tests**

`internal/cli/history_test.go`:
```go
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogBlameCatFromSubdirectory(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "src", "x.go"), "X\nmore\n")
	h.ok(w, "commit", "-m", "extend x", "--land")
	src := filepath.Join(w, "src")

	var log []struct {
		Sha     string `json:"sha"`
		Author  string `json:"author"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(h.ok(src, "log", "x.go", "--json")), &log); err != nil || len(log) != 2 || log[0].Subject != "extend x" || log[1].Subject != "seed" {
		t.Fatalf("log x.go = %+v (%v)", log, err)
	}
	var blame []struct {
		Sha  string `json:"sha"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(h.ok(src, "blame", "x.go", "--json")), &blame); err != nil || len(blame) != 2 || blame[1].Text != "more" || blame[1].Sha != log[0].Sha {
		t.Fatalf("blame = %+v (%v)", blame, err)
	}
	if got := h.ok(src, "cat", "main:src/x.go"); got != "X\nmore\n" {
		t.Fatalf("cat = %q", got)
	}
	outside := t.TempDir()
	if err := json.Unmarshal([]byte(h.ok(outside, "log", "--repo", "web", "--json")), &log); err != nil || len(log) != 3 {
		t.Fatalf("log --repo = %+v (%v)", log, err)
	}
	if got := h.ok(outside, "cat", "--repo", "web", ":a.txt"); got != "A\n" {
		t.Fatalf("cat main default = %q", got)
	}
	if code, _, _ := h.run(outside, "cat", "--repo", "web", "main:nope.txt"); code != 1 {
		t.Fatalf("cat missing: exit %d", code)
	}
	if out := h.ok(w, "log", "-n", "1"); !strings.Contains(out, "extend x") || strings.Contains(out, "seed") {
		t.Fatalf("log -n 1 = %q", out)
	}
}

func TestSparseAddRm(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	root := t.TempDir()
	h.ok(root, "checkout", "web", "--path", "src")
	w := filepath.Join(root, "web")
	var list []string
	if err := json.Unmarshal([]byte(h.ok(w, "sparse", "list", "--json")), &list); err != nil || len(list) != 1 || list[0] != "src" {
		t.Fatalf("list = %v (%v)", list, err)
	}
	h.ok(w, "sparse", "add", "docs")
	if readFile(t, filepath.Join(w, "docs", "d.md")) != "D\n" {
		t.Fatal("sparse add did not fetch docs")
	}
	if _, m := h.status(w); len(m) != 0 {
		t.Fatalf("status after add = %v", m)
	}
	writeFile(t, filepath.Join(w, "docs", "d.md"), "edited\n")
	if code, _, _ := h.run(w, "sparse", "rm", "docs"); code != 1 {
		t.Fatalf("rm with local changes: exit %d", code)
	}
	writeFile(t, filepath.Join(w, "docs", "d.md"), "D\n")
	h.ok(w, "sparse", "rm", "docs")
	if _, err := os.Stat(filepath.Join(w, "docs")); err == nil {
		t.Fatal("sparse rm must remove the files")
	}
	if code, _, _ := h.run(w, "sparse", "rm", "src"); code != 1 {
		t.Fatalf("removing the last sparse path: exit %d", code)
	}
	if code, _, _ := h.run(w, "sparse", "rm", "nope"); code != 1 {
		t.Fatalf("removing a non-sparse path: exit %d", code)
	}
	full := h.checkout(t.TempDir())
	if code, _, _ := h.run(full, "sparse", "add", "src"); code != 1 {
		t.Fatalf("sparse add on a full checkout: exit %d", code)
	}
}

func TestNotLoggedInAndReadOnly(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	h.cfg["nobody"] = t.TempDir()
	if code, _, errs := h.runAs("nobody", t.TempDir(), "log", "--repo", "web"); code != 2 || !strings.Contains(errs, "not logged in") {
		t.Fatalf("not logged in: exit %d %q", code, errs)
	}
	rita := h.env.NewUser("rita", "web", "read")
	h.loginAs("rita", rita)
	root := t.TempDir()
	h.okAs("rita", root, "checkout", "web")
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "nope\n")
	if code, _, _ := h.runAs("rita", w, "commit", "-m", "x"); code != 2 {
		t.Fatalf("read-only commit: exit %d, want 2", code)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/cli/ -run 'Log|Sparse|NotLoggedIn'`
Expected: FAIL (`unknown command "log"`).

- [ ] **Step 3: Implement**

Add to `commandTable()` after the `sync` row:
```go
		{"log", "log [path] [-n N] [--rev REV] [--repo R] [--server S]", cmdLog},
		{"blame", "blame <path> [--rev REV] [--repo R] [--server S]", cmdBlame},
		{"cat", "cat <rev>:<path> [--repo R] [--server S]", cmdCat},
		{"sparse", "sparse list | add <path>... | rm <path>...", cmdSparse},
```

`internal/cli/history.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
)

// repoPath resolves a path argument: relative to the cwd inside the matching
// working copy, otherwise taken as a repo path.
func (a *app) repoPath(s *session, arg string) (string, error) {
	if s.wc != nil && s.wc.Config.Repo == s.name {
		p, err := s.wc.RepoPath(a.io.Dir, arg)
		if err != nil {
			return "", exitf(ExitError, "%v", err)
		}
		return p, nil
	}
	p := strings.Trim(path.Clean(filepath.ToSlash(arg)), "/")
	if p == "." {
		p = ""
	}
	return p, nil
}

func cmdLog(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("log")
	n := fs.Int("n", 20, "number of commits")
	rev := fs.String("rev", "main", "revision to start from")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("log [path] [-n N] [--rev REV] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	p := ""
	if len(pos) == 1 {
		if p, err = a.repoPath(s, pos[0]); err != nil {
			return err
		}
	}
	resp, err := s.repo.Log(ctx, &sgitv1.LogRequest{Repo: s.name, Rev: *rev, Path: p, N: int32(*n)})
	if err != nil {
		return err
	}
	type item struct {
		Sha     string `json:"sha"`
		Author  string `json:"author"`
		Email   string `json:"email"`
		Time    string `json:"time"`
		Subject string `json:"subject"`
	}
	items := []item{}
	for _, e := range resp.Entries {
		items = append(items, item{e.Sha, e.AuthorName, e.AuthorEmail, time.Unix(e.Time, 0).UTC().Format(time.RFC3339), e.Subject})
	}
	return a.emit(items, func(out io.Writer) {
		for _, it := range items {
			fmt.Fprintf(out, "%s %s %-12s %s\n", short(it.Sha), it.Time[:10], it.Author, it.Subject)
		}
	})
}

func cmdBlame(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("blame")
	rev := fs.String("rev", "main", "revision")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("blame <path> [--rev REV] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	p, err := a.repoPath(s, pos[0])
	if err != nil {
		return err
	}
	resp, err := s.repo.Blame(ctx, &sgitv1.BlameRequest{Repo: s.name, Rev: *rev, Path: p})
	if err != nil {
		return err
	}
	type item struct {
		Sha    string `json:"sha"`
		Author string `json:"author"`
		Line   int32  `json:"line"`
		Text   string `json:"text"`
	}
	items := []item{}
	for _, l := range resp.Lines {
		items = append(items, item{l.Sha, l.Author, l.Line, string(l.Text)})
	}
	return a.emit(items, func(out io.Writer) {
		for _, it := range items {
			fmt.Fprintf(out, "%s (%-12s %4d) %s\n", short(it.Sha), it.Author, it.Line, it.Text)
		}
	})
}

// cmdCat prints a file at a revision. The path is repo-relative (git
// "<rev>:<path>" syntax); an empty rev means main.
func cmdCat(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("cat")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !strings.Contains(pos[0], ":") {
		return usageErr("cat <rev>:<path> [--repo R] [--server S]")
	}
	rev, p, _ := strings.Cut(pos[0], ":")
	if rev == "" {
		rev = "main"
	}
	p = strings.Trim(path.Clean(p), "/")
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	rr, err := s.repo.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: s.name, Rev: rev})
	if err != nil {
		return err
	}
	tree, err := fetch.Tree(ctx, s.repo, s.name, rr.Sha, []string{p})
	if err != nil {
		return err
	}
	f, ok := tree[p]
	if !ok {
		return exitf(ExitError, "%s: no such file at %s", p, rev)
	}
	blobs, err := fetch.Blobs(ctx, s.repo, s.name, []string{f.SHA})
	if err != nil {
		return err
	}
	data := blobs[f.SHA]
	return a.emit(map[string]any{"path": p, "sha": f.SHA, "mode": f.Mode, "content": data}, func(out io.Writer) { out.Write(data) })
}
```

`internal/cli/sparse.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/wc"
)

func cmdSparse(ctx context.Context, a *app, args []string) error {
	const u = "sparse list | add <path>... | rm <path>..."
	if len(args) == 0 {
		return usageErr(u)
	}
	sub := args[0]
	fs := a.newFlags("sparse " + sub)
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	w, err := openWC(a)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		list := append([]string{}, w.Config.Sparse...)
		return a.emit(list, func(out io.Writer) {
			if len(list) == 0 {
				fmt.Fprintln(out, "(full checkout)")
			}
			for _, p := range list {
				fmt.Fprintln(out, p)
			}
		})
	case "add":
		if len(pos) == 0 {
			return usageErr(u)
		}
		if len(w.Config.Sparse) == 0 {
			return exitf(ExitError, "this is a full checkout; there is nothing to add")
		}
		var adds []string
		for _, arg := range pos {
			rp, err := w.RepoPath(a.io.Dir, arg)
			if err != nil || rp == "" {
				return exitf(ExitError, "invalid sparse path %q", arg)
			}
			if !wc.InSparse(w.Config.Sparse, rp) {
				adds = append(adds, rp)
			}
		}
		added := 0
		if len(adds) > 0 {
			fresh, err := wc.NormalizeSparse(adds)
			if err != nil {
				return exitf(ExitError, "%v", err)
			}
			s, err := a.connect(target{needWC: true, needRepo: true})
			if err != nil {
				return err
			}
			defer s.Close()
			files, err := fetch.Tree(ctx, s.repo, s.name, w.Base, fresh)
			if err != nil {
				return err
			}
			idx, err := materialize(ctx, s.repo, s.name, w.Root, files)
			if err != nil {
				return err
			}
			for p, e := range idx {
				w.Index[p] = e
			}
			added = len(idx)
			if w.Config.Sparse, err = wc.NormalizeSparse(append(w.Config.Sparse, fresh...)); err != nil {
				return err
			}
		}
		if err := w.Save(); err != nil {
			return err
		}
		return a.emit(map[string]any{"sparse": w.Config.Sparse, "files_added": added}, func(out io.Writer) {
			fmt.Fprintf(out, "sparse set: %v (%d files added)\n", w.Config.Sparse, added)
		})
	case "rm":
		if len(pos) == 0 {
			return usageErr(u)
		}
		remove := map[string]bool{}
		for _, arg := range pos {
			rp, err := w.RepoPath(a.io.Dir, arg)
			if err != nil || !slices.Contains(w.Config.Sparse, rp) {
				return exitf(ExitError, "%s is not a sparse path (see `sg sparse list`)", arg)
			}
			remove[rp] = true
		}
		var remaining []string
		for _, p := range w.Config.Sparse {
			if !remove[p] {
				remaining = append(remaining, p)
			}
		}
		if len(remaining) == 0 {
			return exitf(ExitError, "cannot remove the last sparse path (an empty set would mean a full checkout)")
		}
		changes, err := w.Status()
		if err != nil {
			return err
		}
		for _, c := range changes {
			if !wc.InSparse(remaining, c.Path) {
				return exitf(ExitError, "uncommitted change in %s; commit or revert it first", c.Path)
			}
		}
		removed := 0
		for _, p := range w.SortedPaths() {
			if !wc.InSparse(remaining, p) {
				if err := wc.RemoveLocal(w.Root, p); err != nil {
					return err
				}
				delete(w.Index, p)
				removed++
			}
		}
		w.Config.Sparse = remaining
		if err := w.Save(); err != nil {
			return err
		}
		return a.emit(map[string]any{"sparse": remaining, "files_removed": removed}, func(out io.Writer) {
			fmt.Fprintf(out, "sparse set: %v (%d files removed)\n", remaining, removed)
		})
	}
	return usageErr(u)
}
```

In `README.md`, replace the `> **Status:** ...` line with:
```markdown
> **Status:** phase 1 (server and Docker) and phase 2 (`sg` CLI) are implemented. The daemon (`sgd`) and GitHub mirroring are next. See the [design spec](docs/superpowers/specs/2026-10-14-sgit-design.md).
```
and insert this section before `## Run without Docker`:
````markdown
## Use the CLI

```bash
make build                                    # builds dist/sg and dist/sg-server
sg login sg.example.com:7419 --token "$TOKEN" --ca cert.pem
sg checkout web                               # or: --path src --path docs  (sparse)
cd web
# edit files…
sg status
sg diff
sg commit -m "fix the thing" --land           # one round trip: draft + rebase + land
```

- **There is no `sg add`.** Every new file that isn't ignored by the root `.gitignore` goes into the next commit.
- **`sg commit` without `--land`** saves a draft, which is kept in `.sg/draft`. `sg land` lands it later, and `sg drafts`, `sg show <id>` and `sg abandon` manage drafts.
- **`sg sync`** rebases your working copy onto the latest `main`. The 3-way merges run on the server.
- **Conflicts.** When a land or a sync conflicts, sg writes conflict markers, lists the files in `sg status`, and exits with code 3. Fix the files and run `sg commit -m … --land` again.
- **Reading history.** `sg log`, `sg blame` and `sg cat <rev>:<path>` work with no checkout at all if you pass `--repo`.
- **Machine-readable output.** Every command accepts `--json`.

| Exit code | Meaning |
|---|---|
| 0 | ok |
| 1 | error |
| 2 | not logged in, bad token, or no permission |
| 3 | conflict |
| 4 | `main` kept moving; retry |
````

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -race -count=1 ./internal/cli/`
Expected: `ok`.

- [ ] **Step 5: Final verification**

Run:
```bash
gofmt -l . ; go vet ./... && staticcheck ./... && go test -race -count=1 ./... && make build && ls dist/
```
Expected: clean, all packages `ok`, and `dist/` containing `sg` and `sg-server`.
