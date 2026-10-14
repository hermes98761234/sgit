package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var repoNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidRepoName reports whether name is an acceptable repo name.
func ValidRepoName(name string) bool {
	return repoNameRe.MatchString(name) && !strings.HasSuffix(name, ".git") && !strings.Contains(name, "..")
}

type repoHandle struct {
	name   string
	git    *gitx.Repo
	landMu sync.Mutex // serializes lands and draft mutations per repo; main's correctness comes from CAS
}

type repoManager struct {
	root       string
	st         *store.Store
	allowLocal bool // permit file:// and path remotes (tests)
	mu         sync.Mutex
	open       map[string]*repoHandle
	busy       map[string]bool // names with a create or import in flight
}

func newRepoManager(root string, st *store.Store, allowLocal bool) *repoManager {
	return &repoManager{root: root, st: st, allowLocal: allowLocal, open: map[string]*repoHandle{}, busy: map[string]bool{}}
}

func (m *repoManager) path(name string) string { return filepath.Join(m.root, name+".git") }

func (m *repoManager) get(ctx context.Context, name string) (*repoHandle, error) {
	if !ValidRepoName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repo name %q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.open[name]; ok {
		return h, nil
	}
	if _, err := m.st.GetRepo(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "repo %q not found", name)
		}
		return nil, err
	}
	h := &repoHandle{name: name, git: gitx.Open(m.path(name))}
	m.open[name] = h
	return h, nil
}

// reserve claims name for one create or import at a time. Call the returned
// release once the repo row exists or the attempt has been cleaned up.
func (m *repoManager) reserve(ctx context.Context, name string) (func(), error) {
	if !ValidRepoName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repo name %q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[name] {
		return nil, status.Errorf(codes.AlreadyExists, "repo %q is being created", name)
	}
	if _, err := m.st.GetRepo(ctx, name); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "repo %q exists", name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, err := os.Stat(m.path(name)); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "repo dir for %q exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	m.busy[name] = true
	return func() {
		m.mu.Lock()
		delete(m.busy, name)
		m.mu.Unlock()
	}, nil
}

func (m *repoManager) create(ctx context.Context, name, mirrorURL string, sig gitx.Signature) (string, error) {
	if mirrorURL != "" {
		if err := gitx.ValidateRemote(mirrorURL, m.allowLocal); err != nil {
			return "", err
		}
	}
	release, err := m.reserve(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return "", err
	}
	p := m.path(name)
	r, err := gitx.Init(ctx, p, sig)
	if err != nil {
		os.RemoveAll(p)
		return "", err
	}
	defer r.Close()
	main, err := r.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		os.RemoveAll(p)
		return "", err
	}
	if err := m.st.CreateRepo(ctx, name, mirrorURL); err != nil {
		os.RemoveAll(p)
		return "", err
	}
	return main, nil
}

// importRepo creates name from the main and release/* branches of from. When
// the mirror is the source itself, the mirror is recorded as holding main.
func (m *repoManager) importRepo(ctx context.Context, name, from, mirrorURL string) (string, error) {
	if from == "" {
		return "", status.Error(codes.InvalidArgument, "import needs a source URL")
	}
	if err := gitx.ValidateRemote(from, m.allowLocal); err != nil {
		return "", err
	}
	if mirrorURL != "" {
		if err := gitx.ValidateRemote(mirrorURL, m.allowLocal); err != nil {
			return "", err
		}
	}
	release, err := m.reserve(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return "", err
	}
	p := m.path(name)
	main, err := gitx.Import(ctx, p, from, m.allowLocal)
	if err != nil {
		os.RemoveAll(p)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var ge *gitx.GitError
		if errors.As(err, &ge) && slices.Contains(ge.Args, "fetch") {
			return "", status.Errorf(codes.FailedPrecondition, "import from %s failed: %s", from, gitx.ErrSummary(err))
		}
		return "", err
	}
	seeded := ""
	if mirrorURL == from {
		seeded = main
	}
	// The fetch may have taken minutes; record the result even if the caller gave up.
	if err := m.st.CreateRepoWithMirror(context.WithoutCancel(ctx), name, mirrorURL, seeded); err != nil {
		os.RemoveAll(p)
		return "", err
	}
	return main, nil
}

func (m *repoManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.open {
		h.git.Close()
	}
	m.open = map[string]*repoHandle{}
}
