package gitx

import (
	"bytes"
	"context"
	"testing"
)

// fork returns (base, ours, theirs) commits: ours and theirs both start at base.
func fork(t *testing.T, r *Repo, baseContent, oursContent, theirsContent string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	base := commitChanges(t, r, []Change{{Path: "f.txt", Op: OpAdd, BlobSHA: blob(t, r, baseContent)}})
	ours := commitChanges(t, r, []Change{{Path: "f.txt", Op: OpModify, BlobSHA: blob(t, r, oursContent)}})
	tree, err := r.BuildTree(ctx, base, []Change{{Path: "f.txt", Op: OpModify, BlobSHA: blob(t, r, theirsContent)}})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := r.CommitTree(ctx, tree, []string{base}, "theirs", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	return base, ours, theirs
}

func TestMergeTreeClean(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, ours, theirs := fork(t, r, "a\nb\nc\n", "A\nb\nc\n", "a\nb\nC\n")
	res, err := r.MergeTree(ctx, base, ours, theirs)
	if err != nil || !res.Clean || len(res.Conflicts) != 0 || !IsSHA(res.Tree) {
		t.Fatalf("res = %+v, %v", res, err)
	}
	c, _ := r.CommitTree(ctx, res.Tree, []string{ours}, "m", testSig, testSig)
	files, _ := r.ListFiles(ctx, c)
	obj, _ := r.ReadObject(ctx, files["f.txt"].SHA)
	if string(obj.Data) != "A\nb\nC\n" {
		t.Fatalf("merged = %q", obj.Data)
	}
}

func TestMergeTreeConflict(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, ours, theirs := fork(t, r, "a\nb\nc\n", "a\nB1\nc\n", "a\nB2\nc\n")
	res, err := r.MergeTree(ctx, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean || len(res.Conflicts) != 1 {
		t.Fatalf("res = %+v", res)
	}
	cf := res.Conflicts[0]
	if cf.Path != "f.txt" || !IsSHA(cf.Base) || !IsSHA(cf.Ours) || !IsSHA(cf.Theirs) {
		t.Fatalf("conflict = %+v", cf)
	}
	o, _ := r.ReadObject(ctx, cf.Ours)
	if string(o.Data) != "a\nB1\nc\n" {
		t.Fatalf("ours blob = %q", o.Data)
	}
}

func TestMergeFile(t *testing.T) {
	ctx := context.Background()
	out, conflicted, err := MergeFile(ctx, []byte("a\nb\nc\n"), []byte("A\nb\nc\n"), []byte("a\nb\nC\n"), "ours", "base", "theirs")
	if err != nil || conflicted || string(out) != "A\nb\nC\n" {
		t.Fatalf("clean: %q %v %v", out, conflicted, err)
	}
	out, conflicted, err = MergeFile(ctx, []byte("a\nb\n"), []byte("a\nX\n"), []byte("a\nY\n"), "main", "base", "local")
	if err != nil || !conflicted || !bytes.Contains(out, []byte("<<<<<<< main")) || !bytes.Contains(out, []byte(">>>>>>> local")) {
		t.Fatalf("conflict: %q %v %v", out, conflicted, err)
	}
	out, conflicted, err = MergeFile(ctx, []byte("a\x00"), []byte("b\x00"), []byte("c\x00"), "ours", "base", "theirs")
	if err != nil || !conflicted || string(out) != "b\x00" {
		t.Fatalf("binary: %q %v %v", out, conflicted, err)
	}
}
