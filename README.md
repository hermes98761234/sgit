# sgit

Scion-style source control on top of git:

- Thin, always-online clients with no `.git`.
- Commits and lands run on the server, with compare-and-swap on `main`.
- A shared object cache on each machine for parallel agents.
- Bare git repos on the server are the source of truth.

Inspired by Anthropic's [Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control).

> **Status:** phases 1–3 (server and Docker, `sg` CLI, `sgd` cache daemon) are implemented. GitHub mirroring and admin commands are next. See the [design spec](docs/superpowers/specs/2026-10-14-sgit-design.md).

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

## Run the cache daemon (`sgd`)

`sgd` keeps a per-user disk cache of git objects in front of one server, so several working copies and agents on the same machine share fetched objects.

```bash
sgd --upstream sg.example.com:7419 --ca cert.pem    # or run it as a systemd user service: contrib/sgd.service
```

- `--upstream` and `--ca` default to `$SGD_UPSTREAM` and `$SGD_CA`.
- `sg` uses `sgd` automatically when it fronts the server you are logged in to. Otherwise `sg` connects directly.
- `sg` prints one warning and connects directly whenever a socket path exists but cannot be used: sgd does not answer, the path is not a socket, or its ownership or permissions are unsafe. If there is no socket, or sgd fronts a different server, `sg` connects directly silently.
- To bypass the daemon, set `SG_NO_DAEMON=1`.
- **Socket:** `$XDG_RUNTIME_DIR/sg/sgd.sock`, mode 0600, one daemon per user. The path can be changed with `--socket` (sgd only) or `$SG_SOCKET` (sg and sgd). `sg` refuses to use a socket, or socket directory, that you do not own or that others can write to.
- sgd refuses to start if something other than a socket already exists at the socket path.
- sgd creates the socket directory with mode 0700 if it does not exist, never changes an existing one, and refuses to start if it is group- or world-writable or not yours.
- **Cache:** stored in the user cache directory (`$XDG_CACHE_HOME/sg/objects`, usually `~/.cache/sg/objects`; override with `--cache-dir`), with a default size limit of 10 GiB (`--cache-max 10G`) and least-recently-used eviction. Every object is checked against its hash when stored and again each time it is served.
- **Security:**
  - Every request still runs with your own token.
  - Before serving cached objects for a repo, sgd re-checks your read access at least every 60 seconds.
  - Objects cached for one repo can be served for another when requested by their hash. This is acceptable within a trusted team, as spec §7 describes.

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
