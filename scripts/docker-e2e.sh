#!/usr/bin/env bash
# Full end-to-end test in Docker: a plain HTTPS git host, sg-server, the sgd
# cache daemon and two clients, each in its own container (see
# test/e2e/docker-compose.yml). It runs every sg operation over the network:
# admin (tokens, users, repos, projects, ACLs, migrate), the client workflow
# (checkout, commit, land, drafts, sync and conflicts, history, sparse), the
# cache (use, fallback, upstream down, restart), mirrors and a server restart.
#
#   scripts/docker-e2e.sh            # build, run, tear down
#   KEEP=1 scripts/docker-e2e.sh     # leave the stack running afterwards
#
# Needs docker with compose v2 and jq.
set -uo pipefail
cd "$(dirname "$0")/.."

D=(docker compose -f test/e2e/docker-compose.yml)
PASS=0
FAILED=()
CA=/srv-data/tls/cert.pem

cleanup() {
	if [ "${KEEP:-0}" = 1 ]; then
		echo "stack kept; stop it with: ${D[*]} down -v"
	else
		"${D[@]}" down -v --remove-orphans >/dev/null 2>&1
	fi
}
trap cleanup EXIT

# --- helpers -----------------------------------------------------------------

# run <svc> <dir> <cmd...>: run a command in a container; sets OUT
# (stdout+stderr) and RC.
run() {
	local svc=$1 dir=$2
	shift 2
	OUT=$("${D[@]}" exec -T -w "$dir" "$svc" "$@" 2>&1)
	RC=$?
}
# runo: like run, but OUT is stdout only (for JSON).
runo() {
	local svc=$1 dir=$2
	shift 2
	OUT=$("${D[@]}" exec -T -w "$dir" "$svc" "$@" 2>/dev/null)
	RC=$?
}
pass() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
fail() {
	FAILED+=("$1")
	printf '  FAIL  %s\n' "$1"
	[ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/        | /' | head -20
}
# check <desc> <expected-rc> <svc> <dir> <cmd...>
check() {
	local desc=$1 want=$2
	shift 2
	run "$@"
	if [ "$RC" = "$want" ]; then pass "$desc"; else fail "$desc (exit $RC, want $want)" "$OUT"; fi
}
# has <desc> <ERE>: the last OUT matches.
has() { if grep -Eq -- "$2" <<<"$OUT"; then pass "$1"; else fail "$1 (no /$2/)" "$OUT"; fi; }
hasnt() { if grep -Eq -- "$2" <<<"$OUT"; then fail "$1 (has /$2/)" "$OUT"; else pass "$1"; fi; }
eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got:  $2"$'\n'"want: $3"; fi; }
section() { printf '\n== %s\n' "$*"; }

adm() { check "$1" "$2" server /data sg "${@:3}"; }  # admin, on the server box
al() { check "$1" "$2" alice "$3" sg "${@:4}"; }     # alice, through sgd
bo() { check "$1" "$2" bob "$3" sg "${@:4}"; }       # bob, direct
# put <svc> <path> <content>: write a file in a container.
put() { printf '%s\n' "$3" | "${D[@]}" exec -T "$1" sh -c 'mkdir -p "$(dirname "$1")" && cat >"$1"' put "$2"; }
sh_in() { local svc=$1; shift; "${D[@]}" exec -T "$svc" sh -c "$@"; }
gitsha() { "${D[@]}" exec -T git git -C "/srv/git/$1.git" rev-parse -q --verify "${2:-main}" 2>/dev/null; }
wait_healthy() {
	for _ in $(seq 60); do
		[ "$(docker inspect -f '{{.State.Health.Status}}' "$("${D[@]}" ps -q "$1")" 2>/dev/null)" = healthy ] && return 0
		sleep 1
	done
	return 1
}

# --- start -------------------------------------------------------------------

section "build and start"
"${D[@]}" down -v --remove-orphans >/dev/null 2>&1
if ! "${D[@]}" up -d --build --wait >/tmp/sgit-e2e-up.log 2>&1; then
	tail -40 /tmp/sgit-e2e-up.log
	"${D[@]}" logs --tail 50
	exit 1
fi
pass "stack is up: git, server, cache, alice, bob"

check "server health" 0 server /data sg-server health
has "health says SERVING" SERVING
check "sgd is in the image" 0 bob /home/sg sgd --help

# --- admin: auth, tokens, users ------------------------------------------------

section "auth, tokens, users"
ADMIN=$("${D[@]}" exec -T server cat /data/admin-token)
adm "admin login on the server box" 0 login localhost:7419 --ca /data/tls/cert.pem --token "$ADMIN"
check "login with a bad token fails (exit 2)" 2 bob /home/sg sg login server:7419 --ca "$CA" --token sgt_bogus
check "login without --ca fails on a self-signed server" 1 bob /home/sg sg login server:7419 --token "$ADMIN"
check "remote admin through --server works" 0 bob /home/sg env SG_CONFIG_DIR=/home/sg/admincfg sg login server:7419 --ca "$CA" --token "$ADMIN"
check "remote admin: repo list" 0 bob /home/sg env SG_CONFIG_DIR=/home/sg/admincfg sg repo list

runo server /data sg --json token create alice --email alice@example.com
ALICE=$(jq -r .token <<<"$OUT")
runo server /data sg token create bob
BOB=$OUT
runo server /data sg token create carol
CAROL=$OUT
[ -n "$ALICE" ] && [ -n "$BOB" ] && [ -n "$CAROL" ] && pass "token create alice, bob, carol" || fail "token create"
runo server /data sg --json user list
eq "alice's email is stored" "$(jq -r '.users[]|select(.name=="alice").email' <<<"$OUT")" alice@example.com
adm "token create --admin makes an admin user" 0 token create dave --admin
runo server /data sg --json user list
eq "dave is admin" "$(jq -r '.users[]|select(.name=="dave").admin' <<<"$OUT")" true

al "alice login (through cache box)" 0 /home/sg login server:7419 --ca "$CA" --token "$ALICE"
bo "bob login (direct)" 0 /home/sg login server:7419 --ca "$CA" --token "$BOB"
check "carol login" 0 bob /home/sg env SG_CONFIG_DIR=/home/sg/carol sg login server:7419 --ca "$CA" --token "$CAROL"

adm "user promote carol" 0 user promote carol
runo server /data sg --json user list
eq "carol is admin" "$(jq -r '.users[]|select(.name=="carol").admin' <<<"$OUT")" true
check "carol can run admin commands" 0 bob /home/sg env SG_CONFIG_DIR=/home/sg/carol sg repo list
adm "user demote carol" 0 user demote carol
check "demoted carol cannot (exit 2)" 2 bob /home/sg env SG_CONFIG_DIR=/home/sg/carol sg repo list
al "alice (non-admin) cannot create repos (exit 2)" 2 /home/sg repo create nope

runo server /data sg --json token create bob
TMPID=$(jq -r .id <<<"$OUT")
TMPTOK=$(jq -r .token <<<"$OUT")
check "second token for bob works" 0 bob /home/sg env SG_CONFIG_DIR=/home/sg/bob2 sg login server:7419 --ca "$CA" --token "$TMPTOK"
adm "token revoke" 0 token revoke "$TMPID"
check "revoked token is refused (exit 2)" 2 bob /home/sg env SG_CONFIG_DIR=/home/sg/bob2 sg projects
runo server /data sg --json token list bob
eq "token list shows it revoked" "$(jq -r ".tokens[]|select(.id==\"$TMPID\").status" <<<"$OUT")" revoked

adm "user delete carol" 0 user delete carol
check "deleted user's token is refused (exit 2)" 2 bob /home/sg env SG_CONFIG_DIR=/home/sg/carol sg projects
runo server /data sg --json user list
eq "carol is gone from user list" "$(jq -r '[.users[].name]|index("carol")' <<<"$OUT")" null
adm "cannot delete yourself" 1 user delete admin
adm "user demote dave" 0 user demote dave
adm "cannot demote the last admin" 1 user demote admin

# --- repos and projects --------------------------------------------------------

section "repos and projects"
adm "repo create core" 0 repo create core
adm "repo create web --mirror (empty https mirror)" 0 repo create web --mirror https://git/mirror.git
adm "repo create duplicate fails" 1 repo create core
adm "repo create bad name fails" 1 repo create Bad_Name
adm "repo create with a non-empty mirror fails" 1 repo create web2 --mirror https://git/app.git
adm "repo create with http:// mirror fails" 1 repo create web3 --mirror http://git/mirror2.git
adm "repo import" 0 repo import imp --from https://git/app.git
runo server /data sg --json repo status imp
eq "imported main keeps the source SHA" "$(jq -r .main <<<"$OUT")" "$(gitsha app)"
adm "repo import without main fails" 1 repo import nm --from https://git/nomain.git
adm "repo list" 0 repo list
has "repo list shows core, web, imp" '^core .*' && has "  web" '^web ' && has "  imp" '^imp '
adm "repo delete without --yes refuses" 1 repo delete imp
adm "repo delete --yes" 0 repo delete imp --yes
adm "repo status of a deleted repo fails" 1 repo status imp

adm "project create site" 0 project create site
adm "mount lib" 0 project mount site lib core:lib
adm "mount public (other repo)" 0 project mount site public web:public
adm "mount vendor read-only" 0 project mount site vendor core:vendor --read-only
adm "mount . next to other mounts is refused" 1 project mount site . core:.
adm "nested mount is refused" 1 project mount site lib/sub core:x
adm "mount an existing path with another source is refused" 1 project mount site lib web:lib
adm "mount from an unknown repo fails" 1 project mount site x nosuch:x
adm "alias add" 0 project alias add site www
adm "alias that clashes with a project name fails" 1 project alias add site site
adm "acl grant alice write" 0 acl grant alice www write
adm "acl grant bob read" 0 acl grant bob site read
adm "acl grant bad level fails" 1 acl grant bob site admin
runo server /data sg --json acl list --project site
eq "acl list" "$(jq -c '[.acl[]|[.user,.level]]|sort' <<<"$OUT" 2>/dev/null || echo "$OUT")" '[["alice","write"],["bob","read"]]'
adm "project rename site portal" 0 project rename site portal
adm "old name still resolves" 0 project show site
has "show prints the canonical name" 'project: portal'
has "show lists the read-only mount" 'vendor/ +core:vendor +read-only'
runo server /data sg --json project list
eq "project list: aliases" "$(jq -c '.projects[]|select(.name=="portal").aliases|sort' <<<"$OUT")" '["site","www"]'
PID=$(jq -r '.projects[]|select(.name=="portal").id' <<<"$OUT")
adm "reference by #id" 0 project show "#$PID"
adm "unknown project (exit 2)" 2 project show nosuch

# --- client workflow (alice via sgd, bob direct) --------------------------------

section "client workflow"
runo alice /home/sg sg --json projects
eq "alice sees portal with write" "$(jq -r '.projects[]|select(.name=="portal").level' <<<"$OUT")" write
eq "alice sees only her project" "$(jq '.projects|length' <<<"$OUT")" 1
runo bob /home/sg sg --json projects
eq "bob sees portal with read" "$(jq -r '.projects[]|select(.name=="portal").level' <<<"$OUT")" read

al "alice checkout by alias" 0 /home/sg checkout www wc
W=/home/sg/wc
put alice $W/lib/a.txt "line1"
put alice $W/public/index.html "<h1>hi</h1>"
al "status lists new files" 0 $W status
has "status: lib/a.txt" 'lib/a.txt' && has "status: public/index.html" 'public/index.html'
al "diff" 0 $W diff
has "diff shows content" 'line1'
al "commit --land (two repos)" 0 $W commit -m "first files" --land
has "landed" 'landed'
al "status is clean after land" 0 $W status
runo alice $W sg --json log -n 5
eq "a land across two repos shows once per repo" "$(jq '[.commits[]|select(.subject=="first files")]|length' <<<"$OUT")" 2
FIRST=$(jq -r '.commits[0].sha' <<<"$OUT")

put alice $W/vendor/x.txt "x"
al "write into a read-only mount (exit 2)" 2 $W commit -m ro --land
has "read-only path error" 'read-only path: vendor/x.txt'
sh_in alice "rm -rf $W/vendor"
put alice $W/other/x.txt "x"
al "write outside every mount (exit 1)" 1 $W commit -m out --land
has "outside error" 'path outside project: other/x.txt'
sh_in alice "rm -rf $W/other"
al "commit with nothing to commit fails (exit 1)" 1 $W commit -m none --land

bo "bob checkout (direct)" 0 /home/sg checkout portal wc
BW=/home/sg/wc
run bob $BW cat lib/a.txt
eq "bob sees alice's file" "$OUT" "line1"
put bob $BW/lib/b.txt "bob"
bo "bob with read access cannot land (exit 2)" 2 $BW commit -m "bob try" --land
sh_in bob "rm -f $BW/lib/b.txt"

# drafts
put alice $W/lib/d.txt "draft"
al "commit without --land makes a draft" 0 $W commit -m "draft one" --title "Draft one"
runo alice $W sg --json drafts --mine
DRAFT=$(jq -r '.drafts[0].id' <<<"$OUT")
eq "draft is open" "$(jq -r '.drafts[0].status' <<<"$OUT")" open
al "show draft" 0 $W show "$DRAFT"
has "show draft lists the file" 'lib/d.txt'
put alice $W/lib/d.txt "draft v2"
al "commit again updates the same draft" 0 $W commit -m "draft one v2"
runo alice $W sg --json drafts --mine
eq "still one open draft" "$(jq '[.drafts[]|select(.status=="open")]|length' <<<"$OUT")" 1
bo "bob lists drafts outside a checkout" 0 /home/sg drafts --project portal
has "bob sees alice's draft" "$DRAFT"
al "land the draft by id" 0 $W land "$DRAFT"
run bob /home/sg sg cat main:lib/d.txt --project portal
[ "$RC" = 0 ] || run bob $BW sg cat main:lib/d.txt
eq "landed draft content" "$OUT" "draft v2"
put alice $W/lib/e.txt "abandon me"
al "draft to abandon" 0 $W commit -m "to abandon"
al "abandon" 0 $W abandon
runo alice $W sg --json drafts --mine --status abandoned
eq "abandoned draft listed" "$(jq '.drafts|length' <<<"$OUT")" 1
sh_in alice "rm -f $W/lib/e.txt"
al "drafts --status all" 0 $W drafts --status all

# sync and conflicts
adm "acl grant bob write (upgrade)" 0 acl grant bob portal write
bo "bob sync" 0 $BW sync
has "sync updates files" 'lib/d.txt'
put alice $W/lib/a.txt "alice change"
put bob $BW/lib/a.txt "bob change"
al "alice lands first" 0 $W commit -m "alice edit" --land
bo "bob's land conflicts (exit 3)" 3 $BW commit -m "bob edit" --land
run bob $BW cat lib/a.txt
has "conflict markers" '^<<<<<<<'
bo "status shows conflict" 0 $BW status
has "conflict listed" 'conflict'
put bob $BW/lib/a.txt "merged change"
bo "bob resolves and lands" 0 $BW commit -m "bob edit" --land
al "alice sync" 0 $W sync
run alice $W cat lib/a.txt
eq "alice has the merge result" "$OUT" "merged change"
put alice $W/lib/a.txt "next"
put bob $BW/public/b.html "bob page"
al "alice lands lib" 0 $W commit -m "lib next" --land
bo "bob lands public on an old base (no conflict)" 0 $BW commit -m "bob page" --land

# history
al "log" 0 $W log -n 3
has "log has subjects" 'bob page|lib next'
al "log of a path" 0 $W log lib/a.txt
has "path log" 'alice edit'
hasnt "path log skips other paths" 'bob page'
al "blame" 0 $W blame lib/a.txt
al "cat main:path" 0 $W cat main:public/b.html
al "log --project outside a checkout" 0 /home/sg log --project portal -n 1
al "show a revision" 0 $W show "$FIRST"
runo alice /home/sg sg --json blame lib/a.txt --project portal
eq "blame --json" "$(jq '.lines|length' <<<"$OUT")" 1

# checkout variants
al "checkout --at an old revision" 0 /home/sg checkout portal old --at "$FIRST"
run alice /home/sg/old cat lib/a.txt
eq "old checkout content" "$OUT" "line1"
al "sparse checkout" 0 /home/sg checkout portal sp --path lib
run alice /home/sg/sp ls
eq "sparse has only lib" "$OUT" "lib"
al "sparse list" 0 /home/sg/sp sparse list
al "sparse add" 0 /home/sg/sp sparse add public
run alice /home/sg/sp ls public
has "public appears" 'index.html'
al "sparse rm" 0 /home/sg/sp sparse rm public
run alice /home/sg/sp ls
eq "public is gone" "$OUT" "lib"
al "checkout into an existing non-empty dir fails" 1 /home/sg checkout portal wc
al "checkout unknown project (exit 2)" 2 /home/sg checkout nosuch x

# --- isolation and ACLs -------------------------------------------------------

section "isolation and ACLs"
adm "secret project" 0 project create secret
adm "mount secret" 0 project mount secret s core:secret
put server /tmp/sec/README "x"
adm "acl grant bob secret write" 0 acl grant bob secret write
bo "bob checkout secret" 0 /home/sg checkout secret sec
put bob /home/sg/sec/s/key.txt "top secret"
bo "bob lands secret" 0 /home/sg/sec commit -m "secret" --land
al "alice cannot read secret, through sgd (exit 2)" 2 /home/sg log --project secret
check "alice cannot read secret, direct (exit 2)" 2 alice /home/sg env SG_NO_DAEMON=1 sg log --project secret
al "alice cannot cat secret (exit 2)" 2 /home/sg cat main:s/key.txt --project secret
adm "acl revoke bob portal" 0 acl revoke bob portal
adm "revoke again is a no-op" 0 acl revoke bob portal
has "nothing revoked" 'nothing revoked'
bo "bob lost portal (exit 2)" 2 /home/sg log --project portal
bo "bob's working copy is refused too (exit 2)" 2 $BW sync

# --- cache daemon -------------------------------------------------------------

section "cache daemon (sgd)"
NOBJ=$("${D[@]}" exec -T cache sh -c 'find /home/sg/cache -type f | wc -l' | tr -d ' ')
[ "${NOBJ:-0}" -gt 0 ] && pass "sgd cached $NOBJ objects for alice" || fail "sgd cache is empty"
"${D[@]}" logs cache >/tmp/sgit-e2e-cache.log 2>&1
al "second checkout through sgd" 0 /home/sg checkout portal wc2
NOBJ2=$("${D[@]}" exec -T cache sh -c 'find /home/sg/cache -type f | wc -l' | tr -d ' ')
eq "second checkout added no objects (served from cache)" "$NOBJ2" "$NOBJ"
check "SG_NO_DAEMON=1 bypasses sgd" 0 alice /home/sg env SG_NO_DAEMON=1 sg log --project portal -n 1

"${D[@]}" stop server >/dev/null 2>&1
al "server down: sg through sgd fails (exit 1)" 1 /home/sg log --project portal -n 1
has "error names sgd" 'through sgd'
bo "server down: direct client fails" 1 /home/sg projects
"${D[@]}" start server >/dev/null 2>&1
wait_healthy server && pass "server back up" || fail "server did not come back"
al "after restart, sg through sgd works again" 0 /home/sg log --project portal -n 1

"${D[@]}" stop cache >/dev/null 2>&1
al "sgd down: sg falls back to the server" 0 /home/sg log --project portal -n 1
"${D[@]}" start cache >/dev/null 2>&1
wait_healthy cache && pass "sgd restarted on the old socket" || fail "sgd did not restart" "$("${D[@]}" logs --tail 20 cache)"
al "sg uses sgd again" 0 /home/sg/wc2 sync

# --- mirror -------------------------------------------------------------------

section "mirror"
WEBMAIN=""
for _ in $(seq 30); do
	runo server /data sg --json repo status web
	WEBMAIN=$(jq -r .main <<<"$OUT")
	[ "$(gitsha mirror)" = "$WEBMAIN" ] && break
	sleep 1
done
eq "mirror main follows repo web" "$(gitsha mirror)" "$WEBMAIN"
run git /srv/git git -C mirror.git show main:public/b.html
eq "mirror has bob's page" "$OUT" "bob page"
adm "repo status web" 0 repo status web
has "status shows the mirror" 'mirror'

# --- migrate ------------------------------------------------------------------

section "migrate in"
adm "migrate in (plain)" 0 migrate in https://git/app.git
has "5 first-parent commits" '5 commits'
has "skipped branches listed" 'not migrated: feature/x'
runo server /data sg --json repo status app
eq "migrated repo keeps the source SHA" "$(jq -r .main <<<"$OUT")" "$(gitsha app)"
adm "acl grant alice app read" 0 acl grant alice app read
al "alice checks out the migrated project" 0 /home/sg checkout app app
run alice /home/sg/app cat lib/x.txt
eq "merged branch content is there" "$OUT" "x"
runo alice /home/sg sg --json log --project app
eq "history: 5 commits" "$(jq '.commits|length' <<<"$OUT")" 5
adm "migrate in an existing name fails" 1 migrate in https://git/app.git
has "name taken" 'exists|taken'
adm "migrate in a name that is not valid asks for a name" 1 migrate in https://git/Hello-World.git
adm "migrate in with an explicit name" 0 migrate in https://git/Hello-World.git hello-world
adm "migrate in a repo without main fails" 1 migrate in https://git/nomain.git nm
adm "  ...and leaves no project behind" 2 project show nm
adm "  ...and no repo behind" 1 repo status nm
adm "migrate in with http:// fails" 1 migrate in http://git/app.git x1
adm "migrate in from an unknown host fails" 1 migrate in https://nohost.invalid/a.git x2
adm "  ...and leaves no project" 2 project show x2
al "non-admin cannot migrate (exit 2)" 2 /home/sg migrate in https://git/api.git

runo server /data sg --json repo status core
CORE0=$(jq -r .main <<<"$OUT")
adm "migrate in --into core:services/api/" 0 migrate in https://git/api.git --into core:services/api/
has "3 commits" '3 commits'
adm "acl grant alice api write" 0 acl grant alice api write
al "alice reads the replayed code" 0 /home/sg cat main:handler.go --project api
eq "replayed content" "$OUT" "package main // handler v2"
runo alice /home/sg sg --json log --project api
eq "replayed history keeps subjects" "$(jq -r '[.commits[].subject]|reverse|join(",")' <<<"$OUT")" "api: init,api: add handler,api: handler v2"
eq "replayed history keeps the author" "$(jq -r '.commits[0].author' <<<"$OUT")" "Ann Source"
run alice $W sync
run alice $W cat lib/a.txt
eq "other folders of core are untouched" "$OUT" "next"
adm "--into a non-empty folder fails" 1 migrate in https://git/api.git api2 --into core:services/api
has "not empty" 'not empty'
adm "--into under a file fails" 1 migrate in https://git/api.git api3 --into core:lib/a.txt/x
has "is a file" 'file'
adm "--into an unknown repo fails" 1 migrate in https://git/api.git api4 --into nosuch:x
adm "--into with --mirror is a usage error" 1 migrate in https://git/api.git api5 --into core:x --mirror https://git/mirror2.git
adm "--project without --path is a usage error" 1 migrate in https://git/tool.git --project portal
adm "--project into a mounted path fails before fetching" 1 migrate in https://git/tool.git --project portal --path lib
adm "migrate in --project --path" 0 migrate in https://git/tool.git --project portal --path tools
al "alice sees the new mount after sync" 0 $W sync
run alice $W cat tools/tool.sh
eq "mounted tool code" "$OUT" "echo tool"

section "migrate out"
adm "migrate out to an empty repo" 0 migrate out api --to https://git/out.git
runo alice /home/sg sg --json log --project api -n 1
APIMAIN=$(jq -r '.commits[0].sha' <<<"$OUT")
eq "target main = project main" "$(gitsha out)" "$APIMAIN"
run git /srv/git git -C out.git show main:handler.go
eq "target has the project files at the root" "$OUT" "package main // handler v2"
adm "migrate out again: up to date" 0 migrate out api --to https://git/out.git
has "up to date" 'up to date'
al "alice checks out api" 0 /home/sg checkout api api
put alice /home/sg/api/handler.go "package main // handler v3"
al "alice lands in api" 0 /home/sg/api commit -m "api: handler v3" --land
adm "migrate out fast-forwards" 0 migrate out api --to https://git/out.git
runo alice /home/sg sg --json log --project api -n 1
eq "target moved to the new main" "$(gitsha out)" "$(jq -r '.commits[0].sha' <<<"$OUT")"
sh_in git 'set -e; rm -rf /tmp/div; git clone -q /srv/git/out.git /tmp/div; cd /tmp/div; echo x >diverge.txt; git add .; git -c user.name=x -c user.email=x@x commit -qm diverge; git push -q origin main'
adm "migrate out to a diverged target is refused" 1 migrate out api --to https://git/out.git
has "diverged" 'diverged|fast-forward|non-fast-forward'
adm "migrate out --force" 0 migrate out api --to https://git/out.git --force
runo alice /home/sg sg --json log --project api -n 1
eq "forced target = project main" "$(gitsha out)" "$(jq -r '.commits[0].sha' <<<"$OUT")"
adm "empty project for migrate out" 0 project create empty
adm "migrate out an empty project fails" 1 migrate out empty --to https://git/mirror2.git
adm "migrate out with http:// fails" 1 migrate out api --to http://git/x.git
adm "migrate out an unknown project (exit 2)" 2 migrate out nosuch --to https://git/x.git
al "non-admin cannot migrate out (exit 2)" 2 /home/sg migrate out api --to https://git/x.git
adm "round trip: migrate in tool as rt" 0 migrate in https://git/tool.git rt
adm "round trip: migrate out rt" 0 migrate out rt --to https://git/round.git
eq "round trip keeps the SHA" "$(gitsha round)" "$(gitsha tool)"
runo server /data sg --json migrate in https://git/api.git api6 --into core:services/api6
eq "migrate in --json" "$(jq -r .project <<<"$OUT" 2>/dev/null)" api6

# --- server restart and persistence -------------------------------------------

section "server restart"
runo alice /home/sg sg --json log --project portal -n 1
HEAD_BEFORE=$(jq -r '.commits[0].sha' <<<"$OUT")
"${D[@]}" restart server >/dev/null 2>&1
wait_healthy server && pass "server restarted" || fail "server did not restart"
runo alice /home/sg sg --json log --project portal -n 1
eq "portal head survives a restart" "$(jq -r '.commits[0].sha' <<<"$OUT")" "$HEAD_BEFORE"
put alice $W/lib/after.txt "after restart"
al "land after restart" 0 $W commit -m "after restart" --land
adm "admin token still works" 0 repo list

# --- unmount, delete ----------------------------------------------------------

section "unmount and delete"
adm "unmount public" 0 project unmount portal public
al "sync after unmount" 0 $W sync
run alice $W ls
hasnt "public is gone from the working copy" '^public$'
adm "alias rm" 0 project alias rm www
adm "removed alias no longer resolves (exit 2)" 2 project show www
adm "repo delete of a mounted repo is refused" 1 repo delete core --yes
has "mounted by" 'mounted by|history of'
adm "project delete without --yes refuses" 1 project delete hello-world
adm "project delete --yes" 0 project delete hello-world --yes
adm "repo delete after its project is gone" 0 repo delete hello-world --yes
adm "user delete alice" 0 user delete alice
al "deleted alice is refused (exit 2)" 2 $W sync
adm "token create reuses a deleted name as a new user" 0 token create alice
runo server /data sg --json acl list --user alice
eq "new alice has no old access" "$(jq '.acl|length' <<<"$OUT" 2>/dev/null)" 0

# --- logs ---------------------------------------------------------------------

section "logs"
"${D[@]}" logs server cache >/tmp/sgit-e2e-logs.txt 2>&1
if grep -Eiq 'panic|fatal|data race' /tmp/sgit-e2e-logs.txt; then
	fail "server/cache logs have a panic" "$(grep -Ei 'panic|fatal|data race' /tmp/sgit-e2e-logs.txt | head)"
else
	pass "no panics in server or cache logs"
fi

printf '\n%d passed, %d failed\n' "$PASS" "${#FAILED[@]}"
for f in "${FAILED[@]}"; do printf '  FAIL  %s\n' "$f"; done
[ "${#FAILED[@]}" = 0 ]
