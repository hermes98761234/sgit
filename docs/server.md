# Server guide (`sg-server`)

[← README](../README.md)

## Configuration

The server listens on `:7419` (gRPC over TLS). The data directory (`/data` in Docker, held in the `sgdata` volume) holds the bare repos, `sg.db`, `admin-token` and `tls/`.

| Variable | Default | Meaning |
|---|---|---|
| `SG_DATA_DIR` | `/data` | repos, `sg.db`, `admin-token`, `tls/` |
| `SG_LISTEN` | `:7419` | listen address |
| `SG_TLS_CERT` / `SG_TLS_KEY` | — | PEM certificate and key |
| `SG_TLS_SELF_SIGNED` | `0` | `1` = generate a self-signed certificate on first start |
| `SG_TLS_HOSTS` | `localhost,127.0.0.1` | SANs for the self-signed certificate |
| `SG_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `SG_MAX_BLOB_MB` | `100` | max size of one file |

TLS options:
- With `SG_TLS_SELF_SIGNED=1`, a certificate for `SG_TLS_HOSTS` is generated into `/data/tls/cert.pem`. Clients pin it with `sg login --ca cert.pem`.
- To use your own certificate instead, set `SG_TLS_CERT` and `SG_TLS_KEY` and mount the files.

## Run without Docker

Requires Go and git ≥ 2.40.

```bash
make build
./dist/sg-server init --data ./sgdata                  # prints the initial admin token
./dist/sg-server gen-cert --hosts sg.example.com --out-cert sgdata/cert.pem --out-key sgdata/key.pem
./dist/sg-server serve --data ./sgdata --tls-cert sgdata/cert.pem --tls-key sgdata/key.pem
```

`--dev-insecure` serves plaintext. It is only allowed on a loopback address.

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
sg repo delete web --yes                      # see "Deleting" below
sg user list                                  # NAME EMAIL ADMIN TOKENS CREATED
sg user promote bob                           # make bob a global admin
sg user demote bob
sg user delete bob
sg token list [bob]                           # ID USER CREATED STATUS (active|revoked)
sg acl list [--user bob] [--repo web]         # USER REPO LEVEL
```

- `--admin` and `--email` take effect only when `token create` creates the user.
- `sg token create` prints only the token on stdout, so `TOKEN=$(sg token create bob)` works. The token id goes to stderr.
- `sg repo list` shows MIRROR as `broken` when a repo itself cannot be read. `sg repo status` then prints an `error:` line.

- `sg user list` shows TOKENS as the number of active (not revoked) tokens. `ADMIN` is `yes` or `no`.
- `sg token list` never prints token secrets; the server stores only their hashes. REVOKED timestamps appear in `--json` output as `revoked`.
- `sg acl list` shows explicit grants only. Admins have implicit access to every repo and do not appear in it.
- `sg user demote` and `sg user delete` fail (exit 1) if they would leave the server with no admin. You cannot delete yourself.
- All of these accept `--json` and `--server`.

### Deleting

- **`sg user delete <user>`** revokes all the user's tokens, removes their ACLs, abandons their open drafts and removes them from `sg user list`. It is a soft delete: the database row stays, because landed and abandoned drafts still refer to the owner. The name is freed by renaming the row to `<name>~deleted-<id>` (`~` is not a valid user name character), so a later `sg token create <name>` makes a brand-new user with no old access. Draft history shows the renamed owner. Landed git history is not touched.
- **`sg repo delete <name> --yes`** refuses without `--yes` and does not contact the server. With it, the server removes the repo's ACLs, drafts and mirror state from the database and moves the bare repo to `<data>/deleted/<name>.<unixtime>.git` instead of erasing it. An operator can restore it by moving the directory back to `<data>/repos/<name>.git` and re-creating the repo row (or delete it for good with `rm -rf`). The name can be reused straight away. The delete runs under the repo's land lock, so it never interleaves with a land; lands and draft operations that lose the race fail. The GitHub (or other) mirror remote is never contacted, so its content stays.
- Existing databases are upgraded automatically on start: the `users` table gains a nullable `deleted_at` column.

The server image ships `sg` too, so you can administer from inside the container:

```bash
docker compose exec sg-server sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)"'
docker compose exec sg-server sg repo list
```

## Importing and mirroring

`sg repo import web --from https://github.com/acme/web.git` creates `web` from the source's `main` and `release/*` branches. Other branches and tags are not copied, and the source must have a `main` branch. An interrupted import leaves nothing behind, so the name can be reused.

`--mirror URL` on `repo create` or `repo import` makes the server push `main` to that URL after every land, in the background. Each push uses `--force-with-lease=main:<last mirrored commit>`:

- **The mirror is never overwritten.** If someone changes the mirror's `main` outside sgit, the push is refused and `sg repo status` shows the error. To resume, reset the mirror's `main` to the `last:` commit that `sg repo status` prints. Mirroring resumes at the next retry (within 5 minutes), the next land, or a server restart.
- **A new mirror must be empty** (no `main` branch); `repo create` and `repo import` refuse one that is not. The exception is the exact URL you imported from: `sg repo import web --from URL --mirror URL` mirrors back to the source.
- **Failed pushes retry automatically,** with exponential backoff up to 5 minutes. A server restart catches up any missed pushes.
- **Allowed URLs** are `https://`, `ssh://` and `user@host:path`. A URL containing credentials, a query string or a fragment is rejected, because it would be stored in the server database.

### Mirror credentials

The server never stores credentials. It also runs git with an empty global config, so credential helpers in `~/.gitconfig` are not used. Imports from the same host reuse the mirror credentials.

**SSH deploy key.**
1. Add a deploy key with write access to the GitHub repo.
2. Put the key and a `known_hosts` file in a directory:
   ```bash
   mkdir mirror-ssh && ssh-keygen -t ed25519 -N '' -f mirror-ssh/id_ed25519
   ssh-keyscan github.com > mirror-ssh/known_hosts      # check it against GitHub's published fingerprints
   sudo chown -R 10001 mirror-ssh && chmod 600 mirror-ssh/id_ed25519
   ```
3. Mount that directory read-only at `/home/sg/.ssh`:
   ```yaml
       volumes:
         - sgdata:/data
         - ./mirror-ssh:/home/sg/.ssh:ro
   ```
4. Use `--mirror git@github.com:acme/web.git`.

ssh never prompts, because the server runs it with `BatchMode=yes`, so `known_hosts` must be filled in beforehand. Setting `GIT_SSH_COMMAND` or `GIT_SSH` in the server environment overrides this.

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

Then use `--mirror https://github.com/acme/web.git`.
- The username defaults to `x-access-token`. Set `SG_MIRROR_USER` to change it.
- The helper refuses every host except `SG_MIRROR_HOST`, so the token is never sent to an import source on another host.
- The token file is bind-mounted with its host permissions, so make it readable by uid 10001: `sudo chown 10001 github_token && chmod 600 github_token`.

## Releases

Pushing a tag `vX.Y.Z` runs the tests, then publishes `ghcr.io/hermes98761234/sgit-server:X.Y.Z`, `:X.Y` and `:latest` for linux/amd64 and linux/arm64. Only release tags move `latest`. Prerelease tags such as `v1.0.0-rc1`, and tags that are not semver, do not.

The same tag creates a GitHub release with `sg`, `sgd` and `sg-server` archives for linux and macOS (amd64 and arm64), plus `SHA256SUMS`.

## Development

```bash
make build   # dist/sg, dist/sgd, dist/sg-server
make test    # go test -race ./...
make gen     # regenerate protobuf code (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
make smoke   # build the image and run the end-to-end test against it
```
