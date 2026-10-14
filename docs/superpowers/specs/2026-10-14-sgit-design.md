# sgit — Scion-style source control on top of git

**Date:** 2026-10-14
**Status:** Approved design, pending implementation plan
**Inspiration:** [Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control) (Anthropic's Scion)

## 1. Goal

Bring Scion's core ideas to teams that keep git as their storage:

- **Always-online thin clients.** A working copy holds files only, with no `.git`. Objects are fetched on demand.
- **Server-side writes.** Commits, rebases, merges and ref moves run on the server. Commits are cheap and there's nothing to push.
- **Few refs.** `main` and `release/*` are the only branches. Drafts live under hidden refs.
- **Shared per-machine cache.** One daemon serves many parallel agents or jobs on a node.
- **Source control as an API.** Log, blame and cat work without any checkout.
- **Agents as first-class users.** Parallel lands use compare-and-swap with retry and no global merge-queue lock. Conflicts are returned in a machine-actionable form.

Git (bare repos plus plumbing commands) is the source of truth on the server only. Clients never run git.

### Target deployment

A small team: one server, many machines, per-user API tokens, TLS, and per-repo read/write ACLs. `main` is mirrored asynchronously to a GitHub remote, so GitHub Pages and Actions keep working.

### Non-goals (v1)

These are out of scope for v1: per-directory ACLs, a review UI, virtualized (FUSE) working copies, regional cache tier, multi-server or HA, Postgres, and Windows support.

## 2. Architecture

```
 agents / humans
      │  sg (CLI)  — working dir: files + .sg/ (metadata, no .git)
      ▼  unix socket (gRPC)
 sgd (node daemon, 1 per machine)
      │  object cache (content-addressed, LRU, disk) + token passthrough
      ▼  gRPC over TLS
 sg-server
      ├─ auth (tokens → user → per-repo read/write, admin role)
      ├─ repo service (reads: resolve rev, tree/blob, log, blame, diff)
      ├─ write service (blobs, drafts, land with CAS, merge-file)
      ├─ mirror worker (async push main → GitHub)
      └─ <data-dir>/repos/<name>.git   (bare repos — source of truth)
          + <data-dir>/sg.db            (SQLite: users, tokens, ACLs, drafts, mirror state)
```

- There is one Go module, `github.com/hermes98761234/sgit`, with three binaries: `sg`, `sgd` and `sg-server`.
- The daemon and server implement the **same gRPC `Repo` service**. `sg` connects to `sgd` when its socket exists. Otherwise it connects **directly to the server** and prints a one-line warning, which suits CI and laptops.
- The server requires **git ≥ 2.40**, for `merge-tree --write-tree --merge-base`.

### Package layout (indicative)

```
cmd/sg, cmd/sgd, cmd/sg-server
proto/sgit/v1/repo.proto          gRPC service + messages
internal/gitx        git plumbing wrappers (cat-file pool, hash-object, mktree/index, commit-tree, merge-tree, update-ref, merge-file, log, blame)
internal/server      gRPC handlers, write path, land/CAS loop
internal/store       SQLite: users, tokens, acl, drafts, mirror state
internal/auth        token hashing, interceptor, ACL check (single Authorize function)
internal/mirror      async push worker
internal/daemon      cache, singleflight, pass-through proxy
internal/cache       content-addressed disk LRU
internal/wc          working copy: .sg/ config, base, index, status, sparse, atomic writes
internal/client      dial logic (sgd socket → server fallback), credentials
internal/cli         command implementations, --json output, exit codes
```

## 3. Data model

- **Repo.** A bare git repo plus a DB row (name, mirror URL, created_at). Refs are `refs/heads/main` and optional `refs/heads/release/*`.
- **Object IDs.** Git SHA-1 object IDs are used as-is (content-addressed and immutable).
- **Change list.** This is how a client describes a change: `base` (commit SHA) plus `[{path, op: add|modify|delete, blob_sha, mode}]`. It is relative to the parent, Scion-style. Blob contents are uploaded first with `UploadBlobs`.
- **Draft.**
  - The ref `refs/sg/drafts/<id>/head` points to the latest draft commit. Every revision is kept as `refs/sg/drafts/<id>/v<n>`. Git refs are files, so `<id>` itself can't also be a ref.
  - The DB row holds: id, repo, owner, title, base, head, version, and status (`open|landed|abandoned`), plus `landed_as` (the commit on main).
  - `<id>` is a short random base32 string.
- **Land.** The draft is rebased onto the current main as a **single linear commit**: no merge commits, with main as the only parent. Main then advances with a compare-and-swap.

### Working copy `.sg/`

| File | Contents |
|---|---|
| `.sg/config` | Server address, repo, sparse path list (TOML) |
| `.sg/base` | Commit SHA the working copy is based on |
| `.sg/index` | Per tracked path: size, mtime, mode, blob SHA (binary or JSON lines) |
| `.sg/draft` | Current open draft id (optional) |
| `.sg/conflicts` | Paths with unresolved conflict markers (optional) |

`sg status` compares stat data with the index and hashes only the files whose stat changed. Hashing uses git's blob format (`blob <len>\0<data>`, SHA-1), so the IDs match the server's. Every `.sg/` write is atomic (write to a temp file, then rename).

## 4. CLI (`sg`)

```
sg login <server> [--token T]           store token in ~/.config/sg/credentials (0600)
sg checkout <repo> [dir] [--path P]... [--at <rev>]
sg status | sg diff [path...]
sg sync                                  rebase working copy onto latest main
sg commit -m MSG [--draft ID] [--land]   create/update draft; --land also lands it
sg land [ID]                             land a draft (default: current)
sg drafts [--mine] | sg show <draft|rev> | sg abandon <ID>
sg log [path] [-n N] | sg blame <path> | sg cat <rev>:<path>   (accept --repo, no checkout needed)
sg sparse add|rm|list <path>
sg repo create|import <name> [--from URL] [--mirror URL] | sg repo status <name>   (admin)
sg token create <user> [--admin] | sg token revoke <id>                            (admin)
sg acl grant|revoke <user> <repo> read|write                                        (admin)
```

- **One open draft per working copy.** It's recorded in `.sg/draft`, and each `sg commit` updates it until it lands or is abandoned. After a successful land, `.sg/base` becomes the new main commit and `.sg/draft` is cleared.
- **`--json` on every command** gives machine-readable output.
- **Exit codes:**
  - `0` ok
  - `1` generic error (NotFound, InvalidArgument, …)
  - `2` auth or permission error
  - `3` conflict
  - `4` CAS retries exhausted

### Agent loop

```
sg checkout web --path src/      # seconds: only needed trees/blobs
# edit files
sg commit -m "fix" --land        # exit 0 → landed; exit 3 → conflict markers written, fix & rerun
```

### `sg sync`

1. The client resolves the latest main.
2. For each locally modified path that also changed upstream, it sends `(base, local, upstream)` to the `MergeFile` RPC. The server runs `git merge-file`, so the client has no git logic.
3. A clean result is written to the file. A conflict writes the content with markers and adds the path to `.sg/conflicts`.
4. Files changed only upstream are updated directly.
5. `.sg/base` moves to the new main.
6. `sg commit` refuses to run while `.sg/conflicts` still contains a path whose file has markers.

### Conflict on land

1. The server returns `Aborted` with a `ConflictDetail` listing paths and base/ours/theirs blob SHAs.
2. The client runs the same per-file merge as `sg sync`, writes markers, records `.sg/conflicts`, and exits with code 3.
3. The draft stays open.

## 5. gRPC API (`sgit.v1.Repo`)

All RPCs carry `authorization: Bearer <token>` metadata, and all are served by both `sgd` and `sg-server`.

| RPC | Kind | Notes |
|---|---|---|
| `ResolveRev(repo, rev) → sha` | read | Always forwarded fresh to the server for refs. Full SHAs resolve locally. |
| `GetObjects(repo, shas[]) → stream Object{sha, type, zstd_data}` | read | Streamed and resumable (client re-requests missing). Cached by sgd. |
| `UploadBlobs(repo, stream BlobChunk) → shas[]` | write | Each blob is streamed in ≤1 MiB chunks, with `end_of_blob` marking its last one. Shas come back in upload order. Writing an existing blob is a no-op. |
| `HasObjects(repo, shas[]) → bool[]` | read | |
| `CreateDraft(repo, base, changes[], title, message) → Draft` | write | |
| `UpdateDraft(repo, id, base, changes[], message) → Draft` | write | New version ref |
| `Land(repo, id) → {commit}` / `ConflictDetail` | write | See §6 |
| `CommitAndLand(...)` | write | Create or update the draft, then land, in one round trip |
| `ListDrafts`, `GetDraft`, `AbandonDraft` | meta | |
| `Log(repo, rev, path, n)`, `Blame(repo, rev, path)`, `Diff(repo, a, b, paths)` | read | Run on the server, uncached |
| `MergeFile(base, ours, theirs blob shas) → {content, conflicted}` | compute | |
| Separate service `sgit.v1.Admin`: `WhoAmI` (any user), and `CreateRepo`, `ImportRepo`, `RepoStatus`, `CreateToken`, `RevokeToken`, `GrantACL`, `RevokeACL` | admin | Admin role required (except `WhoAmI`). `sg` always dials the server directly for Admin calls, never `sgd`. |

**Fetch protocol (Scion-style).**
1. The client starts from a commit's root tree.
2. It checks its local store for missing tree SHAs and requests them in batches, recursing only into sparse paths.
3. It does the same for blobs.

There's no "have/want" negotiation and no packfile. An interrupted fetch simply re-requests whatever is still missing.

## 6. Server write path

1. **Upload.** `UploadBlobs` runs `git hash-object -w --stdin` for each blob, or `--stdin-paths` in batches.
2. **Build the draft commit.**
   1. Use a temporary `GIT_INDEX_FILE` and run `git read-tree <base>`.
   2. Run `git update-index --index-info` with the change list, then `git write-tree`.
   3. Run `git commit-tree <tree> -p <base> -m <msg>` with author and committer set from the token's user.
   4. Run `git update-ref refs/sg/drafts/<id>/head <commit>` and write `refs/sg/drafts/<id>/v<n>`.

   Change semantics: `add`/`modify` upsert the path. `delete` of a path that isn't a file in `base` is `InvalidArgument`. A file can replace a directory, or the reverse. A change list is always the **full** delta of the working copy against `base`, never incremental against the previous draft version. An empty change list is `InvalidArgument`.
3. **Land.** This is a retry loop, at most 5 attempts.
   1. `main_old := rev-parse main`.
   2. If `draft.base == main_old`, use `tree := draft tree`. Otherwise run `git merge-tree --write-tree --merge-base=<draft.base> <main_old> <draft.head>`.
   3. If merge-tree exits 1, parse the conflicted file list, then return `Aborted` with `ConflictDetail`.
   4. Run `git commit-tree <tree> -p <main_old>`, keeping the draft's message and author.
   5. Run `git update-ref refs/heads/main <new> <main_old>`. If it fails because main moved, go to step 1.
   6. On success, mark the draft `landed`, set `landed_as`, and enqueue a mirror job.
   7. If all attempts fail, return `ResourceExhausted`.
4. **Per-repo lock.** An in-process mutex is held around steps 3.1–3.5. Correctness comes from the CAS in `update-ref`; the mutex only cuts wasted retries.
5. **Read pool.** Each repo keeps N long-running `git cat-file --batch` processes. They're restarted on error and their stderr is logged.
6. **Validation.** Paths must be relative, normalized and free of `..`. Absolute paths and any `.git/` component are rejected. Limits are max changes per request (default 10k) and max blob size (default 100 MB).

## 7. Daemon (`sgd`)

- **Socket.** Listens on `$XDG_RUNTIME_DIR/sg/sgd.sock`, or `/run/sg/sgd.sock` as a system service, with mode 0660.
- **Cache.** `GetObjects` and `HasObjects` are served from `~/.cache/sg/objects/<2>/<38>`, where each file holds the zstd payload exactly as received from the server.
  - Eviction is LRU by atime-equivalent bookkeeping, with a size cap (default 10 GB).
  - There's no invalidation, because objects are immutable.
- **Dedup.** Concurrent requests for the same SHA are fetched once, with singleflight per SHA.
- **Pass-through.** Every other RPC, `ResolveRev` for refs included, is forwarded to the server with the caller's token.
- **Security.**
  - The daemon stores no tokens.
  - The **first** request for a repo on each connection is forwarded to the server as an ACL check, `HasObjects` with an empty list. The result is cached for 60 s per token hash and repo.
  - Cached objects are shared between all users on the node. That's acceptable for the trusted-team deployment model, and it's documented in the README.

### Phase 3 implementation notes

- **Socket.** sgd runs as one daemon per user. The socket is mode 0600, inside a directory that must be owned by the user and not group- or world-writable (sgd creates it 0700 if missing), and sg checks that ownership before connecting. This replaces the 0660 system socket described above.
- **`Daemon/Info`.** This RPC reports the upstream sgd fronts. sg calls it without a token and uses the daemon only when the upstream matches its own server.
- **Which RPCs use the cache.** `HasObjects` and `ResolveRev` always go to the server. Only `GetObjects` is served from the cache: every object is checked on the way in and on every read, and in-flight fetches are deduplicated per (repo, sha).

## 8. Mirror and import

- **Import.** `sg repo import <name> --from <git-url>` runs `git clone --bare --single-branch` (main plus release branches) into the data dir.
- **Mirror.**
  - Each repo can have an optional `mirror_url`. Every land enqueues a job.
  - A worker goroutine per repo runs `git push --force-with-lease=main:<last_mirrored> <mirror_url> main`.
  - Failed pushes retry with exponential backoff, capped at 5 min.
  - `last_mirrored`, `last_error` and `pending` are stored in SQLite and shown by `sg repo status`.
- **Credentials.** Mirror push credentials come from the server's environment (for example a git credential helper or an SSH key). sgit never stores them.

## 9. Auth

- **Tokens.** A token is `sgt_` plus 32 random bytes in base32. The DB stores only its SHA-256.
- **Bootstrap.** `sg-server init` creates the data dir and DB, then prints an initial admin token once.
- **Roles.** `admin` is global. `read` and `write` are per repo, and `write` implies `read`.
- **Authorize.** A single function, `Authorize(user, repo, action) error`, makes every permission decision. Per-directory ACLs can be added later inside it.
- **TLS.** `--tls-cert`/`--tls-key` are required by default. `--dev-insecure` allows plaintext only when the listen address is loopback.

## 10. Error handling

- **gRPC codes to exit codes:**
  - `Unauthenticated` / `PermissionDenied` → 2
  - `Aborted` + `ConflictDetail` → 3
  - `ResourceExhausted` (CAS) → 4
  - everything else → 1
- **Server errors.** Git stderr is captured into structured logs (slog JSON) with a request ID. The client gets a short message plus that request ID.
- **Idempotency.** `checkout` and `sync` can be resumed: files already matching the index are skipped, and `.sg/` is written atomically at the end of each phase.
- **Daemon down.** `sg` falls back to dialing the server directly, with one warning to stderr.

## 11. Testing

- **Unit tests:**
  - change list → index-info translation
  - merge-tree conflict output parser
  - wc index/status (stat fast path, hash path)
  - path validation
  - Authorize
  - cache LRU and singleflight
  - exit-code mapping
- **Integration tests** use real git in `t.TempDir()`, with in-process `sg-server` and `sgd` on random ports and sockets. The CLI is driven via its Go entrypoint.
  - checkout → edit → `commit --land` → log shows the commit
  - sparse checkout fetches only the requested subtree, asserted by counting objects
  - two working copies edit the same line → the second land exits 3 with markers → resolve → land succeeds
  - 50 goroutines land disjoint changes concurrently → all present on main, history linear
  - `sg sync` merges upstream changes cleanly and with conflicts
  - mirror pushes to a local bare "GitHub" repo, and recovers after the remote is made temporarily unavailable
  - auth: no token, wrong token, read-only token attempting land, non-admin admin call
  - daemon: second checkout served from cache with zero server `GetObjects`; daemon down → fallback works
- **Benchmarks.** Concurrent lands/sec, and cold vs. warm checkout of a generated repo with 10k files.
- **CI.** GitHub Actions runs `go vet`, `staticcheck` and `go test -race ./...` with git ≥ 2.40.

## 12. Delivery phases

All four phases fall under one spec and one plan, and are implemented in order. Each phase ends green.

1. **Server core.** gitx, store, auth, Repo RPCs (read + write + drafts + land/CAS), TLS, `sg-server init/serve`.
2. **CLI.** login, checkout (sparse), status, diff, commit/land, sync, drafts/show/abandon, log/blame/cat, all connecting directly to the server.
3. **Daemon.** cache, singleflight, pass-through, socket discovery and fallback.
4. **Mirror + admin.** import, mirror worker, `repo/token/acl` commands, `repo status`.

## 13. Docker (server deployment)

The supported way to run `sg-server` is a container. The CLI and daemon stay native binaries.

- **`Dockerfile`** (multi-stage):
  1. `golang:1.23-alpine` builds `sg-server` (plus `sg` for admin use inside the container) with `CGO_ENABLED=0`.
  2. The runtime image is `alpine:3.20` with `git` (≥ 2.40), `openssh-client` and `ca-certificates`.
  3. It runs as non-root user `sg` (uid 10001).
- **`VOLUME /data`** holds the bare repos, `sg.db` and the TLS material under `/data/tls/`.
- **`EXPOSE 7419`**: the gRPC port.
- **Entrypoint (`docker-entrypoint.sh`):**
  - If `/data/sg.db` is missing, it runs `sg-server init`. The initial admin token is printed to logs once and also written to `/data/admin-token` (mode 0600).
  - If `SG_TLS_CERT`/`SG_TLS_KEY` are unset and `SG_TLS_SELF_SIGNED=1`, it generates a self-signed certificate for the hostnames in `SG_TLS_HOSTS` with the server's built-in `sg-server gen-cert` helper (Go `crypto/x509`, no openssl).
  - It then runs `exec sg-server serve`.
- **Config via env vars**, each mirroring a CLI flag:
  - `SG_LISTEN` (default `:7419`), `SG_DATA_DIR` (`/data`)
  - `SG_TLS_CERT`, `SG_TLS_KEY`, `SG_TLS_SELF_SIGNED`, `SG_TLS_HOSTS`
  - `SG_LOG_LEVEL`, `SG_MAX_BLOB_MB`
- **Mirror credentials.** Mount an SSH key at `/home/sg/.ssh/` (read-only), or set `GIT_ASKPASS`/a credential helper through env. Nothing is baked into the image.
- **Healthcheck.** `sg-server health` calls the standard gRPC health service on localhost. It's wired into a Docker `HEALTHCHECK`.
- **`docker-compose.yml`** has one `sg-server` service, a named volume `sgdata`, port `7419:7419`, and the env shown above. The README walks through `docker compose up -d`, then `docker compose exec sg-server cat /data/admin-token`, then `sg login`.
- **Image publishing.** A GitHub Actions workflow builds multi-arch images (amd64 + arm64) and pushes them to `ghcr.io/hermes98761234/sgit-server` on tags `v*`.
- **Integration test.** CI builds the image, starts it with `SG_TLS_SELF_SIGNED=1`, and runs a smoke test from the host: login, `repo create`, checkout, `commit --land`, `log`.

Docker work is delivered at the end of **Phase 1** (server image and compose) and finished in **Phase 4** (mirror creds docs, ghcr workflow).

## 14. Tooling

- Go 1.23+, gRPC + protobuf (generated code committed), `modernc.org/sqlite` (pure Go, no cgo), and `klauspost/compress/zstd`.
- The repo is public on GitHub: `hermes98761234/sgit`, MIT license.
