package client

import (
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
