package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFirstParentsAndSubtrees(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c0, _ := r.ReadRef(ctx, MainRef)
	c1 := commitChanges(t, r, []Change{{Path: "a/x.txt", Op: OpAdd, BlobSHA: blob(t, r, "x1")}})
	c2 := commitChanges(t, r, []Change{{Path: "b/y.txt", Op: OpAdd, BlobSHA: blob(t, r, "y1")}})
	c3 := commitChanges(t, r, []Change{{Path: "a/x.txt", Op: OpModify, BlobSHA: blob(t, r, "x2")}})

	all, err := r.FirstParents(ctx, c3, "")
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for _, c := range all {
		shas = append(shas, c.SHA)
		if c.CommitterUnix != testSig.When.Unix() {
			t.Fatalf("committer time %d", c.CommitterUnix)
		}
	}
	if !reflect.DeepEqual(shas, []string{c0, c1, c2, c3}) {
		t.Fatalf("FirstParents = %v", shas)
	}
	tail, err := r.FirstParents(ctx, c3, c1)
	if err != nil || len(tail) != 2 || tail[0].SHA != c2 || tail[1].SHA != c3 {
		t.Fatalf("FirstParents(c3, c1) = %v, %v", tail, err)
	}
	if none, err := r.FirstParents(ctx, c3, c3); err != nil || len(none) != 0 {
		t.Fatalf("FirstParents(c3, c3) = %v, %v", none, err)
	}

	trees, err := r.Subtrees(ctx, []string{c0, c1, c2, c3}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if trees[0] != "" || trees[1] == "" || trees[2] != trees[1] || trees[3] == trees[2] {
		t.Fatalf("Subtrees(a) = %v", trees)
	}
	root, err := r.Subtrees(ctx, []string{c0}, ".")
	if err != nil || root[0] != EmptyTree {
		t.Fatalf("Subtrees(.) = %v, %v", root, err)
	}
	// A file is not a folder.
	if f, err := r.Subtrees(ctx, []string{c3}, "a/x.txt"); err != nil || f[0] != "" {
		t.Fatalf("Subtrees(file) = %v, %v", f, err)
	}
}

func TestComposeTree(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c := commitChanges(t, r, []Change{
		{Path: "src/a.go", Op: OpAdd, BlobSHA: blob(t, r, "a")},
		{Path: "docs/r.md", Op: OpAdd, BlobSHA: blob(t, r, "r")},
	})
	src := must(r.Subtrees(ctx, []string{c}, "src"))[0]
	docs := must(r.Subtrees(ctx, []string{c}, "docs"))[0]
	tree, err := r.ComposeTree(ctx, []MountTree{{Path: "lib/core", Tree: src}, {Path: "lib/doc", Tree: docs}, {Path: "gone", Tree: ""}})
	if err != nil {
		t.Fatal(err)
	}
	pc := must(r.CommitTree(ctx, tree, nil, "p", testSig, testSig))
	files := must(r.ListFiles(ctx, pc))
	if len(files) != 2 || files["lib/core/a.go"].SHA == "" || files["lib/doc/r.md"].SHA == "" {
		t.Fatalf("composed files = %v", files)
	}
	if got := must(r.ComposeTree(ctx, []MountTree{{Path: ".", Tree: src}})); got != src {
		t.Fatalf(". mount = %s, want %s", got, src)
	}
	if got := must(r.ComposeTree(ctx, []MountTree{{Path: ".", Tree: ""}})); got != EmptyTree {
		t.Fatalf("missing . mount = %s, want empty tree", got)
	}
	if got := must(r.ComposeTree(ctx, nil)); got != EmptyTree {
		t.Fatalf("no mounts = %s, want empty tree", got)
	}
}

func TestCopyObjectsCopiesOnlyWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	c1 := commitChanges(t, src, []Change{
		{Path: "pub/a.txt", Op: OpAdd, BlobSHA: blob(t, src, "public a")},
		{Path: "secret/k.txt", Op: OpAdd, BlobSHA: blob(t, src, "secret k")},
	})
	t1 := must(src.Subtrees(ctx, []string{c1}, "pub"))[0]
	dstDir := filepath.Join(t.TempDir(), "p.git")
	if err := InitBare(ctx, dstDir); err != nil {
		t.Fatal(err)
	}
	dst := Open(dstDir)
	defer dst.Close()
	n, err := CopyObjects(ctx, src, dst, []string{t1}, nil)
	if err != nil || n != 2 { // tree + one blob
		t.Fatalf("CopyObjects = %d, %v; want 2", n, err)
	}
	secret := must(src.Subtrees(ctx, []string{c1}, "secret"))[0]
	if _, _, err := dst.ObjectInfo(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unmounted tree copied: %v", err)
	}
	c2 := commitChanges(t, src, []Change{{Path: "pub/b.txt", Op: OpAdd, BlobSHA: blob(t, src, "public b")}})
	t2 := must(src.Subtrees(ctx, []string{c2}, "pub"))[0]
	n, err = CopyObjects(ctx, src, dst, []string{t2}, []string{t1})
	if err != nil || n != 2 { // new pub tree + b.txt; a.txt excluded by have
		t.Fatalf("incremental CopyObjects = %d, %v; want 2", n, err)
	}
	if n, err := CopyObjects(ctx, src, dst, []string{t2}, nil); err != nil || n != 0 {
		t.Fatalf("repeat CopyObjects = %d, %v; want 0", n, err)
	}
	if n, err := CopyObjects(ctx, src, dst, nil, nil); err != nil || n != 0 {
		t.Fatalf("empty CopyObjects = %d, %v", n, err)
	}
	// Blobs can be copied directly (used to move uploaded blobs to a source repo).
	b := blob(t, dst, "uploaded")
	if n, err := CopyObjects(ctx, dst, src, []string{b}, nil); err != nil || n != 1 {
		t.Fatalf("blob CopyObjects = %d, %v", n, err)
	}
}

func TestWriteCommitIsDeterministic(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c1 := commitChanges(t, r, []Change{{Path: "a", Op: OpAdd, BlobSHA: blob(t, r, "a")}})
	sc := must(r.ReadCommit(ctx, c1))
	if !strings.HasPrefix(sc.AuthorRaw, "alice <alice@sgit.local> 1700000000 ") {
		t.Fatalf("AuthorRaw = %q", sc.AuthorRaw)
	}
	// Re-writing the same commit from its raw parts yields the same SHA.
	again, err := r.WriteCommit(ctx, sc.Tree, sc.Parents, sc.AuthorRaw, sc.CommitterRaw, sc.Message)
	if err != nil || again != c1 {
		t.Fatalf("WriteCommit = %s, %v; want %s", again, err, c1)
	}
	sig := Signature{Name: "admin", Email: "admin@sgit.local", When: time.Unix(1700000500, 0).In(time.FixedZone("x", 3600))}
	if got := FormatIdent(sig); got != "admin <admin@sgit.local> 1700000500 +0000" {
		t.Fatalf("FormatIdent = %q", got)
	}
	if _, err := r.WriteCommit(ctx, sc.Tree, nil, "bad\nident", sc.CommitterRaw, "m\n"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("newline in ident: %v", err)
	}
}

func TestDiffTree(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base := commitChanges(t, r, []Change{
		{Path: "keep", Op: OpAdd, BlobSHA: blob(t, r, "k")},
		{Path: "mod", Op: OpAdd, BlobSHA: blob(t, r, "m1")},
		{Path: "del", Op: OpAdd, BlobSHA: blob(t, r, "d")},
	})
	m2 := blob(t, r, "m2")
	n := blob(t, r, "n")
	head := commitChanges(t, r, []Change{
		{Path: "mod", Op: OpModify, BlobSHA: m2, Mode: "100755"},
		{Path: "del", Op: OpDelete},
		{Path: "dir/new", Op: OpAdd, BlobSHA: n},
	})
	got, err := r.DiffTree(ctx, base, head)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "del", Op: OpDelete},
		{Path: "dir/new", Op: OpAdd, BlobSHA: n, Mode: "100644"},
		{Path: "mod", Op: OpModify, BlobSHA: m2, Mode: "100755"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffTree = %+v", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
