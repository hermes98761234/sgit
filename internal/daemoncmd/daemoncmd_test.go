package daemoncmd

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func shortSocket(t *testing.T) string {
	dir := testenv.Must(os.MkdirTemp("", "sgdc"))
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "s.sock")
}

func start(t *testing.T, args ...string) (<-chan int, context.CancelFunc, *syncBuf) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args, out, out) }()
	t.Cleanup(cancel)
	return done, cancel, out
}

func waitInfo(t *testing.T, sock string) *sgitv1.DaemonInfoResponse {
	t.Helper()
	conn := testenv.Must(grpc.NewClient("unix://"+sock, rpc.DialOptions("", nil)...))
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		info, err := sgitv1.NewDaemonClient(conn).Info(ctx, &sgitv1.DaemonInfoRequest{})
		cancel()
		if err == nil {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("sgd never answered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func baseArgs(e *testenv.Env, sock string, t *testing.T) []string {
	return []string{"--upstream", e.Addr, "--dev-insecure", "--socket", sock, "--cache-dir", t.TempDir(), "--cache-max", "64M"}
}

func TestServeInfoModeAndCleanup(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	done, cancel, out := start(t, baseArgs(e, sock, t)...)
	info := waitInfo(t, sock)
	if info.Upstream != e.Addr || info.CacheMaxBytes != 64<<20 {
		t.Fatalf("Info = %+v", info)
	}
	fi := testenv.Must(os.Stat(sock))
	di := testenv.Must(os.Stat(filepath.Dir(sock)))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes: socket %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	second := Run(context.Background(), baseArgs(e, sock, t), &syncBuf{}, &syncBuf{})
	if second != 1 {
		t.Fatalf("second sgd on the same socket: exit %d", second)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sgd did not stop")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket must be removed on shutdown")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	l := testenv.Must(net.Listen("unix", sock))
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	start(t, baseArgs(e, sock, t)...)
	waitInfo(t, sock)
}

func TestRefusesNonSocketAtPath(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	if err := os.WriteFile(sock, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errb syncBuf
	if code := Run(context.Background(), baseArgs(e, sock, t), &syncBuf{}, &errb); code != 1 || !strings.Contains(errb.String(), "not a socket") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if b, err := os.ReadFile(sock); err != nil || string(b) != "precious" {
		t.Fatalf("file must be untouched: %q %v", b, err)
	}
}

func TestTightensLooseSocketDir(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	_ = os.Chmod(filepath.Dir(sock), 0o755)
	start(t, baseArgs(e, sock, t)...)
	waitInfo(t, sock)
	if m := testenv.Must(os.Stat(filepath.Dir(sock))).Mode().Perm(); m != 0o700 {
		t.Fatalf("dir mode %v", m)
	}
}

func TestRefusesUnsafeSocketDir(t *testing.T) {
	e := testenv.Start(t)
	sock := shortSocket(t)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	_ = os.Chmod(filepath.Dir(sock), 0o777)
	var errb syncBuf
	if code := Run(context.Background(), baseArgs(e, sock, t), &syncBuf{}, &errb); code != 1 || !strings.Contains(errb.String(), "writable") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestFlagsValidation(t *testing.T) {
	t.Setenv("SGD_UPSTREAM", "")
	if code := Run(context.Background(), []string{"--socket", shortSocket(t)}, &syncBuf{}, &syncBuf{}); code != 2 {
		t.Fatalf("missing --upstream: exit %d", code)
	}
	if code := Run(context.Background(), []string{"--upstream", "127.0.0.1:1", "--dev-insecure", "--cache-max", "lots", "--socket", shortSocket(t)}, &syncBuf{}, &syncBuf{}); code != 2 {
		t.Fatalf("bad --cache-max: exit %d", code)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"1024": 1024, "1K": 1 << 10, "512M": 512 << 20, "10G": 10 << 30, "2T": 2 << 40, "3g": 3 << 30}
	for in, want := range cases {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-5", "10X", "G", "9999999999T"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) must fail", bad)
		}
	}
}
