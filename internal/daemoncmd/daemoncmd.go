// Package daemoncmd implements the sgd command line.
package daemoncmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/daemon"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// ParseSize parses "1024", "512M", "10G", ... (powers of 1024).
func ParseSize(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = 1<<10, s[:n-1]
		case 'M':
			mult, s = 1<<20, s[:n-1]
		case 'G':
			mult, s = 1<<30, s[:n-1]
		case 'T':
			mult, s = 1<<40, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid size %q", orig)
	}
	return v * mult, nil
}

func defaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "sg", "objects")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("sg-cache-%d", os.Getuid()))
}

// maxSocketPath is the longest unix socket path bind accepts (sun_path
// minus its terminating NUL: 107 bytes on Linux, 103 on macOS).
var maxSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// probeUpstream checks once that the upstream answers. A TLS failure (no or
// wrong --ca) is fatal: every sg call through sgd would fail with it. An
// upstream that is merely down is not: sgd keeps retrying.
func probeUpstream(ctx context.Context, conn *grpc.ClientConn, upstream string, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		return nil
	}
	msg := status.Convert(err).Message()
	for _, s := range []string{"x509:", "tls:", "handshake"} {
		if strings.Contains(msg, s) {
			return fmt.Errorf("cannot verify the TLS certificate of upstream %s: %s; pass --ca with the server's certificate (PEM), or set SGD_CA", upstream, msg)
		}
	}
	log.Warn("upstream not reachable at startup; sgd keeps trying", "upstream", upstream, "err", msg)
	return nil
}

// listen prepares the socket directory (created 0700 if missing, never
// chmod-ed, must be ours and not group/world-writable), refuses to start when
// a daemon already answers on path, removes a stale socket and listens (0600).
func listen(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is too long (%d bytes; unix socket paths are limited to %d bytes): pass a shorter --socket, or set SG_SOCKET for both sgd and sg", path, len(path), maxSocketPath)
	}
	dir := filepath.Dir(path)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := client.CheckOwned(dir, os.Getuid()); err != nil {
		return nil, fmt.Errorf("unsafe socket directory: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; remove it or choose another --socket", path)
		}
		if err := client.CheckOwned(path, os.Getuid()); err != nil {
			return nil, fmt.Errorf("existing socket %s is not yours", path)
		}
		c, err := net.DialTimeout("unix", path, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return nil, fmt.Errorf("another sgd is already listening on %s", path)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot tell whether sgd is running on %s: %v", path, err)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	old := syscall.Umask(0o177) // create the socket 0600 atomically
	lis, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close()
		return nil, err
	}
	return lis, nil
}

// Run executes sgd and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sgd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	upstream := fs.String("upstream", os.Getenv("SGD_UPSTREAM"), "sg-server address host:port (required)")
	ca := fs.String("ca", os.Getenv("SGD_CA"), "PEM file with the server's (self-signed) certificate")
	insecure := fs.Bool("dev-insecure", false, "plaintext connection to a loopback upstream")
	socket := fs.String("socket", client.DefaultSocket(), "unix socket path")
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "object cache directory")
	cacheMax := fs.String("cache-max", "10G", "cache size limit, e.g. 512M or 10G")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *upstream == "" {
		fmt.Fprintln(stderr, "sgd: --upstream (or $SGD_UPSTREAM) is required")
		return 2
	}
	max, err := ParseSize(*cacheMax)
	if err != nil {
		fmt.Fprintln(stderr, "sgd: --cache-max:", err)
		return 2
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(stderr, "sgd: --log-level:", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl}))
	conn, err := client.Dial(*upstream, creds.Server{CAFile: *ca, Insecure: *insecure})
	if err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 2
	}
	defer conn.Close()
	if err := probeUpstream(ctx, conn, *upstream, log); err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 2
	}
	c, err := cache.Open(*cacheDir, max)
	if err != nil {
		fmt.Fprintln(stderr, "sgd: cache:", err)
		return 1
	}
	d, err := daemon.New(daemon.Config{Upstream: *upstream, Conn: conn, Cache: c, Log: log})
	if err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 1
	}
	defer d.Close()
	lis, err := listen(*socket)
	if err != nil {
		fmt.Fprintln(stderr, "sgd:", err)
		return 1
	}
	g := daemon.NewGRPC()
	d.Register(g)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	log.Info("sgd listening", "socket", *socket, "upstream", *upstream, "cache", *cacheDir, "cache_max", max)
	serveErr := g.Serve(lis)
	if serveErr != nil {
		log.Error("serve", "err", serveErr)
		return 1
	}
	log.Info("sgd stopped")
	return 0
}
