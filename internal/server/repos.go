package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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

// incomingDir holds repos while create or import builds them. Repo names
// cannot start with '.', so it never clashes with a repo, and Server.New
// empties it, so a crash mid-build never blocks the name.
func incomingDir(root string) string { return filepath.Join(root, ".incoming") }

// staging returns a fresh path under incomingDir for building name.
func (m *repoManager) staging(name string) (string, error) {
	dir := incomingDir(m.root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, name+"-"+hex.EncodeToString(b[:])+".git"), nil
}

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

// mirrorPreflight refuses a mirror that already has a main branch, so a repo
// is never created with a mirror it can never push to. An unreachable mirror
// is allowed: the mirror worker retries it and reports the error.
func (m *repoManager) mirrorPreflight(ctx context.Context, mirrorURL string) error {
	ctx, cancel := context.WithTimeout(ctx, mirrorPreflightTimeout)
	defer cancel()
	sha, err := gitx.RemoteRef(ctx, mirrorURL, gitx.MainRef, m.allowLocal)
	if err != nil || sha == "" {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition, "mirror %s already has a main branch (%s); a mirror must start empty", mirrorURL, sha[:7])
}

const mirrorPreflightTimeout = 30 * time.Second

func (m *repoManager) create(ctx context.Context, name, mirrorURL string, sig gitx.Signature) (string, error) {
	if mirrorURL != "" {
		if err := gitx.ValidateRemote(mirrorURL, m.allowLocal); err != nil {
			return "", err
		}
		if err := m.mirrorPreflight(ctx, mirrorURL); err != nil {
			return "", err
		}
	}
	release, err := m.reserve(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	stage, err := m.staging(name)
	if err != nil {
		return "", err
	}
	r, err := gitx.Init(ctx, stage, sig)
	if err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	main, err := r.ReadRef(ctx, gitx.MainRef)
	r.Close()
	if err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	p := m.path(name)
	if err := os.Rename(stage, p); err != nil {
		os.RemoveAll(stage)
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
		if mirrorURL != from {
			if err := m.mirrorPreflight(ctx, mirrorURL); err != nil {
				return "", err
			}
		}
	}
	release, err := m.reserve(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	stage, err := m.staging(name)
	if err != nil {
		return "", err
	}
	main, err := gitx.Import(ctx, stage, from, m.allowLocal)
	if err != nil {
		os.RemoveAll(stage)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var ge *gitx.GitError
		if errors.As(err, &ge) && slices.Contains(ge.Args, "fetch") {
			return "", status.Errorf(codes.FailedPrecondition, "import from %s failed: %s", from, gitx.ErrSummary(err))
		}
		return "", err
	}
	p := m.path(name)
	if err := os.Rename(stage, p); err != nil {
		os.RemoveAll(stage)
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

// deleteRepo removes name from the database and moves its bare directory to
// <deletedRoot>/<name>.<unixtime>.git, so an operator can recover it and the
// name can be reused. landMu is held throughout, so no land or draft change
// is mid-flight; once released, those calls fail with NotFound. The mirror
// remote is never contacted.
func (m *repoManager) deleteRepo(ctx context.Context, name, deletedRoot string, now time.Time) (string, error) {
	h, err := m.get(ctx, name)
	if err != nil {
		return "", err
	}
	h.landMu.Lock()
	defer h.landMu.Unlock()
	// Lost a race with another delete.
	if _, err := m.st.GetRepo(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", status.Errorf(codes.NotFound, "repo %q not found", name)
		}
		return "", err
	}
	if err := os.MkdirAll(deletedRoot, 0o750); err != nil {
		return "", err
	}
	src := m.path(name)
	var dst string
	for ts := now.Unix(); ; ts++ {
		dst = filepath.Join(deletedRoot, name+"."+strconv.FormatInt(ts, 10)+".git")
		if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
	}
	h.git.Close()
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	// Past the point of no return for the caller's context: finish the DB
	// change or put the directory back.
	if err := m.st.DeleteRepo(context.WithoutCancel(ctx), name); err != nil {
		if rerr := os.Rename(dst, src); rerr != nil {
			return "", errors.Join(err, rerr)
		}
		return "", err
	}
	m.mu.Lock()
	delete(m.open, name)
	m.mu.Unlock()
	return dst, nil
}
