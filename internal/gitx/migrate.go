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
// match. folder must be absent from base, or an empty tree there. tip's
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

// folderFree returns ErrFolderNotEmpty unless folder is absent from tree or
// is an empty tree there.
func (r *Repo) folderFree(ctx context.Context, tree, folder string) error {
	out, err := r.run(ctx, nil, nil, "ls-tree", "-z", tree, "--", folder)
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok || name != folder {
			continue
		}
		if f := strings.Fields(meta); len(f) == 3 && f[1] == "tree" && f[2] == EmptyTree {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrFolderNotEmpty, folder)
	}
	return nil
}
