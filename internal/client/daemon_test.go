package client

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultSocket(t *testing.T) {
	t.Setenv("SG_SOCKET", "/x/explicit.sock")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	if got := DefaultSocket(); got != "/x/explicit.sock" {
		t.Fatalf("SG_SOCKET not honoured: %s", got)
	}
	t.Setenv("SG_SOCKET", "")
	if got := DefaultSocket(); got != "/run/user/1/sg/sgd.sock" {
		t.Fatalf("XDG_RUNTIME_DIR: %s", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := DefaultSocket(); !strings.HasSuffix(got, "/sgd.sock") || !strings.Contains(got, "sg-") {
		t.Fatalf("tmp fallback: %s", got)
	}
}

func TestCheckOwned(t *testing.T) {
	dir := t.TempDir()
	uid := os.Getuid()
	if err := os.Chmod(dir, 0o700); err != nil { // t.TempDir honours the umask (may be group-writable)
		t.Fatal(err)
	}
	if err := CheckOwned(dir, uid); err != nil {
		t.Fatalf("own 0700 dir: %v", err)
	}
	if err := CheckOwned(dir, uid+1); err == nil {
		t.Fatal("wrong owner must be refused")
	}
	open := filepath.Join(dir, "open")
	_ = os.Mkdir(open, 0o700)
	_ = os.Chmod(open, 0o777)
	if err := CheckOwned(open, uid); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("world-writable dir: %v", err)
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(dir, link)
	if err := CheckOwned(link, uid); err == nil {
		t.Fatal("symlink must be refused")
	}
	if err := CheckOwned(filepath.Join(dir, "missing"), uid); err == nil {
		t.Fatal("missing path must be an error")
	}
}

func TestDialDaemonEdgeCases(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "sgdd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if conn, err := DialDaemon(ctx, filepath.Join(dir, "none.sock"), "s:1", "sgt_x"); conn != nil || err != nil {
		t.Fatalf("missing socket: %v %v", conn, err)
	}
	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DialDaemon(ctx, file, "s:1", "sgt_x"); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("a regular file is not a usable socket: %v", err)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(open, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := DialDaemon(ctx, filepath.Join(open, "s.sock"), "s:1", "sgt_x"); err == nil || errors.Is(err, ErrOtherUpstream) || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("socket in a world-writable dir must be refused: %v", err)
	}
}
