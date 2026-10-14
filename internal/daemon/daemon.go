// Package daemon implements sgd: a per-user gRPC proxy in front of one
// sg-server. GetObjects is served from a verified disk cache; every other
// Repo RPC is forwarded with the caller's token.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Version is reported by Daemon/Info.
const Version = "0.3.0"

// Config configures a Daemon.
type Config struct {
	Upstream string                   // upstream server address, reported by Info
	Conn     grpc.ClientConnInterface // connection to the upstream (no token attached)
	Cache    *cache.Cache
	ACLTTL   time.Duration // how long a successful read probe is trusted (default 60s)
	Log      *slog.Logger
}

// Daemon is the sgd service.
type Daemon struct {
	cfg      Config
	up       sgitv1.ProjectClient
	dec      *zstd.Decoder
	mu       sync.Mutex
	acl      map[aclKey]time.Time // successful read probes, until expiry
	inflight map[string]*call     // project+"\x00"+sha -> shared upstream fetch
}

// New validates cfg and returns a Daemon.
func New(cfg Config) (*Daemon, error) {
	if cfg.Conn == nil || cfg.Cache == nil {
		return nil, errors.New("daemon: Conn and Cache are required")
	}
	if cfg.ACLTTL == 0 {
		cfg.ACLTTL = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(fetch.MaxObjectSize))
	if err != nil {
		return nil, err
	}
	return &Daemon{
		cfg: cfg, up: sgitv1.NewProjectClient(cfg.Conn), dec: dec,
		acl: map[aclKey]time.Time{}, inflight: map[string]*call{},
	}, nil
}

// Close releases resources.
func (d *Daemon) Close() { d.dec.Close() }

// NewGRPC returns a gRPC server with sgit's message limits.
func NewGRPC() *grpc.Server { return grpc.NewServer(rpc.ServerOptions()...) }

// Register adds the Repo, Daemon and health services to g.
func (d *Daemon) Register(g *grpc.Server) {
	sgitv1.RegisterProjectServer(g, &proxy{d: d})
	sgitv1.RegisterDaemonServer(g, &infoServer{d: d})
	healthpb.RegisterHealthServer(g, health.NewServer())
}

// forward copies the caller's authorization header onto a context for the
// upstream call. Calls without one are rejected here.
func forward(ctx context.Context) (context.Context, string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 || vals[0] == "" {
		return nil, "", status.Error(codes.Unauthenticated, "missing token")
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", vals[0])), vals[0], nil
}

type infoServer struct {
	sgitv1.UnimplementedDaemonServer
	d *Daemon
}

func (s *infoServer) Info(context.Context, *sgitv1.DaemonInfoRequest) (*sgitv1.DaemonInfoResponse, error) {
	return &sgitv1.DaemonInfoResponse{
		Upstream: s.d.cfg.Upstream, Version: Version,
		CacheBytes: uint64(s.d.cfg.Cache.Used()), CacheMaxBytes: uint64(s.d.cfg.Cache.Max()),
	}, nil
}
