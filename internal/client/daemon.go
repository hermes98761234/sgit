package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"google.golang.org/grpc"
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

// ErrOtherUpstream means sgd is running but fronts a different server.
var ErrOtherUpstream = errors.New("sgd fronts a different server")

// DialDaemon connects to sgd at socket when it fronts server. It returns
// (nil, nil) when no socket exists and ErrOtherUpstream when sgd fronts
// another server; any other error means the socket exists but is unusable
// or unsafe. The Info probe carries no token: the token is only sent once
// sgd has confirmed the upstream.
func DialDaemon(ctx context.Context, socket, server, token string) (*grpc.ClientConn, error) {
	if socket == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(socket)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uid := os.Getuid()
	if err := CheckOwned(filepath.Dir(abs), uid); err != nil {
		return nil, err
	}
	if err := CheckOwned(abs, uid); err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%s is not a socket", abs)
	}
	probe, err := grpc.NewClient("unix://"+abs, rpc.DialOptions("", nil)...)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	info, err := sgitv1.NewDaemonClient(probe).Info(pctx, &sgitv1.DaemonInfoRequest{})
	cancel()
	probe.Close()
	if err != nil {
		return nil, err
	}
	if info.Upstream != server {
		return nil, ErrOtherUpstream
	}
	return grpc.NewClient("unix://"+abs, rpc.DialOptions(token, nil)...)
}
