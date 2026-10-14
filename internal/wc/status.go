package wc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hermes98761234/sgit/internal/objfmt"
	ignore "github.com/sabhiram/go-gitignore"
)

// Kind is the kind of a working-copy change.
type Kind int

const (
	Added Kind = iota + 1
	Modified
	Deleted
)

func (k Kind) String() string {
	switch k {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Deleted:
		return "deleted"
	}
	return "unknown"
}

// Change is one difference between the working tree and Base. Mode and SHA
// describe the local file (empty for deletions).
type Change struct {
	Path string
	Kind Kind
	Mode string
	SHA  string
}

// racyWindow: files modified this recently are always re-hashed, because an
// edit within the same mtime tick would be invisible to the stat check.
const racyWindow = 2 * time.Second

func loadIgnore(root string) (*ignore.GitIgnore, error) {
	p := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return ignore.CompileIgnoreLines(), nil
	}
	return ignore.CompileIgnoreFile(p)
}

// Status compares the working tree inside the sparse set with the index. It
// refreshes the stat cache of unchanged files outside the racy window in
// w.Index; callers may Save to persist the refresh.
func (w *WC) Status() ([]Change, error) {
	ign, err := loadIgnore(w.Root)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Change
	for _, p := range w.SortedPaths() {
		if !InSparse(w.Config.Sparse, p) {
			continue
		}
		e := w.Index[p]
		fi, err := os.Lstat(w.Abs(p))
		if errors.Is(err, os.ErrNotExist) {
			out = append(out, Change{Path: p, Kind: Deleted})
			continue
		}
		if err != nil {
			return nil, err
		}
		mode, ok := ModeOf(fi)
		if !ok {
			out = append(out, Change{Path: p, Kind: Deleted})
			continue
		}
		mtime := fi.ModTime().UnixNano()
		settled := now.Sub(fi.ModTime()) > racyWindow
		if settled && e.Size >= 0 && fi.Size() == e.Size && mtime == e.MtimeNs && mode == e.Mode {
			continue
		}
		data, _, err := ReadLocal(w.Root, p)
		if err != nil {
			return nil, err
		}
		sha := objfmt.BlobSHA(data)
		if sha == e.SHA && mode == e.Mode {
			if settled {
				e.Size, e.MtimeNs = fi.Size(), mtime
				w.Index[p] = e
			}
			continue
		}
		out = append(out, Change{Path: p, Kind: Modified, Mode: mode, SHA: sha})
	}
	err = filepath.WalkDir(w.Root, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.Root, f)
		if err != nil || rel == "." {
			return err
		}
		p := filepath.ToSlash(rel)
		if d.IsDir() {
			if p == metaDir || !TreeNeeded(w.Config.Sparse, p) || ign.MatchesPath(p+"/") || ValidPath(p) != nil {
				return filepath.SkipDir
			}
			return nil
		}
		if _, tracked := w.Index[p]; tracked || !InSparse(w.Config.Sparse, p) || ign.MatchesPath(p) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode, ok := ModeOf(fi)
		if !ok || ValidPath(p) != nil {
			return nil
		}
		data, _, err := ReadLocal(w.Root, p)
		if err != nil {
			return err
		}
		out = append(out, Change{Path: p, Kind: Added, Mode: mode, SHA: objfmt.BlobSHA(data)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
