package gitx

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestLogBlameDiff(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c1 := commitChanges(t, r, []Change{{Path: "f.txt", Op: OpAdd, BlobSHA: blob(t, r, "one\n")}})
	c2 := commitChanges(t, r, []Change{
		{Path: "f.txt", Op: OpModify, BlobSHA: blob(t, r, "one\ntwo\n")},
		{Path: "g.txt", Op: OpAdd, BlobSHA: blob(t, r, "g\n")},
	})

	all, err := r.Log(ctx, "main", "", 10)
	if err != nil || len(all) != 3 || all[0].SHA != c2 || all[1].SHA != c1 || all[0].Subject != "c" || all[0].AuthorName != "alice" {
		t.Fatalf("log = %+v, %v", all, err)
	}
	onlyG, _ := r.Log(ctx, "main", "g.txt", 10)
	if len(onlyG) != 1 || onlyG[0].SHA != c2 {
		t.Fatalf("path log = %+v", onlyG)
	}
	limited, _ := r.Log(ctx, "main", "", 1)
	if len(limited) != 1 {
		t.Fatalf("n=1 log = %+v", limited)
	}

	lines, err := r.Blame(ctx, "main", "f.txt")
	if err != nil || len(lines) != 2 {
		t.Fatalf("blame = %+v, %v", lines, err)
	}
	if lines[0].SHA != c1 || lines[0].Line != 1 || lines[0].Text != "one" || lines[0].Author != "alice" ||
		lines[1].SHA != c2 || lines[1].Line != 2 || lines[1].Text != "two" {
		t.Fatalf("blame lines = %+v", lines)
	}
	if _, err := r.Blame(ctx, "main", "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blame missing: want ErrNotFound, got %v", err)
	}

	patch, err := r.Diff(ctx, c1, c2, nil)
	if err != nil || !bytes.Contains(patch, []byte("+two")) || !bytes.Contains(patch, []byte("g.txt")) {
		t.Fatalf("diff = %s, %v", patch, err)
	}
	patch, _ = r.Diff(ctx, c1, c2, []string{"g.txt"})
	if bytes.Contains(patch, []byte("+two")) {
		t.Fatalf("path-limited diff leaked f.txt: %s", patch)
	}
}

func TestIsAncestor(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c0, err := r.ReadRef(ctx, MainRef)
	if err != nil {
		t.Fatal(err)
	}
	c1, err := r.CommitTree(ctx, EmptyTree, []string{c0}, "one\n", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	sib, err := r.CommitTree(ctx, EmptyTree, []string{c0}, "sibling\n", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{c0, c1, true},
		{c1, c1, true},
		{c1, c0, false},
		{sib, c1, false},
		{"0123456789abcdef0123456789abcdef01234567", c1, false},
	} {
		if got, err := r.IsAncestor(ctx, tc.a, tc.b); err != nil || got != tc.want {
			t.Errorf("IsAncestor(%s, %s) = %v, %v; want %v, nil", tc.a, tc.b, got, err, tc.want)
		}
	}
	if _, err := r.IsAncestor(ctx, "main", c1); !errors.Is(err, ErrInvalid) {
		t.Errorf("IsAncestor(main, ...) = %v, want ErrInvalid", err)
	}
}
