// Package server implements the sgit.v1 Repo and Admin gRPC services over
// bare git repositories.
package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/mirror"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/store"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type Config struct {
	DataDir      string
	MaxBlobBytes int64 // default 100 MiB
	MaxChanges   int   // default 10000
	LandRetries  int   // default 5
	// AllowLocalRemotes permits file:// and path remotes for import and
	// mirroring. Tests only; sg-server has no flag for it.
	AllowLocalRemotes bool
	MirrorBackoffMin  time.Duration    // first mirror retry delay; default 1s
	MirrorBackoffMax  time.Duration    // mirror retry delay cap; default 5m
	MirrorTimeout     time.Duration    // per mirror push; default 10m
	Now               func() time.Time // default time.Now
	// BeforeCAS is a test hook called right before main is compare-and-swapped.
	BeforeCAS func(repo string)
}

func (c *Config) defaults() {
	if c.MaxBlobBytes == 0 {
		c.MaxBlobBytes = 100 << 20
	}
	if c.MaxChanges == 0 {
		c.MaxChanges = 10000
	}
	if c.LandRetries == 0 {
		c.LandRetries = 5
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

type Server struct {
	cfg    Config
	store  *store.Store
	log    *slog.Logger
	repos  *repoManager
	zenc   *zstd.Encoder
	mirror *mirror.Manager
}

func New(cfg Config, st *store.Store, log *slog.Logger) (*Server, error) {
	cfg.defaults()
	root := filepath.Join(cfg.DataDir, "repos")
	// Leftovers of a create or import interrupted by a crash.
	if err := os.RemoveAll(incomingDir(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:   cfg,
		store: st,
		log:   log,
		repos: newRepoManager(root, st, cfg.AllowLocalRemotes),
		zenc:  enc,
	}
	s.mirror = mirror.New(mirror.Config{
		RepoDir:    s.repos.path,
		AllowLocal: cfg.AllowLocalRemotes,
		BackoffMin: cfg.MirrorBackoffMin,
		BackoffMax: cfg.MirrorBackoffMax,
		Timeout:    cfg.MirrorTimeout,
		Now:        cfg.Now,
	}, st, log)
	if err := s.mirror.Start(context.Background()); err != nil {
		s.mirror.Close()
		_ = enc.Close()
		return nil, err
	}
	return s, nil
}

// NewGRPC returns a gRPC server with sgit's message limits and auth interceptors.
func NewGRPC(st *store.Store, extra ...grpc.ServerOption) *grpc.Server {
	opts := append(rpc.ServerOptions(),
		grpc.InTapHandle(auth.TapHandle),
		grpc.ChainUnaryInterceptor(auth.UnaryInterceptor(st)),
		grpc.ChainStreamInterceptor(auth.StreamInterceptor(st)),
		// Stop waits for handlers to return, so a cancelled import finishes
		// its cleanup before the store is closed.
		grpc.WaitForHandlers(true),
	)
	return grpc.NewServer(append(opts, extra...)...)
}

func (s *Server) Register(g *grpc.Server) {
	sgitv1.RegisterRepoServer(g, &repoService{s: s})
	sgitv1.RegisterAdminServer(g, &adminService{s: s})
	healthpb.RegisterHealthServer(g, health.NewServer())
}

func (s *Server) Close() {
	s.mirror.Close()
	s.repos.closeAll()
	_ = s.zenc.Close()
}

func (s *Server) sig(u store.User) gitx.Signature {
	return gitx.Signature{Name: u.Name, Email: u.Email, When: s.cfg.Now()}
}

// toStatus maps internal errors to gRPC status errors. Unknown errors are
// logged with the request id and returned as Internal without details.
func (s *Server) toStatus(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, gitx.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, gitx.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, store.ErrExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	id := auth.RequestID(ctx)
	s.log.ErrorContext(ctx, "internal error", "request_id", id, "err", err)
	return status.Errorf(codes.Internal, "internal error (request %s)", id)
}

// open authenticates, authorizes and opens the repo for a Repo RPC.
func (s *Server) open(ctx context.Context, repo string, a auth.Action) (*repoHandle, store.User, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, u, status.Error(codes.Unauthenticated, "no user")
	}
	if err := auth.Authorize(ctx, s.store, u, repo, a); err != nil {
		return nil, u, err
	}
	h, err := s.repos.get(ctx, repo)
	if err != nil {
		return nil, u, s.toStatus(ctx, err)
	}
	return h, u, nil
}
