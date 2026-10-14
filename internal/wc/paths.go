package wc

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RepoPath converts a user-supplied path (relative to cwd, or absolute) into a
// slash-separated repo path. The working copy root itself is "".
func (w *WC) RepoPath(cwd, arg string) (string, error) {
	base, err := realDir(cwd)
	if err != nil {
		return "", err
	}
	f := arg
	if !filepath.IsAbs(f) {
		f = filepath.Join(base, arg)
	}
	rel, err := filepath.Rel(w.Root, filepath.Clean(f))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the working copy", arg)
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}
