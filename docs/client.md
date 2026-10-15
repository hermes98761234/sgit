# Client guide (`sg`)

[← README](../README.md)

You work on **projects**. A project is a named set of folders, set up by an admin. You see only your projects and their folders.

Name a project by its name, one of its aliases, or `#<id>` (for example `#4`). A bare number such as `4` is a name, not an id.

## Commands

| Command | What it does |
|---|---|
| `sg login <host:port> --token T --ca cert.pem` | store credentials in `~/.config/sg/credentials` (mode 0600) |
| `sg projects` | list the projects you can access: NAME ALIASES LEVEL (`read`, `write`, or `admin` for admins) |
| `sg checkout <project\|alias> [dir] [--path P]... [--at REV]` | check out a project into `dir` (default: the project's name); repeat `--path` for a sparse checkout |
| `sg status`, `sg diff [path...]` | show local changes |
| `sg commit -m MSG [--land]` | create or update the draft; `--land` also lands it |
| `sg land [draft-id] [--project P]` | land a draft (default: this working copy's draft) |
| `sg sync` | rebase the working copy onto the latest `main` |
| `sg drafts [--mine] [--status S]`, `sg show <id\|rev>`, `sg abandon [id]` | manage drafts; outside a working copy add `--project P` |
| `sg log [path] [-n N]`, `sg blame <path>`, `sg cat <rev>:<path>` | history; works without a checkout with `--project P` |
| `sg sparse list \| add <path>... \| rm <path>...` | change a sparse checkout |

## How it works

- **`.sg/config` holds the project id**, not its name (`project = <id>`, plus `server` and a display-only `name`). Renames and aliases never break a working copy. `sg checkout` and `sg sync` refresh the name. A working copy made before projects existed must be checked out again.
- **`--project`** picks a project outside a working copy. Inside one, commands use the working copy's project.
- **Paths are project paths.** A commit that touches a path outside every mount fails with `path outside project: <path>` (exit 1). A path in a read-only mount fails with `read-only path: <path>` (exit 2).
- **No `sg add`.** Every new file that no `.gitignore` ignores goes into the next commit. Nested `.gitignore` files work as in git. Nested `.git` and `.sg` directories (other checkouts and working copies) are left out, and files whose names the server cannot store (with a newline, CR or invalid UTF-8) are listed as `skipped:` in `sg status`.
- **One open draft per working copy.** It is kept in `.sg/draft`, and each `sg commit` updates it until it lands or is abandoned.
- **Merges run on the server.** That includes the 3-way merges for `sg sync`.
- **Conflicts.** When a land or a sync conflicts, `sg` writes conflict markers, lists the files in `sg status`, and exits with code 3. Fix the files and run `sg commit -m … --land` again.
- **Partial lands.** The folders of a project can come from several sources. A land writes to them one source at a time and is not atomic. If one source conflicts after another already landed, you get a **partial land** (exit 3): the landed folders stay landed, the working copy is rebased onto the new `main`, and the draft keeps only what did not land. Fix the conflicts and run `sg commit -m … --land` again. The retry lands only the rest, never a folder twice. On the server the draft keeps its original base, so `sg land <id>` without fixing anything may report the same conflict again. With `--json`, the answer has `"landed": false`, `"partial": true` and `"landed_mounts": [...]` (the folders that landed). If the rest failed for another reason than a conflict (for example `main` kept moving), nothing is conflicted: run `sg land` again. `sg land <id> --project P` outside the working copy reports a partial land the same way (exit 3, the same `--json` keys) but rebases nothing.
- **Admin commands** are covered in the [server guide](server.md#administration).
- **Machine-readable output.** Every command accepts `--json`.
- **Cache daemon.** `sg` uses the `sgd` cache daemon automatically when one is running for the same server. See the [sgd guide](sgd.md).

## Exit codes

| Exit code | Meaning |
|---|---|
| 0 | ok |
| 1 | error |
| 2 | not logged in, bad token, or no permission; also `project "<name>" not found or no access` (an unknown project and one you cannot read give the same error) |
| 3 | conflict, including a partial land |
| 4 | `main` kept moving; retry |

## Environment

| Variable | Meaning |
|---|---|
| `SG_TOKEN` | token for `sg login` when `--token` is not given |
| `SG_CONFIG_DIR` | where credentials are stored (default `~/.config/sg`) |
| `SG_NO_DAEMON=1` | never use `sgd` |
| `SG_SOCKET` | sgd socket path |
