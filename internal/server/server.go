// Package server implements the sgit.v1 Project and Admin gRPC services over
// bare git repositories.
package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/mirror"
	"github.com/hermes98761234/sgit/internal/project"
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
	// BeforeLand is a test hook called when a land through a project starts,
	// after the project was opened and before the land takes its lock.
	BeforeLand func(projectID int64)
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
	cfg      Config
	store    *store.Store
	log      *slog.Logger
	repos    *repoManager
	projects *project.Engine
	zenc     *zstd.Encoder
	mirror   *mirror.Manager
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
	s.projects = project.New(project.Config{
		Root:    filepath.Join(cfg.DataDir, "projects"),
		Store:   st,
		Sources: s.repos,
		Now:     cfg.Now,
		// A rebuilt repo lost the objects of every open draft.
		AfterRebuild: func(ctx context.Context, id int64) error {
			_, err := st.AbandonOpenDrafts(ctx, id)
			return err
		},
	})
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
	sgitv1.RegisterProjectServer(g, &projectService{s: s})
	sgitv1.RegisterAdminServer(g, &adminService{s: s})
	healthpb.RegisterHealthServer(g, health.NewServer())
}

// Shutdown cancels long work that ignores its caller's context (a project
// mount importing a folder's history), so a stopping server is not held up
// by it. The interrupted work resumes later; see project.Engine.Stop.
func (s *Server) Shutdown() { s.projects.Stop() }

func (s *Server) Close() {
	s.projects.Stop()
	s.mirror.Close()
	s.projects.CloseAll()
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
	case errors.Is(err, gitx.ErrNotFound):
		return status.Error(codes.NotFound, userMessage(err, gitx.ErrNotFound, " not found"))
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, userMessage(err, store.ErrNotFound, " not found"))
	case errors.Is(err, gitx.ErrInvalid):
		return status.Error(codes.InvalidArgument, userMessage(err, gitx.ErrInvalid, ""))
	case errors.Is(err, store.ErrExists):
		return status.Error(codes.AlreadyExists, userMessage(err, store.ErrExists, " already exists"))
	case errors.Is(err, store.ErrMounted):
		return status.Error(codes.FailedPrecondition, "repo "+userMessage(err, store.ErrMounted, " is mounted by a project"))
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	// The caller went away (its git children were killed): not our error.
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	id := auth.RequestID(ctx)
	s.log.ErrorContext(ctx, "internal error", "request_id", id, "err", err)
	return status.Errorf(codes.Internal, "internal error (request %s)", id)
}

// userMessage rewrites err, which wraps sentinel as "<sentinel>: <detail>",
// into "<detail><suffix>" for clients: "store: not found: draft "x"" becomes
// "draft "x" not found". A detail that already contains suffix ("mount "x"
// already exists; unmount it first") is returned as is. The internal package
// prefix never reaches a client; the log keeps the original error.
func userMessage(err, sentinel error, suffix string) string {
	msg, p := err.Error(), sentinel.Error()
	if i := strings.Index(msg, p+": "); i >= 0 {
		detail := msg[:i] + msg[i+len(p)+2:]
		if strings.Contains(detail, suffix) { // the detail says it already
			return detail
		}
		return detail + suffix
	}
	if _, bare, ok := strings.Cut(p, ": "); ok {
		return strings.Replace(msg, p, bare, 1)
	}
	return msg
}

// proj is an opened, authorized and caught-up project for one RPC.
type proj struct {
	h      *project.Handle
	p      store.Project
	ref    string // what the caller sent; used in errors
	mounts []store.Mount
}

// open resolves ref, authorizes the caller and catches the project up.
// Unknown projects and projects the caller cannot read give the identical
// ProjectNotFound error.
func (s *Server) open(ctx context.Context, ref string, a auth.Action) (*proj, store.User, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, u, status.Error(codes.Unauthenticated, "no user")
	}
	p, err := s.store.ResolveProject(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, u, auth.ProjectNotFound(ref)
	}
	if err != nil {
		return nil, u, s.hide(ctx, err)
	}
	if err := auth.Authorize(ctx, s.store, u, p.ID, a); err != nil {
		if errors.Is(err, auth.ErrNoAccess) {
			return nil, u, auth.ProjectNotFound(ref)
		}
		return nil, u, err
	}
	h, err := s.projects.Get(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) { // deleted since it was resolved
		return nil, u, auth.ProjectNotFound(ref)
	}
	if err != nil {
		return nil, u, s.hide(ctx, err)
	}
	if _, err := s.projects.CatchUp(ctx, h); err != nil {
		if errors.Is(err, store.ErrNotFound) && s.gone(ctx, p.ID) {
			return nil, u, auth.ProjectNotFound(ref)
		}
		return nil, u, s.hide(ctx, err)
	}
	mounts, err := s.store.Mounts(ctx, p.ID)
	if err != nil {
		return nil, u, s.hide(ctx, err)
	}
	return &proj{h: h, p: p, ref: ref, mounts: mounts}, u, nil
}

// gone reports whether project id no longer exists.
func (s *Server) gone(ctx context.Context, id int64) bool {
	_, err := s.store.GetProject(ctx, id)
	return errors.Is(err, store.ErrNotFound)
}

// lock takes the project's land lock for a draft change or a land, and
// re-checks that the project still exists (a delete holds the same lock
// while it removes the rows). The caller must call the returned unlock.
func (s *Server) lock(ctx context.Context, pr *proj) (unlock func(), err error) {
	pr.h.LandMu.Lock()
	if _, err := s.store.GetProject(ctx, pr.p.ID); err != nil {
		pr.h.LandMu.Unlock()
		if errors.Is(err, store.ErrNotFound) {
			return nil, auth.ProjectNotFound(pr.ref)
		}
		return nil, s.hide(ctx, err)
	}
	return pr.h.LandMu.Unlock, nil
}

// hide logs err and returns a generic Internal error. Every failure inside a
// source repo goes through it: such messages can name repos, source paths or
// source SHAs, none of which may reach a client.
func (s *Server) hide(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	id := auth.RequestID(ctx)
	s.log.ErrorContext(ctx, "project operation failed", "request_id", id, "err", err)
	return status.Errorf(codes.Internal, "internal error (request %s)", id)
}
