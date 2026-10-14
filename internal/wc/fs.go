package wc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hermes98761234/sgit/internal/fsutil"
)

// ModeOf maps file info to a git mode; ok is false for unsupported types.
func ModeOf(fi fs.FileInfo) (string, bool) {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "120000", true
	case fi.Mode().IsRegular():
		if fi.Mode().Perm()&0o111 != 0 {
			return "100755", true
		}
		return "100644", true
	}
	return "", false
}

func full(root, p string) string { return filepath.Join(root, filepath.FromSlash(p)) }

// ValidPath rejects repo paths that could escape the working copy or touch
// its metadata: absolute paths, empty, "." or ".." segments, backslashes, NULs,
// and anything under .sg/.
func ValidPath(p string) error {
	bad := p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00")
	if !bad {
		for i, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." || (i == 0 && strings.EqualFold(seg, metaDir)) {
				bad = true
				break
			}
		}
	}
	if bad {
		return fmt.Errorf("invalid repo path %q", p)
	}
	return nil
}

// errBadParent means an ancestor of a repo path exists but is not a real
// directory (a file, or a symlink that could lead outside the working copy).
var errBadParent = errors.New("not a directory")

// checkParents Lstats every ancestor of repo path p under root and fails if
// one exists and is not a real directory. A missing ancestor is fine.
func checkParents(root, p string) error {
	dir := root
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			rel, _ := filepath.Rel(root, dir)
			return fmt.Errorf("%s: %w", filepath.ToSlash(rel), errBadParent)
		}
	}
	return nil
}

// ReadLocal returns the content of repo path p as git would store it (the
// target for symlinks) and its mode.
func ReadLocal(root, p string) ([]byte, string, error) {
	if err := ValidPath(p); err != nil {
		return nil, "", err
	}
	if err := checkParents(root, p); err != nil {
		return nil, "", err
	}
	f := full(root, p)
	fi, err := os.Lstat(f)
	if err != nil {
		return nil, "", err
	}
	mode, ok := ModeOf(fi)
	if !ok {
		return nil, "", fmt.Errorf("%s: unsupported file type", p)
	}
	if mode == "120000" {
		t, err := os.Readlink(f)
		return []byte(t), mode, err
	}
	data, err := os.ReadFile(f)
	return data, mode, err
}

// makeParents creates the parent directories of p, replacing files that are
// in the way (a tracked file that became a directory upstream).
func makeParents(root, p string) error {
	dir := root
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil:
			if err := os.Remove(dir); err != nil {
				return err
			}
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// WriteLocal writes data at repo path p with git mode, replacing an existing
// file or symlink. An empty directory in the way is removed; a non-empty one
// is an error.
func WriteLocal(root, p string, data []byte, mode string) error {
	if err := ValidPath(p); err != nil {
		return err
	}
	if err := makeParents(root, p); err != nil {
		return err
	}
	f := full(root, p)
	if fi, err := os.Lstat(f); err == nil && fi.IsDir() {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("%s: a directory is in the way", p)
		}
	}
	switch mode {
	case "120000":
		tmp := f + ".sg-tmp-link"
		_ = os.Remove(tmp)
		if err := os.Symlink(string(data), tmp); err != nil {
			return err
		}
		return os.Rename(tmp, f)
	case "100755":
		return fsutil.WriteAtomic(f, data, 0o755)
	default:
		return fsutil.WriteAtomic(f, data, 0o644)
	}
}

// RemoveLocal deletes repo path p and prunes parent directories that became
// empty. The root itself is never removed.
func RemoveLocal(root, p string) error {
	if err := ValidPath(p); err != nil {
		return err
	}
	if err := checkParents(root, p); err != nil {
		return err
	}
	f := full(root, p)
	if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(f); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

// StatEntry builds an index entry for p with the given mode and SHA and the
// file's current stat. A file still inside the racy window gets Size -1, so
// that a later same-size edit is re-hashed rather than trusted from the stat.
func StatEntry(root, p, mode, sha string) (Entry, error) {
	if err := ValidPath(p); err != nil {
		return Entry{}, err
	}
	fi, err := os.Lstat(full(root, p))
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Path: p, Mode: mode, SHA: sha, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if time.Since(fi.ModTime()) <= racyWindow {
		e.Size = -1
	}
	return e, nil
}
