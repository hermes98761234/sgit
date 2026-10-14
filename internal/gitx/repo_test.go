package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var testSig = Signature{Name: "alice", Email: "alice@sgit.local", When: time.Unix(1700000000, 0).UTC()}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(context.Background(), filepath.Join(t.TempDir(), "r.git"), testSig)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestInitCreatesMain(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	main, err := r.ReadRef(ctx, MainRef)
	if err != nil || !IsSHA(main) {
		t.Fatalf("ReadRef main = %q, %v", main, err)
	}
	c, err := r.ReadCommit(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tree != EmptyTree || len(c.Parents) != 0 || c.Message != "Initial commit\n" {
		t.Fatalf("unexpected initial commit: %+v", c)
	}
	if c.Author.Name != "alice" || c.Author.Email != "alice@sgit.local" || !c.Author.When.Equal(testSig.When) {
		t.Fatalf("author = %+v", c.Author)
	}
}

func TestWriteAndReadBlob(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	sha, err := r.WriteBlob(ctx, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sha != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("sha = %s", sha)
	}
	obj, err := r.ReadObject(ctx, sha)
	if err != nil || obj.Type != "blob" || string(obj.Data) != "hello\n" {
		t.Fatalf("ReadObject = %+v, %v", obj, err)
	}
	typ, size, err := r.ObjectInfo(ctx, sha)
	if err != nil || typ != "blob" || size != 6 {
		t.Fatalf("ObjectInfo = %s %d %v", typ, size, err)
	}
}

func TestReadObjectMissingKeepsPoolHealthy(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	missing := "1111111111111111111111111111111111111111"
	for i := 0; i < 10; i++ {
		if _, err := r.ReadObject(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if _, err := r.ReadObject(ctx, "nothex"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := r.ReadObject(ctx, EmptyTree); err != nil {
		t.Fatalf("pool broken after misses: %v", err)
	}
}

func TestResolveRev(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	main, _ := r.ReadRef(ctx, MainRef)
	if got, err := r.ResolveRev(ctx, "main"); err != nil || got != main {
		t.Fatalf("ResolveRev main = %s, %v", got, err)
	}
	if got, err := r.ResolveRev(ctx, main); err != nil || got != main {
		t.Fatalf("ResolveRev sha = %s, %v", got, err)
	}
	if _, err := r.ResolveRev(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	for _, bad := range []string{"", "-x", "a b"} {
		if _, err := r.ResolveRev(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ResolveRev(%q): want ErrInvalid, got %v", bad, err)
		}
	}
}

func TestUpdateRefCAS(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	main, _ := r.ReadRef(ctx, MainRef)
	c2, err := r.CommitTree(ctx, EmptyTree, []string{main}, "second", testSig, testSig)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, MainRef, c2, c2); !errors.Is(err, ErrRefMoved) {
		t.Fatalf("stale old: want ErrRefMoved, got %v", err)
	}
	if err := r.UpdateRef(ctx, MainRef, c2, main); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/sg/drafts/abcdefgh/head", c2, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/sg/drafts/abcdefgh/head", c2, ""); !errors.Is(err, ErrRefMoved) {
		t.Fatalf("create existing: want ErrRefMoved, got %v", err)
	}
	if _, err := r.ReadRef(ctx, "refs/heads/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ref: want ErrNotFound, got %v", err)
	}
	c, _ := r.ReadCommit(ctx, c2)
	if c.Message != "second\n" || len(c.Parents) != 1 || c.Parents[0] != main {
		t.Fatalf("commit = %+v", c)
	}
}

func TestConcurrentReads(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	sha, _ := r.WriteBlob(ctx, []byte("x"))
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obj, err := r.ReadObject(ctx, sha)
			if err == nil && string(obj.Data) != "x" {
				err = errors.New("bad data")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
