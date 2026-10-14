package wc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// ignoreRule is one .gitignore line; the last matching rule in a file wins.
type ignoreRule struct {
	m      *ignore.GitIgnore
	negate bool
}

// ignorer applies every .gitignore in the tree, like git: a file's nearest
// .gitignore with a matching rule decides, and the patterns are relative to
// the directory that holds that .gitignore.
type ignorer struct {
	root  string
	rules map[string][]ignoreRule // keyed by slash dir, "" for the root
}

func newIgnorer(root string) *ignorer {
	return &ignorer{root: root, rules: map[string][]ignoreRule{}}
}

// load reads dir/.gitignore (dir is slash-separated, "" for the root).
func (ig *ignorer) load(dir string) error {
	f := filepath.Join(ig.root, filepath.FromSlash(dir), ".gitignore")
	// Like git, only a regular file is a rule file; a directory or a
	// symlink named .gitignore is just another entry.
	if fi, err := os.Lstat(f); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	} else if err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	var rules []ignoreRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		negate := strings.HasPrefix(line, "!")
		if negate {
			line = line[1:]
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rules = append(rules, ignoreRule{m: ignore.CompileIgnoreLines(line), negate: negate})
	}
	if len(rules) > 0 {
		ig.rules[dir] = rules
	}
	return nil
}

// ignored reports whether the slash path p is ignored. Only directories
// already passed to load are consulted.
func (ig *ignorer) ignored(p string, isDir bool) bool {
	dir := p
	for {
		if i := strings.LastIndexByte(dir, '/'); i >= 0 {
			dir = dir[:i]
		} else if dir != "" {
			dir = ""
		} else {
			return false
		}
		rel := p
		if dir != "" {
			rel = p[len(dir)+1:]
		}
		if isDir {
			rel += "/"
		}
		rules := ig.rules[dir]
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].m.MatchesPath(rel) {
				return !rules[i].negate
			}
		}
	}
}

// Status compares the working tree inside the sparse set with the index. It
// refreshes the stat cache of unchanged files outside the racy window in
// w.Index; callers may Save to persist the refresh.
func (w *WC) Status() ([]Change, error) {
	ign := newIgnorer(w.Root)
	now := time.Now()
	var out []Change
	for _, p := range w.SortedPaths() {
		if !InSparse(w.Config.Sparse, p) {
			continue
		}
		e := w.Index[p]
		// A tracked file under a symlink or a file is gone from the tree.
		if err := checkParents(w.Root, p); errors.Is(err, errBadParent) {
			out = append(out, Change{Path: p, Kind: Deleted})
			continue
		} else if err != nil {
			return nil, err
		}
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
	w.Skipped = nil
	if err := ign.load(""); err != nil {
		return nil, err
	}
	err := filepath.WalkDir(w.Root, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.Root, f)
		if err != nil || rel == "." {
			return err
		}
		p := filepath.ToSlash(rel)
		if d.IsDir() {
			if nestedMeta(d.Name()) || !TreeNeeded(w.Config.Sparse, p) || ign.ignored(p, true) || ValidPath(p) != nil {
				return filepath.SkipDir
			}
			return ign.load(p)
		}
		// A .git file is a submodule or worktree link, never content.
		if _, tracked := w.Index[p]; tracked || !InSparse(w.Config.Sparse, p) || ign.ignored(p, false) || isTempFile(d.Name()) || nestedMeta(d.Name()) {
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
		if !Committable(p) {
			w.Skipped = append(w.Skipped, p)
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
	sort.Strings(w.Skipped)
	return out, nil
}

// isTempFile reports whether name is a temp file left by an interrupted
// atomic write (fsutil.WriteAtomic or WriteLocal's symlink swap).
func isTempFile(name string) bool {
	return (strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-")) || strings.HasSuffix(name, ".sg-tmp-link")
}
