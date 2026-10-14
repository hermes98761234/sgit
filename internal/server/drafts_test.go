package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateAndUpdateDraft(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Repo(alice)
	ctx := context.Background()
	shas := e.Upload(alice, "web", []byte("v1\n"), []byte("v2\n"))

	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "add a\n\nbody"}))
	if len(d.Id) != 8 || d.Owner != "alice" || d.Title != "add a" || d.Version != 1 || d.Status != "open" || d.Base != main {
		t.Fatalf("draft = %+v", d)
	}
	g := e.Git("web")
	defer g.Close()
	head := testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/head"))
	v1 := testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v1"))
	if head != d.Head || v1 != d.Head {
		t.Fatalf("refs head=%s v1=%s draft.head=%s", head, v1, d.Head)
	}
	if got := e.Files("web", d.Head); got["a.txt"] != "v1\n" {
		t.Fatalf("draft files = %v", got)
	}
	if got := e.Files("web", "main"); len(got) != 0 {
		t.Fatal("creating a draft must not touch main")
	}
	commit := testenv.Must(g.ReadCommit(ctx, d.Head))
	if commit.Author.Name != "alice" || len(commit.Parents) != 1 || commit.Parents[0] != main {
		t.Fatalf("draft commit = %+v", commit)
	}

	d2 := testenv.Must(c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[1])}, Message: "add a v2"}))
	if d2.Version != 2 || d2.Head == d.Head {
		t.Fatalf("updated = %+v", d2)
	}
	if testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v1")) != d.Head {
		t.Fatal("v1 ref must be preserved")
	}
	if e.Files("web", d2.Head)["a.txt"] != "v2\n" {
		t.Fatal("update must be a full delta vs base")
	}
	got := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}))
	if got.Head != d2.Head {
		t.Fatalf("GetDraft = %+v", got)
	}
	list := testenv.Must(c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Repo: "web", Mine: true, Status: "open"}))
	if len(list.Drafts) != 1 {
		t.Fatalf("ListDrafts = %+v", list.Drafts)
	}
	if _, err := c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Repo: "web", Status: "weird"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad status filter: want InvalidArgument, got %v", err)
	}
	ab := testenv.Must(c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}))
	if ab.Status != "abandoned" {
		t.Fatalf("abandon = %+v", ab)
	}
	if _, err := c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "m"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("update abandoned: want FailedPrecondition, got %v", err)
	}
}

func TestCreateDraftValidation(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("x"))[0]
	missing := "1111111111111111111111111111111111111111"
	cases := map[string]*sgitv1.CreateDraftRequest{
		"empty changes": {Repo: "web", Base: main, Message: "m"},
		"no message":    {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}},
		"bad base":      {Repo: "web", Base: "xyz", Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"},
		"unknown base":  {Repo: "web", Base: missing, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"},
		"missing blob":  {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", missing)}, Message: "m"},
		"bad path":      {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("../a", sha)}, Message: "m"},
		"git dir":       {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add(".git/hooks/x", sha)}, Message: "m"},
		"bad mode":      {Repo: "web", Base: main, Changes: []*sgitv1.Change{{Path: "a", Op: sgitv1.ChangeOp_CHANGE_OP_ADD, BlobSha: sha, Mode: "040000"}}, Message: "m"},
		"no op":         {Repo: "web", Base: main, Changes: []*sgitv1.Change{{Path: "a", BlobSha: sha}}, Message: "m"},
		"duplicate":     {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha), testenv.Add("a", sha)}, Message: "m"},
		"file and dir":  {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha), testenv.Add("a/b", sha)}, Message: "m"},
		"delete absent": {Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Delete("nope")}, Message: "m"},
	}
	for name, req := range cases {
		if _, err := c.CreateDraft(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", name, err)
		}
	}
}

func TestCreateDraftUnicodePath(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	c := e.Repo(e.AdminToken)
	sha := e.Upload(e.AdminToken, "web", []byte("x\n"))[0]
	d := testenv.Must(c.CreateDraft(context.Background(), &sgitv1.CreateDraftRequest{
		Repo: "web", Base: main, Message: "unicode",
		Changes: []*sgitv1.Change{testenv.Add("docs/my notes/файл ü.md", sha)},
	}))
	if e.Files("web", d.Head)["docs/my notes/файл ü.md"] != "x\n" {
		t.Fatalf("files = %v", e.Files("web", d.Head))
	}
}

func TestDraftOwnership(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	bob := e.NewUser("bob", "web", "write")
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("x"))[0]
	d := testenv.Must(e.Repo(alice).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}))
	b := e.Repo(bob)
	if _, err := b.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob update: want PermissionDenied, got %v", err)
	}
	if _, err := b.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob abandon: want PermissionDenied, got %v", err)
	}
	testenv.Must(b.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id})) // reading is fine
	testenv.Must(e.Repo(e.AdminToken).AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id}))
	if _, err := b.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: "zzzzzzzz"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown draft: want NotFound, got %v", err)
	}
	if _, err := b.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: "../x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad draft id: want InvalidArgument, got %v", err)
	}
}

func TestCreateDraftQuotedPathCannotSmuggleSgDir(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	alice := e.NewUser("alice", "web", "write")
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	const p = `"\056sg/config"`
	d, err := e.Repo(alice).CreateDraft(context.Background(), &sgitv1.CreateDraftRequest{
		Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add(p, sha)}, Message: "sneaky"})
	if err != nil {
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want success or InvalidArgument, got %v", err)
		}
		return
	}
	files := e.Files("web", d.Head)
	if _, ok := files[".sg/config"]; ok {
		t.Fatalf(".sg/config smuggled in: %v", files)
	}
	if _, ok := files[p]; !ok {
		t.Fatalf("literal name missing: %v", files)
	}
}
