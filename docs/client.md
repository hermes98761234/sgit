# Client guide (`sg`)

[← README](../README.md)

You work on **projects**. A project is a named set of folders, set up by an admin. You see only your projects and their folders.

Name a project by its name, one of its aliases, or `#<id>` (for example `#4`). A bare number such as `4` is a name, not an id.

## Commands

| Command | What it does |
|---|---|
| `sg login <host:port> --token T --ca cert.pem` | store credentials in `~/.config/sg/credentials` (mode 0600) |
| `sg projects` | list the projects you can access: NAME ALIASES LEVEL (`read`, `write`, or `admin` for admins) |
| `sg checkout <project\|alias> [dir] [--path P]... [--at REV]` | check out a project into `dir` (default: the project's canonical name, also when you name it by an alias or `#<id>`); repeat `--path` for a sparse checkout |
| `sg status`, `sg diff [path...]` | show local changes |
| `sg commit -m MSG [--land]` | create or update the draft; `--land` also lands it |
| `sg land [draft-id] [--project P]` | land a draft (default: this working copy's draft) |
| `sg sync` | rebase the working copy onto the latest `main`; lists files as `updated:`, `deleted:`, `merged:` or `CONFLICT:` |
| `sg drafts [--mine] [--status S]`, `sg show <id\|rev>`, `sg abandon [id]` | manage drafts; outside a working copy add `--project P` |
| `sg log [path] [-n N]`, `sg blame <path>`, `sg cat <rev>:<path>` | history; works without a checkout with `--project P` |
| `sg sparse list \| add <path>... \| rm <path>...` | change a sparse checkout; `status` and `commit` ignore files outside the sparse paths |

## How it works

- **`.sg/config` holds the project id**, not its name (`project = <id>`, plus `server` and a display-only `name`). Renames and aliases never break a working copy. `sg checkout` and `sg sync` refresh the name. A working copy made before projects existed must be checked out again.
- **`--project`** picks a project outside a working copy. Inside one, commands use the working copy's project. `--server` and `--json` may go before or after the command.
- **Paths are project paths.** A commit that touches a path outside every mount fails with `path outside project: <path>` (exit 1). A path in a read-only mount fails with `read-only path: <path>` (exit 2).
- **No `sg add`.** Every new file that no `.gitignore` ignores goes into the next commit. Nested `.gitignore` files work as in git. Nested `.git` and `.sg` directories (other checkouts and working copies) are left out, and files whose names the server cannot store are listed as `skipped:` in `sg status` and left out of commits: names with a newline, CR, backslash or invalid UTF-8, and names git treats as `.git` (such as `GIT~1`, `.git.` or `.git ` as a folder, or a symlink named `.gitmodules`). Such a name can also come with imported history: you can delete that file through `sg commit`, but changes to it are listed as `skipped:` and never sent.
- **Sparse checkouts see only their paths.** In a sparse checkout, `sg status` and `sg commit` ignore every file outside the sparse paths, even a new one you create there: it is never listed and never landed. Run `sg sparse add <path>` first, then commit.
- **One open draft per working copy.** It is kept in `.sg/draft`, and each `sg commit` updates it until it lands or is abandoned.
- **Merges run on the server.** That includes the 3-way merges for `sg sync`.
- **Conflicts.** When a land or a sync conflicts, `sg` writes conflict markers, lists the files in `sg status`, and exits with code 3. Fix the files and run `sg commit -m … --land` again.
- **Discarding local changes.** There is no revert command. Restore a file from `main` by hand with `sg cat main:<path> > <path>` (delete a file you added). This also works for a conflicted file. `sg abandon` drops the draft on the server but leaves your files as they are. A file stays listed as `conflict:` until its markers are gone and you run `sg commit` (which drops it from the list even when there is then nothing to commit).
- **A land whose answer was lost.** If `sg commit --land` says `landed <sha> (draft <id>), but the working copy was not updated: …; fix that, then run sg sync`, the change is on `main`: run `sg sync`, do not commit it again. If you do rerun `sg commit -m … --land`, it notices that the draft already landed, syncs the working copy and lands only edits made since; with none it prints `these changes are already on main` and exits 0 (`--json`: `"already_on_main": true`). If syncing conflicts with edits you made since, it exits 3 instead and `--json` has `"landed": false` with the conflicts under `sync`; resolve them and commit again.
- **Partial lands.** The folders of a project can come from several sources. A land writes to them one source at a time and is not atomic. If one source conflicts after another already landed, you get a **partial land** (exit 3): the landed folders stay landed, the working copy is rebased onto the new `main`, and the draft keeps only what did not land. Fix the conflicts and run `sg commit -m … --land` again. The retry lands only the rest, never a folder twice. On the server the draft keeps its original base, so `sg land <id>` without fixing anything may report the same conflict again. With `--json`, the answer has `"landed": false`, `"partial": true` and `"landed_mounts": [...]` (the folders that landed). If the rest failed for another reason than a conflict (for example `main` kept moving), nothing is conflicted: run `sg land` again. In a working copy the partial land prints `partially landed: <path>/ landed; the rest stays in draft <id>`. `sg land <id> --project P` outside the working copy prints `partially landed: <path>/ landed on main <sha>; the rest stays in draft <id>` and `sg: landed <path>; the rest did not land: conflict in N file(s)`, with exit 3 and the same `--json` keys, but rebases nothing. A later `sg land <id>` that still conflicts lands nothing, so it reports a plain conflict (`conflict in N file(s)`, exit 3, no `partial` or `landed_mounts` keys).
- **Admin commands** are covered in the [server guide](server.md#administration).
- **Machine-readable output.** Every command accepts `--json`. Lists come wrapped in an object: `log` → `{"commits": [...]}`, `blame` → `{"lines": [...]}`, `drafts` → `{"drafts": [...]}`, `sparse list` → `{"sparse": [...]}`, `projects` → `{"projects": [...]}`. `sync` prints `{"from", "to", "updated", "deleted", "merged", "conflicts", "warnings"}`. Errors print `{"error", "exit_code"}`.
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
| `SG_SOCKET` | sgd socket path (relative paths are resolved against the current directory) |
