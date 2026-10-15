package servercmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"google.golang.org/grpc"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	code, out, errs := run(t, "init", "--data", dir)
	if code != 0 || !strings.Contains(out, "sgt_") {
		t.Fatalf("init: code=%d out=%q err=%q", code, out, errs)
	}
	st, err := os.Stat(filepath.Join(dir, "admin-token"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("admin-token: %v %v", st, err)
	}
	code, out, _ = run(t, "init", "--data", dir)
	if code != 0 || !strings.Contains(out, "already initialized") {
		t.Fatalf("second init: code=%d out=%q", code, out)
	}
}

func TestServeRefusesUnsafeConfig(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	if code, _, errs := run(t, "serve", "--data", dir, "--listen", "127.0.0.1:0"); code != 2 || !strings.Contains(errs, "tls") {
		t.Fatalf("no TLS: code=%d err=%q", code, errs)
	}
	if code, _, errs := run(t, "serve", "--data", dir, "--listen", "0.0.0.0:0", "--dev-insecure"); code != 2 || !strings.Contains(errs, "loopback") {
		t.Fatalf("insecure non-loopback: code=%d err=%q", code, errs)
	}
	if code, _, errs := run(t, "serve", "--data", t.TempDir(), "--listen", "127.0.0.1:0", "--dev-insecure"); code != 1 || !strings.Contains(errs, "init") {
		t.Fatalf("uninitialized: code=%d err=%q", code, errs)
	}
	if code, _, _ := run(t, "bogus"); code != 2 {
		t.Fatalf("unknown subcommand: code=%d", code)
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServeTLSAndHealth(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if code, _, errs := run(t, "gen-cert", "--hosts", "localhost,127.0.0.1", "--out-cert", cert, "--out-key", key); code != 0 {
		t.Fatalf("gen-cert: %s", errs)
	}
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		var out, errb bytes.Buffer
		done <- Run(ctx, []string{"serve", "--data", dir, "--listen", addr, "--tls-cert", cert, "--tls-key", key}, &out, &errb)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, out, _ := run(t, "health", "--addr", addr)
		if code == 0 && strings.Contains(out, "SERVING") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became healthy")
		}
		time.Sleep(100 * time.Millisecond)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, "admin-token"))
	pemBytes, _ := os.ReadFile(cert)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	conn, err := grpc.NewClient(addr, rpc.DialOptions(strings.TrimSpace(string(tok)), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	who, err := sgitv1.NewAdminClient(conn).WhoAmI(context.Background(), &sgitv1.WhoAmIRequest{})
	if err != nil || who.User != "admin" || !who.Admin {
		t.Fatalf("WhoAmI over TLS = %+v, %v", who, err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestServeRejectsBadMaxBlob(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	for _, v := range []string{"abc", "0", "-5"} {
		t.Setenv("SG_MAX_BLOB_MB", v)
		if code, _, errs := run(t, "serve", "--data", dir, "--listen", "127.0.0.1:0", "--dev-insecure"); code != 2 || !strings.Contains(errs, "MAX_BLOB") {
			t.Errorf("env %q: code=%d err=%q", v, code, errs)
		}
	}
	t.Setenv("SG_MAX_BLOB_MB", "100")
	if code, _, errs := run(t, "serve", "--data", dir, "--listen", "127.0.0.1:0", "--dev-insecure", "--max-blob-mb", "0"); code != 2 || !strings.Contains(errs, "MAX_BLOB") {
		t.Errorf("flag 0: code=%d err=%q", code, errs)
	}
}

// startServe runs serve on a free loopback port until the test ends.
func startServe(t *testing.T, dir string) string {
	t.Helper()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		var out, errb bytes.Buffer
		done <- Run(ctx, []string{"serve", "--data", dir, "--listen", addr, "--dev-insecure"}, &out, &errb)
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, out, _ := run(t, "health", "--addr", addr, "--dev-insecure"); code == 0 && strings.Contains(out, "SERVING") {
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became healthy")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A second server on the same data dir would race the first on the database
// and the repos (the land locks are in-process), so it must refuse to start.
func TestServeRefusesDataDirInUse(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	startServe(t, dir)
	// Without the lock the second server would run until ctx ends.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	code := Run(ctx, []string{"serve", "--data", dir, "--listen", freePort(t), "--dev-insecure"}, &out, &errb)
	errs := errb.String()
	if code != 1 || !strings.Contains(errs, "another sg-server") || !strings.Contains(errs, dir) {
		t.Fatalf("second serve: code=%d err=%q", code, errs)
	}
}

// sg.db holds token hashes and the user list: owner-only, WAL files too.
func TestDatabaseFilesOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	run(t, "init", "--data", dir)
	startServe(t, dir)
	for _, f := range []string{"sg.db", "sg.db-wal", "sg.db-shm"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			if f != "sg.db" && os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", f, st.Mode().Perm())
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		code, out, errs := run(t, arg)
		if code != 0 || !strings.Contains(out, "usage: sg-server") || strings.Contains(errs, "unknown") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", arg, code, out, errs)
		}
	}
}
