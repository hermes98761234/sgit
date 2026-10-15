package server_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/testenv"
)

// A land builds the source tree by splicing the touched mounts' subtrees into
// the source base. Every land here must give exactly the tree BuildTree gives
// for the same changes on the same source base: adds, modifies and deletes,
// nested project paths and folders, a missing folder, a folder emptied, a
// "." mount, two touched mounts, and two touched mounts of nesting folders
// (which fall back to BuildTree).
func TestLandTreeEqualsBuildTree(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{
		"top.txt": "top\n", "src/a.txt": "a\n", "src/b.txt": "b\n", "src/sub/c.txt": "c\n",
		"src/lib/x.txt": "x\n", "docs/r.md": "r\n", "deep/er/f/g.txt": "g\n", "deep/keep.txt": "k\n",
	}, "seed")
	e.CreateProject("fold")
	e.Mount("fold", "app", "core", "src", false)
	e.Mount("fold", "n/e/s/t", "core", "deep/er", false)
	e.Mount("fold", "gone", "core", "missing/dir", false)
	e.Mount("fold", "lib2", "core", "src/lib", false)
	e.CreateProject("dot")
	e.Mount("dot", ".", "core", ".", false)
	e.CreateProject("sub")
	e.Mount("sub", ".", "core", "src", false)
	c := e.Project(e.AdminToken)
	src := e.Git("core")
	defer src.Close()

	n := 0
	land := func(project string, changes map[string]string) {
		t.Helper()
		n++
		base := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: project})).Sha
		srcBase := testenv.Must(src.ReadRef(ctx, gitx.MainRef))
		var pb []*sgitv1.Change
		for p, content := range changes {
			if content == "" {
				pb = append(pb, testenv.Delete(p))
				continue
			}
			sha := e.Upload(e.AdminToken, project, []byte(content))[0]
			pb = append(pb, testenv.Add(p, sha))
		}
		testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: project, Base: base, Changes: pb, Message: fmt.Sprintf("land %d", n)}))
		main := testenv.Must(src.ReadRef(ctx, gitx.MainRef))
		lc := testenv.Must(src.ReadCommit(ctx, main))
		if len(lc.Parents) != 1 || lc.Parents[0] != srcBase {
			t.Fatalf("land %d: source main %s has parents %v, want %s", n, main, lc.Parents, srcBase)
		}
		// The same changes in source paths, applied with a full index.
		var sc []gitx.Change
		for p, content := range changes {
			sp := sourcePathFor(project, p)
			if content == "" {
				sc = append(sc, gitx.Change{Path: sp, Op: gitx.OpDelete})
				continue
			}
			sc = append(sc, gitx.Change{Path: sp, Op: gitx.OpAdd, BlobSHA: testenv.Must(src.WriteBlob(ctx, []byte(content)))})
		}
		want := testenv.Must(src.BuildTree(ctx, srcBase, sc))
		if lc.Tree != want {
			t.Fatalf("land %d (%s %v): tree %s, BuildTree %s", n, project, changes, lc.Tree, want)
		}
	}

	land("fold", map[string]string{"app/a.txt": "a2\n", "app/new/deep/x.txt": "nx\n", "app/sub/c.txt": ""})
	land("fold", map[string]string{"n/e/s/t/h.txt": "h\n", "n/e/s/t/f/g.txt": "g2\n"})
	land("fold", map[string]string{"gone/first.txt": "first\n"})
	land("fold", map[string]string{"n/e/s/t/f/g.txt": "", "n/e/s/t/h.txt": ""}) // folder emptied
	land("fold", map[string]string{"app/b.txt": "b2\n", "gone/first.txt": ""})  // two mounts
	land("fold", map[string]string{"app/a.txt": "a3\n", "lib2/x.txt": "x2\n"})  // nesting folders
	land("dot", map[string]string{"top.txt": "top2\n", "docs/r.md": "", "new/y.txt": "y\n"})
	land("sub", map[string]string{"a.txt": "a4\n", "lib/z.txt": "z\n"})
	land("sub", map[string]string{"a.txt": "", "b.txt": "", "lib/x.txt": "", "lib/z.txt": "", "new/deep/x.txt": ""}) // src emptied
	land("dot", map[string]string{"top.txt": "", "deep/keep.txt": "", "new/y.txt": ""})                              // repo emptied
	if files := e.Files("core", "main"); len(files) != 0 {
		t.Fatalf("core main still has %v", files)
	}
}

// sourcePathFor maps a project path of the projects above to its core path.
func sourcePathFor(project, p string) string {
	switch project {
	case "dot":
		return p
	case "sub":
		return "src/" + p
	}
	for _, m := range [][2]string{{"app/", "src/"}, {"n/e/s/t/", "deep/er/"}, {"gone/", "missing/dir/"}, {"lib2/", "src/lib/"}} {
		if rest, ok := strings.CutPrefix(p, m[0]); ok {
			return m[1] + rest
		}
	}
	panic("unmapped path " + p)
}
