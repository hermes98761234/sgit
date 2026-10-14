package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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
	root string
	st   *store.Store
	mu   sync.Mutex
	open map[string]*repoHandle
}

func newRepoManager(root string, st *store.Store) *repoManager {
	return &repoManager{root: root, st: st, open: map[string]*repoHandle{}}
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

func (m *repoManager) create(ctx context.Context, name string, sig gitx.Signature) (string, error) {
	if !ValidRepoName(name) {
		return "", status.Errorf(codes.InvalidArgument, "invalid repo name %q", name)
	}
	if _, err := m.st.GetRepo(ctx, name); err == nil {
		return "", status.Errorf(codes.AlreadyExists, "repo %q exists", name)
	}
	p := m.path(name)
	if _, err := os.Stat(p); err == nil {
		return "", status.Errorf(codes.AlreadyExists, "repo dir for %q exists", name)
	}
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return "", err
	}
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
	if err := m.st.CreateRepo(ctx, name, ""); err != nil {
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
