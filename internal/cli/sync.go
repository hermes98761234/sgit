package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
)

type syncResult struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Updated   []string `json:"updated"`
	Merged    []string `json:"merged"`
	Conflicts []string `json:"conflicts"`
	Warnings  []string `json:"warnings"`
}

func cmdSync(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("sync")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("sync")
	}
	s, err := a.connect(target{needWC: true, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	rr, err := s.repo.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: s.name, Rev: "main"})
	if err != nil {
		return err
	}
	res, err := syncTo(ctx, s, rr.Sha)
	if err != nil {
		return err
	}
	if err := a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "synced %s -> %s\n", short(res.From), short(res.To))
		printSync(out, res)
	}); err != nil {
		return err
	}
	if len(res.Conflicts) > 0 {
		return exitf(ExitConflict, "%d file(s) have conflicts; edit them, then commit", len(res.Conflicts))
	}
	return nil
}

func printSync(out io.Writer, r syncResult) {
	for _, p := range r.Updated {
		fmt.Fprintf(out, "  updated:  %s\n", p)
	}
	for _, p := range r.Merged {
		fmt.Fprintf(out, "  merged:   %s\n", p)
	}
	for _, p := range r.Conflicts {
		fmt.Fprintf(out, "  CONFLICT: %s\n", p)
	}
	for _, m := range r.Warnings {
		fmt.Fprintf(out, "  warning:  %s\n", m)
	}
}

func newSyncResult(from, to string) syncResult {
	return syncResult{From: from, To: to, Updated: []string{}, Merged: []string{}, Conflicts: []string{}, Warnings: []string{}}
}

// syncTo rebases the working copy from its base onto target, merging local
// changes with upstream ones (3-way merges run on the server). It works in
// four phases: plan and pre-flight checks (read-only), network (nothing
// written locally), then the local apply. On any error before the apply
// phase nothing has changed on disk, and Save happens only on success.
func syncTo(ctx context.Context, s *session, target string) (syncResult, error) {
	w := s.wc
	res := newSyncResult(w.Base, target)
	if target == w.Base {
		return res, w.Save()
	}
	changes, err := w.Status()
	if err != nil {
		return res, err
	}
	local := make(map[string]wc.Change, len(changes))
	for _, c := range changes {
		local[c.Path] = c
	}
	oldT, err := fetch.Tree(ctx, s.repo, s.name, w.Base, w.Config.Sparse)
	if err != nil {
		return res, err
	}
	newT, err := fetch.Tree(ctx, s.repo, s.name, target, w.Config.Sparse)
	if err != nil {
		return res, err
	}
	all := map[string]bool{}
	for p := range oldT {
		all[p] = true
	}
	for p := range newT {
		all[p] = true
	}
	for p := range local {
		all[p] = true
	}
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Phase A: plan (no disk mutation).
	type merge struct {
		path, base, mode string
		local            wc.Change
		content          []byte
		conflicted       bool
	}
	var removes, writes []string
	var merges []*merge
	setIndex := map[string]wc.Entry{}
	dropIndex := map[string]bool{}
	for _, p := range paths {
		o, inOld := oldT[p]
		n, inNew := newT[p]
		lc, changed := local[p]
		if inOld == inNew && o == n {
			continue
		}
		if !changed {
			if inNew {
				writes = append(writes, p)
			} else {
				removes = append(removes, p)
			}
			continue
		}
		mergedMode := lc.Mode
		if inOld && lc.Mode == o.Mode && inNew {
			mergedMode = n.Mode
		}
		switch {
		case lc.Kind == wc.Deleted && inNew:
			setIndex[p] = wc.Entry{Path: p, Mode: n.Mode, SHA: n.SHA, Size: -1}
			res.Warnings = append(res.Warnings, p+": deleted locally but changed on main; keeping your deletion")
		case lc.Kind == wc.Deleted:
			dropIndex[p] = true
		case inNew && lc.SHA == n.SHA:
			switch {
			case mergedMode == n.Mode && lc.Mode == n.Mode:
				e, err := wc.StatEntry(w.Root, p, n.Mode, n.SHA)
				if err != nil {
					return res, err
				}
				setIndex[p] = e
			case mergedMode == n.Mode:
				writes = append(writes, p)
			default:
				setIndex[p] = wc.Entry{Path: p, Mode: n.Mode, SHA: n.SHA, Size: -1}
			}
		case !inNew:
			dropIndex[p] = true
			res.Warnings = append(res.Warnings, p+": changed locally but deleted on main; keeping your version")
		case lc.Mode == "120000" || n.Mode == "120000" || (inOld && o.Mode == "120000"):
			setIndex[p] = wc.Entry{Path: p, Mode: n.Mode, SHA: n.SHA, Size: -1}
			res.Warnings = append(res.Warnings, p+": symlink changed both locally and on main; keeping your version")
		default:
			base := ""
			if inOld {
				base = o.SHA
			}
			merges = append(merges, &merge{path: p, base: base, mode: mergedMode, local: lc})
		}
	}

	// Phase B: pre-flight blockers (read-only).
	removing := map[string]bool{}
	for _, p := range removes {
		removing[p] = true
	}
	var blockers []string
	blocked := func(p string) bool {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if removing[dir] {
				continue
			}
			if fi, err := os.Lstat(w.Abs(dir)); err == nil && !fi.IsDir() {
				return true
			}
		}
		fi, err := os.Lstat(w.Abs(p))
		if err != nil || !fi.IsDir() {
			return false
		}
		// A directory in the way is fine only if every file under it is
		// an unchanged tracked file that this sync removes.
		inWay := false
		_ = filepath.WalkDir(w.Abs(p), func(abs string, d fs.DirEntry, err error) error {
			if err != nil {
				inWay = true
				return filepath.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(w.Root, abs)
			if err != nil || !removing[filepath.ToSlash(rel)] {
				inWay = true
				return filepath.SkipAll
			}
			return nil
		})
		return inWay
	}
	for _, p := range removes {
		if blocked(p) {
			blockers = append(blockers, p)
		}
	}
	for _, m := range merges {
		if blocked(m.path) {
			blockers = append(blockers, m.path)
		}
	}
	keptWrites := writes[:0:0]
	for _, p := range writes {
		if blocked(p) {
			blockers = append(blockers, p)
			continue
		}
		if _, tracked := w.Index[p]; !tracked {
			// A directory in the way was already vetted by blocked().
			if fi, err := os.Lstat(w.Abs(p)); err == nil && !fi.IsDir() {
				n := newT[p]
				data, mode, err := wc.ReadLocal(w.Root, p)
				if err != nil || mode != n.Mode || objfmt.BlobSHA(data) != n.SHA {
					blockers = append(blockers, p)
					continue
				}
				e, err := wc.StatEntry(w.Root, p, n.Mode, n.SHA)
				if err != nil {
					return res, err
				}
				setIndex[p] = e
				continue
			} else if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				return res, err
			}
		}
		keptWrites = append(keptWrites, p)
	}
	writes = keptWrites
	if len(blockers) > 0 {
		sort.Strings(blockers)
		return res, exitf(ExitError, "sync would overwrite local files; move or commit them first: %s", strings.Join(blockers, ", "))
	}

	// Phase C: network (nothing written locally).
	blobs := map[string][]byte{}
	for start := 0; start < len(writes); start += materializeGroup {
		chunk := writes[start:min(start+materializeGroup, len(writes))]
		shas := make([]string, 0, len(chunk))
		for _, p := range chunk {
			shas = append(shas, newT[p].SHA)
		}
		got, err := fetch.Blobs(ctx, s.repo, s.name, shas)
		if err != nil {
			return res, err
		}
		for sha, data := range got {
			blobs[sha] = data
		}
	}
	if len(merges) > 0 {
		want := map[string]string{}
		for _, m := range merges {
			want[m.local.SHA] = m.path
		}
		if err := uploadMissing(ctx, s, want); err != nil {
			return res, err
		}
		for _, m := range merges {
			r, err := s.repo.MergeFile(ctx, &sgitv1.MergeFileRequest{
				Repo: s.name, BaseSha: m.base, OursSha: newT[m.path].SHA, TheirsSha: m.local.SHA, OursLabel: "main", TheirsLabel: "local",
			})
			if err != nil {
				return res, err
			}
			m.content, m.conflicted = r.Content, r.Conflicted
		}
	}

	// Phase D: apply locally.
	for _, p := range removes {
		if err := wc.RemoveLocal(w.Root, p); err != nil {
			return res, err
		}
		delete(w.Index, p)
	}
	for _, p := range writes {
		f := newT[p]
		if err := wc.WriteLocal(w.Root, p, blobs[f.SHA], f.Mode); err != nil {
			return res, err
		}
		e, err := wc.StatEntry(w.Root, p, f.Mode, f.SHA)
		if err != nil {
			return res, err
		}
		w.Index[p] = e
	}
	res.Updated = append(append(res.Updated, removes...), writes...)
	sort.Strings(res.Updated)
	for _, m := range merges {
		n := newT[m.path]
		if err := wc.WriteLocal(w.Root, m.path, m.content, m.mode); err != nil {
			return res, err
		}
		w.Index[m.path] = wc.Entry{Path: m.path, Mode: n.Mode, SHA: n.SHA, Size: -1}
		if m.conflicted {
			res.Conflicts = append(res.Conflicts, m.path)
		} else {
			res.Merged = append(res.Merged, m.path)
		}
	}
	for p := range dropIndex {
		delete(w.Index, p)
	}
	for p, e := range setIndex {
		w.Index[p] = e
	}
	w.Base = target
	seen := map[string]bool{}
	var conflicts []string
	for _, p := range append(append([]string{}, w.Conflicts...), res.Conflicts...) {
		if !seen[p] {
			seen[p] = true
			conflicts = append(conflicts, p)
		}
	}
	w.Conflicts = conflicts
	return res, w.Save()
}
