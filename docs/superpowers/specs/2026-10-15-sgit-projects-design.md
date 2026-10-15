# sgit projects: per-folder access with hard isolation

**Date:** 2026-10-15
**Status:** Approved design
**Builds on:** [sgit design](2026-10-14-sgit-design.md), which listed per-directory ACLs as a v1 non-goal.

## 1. Goal

Give each client access to chosen folders of chosen repos, and nothing else.

- A **project** is a named view made of one or more folders, which can come from several repos.
- Clients see and use **only projects**. Repos are an admin-only concept, and clients never learn repo names, source paths or source commit SHAs.
- Projects have **aliases**: extra names for the same project.
- Isolation is a **hard security boundary**. A client must not be able to read file names, file contents or history outside its projects, or find out which projects or repos exist. This holds even for a client that sends hand-crafted RPCs or guesses SHAs.

There is no migration. No production data exists yet, so per-repo client ACLs are removed outright.

### Accepted limits

- Commit messages, authors and dates are copied as written. A commit that touched both a project folder and another folder shows its full message to project users.
- A land that spans several repos is **not atomic**. It lands repo by repo (section 6.4).
- A GitHub mirror publishes its whole source repo. Mirrors are admin-only, and a public mirror is outside the project boundary.

## 2. Concepts

- **Repo**: unchanged. A bare git repo in `<data>/repos/<name>.git`, plus an optional GitHub mirror. Only admins see repos.
- **Project**: an id, a unique name, and one or more mounts.
- **Mount**: `<mount path>` in the project maps to `<repo>:<folder>`, and can be flagged `read-only`.
  - A mount path is a clean relative path (same rules as file paths) or `.`.
  - Mount paths must not nest inside each other. `.` is allowed only when it is the project's single mount.
  - The folder is a clean repo-relative path, or `.` for the repo root.
  - A mount may point at a folder that does not exist yet. It shows up as empty until the folder is created.
- **Alias**: an extra name for a project. Names and aliases share one namespace. Both follow the repo-name rule `^[a-z0-9][a-z0-9._-]{0,63}$`.
- **Access**: one `read` or `write` level per (user, project). Write implies read. Admins can reach every project. A `read-only` mount refuses writes from everyone, admins included.

## 3. Data model (SQLite)

The schema below is the new state. `acl` changes from per-repo to per-project, and `drafts.repo` becomes `drafts.project_id`.

```sql
projects(id INTEGER PK, name TEXT UNIQUE NOT NULL, created_at INT NOT NULL)
project_aliases(alias TEXT PK, project_id INT NOT NULL REFERENCES projects(id))
project_mounts(project_id INT, mount_path TEXT, repo TEXT REFERENCES repos(name),
               folder TEXT, read_only INT NOT NULL DEFAULT 0, added_at INT,
               PRIMARY KEY(project_id, mount_path))
acl(user_id INT, project_id INT, level INT, PRIMARY KEY(user_id, project_id))
drafts(..., project_id INT NOT NULL, ...)            -- replaces drafts.repo
project_commits(project_id INT, project_sha TEXT, seq INT,
                PRIMARY KEY(project_id, project_sha))
project_commit_sources(project_id INT, project_sha TEXT, repo TEXT, source_sha TEXT,
                PRIMARY KEY(project_id, project_sha, repo))  -- source main of every mounted repo at that commit
project_cursors(project_id INT, repo TEXT, source_sha TEXT, PRIMARY KEY(project_id, repo))
```

Lookups by name resolve names and aliases in one query. Deleted projects are removed together with their aliases, mounts, ACLs, drafts and derived repo.

## 4. Commands

### Admin

```
sg project create <name>
sg project mount   <project> <path> <repo>:<folder> [--read-only]
sg project unmount <project> <path>
sg project alias add <project> <alias>
sg project alias rm  <alias>
sg project rename  <old> <new>            # old name kept as an alias
sg project list                           # NAME ALIASES MOUNTS
sg project show <project>                 # mounts, aliases, grants
sg project delete <project> --yes
sg acl grant <user> <project> read|write
sg acl revoke <user> <project>
sg acl list [--user U] [--project P]
```

The `sg repo …`, `sg user …` and `sg token …` commands are unchanged. `sg repo delete` is refused while any project mounts the repo.

### Client

- `sg projects` lists the projects the caller can access, with name, aliases and level. Admins see all.
- `sg checkout <project|alias> [dir]` defaults `dir` to the project's canonical name.
- Every command that took `--repo` now takes `--project`.
- `.sg/config` stores `server` and `project` (the project **id**), so renames and aliases never break a working copy. The client shows the canonical name from the server.

## 5. Derived project repo

### 5.1 Storage

Each project has a bare repo at `<data>/projects/<id>.git` with **its own object store**: no alternates, no shared packs. Source repos stay the only source of truth. A project repo is a cache that can be deleted and rebuilt, and rebuilding must produce byte-identical SHAs.

### 5.2 Translating a source commit

Each source commit on a mounted repo's `main` that changes at least one mounted folder of that repo becomes one project commit.

- **Tree.** Build the root tree from the mounts. Each mount is the **unchanged** tree object `<source commit of that repo>:<folder>`. Other repos' mounts keep their state from the previous project commit. A missing folder is left out, and a `.` mount uses the folder tree as the root.
- **Metadata.** Author, author date, committer, committer date and message are copied from the source commit.
- **Parent.** The previous project commit, so project history is linear. The very first project commit has no parent.
- **Objects.** The new objects are the result of `rev-list --objects` of the new mount trees, minus objects the project repo already has. They are copied from the source repo into the project repo with pack-objects / index-pack (or an equivalent).
- **Skipped commits.** A source commit that leaves every mounted folder of its repo unchanged produces nothing.

### 5.3 Ordering across repos

The server keeps a per-repo **land sequence**: a row per commit that `main` reaches through a land, import or create, recording time and order. Project translation merges the first-parent `main` histories of all mounted repos by that sequence. Commits from before the project existed (create or import history) are ordered by committer date, then by repo name, then by first-parent position.

### 5.4 Initial build

`sg project create` makes an empty project with no commits. The first `mount` gives the project its history: every first-parent `main` commit of that repo that touched the folder is translated in order.

### 5.5 Mount changes (forward only)

- A mount added to a project that already has history creates **one** project commit, "Project layout: added `<path>/`", on top of the current head. It contains the folder's current content. Older history of that folder is not imported. The message never names the repo or the source folder.
- Unmount works the same way, with "Project layout: removed `<path>/`".
- Changing `--read-only` is metadata only and creates no commit.
- Layout commits use the acting admin as author and committer.

### 5.6 Keeping up to date

- **Catch-up.** Before every project RPC, compare `project_cursors` with each mounted repo's `main`. If any moved, translate the new commits under a per-project lock. If nothing moved, the cost is one ref read per mounted repo.
- **After a land** through a project, the server catches up before replying, so the land result is a project commit.
- No background worker is needed.

## 6. Writes

### 6.1 Path mapping

Clients send `Change{path}` relative to the project root. The server maps each path to a mount by longest mount-path prefix (`.` matches everything).

- A path under no mount, or one that equals a mount path itself, is rejected with InvalidArgument "path outside project: <path>".
- A path in a read-only mount is rejected with PermissionDenied "read-only path: <path>".
- Source paths are `<folder>/<rest>`. Existing path validation still applies to both forms.

### 6.2 Drafts

- A draft is a commit in the project repo at `refs/drafts/<id>`, on a project base.
- `drafts.project_id` scopes `sg drafts`, `sg show`, `sg land` and `sg abandon` to the project.
- Land, update and abandon still require the owner or an admin.

### 6.3 Land (single repo)

1. Catch the project up.
2. Re-check the caller's access: write level, plus the read-only flag of every touched path. A grant revoked after the draft was made blocks the land.
3. Diff the draft head against its project base, and map each changed path to its mount and source path.
4. Take the source base as the source `main` of that repo recorded for the draft's project base (`project_commit_sources`). Build the source commit from that base with the mapped changes, and land it with today's code: CAS on `main`, plus a server-side rebase or 3-way merge when `main` moved.
5. Catch the project up and return the new project head.

Conflict paths come back as source paths inside the touched folder. They are mapped back to project paths before they are returned.

### 6.4 Land across repos (best effort)

- The changes are split per repo, and the repos are landed one at a time in repo-name order.
- **On the first conflict or error, stop.** Repos that already landed stay landed.
- The draft is rebuilt on the new project head (after catch-up), so it now holds only the changes that did not land. Its id stays the same.
- The response is the usual conflict (exit 3) with the files in conflict, plus `partial: true` and `landed: [<mount paths>]`.
- A retry with `sg commit -m … --land` lands only what is left, so a mount is never landed twice.
- Each repo produces its own source commit, with the draft's message and author. The project log shows one commit per repo.

### 6.5 Commit and land in one step

`CommitAndLand` is a temporary draft (as today) followed by 6.3 or 6.4.

## 7. Reads

- Every read RPC serves from the project repo: resolve, objects, has-objects, log, blame, diff, merge-file, drafts. No extra filtering is needed.
- `ResolveRev` accepts `main`, draft ids and project SHAs. A source-repo SHA gives NotFound.
- The `Project` service replaces the client-facing `Repo` service. The `repo` field in every request becomes `project`, which accepts a name, an alias or a decimal id.
- New client RPC `ListProjects`. New admin RPCs: CreateProject, MountProject, UnmountProject, AddProjectAlias, RemoveProjectAlias, RenameProject, ListProjects (admin view), GetProject, DeleteProject. The ACL RPCs take a project instead of a repo.
- No client-facing message carries a repo name, source path or source SHA.

## 8. Errors and existence hiding

- An unknown project and a project the caller cannot read both return **identical** NotFound errors (exit 2): `project "<name>" not found or no access`.
- PermissionDenied is returned only to callers who can already read the project (a write with `read` access, or a read-only path).
- Admin RPCs keep returning PermissionDenied to non-admins, as today.

## 9. sgd

- The object cache stays keyed by SHA and shared across users.
- The access memo is keyed by (token hash, project) and probed with `HasObjects(project, [])`.
- **Every** GetObjects request that would be served from cache is first confirmed with one batched `HasObjects(project, shas)` call to the server. Only SHAs the server confirms are served. A cache hit can therefore never leak another project's object.

## 10. Tests

### Isolation

- The project root lists only mount paths.
- GetObjects/HasObjects for a SHA from an unmounted folder (taken from the source repo) give NotFound, both directly and through `sgd` with a warm cache.
- A commit that touched only unmounted folders does not appear in project `log`.
- `blame`, `diff` and `cat` never return content outside the project.
- "Unknown project" and "no access" errors are byte-identical, in text and in `--json`.
- Client-facing responses never contain a repo name or a source SHA.

### Translation

- A single mount, a `.` mount, and several mounts from one repo.
- Mounts from two repos, interleaved in land order.
- Catch-up after a land through another project, or through a different project that shares the repo.
- Forward-only mount add and remove.
- Deleting the project repo and rebuilding it gives the same SHAs.

### Writes

- A path outside the project and a read-only path are rejected.
- A revoked grant blocks the land of an existing draft.
- A partial cross-repo land, followed by a retry that lands only the rest.
- Concurrent lands through two projects that share a repo lose no work.

### End to end

- An extended Docker smoke test.
- A randomized monkey run over projects.
- Benchmarks for checkout, land and log on a project over the 20,000-file repo, compared with the v0.2.0 numbers.

## 11. Docs

Update README, `docs/server.md`, `docs/client.md`, `docs/sgd.md`, and both skills. The `sgit-client` skill only ever talks about projects.

## 12. Implementation notes

These notes record what the implementation decided beyond this spec, and where it deliberately differs from it.

### Changes to the spec

- **Project references (§7).** A project is named by its name, an alias, or `#<id>`, where `<id>` is the canonical positive decimal (`"#" + id`). A bare decimal string is only ever a name or alias, never an id. Otherwise creating a project named `5` would silently redirect working copies of project 5. Names and aliases cannot contain `#`. `.sg/config` still stores `project = <id>`, and the client sends it as `#<id>`. Side effect: `--project <name>` inside a working copy is not treated as "this working copy".
- **Repo delete (§4).** `sg repo delete` is refused while any project mounts the repo **or has it in its history** (`project_commits` or `project_commit_sources`), because a rebuild replays that history. The admin must delete those projects first. The error names the projects: `repo "<name>" is mounted by project(s) …; delete those projects first (unmounting keeps the repo in their history)` or `repo "<name>" is in the history of project(s) …; delete those projects first`. The server check builds the message; the store re-checks inside the delete transaction.
- **Partial land (§6.4).** After a partial cross-repo land, the server draft keeps its **original base**, and its head is rebuilt as that base plus only the changes that did not land. It is not rebuilt on the new project head, because that would silently overwrite upstream changes. `ConflictDetail.main` carries the new project head. The client syncs the working copy to it (writing conflict markers), and the next `sg commit --land` re-commits from the new base, so a retry lands only the rest. A retry of the unchanged server draft does a real 3-way merge from the old base and may report the same conflict until the user syncs.
- **Upstream renames out of the mounts (§6.1, §6.3).** `git merge-tree` follows renames, so a clean rebase could move draft content to a source path outside the writable mounts. Every rebased land is diffed after the merge; if any changed source path is outside the writable mounts of that repo, the land is refused with FailedPrecondition `draft <id>: main changed under the draft (files moved outside the writable mounts); sync and commit again`.
- **Drafts that predate a mount change (§6.3).** Before landing anything, the layout recorded at the draft's base must equal the current mount (path, repo, folder) of every touched mount. Otherwise the land is refused with FailedPrecondition `draft <id> was made before mount <path>/ was added; sync and commit again` (or `… changed; …`). The check runs up front, so a stale layout never causes a partial land. Unmounting and remounting the same `repo:folder` is accepted.
- **Mounts are re-read under the land lock (§6.3 step 2).** A mount flipped to read-only, or unmounted, between creating the draft and landing it, is enforced at land time.

### Decisions beyond the spec

- **`project_commits` stores the derivation recipe:** `kind` (`source` or `layout`), `repo` and `source_sha` (for `source`), `layout` (JSON list of `{path, repo, folder}` in effect) and, for layout commits, `actor_name`, `actor_email`, `actor_when` and `message`. Without it a rebuild cannot reproduce forward-only layout commits (§5.1 requires byte-identical SHAs).
- **Land sequence table:** `land_seq(seq INTEGER PRIMARY KEY AUTOINCREMENT, repo, sha, at, UNIQUE(repo, sha))`. The row is written before the CAS on `main`, so a concurrent catch-up never sees a landed commit without its sequence number. Rows for commits that lost the CAS are harmless. Import records its imported `main`; create records its initial commit. If recording fails, create and import roll back the repo row and directory.
- **Ordering comparator (§5.3):** two commits that both have a sequence compare by sequence; otherwise by committer date, then repo name, then first-parent position. The merge is a k-way merge of per-repo first-parent queues, so per-repo order is always kept.
- **An initial mount that finds no commit touching the folder** writes one layout commit ("Project layout: added `<path>/`", empty content), so the project has a `main` that can be checked out.
- **An empty folder tree counts as absent** for any mount except `.`, so an empty source repo mounted at `x` adds no `x/` entry (git trees hold no empty directories).
- **`.` mounts of a whole repo** reproduce the source history (same trees, author, committer, message and first parent), so project SHAs equal source SHAs for ordinary commits. Signed commits (`gpgsig`, `mergetag`), commits with an `encoding` header and merge commits (only the first parent is kept) get different SHAs. Rebuilds stay deterministic either way. This is harmless: such a project contains exactly that repo. Isolation tests use folder mounts.
- **Draft refs** are `refs/sg/drafts/<id>/head` and `refs/sg/drafts/<id>/v<n>` (§6.2's `refs/drafts/<id>` is descriptive). A draft's base must be a project commit, otherwise InvalidArgument `base commit <sha> not found`.
- **Rebuild** happens when `<data>/projects/<id>.git` is missing but the project has commits. It runs when the server first opens the project, so the directory must be deleted while the server is stopped. Open drafts of that project are marked abandoned, because their objects lived only in the deleted repo.
- **A cross-repo land that fails with a non-conflict error after at least one repo landed** returns the same Aborted + `ConflictDetail{partial: true, landed, files: []}` as a conflict, so the client still syncs and exits 3. If nothing landed, the original error is returned.
- **`LandResponse.parent`** is the new project head's parent only when the head's tree equals the draft's tree; otherwise it is empty, which makes the CLI do a full sync.
- **`ResolveRevResponse` gains `project_id` and `project_name`**, so `sg checkout` resolves an alias to an id and learns the canonical name in the same round trip. `.sg/config` stores `project = <id>` plus a display-only `name`, refreshed by `checkout` and `sync`. A working copy from before projects (no `project` id) must be checked out again.
- **`ResolveRev` (§7)** accepts any git revision syntax, but only against the project repo, plus a draft id (8 base32 characters with an existing draft head ref). Source SHAs are absent from that object store, so they give NotFound. `Log`, `Blame` and `Diff` resolve their revisions the same way, so a foreign SHA gives NotFound rather than Internal.
- **`HasObjects` with a foreign SHA returns `present=false`** (it never errors per SHA); `GetObjects` returns NotFound.
- **`sg project mount` on an existing path** with the same `repo:folder` only sets the read-only flag to the given value, with no commit. A different source is AlreadyExists.
- **`sg project rename`** to one of the project's own aliases swaps them: the alias is removed and the old name becomes an alias.
- **`ListProjects` level** for admins is `admin`.
- **CLI `--json` for a partial land** keeps the boolean `"landed": false` and adds `"partial": true` and `"landed_mounts": [...]`. The spec's `landed` list lives in `ConflictDetail`; reusing the JSON key would break agents that read the boolean.
- **The not-found error echoes the caller's reference** (`project "<ref>" not found or no access`), so the unknown and no-access cases are byte-identical for the same reference. Invalid names get the same shape. The CLI maps NotFound with the suffix ` not found or no access` to exit 2.
- **Admin project RPCs** other than mount and unmount read only the store, so a project whose repo cannot be rebuilt can still be inspected, renamed or deleted.
- **sgd (§9).** The access memo is keyed by (token hash, project) and trusted for 60 seconds (`ACLTTL`). Every cache hit, including hits found while registering an in-flight fetch, is confirmed with one batched `HasObjects(project, shas)` before it is served; a failed confirmation serves nothing (fail closed). In-flight fetches are shared only per (project, SHA). A request that waits for another request's in-flight fetch relies on its access memo rather than its own confirmation, so for up to `ACLTTL` after a revoke it can still receive an object of the same project. `ListProjects` passes through uncached.
- **Existing databases** with the old per-repo `acl` or `drafts.repo` are migrated by dropping those two tables on open (no production data exists).
- **Mounts run detached from the caller.** The engine runs `Mount` under `context.WithoutCancel` with a server-side bound (`MountTimeout`, default one hour), so a client that gives up does not undo a long first-mount import. `sg project mount` waits up to 30 minutes.
- **Catch-up records progress in chunks** of 500 translated commits (commits, cursors and the `main` ref together), so a cancelled or failed catch-up resumes where it stopped. Opening a project repo also moves a lagging `main` ref to the recorded head.
- **A failed first mount replaces the project repo with an empty one**, so the objects it copied cannot be probed by SHA. With no commits there are no drafts to lose.
- **Conflict stage SHAs the project repo does not hold are blanked** (merge-tree follows renames, so a stage could come from outside the mounts).
- **A partial land's status message names the landed mounts** (`landed <paths>; the rest did not land: …`), and the CLI reports `partial` and `landed_mounts` in text and `--json` even when it does not rebase a working copy.
