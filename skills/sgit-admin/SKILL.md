---
name: sgit-admin
description: Administer an sgit server - run sg-server, create, list, promote, demote and delete users, create, list, rotate and revoke tokens, create projects and mount repo folders into them, manage aliases, renames and read-only mounts, grant, list or revoke per-project access, create, import and delete repos, migrate a repo in or out, set up GitHub mirrors. Use when asked to add or remove a user, give or check someone's access to a project, set up a project, make someone admin, manage repos, check mirror status, or set up or troubleshoot sg-server.
---

# sgit administration

sgit is source control on top of git where git runs only on the server. `sg-server` holds the bare repos plus a SQLite database of users, tokens, projects and ACLs. Admin work is done with the normal `sg` client, logged in with an **admin token**. Admin commands always dial the server directly, never `sgd`.

## Model

- **Users** are created implicitly by `sg token create <user>`. Name: `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`.
- **Tokens** look like `sgt_…`. Only the hash is stored, so a token is shown **once**. Each token has a short **id** (printed on stderr) used to revoke it.
- **Global admin** is a user flag: set at creation with `--admin`, or later with `sg user promote` / `sg user demote`. It bypasses all project ACLs. The server never lets the last admin be demoted or deleted.
- **Repos are admin-only.** Users never see repo names or source paths, and never source commit SHAs except in one case: a project whose first mount is a whole repo at `.` copies that repo's commits byte for byte, so they keep their SHAs (up to the first merge commit or commit with any extra header, such as a signature or `encoding`; all of them for repos made through sgit). Use folder mounts when the SHAs must stay private. Users get **projects**.
- **Project**: an id, a unique name, optional **aliases**, and **mounts**. A mount maps a project path to `<repo>:<folder>` and can be `--read-only` (refuses writes from everyone, admins included). One project can mount folders of several repos.
- **Project reference**: name, alias or `#<id>` (e.g. `#3`). A bare number is a name, never an id.
- **Mount rules**: the path is a clean relative path or `.` (project root); the folder is a clean repo-relative path or `.` (repo root). Mount paths must not nest. `.` only as the single mount. A missing folder shows up empty until it is created.
- **History**: the first mount imports that folder's history. Later mounts and unmounts are forward only: one "Project layout: added/removed `<path>/`" commit by the acting admin, no older history. Changing `--read-only` makes no commit.
- **Per-project access**: `read` or `write` (write implies read). No grant means no access. An unknown project and one the user cannot read give the same error, `project "<name>" not found or no access` (exit 2).
- **Names** of repos, projects and aliases: `^[a-z0-9][a-z0-9._-]{0,63}$`. Project names and aliases share one namespace. Repo names must not end in `.git`.
- **Deleting a user** is a soft delete: their tokens are revoked, ACLs removed, open drafts abandoned, and the name becomes free to reuse (a re-created user starts with no access). You cannot delete yourself.
- **Deleting a project** removes its aliases, mounts, grants, drafts and derived repo `<data>/projects/<id>.git`. Repos are untouched. Requires `--yes`.
- **Deleting a repo** is refused while any project mounts it or has it in its history; delete those projects first. Otherwise it removes the mirror state and moves the git data to `<data>/deleted/<name>.<unixtime>.git` on the server (recoverable by an operator). The GitHub mirror is not touched. Requires `--yes`.
- **`<data>/projects/<id>.git`** is a cache. With the server stopped it can be deleted; the next request rebuilds it with identical SHAs, but open drafts of that project are abandoned.
- **Accepted limits**: commit messages are copied as written (a commit touching a mounted and an unmounted folder shows its full message); a land across several repos is not atomic; a mirror publishes its whole repo.

## Log in as admin

The first admin token is written by `sg-server init` to `<data>/admin-token` (user `admin`).

```bash
# Docker (server image ships sg too)
docker compose exec sg-server cat /data/admin-token
docker compose cp sg-server:/data/tls/cert.pem ./cert.pem
sg login sg.example.com:7419 --token "$ADMIN_TOKEN" --ca cert.pem

# Or run admin commands inside the container
docker compose exec sg-server sh -c \
  'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)"'
docker compose exec sg-server sg repo list
```

Pass `--server host:port` to any admin command to target a server other than the default login, after the command (`sg repo list --server host:port`) or before it (`sg --server host:port repo list`).

## Onboard a user (most common task)

```bash
sg repo create web                       # if the repo doesn't exist yet
TOKEN=$(sg token create bob --email bob@example.com)   # token on stdout, id on stderr
sg project create site                   # if the project doesn't exist yet
sg project mount site public web:public  # or: sg project mount site . web:.  (whole repo)
sg acl grant bob site write              # or: read
```

Give the user, over a secure channel: the token, the server address (`host:7419`) and `cert.pem` (if self-signed). They then run `sg login host:7419 --token "$TOKEN" --ca cert.pem` and `sg checkout site`. Never paste tokens into logs, commits, tickets or chat history you don't control.

## Offboard a user / audit access

```bash
sg acl list --user bob            # which projects bob can reach
sg acl list --project site        # who can reach site
sg project show site              # mounts, aliases and grants of site
sg token list bob                 # bob's tokens and their ids
sg user delete bob                # revoke everything (or: sg acl revoke bob site / sg token revoke <id>)
```

## Command reference

| Task | Command |
|---|---|
| Create user + token | `sg token create <user> [--email E]` |
| Create admin user | `sg token create <user> --admin` |
| Extra token for existing user | `sg token create <user>` (flags ignored for existing users) |
| List users | `sg user list` (NAME EMAIL ADMIN TOKENS CREATED) |
| Make / unmake admin | `sg user promote <user>` / `sg user demote <user>` |
| Delete user | `sg user delete <user>` |
| List tokens | `sg token list [user]` (ID USER CREATED STATUS; never shows secrets) |
| Revoke a token | `sg token revoke <token-id>` |
| Rotate a token | `sg token list <user>` to find the old id, `sg token create <user>`, hand it over, then `sg token revoke <old-id>` |
| Create project | `sg project create <name>` |
| Mount a folder | `sg project mount <project> <path> <repo>:<folder> [--read-only]` (`.` = project root / repo root) |
| Set or clear read-only | `sg project mount` again with the same `<path> <repo>:<folder>`, with or without `--read-only` (no commit) |
| Unmount | `sg project unmount <project> <path>` |
| Add / remove alias | `sg project alias add <project> <alias>` / `sg project alias rm <alias>` |
| Rename project | `sg project rename <old> <new>` (old name kept as an alias; renaming to an own alias swaps them) |
| List projects | `sg project list` (NAME ALIASES MOUNTS; mounts as `path=repo:folder`, `(ro)` = read-only) |
| Show project | `sg project show <project>` (id, aliases, head, mounts, grants) |
| Delete project | `sg project delete <project> --yes` |
| Grant access | `sg acl grant <user> <project> read\|write` |
| Change access | `sg acl grant` again with the new level |
| Remove access | `sg acl revoke <user> <project>` (says `nothing revoked` when the user had no explicit grant) |
| List access | `sg acl list [--user U] [--project P]` (USER PROJECT LEVEL; explicit grants only: admins are not listed, but can access every project) |
| Create repo | `sg repo create <name> [--mirror URL]` |
| Import from git | `sg repo import <name> --from URL [--mirror URL]` (URL: `https://`, `ssh://` or `user@host:path`; local paths and `file://` are refused) |
| List repos | `sg repo list` |
| Delete repo | `sg repo delete <name> --yes` |
| Repo / mirror status | `sg repo status <name>` |

Add `--json` to any command for machine-readable output (`token create --json` includes the token id; `project` commands print `id`, `name`, `aliases`, `head`, `mounts`, `grants`).

### Import and mirroring

- `repo import` copies only `main` and `release/*`; the source must have `main`. It can take up to 30 minutes.
- To mirror an imported repo back, `--mirror` must equal `--from`.
- The mirror pushes `main` after every land with `--force-with-lease`, never overwrites, and retries with backoff.
- Mirror URLs: `https://…`, `ssh://…` or `user@host:path`, with no credentials in the URL. Credentials come from server env: `SG_MIRROR_TOKEN` / `SG_MIRROR_TOKEN_FILE` (HTTPS, user `SG_MIRROR_USER`, default `x-access-token`), or an SSH deploy key mounted at `/home/sg/.ssh`.
- `sg repo list` MIRROR column: `-`, `ok`, `pending`, `error`, `broken`. For `error`/`broken`, read `sg repo status <name>` and fix credentials or the remote.

### Migrate

```
sg migrate in <url> [name] [--into <repo>:<folder>] [--project P --path DIR] [--mirror URL]
sg migrate out <project> --to <url> [--force]
```

```
sg migrate in https://github.com/acme/api.git                       # repo api + project api
sg migrate in https://github.com/acme/api.git --into mono:services/api  # into the monorepo
sg migrate out api --to git@github.com:acme/api-export.git
```

- `--into` replays the history of `main` under the folder with new SHAs (signatures dropped, merges flattened); the folder must not exist in the repo yet (a file there or above it is refused). Only `main` is migrated; other branches, `release/*` included, are listed as skipped. `--mirror` cannot be combined with `--into`; set the mirror on the target repo.
- Without `--into`, `main` and `release/*` are imported with their SHAs; other branches are listed as skipped. Tags are never migrated.
- `name` defaults to the URL's last segment without `.git` and must match `^[a-z0-9][a-z0-9._-]{0,63}$` (`Hello-World.git` needs an explicit name). It is not used with both `--into` and `--project`.
- The mount (`--project P --path DIR`) is checked against `P`'s mounts before anything is fetched; the new project is created first and removed if a later step fails.
- Project SHAs equal the source SHAs only for linear, unsigned history; a merge or signed commit and everything after it get new SHAs.
- `migrate out` pushes the project's `main`, fast-forward only; `--force` overwrites a diverged target (to the original repo it rewrites that repo's history when merges or signatures were dropped). The project is not changed.

## Running the server

```bash
sg-server init  [--data DIR] [--admin-name admin]     # idempotent; writes <data>/admin-token
sg-server gen-cert --hosts sg.example.com,localhost --out-cert cert.pem --out-key key.pem
sg-server serve [--data DIR] [--listen :7419] --tls-cert cert.pem --tls-key key.pem
sg-server health [--addr 127.0.0.1:7419]              # prints SERVING
```

Env: `SG_DATA_DIR` (`/data` in Docker, `./sgdata` otherwise), `SG_LISTEN` (`:7419`), `SG_TLS_CERT`, `SG_TLS_KEY`, `SG_TLS_SELF_SIGNED=1` + `SG_TLS_HOSTS` (Docker image only; without Docker use `gen-cert`), `SG_MAX_BLOB_MB` (100), `SG_LOG_LEVEL`, `SG_DEV_INSECURE`. TLS is required; `--dev-insecure` works only on loopback. Docker: `docker compose up -d`, data in the `/data` volume. Only one `sg-server` can use a data dir at a time (a lock on `<data>/sg.lock`); a second one exits with `another sg-server is already using data dir …`. A filesystem without locks, or a `sg.db` it cannot chmod to 0600, only gives a warning in the log.

## Exit codes

| Code | Meaning | Action |
|---|---|---|
| 0 | ok | |
| 1 | error | read the message (bad name, repo or project exists, unknown user/repo, overlapping mount, repo still used by a project, last-admin or self-delete guard, missing `--yes`…) |
| 2 | not logged in, bad token or not admin; or `project "<name>" not found or no access` | `sg login` with an admin token; check the project name with `sg project list` |

## Safety

- Confirm with the human before revoking tokens or access, promoting or deleting users, unmounting folders, or deleting projects or repos. Never add `--yes` to `project delete` or `repo delete` on your own.
- `sg project delete <project> --yes` removes the project's grants and drafts for good.
- `sg repo delete` is refused while a project mounts the repo or has it in its history. Do not delete projects just to get past this without the human's explicit approval.
- Mount only the folders the users need: project users can read everything under a mount, plus the full messages of commits that touched it.
- Grant the least access needed: `read` unless the user must land changes.
- Never print a token back to the conversation unless the human asked for it; store it in a variable or file with mode 0600.
