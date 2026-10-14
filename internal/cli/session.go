package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc"
)

// session is a connection to one server plus the working copy (if any).
type session struct {
	conn   *grpc.ClientConn
	cached *grpc.ClientConn // sgd connection; nil when not used
	repo   sgitv1.RepoClient
	admin  sgitv1.AdminClient
	server string
	name   string // repository name
	wc     *wc.WC
}

func (s *session) Close() {
	s.conn.Close()
	if s.cached != nil {
		s.cached.Close()
	}
}

// target says which server/repo a command needs; empty fields come from the
// working copy, then from the default login.
type target struct {
	server    string
	repo      string
	needWC    bool
	needRepo  bool
	adminOnly bool // Admin RPCs only: never dial sgd
}

func openWC(a *app) (*wc.WC, error) {
	w, err := wc.Open(a.io.Dir)
	if errors.Is(err, wc.ErrNotWorkingCopy) {
		return nil, exitf(ExitError, "%v", err)
	}
	return w, err
}

// lockWC takes the working-copy lock and reloads the metadata, so the
// command works on the state as of the lock rather than the earlier open.
func lockWC(w *wc.WC) (*wc.WC, func(), error) {
	unlock, err := w.Lock()
	if err != nil {
		return nil, nil, exitf(ExitError, "%v", err)
	}
	fresh, err := wc.Open(w.Root)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return fresh, unlock, nil
}

// lock locks the session's working copy and reloads it (see lockWC).
func (s *session) lock() (func(), error) {
	w, unlock, err := lockWC(s.wc)
	if err != nil {
		return nil, err
	}
	s.wc = w
	return unlock, nil
}

// ownsDraft reports whether draft id belongs to the session's working copy
// (same server and repo, and recorded in .sg/draft).
func (s *session) ownsDraft(id string) bool {
	return s.wc != nil && s.wc.Config.Server == s.server && s.wc.Config.Repo == s.name && s.wc.Draft == id
}

func (a *app) connect(t target) (*session, error) {
	w, err := wc.Open(a.io.Dir)
	switch {
	case err == nil:
	case errors.Is(err, wc.ErrNotWorkingCopy):
		if t.needWC {
			return nil, exitf(ExitError, "%v", err)
		}
		w = nil
	default:
		return nil, err
	}
	server, repo := t.server, t.repo
	if w != nil {
		if server == "" {
			server = w.Config.Server
		}
		if repo == "" {
			repo = w.Config.Repo
		}
	}
	f, err := creds.Load()
	if err != nil {
		return nil, err
	}
	name, cred, ok := f.Lookup(server)
	if !ok {
		if name == "" {
			return nil, exitf(ExitAuth, "not logged in; run `sg login <host:port>`")
		}
		return nil, exitf(ExitAuth, "no credentials for %s; run `sg login %s`", name, name)
	}
	if t.needRepo && repo == "" {
		return nil, exitf(ExitError, "no repository: run inside a working copy or pass --repo")
	}
	conn, err := client.Dial(name, cred)
	if err != nil {
		return nil, exitf(ExitError, "%v", err)
	}
	repoConn := grpc.ClientConnInterface(conn)
	var cached *grpc.ClientConn
	if os.Getenv("SG_NO_DAEMON") == "" && !t.adminOnly {
		sock := client.DefaultSocket()
		dc, err := client.DialDaemon(context.Background(), sock, name, cred.Token)
		switch {
		case err == nil && dc != nil:
			cached, repoConn = dc, dc
		case err != nil && !errors.Is(err, client.ErrOtherUpstream):
			fmt.Fprintf(a.io.Stderr, "sg: warning: sgd at %s is not usable (%v); connecting directly\n", sock, err)
		}
	}
	return &session{
		conn: conn, cached: cached, repo: sgitv1.NewRepoClient(repoConn), admin: sgitv1.NewAdminClient(conn),
		server: name, name: repo, wc: w,
	}, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
