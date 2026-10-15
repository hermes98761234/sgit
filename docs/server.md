# Server guide (`sg-server`)

[← README](../README.md)

## Configuration

The server listens on `:7419` (gRPC over TLS). The data directory (`/data` in Docker, held in the `sgdata` volume) holds the bare repos (`repos/`), the derived project repos (`projects/`), `sg.db` (mode 0600: it holds token hashes and the user list), `admin-token`, `tls/` and `sg.lock`. A running server holds a lock on `sg.lock`, so a second `sg-server serve` on the same data directory exits with `another sg-server is already using data dir …`. On a filesystem without locks the server logs a warning and runs unlocked; if it cannot set `sg.db` to 0600 (the files belong to another user) it logs a warning and starts anyway.

| Variable | Default | Meaning |
|---|---|---|
| `SG_DATA_DIR` | `/data` in Docker, `./sgdata` otherwise | repos, projects, `sg.db`, `admin-token`, `tls/` |
| `SG_LISTEN` | `:7419` | listen address |
| `SG_TLS_CERT` / `SG_TLS_KEY` | — | PEM certificate and key |
| `SG_TLS_SELF_SIGNED` | `0` | Docker image only: `1` = generate a self-signed certificate on first start |
| `SG_TLS_HOSTS` | `localhost,127.0.0.1` | SANs for the self-signed certificate (Docker image, and the default of `gen-cert --hosts`) |
| `SG_DEV_INSECURE` | `0` | `1` = plaintext, like `serve --dev-insecure` (loopback listen address only); also read by `health` |
| `SG_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `SG_MAX_BLOB_MB` | `100` | max size of one file |

TLS options:
- In the Docker image, `SG_TLS_SELF_SIGNED=1` generates a certificate for `SG_TLS_HOSTS` into `/data/tls/cert.pem` (the image's entrypoint does this, not `sg-server` itself). Clients pin it with `sg login --ca cert.pem`. Without Docker, make one with `sg-server gen-cert` (see below).
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

Repos are admin-only. Users get access to **projects**, which mount folders of repos (see [Projects](#projects)). A typical onboarding:

```bash
sg repo create web
sg project create web
sg project mount web . web:.                  # the whole repo at the project root
sg token create bob --email bob@example.com   # prints the token once; give it to bob
sg acl grant bob web write                    # web is the project; or: read
```

The other commands:

```bash
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
sg acl list [--user bob] [--project web]      # USER PROJECT LEVEL
```

- `--admin` and `--email` take effect only when `token create` creates the user.
- `sg token create` prints only the token on stdout, so `TOKEN=$(sg token create bob)` works. The token id goes to stderr.
- `sg repo list` shows MIRROR as `broken` when a repo itself cannot be read. `sg repo status` then prints an `error:` line.

- `sg user list` shows TOKENS as the number of active (not revoked) tokens. `ADMIN` is `yes` or `no`.
- `sg token list` never prints token secrets; the server stores only their hashes. REVOKED timestamps appear in `--json` output as `revoked`.
- `sg acl grant` and `sg acl revoke` take a project (name, alias or `#<id>`) and print its canonical name. Access is one `read` or `write` level per user and project; `write` implies `read`. `sg acl list` shows the project's canonical name. `sg acl revoke` for a user without a grant says `<user> had no explicit access to <project>; nothing revoked` (exit 0; `--json` has `"revoked": false`).
- `sg acl list` shows explicit grants only. Admins have implicit access to every project and do not appear in it.
- `sg user demote` and `sg user delete` fail (exit 1) if they would leave the server with no admin. You cannot delete yourself.
- All of these accept `--json` and `--server`, before or after the command (`sg --server host:port repo list` or `sg repo list --server host:port`).

### Projects

A **project** is what users see. It has an id, a unique name, optional aliases, and its **mounts**. A new project has no mounts and no commits until the first `mount`. A mount maps a path in the project to `<repo>:<folder>`. Clients never learn repo names or source paths. They do not learn source commit SHAs either, with one exception: when a project's first mount is a whole repo at `.`, its project commits are byte-identical copies of the source commits, so they keep the source SHAs. That holds from the start of the history up to the first commit with any header besides `tree`, `parent`, `author` and `committer` (a signature such as `gpgsig`, `gpgsig-sha256` or `mergetag`, an `encoding` header, or any other) or with a second parent; that commit and every later one get new SHAs, because each commit names its parent. Repos made and landed through sgit have none of these, so all their SHAs match. Such a project shows the whole repo anyway; use folder mounts when the source SHAs must stay private.

```bash
sg project create site                        # created project site (id 1)
sg project mount site public web:public       # mounted web:public at public/ in site
sg project mount site lib core:lib --read-only
sg project unmount site lib                   # unmounted lib/ from site
sg project alias add site www                 # added alias www for site
sg project alias rm www                       # removed alias www (was site)
sg project rename site portal                 # renamed site to portal (site is kept as an alias)
sg project list                               # NAME ALIASES MOUNTS
sg project show portal                        # id, aliases, head, mounts, grants
sg project delete portal --yes                # deleted project portal (N drafts, M grants removed)
```

For a project `portal` (alias `site`) with the two mounts `lib` and `public`, `sg project list` prints one line per project. MOUNTS lists `path=repo:folder`, with `(ro)` for a read-only mount, and `-` stands for none:

```
NAME    ALIASES  MOUNTS
portal  site     lib=core:lib(ro),public=web:public
```

`sg project show` prints:

```
project: portal (id 1)
aliases: site
head:    1856bd9
mounts:
  lib/  core:lib  read-only
  public/  web:public
grants:
  bob  write
```

With `--json`, `create`, `mount`, `unmount`, `alias`, `rename` and `show` print `{"id", "name", "aliases", "head", "mounts": [{"path", "repo", "folder", "read_only"}], "grants": [{"user", "level"}]}`; `list` prints `{"projects": [...]}` of the same objects; `delete` prints `{"project", "drafts_removed", "acls_removed"}`.

**References.** Every command that takes a project accepts its name, an alias, or `#<id>` (for example `#1`). A bare number is a name, never an id. Names and aliases share one namespace and follow the repo-name rule `^[a-z0-9][a-z0-9._-]{0,63}$`. An unknown project gives `project "<name>" not found or no access` (exit 2).

**Mount rules.**
- The mount path is a clean relative path (`src`, `tools/build`) or `.` for the project root. The folder is a clean repo-relative path, or `.` for the repo root.
- Mount paths must not nest inside each other. `.` is allowed only as the project's single mount.
- A mount may point at a folder that does not exist yet. It shows up empty until the folder is created.
- Mounting an existing path again with the same `repo:folder` only sets the read-only flag (on with `--read-only`, off without it) and creates no commit. A different source for an existing path is refused; unmount it first.
- Unmounting the last mount leaves the project with its history and an empty tree. Mount a folder again to give it content, or delete the project.

**History.**
- The first mount of a project imports the history of that folder: every first-parent `main` commit that touched it becomes a project commit, with the same author, dates and message. Only the `tree`, `parent`, `author` and `committer` headers are kept: signatures (`gpgsig`, `gpgsig-sha256`, `mergetag`), the `encoding` header, any other extra header and second parents are dropped, so even a `.` mount gives such commits new SHAs. A long history takes a while: `sg project mount` waits up to 30 minutes, and the server finishes the import even if the client gives up (bounded at one hour). If the import fails, the project is left as it was before the mount. A server shutdown stops a running import after the usual 5 s grace period; the part already imported is kept and the import resumes on the project's next request.
- Later mounts and unmounts are **forward only**. Each one adds one layout commit on top of the project head, "Project layout: added `<path>/`" or "Project layout: removed `<path>/`", authored by the acting admin. Older history of a newly mounted folder is not imported. Changing `--read-only` creates no commit.
- Commits landed in a mounted folder, through any project or directly in the repo, show up in the project on its next request. Commits from several repos are ordered by land order.
- A land that touches mounts of several repos makes one project commit per repo, in land order, each with the land's message, so `sg log` shows it once per repo.

**Read-only mounts** refuse writes from everyone, admins included (`read-only path: <path>`, exit 2).

**Aliases and rename.** An alias is an extra name for the same project. `sg project rename <old> <new>` keeps the old name as an alias, so existing references keep working. Renaming to one of the project's own aliases swaps the two. Working copies store the project id, so renames and aliases never break them.

**Drafts that predate a mount change.** A draft cannot land into a mount that did not exist at the draft's base, or that now points at a different repo or folder than it did then. The check compares the mount's source at the base with its source now, so a mount removed and added again with the same `repo:folder` does not block the draft. It fails with `draft <id> was made before mount <path>/ was added; sync and commit again` (or `… changed; …`). A draft that touches an unmounted path fails with `path outside project: <path>`.

**Lands across repos.** A land that touches mounts of several repos lands one repo at a time, in repo-name order. On the first conflict or error it stops. Repos that already landed stay landed, and the client gets a partial-land conflict (exit 3) and retries only the rest.

**The project repo is a cache.** Each project has its own bare repo at `<data>/projects/<id>.git`, with its own object store (no alternates, no shared packs). The repos in `<data>/repos/` stay the source of truth. To rebuild a project repo, stop the server, delete `<data>/projects/<id>.git` and start it again. The next request rebuilds it with identical commit SHAs, so working copies keep working. Open drafts of that project are abandoned, because their objects lived only in the deleted repo.

**Accepted limits.**
- Commit messages, authors and dates are copied as written. A commit that touched a mounted folder and another folder shows its full message to project users.
- A land across several repos is not atomic (see above).
- A GitHub mirror publishes its whole source repo, outside the project boundary. Mirrors are admin-only.

### Deleting

- **`sg user delete <user>`** revokes all the user's tokens, removes their ACLs, abandons their open drafts and removes them from `sg user list`. It is a soft delete: the database row stays, because landed and abandoned drafts still refer to the owner. The name is freed by renaming the row to `<name>~deleted-<id>` (`~` is not a valid user name character), so a later `sg token create <name>` makes a brand-new user with no old access. Draft history shows the renamed owner. Landed git history is not touched.
- **`sg project delete <project> --yes`** refuses without `--yes`. With it, the server removes the project's aliases, mounts, grants and drafts, and deletes its derived repo `<data>/projects/<id>.git`. Repos are not touched. Project ids are never reused.
- **`sg repo delete <name> --yes`** refuses without `--yes` and does not contact the server. It is also refused while any project mounts the repo (`repo "<name>" is mounted by project(s) …; delete those projects first (unmounting keeps the repo in their history)`) or has it in its history (`repo "<name>" is in the history of project(s) …; delete those projects first`), because a project rebuild replays that history. Delete those projects first. Otherwise the server removes the repo's mirror state from the database and moves the bare repo to `<data>/deleted/<name>.<unixtime>.git` instead of erasing it. An operator can restore it by moving the directory back to `<data>/repos/<name>.git` and re-creating the repo row (or delete it for good with `rm -rf`). The name can be reused straight away. The delete runs under the repo's land lock, so it never interleaves with a land; lands and draft operations that lose the race fail. The GitHub (or other) mirror remote is never contacted, so its content stays.
- Existing databases are upgraded automatically on start: the `users` table gains a nullable `deleted_at` column. Databases from before projects lose their per-repo `acl` and `drafts` tables, which are re-created per project; grant access again with `sg acl grant <user> <project>`.

The server image ships `sg` too, so you can administer from inside the container:

```bash
docker compose exec sg-server sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)"'
docker compose exec sg-server sg repo list
```

## Importing and mirroring

`sg repo import web --from https://github.com/acme/web.git` creates `web` from the source's `main` and `release/*` branches. The source must be an `https://`, `ssh://` or `user@host:path` URL; local paths, `file://`, `http://` and `git://` are refused. To import a repo from the server's own disk, reach it over ssh (for example `ssh://localhost/srv/git/web.git`, with an SSH key the server can use). Other branches and tags are not copied, and the source must have a `main` branch. An interrupted import leaves nothing behind, so the name can be reused.

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
