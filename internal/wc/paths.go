package wc

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hermes98761234/sgit/internal/repopath"
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

// nestedMeta reports whether a directory name holds another repository or
// working copy (.git or .sg, any case); such directories are never walked.
func nestedMeta(name string) bool {
	return strings.EqualFold(name, ".git") || strings.EqualFold(name, metaDir)
}

// Committable reports whether the server can store p as a file (symlink for
// mode 120000). It is repopath.Check, the same rule the server applies, so a
// file is either sent or listed as skipped, never dropped on the way.
func Committable(p string, symlink bool) bool {
	return repopath.Check(p, symlink) == nil
}
