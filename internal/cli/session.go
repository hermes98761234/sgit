package cli

import (
	"errors"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc"
)

// session is a connection to one server plus the working copy (if any).
type session struct {
	conn   *grpc.ClientConn
	repo   sgitv1.RepoClient
	admin  sgitv1.AdminClient
	server string
	name   string // repository name
	wc     *wc.WC
}

func (s *session) Close() { s.conn.Close() }

// target says which server/repo a command needs; empty fields come from the
// working copy, then from the default login.
type target struct {
	server   string
	repo     string
	needWC   bool
	needRepo bool
}

func openWC(a *app) (*wc.WC, error) {
	w, err := wc.Open(a.io.Dir)
	if errors.Is(err, wc.ErrNotWorkingCopy) {
		return nil, exitf(ExitError, "%v", err)
	}
	return w, err
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
	return &session{
		conn: conn, repo: sgitv1.NewRepoClient(conn), admin: sgitv1.NewAdminClient(conn),
		server: name, name: repo, wc: w,
	}, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
