package wc

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// NormalizeSparse cleans sparse paths (slash-separated, relative, no "..")
// and drops entries already covered by another entry.
func NormalizeSparse(paths []string) ([]string, error) {
	var clean []string
	for _, p := range paths {
		c := path.Clean(strings.TrimSuffix(filepath.ToSlash(p), "/"))
		if p == "" || c == "." || strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") {
			return nil, fmt.Errorf("invalid sparse path %q", p)
		}
		clean = append(clean, c)
	}
	sort.Strings(clean)
	var out []string
	for _, c := range clean {
		if len(out) > 0 && InSparse(out, c) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// InSparse reports whether file path p is inside the sparse set; an empty set
// means the whole repository.
func InSparse(sparse []string, p string) bool {
	if len(sparse) == 0 {
		return true
	}
	for _, s := range sparse {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// TreeNeeded reports whether directory dir ("" = root) can contain files of
// the sparse set, i.e. whether a fetch must descend into it.
func TreeNeeded(sparse []string, dir string) bool {
	if len(sparse) == 0 || dir == "" {
		return true
	}
	for _, s := range sparse {
		if dir == s || strings.HasPrefix(dir, s+"/") || strings.HasPrefix(s, dir+"/") {
			return true
		}
	}
	return false
}
