# Client guide (`sg`)

[← README](../README.md)

## Commands

| Command | What it does |
|---|---|
| `sg login <host:port> --token T --ca cert.pem` | store credentials in `~/.config/sg/credentials` (mode 0600) |
| `sg checkout <repo> [dir] [--path P]... [--at REV]` | check out a repo; repeat `--path` for a sparse checkout |
| `sg status`, `sg diff [path...]` | show local changes |
| `sg commit -m MSG [--land]` | create or update the draft; `--land` also lands it |
| `sg land [draft-id]` | land a draft (default: this working copy's draft) |
| `sg sync` | rebase the working copy onto the latest `main` |
| `sg drafts [--mine]`, `sg show <id\|rev>`, `sg abandon [id]` | manage drafts |
| `sg log [path] [-n N]`, `sg blame <path>`, `sg cat <rev>:<path>` | history; works without a checkout with `--repo` |
| `sg sparse list \| add <path>... \| rm <path>...` | change a sparse checkout |
| `sg repo`, `sg token`, `sg acl` | admin commands, see the [server guide](server.md#administration) |

## How it works

- **No `sg add`.** Every new file that the root `.gitignore` doesn't ignore goes into the next commit.
- **One open draft per working copy.** It is kept in `.sg/draft`, and each `sg commit` updates it until it lands or is abandoned.
- **Merges run on the server.** That includes the 3-way merges for `sg sync`.
- **Conflicts.** When a land or a sync conflicts, `sg` writes conflict markers, lists the files in `sg status`, and exits with code 3. Fix the files and run `sg commit -m … --land` again.
- **Machine-readable output.** Every command accepts `--json`.
- **Cache daemon.** `sg` uses the `sgd` cache daemon automatically when one is running for the same server. See the [sgd guide](sgd.md).

## Exit codes

| Exit code | Meaning |
|---|---|
| 0 | ok |
| 1 | error |
| 2 | not logged in, bad token, or no permission |
| 3 | conflict |
| 4 | `main` kept moving; retry |

## Environment

| Variable | Meaning |
|---|---|
| `SG_TOKEN` | token for `sg login` when `--token` is not given |
| `SG_CONFIG_DIR` | where credentials are stored (default `~/.config/sg`) |
| `SG_NO_DAEMON=1` | never use `sgd` |
| `SG_SOCKET` | sgd socket path |
