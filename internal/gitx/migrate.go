package gitx

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrFolderNotEmpty means a replay target folder already holds files.
var ErrFolderNotEmpty = errors.New("gitx: folder not empty")

// ReplayUnder copies the first-parent chain of tip, oldest first, on top of
// base. Each copy's tree is base's tree with folder replaced by the original
// commit's root tree. Author, committer, encoding and message are kept byte
// for byte; signatures (gpgsig, mergetag) are dropped because they no longer
// match. folder must be absent from base, or an empty tree there
// (ErrFolderNotEmpty; *NotAFolderError for a file at folder or a parent). tip's
// objects must already be in r. Refs are not touched.
func (r *Repo) ReplayUnder(ctx context.Context, base, tip, folder string) (string, int, error) {
	if !IsSHA(base) || !IsSHA(tip) || folder == "" || folder == "." ||
		strings.HasPrefix(folder, "/") || strings.HasSuffix(folder, "/") || strings.ContainsAny(folder, "\x00\n") {
		return "", 0, fmt.Errorf("%w: replay %q onto %q under %q", ErrInvalid, tip, base, folder)
	}
	bc, err := r.ReadCommit(ctx, base)
	if err != nil {
		return "", 0, err
	}
	if err := r.folderFree(ctx, bc.Tree, folder); err != nil {
		return "", 0, err
	}
	list, err := r.FirstParents(ctx, tip, "")
	if err != nil {
		return "", 0, err
	}
	parent := base
	for _, fp := range list {
		c, err := r.ReadCommit(ctx, fp.SHA)
		if err != nil {
			return "", 0, err
		}
		tree, err := r.SpliceTree(ctx, bc.Tree, map[string]string{folder: c.Tree})
		if err != nil {
			return "", 0, err
		}
		parent, err = r.writeCommit(ctx, tree, []string{parent}, c.AuthorRaw, c.CommitterRaw, c.Encoding, c.Message)
		if err != nil {
			return "", 0, err
		}
	}
	return parent, len(list), nil
}

// folderFree returns nil when folder is absent from tree or is an empty tree
// there, a *NotAFolderError when folder or a parent of it is not a tree, and
// ErrFolderNotEmpty otherwise.
func (r *Repo) folderFree(ctx context.Context, tree, folder string) error {
	parts := strings.Split(folder, "/")
	paths := make([]string, len(parts))
	for i := range parts {
		paths[i] = strings.Join(parts[:i+1], "/")
	}
	isPrefix := make(map[string]bool, len(paths))
	for _, p := range paths {
		isPrefix[p] = true
	}
	out, err := r.run(ctx, nil, nil, append([]string{"ls-tree", "-z", tree, "--"}, paths...)...)
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		if name != folder {
			if !isPrefix[name] {
				continue
			}
			if f[1] != "tree" {
				return &NotAFolderError{Path: name}
			}
			continue
		}
		if f[1] != "tree" {
			return &NotAFolderError{Path: name}
		}
		if f[2] == EmptyTree {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrFolderNotEmpty, folder)
	}
	return nil
}

// NotAFolderError means a replay target folder, or one of its parents, is
// not a folder: a file, a symlink or a submodule. Path is that entry.
type NotAFolderError struct{ Path string }

func (e *NotAFolderError) Error() string { return "gitx: " + e.Path + " is not a folder" }
