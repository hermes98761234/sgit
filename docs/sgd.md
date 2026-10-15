# Cache daemon guide (`sgd`)

[← README](../README.md)

`sgd` keeps a per-user disk cache of git objects in front of one server, so several working copies and agents on the same machine share fetched objects.

```bash
sgd --upstream sg.example.com:7419 --ca cert.pem
```

`--upstream` and `--ca` default to `$SGD_UPSTREAM` and `$SGD_CA`.

## Run as a systemd user service

```bash
mkdir -p ~/.config/systemd/user
cp contrib/sgd.service ~/.config/systemd/user/
# edit ExecStart: the sgd path (go install puts it in ~/go/bin), --upstream and --ca
systemctl --user daemon-reload
systemctl --user enable --now sgd
```

## How `sg` uses it

- **When `sg` uses it.** `sg` uses `sgd` automatically when the daemon fronts the server you are logged in to. If there is no socket, or sgd fronts a different server, `sg` connects directly without saying anything.
- **Unusable socket.** If a socket path exists but cannot be used, `sg` prints one warning and connects directly. This covers sgd not answering, a path that is not a socket, and unsafe ownership or permissions.
- **Bypass.** Set `SG_NO_DAEMON=1` to skip the daemon.
- **Admin commands** always go to the server directly.

## Socket

- The socket is `$XDG_RUNTIME_DIR/sg/sgd.sock` with mode 0600, one daemon per user. Change it with `--socket` (sgd only) or `$SG_SOCKET` (both sg and sgd).
- `sg` refuses a socket or socket directory that you do not own, or that others can write to.
- `sgd` refuses to start if something other than a socket already exists at the socket path.
- `sgd` creates the socket directory with mode 0700 if it does not exist. It never changes an existing directory, and refuses to start if that directory is group- or world-writable or not yours.

## Cache

- **Location.** `$XDG_CACHE_HOME/sg/objects`, usually `~/.cache/sg/objects`. Override it with `--cache-dir`.
- **Size.** The default limit is 10 GiB (`--cache-max 10G`). The least recently used objects are evicted first.
- **Integrity.** Every object is checked against its hash when it is stored, and again each time it is served.

## Security

- Every request still runs with your own token.
- **Access memo per (token, project).** Before serving cached objects for a project, sgd checks that your token can read that project, and re-checks it at least every 60 seconds.
- **Every cache hit is confirmed.** The cache is keyed by object hash and shared across users and projects. Before sgd serves cached objects, it asks the server once, with one batched `HasObjects(project, shas)` call and your token, whether the project has them. Only the objects the server confirms are served; the rest are fetched from the server, which answers NotFound for objects outside the project. So the shared cache never crosses project boundaries.
- **Fail closed.** If that confirmation fails (for example, the server is unreachable), sgd serves nothing from the cache and returns the error.
- **In-flight fetches.** When several requests for the same project ask for the same object at once, sgd fetches it from the server once and hands it to all of them. A request that waits for another's fetch relies on its access memo, not on its own confirmation. So for up to 60 seconds after a revoke, a user can still receive an object of that project that another user's request is fetching at that moment. Objects of other projects are never handed over this way.
- `sg projects` passes through to the server with your token; nothing is cached.
