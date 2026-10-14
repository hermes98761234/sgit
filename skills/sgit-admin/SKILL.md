---
name: sgit-admin
description: Administer an sgit server - run sg-server, create, list, promote, demote and delete users, create, list, rotate and revoke tokens, grant, list or revoke per-repo access, create, import and delete repos, set up GitHub mirrors. Use when asked to add or remove a user, give or check someone's access to a repo, make someone admin, manage repos, check mirror status, or set up or troubleshoot sg-server.
---

# sgit administration

sgit is source control on top of git where git runs only on the server. `sg-server` holds the bare repos plus a SQLite database of users, tokens and ACLs. Admin work is done with the normal `sg` client, logged in with an **admin token**. Admin commands always dial the server directly, never `sgd`.

## Model

- **Users** are created implicitly by `sg token create <user>`. Name: `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`.
- **Tokens** look like `sgt_…`. Only the hash is stored, so a token is shown **once**. Each token has a 5-character **id** (printed on stderr) used to revoke it.
- **Global admin** is a user flag: set at creation with `--admin`, or later with `sg user promote` / `sg user demote`. It bypasses all repo ACLs. The server never lets the last admin be demoted or deleted.
- **Per-repo access**: `read` or `write` (write implies read). No grant means no access.
- **Repo names**: `^[a-z0-9][a-z0-9._-]{0,63}$`, must not end in `.git`.
- **Deleting a user** is a soft delete: their tokens are revoked, ACLs removed, open drafts abandoned, and the name becomes free to reuse (a re-created user starts with no access). You cannot delete yourself.
- **Deleting a repo** removes its ACLs, drafts and mirror state, and moves the git data to `<data>/deleted/<name>.<unixtime>.git` on the server (recoverable by an operator). The GitHub mirror is not touched. Requires `--yes`.

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

Pass `--server host:port` to any admin command to target a server other than the default login.

## Onboard a user (most common task)

```bash
sg repo create web                       # if the repo doesn't exist yet
TOKEN=$(sg token create bob --email bob@example.com)   # token on stdout, id on stderr
sg acl grant bob web write               # or: read
```

Give the user, over a secure channel: the token, the server address (`host:7419`) and `cert.pem` (if self-signed). They then run `sg login host:7419 --token "$TOKEN" --ca cert.pem`. Never paste tokens into logs, commits, tickets or chat history you don't control.

## Offboard a user / audit access

```bash
sg acl list --user bob            # what bob can reach
sg acl list --repo web            # who can reach web
sg token list bob                 # bob's tokens and their ids
sg user delete bob                # revoke everything (or: sg acl revoke bob web / sg token revoke <id>)
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
| Grant access | `sg acl grant <user> <repo> read\|write` |
| Change access | `sg acl grant` again with the new level |
| Remove access | `sg acl revoke <user> <repo>` |
| List access | `sg acl list [--user U] [--repo R]` (explicit grants only; admins see everything) |
| Create repo | `sg repo create <name> [--mirror URL]` |
| Import from git | `sg repo import <name> --from URL [--mirror URL]` |
| List repos | `sg repo list` |
| Delete repo | `sg repo delete <name> --yes` |
| Repo / mirror status | `sg repo status <name>` |

Add `--json` to any command for machine-readable output (`token create --json` includes the token id).

### Import and mirroring

- `repo import` copies only `main` and `release/*`; the source must have `main`. It can take up to 30 minutes.
- To mirror an imported repo back, `--mirror` must equal `--from`.
- The mirror pushes `main` after every land with `--force-with-lease`, never overwrites, and retries with backoff.
- Mirror URLs: `https://…`, `ssh://…` or `user@host:path`, with no credentials in the URL. Credentials come from server env: `SG_MIRROR_TOKEN` / `SG_MIRROR_TOKEN_FILE` (HTTPS, user `SG_MIRROR_USER`, default `x-access-token`), or an SSH deploy key mounted at `/home/sg/.ssh`.
- `sg repo list` MIRROR column: `-`, `ok`, `pending`, `error`, `broken`. For `error`/`broken`, read `sg repo status <name>` and fix credentials or the remote.

## Running the server

```bash
sg-server init  [--data DIR] [--admin-name admin]     # idempotent; writes <data>/admin-token
sg-server gen-cert --hosts sg.example.com,localhost --out-cert cert.pem --out-key key.pem
sg-server serve [--data DIR] [--listen :7419] --tls-cert cert.pem --tls-key key.pem
sg-server health [--addr 127.0.0.1:7419]              # prints SERVING
```

Env: `SG_DATA_DIR`, `SG_LISTEN` (`:7419`), `SG_TLS_CERT`, `SG_TLS_KEY`, `SG_TLS_SELF_SIGNED=1` + `SG_TLS_HOSTS` (Docker), `SG_MAX_BLOB_MB` (100), `SG_LOG_LEVEL`. TLS is required; `--dev-insecure` works only on loopback. Docker: `docker compose up -d`, data in the `/data` volume.

## Exit codes

| Code | Meaning | Action |
|---|---|---|
| 0 | ok | |
| 1 | error | read the message (bad name, repo exists, unknown user/repo, last-admin or self-delete guard, missing `--yes`…) |
| 2 | not logged in, bad token or not admin | `sg login` with an admin token |

## Safety

- Confirm with the human before revoking tokens or access, promoting or deleting users, or deleting repos. Never add `--yes` to `repo delete` on your own.
- Grant the least access needed: `read` unless the user must land changes.
- Never print a token back to the conversation unless the human asked for it; store it in a variable or file with mode 0600.
