package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/cli"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
)

type harness struct {
	t   *testing.T
	env *testenv.Env
	cfg map[string]string
}

func newHarness(t *testing.T, opts ...func(*server.Config)) *harness {
	t.Helper()
	t.Setenv("SG_TOKEN", "")
	h := &harness{t: t, env: testenv.Start(t, opts...), cfg: map[string]string{}}
	h.loginAs("admin", h.env.AdminToken)
	return h
}

func (h *harness) loginAs(user, token string) {
	h.t.Helper()
	h.cfg[user] = h.t.TempDir()
	if code, _, errs := h.runAs(user, h.t.TempDir(), "login", h.env.Addr, "--token", token, "--dev-insecure"); code != 0 {
		h.t.Fatalf("login %s: exit %d: %s", user, code, errs)
	}
}

func (h *harness) runStdin(user, dir, stdin string, args ...string) (int, string, string) {
	h.t.Setenv("SG_CONFIG_DIR", h.cfg[user])
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), args, cli.IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Dir: dir})
	return code, out.String(), errb.String()
}

func (h *harness) runAs(user, dir string, args ...string) (int, string, string) {
	return h.runStdin(user, dir, "", args...)
}

func (h *harness) run(dir string, args ...string) (int, string, string) {
	return h.runAs("admin", dir, args...)
}

func (h *harness) okAs(user, dir string, args ...string) string {
	h.t.Helper()
	code, out, errs := h.runAs(user, dir, args...)
	if code != 0 {
		h.t.Fatalf("sg %s (as %s): exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), user, code, out, errs)
	}
	return out
}

func (h *harness) ok(dir string, args ...string) string {
	h.t.Helper()
	return h.okAs("admin", dir, args...)
}

// seed creates repo "web" with files committed directly on main and returns main.
func (h *harness) seed(files map[string]string) string {
	h.t.Helper()
	h.env.CreateRepo("web")
	return h.env.CommitDirect("web", files, "seed")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
