package gitx

import (
	"context"
	"testing"
)

// SpliceTree must give the tree BuildTree gives for the same changes: the
// folder's subtree is derived on its own (as a project mount sees it), the
// changes are applied there, and the result is spliced into the base tree.
func TestSpliceTreeEqualsBuildTree(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base := commitChanges(t, r, []Change{
		{Path: "top.txt", Op: OpAdd, BlobSHA: blob(t, r, "top")},
		{Path: "d1/a.txt", Op: OpAdd, BlobSHA: blob(t, r, "a")},
		{Path: "d1/b.txt", Op: OpAdd, BlobSHA: blob(t, r, "b")},
		{Path: "d1/sub/c.txt", Op: OpAdd, BlobSHA: blob(t, r, "c")},
		{Path: "d2/z.txt", Op: OpAdd, BlobSHA: blob(t, r, "z")},
		{Path: "a/side.txt", Op: OpAdd, BlobSHA: blob(t, r, "side")},
		{Path: "a/b/side.txt", Op: OpAdd, BlobSHA: blob(t, r, "side2")},
		{Path: "a/b/c/deep.txt", Op: OpAdd, BlobSHA: blob(t, r, "deep")},
		{Path: "a/b/c/run.sh", Op: OpAdd, BlobSHA: blob(t, r, "#!/bin/sh"), Mode: "100755"},
	})
	n := blob(t, r, "new")
	type folderChanges struct {
		folder  string
		changes []Change // paths relative to folder
	}
	cases := []struct {
		name string
		fc   []folderChanges
	}{
		{"modify", []folderChanges{{"d1", []Change{{Path: "a.txt", Op: OpModify, BlobSHA: n}}}}},
		{"add nested dirs", []folderChanges{{"d1", []Change{{Path: "new/deep/x.txt", Op: OpAdd, BlobSHA: n}}}}},
		{"delete", []folderChanges{{"d1", []Change{{Path: "sub/c.txt", Op: OpDelete}}}}},
		{"delete whole folder", []folderChanges{{"d1", []Change{
			{Path: "a.txt", Op: OpDelete}, {Path: "b.txt", Op: OpDelete}, {Path: "sub/c.txt", Op: OpDelete}}}}},
		{"nested folder", []folderChanges{{"a/b/c", []Change{
			{Path: "deep.txt", Op: OpModify, BlobSHA: n}, {Path: "more/y.txt", Op: OpAdd, BlobSHA: n}}}}},
		{"nested folder emptied", []folderChanges{{"a/b/c", []Change{
			{Path: "deep.txt", Op: OpDelete}, {Path: "run.sh", Op: OpDelete}}}}},
		{"mode change", []folderChanges{{"a/b/c", []Change{{Path: "deep.txt", Op: OpModify, BlobSHA: n, Mode: "100755"}}}}},
		{"missing folder", []folderChanges{{"missing/x", []Change{{Path: "f.txt", Op: OpAdd, BlobSHA: n}}}}},
		{"two folders", []folderChanges{
			{"d2", []Change{{Path: "z.txt", Op: OpDelete}, {Path: "w.txt", Op: OpAdd, BlobSHA: n}}},
			{"a/b/c", []Change{{Path: "deep.txt", Op: OpModify, BlobSHA: n}}}}},
		{"dot", []folderChanges{{".", []Change{
			{Path: "top.txt", Op: OpModify, BlobSHA: n}, {Path: "d2/z.txt", Op: OpDelete}, {Path: "e/f.txt", Op: OpAdd, BlobSHA: n}}}}},
		{"dot emptied", []folderChanges{{".", func() []Change {
			files := must(r.ListFiles(ctx, base))
			var ch []Change
			for p := range files {
				ch = append(ch, Change{Path: p, Op: OpDelete})
			}
			return ch
		}()}}},
	}
	bc := must(r.ReadCommit(ctx, base))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var all []Change
			repl := map[string]string{}
			for _, fc := range c.fc {
				// The folder as a mount sees it: its own root, changed there.
				sub := must(r.Subtrees(ctx, []string{base}, fc.folder))[0]
				if sub == "" {
					sub = EmptyTree
				}
				mc := must(r.CommitTree(ctx, sub, nil, "mount", testSig, testSig))
				head := must(r.BuildTree(ctx, mc, fc.changes))
				repl[fc.folder] = head
				for _, ch := range fc.changes {
					if fc.folder != "." {
						ch.Path = fc.folder + "/" + ch.Path
					}
					all = append(all, ch)
				}
			}
			want := must(r.BuildTree(ctx, base, all))
			got := must(r.SpliceTree(ctx, bc.Tree, repl))
			if got != want {
				t.Fatalf("SpliceTree = %s, BuildTree = %s", got, want)
			}
		})
	}
}

func TestSpliceTreeRejectsNestedFolders(t *testing.T) {
	r := newRepo(t)
	for _, repl := range []map[string]string{
		{"a": EmptyTree, "a/b": EmptyTree},
		{".": EmptyTree, "a": EmptyTree},
		{"/a": EmptyTree},
		{"a/": EmptyTree},
		{"a": "nope"},
	} {
		if _, err := r.SpliceTree(context.Background(), EmptyTree, repl); err == nil {
			t.Fatalf("SpliceTree(%v) succeeded", repl)
		}
	}
}
