package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Group commands print their usage for --help, -h and help, and exit 0.
func TestGroupHelpExitsZero(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	for _, g := range []string{"repo", "project", "user", "token", "acl", "sparse"} {
		for _, flag := range []string{"--help", "-h", "help"} {
			code, out, errs := h.run(dir, g, flag)
			if code != 0 || !strings.Contains(out, "usage: sg "+g) || strings.Contains(errs, "sg:") {
				t.Errorf("sg %s %s: exit %d, stdout %q, stderr %q", g, flag, code, out, errs)
			}
		}
	}
}

// Global flags (--server, --json) work before the command too.
func TestGlobalFlagsBeforeCommand(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	dir := t.TempDir()
	want := h.ok(dir, "repo", "list", "--server", h.env.Addr)
	if out := h.ok(dir, "--server", h.env.Addr, "repo", "list"); out != want {
		t.Fatalf("--server first = %q, want %q", out, want)
	}
	if out := h.ok(dir, "--server="+h.env.Addr, "--json", "projects"); !json.Valid([]byte(out)) {
		t.Fatalf("--json first = %q", out)
	}
	if code, _, errs := h.run(dir, "--bogus", "repo", "list"); code != 1 || !strings.Contains(errs, "--bogus") {
		t.Fatalf("unknown global flag: exit %d %q", code, errs)
	}
	for _, help := range []string{"help", "--help", "-h"} {
		if code, out, errs := h.run(dir, "--json", help); code != 0 || !strings.Contains(out, "usage") {
			t.Errorf("--json %s: exit %d, stdout %q, stderr %q", help, code, out, errs)
		}
	}
	// A flag missing its value must not take a hoisted flag as the value.
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "x\n")
	if code, _, errs := h.run(w, "--json", "commit", "-m"); code == 0 || strings.Contains(h.ok(w, "status"), "draft") {
		t.Fatalf("--json commit -m: exit %d %q; a draft was created", code, errs)
	}
}

// A token piped on stdin is read without printing a prompt.
func TestLoginNoPromptWhenStdinIsNotATerminal(t *testing.T) {
	h := newHarness(t)
	h.cfg["piped"] = t.TempDir()
	code, out, errs := h.runStdin("piped", t.TempDir(), h.env.AdminToken+"\n", "login", h.env.Addr, "--dev-insecure")
	if code != 0 || strings.Contains(out+errs, "token:") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// List commands wrap their --json list in an object, like projects, repos,
// users and tokens do.
func TestJSONListsAreObjects(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	w := h.checkout(t.TempDir())
	writeFile(t, filepath.Join(w, "a.txt"), "x\n")
	h.ok(w, "commit", "-m", "d")
	for key, args := range map[string][]string{
		"commits": {"log"},
		"lines":   {"blame", "a.txt"},
		"drafts":  {"drafts"},
		"sparse":  {"sparse", "list"},
	} {
		var obj map[string]json.RawMessage
		out := h.ok(w, append(args, "--json")...)
		if err := json.Unmarshal([]byte(out), &obj); err != nil || obj[key] == nil || obj[key][0] != '[' {
			t.Errorf("sg %s --json = %s (%v), want an object with list %q", strings.Join(args, " "), out, err, key)
		}
	}
}

// unwrap returns the list under key in a --json object.
func unwrap(t testing.TB, out, key string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("not a JSON object: %q (%v)", out, err)
	}
	return obj[key]
}
