package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
)

const materializeGroup = 500

func cmdCheckout(ctx context.Context, a *app, args []string) error {
	const u = "checkout <repo> [dir] [--path P]... [--at REV] [--server S]"
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
	repo, dir := pos[0], pos[0]
	if len(pos) == 2 {
		dir = pos[1]
	}
	dir = a.abs(dir)
	sparse, err := wc.NormalizeSparse(paths)
	if err != nil {
		return exitf(ExitError, "%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sg", "base")); err == nil {
		return exitf(ExitError, "%s is already an sg working copy", dir)
	}
	s, err := a.connect(target{server: *server, repo: repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	rr, err := s.repo.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: repo, Rev: *at})
	if err != nil {
		return err
	}
	files, err := fetch.Tree(ctx, s.repo, repo, rr.Sha, sparse)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	w, err := wc.Create(dir, wc.Config{Server: s.server, Repo: repo, Sparse: sparse})
	if err != nil {
		return err
	}
	index, err := materialize(ctx, s.repo, repo, w.Root, files)
	if err != nil {
		return err
	}
	w.Base, w.Index = rr.Sha, index
	if err := w.Save(); err != nil {
		return err
	}
	res := map[string]any{"repo": repo, "dir": w.Root, "commit": rr.Sha, "files": len(index)}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "checked out %s@%s (%d files) into %s\n", repo, short(rr.Sha), len(index), w.Root)
	})
}

// materialize writes files under root and returns their index entries. Files
// that already hold the right content (a resumed checkout) are not rewritten.
func materialize(ctx context.Context, c sgitv1.RepoClient, repo, root string, files map[string]fetch.File) (map[string]wc.Entry, error) {
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
		blobs, err := fetch.Blobs(ctx, c, repo, need)
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
