package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
)

const materializeGroup = 500

func cmdCheckout(ctx context.Context, a *app, args []string) error {
	const u = "checkout <project|alias> [dir] [--path P]... [--at REV] [--server S]"
	fs := a.newFlags("checkout")
	var paths stringList
	fs.Var(&paths, "path", "check out only this directory (repeatable; sparse checkout)")
	at := fs.String("at", "main", "revision to check out")
	server := fs.String("server", "", "server (default: from sg login)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageErr(u)
	}
	sparse, err := wc.NormalizeSparse(paths)
	if err != nil {
		return exitf(ExitError, "%v", err)
	}
	s, err := a.connect(target{server: *server, project: pos[0], needProject: true})
	if err != nil {
		return err
	}
	defer s.Close()
	// Resolve first: the default dir is the canonical name even for an
	// alias or #id, and later calls use the id so a rename cannot race.
	rr, err := s.proj.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: s.project, Rev: *at})
	if err != nil {
		return err
	}
	dir := rr.ProjectName
	if len(pos) == 2 {
		dir = pos[1]
	}
	dir = a.abs(dir)
	if _, err := os.Stat(filepath.Join(dir, ".sg", "base")); err == nil {
		return exitf(ExitError, "%s is already an sg working copy", dir)
	}
	if err := checkoutTarget(dir); err != nil {
		return err
	}
	ref := wcRef(rr.ProjectId)
	files, err := fetch.Tree(ctx, s.proj, ref, rr.Sha, sparse)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	w, err := wc.Create(dir, wc.Config{Server: s.server, Project: rr.ProjectId, Name: rr.ProjectName, Sparse: sparse})
	if err != nil {
		return err
	}
	unlock, err := w.Lock()
	if err != nil {
		return exitf(ExitError, "%v", err)
	}
	defer unlock()
	index, err := materialize(ctx, s.proj, ref, w.Root, files)
	if err != nil {
		return err
	}
	w.Base, w.Index = rr.Sha, index
	if err := w.Save(); err != nil {
		return err
	}
	res := map[string]any{"project": rr.ProjectName, "dir": w.Root, "commit": rr.Sha, "files": len(index)}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "checked out %s@%s (%d files) into %s\n", rr.ProjectName, short(rr.Sha), len(index), w.Root)
	})
}

// checkoutTarget accepts a missing or empty directory, or an interrupted
// checkout to resume: .sg/ exists but .sg/base does not. Anything else would
// put the user's files at risk.
func checkoutTarget(dir string) error {
	if fi, err := os.Stat(filepath.Join(dir, ".sg")); err == nil && fi.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err == nil && len(entries) == 0:
		return nil
	case err != nil && !errors.Is(err, syscall.ENOTDIR):
		return err
	}
	return exitf(ExitError, "%s exists and is not empty", dir)
}

// materialize writes files under root and returns their index entries. Files
// that already hold the right content (a resumed checkout) are not rewritten.
func materialize(ctx context.Context, c sgitv1.ProjectClient, project, root string, files map[string]fetch.File) (map[string]wc.Entry, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	index := make(map[string]wc.Entry, len(files))
	for start := 0; start < len(paths); start += materializeGroup {
		chunk := paths[start:min(start+materializeGroup, len(paths))]
		var need []string
		current := map[string]bool{}
		for _, p := range chunk {
			f := files[p]
			if data, mode, err := wc.ReadLocal(root, p); err == nil && mode == f.Mode && objfmt.BlobSHA(data) == f.SHA {
				current[p] = true
				continue
			}
			need = append(need, f.SHA)
		}
		blobs, err := fetch.Blobs(ctx, c, project, need)
		if err != nil {
			return nil, err
		}
		for _, p := range chunk {
			f := files[p]
			if !current[p] {
				if err := wc.WriteLocal(root, p, blobs[f.SHA], f.Mode); err != nil {
					return nil, err
				}
			}
			e, err := wc.StatEntry(root, p, f.Mode, f.SHA)
			if err != nil {
				return nil, err
			}
			index[p] = e
		}
	}
	return index, nil
}
