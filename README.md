# sgit

Scion-style source control on top of git:

- Thin, always-online clients with no `.git`.
- Commits and lands run on the server, with compare-and-swap on `main`.
- A shared object cache on each machine for parallel agents.
- Bare git repos on the server are the source of truth.

Inspired by Anthropic's [Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control).

> **Status:** phase 1 (server core and Docker) is implemented. The CLI (`sg`), daemon (`sgd`) and GitHub mirroring are next. See the [design spec](docs/superpowers/specs/2026-10-14-sgit-design.md).

## Run the server with Docker

```bash
docker compose up -d
docker compose exec sg-server cat /data/admin-token   # initial admin token
docker compose exec sg-server sg-server health        # SERVING
```

- The server listens on `:7419` (gRPC over TLS).
- With `SG_TLS_SELF_SIGNED=1`, a certificate for `SG_TLS_HOSTS` is generated into the volume at `/data/tls/cert.pem`. Clients should pin this certificate.
- To use your own certificate instead, set `SG_TLS_CERT` and `SG_TLS_KEY` and mount the files.

| Variable | Default | Meaning |
|---|---|---|
| `SG_DATA_DIR` | `/data` | repos, `sg.db`, `admin-token`, `tls/` |
| `SG_LISTEN` | `:7419` | listen address |
| `SG_TLS_CERT` / `SG_TLS_KEY` | — | PEM certificate and key |
| `SG_TLS_SELF_SIGNED` | `0` | `1` = generate a self-signed certificate on first start |
| `SG_TLS_HOSTS` | `localhost,127.0.0.1` | SANs for the self-signed certificate |
| `SG_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `SG_MAX_BLOB_MB` | `100` | max size of one file |

All users on a machine share the object cache, which is fine for a trusted team (see spec §7).

## Run without Docker

Requires git ≥ 2.40.

```bash
make build
./dist/sg-server init --data ./sgdata
./dist/sg-server gen-cert --out-cert sgdata/cert.pem --out-key sgdata/key.pem
./dist/sg-server serve --data ./sgdata --tls-cert sgdata/cert.pem --tls-key sgdata/key.pem
```

## Development

```bash
make gen     # regenerate protobuf code (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
make test    # go test -race ./...
make smoke   # build the image and run the end-to-end test against it
```

## License

MIT
