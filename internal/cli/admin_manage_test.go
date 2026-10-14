package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/cli"
)

func rows(out string) [][]string {
	var r [][]string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		r = append(r, strings.Fields(l))
	}
	return r
}

func TestUserCommands(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.ok(dir, "token", "create", "bob")
	var tok struct{ ID string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	h.ok(dir, "token", "revoke", tok.ID)

	r := rows(h.ok(dir, "user", "list"))
	if len(r) != 3 || strings.Join(r[0], " ") != "NAME EMAIL ADMIN TOKENS CREATED" ||
		r[1][0] != "admin" || r[1][2] != "yes" || r[1][3] != "1" ||
		r[2][0] != "bob" || r[2][1] != "bob@sgit.local" || r[2][2] != "no" || r[2][3] != "1" {
		t.Fatalf("user list = %v", r)
	}
	var list struct {
		Users []struct {
			Name    string
			Admin   bool
			Tokens  int
			Created string
		}
	}
	decode(t, h.ok(dir, "user", "list", "--json"), &list)
	if len(list.Users) != 2 || list.Users[1].Name != "bob" || list.Users[1].Tokens != 1 || list.Users[1].Created == "" {
		t.Fatalf("user list --json = %+v", list)
	}

	if out := h.ok(dir, "user", "promote", "bob"); !strings.Contains(out, "promoted bob") {
		t.Fatalf("promote printed %q", out)
	}
	if r := rows(h.ok(dir, "user", "list")); r[2][2] != "yes" {
		t.Fatalf("after promote: %v", r)
	}
	if out := h.ok(dir, "user", "demote", "bob", "--json"); !strings.Contains(out, `"admin": false`) {
		t.Fatalf("demote --json = %q", out)
	}
	code, out, errs := h.run(dir, "user", "demote", "admin", "--json")
	if code != cli.ExitError || !strings.Contains(errs, "last admin") || !strings.Contains(out, `"exit_code":1`) {
		t.Fatalf("demote last admin: exit %d out %q err %q", code, out, errs)
	}
	if code, _, errs := h.run(dir, "user", "promote", "ghost"); code != cli.ExitError || !strings.Contains(errs, `user "ghost" not found`) {
		t.Fatalf("promote ghost: exit %d err %q", code, errs)
	}
}

func TestUserDelete(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "hi\n"})
	dir := t.TempDir()
	var tok struct{ Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	h.loginAs("bob", tok.Token)
	h.ok(dir, "acl", "grant", "bob", "web", "write")

	code, _, errs := h.run(dir, "user", "delete", "admin")
	if code != cli.ExitError || !strings.Contains(errs, "yourself") {
		t.Fatalf("self delete: exit %d err %q", code, errs)
	}
	out := h.ok(dir, "user", "delete", "bob")
	if !strings.Contains(out, "deleted user bob (revoked 1 tokens, removed 1 ACLs, abandoned 0 drafts)") {
		t.Fatalf("delete printed %q", out)
	}
	if code, _, _ := h.runAs("bob", dir, "log", "--repo", "web"); code != cli.ExitAuth {
		t.Fatalf("deleted user's token: exit %d, want %d", code, cli.ExitAuth)
	}
	if r := rows(h.ok(dir, "user", "list")); len(r) != 2 {
		t.Fatalf("user list after delete = %v", r)
	}
	if code, _, errs := h.run(dir, "user", "delete", "bob"); code != cli.ExitError || !strings.Contains(errs, "not found") {
		t.Fatalf("delete twice: exit %d err %q", code, errs)
	}
}

func TestTokenList(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	var a, b struct{ ID, Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &a)
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &b)
	h.ok(dir, "token", "revoke", a.ID)

	out := h.ok(dir, "token", "list", "bob")
	r := rows(out)
	if len(r) != 3 || strings.Join(r[0], " ") != "ID USER CREATED STATUS" {
		t.Fatalf("token list = %q", out)
	}
	status := map[string]string{}
	for _, row := range r[1:] {
		if len(row) != 4 || row[1] != "bob" {
			t.Fatalf("row = %v", row)
		}
		status[row[0]] = row[3]
	}
	if status[a.ID] != "revoked" || status[b.ID] != "active" {
		t.Fatalf("status = %v", status)
	}
	if strings.Contains(out, "sgt_") || strings.Contains(out, a.Token) || strings.Contains(out, b.Token) {
		t.Fatalf("token secret leaked: %q", out)
	}
	if all := rows(h.ok(dir, "token", "list")); len(all) != 4 { // + admin's
		t.Fatalf("token list (all) = %v", all)
	}
	var j struct {
		Tokens []struct{ ID, User, Created, Status, Revoked string }
	}
	js := h.ok(dir, "token", "list", "bob", "--json")
	decode(t, js, &j)
	if len(j.Tokens) != 2 || strings.Contains(js, "sgt_") {
		t.Fatalf("token list --json = %s", js)
	}
	for _, tk := range j.Tokens {
		if (tk.Status == "revoked") != (tk.Revoked != "") || tk.Created == "" {
			t.Fatalf("token = %+v", tk)
		}
	}
	if code, _, errs := h.run(dir, "token", "list", "ghost"); code != cli.ExitError || !strings.Contains(errs, "not found") {
		t.Fatalf("list ghost: exit %d err %q", code, errs)
	}
}

func TestACLList(t *testing.T) {
	h := newHarness(t)
	h.seed(nil)
	dir := t.TempDir()
	h.ok(dir, "repo", "create", "api")
	h.ok(dir, "token", "create", "bob")
	h.ok(dir, "token", "create", "carol")
	h.ok(dir, "acl", "grant", "bob", "web", "write")
	h.ok(dir, "acl", "grant", "bob", "api", "read")
	h.ok(dir, "acl", "grant", "carol", "web", "read")

	r := rows(h.ok(dir, "acl", "list"))
	if len(r) != 4 || strings.Join(r[0], " ") != "USER REPO LEVEL" || strings.Join(r[1], " ") != "bob api read" || strings.Join(r[2], " ") != "bob web write" {
		t.Fatalf("acl list = %v", r)
	}
	if r := rows(h.ok(dir, "acl", "list", "--repo", "web")); len(r) != 3 {
		t.Fatalf("by repo = %v", r)
	}
	if r := rows(h.ok(dir, "acl", "list", "--user", "carol", "--repo", "web")); len(r) != 2 || strings.Join(r[1], " ") != "carol web read" {
		t.Fatalf("by user+repo = %v", r)
	}
	var j struct {
		ACL []struct{ User, Repo, Level string }
	}
	decode(t, h.ok(dir, "acl", "list", "--user", "bob", "--json"), &j)
	if len(j.ACL) != 2 || j.ACL[0].Level != "read" {
		t.Fatalf("acl list --json = %+v", j)
	}
	if code, _, _ := h.run(dir, "acl", "list", "--repo", "nope"); code != cli.ExitError {
		t.Fatalf("unknown repo exit %d", code)
	}
}

func TestRepoDelete(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "hi\n"})
	dir := t.TempDir()
	h.ok(dir, "token", "create", "bob")
	h.ok(dir, "acl", "grant", "bob", "web", "read")

	code, _, errs := h.run(dir, "repo", "delete", "web")
	if code != cli.ExitError || !strings.Contains(errs, "--yes") {
		t.Fatalf("no --yes: exit %d err %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(h.env.DataDir, "repos", "web.git")); err != nil {
		t.Fatalf("repo must be untouched: %v", err)
	}
	h.ok(dir, "repo", "status", "web")

	out := h.ok(dir, "repo", "delete", "web", "--yes")
	if !strings.HasPrefix(out, "deleted repo web (git data moved to ") || !strings.Contains(out, filepath.Join(h.env.DataDir, "deleted", "web.")) {
		t.Fatalf("delete printed %q", out)
	}
	if code, _, _ := h.run(dir, "repo", "status", "web"); code != cli.ExitError {
		t.Fatalf("status after delete: exit %d", code)
	}
	if r := rows(h.ok(dir, "acl", "list")); len(r) != 1 {
		t.Fatalf("ACLs remain: %v", r)
	}
	if code, _, errs := h.run(dir, "repo", "delete", "web", "--yes"); code != cli.ExitError || !strings.Contains(errs, "not found") {
		t.Fatalf("delete twice: exit %d err %q", code, errs)
	}
	h.ok(dir, "repo", "create", "web") // name reusable
	var j struct {
		Repo    string
		MovedTo string `json:"moved_to"`
	}
	decode(t, h.ok(dir, "repo", "delete", "web", "--yes", "--json"), &j)
	if j.Repo != "web" || j.MovedTo == "" {
		t.Fatalf("--json = %+v", j)
	}
}

func TestNewAdminCommandsNeedAdmin(t *testing.T) {
	h := newHarness(t)
	h.seed(nil)
	dir := t.TempDir()
	var tok struct{ Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	h.loginAs("bob", tok.Token)
	for _, args := range [][]string{
		{"user", "list"},
		{"user", "promote", "bob"},
		{"user", "demote", "admin"},
		{"user", "delete", "admin"},
		{"token", "list"},
		{"acl", "list"},
		{"repo", "delete", "web", "--yes"},
	} {
		if code, _, _ := h.runAs("bob", dir, args...); code != cli.ExitAuth {
			t.Errorf("sg %v as bob: exit %d, want %d", args, code, cli.ExitAuth)
		}
	}
	h.ok(dir, "repo", "status", "web")
}

func TestNewAdminUsageErrors(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"user"}, {"user", "frob"}, {"user", "list", "x"}, {"user", "promote"}, {"user", "delete", "a", "b"},
		{"token", "list", "a", "b"}, {"acl", "list", "x"}, {"repo", "delete"},
	} {
		code, _, errs := h.run(dir, args...)
		if code != cli.ExitError || !strings.Contains(errs, "usage:") {
			t.Errorf("sg %v: exit %d stderr %q, want usage error", args, code, errs)
		}
	}
	code, out, _ := h.run(dir, "help")
	if code != 0 || !strings.Contains(out, "sg user list") || !strings.Contains(out, "token list") ||
		!strings.Contains(out, "acl list") || !strings.Contains(out, "repo delete <name> --yes") || !strings.Contains(out, "implicit access") {
		t.Fatalf("help = %q", out)
	}
}
