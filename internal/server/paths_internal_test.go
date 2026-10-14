package server

import (
	"testing"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMountMapping(t *testing.T) {
	ms := []store.Mount{
		{Path: "lib", Repo: "core", Folder: "src"},
		{Path: "lib/x", Repo: "other", Folder: "."}, // nesting is refused by Mount; mapping still picks the longest
		{Path: "docs", Repo: "core", Folder: "doc/en", ReadOnly: true},
	}
	cases := []struct{ p, repo, src string }{
		{"lib/a.go", "core", "src/a.go"},
		{"lib/x/y", "other", "y"},
		{"docs/r.md", "core", "doc/en/r.md"},
	}
	for _, c := range cases {
		m, ok := mountFor(ms, c.p)
		if !ok || m.Repo != c.repo || sourcePath(m, c.p) != c.src {
			t.Errorf("map %q = %+v %v -> %q", c.p, m, ok, sourcePath(m, c.p))
		}
	}
	for _, p := range []string{"lib", "other.txt", "libx/a", "docs"} {
		if _, ok := mountFor(ms, p); ok {
			t.Errorf("%q mapped to a mount", p)
		}
	}
	dot := []store.Mount{{Path: ".", Repo: "core", Folder: "src"}}
	if m, ok := mountFor(dot, "a/b"); !ok || sourcePath(m, "a/b") != "src/a/b" {
		t.Errorf(". mount mapping = %+v %v", m, ok)
	}
	if p, ok := projectPath(ms, "core", "doc/en/r.md"); !ok || p != "docs/r.md" {
		t.Errorf("projectPath = %q %v", p, ok)
	}
	if _, ok := projectPath(ms, "core", "secret/k"); ok {
		t.Error("projectPath mapped an unmounted source path")
	}
	err := checkPaths(ms, []string{"lib/a.go", "other.txt"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "path outside project: other.txt" {
		t.Errorf("outside = %v", err)
	}
	err = checkPaths(ms, []string{"docs/r.md"})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "read-only path: docs/r.md" {
		t.Errorf("read-only = %v", err)
	}
}

func TestWritableSource(t *testing.T) {
	ms := []store.Mount{
		{Path: "lib", Repo: "core", Folder: "src"},
		{Path: "gen", Repo: "core", Folder: "src/gen", ReadOnly: true},
		{Path: "all", Repo: "docs", Folder: ".", ReadOnly: true},
		{Path: "w", Repo: "docs", Folder: "w"},
		{Path: "x1", Repo: "dup", Folder: "f"},
		{Path: "x2", Repo: "dup", Folder: "f", ReadOnly: true},
	}
	for _, c := range []struct {
		repo, sp string
		ok       bool
	}{
		{"core", "src/a.go", true},
		{"core", "src/gen/z.go", false}, // nested read-only folder wins
		{"core", "secret/k.txt", false}, // unmounted
		{"core", "src", false},          // the folder itself is not inside it
		{"docs", "w/a", true},
		{"docs", "r/a", false},
		{"dup", "f/a", false}, // mounted twice, once read-only
		{"other", "src/a.go", false},
	} {
		if got := writableSource(ms, c.repo, c.sp); got != c.ok {
			t.Errorf("writableSource(%s, %s) = %v, want %v", c.repo, c.sp, got, c.ok)
		}
	}
}
