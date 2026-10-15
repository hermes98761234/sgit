#!/bin/sh
# Creates the source and target repos of the Docker end-to-end test, then
# starts githttpd. Runs in the "git" container.
set -eu
ROOT=/srv/git
rm -rf "$ROOT" /tmp/seed && mkdir -p "$ROOT" /tmp/seed
export GIT_AUTHOR_NAME="Ann Source" GIT_AUTHOR_EMAIL=ann@example.com
export GIT_COMMITTER_NAME="Ann Source" GIT_COMMITTER_EMAIL=ann@example.com
git config --global init.defaultBranch main

commit() { # commit <file> <content> <message>
	mkdir -p "$(dirname "$1")"
	printf '%s\n' "$2" >"$1"
	git add -A && git commit -q -m "$3"
}

# app: linear main, release/1, feature/x, a tag, then a merge on main.
git init -q /tmp/seed/app && cd /tmp/seed/app
commit README.md "app" "app: init"
commit lib/a.txt "a1" "app: add lib/a"
commit docs/guide.md "guide" "app: add docs"
git tag v1
git branch release/1
git checkout -q -b feature/x
commit lib/x.txt "x" "app: feature x"
git checkout -q main
commit lib/a.txt "a2" "app: change lib/a"
git merge -q --no-ff -m "app: merge feature/x" feature/x
git clone -q --bare /tmp/seed/app "$ROOT/app.git"

# api: small linear repo for migrate in --into.
git init -q /tmp/seed/api && cd /tmp/seed/api
commit main.go "package main" "api: init"
commit handler.go "package main // handler" "api: add handler"
commit handler.go "package main // handler v2" "api: handler v2"
git clone -q --bare /tmp/seed/api "$ROOT/api.git"

# tool: for migrate in --project --path.
git init -q /tmp/seed/tool && cd /tmp/seed/tool
commit tool.sh "echo tool" "tool: init"
git clone -q --bare /tmp/seed/tool "$ROOT/tool.git"

# Hello-World: its default name is not a valid sgit name.
git clone -q --bare /tmp/seed/tool "$ROOT/Hello-World.git"

# nomain: a repo without main.
git init -q /tmp/seed/nomain && cd /tmp/seed/nomain
git checkout -q -b dev && commit f.txt "f" "nomain: dev only"
git clone -q --bare /tmp/seed/nomain "$ROOT/nomain.git"

# Empty push targets: migrate out, mirrors.
for r in out round mirror mirror2; do git init -q --bare "$ROOT/$r.git"; done

exec githttpd -root "$ROOT"
