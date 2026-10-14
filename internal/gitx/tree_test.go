package gitx

import (
	"context"
	"errors"
	"testing"

	"github.com/hermes98761234/sgit/internal/objfmt"
)

// commitChanges builds a tree from base+changes, commits it and moves main.
func commitChanges(t *testing.T, r *Repo, changes []Change) string {
	t.Helper()
	ctx := context.Background()
	main, err := r.ReadRef(ctx, MainRef)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.BuildTree(ctx, main, changes)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	c, err := r.CommitTree(ctx, tree, []string{main}, "c", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, MainRef, c, main); err != nil {
		t.Fatal(err)
	}
	return c
}

func blob(t *testing.T, r *Repo, s string) string {
	t.Helper()
	sha, err := r.WriteBlob(context.Background(), []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestBuildTreeAddModifyDelete(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	a, b := blob(t, r, "a\n"), blob(t, r, "b\n")
	commitChanges(t, r, []Change{
		{Path: "a.txt", Op: OpAdd, BlobSHA: a},
		{Path: "x/y/b.sh", Op: OpAdd, BlobSHA: b, Mode: "100755"},
	})
	files, err := r.ListFiles(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if files["a.txt"] != (FileEntry{Mode: "100644", SHA: a}) || files["x/y/b.sh"] != (FileEntry{Mode: "100755", SHA: b}) || len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	commitChanges(t, r, []Change{
		{Path: "a.txt", Op: OpModify, BlobSHA: b},
		{Path: "x/y/b.sh", Op: OpDelete},
	})
	files, _ = r.ListFiles(ctx, "main")
	if len(files) != 1 || files["a.txt"].SHA != b {
		t.Fatalf("after modify/delete: %+v", files)
	}
	main, _ := r.ReadRef(ctx, MainRef)
	if _, err := r.BuildTree(ctx, main, []Change{{Path: "nope.txt", Op: OpDelete}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete missing: want ErrInvalid, got %v", err)
	}
}

func TestBuildTreeUnicodeAndReplace(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	a := blob(t, r, "a\n")
	commitChanges(t, r, []Change{
		{Path: "dir with space/файл.txt", Op: OpAdd, BlobSHA: a},
		{Path: "d/inner.txt", Op: OpAdd, BlobSHA: a},
	})
	files, _ := r.ListFiles(ctx, "main")
	if _, ok := files["dir with space/файл.txt"]; !ok {
		t.Fatalf("unicode path missing: %+v", files)
	}
	// File replaces directory "d".
	commitChanges(t, r, []Change{{Path: "d", Op: OpAdd, BlobSHA: a}})
	files, _ = r.ListFiles(ctx, "main")
	if _, ok := files["d/inner.txt"]; ok || files["d"].SHA != a {
		t.Fatalf("file did not replace dir: %+v", files)
	}
	// Directory replaces file "d".
	commitChanges(t, r, []Change{{Path: "d/again.txt", Op: OpAdd, BlobSHA: a}})
	files, _ = r.ListFiles(ctx, "main")
	if _, ok := files["d"]; ok || files["d/again.txt"].SHA != a {
		t.Fatalf("dir did not replace file: %+v", files)
	}
}

func TestRawTreeParsesWithObjfmt(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	a := blob(t, r, "a\n")
	c := commitChanges(t, r, []Change{{Path: "a.txt", Op: OpAdd, BlobSHA: a}, {Path: "sub/b.txt", Op: OpAdd, BlobSHA: a}})
	cm, _ := r.ReadCommit(ctx, c)
	obj, err := r.ReadObject(ctx, cm.Tree)
	if err != nil || obj.Type != "tree" {
		t.Fatalf("tree read: %v %s", err, obj.Type)
	}
	entries, err := objfmt.ParseTree(obj.Data)
	if err != nil || len(entries) != 2 || entries[0].Name != "a.txt" || entries[0].SHA != a || !entries[1].IsTree() {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if objfmt.BlobSHA([]byte("a\n")) != a {
		t.Fatal("objfmt.BlobSHA disagrees with git")
	}
}
