package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConflictFile describes one conflicted path. "Ours" is the first side passed
// to MergeTree (main when landing), "Theirs" the second (the draft).
type ConflictFile struct {
	Path   string
	Base   string
	Ours   string
	Theirs string
}

// MergeResult is the outcome of an in-memory 3-way merge.
type MergeResult struct {
	Tree      string
	Clean     bool
	Conflicts []ConflictFile
}

// MergeTree runs `git merge-tree --write-tree` (git >= 2.40) without a worktree.
func (r *Repo) MergeTree(ctx context.Context, mergeBase, ours, theirs string) (MergeResult, error) {
	out, err := r.run(ctx, nil, nil, "merge-tree", "--write-tree", "-z", "--merge-base="+mergeBase, ours, theirs)
	clean := true
	if err != nil {
		if exitCode(err) != 1 {
			return MergeResult{}, err
		}
		clean = false
	}
	res, perr := parseMergeTree(out)
	if perr != nil {
		return MergeResult{}, perr
	}
	res.Clean = clean
	return res, nil
}

// parseMergeTree parses -z output: "<tree>\0" then "<mode> <sha> <stage>\t<path>\0"
// entries, then an empty token, then informational messages (ignored).
func parseMergeTree(out []byte) (MergeResult, error) {
	toks := strings.Split(string(out), "\x00")
	if len(toks) == 0 || !IsSHA(toks[0]) {
		return MergeResult{}, fmt.Errorf("gitx: unexpected merge-tree output %q", out)
	}
	res := MergeResult{Tree: toks[0]}
	byPath := map[string]*ConflictFile{}
	var order []string
	for _, tok := range toks[1:] {
		if tok == "" {
			break
		}
		meta, p, ok := strings.Cut(tok, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			break
		}
		cf := byPath[p]
		if cf == nil {
			cf = &ConflictFile{Path: p}
			byPath[p] = cf
			order = append(order, p)
		}
		switch f[2] {
		case "1":
			cf.Base = f[1]
		case "2":
			cf.Ours = f[1]
		case "3":
			cf.Theirs = f[1]
		}
	}
	for _, p := range order {
		res.Conflicts = append(res.Conflicts, *byPath[p])
	}
	return res, nil
}

// MergeFile 3-way merges file contents with `git merge-file`. On conflict the
// result contains conflict markers labelled with the given labels. Binary
// inputs cannot be merged: ours is returned with conflicted=true.
func MergeFile(ctx context.Context, base, ours, theirs []byte, oursLabel, baseLabel, theirsLabel string) ([]byte, bool, error) {
	dir, err := os.MkdirTemp("", "sgit-merge-*")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	paths := [3]string{filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs")}
	for i, data := range [][]byte{ours, base, theirs} {
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			return nil, false, err
		}
	}
	out, err := runGit(ctx, nil, nil, "merge-file", "-p", "-L", oursLabel, "-L", baseLabel, "-L", theirsLabel, paths[0], paths[1], paths[2])
	if err == nil {
		return out, false, nil
	}
	var ge *GitError
	if errors.As(err, &ge) {
		if ge.ExitCode > 0 && ge.ExitCode < 128 {
			return out, true, nil
		}
		if strings.Contains(strings.ToLower(ge.Stderr), "binary") {
			return ours, true, nil
		}
	}
	return nil, false, err
}
