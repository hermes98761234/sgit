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
- **Administration.** `sg token`, `sg acl` and `sg repo` manage users, permissions and repos; see [Administration](#administration).
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

`sg repo list` shows MIRROR as `broken` when a repo itself cannot be read; `sg repo status` then prints an `error:` line.

The server image ships `sg` too, so you can administer from inside the container:

```bash
docker compose exec sg-server sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)"'
docker compose exec sg-server sg repo list
```

## Importing and mirroring

`sg repo import web --from https://github.com/acme/web.git` creates `web` from the source's `main` and `release/*` branches. Other branches and tags are not copied, and the source must have a `main` branch.

`--mirror URL` on `repo create` or `repo import` makes the server push `main` to that URL after every land, in the background. Each push uses `--force-with-lease=main:<last mirrored commit>`:

- **The mirror is never overwritten.** If someone changes the mirror's `main` outside sgit, the push is refused and `sg repo status` shows the error. To resume, reset the mirror's `main` to the `last:` commit that `sg repo status` prints. Mirroring then resumes at the next retry (within 5 minutes), the next land, or a server restart.
- **A new mirror must be empty** (no `main` branch). The exception is the URL you imported from (the same URL, exactly): `sg repo import web --from URL --mirror URL` mirrors back to the source.
- **Failed pushes retry automatically,** with exponential backoff up to 5 minutes. A server restart catches up any missed pushes.
- **Allowed URLs** are `https://`, `ssh://` and `user@host:path`. A URL containing credentials, a query string or a fragment is rejected, because it would be stored in the server database.
- **An interrupted import leaves nothing behind,** so the name can be reused.

### Mirror credentials

The server runs git with an empty global config, so credential helpers in `~/.gitconfig` are not used. Imports from the same host reuse the mirror credentials. Choose one of the following.

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

ssh never prompts (the server runs it with `BatchMode=yes`), so `known_hosts` must be pre-populated. Setting `GIT_SSH_COMMAND` or `GIT_SSH` in the server environment overrides this.

**HTTPS token.** The image includes `sg-git-askpass`, which answers git's prompts from the environment, but only for one host:

```yaml
services:
  sg-server:
    environment:
      GIT_ASKPASS: /usr/local/bin/sg-git-askpass
      SG_MIRROR_TOKEN_FILE: /run/secrets/github_token   # or SG_MIRROR_TOKEN: <token>
      # SG_MIRROR_HOST: github.com                    # the only host that gets the token
    secrets: [github_token]
secrets:
  github_token:
    file: ./github_token   # fine-grained token with Contents: read and write
```

Then use `--mirror https://github.com/acme/web.git`. The username defaults to `x-access-token`; set `SG_MIRROR_USER` to change it. The helper answers only for `SG_MIRROR_HOST` (default `github.com`) and refuses every other host, so the token is never sent to an import source on another host. The token file is bind-mounted with its host permissions, so make it readable by uid 10001 (`sudo chown 10001 github_token && chmod 600 github_token`).

### Releases

Pushing a tag `vX.Y.Z` runs the tests, then publishes `ghcr.io/hermes98761234/sgit-server:X.Y.Z`, `:X.Y` and `:latest` for linux/amd64 and linux/arm64. Only release tags move `latest`: prerelease tags such as `v1.0.0-rc1`, and tags that are not semver, do not.

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
