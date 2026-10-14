package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/server"
)

type repoStatusJSON struct {
	Name          string `json:"name"`
	Main          string `json:"main"`
	Created       string `json:"created"`
	MirrorURL     string `json:"mirror_url"`
	MirrorLast    string `json:"mirror_last"`
	MirrorPending bool   `json:"mirror_pending"`
	MirrorError   string `json:"mirror_error"`
	MirrorAttempt string `json:"mirror_attempt"`
}

func decode(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}
}

func TestRepoCreateStatusList(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	if out := h.ok(dir, "repo", "create", "web"); !strings.HasPrefix(out, "created web (main ") {
		t.Fatalf("repo create printed %q", out)
	}
	var st repoStatusJSON
	decode(t, h.ok(dir, "repo", "status", "web", "--json"), &st)
	if st.Name != "web" || len(st.Main) != 40 || st.Created == "" || st.MirrorURL != "" || st.MirrorPending {
		t.Fatalf("repo status = %+v", st)
	}
	if out := h.ok(dir, "repo", "status", "web"); !strings.Contains(out, "mirror:   none") {
		t.Fatalf("repo status text = %q", out)
	}
	out := h.ok(dir, "repo", "list")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.HasPrefix(lines[1], "web") || !strings.HasSuffix(lines[1], "-") {
		t.Fatalf("repo list = %q", out)
	}
	var list struct{ Repos []repoStatusJSON }
	decode(t, h.ok(dir, "repo", "list", "--json"), &list)
	if len(list.Repos) != 1 || list.Repos[0].Name != "web" {
		t.Fatalf("repo list --json = %+v", list)
	}
	if code, _, _ := h.run(dir, "repo", "create", "web"); code != cli.ExitError {
		t.Fatalf("duplicate create exit = %d, want %d", code, cli.ExitError)
	}
}

func TestRepoImportWithMirror(t *testing.T) {
	h := newHarness(t, func(c *server.Config) {
		c.AllowLocalRemotes = true
		c.MirrorBackoffMin = 5 * time.Millisecond
		c.MirrorBackoffMax = 50 * time.Millisecond
	})
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.git")
	r, err := gitx.Init(ctx, src, gitx.Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	main, _ := r.ReadRef(ctx, gitx.MainRef)
	r.Close()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	if err := gitx.InitBare(ctx, mirror); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := h.ok(dir, "repo", "import", "app", "--from", "file://"+src, "--mirror", "file://"+mirror)
	if !strings.HasPrefix(out, "imported app from file://"+src+" (main ") {
		t.Fatalf("repo import printed %q", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var st repoStatusJSON
		decode(t, h.ok(dir, "repo", "status", "app", "--json"), &st)
		if st.MirrorLast == main && !st.MirrorPending && st.MirrorError == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror never caught up: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := h.ok(dir, "repo", "status", "app")
	for _, want := range []string{"mirror:   file://" + mirror, "  last:    " + main, "  pending: no"} {
		if !strings.Contains(text, want) {
			t.Errorf("repo status text lacks %q:\n%s", want, text)
		}
	}
	if out := h.ok(dir, "repo", "list"); !strings.Contains(out, "ok") {
		t.Errorf("repo list = %q, want mirror column ok", out)
	}
}

func TestTokenAndACLCommands(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"a.txt": "hi\n"})
	dir := t.TempDir()

	var tok struct{ ID, User, Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	if tok.User != "bob" || tok.ID == "" || !strings.HasPrefix(tok.Token, "sgt_") {
		t.Fatalf("token create = %+v", tok)
	}
	h.loginAs("bob", tok.Token)
	bobLog := func() int {
		code, _, _ := h.runAs("bob", dir, "log", "--project", "web")
		return code
	}
	if code := bobLog(); code != cli.ExitAuth {
		t.Fatalf("log without ACL exit = %d, want %d", code, cli.ExitAuth)
	}
	h.ok(dir, "acl", "grant", "bob", "web", "read")
	if code := bobLog(); code != cli.ExitOK {
		t.Fatalf("log with read exit = %d", code)
	}
	h.ok(dir, "acl", "revoke", "bob", "web")
	if code := bobLog(); code != cli.ExitAuth {
		t.Fatalf("log after acl revoke exit = %d, want %d", code, cli.ExitAuth)
	}
	h.ok(dir, "acl", "grant", "bob", "web", "write")
	if out := h.ok(dir, "token", "revoke", tok.ID); !strings.Contains(out, tok.ID) {
		t.Fatalf("token revoke printed %q", out)
	}
	if code := bobLog(); code != cli.ExitAuth {
		t.Fatalf("log after token revoke exit = %d, want %d", code, cli.ExitAuth)
	}

	code, out, errs := h.run(dir, "token", "create", "carol")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "sgt_") {
		t.Fatalf("token create text: exit %d stdout %q", code, out)
	}
	if !strings.Contains(errs, "for carol (shown only once)") {
		t.Fatalf("token create stderr = %q", errs)
	}
}

func TestAdminCommandsNeedAdmin(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	var tok struct{ Token string }
	decode(t, h.ok(dir, "token", "create", "bob", "--json"), &tok)
	h.loginAs("bob", tok.Token)
	for _, args := range [][]string{
		{"repo", "create", "x"},
		{"repo", "list"},
		{"token", "create", "eve"},
		{"acl", "grant", "bob", "x", "read"},
	} {
		if code, _, _ := h.runAs("bob", dir, args...); code != cli.ExitAuth {
			t.Errorf("sg %v as bob: exit %d, want %d", args, code, cli.ExitAuth)
		}
	}
}

func TestAdminUsageErrors(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"repo"},
		{"repo", "frob"},
		{"repo", "create"},
		{"repo", "import", "x"},
		{"repo", "status"},
		{"token"},
		{"token", "create"},
		{"token", "revoke"},
		{"acl", "grant", "bob", "web"},
		{"acl", "revoke", "bob"},
	} {
		code, _, errs := h.run(dir, args...)
		if code != cli.ExitError || !strings.Contains(errs, "usage:") {
			t.Errorf("sg %v: exit %d stderr %q, want usage error", args, code, errs)
		}
	}
	if code, _, _ := h.run(dir, "acl", "grant", "bob", "web", "admin"); code != cli.ExitError {
		t.Errorf("acl grant with a bad level: exit %d, want %d", code, cli.ExitError)
	}
}

func TestAdminCommandsSkipDaemon(t *testing.T) {
	h := newHarness(t)
	sock := filepath.Join(t.TempDir(), "sgd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil { // exists, but is not a socket
		t.Fatal(err)
	}
	t.Setenv("SG_SOCKET", sock)
	code, _, errs := h.run(t.TempDir(), "repo", "list")
	if code != 0 || strings.Contains(errs, "sgd") {
		t.Fatalf("repo list: exit %d stderr %q; admin commands must not touch sgd", code, errs)
	}
}

func TestRepoListBroken(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.ok(dir, "repo", "create", "a")
	h.ok(dir, "repo", "create", "b")
	if err := os.RemoveAll(filepath.Join(h.env.DataDir, "repos", "b.git")); err != nil {
		t.Fatal(err)
	}
	out := h.ok(dir, "repo", "list")
	found := false
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(l, "b") {
			f := strings.Fields(l)
			if len(f) != 3 || f[1] != "-" || f[2] != "broken" {
				t.Fatalf("broken line = %q", l)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("repo list lacks b: %q", out)
	}
	var list struct {
		Repos []struct{ Name, Main, Error string }
	}
	decode(t, h.ok(dir, "repo", "list", "--json"), &list)
	ok := false
	for _, r := range list.Repos {
		if r.Name == "b" {
			ok = r.Error != "" && r.Main == ""
		}
	}
	if !ok {
		t.Fatalf("repo list --json = %+v", list)
	}
}

func TestTokenCreateJSONClean(t *testing.T) {
	h := newHarness(t)
	code, out, errs := h.run(t.TempDir(), "token", "create", "dave", "--json")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&v); err != io.EOF {
		t.Fatalf("second Decode = %v, want io.EOF", err)
	}
}

func TestRepoCreateFlagAfterPositional(t *testing.T) {
	h := newHarness(t, func(c *server.Config) { c.AllowLocalRemotes = true })
	mirror := filepath.Join(t.TempDir(), "m.git")
	if err := gitx.InitBare(context.Background(), mirror); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h.ok(dir, "repo", "create", "web2", "--mirror", "file://"+mirror)
	var st repoStatusJSON
	decode(t, h.ok(dir, "repo", "status", "web2", "--json"), &st)
	if st.MirrorURL != "file://"+mirror {
		t.Fatalf("mirror_url = %q", st.MirrorURL)
	}
}
