// Package mirror pushes each repo's main to its mirror_url in the background
// (spec §8). One worker per mirrored repo pushes with
// --force-with-lease=main:<last_mirrored>, so commits made on the mirror
// outside sgit are never overwritten. Failed pushes retry with exponential
// backoff; Start queues every mirrored repo so a restart catches up.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
)

type Config struct {
	RepoDir    func(repo string) string // bare repo directory for a repo name
	AllowLocal bool                     // permit file:// and path mirrors (tests only)
	BackoffMin time.Duration            // first retry delay; default 1s
	BackoffMax time.Duration            // retry delay cap; default 5m
	Timeout    time.Duration            // per push attempt; default 10m
	Now        func() time.Time         // default time.Now
}

type Manager struct {
	cfg    Config
	st     *store.Store
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	kicks  map[string]chan struct{} // one per repo with a running worker
	closed bool
}

func New(cfg Config, st *store.Store, log *slog.Logger) *Manager {
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 5 * time.Minute
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, st: st, log: log, ctx: ctx, cancel: cancel, kicks: map[string]chan struct{}{}}
}

// Start queues every repo that has a mirror_url, so pushes missed while the
// server was down (or whose bookkeeping was lost) are caught up.
func (m *Manager) Start(ctx context.Context) error {
	repos, err := m.st.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("mirror: list repos: %w", err)
	}
	for _, r := range repos {
		if r.MirrorURL != "" {
			m.Notify(r.Name)
		}
	}
	return nil
}

// Notify queues a mirror check for repo. It never blocks.
func (m *Manager) Notify(repo string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	kick, ok := m.kicks[repo]
	if !ok {
		kick = make(chan struct{}, 1)
		m.kicks[repo] = kick
		m.wg.Add(1)
		go m.run(repo, kick)
	}
	select {
	case kick <- struct{}{}:
	default: // a check is already queued
	}
}

// Close stops all workers, killing any push in flight, and waits for them.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) run(repo string, kick <-chan struct{}) {
	defer m.wg.Done()
	g := gitx.Open(m.cfg.RepoDir(repo))
	defer g.Close()
	var delay time.Duration
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-kick:
		}
		for {
			caughtUp, err := m.sync(repo, g)
			if err == nil {
				delay = 0
				if caughtUp {
					break
				}
				continue // pushed; main may have moved meanwhile
			}
			if m.ctx.Err() != nil {
				return
			}
			delay = m.backoff(delay)
			m.log.Warn("mirror push failed", "repo", repo, "retry_in", delay.String(), "err", err)
			t := time.NewTimer(delay)
			select {
			case <-m.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

func (m *Manager) backoff(d time.Duration) time.Duration {
	if d == 0 {
		return m.cfg.BackoffMin
	}
	if d *= 2; d > m.cfg.BackoffMax {
		d = m.cfg.BackoffMax
	}
	return d
}

// sync pushes main to repo's mirror if the mirror is behind. It reports
// whether the mirror is caught up (nothing was pushed).
func (m *Manager) sync(repo string, g *gitx.Repo) (bool, error) {
	ctx := m.ctx
	row, err := m.st.GetRepo(ctx, repo)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if row.MirrorURL == "" {
		return true, nil
	}
	main, err := g.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		msg := "mirror check failed: " + gitx.ErrSummary(err)
		if rerr := m.st.RecordMirrorFailure(ctx, repo, msg, m.cfg.Now()); rerr != nil {
			m.log.Error("mirror: record failure", "repo", repo, "err", rerr)
		}
		return false, errors.New(msg)
	}
	state, err := m.st.MirrorState(ctx, repo)
	if err != nil {
		return false, err
	}
	if state.LastMirrored == main {
		if state.Pending {
			return true, m.st.SetMirrorPending(ctx, repo, false)
		}
		return true, nil
	}
	if !state.Pending {
		if err := m.st.SetMirrorPending(ctx, repo, true); err != nil {
			return false, err
		}
	}
	pctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	err = g.Push(pctx, row.MirrorURL, main, state.LastMirrored, m.cfg.AllowLocal)
	if errors.Is(err, gitx.ErrRejected) {
		// Git normally treats a ref that is already up to date as success
		// before checking the lease, so a crash-lost push is recorded via the
		// normal path. This guards the race where the mirror receives our
		// exact commit by another route between the lease check and the push:
		// if it holds main, that is a success, not a conflict.
		if remote, rerr := gitx.RemoteRef(pctx, row.MirrorURL, gitx.MainRef, m.cfg.AllowLocal); rerr == nil && remote == main {
			err = nil
		}
	}
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		msg := describe(err, state.LastMirrored)
		if rerr := m.st.RecordMirrorFailure(ctx, repo, msg, m.cfg.Now()); rerr != nil {
			m.log.Error("mirror: record failure", "repo", repo, "err", rerr)
		}
		return false, errors.New(msg)
	}
	if err := m.st.RecordMirrorSuccess(ctx, repo, main, m.cfg.Now()); err != nil {
		return false, err
	}
	m.log.Info("mirrored", "repo", repo, "commit", main)
	return false, nil
}

// describe turns a push error into the message shown by sg repo status.
func describe(err error, lease string) string {
	switch {
	case errors.Is(err, gitx.ErrRejected) && lease == "":
		return "mirror already has a main branch that sgit did not push; refusing to overwrite it (a mirror must start empty)"
	case errors.Is(err, gitx.ErrRejected):
		return fmt.Sprintf("mirror main is no longer %s (changed outside sgit); refusing to overwrite it", lease)
	default:
		return "push failed: " + gitx.ErrSummary(err)
	}
}
