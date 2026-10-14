package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func loadCreds(t *testing.T, dir string) creds.File {
	t.Helper()
	t.Setenv("SG_CONFIG_DIR", dir)
	return testenv.Must(creds.Load())
}

func TestLoginStoresCredentials(t *testing.T) {
	h := newHarness(t)
	f := loadCreds(t, h.cfg["admin"])
	s := f.Servers[h.env.Addr]
	if f.Default != h.env.Addr || s.Token != h.env.AdminToken || !s.Insecure {
		t.Fatalf("creds = %+v", f)
	}
	out := h.ok(t.TempDir(), "login", h.env.Addr, "--token", h.env.AdminToken, "--dev-insecure", "--json")
	var res struct {
		Server string `json:"server"`
		User   string `json:"user"`
		Admin  bool   `json:"admin"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.User != "admin" || !res.Admin || res.Server != h.env.Addr {
		t.Fatalf("json = %q (%v)", out, err)
	}
}

func TestLoginTokenFromStdinAndEnv(t *testing.T) {
	h := newHarness(t)
	bob := h.env.NewUser("bob", "", "")
	h.cfg["bob"] = t.TempDir()
	if code, _, errs := h.runStdin("bob", t.TempDir(), bob+"\n", "login", h.env.Addr, "--dev-insecure"); code != 0 {
		t.Fatalf("stdin login: %d %s", code, errs)
	}
	if loadCreds(t, h.cfg["bob"]).Servers[h.env.Addr].Token != bob {
		t.Fatal("token from stdin not stored")
	}
	h.cfg["bob2"] = t.TempDir()
	t.Setenv("SG_TOKEN", bob)
	if code, _, errs := h.runAs("bob2", t.TempDir(), "login", h.env.Addr, "--dev-insecure"); code != 0 {
		t.Fatalf("env login: %d %s", code, errs)
	}
	if loadCreds(t, h.cfg["bob2"]).Servers[h.env.Addr].Token != bob {
		t.Fatal("token from $SG_TOKEN not stored")
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"sgt_bogus", "bogus"} {
		if code, _, _ := h.run(t.TempDir(), "login", h.env.Addr, "--token", tok, "--dev-insecure"); code != 2 {
			t.Errorf("token %q: exit %d, want 2", tok, code)
		}
	}
	if loadCreds(t, h.cfg["admin"]).Servers[h.env.Addr].Token != h.env.AdminToken {
		t.Fatal("a failed login must not overwrite stored credentials")
	}
}

func TestLoginRefusesInsecureRemote(t *testing.T) {
	h := newHarness(t)
	code, _, errs := h.run(t.TempDir(), "login", "10.9.9.9:7419", "--token", "sgt_x", "--dev-insecure")
	if code != 1 || !strings.Contains(errs, "loopback") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestUnknownCommandAndHelp(t *testing.T) {
	h := newHarness(t)
	if code, _, errs := h.run(t.TempDir(), "frobnicate"); code != 1 || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown: %d %q", code, errs)
	}
	if code, out, _ := h.run(t.TempDir(), "help"); code != 0 || !strings.Contains(out, "sg login") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, _ := h.run(t.TempDir()); code != 1 {
		t.Fatalf("no args: exit %d, want 1", code)
	}
	if code, _, errs := h.run(t.TempDir(), "login"); code != 1 || !strings.Contains(errs, "usage") {
		t.Fatalf("login without server: %d %q", code, errs)
	}
}

func TestLoginDoesNotLeakEnvToken(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SG_TOKEN", "sgt_supersecretvalue")
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"login", h.env.Addr, "--bogus"}, 1},
		{[]string{"login", "--help"}, 0},
	} {
		code, out, errs := h.run(t.TempDir(), tc.args...)
		if code != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.args, code, tc.want)
		}
		if strings.Contains(out, "supersecretvalue") || strings.Contains(errs, "supersecretvalue") {
			t.Errorf("%v leaked the env token:\nstdout: %s\nstderr: %s", tc.args, out, errs)
		}
	}
}
