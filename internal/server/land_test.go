package server_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func commitAndLand(c sgitv1.RepoClient, base string, path, sha, msg string) (*sgitv1.LandResponse, error) {
	return c.CommitAndLand(context.Background(), &sgitv1.CommitAndLandRequest{
		Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Add(path, sha)}, Message: msg,
	})
}

// assertLinear checks that every commit on main has exactly one parent except the root.
func assertLinear(t *testing.T, e *testenv.Env, wantCommits int) {
	t.Helper()
	ctx := context.Background()
	g := e.Git("web")
	defer g.Close()
	log := testenv.Must(g.Log(ctx, "main", "", 1000))
	if len(log) != wantCommits {
		t.Fatalf("main has %d commits, want %d", len(log), wantCommits)
	}
	for i, le := range log {
		c := testenv.Must(g.ReadCommit(ctx, le.SHA))
		want := 1
		if i == len(log)-1 {
			want = 0
		}
		if len(c.Parents) != want {
			t.Fatalf("commit %s has %d parents", le.SHA, len(c.Parents))
		}
	}
}

func TestLandFastPath(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Repo(alice)
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("hello\n"))[0]
	resp := testenv.Must(commitAndLand(c, main, "a.txt", sha, "add a"))
	if e.Files("web", "main")["a.txt"] != "hello\n" {
		t.Fatal("file not on main")
	}
	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}))
	if rr.Sha != resp.Commit {
		t.Fatalf("main = %s, landed %s", rr.Sha, resp.Commit)
	}
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: resp.DraftId}))
	if d.Status != "landed" || d.LandedAs != resp.Commit {
		t.Fatalf("draft = %+v", d)
	}
	g := e.Git("web")
	defer g.Close()
	lc := testenv.Must(g.ReadCommit(ctx, resp.Commit))
	if lc.Author.Name != "alice" || lc.Message != "add a\n" {
		t.Fatalf("landed commit = %+v", lc)
	}
	assertLinear(t, e, 2)
}

func TestLandRebasesOntoMovedMain(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("a\n"), []byte("b\n"))
	d1 := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "a"}))
	d2 := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("b.txt", shas[1])}, Message: "b"}))
	testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d1.Id}))
	testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d2.Id}))
	files := e.Files("web", "main")
	if files["a.txt"] != "a\n" || files["b.txt"] != "b\n" {
		t.Fatalf("files = %v", files)
	}
	assertLinear(t, e, 3)
}

func TestLandConflict(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	base := e.CommitDirect("web", map[string]string{"f.txt": "a\nb\nc\n"}, "base")
	shas := e.Upload(e.AdminToken, "web", []byte("a\nB1\nc\n"), []byte("a\nB2\nc\n"))
	first := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[0])}, Message: "one"}))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[1])}, Message: "two"})
	st := status.Convert(err)
	if st.Code() != codes.Aborted {
		t.Fatalf("want Aborted, got %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range st.Details() {
		if cd, ok := d.(*sgitv1.ConflictDetail); ok {
			det = cd
		}
	}
	if det == nil || det.Main != first.Commit || len(det.DraftId) != 8 || len(det.Files) != 1 {
		t.Fatalf("detail = %+v", det)
	}
	f := det.Files[0]
	if f.Path != "f.txt" || f.OursSha != shas[0] || f.TheirsSha != shas[1] || f.BaseSha == "" {
		t.Fatalf("conflict file = %+v", f)
	}
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: det.DraftId}))
	if d.Status != "open" {
		t.Fatalf("conflicted draft must stay open: %+v", d)
	}
	if e.Files("web", "main")["f.txt"] != "a\nB1\nc\n" {
		t.Fatal("main changed by a conflicting land")
	}
	// Resolve: update the draft against the new main and land again.
	fixed := e.Upload(e.AdminToken, "web", []byte("a\nB1+B2\nc\n"))[0]
	testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", DraftId: det.DraftId, Base: first.Commit, Changes: []*sgitv1.Change{testenv.Modify("f.txt", fixed)}, Message: "two (resolved)"}))
	if e.Files("web", "main")["f.txt"] != "a\nB1+B2\nc\n" {
		t.Fatal("resolved land missing")
	}
}

func TestLandTwiceAndAbandoned(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("x"))[0]
	resp := testenv.Must(commitAndLand(c, main, "x", sha, "x"))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: resp.DraftId}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("land twice: want FailedPrecondition, got %v", err)
	}
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: resp.Commit, Changes: []*sgitv1.Change{testenv.Delete("x")}, Message: "rm"}))
	testenv.Must(c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("land abandoned: want FailedPrecondition, got %v", err)
	}
	assertLinear(t, e, 2)
}

func TestConcurrentLands(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	const n = 50
	var blobs [][]byte
	for i := 0; i < n; i++ {
		blobs = append(blobs, []byte(fmt.Sprintf("content %d\n", i)))
	}
	shas := e.Upload(e.AdminToken, "web", blobs...)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := commitAndLand(c, main, fmt.Sprintf("f%02d.txt", i), shas[i], fmt.Sprintf("agent %d", i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent land: %v", err)
		}
	}
	if got := len(e.Files("web", "main")); got != n {
		t.Fatalf("main has %d files, want %d", got, n)
	}
	assertLinear(t, e, n+1)
}

func TestLandRetriesWhenMainMoves(t *testing.T) {
	var e *testenv.Env
	var moved atomic.Bool
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeCAS = func(repo string) {
			if moved.CompareAndSwap(false, true) {
				e.CommitDirect(repo, map[string]string{"external.txt": "ext\n"}, "external writer")
			}
		}
	})
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	sha := e.Upload(e.AdminToken, "web", []byte("mine\n"))[0]
	testenv.Must(commitAndLand(c, main, "mine.txt", sha, "mine"))
	files := e.Files("web", "main")
	if files["external.txt"] != "ext\n" || files["mine.txt"] != "mine\n" {
		t.Fatalf("files = %v", files)
	}
	assertLinear(t, e, 3)
}

func TestLandGivesUpWhenMainKeepsMoving(t *testing.T) {
	var e *testenv.Env
	var n atomic.Int32
	e = testenv.Start(t, func(c *server.Config) {
		c.LandRetries = 3
		c.BeforeCAS = func(repo string) {
			i := n.Add(1)
			e.CommitDirect(repo, map[string]string{fmt.Sprintf("ext%d.txt", i): "x"}, "external")
		}
	})
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("mine\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("mine.txt", sha)}, Message: "mine"}))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", n.Load())
	}
	got := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}))
	if got.Status != "open" {
		t.Fatalf("draft must stay open: %+v", got)
	}
	if _, ok := e.Files("web", "main")["mine.txt"]; ok {
		t.Fatal("draft landed despite exhaustion")
	}
}

func TestLandByNonOwner(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	bob := e.NewUser("bob", "web", "write")
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("x"))[0]
	d := testenv.Must(e.Repo(alice).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}))
	if _, err := e.Repo(bob).Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob land: want PermissionDenied, got %v", err)
	}
	if _, err := e.Repo(bob).CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", DraftId: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob commit-and-land: want PermissionDenied, got %v", err)
	}
	testenv.Must(e.Repo(e.AdminToken).Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id})) // admin may land
}

func TestAbandonDuringLand(t *testing.T) {
	var e *testenv.Env
	var once atomic.Bool
	var draftID atomic.Value
	abandonErr := make(chan error, 1)
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeCAS = func(repo string) {
			if once.CompareAndSwap(false, true) {
				id, _ := draftID.Load().(string)
				go func() {
					_, err := e.Repo(e.AdminToken).AbandonDraft(context.Background(), &sgitv1.AbandonDraftRequest{Repo: repo, Id: id})
					abandonErr <- err
				}()
			}
		}
	})
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("mine\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("mine.txt", sha)}, Message: "mine"}))
	draftID.Store(d.Id)
	resp := testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}))
	select {
	case err := <-abandonErr:
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("abandon: want FailedPrecondition, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("abandon did not return")
	}
	got := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}))
	if got.Status != "landed" || got.LandedAs != resp.Commit {
		t.Fatalf("draft = %+v", got)
	}
}

func TestUpdateDuringLandCannotReopen(t *testing.T) {
	var e *testenv.Env
	var once atomic.Bool
	var draftID atomic.Value
	var other atomic.Value
	main := ""
	updateErr := make(chan error, 1)
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeCAS = func(repo string) {
			if once.CompareAndSwap(false, true) {
				id, _ := draftID.Load().(string)
				sha, _ := other.Load().(string)
				go func() {
					_, err := e.Repo(e.AdminToken).UpdateDraft(context.Background(), &sgitv1.UpdateDraftRequest{Repo: repo, Id: id, Base: main, Changes: []*sgitv1.Change{testenv.Add("other.txt", sha)}, Message: "again"})
					updateErr <- err
				}()
			}
		}
	})
	main = e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("mine\n"), []byte("other\n"))
	other.Store(shas[1])
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("mine.txt", shas[0])}, Message: "mine"}))
	draftID.Store(d.Id)
	testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}))
	select {
	case err := <-updateErr:
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("update: want FailedPrecondition, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("update did not return")
	}
	got := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}))
	if got.Status != "landed" {
		t.Fatalf("draft = %+v", got)
	}
	files := e.Files("web", "main")
	if files["mine.txt"] != "mine\n" {
		t.Fatalf("files = %v", files)
	}
	if _, ok := files["other.txt"]; ok {
		t.Fatal("update leaked onto main")
	}
}
