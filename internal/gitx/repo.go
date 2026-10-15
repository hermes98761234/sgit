package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const catFilePoolSize = 4

// Repo is a handle to one bare repository. It is safe for concurrent use.
type Repo struct {
	dir    string
	slots  chan struct{} // one token per cat-file a caller may hold at once
	mu     sync.Mutex
	procs  []*catFile // every live cat-file process
	idle   []*catFile // live processes not currently checked out
	gen    int        // processes from an older generation are not reused
	closed bool
}

// Open returns a handle to an existing bare repo. Processes start lazily.
func Open(dir string) *Repo {
	r := &Repo{dir: dir, slots: make(chan struct{}, catFilePoolSize)}
	for i := 0; i < catFilePoolSize; i++ {
		r.slots <- struct{}{}
	}
	return r
}

// Init creates a bare repo at dir whose main points at an initial empty commit.
func Init(ctx context.Context, dir string, sig Signature) (*Repo, error) {
	if err := InitBare(ctx, dir); err != nil {
		return nil, err
	}
	r := Open(dir)
	out, err := r.run(ctx, strings.NewReader(""), nil, "hash-object", "-w", "-t", "tree", "--stdin")
	if err != nil {
		return nil, err
	}
	if got := strings.TrimSpace(string(out)); got != EmptyTree {
		return nil, fmt.Errorf("gitx: unexpected empty tree id %q", got)
	}
	c, err := r.CommitTree(ctx, EmptyTree, nil, "Initial commit\n", sig, sig)
	if err != nil {
		return nil, err
	}
	if err := r.UpdateRef(ctx, MainRef, c, ""); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Repo) Dir() string { return r.dir }

// Close stops all cat-file processes.
func (r *Repo) Close() error {
	r.mu.Lock()
	r.closed = true
	procs := r.procs
	r.procs = nil
	r.idle = nil
	r.mu.Unlock()
	for _, p := range procs {
		p.close()
	}
	return nil
}

// Recycle stops the idle cat-file processes and retires the busy ones when
// they are released, so later reads see the repo as it is on disk now (a
// running cat-file may still hold packs that were deleted).
func (r *Repo) Recycle() {
	r.mu.Lock()
	r.gen++
	idle := r.idle
	r.idle = nil
	for _, c := range idle {
		for i, p := range r.procs {
			if p == c {
				r.procs = append(r.procs[:i], r.procs[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	for _, c := range idle {
		c.close()
	}
}

func (r *Repo) run(ctx context.Context, stdin io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	return runGit(ctx, stdin, extraEnv, append([]string{"--git-dir=" + r.dir}, args...)...)
}

// ReadRef returns the SHA a full ref name points to, or ErrNotFound.
func (r *Repo) ReadRef(ctx context.Context, ref string) (string, error) {
	out, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		if exitCode(err) == 1 {
			return "", ErrNotFound
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveRev resolves a revision expression (branch name, SHA, ...) to a commit SHA.
func (r *Repo) ResolveRev(ctx context.Context, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, " \t\r\n\x00") || len(rev) > 256 {
		return "", fmt.Errorf("%w: invalid revision %q", ErrInvalid, rev)
	}
	out, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		if exitCode(err) == 1 {
			return "", fmt.Errorf("%w: revision %q", ErrNotFound, rev)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// UpdateRef atomically moves ref from oldSHA to newSHA. oldSHA == "" means the
// ref must not exist yet. A mismatch (or lock contention) returns ErrRefMoved.
func (r *Repo) UpdateRef(ctx context.Context, ref, newSHA, oldSHA string) error {
	if oldSHA == "" {
		oldSHA = ZeroSHA
	}
	_, err := r.run(ctx, nil, nil, "update-ref", "--no-deref", ref, newSHA, oldSHA)
	if err == nil {
		return nil
	}
	var ge *GitError
	if !errors.As(err, &ge) {
		return err
	}
	cur, rerr := r.ReadRef(ctx, ref)
	switch {
	case errors.Is(rerr, ErrNotFound):
		cur = ZeroSHA
	case rerr != nil:
		return err
	}
	if cur != oldSHA || strings.Contains(ge.Stderr, ".lock") {
		return ErrRefMoved
	}
	return err
}

// WriteBlob stores data as a blob and returns its SHA.
func (r *Repo) WriteBlob(ctx context.Context, data []byte) (string, error) {
	out, err := r.run(ctx, bytes.NewReader(data), nil, "hash-object", "-w", "-t", "blob", "--no-filters", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
