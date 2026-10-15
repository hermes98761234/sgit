package server_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateAndUpdateDraft(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	ctx := context.Background()
	shas := e.Upload(alice, "web", []byte("v1\n"), []byte("v2\n"))

	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "add a\n\nbody"}))
	if len(d.Id) != 8 || d.Owner != "alice" || d.Title != "add a" || d.Version != 1 || d.Status != "open" || d.Base != main {
		t.Fatalf("draft = %+v", d)
	}
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	head := testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/head"))
	v1 := testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v1"))
	if head != d.Head || v1 != d.Head {
		t.Fatalf("refs head=%s v1=%s draft.head=%s", head, v1, d.Head)
	}
	if got := e.ProjectFiles("web", d.Head); got["a.txt"] != "v1\n" {
		t.Fatalf("draft files = %v", got)
	}
	if got := e.Files("web", "main"); len(got) != 0 {
		t.Fatal("creating a draft must not touch main")
	}
	commit := testenv.Must(g.ReadCommit(ctx, d.Head))
	if commit.Author.Name != "alice" || len(commit.Parents) != 1 || commit.Parents[0] != main {
		t.Fatalf("draft commit = %+v", commit)
	}

	d2 := testenv.Must(c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[1])}, Message: "add a v2"}))
	if d2.Version != 2 || d2.Head == d.Head {
		t.Fatalf("updated = %+v", d2)
	}
	if testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v1")) != d.Head {
		t.Fatal("v1 ref must be preserved")
	}
	if e.ProjectFiles("web", d2.Head)["a.txt"] != "v2\n" {
		t.Fatal("update must be a full delta vs base")
	}
	got := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: d.Id}))
	if got.Head != d2.Head {
		t.Fatalf("GetDraft = %+v", got)
	}
	list := testenv.Must(c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: "web", Mine: true, Status: "open"}))
	if len(list.Drafts) != 1 {
		t.Fatalf("ListDrafts = %+v", list.Drafts)
	}
	if _, err := c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: "web", Status: "weird"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad status filter: want InvalidArgument, got %v", err)
	}
	ab := testenv.Must(c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: "web", Id: d.Id}))
	if ab.Status != "abandoned" {
		t.Fatalf("abandon = %+v", ab)
	}
	if _, err := c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "m"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("update abandoned: want FailedPrecondition, got %v", err)
	}
}

func TestCreateDraftValidation(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	c := e.Project(e.AdminToken)
	ctx := context.Background()
	sha := e.Upload(e.AdminToken, "web", []byte("x"))[0]
	missing := "1111111111111111111111111111111111111111"
	cases := map[string]*sgitv1.CreateDraftRequest{
		"empty changes": {Project: "web", Base: main, Message: "m"},
		"no message":    {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}},
		"bad base":      {Project: "web", Base: "xyz", Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"},
		"unknown base":  {Project: "web", Base: missing, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"},
		"missing blob":  {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", missing)}, Message: "m"},
		"bad path":      {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("../a", sha)}, Message: "m"},
		"git dir":       {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add(".git/hooks/x", sha)}, Message: "m"},
		"bad mode":      {Project: "web", Base: main, Changes: []*sgitv1.Change{{Path: "a", Op: sgitv1.ChangeOp_CHANGE_OP_ADD, BlobSha: sha, Mode: "040000"}}, Message: "m"},
		"no op":         {Project: "web", Base: main, Changes: []*sgitv1.Change{{Path: "a", BlobSha: sha}}, Message: "m"},
		"duplicate":     {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha), testenv.Add("a", sha)}, Message: "m"},
		"file and dir":  {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha), testenv.Add("a/b", sha)}, Message: "m"},
		"delete absent": {Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Delete("nope")}, Message: "m"},
	}
	for name, req := range cases {
		if _, err := c.CreateDraft(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", name, err)
		}
	}
}

func TestCreateDraftUnicodePath(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	c := e.Project(e.AdminToken)
	sha := e.Upload(e.AdminToken, "web", []byte("x\n"))[0]
	d := testenv.Must(c.CreateDraft(context.Background(), &sgitv1.CreateDraftRequest{
		Project: "web", Base: main, Message: "unicode",
		Changes: []*sgitv1.Change{testenv.Add("docs/my notes/файл ü.md", sha)},
	}))
	if e.ProjectFiles("web", d.Head)["docs/my notes/файл ü.md"] != "x\n" {
		t.Fatalf("files = %v", e.ProjectFiles("web", d.Head))
	}
}

func TestDraftOwnership(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	alice := e.NewUser("alice", "web", "write")
	bob := e.NewUser("bob", "web", "write")
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("x"))[0]
	d := testenv.Must(e.Project(alice).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}))
	b := e.Project(bob)
	if _, err := b.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: d.Id, Base: main, Changes: []*sgitv1.Change{testenv.Add("a", sha)}, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob update: want PermissionDenied, got %v", err)
	}
	if _, err := b.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob abandon: want PermissionDenied, got %v", err)
	}
	testenv.Must(b.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: d.Id})) // reading is fine
	testenv.Must(e.Project(e.AdminToken).AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: "web", Id: d.Id}))
	if _, err := b.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: "zzzzzzzz"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown draft: want NotFound, got %v", err)
	}
	if _, err := b.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: "../x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad draft id: want InvalidArgument, got %v", err)
	}
}

func TestCreateDraftQuotedPathCannotSmuggleSgDir(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	alice := e.NewUser("alice", "web", "write")
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	const p = `"\056sg/config"`
	d, err := e.Project(alice).CreateDraft(context.Background(), &sgitv1.CreateDraftRequest{
		Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add(p, sha)}, Message: "sneaky"})
	if err != nil {
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want success or InvalidArgument, got %v", err)
		}
		return
	}
	files := e.ProjectFiles("web", d.Head)
	if _, ok := files[".sg/config"]; ok {
		t.Fatalf(".sg/config smuggled in: %v", files)
	}
	if _, ok := files[p]; !ok {
		t.Fatalf("literal name missing: %v", files)
	}
}

// Git drops index entries named like an alias of .git (GIT~1, ".git.",
// ".git ") with only a warning. The server must refuse them by name instead
// of landing a commit that silently lacks the file.
func TestGitAliasPathsRefusedNotDropped(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	ctx := context.Background()
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	for _, p := range []string{"lib/GIT~1/a.txt", "lib/.git./b.txt", "lib/.git /x", "git~1", ".git:x/y"} {
		ch := []*sgitv1.Change{testenv.Add("ok.txt", sha), testenv.Add(p, sha)}
		_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: main, Changes: ch, Message: "m"})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), strconv.Quote(p)) {
			t.Errorf("CreateDraft %q: want InvalidArgument naming the path, got %v", p, err)
		}
		_, err = c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: main, Changes: ch, Message: "m"})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), strconv.Quote(p)) {
			t.Errorf("CommitAndLand %q: want InvalidArgument naming the path, got %v", p, err)
		}
	}
	link := &sgitv1.Change{Path: ".gitmodules", Op: sgitv1.ChangeOp_CHANGE_OP_ADD, BlobSha: sha, Mode: "120000"}
	if _, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: main, Changes: []*sgitv1.Change{link}, Message: "m"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("symlink .gitmodules: want InvalidArgument, got %v", err)
	}
	if got := e.Files("web", "main"); len(got) != 0 {
		t.Fatalf("main changed: %v", got)
	}
}

// TestReservedNameInHistoryDeletableNotWritable: a file whose name the
// server refuses to add, already on main from imported history, can be
// deleted but not modified, and does not block unrelated lands.
func TestReservedNameInHistoryDeletableNotWritable(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	e.CommitReservedName("web")
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	ctx := context.Background()
	head := func() string {
		return testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main"})).Sha
	}
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	if _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head(), Changes: []*sgitv1.Change{testenv.Modify("GIT~1/a.txt", sha)}, Message: "m"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("modify: want InvalidArgument, got %v", err)
	}
	if _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head(), Changes: []*sgitv1.Change{testenv.Add("new.txt", sha)}, Message: "m"}); err != nil {
		t.Fatalf("unrelated add: %v", err)
	}
	if _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head(), Changes: []*sgitv1.Change{testenv.Delete("GIT~1/a.txt")}, Message: "m"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := e.Files("web", "main"); len(got) != 2 || got["new.txt"] != "x\n" || got["ok.txt"] != "a\n" {
		t.Fatalf("main = %v", got)
	}
}
