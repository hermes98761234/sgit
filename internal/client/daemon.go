package client

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultSocket is where sgd listens and sg looks for it: $SG_SOCKET, else
// $XDG_RUNTIME_DIR/sg/sgd.sock, else <tmp>/sg-<uid>/sgd.sock.
func DefaultSocket() string {
	if s := os.Getenv("SG_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sg", "sgd.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("sg-%d", os.Getuid()), "sgd.sock")
}

// CheckOwned verifies that path is not a symlink, is owned by uid and, if it
// is a directory, is not writable by group or others. It guards the sgd
// socket against other local users (who could otherwise collect tokens).
func CheckOwned(path string, uid int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine owner", path)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, uid)
	}
	if fi.IsDir() && fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others", path)
	}
	return nil
}
