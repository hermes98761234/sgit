package wc

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
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

// Committable reports whether the server accepts p as a file path (see
// server.validatePath): valid UTF-8, at most 4096 bytes, no CR, LF or NUL.
// ValidPath must hold too.
func Committable(p string) bool {
	return len(p) <= 4096 && utf8.ValidString(p) && !strings.ContainsAny(p, "\x00\r\n")
}
