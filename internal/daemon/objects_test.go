package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/daemon"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func seeded(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	return e, e.CommitDirect("web", map[string]string{"a.txt": "A\n", "src/x.go": "X\n", "docs/d.md": "D\n"}, "seed")
}

func readAll(t *testing.T, c sgitv1.ProjectClient, commit string) map[string]fetch.File {
	t.Helper()
	ctx := context.Background()
	tree := testenv.Must(fetch.Tree(ctx, c, "web", commit, nil))
	var shas []string
	for _, f := range tree {
		shas = append(shas, f.SHA)
	}
	testenv.Must(fetch.Blobs(ctx, c, "web", shas))
	return tree
}

func TestSecondFetchServedFromCache(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	readAll(t, d.Project(e.AdminToken), commit)
	first := d.Counter.Total()
	if first == 0 {
		t.Fatal("cold fetch must go upstream")
	}
	readAll(t, d.Project(e.AdminToken), commit)
	if d.Counter.Total() != first {
		t.Fatalf("warm fetch went upstream: %d -> %d", first, d.Counter.Total())
	}
	// A restarted daemon on the same cache directory serves from disk.
	reopened := testenv.Must(cache.Open(d.CacheDir, 1<<30))
	d2 := daemontest.Start(t, e.Addr, func(c *daemon.Config) { c.Cache = reopened })
	readAll(t, d2.Project(e.AdminToken), commit)
	if d2.Counter.Total() != 0 {
		t.Fatalf("restarted daemon went upstream for %d objects", d2.Counter.Total())
	}
}

func TestConcurrentRequestsShareOneUpstreamFetch(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	ctx := context.Background()
	tree := testenv.Must(fetch.Tree(ctx, e.Project(e.AdminToken), "web", commit, nil)) // directly, not via sgd
	sha := tree["a.txt"].SHA
	c := d.Project(e.AdminToken)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			b, err := fetch.Blobs(ctx, c, "web", []string{sha})
			if err == nil && string(b[sha]) != "A\n" {
				err = errors.New("wrong content")
			}
			errs <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := d.Counter.Requested(sha); n != 1 {
		t.Fatalf("blob requested upstream %d times, want 1", n)
	}
}

func TestCachedObjectsStillRequireReadAccess(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr, func(c *daemon.Config) { c.ACLTTL = 50 * time.Millisecond })
	ctx := context.Background()
	readAll(t, d.Project(e.AdminToken), commit) // cache is warm now
	nobody := e.NewUser("bob", "", "")
	if _, err := fetch.Tree(ctx, d.Project(nobody), "web", commit, nil); status.Code(err) != codes.NotFound {
		t.Fatalf("user without access got cached objects: %v", err)
	}
	reader := e.NewUser("bob-reader", "web", "read")
	testenv.Must(fetch.Tree(ctx, d.Project(reader), "web", commit, nil))
	testenv.Must(e.Admin(e.AdminToken).RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "bob-reader", Project: "web"}))
	time.Sleep(150 * time.Millisecond)
	if _, err := fetch.Tree(ctx, d.Project(reader), "web", commit, nil); status.Code(err) != codes.NotFound {
		t.Fatalf("revoked access still served after ACLTTL: %v", err)
	}
}

func TestCachedGetObjectsRequiresToken(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	tree := readAll(t, d.Project(e.AdminToken), commit) // cache is warm now
	sha := tree["a.txt"].SHA
	for _, tok := range []string{"", "sgt_nope"} {
		stream, err := d.Project(tok).GetObjects(context.Background(), &sgitv1.GetObjectsRequest{Project: "web", Shas: []string{sha}})
		if err == nil {
			var o *sgitv1.Object
			if o, err = stream.Recv(); err == nil {
				t.Fatalf("token %q received object %s", tok, o.Sha)
			}
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("token %q: want Unauthenticated, got %v", tok, err)
		}
	}
}

func TestCorruptCacheEntryIsRefetched(t *testing.T) {
	e, commit := seeded(t)
	d := daemontest.Start(t, e.Addr)
	c := d.Project(e.AdminToken)
	tree := readAll(t, c, commit)
	sha := tree["a.txt"].SHA
	before := d.Counter.Requested(sha)
	enc, _ := zstd.NewWriter(nil)
	forged := append([]byte("sgc1 blob 2\n"), enc.EncodeAll([]byte("B\n"), nil)...) // well-formed, wrong content
	if err := os.WriteFile(filepath.Join(d.CacheDir, sha[:2], sha[2:]), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	b := testenv.Must(fetch.Blobs(context.Background(), c, "web", []string{sha}))
	if string(b[sha]) != "A\n" {
		t.Fatalf("served forged content: %q", b[sha])
	}
	if d.Counter.Requested(sha) != before+1 {
		t.Fatal("corrupt entry must be refetched from upstream")
	}
}

func TestGetObjectsRejectsBadSHA(t *testing.T) {
	e, _ := seeded(t)
	d := daemontest.Start(t, e.Addr)
	stream, err := d.Project(e.AdminToken).GetObjects(context.Background(), &sgitv1.GetObjectsRequest{Project: "web", Shas: []string{"../../etc/passwd"}})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
