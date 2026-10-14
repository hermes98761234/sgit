# sgit

Scion-style source control on top of git. It uses thin, always-online clients with no `.git`, runs commits and lands on the server with compare-and-swap, shares an object cache on each machine for parallel agents, and keeps git bare repos on the server as the source of truth.

Inspired by Anthropic's [Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control).

> **Status:** design phase. See the [design spec](docs/superpowers/specs/2026-10-14-sgit-design.md).

Components:
- `sg`: the CLI
- `sgd`: the per-node cache daemon
- `sg-server`: the gRPC server over bare git repos, Docker-deployable

License: MIT
