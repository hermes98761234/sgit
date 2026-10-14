package cli_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/daemon"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func (h *harness) startDaemon(opts ...func(*daemon.Config)) *daemontest.Env {
	h.t.Helper()
	d := daemontest.Start(h.t, h.env.Addr, opts...)
	h.t.Setenv("SG_SOCKET", d.Socket)
	return d
}

func TestCheckoutUsesDaemonCache(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	d := h.startDaemon()
	root := t.TempDir()
	code, _, errs := h.run(root, "checkout", "web")
	if code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("checkout via sgd: exit %d %q", code, errs)
	}
	sawInfo, sawRepo := false, false
	for _, c := range d.SeenCalls() {
		switch {
		case c.Method == "/sgit.v1.Daemon/Info":
			sawInfo = true
			if c.Auth != "" {
				t.Fatalf("Info carried a token: %+v", c)
			}
		case strings.HasPrefix(c.Method, "/sgit.v1.Project/"):
			sawRepo = true
			if !strings.HasPrefix(c.Auth, "Bearer ") {
				t.Fatalf("Repo call without token: %+v", c)
			}
		}
	}
	if !sawInfo || !sawRepo {
		t.Fatalf("expected Info and Repo calls, saw %+v", d.SeenCalls())
	}
	cold := d.Counter.Total()
	if cold == 0 {
		t.Fatal("first checkout must fetch through sgd")
	}
	h.ok(root, "checkout", "web", "second")
	if d.Counter.Total() != cold {
		t.Fatalf("second checkout went upstream: %d -> %d", cold, d.Counter.Total())
	}
	w := filepath.Join(root, "web")
	writeFile(t, filepath.Join(w, "a.txt"), "via sgd\n")
	h.ok(w, "commit", "-m", "through the daemon", "--land")
	if h.env.Files("web", "main")["a.txt"] != "via sgd\n" {
		t.Fatal("land through sgd did not reach main")
	}
}

func TestUnusableDaemonWarnsAndFallsBack(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	dir := testenv.Must(os.MkdirTemp("", "sgdf"))
	t.Cleanup(func() { os.RemoveAll(dir) })
	stale := filepath.Join(dir, "s.sock")
	_ = os.WriteFile(stale, nil, 0o600)
	t.Setenv("SG_SOCKET", stale)
	code, _, errs := h.run(t.TempDir(), "checkout", "web")
	if code != 0 || !strings.Contains(errs, "warning") || !strings.Contains(errs, "connecting directly") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestDeadDaemonSocketWarnsAndFallsBack(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	dir := testenv.Must(os.MkdirTemp("", "sgdd"))
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "s.sock")
	l := testenv.Must(net.Listen("unix", dead))
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if fi := testenv.Must(os.Lstat(dead)); fi.Mode()&os.ModeSocket == 0 {
		t.Fatal("dead socket file missing")
	}
	t.Setenv("SG_SOCKET", dead)
	code, _, errs := h.run(t.TempDir(), "checkout", "web")
	if code != 0 || !strings.Contains(errs, "warning") || !strings.Contains(errs, "connecting directly") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestDaemonForOtherServerIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	d := h.startDaemon(func(c *daemon.Config) { c.Upstream = "other.example:7419" })
	code, _, errs := h.run(t.TempDir(), "checkout", "web")
	if code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if d.Counter.Total() != 0 {
		t.Fatal("sgd fronting another server must not be used")
	}
	for _, c := range d.SeenCalls() {
		if c.Auth != "" {
			t.Fatalf("token leaked to sgd fronting another server: %+v", c)
		}
	}
}

func TestNoDaemonEnvDisablesSgd(t *testing.T) {
	h := newHarness(t)
	h.seed(seedFiles)
	d := h.startDaemon()
	t.Setenv("SG_NO_DAEMON", "1")
	h.ok(t.TempDir(), "checkout", "web")
	if d.Counter.Total() != 0 {
		t.Fatal("SG_NO_DAEMON=1 must bypass sgd")
	}
}

func TestProjectsThroughDaemon(t *testing.T) {
	h, _ := coreProject(t)
	h.env.CreateProject("other")
	d := h.startDaemon()
	var res struct {
		Projects []struct {
			Name  string `json:"name"`
			Level string `json:"level"`
		} `json:"projects"`
	}
	decode(t, h.okAs("bob", t.TempDir(), "projects", "--json"), &res)
	if len(res.Projects) != 1 || res.Projects[0].Name != "web" || res.Projects[0].Level != "write" {
		t.Fatalf("bob projects via sgd = %+v", res)
	}
	saw := false
	for _, c := range d.SeenCalls() {
		if c.Method == "/sgit.v1.Project/ListProjects" {
			saw = true
			if !strings.HasPrefix(c.Auth, "Bearer ") {
				t.Fatalf("ListProjects via sgd without token: %+v", c)
			}
		}
	}
	if !saw {
		t.Fatalf("sg projects did not go through sgd: %+v", d.SeenCalls())
	}
}
