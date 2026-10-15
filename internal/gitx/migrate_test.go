package gitx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// sideCommit writes a commit on top of parent without moving main.
func sideCommit(t *testing.T, r *Repo, parent string, changes []Change, msg string, sig Signature) string {
	t.Helper()
	ctx := context.Background()
	tree, err := r.BuildTree(ctx, parent, changes)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	c, err := r.CommitTree(ctx, tree, []string{parent}, msg, sig, sig)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReplayUnder(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	root, _ := r.ReadRef(ctx, MainRef)
	base := commitChanges(t, r, []Change{{Path: "keep.txt", Op: OpAdd, BlobSHA: blob(t, r, "k")}})

	ann := Signature{Name: "Ann", Email: "ann@example.com", When: time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3*3600))}
	s1 := sideCommit(t, r, root, []Change{{Path: "a.go", Op: OpAdd, BlobSHA: blob(t, r, "a1")}}, "one\n", ann)
	s2 := sideCommit(t, r, s1, []Change{{Path: "b/c.go", Op: OpAdd, BlobSHA: blob(t, r, "c1")}}, "two\n", ann)
	x := sideCommit(t, r, s1, []Change{{Path: "side.txt", Op: OpAdd, BlobSHA: blob(t, r, "s")}}, "side\n", ann)
	mtree, err := r.BuildTree(ctx, s2, []Change{{Path: "side.txt", Op: OpAdd, BlobSHA: blob(t, r, "s")}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.CommitTree(ctx, mtree, []string{s2, x}, "merge\n", ann, ann)
	if err != nil {
		t.Fatal(err)
	}

	tip, n, err := r.ReplayUnder(ctx, base, m, "svc/api")
	if err != nil {
		t.Fatalf("ReplayUnder: %v", err)
	}
	orig, _ := r.FirstParents(ctx, m, "")
	if n != len(orig) || n != 4 { // root, s1, s2, m; x is off the first-parent line
		t.Fatalf("n = %d, first parents = %d", n, len(orig))
	}
	copies, err := r.FirstParents(ctx, tip, base)
	if err != nil || len(copies) != n {
		t.Fatalf("copies = %v, %v", copies, err)
	}
	bc, _ := r.ReadCommit(ctx, base)
	prev := base
	for i := range copies {
		o, _ := r.ReadCommit(ctx, orig[i].SHA)
		c, _ := r.ReadCommit(ctx, copies[i].SHA)
		if c.AuthorRaw != o.AuthorRaw || c.CommitterRaw != o.CommitterRaw || c.Message != o.Message {
			t.Fatalf("copy %d header/message differ: %+v vs %+v", i, c, o)
		}
		if len(c.Parents) != 1 || c.Parents[0] != prev {
			t.Fatalf("copy %d parents = %v, want [%s]", i, c.Parents, prev)
		}
		want, _ := r.SpliceTree(ctx, bc.Tree, map[string]string{"svc/api": o.Tree})
		if c.Tree != want {
			t.Fatalf("copy %d tree = %s, want %s", i, c.Tree, want)
		}
		prev = c.SHA
	}
	files, err := r.ListFiles(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"keep.txt", "svc/api/a.go", "svc/api/b/c.go", "svc/api/side.txt"} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing %s in %v", p, files)
		}
	}
	if len(files) != 4 {
		t.Errorf("files = %v", files)
	}
	if got, _ := r.ReadRef(ctx, MainRef); got != base {
		t.Fatalf("ReplayUnder moved main to %s", got)
	}
}

func TestReplayUnderKeepsEncodingDropsSignature(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, _ := r.ReadRef(ctx, MainRef)
	bc, _ := r.ReadCommit(ctx, base)
	raw := "tree " + bc.Tree + "\n" +
		"author A U <a@x.org> 1700000000 +0300\n" +
		"committer C <c@x.org> 1700000001 -0500\n" +
		"encoding ISO-8859-1\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n" +
		"\nsigned msg\n"
	out, err := r.run(ctx, strings.NewReader(raw), nil, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	signed := strings.TrimSpace(string(out))
	tip, n, err := r.ReplayUnder(ctx, base, signed, "x")
	if err != nil || n != 1 {
		t.Fatalf("ReplayUnder = %d, %v", n, err)
	}
	obj, err := r.ReadObject(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	got := string(obj.Data)
	want := "\nauthor A U <a@x.org> 1700000000 +0300\ncommitter C <c@x.org> 1700000001 -0500\nencoding ISO-8859-1\n\nsigned msg\n"
	if !strings.Contains(got, want) {
		t.Fatalf("copy %q lacks %q", got, want)
	}
	if strings.Contains(got, "gpgsig") {
		t.Fatalf("copy kept the signature: %q", got)
	}
}

func TestReplayUnderRefusesNonEmptyFolder(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	root, _ := r.ReadRef(ctx, MainRef)
	base := commitChanges(t, r, []Change{
		{Path: "svc/x.txt", Op: OpAdd, BlobSHA: blob(t, r, "x")},
		{Path: "file", Op: OpAdd, BlobSHA: blob(t, r, "f")},
	})
	tip := sideCommit(t, r, root, []Change{{Path: "a", Op: OpAdd, BlobSHA: blob(t, r, "a")}}, "a\n", testSig)
	for _, folder := range []string{"svc", "file", "file/sub", "file/sub/deep"} {
		if _, _, err := r.ReplayUnder(ctx, base, tip, folder); !errors.Is(err, ErrFolderNotEmpty) {
			t.Errorf("folder %q: err = %v, want ErrFolderNotEmpty", folder, err)
		}
	}
	if _, n, err := r.ReplayUnder(ctx, base, tip, "svc/new"); err != nil || n != 2 {
		t.Errorf("nested new folder: n=%d err=%v", n, err)
	}
	for _, folder := range []string{"", ".", "/abs", "a/"} {
		if _, _, err := r.ReplayUnder(ctx, base, tip, folder); !errors.Is(err, ErrInvalid) {
			t.Errorf("folder %q: err = %v, want ErrInvalid", folder, err)
		}
	}
}
