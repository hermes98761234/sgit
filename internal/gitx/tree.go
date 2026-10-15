package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hermes98761234/sgit/internal/repopath"
)

// ChangeOp is the kind of a single path change.
type ChangeOp int

const (
	OpAdd ChangeOp = iota + 1
	OpModify
	OpDelete
)

// Change is one path change relative to a base commit. Add and Modify are
// upserts; Delete requires the path to be a file in the base.
type Change struct {
	Path    string
	Op      ChangeOp
	BlobSHA string
	Mode    string // "" means 100644
}

// FileEntry is a file in a recursive tree listing.
type FileEntry struct {
	Mode string
	SHA  string
}

// ListFiles lists every file (recursively) in rev's tree.
func (r *Repo) ListFiles(ctx context.Context, rev string) (map[string]FileEntry, error) {
	out, err := r.run(ctx, nil, nil, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	files := make(map[string]FileEntry)
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta) // mode type sha
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("gitx: bad ls-tree record %q", rec)
		}
		files[p] = FileEntry{Mode: f[0], SHA: f[2]}
	}
	return files, nil
}

// BuildTree applies changes on top of baseCommit's tree using a temporary
// index and returns the new tree SHA. No refs or working trees are touched.
func (r *Repo) BuildTree(ctx context.Context, baseCommit string, changes []Change) (string, error) {
	var deletes []string
	var info bytes.Buffer
	for _, c := range changes {
		// git update-index skips a path it refuses with only a warning;
		// refuse it here so a file is never dropped from a tree.
		if err := repopath.Check(c.Path, c.Mode == "120000"); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		switch c.Op {
		case OpAdd, OpModify:
			mode := c.Mode
			if mode == "" {
				mode = "100644"
			}
			fmt.Fprintf(&info, "%s %s\t%s\x00", mode, c.BlobSHA, c.Path)
		case OpDelete:
			deletes = append(deletes, c.Path)
			fmt.Fprintf(&info, "0 %s\t%s\x00", ZeroSHA, c.Path)
		default:
			return "", fmt.Errorf("%w: unknown op %d for %q", ErrInvalid, c.Op, c.Path)
		}
	}
	if len(deletes) > 0 {
		files, err := r.ListFiles(ctx, baseCommit)
		if err != nil {
			return "", err
		}
		for _, p := range deletes {
			if _, ok := files[p]; !ok {
				return "", fmt.Errorf("%w: delete %q: no such file in base", ErrInvalid, p)
			}
		}
	}
	tmp, err := os.MkdirTemp("", "sgit-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := r.run(ctx, nil, env, "read-tree", baseCommit); err != nil {
		return "", err
	}
	if _, err := r.run(ctx, &info, env, "update-index", "-z", "--replace", "--index-info"); err != nil {
		return "", err
	}
	out, err := r.run(ctx, nil, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
