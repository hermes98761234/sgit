---
name: sgit-client
description: Work in an sgit (sg) project - log in, list projects, check out, view status and diffs, commit and land changes, use drafts, sync with main, resolve conflicts, read history with log/blame/cat, manage sparse checkouts. Use whenever a directory has a .sg/ folder, the user mentions sg or sgit, or a task needs code changes landed through sgit instead of git.
---

# Using sgit (`sg`)

sgit is source control where git runs only on the server. You work on **projects**: a project is a named set of folders that an admin set up for you. A working copy has your files plus a `.sg/` directory and **no `.git`**. There is no staging area: every changed file in the working copy is part of the next commit. Do not run `git` commands in an sg working copy, and never edit files inside `.sg/`.

## Setup (once per machine)

```bash
go install github.com/hermes98761234/sgit/cmd/sg@latest   # or a release binary
sg login sg.example.com:7419 --token "$SG_TOKEN" --ca cert.pem
```

Get the token, server address and `cert.pem` from your sgit admin. The token can also come from `$SG_TOKEN` or stdin. Credentials are saved in `~/.config/sg/credentials` (`$SG_CONFIG_DIR` overrides). Never echo the token or commit it.

## Projects

```bash
sg projects                        # NAME ALIASES LEVEL: the projects you can access
sg checkout <project> [dir]        # dir defaults to the project's name
```

Name a project by its name, an alias, or `#<id>` (e.g. `#4`). A bare number is a name, not an id. `.sg/config` stores the project id, so a rename or a new alias never breaks a working copy.

An unknown project and one you cannot access give the same error: `project "<name>" not found or no access` (exit 2). Some folders of a project can be **read-only**: committing there fails with `read-only path: <path>` (exit 2). A path outside every folder of the project fails with `path outside project: <path>` (exit 1).

## Everyday workflow

```bash
sg checkout site && cd site        # full checkout of main of project site
sg checkout site --path src        # sparse: only src/
sg sync                            # update to latest main before starting
# ... edit files ...
sg status                          # changed, added, deleted (+ skipped) files
sg diff [path...]                  # review the change
sg commit -m "fix the thing" --land    # commit and land on main in one step
```

`--land` is the normal path. The server rebases your change onto the current `main`, so lands touching different files never conflict.

## Drafts (review before landing)

```bash
sg commit -m "wip" [--title T]     # create/update the open draft (id in .sg/draft)
sg drafts [--mine] [--status open|landed|abandoned|all]
sg show <draft-id|rev>
sg land [draft-id]                 # land it
sg abandon [draft-id]              # drop it
```

Draft ids are 8 chars of `a-z2-7`.

## History without a checkout

```bash
sg log [path] [-n 20] [--rev main]
sg blame <path> [--rev REV]
sg cat <rev>:<path>                # e.g. sg cat main:README.md
```

Outside a working copy, add `--project <name>` (and `--server host:port` if not the default). The same works for `sg drafts`, `sg show`, `sg land` and `sg abandon`.

## Sparse checkouts

```bash
sg sparse list
sg sparse add docs tools
sg sparse rm tools
```

## Exit codes - handle them

| Code | Meaning | What to do |
|---|---|---|
| 0 | ok | |
| 1 | error | read the message |
| 2 | not logged in, bad token, no access, `project "<name>" not found or no access`, or `read-only path` | `sg login`; check the name with `sg projects`; or ask the admin for `read`/`write` on the project |
| 3 | conflict | conflict markers are written; `sg status` lists the files. Edit them to remove all markers, then rerun `sg commit -m "..." --land` |
| 3 | partial land: `partially landed: <path>/ landed; the rest stays in draft <id>` (`--json`: `"partial": true`, `"landed_mounts": [...]`) | a project can span several sources, and a land is not atomic across them. The listed folders landed and stay landed; the working copy is already synced to the new `main`. Fix the conflicts, then rerun `sg commit -m "..." --land`: it lands only the rest. Do not just rerun `sg land <id>` on the old draft |
| 4 | `main` kept moving | just retry the same command |

## Tips for agents

- Use `--json` on any command for machine-readable output; errors become `{"error": ..., "exit_code": ...}`.
- Run `sg sync` before editing and `sg status` + `sg diff` before landing, so you only land what you intended.
- Check `sg status` for `skipped:` files: names with newlines, CR or invalid UTF-8 cannot be stored.
- Max file size is set by the server (100 MB by default).
- If `sgd` (local cache daemon) runs, `sg` uses it automatically; `SG_NO_DAEMON=1` bypasses it.
- Confirm with the human before landing on `main` unless they already asked you to land.
- Admin work (users, tokens, access, projects) is covered by the `sgit-admin` skill.
