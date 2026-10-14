package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
)

func cmdSparse(ctx context.Context, a *app, args []string) error {
	const u = "sparse list | add <path>... | rm <path>..."
	if len(args) == 0 {
		return usageErr(u)
	}
	sub := args[0]
	fs := a.newFlags("sparse " + sub)
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	w, err := openWC(a)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		list := append([]string{}, w.Config.Sparse...)
		return a.emit(list, func(out io.Writer) {
			if len(list) == 0 {
				fmt.Fprintln(out, "(full checkout)")
			}
			for _, p := range list {
				fmt.Fprintln(out, p)
			}
		})
	case "add":
		if len(pos) == 0 {
			return usageErr(u)
		}
		if len(w.Config.Sparse) == 0 {
			return exitf(ExitError, "this is a full checkout; there is nothing to add")
		}
		var adds []string
		for _, arg := range pos {
			rp, err := w.RepoPath(a.io.Dir, arg)
			if err != nil || rp == "" {
				return exitf(ExitError, "invalid sparse path %q", arg)
			}
			if !wc.InSparse(w.Config.Sparse, rp) {
				adds = append(adds, rp)
			}
		}
		added := 0
		if len(adds) > 0 {
			fresh, err := wc.NormalizeSparse(adds)
			if err != nil {
				return exitf(ExitError, "%v", err)
			}
			s, err := a.connect(target{needWC: true, needRepo: true})
			if err != nil {
				return err
			}
			defer s.Close()
			files, err := fetch.Tree(ctx, s.repo, s.name, w.Base, fresh)
			if err != nil {
				return err
			}
			// Files under the old sparse set are already managed by the working
			// copy; never refetch them over local edits.
			oldSparse := w.Config.Sparse
			for p := range files {
				if wc.InSparse(oldSparse, p) {
					delete(files, p)
				}
			}
			var blockers []string
			for p, f := range files {
				if sparseBlocked(w.Root, p) {
					blockers = append(blockers, p)
					continue
				}
				if _, err := os.Lstat(filepath.Join(w.Root, filepath.FromSlash(p))); err != nil {
					continue
				}
				if data, mode, err := wc.ReadLocal(w.Root, p); err == nil && mode == f.Mode && objfmt.BlobSHA(data) == f.SHA {
					continue
				}
				blockers = append(blockers, p)
			}
			if len(blockers) > 0 {
				slices.Sort(blockers)
				return exitf(ExitError, "sparse add would overwrite local files; move them first: %s", strings.Join(blockers, ", "))
			}
			idx, err := materialize(ctx, s.repo, s.name, w.Root, files)
			if err != nil {
				return err
			}
			for p, e := range idx {
				w.Index[p] = e
			}
			added = len(idx)
			if w.Config.Sparse, err = wc.NormalizeSparse(append(oldSparse, fresh...)); err != nil {
				return err
			}
		}
		if err := w.Save(); err != nil {
			return err
		}
		return a.emit(map[string]any{"sparse": w.Config.Sparse, "files_added": added}, func(out io.Writer) {
			fmt.Fprintf(out, "sparse set: %v (%d files added)\n", w.Config.Sparse, added)
		})
	case "rm":
		if len(pos) == 0 {
			return usageErr(u)
		}
		remove := map[string]bool{}
		for _, arg := range pos {
			rp, err := w.RepoPath(a.io.Dir, arg)
			if err != nil || !slices.Contains(w.Config.Sparse, rp) {
				return exitf(ExitError, "%s is not a sparse path (see `sg sparse list`)", arg)
			}
			remove[rp] = true
		}
		var remaining []string
		for _, p := range w.Config.Sparse {
			if !remove[p] {
				remaining = append(remaining, p)
			}
		}
		if len(remaining) == 0 {
			return exitf(ExitError, "cannot remove the last sparse path (an empty set would mean a full checkout)")
		}
		changes, err := w.Status()
		if err != nil {
			return err
		}
		for _, c := range changes {
			if !wc.InSparse(remaining, c.Path) {
				return exitf(ExitError, "uncommitted change in %s; commit or revert it first", c.Path)
			}
		}
		removed := 0
		for _, p := range w.SortedPaths() {
			if !wc.InSparse(remaining, p) {
				if err := wc.RemoveLocal(w.Root, p); err != nil {
					return err
				}
				delete(w.Index, p)
				removed++
			}
		}
		w.Config.Sparse = remaining
		if err := w.Save(); err != nil {
			return err
		}
		return a.emit(map[string]any{"sparse": remaining, "files_removed": removed}, func(out io.Writer) {
			fmt.Fprintf(out, "sparse set: %v (%d files removed)\n", remaining, removed)
		})
	}
	return usageErr(u)
}

// sparseBlocked reports whether materializing p would clobber something: an
// ancestor that exists but is not a directory, or an unexpected Lstat error.
func sparseBlocked(root, p string) bool {
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(strings.Join(parts[:i], "/"))))
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil || !fi.IsDir() {
			return true
		}
	}
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
	return err != nil && !errors.Is(err, os.ErrNotExist)
}
