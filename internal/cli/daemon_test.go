package cli_test

import (
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
