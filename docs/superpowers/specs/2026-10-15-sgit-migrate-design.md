# sgit migrate: git repos in, projects out

**Date:** 2026-10-15
**Status:** Draft, awaiting review
**Builds on:** [sgit design](2026-10-14-sgit-design.md) (§8 import and mirror), [sgit projects](2026-10-15-sgit-projects-design.md).

## 1. Goal

Move code between plain git repos and an sgit server in one admin command each way.

- **In:** bring a git repo (for example on GitHub) into sgit and expose it as a project. Either as its own sgit repo, or merged with full history into a folder of an existing sgit repo (a real monorepo).
- **Out:** publish a project as a standalone git repo.

`migrate out` is a **copy**. sgit is not changed, and running it again updates the target. Removing the project afterwards is left to the existing `project delete` and `repo delete`.

### Accepted limits

- `--into` gives the imported commits new SHAs, because their paths move. Merge commits are flattened to their first-parent line, as sgit does everywhere.
- `--into` migrates `main` only. `release/*` branches are skipped with a warning.
- `--into` holds the target repo's land lock for the whole replay. Lands into that repo wait until it finishes.
- `migrate out` publishes the project's derived history. A project that mounts a whole repo at `.` keeps the source SHAs. Other projects get their translated SHAs, with only the mounted paths.
- `migrate out` pushes `main` only.

## 2. Commands

Both are admin only. They are a new group, `sg migrate`, served by two new Admin RPCs, `MigrateIn` and `MigrateOut`. The CLI uses the 30-minute import timeout for both. Both print a text summary, or JSON with `--json`.

```
sg migrate in  <url> [<name>] [--into <repo>:<folder>] [--project <p> --path <dir>] [--mirror <url>]
sg migrate out <project> --to <url> [--force]
```

### 2.1 `migrate in`

- `<name>` defaults to the last path segment of `<url>`, with a trailing `.git` stripped. If that is not a valid name (`^[a-z0-9][a-z0-9._-]{0,63}$`), the command fails and asks for `<name>`.
- **Plain form:** imports `<url>` as a new repo `<name>`, exactly like `repo import`. `main` and `release/*` keep their SHAs. `--mirror` works as in `repo import`.
- **`--into <repo>:<folder>`:** replays the imported `main` into the existing `<repo>` under `<folder>/` (section 3.2). No repo is created, and `<name>` is used only as the project name. `--mirror` is a usage error in this form. `<folder>` is a clean repo-relative path and must not be `.`.
- **Project step:**
  - Default: create project `<name>` and mount the imported code at `.`. The mounted source is `<name>:.` in the plain form and `<repo>:<folder>` with `--into`.
  - `--project <p> --path <dir>`: mount into the existing project `<p>` at `<dir>` instead. Both flags are required together. Mount rules (no nesting, `.` only as a single mount) are checked by the existing mount validation.
- **Output:** repo, project, mount path and source, number of commits migrated, skipped branches.

### 2.2 `migrate out`

- Pushes the project's `main` to `<url>`.
- Fast-forward only by default. `--force` overwrites a diverged target, with a lease on the target `main` that was read.
- **Output:** the pushed SHA, or "up to date".

## 3. Data flow

### 3.1 `migrate in`, plain form

1. Run the existing import flow, `repos.importRepo`: validate URLs, reserve the name, fetch into `.incoming/`, rename into place, create the store row, record the land, notify the mirror.
2. Run the project step (section 3.3).
3. If the project step fails, roll back: delete a project created in step 2, then remove the new repo with `rollbackNew`. Return the error.

### 3.2 `migrate in --into <repo>:<folder>`

1. Validate the URL with `ValidateRemote`, check that the target repo exists, and check `<folder>` with the path rules (git-reserved names refused).
2. Fetch the URL's `main` into a staging bare repo under `.incoming/`.
3. Fetch the staged `main` into the target repo as `refs/sgit/migrate/<rand>`, so its objects are present.
4. Take the target's `repoHandle.landMu`. Read `main` as `base`. Refuse if `<folder>` exists in `base`'s tree and is not empty.
5. List the imported commits with `FirstParents`, oldest first. For each commit `c`:
   - tree = `base`'s tree with `<folder>` replaced by `c`'s root tree (`SpliceTree`);
   - parent = the previous rewritten commit, or `base` for the first one;
   - write it with `WriteCommit`. Author, committer, `encoding` and message are copied byte for byte. `gpgsig` and `mergetag` headers are dropped, because they no longer match.
6. Record one land-sequence row, then move `main` from `base` to the last rewritten commit with `UpdateRef` (compare and swap). Release the lock and notify the mirror if the repo has one.
7. Delete the temporary ref and the staging repo. This also runs on every failure path.
8. Run the project step (section 3.3).

Failure handling:

- Before step 6 nothing is visible.
- After step 6 the commits stay in the target repo, because other lands may already be on top. If the project step fails, the error says that the code landed at `<repo>:<folder>` and can be mounted by hand. A project created in this run is deleted.
- An imported repo with no `main`, or no commits, is refused at step 2.

### 3.3 Project step

- Default: `CreateProject(<name>)`, then mount at `.`.
- With `--project`: mount into `<p>` at `<dir>`.
- The mount goes through the existing `MountProject` path (`lockProject`, then `engine.Mount`). The first mount of a project imports the whole folder history, and later mounts write one layout commit, as today.

### 3.4 `migrate out`

1. Resolve the project (name or alias), open it with `projects.Get`, and catch up so the latest lands are included.
2. Read the project's `main`. Refuse if the project has no commits.
3. Validate `<url>` with `ValidateRemote` and read the target's `main` with `RemoteRef`.
   - No `main` on the target: push.
   - Same SHA: report "up to date", push nothing.
   - The target `main` is an ancestor of the project `main`: push (fast-forward). The ancestor check needs the target commit locally, so fetch it into a temporary ref first. If it cannot be fetched, the target is treated as diverged.
   - Otherwise refuse with "target has diverged", unless `--force` is set. With `--force`, push with force-with-lease against the target `main` that was read.
4. Push from `<data>/projects/<id>.git` with `gitx.Push`. The remote rules (https and ssh only, no credentials in the URL, local paths only when allowed) are the same as for mirrors.

## 4. Errors and security

- Only admins can call either RPC. Repo names stay out of client-facing RPCs, docs and skills.
- Errors follow the current style (no internal `store:` or `gitx:` prefixes). Distinct messages cover:
  - name already taken;
  - unknown repo or project;
  - folder not empty;
  - target diverged;
  - empty project;
  - imported repo has no `main`;
  - `--project` without `--path`, or the reverse;
  - `--mirror` with `--into`.

## 5. Tests

Tests come first. They use `testenv`, with `file://` remotes in temp dirs.

### gitx replay

- Every rewritten commit has the imported tree under `<folder>`, and the target's other files are untouched.
- Author, committer and message are byte-identical to the originals.
- A merge in the imported history is flattened to first parent.
- `gpgsig` is dropped.

### Server and CLI

- Plain `in`: repo and project exist, project SHAs equal the source SHAs.
- `in --into`: target `main` advanced by one commit per imported commit; `sg log` in the new project lists every original commit.
- `in --project --path` into an existing project.
- Rollback when the mount fails: no new project and, in the plain form, no new repo.
- `--into` with a non-empty folder is refused, and the target is unchanged.
- `out` to a new target, then again after a land (fast-forward), then to a diverged target (refused), then with `--force` (succeeds).
- `out` on an empty project is refused.
- Non-admins get permission denied for both RPCs.
- Round trip: `migrate in` a repo, then `migrate out` its project. The target's `main` equals the original `main`.

Everything runs under `go test -race -count=1 ./...`, plus `go vet` and staticcheck.

## 6. Docs

- `docs/server.md`: a migrate section with both directions and the limits from section 1.
- `sgit-admin` skill (`skills/` and `.claude/skills/`): the commands and examples.
- `README.md`: one line in the admin overview.
