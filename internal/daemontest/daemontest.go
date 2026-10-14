// Package daemontest runs an in-process sgd for tests, in front of an
// upstream sg-server (usually testenv), with a counting upstream connection.
package daemontest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/daemon"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc"
)

// Counter counts GetObjects traffic the daemon sends upstream.
type Counter struct {
	mu    sync.Mutex
	calls int
	shas  map[string]int
}

func (c *Counter) add(shas []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	for _, s := range shas {
		c.shas[s]++
	}
}

// Calls is the number of upstream GetObjects streams opened.
func (c *Counter) Calls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

// Requested is how often sha was requested upstream.
func (c *Counter) Requested(sha string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.shas[sha] }

// Total is the number of SHAs requested upstream.
func (c *Counter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.shas {
		n += v
	}
	return n
}

type countingStream struct {
	grpc.ClientStream
	c *Counter
}

func (s *countingStream) SendMsg(m any) error {
	if r, ok := m.(*sgitv1.GetObjectsRequest); ok {
		s.c.add(r.Shas)
	}
	return s.ClientStream.SendMsg(m)
}

// CountingConn dials addr (plaintext, no token) and counts GetObjects requests.
func CountingConn(t testing.TB, addr string) (*grpc.ClientConn, *Counter) {
	t.Helper()
	c := &Counter{shas: map[string]int{}}
	si := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err == nil && method == "/sgit.v1.Repo/GetObjects" {
			return &countingStream{ClientStream: cs, c: c}, nil
		}
		return cs, err
	}
	conn := testenv.Must(grpc.NewClient(addr, append(rpc.DialOptions("", nil), grpc.WithStreamInterceptor(si))...))
	t.Cleanup(func() { conn.Close() })
	return conn, c
}

// Env is a running in-process sgd.
type Env struct {
	t        testing.TB
	Socket   string
	CacheDir string
	Cache    *cache.Cache
	Daemon   *daemon.Daemon
	Counter  *Counter
}

// Start runs sgd on a short temp socket in front of upstreamAddr. opts may
// adjust the config (e.g. ACLTTL, Upstream, Cache).
func Start(t testing.TB, upstreamAddr string, opts ...func(*daemon.Config)) *Env {
	t.Helper()
	conn, counter := CountingConn(t, upstreamAddr)
	cacheDir := t.TempDir()
	c := testenv.Must(cache.Open(cacheDir, 1<<30))
	cfg := daemon.Config{Upstream: upstreamAddr, Conn: conn, Cache: c}
	for _, o := range opts {
		o(&cfg)
	}
	d := testenv.Must(daemon.New(cfg))
	dir := testenv.Must(os.MkdirTemp("", "sgd")) // short path: unix sockets are limited to ~108 bytes
	sock := filepath.Join(dir, "s.sock")
	lis := testenv.Must(net.Listen("unix", sock))
	g := daemon.NewGRPC()
	d.Register(g)
	go g.Serve(lis)
	t.Cleanup(func() {
		g.Stop()
		d.Close()
		os.RemoveAll(dir)
	})
	return &Env{t: t, Socket: sock, CacheDir: cacheDir, Cache: cfg.Cache, Daemon: d, Counter: counter}
}

// Conn dials the daemon socket with token.
func (e *Env) Conn(token string) *grpc.ClientConn {
	conn := testenv.Must(grpc.NewClient("unix://"+e.Socket, rpc.DialOptions(token, nil)...))
	e.t.Cleanup(func() { conn.Close() })
	return conn
}

func (e *Env) Repo(token string) sgitv1.RepoClient { return sgitv1.NewRepoClient(e.Conn(token)) }

// Info calls Daemon/Info.
func (e *Env) Info() *sgitv1.DaemonInfoResponse {
	return testenv.Must(sgitv1.NewDaemonClient(e.Conn("")).Info(context.Background(), &sgitv1.DaemonInfoRequest{}))
}
