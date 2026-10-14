package server_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// coreWeb makes repo "core" (src/a.go, secret/k.txt) and project "web"
// mounting core:src at lib, and returns the env and the source main.
func coreWeb(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	e := testenv.Start(t)
	e.CreateRepo("core")
	main := e.CommitDirect("core", map[string]string{"src/a.go": "package a\n", "secret/k.txt": "top secret\n"}, "seed")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	return e, main
}

func TestProjectReadsServeOnlyMounts(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	bob := e.NewUser("bob", "web", "read")
	c := e.Project(bob)
	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}))
	if rr.Sha == srcMain || rr.ProjectName != "web" || rr.ProjectId != e.ProjectID("web") {
		t.Fatalf("ResolveRev = %+v", rr)
	}
	objs := testenv.Must(fetch(t, c, "web", rr.Sha))
	if len(objs) != 1 || objs[0].Type != "commit" {
		t.Fatalf("commit fetch = %+v", objs)
	}
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	files := testenv.Must(g.ListFiles(ctx, rr.Sha))
	if len(files) != 1 || files["lib/a.go"].SHA == "" {
		t.Fatalf("project files = %v", files)
	}
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web"}))
	if len(lg.Entries) != 1 || lg.Entries[0].Subject != "seed" || lg.Entries[0].Sha != rr.Sha {
		t.Fatalf("log = %+v", lg.Entries)
	}
	bl := testenv.Must(c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "lib/a.go"}))
	if len(bl.Lines) != 1 || bl.Lines[0].Sha != rr.Sha {
		t.Fatalf("blame = %+v", bl.Lines)
	}
	for _, rev := range []string{srcMain, srcMain[:12]} {
		if _, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: rev}); status.Code(err) != codes.NotFound {
			t.Fatalf("ResolveRev(source sha %s) = %v", rev, err)
		}
	}
	list := testenv.Must(c.ListProjects(ctx, &sgitv1.ListProjectsRequest{}))
	if len(list.Projects) != 1 || list.Projects[0].Name != "web" || list.Projects[0].Level != "read" {
		t.Fatalf("ListProjects = %+v", list)
	}
}

// statusBytes is the full wire form of a gRPC error (code, message, details).
func statusBytes(t *testing.T, err error) []byte {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return testenv.Must(proto.Marshal(status.Convert(err).Proto()))
}

func TestUnknownAndNoAccessAreIdentical(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	bob := e.NewUser("bob", "", "")
	c := e.Project(bob)
	sha := strings.Repeat("a", 40)
	calls := map[string]func(ref string) error{
		"ResolveRev": func(ref string) error {
			_, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: ref})
			return err
		},
		"GetObjects": func(ref string) error { _, err := fetch(t, c, ref, sha); return err },
		"HasObjects": func(ref string) error {
			_, err := c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: ref})
			return err
		},
		"UploadBlobs": func(ref string) error {
			_, err := rpc.UploadBlobs(ctx, c, ref, [][]byte{[]byte("x")})
			return err
		},
		"CreateDraft": func(ref string) error {
			_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: ref, Base: sha, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}})
			return err
		},
		"UpdateDraft": func(ref string) error {
			_, err := c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: ref, Id: "aaaaaaaa"})
			return err
		},
		"GetDraft": func(ref string) error {
			_, err := c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: ref, Id: "aaaaaaaa"})
			return err
		},
		"ListDrafts": func(ref string) error {
			_, err := c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: ref})
			return err
		},
		"AbandonDraft": func(ref string) error {
			_, err := c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: ref, Id: "aaaaaaaa"})
			return err
		},
		"Land": func(ref string) error {
			_, err := c.Land(ctx, &sgitv1.LandRequest{Project: ref, Id: "aaaaaaaa"})
			return err
		},
		"CommitAndLand": func(ref string) error {
			_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: ref, Base: sha, Message: "m"})
			return err
		},
		"Log": func(ref string) error { _, err := c.Log(ctx, &sgitv1.LogRequest{Project: ref}); return err },
		"Blame": func(ref string) error {
			_, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: ref, Path: "a"})
			return err
		},
		"Diff": func(ref string) error {
			_, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: ref, A: sha, B: sha})
			return err
		},
		"MergeFile": func(ref string) error { _, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: ref}); return err },
	}
	refs := []string{"web", "#1", "site", "1"} // "1" is a name that never exists (F5)
	before := map[string][]byte{}
	for name, call := range calls {
		for _, ref := range refs {
			err := call(ref)
			if status.Code(err) != codes.NotFound || status.Convert(err).Message() != `project "`+ref+`" not found or no access` {
				t.Fatalf("%s(%s) before create = %v", name, ref, err)
			}
			before[name+ref] = statusBytes(t, err)
		}
	}
	id := e.CreateProject("web") // id 1, the first project
	if id != 1 {
		t.Fatalf("first project id = %d", id)
	}
	e.Mount("web", "lib", "core", "src", false)
	testenv.Must(e.Admin(e.AdminToken).AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: "web", Alias: "site"}))
	for name, call := range calls {
		for _, ref := range refs {
			if got := statusBytes(t, call(ref)); string(got) != string(before[name+ref]) {
				t.Errorf("%s(%s): no-access error differs from unknown-project error", name, ref)
			}
		}
	}
	// Invalid names get the same shape too.
	if err := calls["Log"]("Bad Name!"); status.Convert(err).Message() != `project "Bad Name!" not found or no access` {
		t.Errorf("invalid name = %v", err)
	}
}

func TestWriteWithReadAccessIsPermissionDenied(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	bob := e.NewUser("bob", "web", "read")
	c := e.Project(bob)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if _, err := rpc.UploadBlobs(ctx, c, "web", [][]byte{[]byte("x")}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("UploadBlobs with read = %v", err)
	}
	if _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "m"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CommitAndLand with read = %v", err)
	}
}

func TestPathOutsideProjectRejected(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	for _, p := range []string{"other.txt", "lib", "secret/k.txt"} {
		_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add(p, sha)}})
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "path outside project: "+p {
			t.Errorf("CreateDraft(%s) = %v", p, err)
		}
	}
}

func TestReadOnlyPathRejected(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}}))
	e.Mount("web", "lib", "core", "src", true) // flip to read-only after the draft exists
	for _, tok := range []string{alice, e.AdminToken} {
		_, err := e.Project(tok).Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "read-only path: lib/x" {
			t.Fatalf("Land on read-only mount = %v", err)
		}
		_, err = e.Project(tok).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/y", sha)}})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("CreateDraft on read-only mount = %v", err)
		}
	}
}

func TestRevokedGrantBlocksLand(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}}))
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "alice", Project: "web", Level: "read"}))
	if _, err := c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Land after downgrade = %v", err)
	}
	testenv.Must(ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "alice", Project: "web"}))
	_, err := c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != `project "web" not found or no access` {
		t.Fatalf("Land after revoke = %v", err)
	}
	if files := e.Files("core", "main"); files["src/x"] != "" {
		t.Fatal("revoked draft reached the source repo")
	}
}

func TestLandThroughProject(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("new\n"))[0]
	resp := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "add b",
		Changes: []*sgitv1.Change{testenv.Add("lib/b.go", sha)}}))
	if resp.Parent != head {
		t.Fatalf("LandResponse.Parent = %s, want %s (landed on its base)", resp.Parent, head)
	}
	if got := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha; got != resp.Commit {
		t.Fatalf("project main = %s, land returned %s", got, resp.Commit)
	}
	files := e.Files("core", "main")
	if files["src/b.go"] != "new\n" || files["secret/k.txt"] != "top secret\n" || len(files) != 3 {
		t.Fatalf("source files = %v", files)
	}
	g := e.Git("core")
	defer g.Close()
	sm := testenv.Must(g.ReadCommit(ctx, testenv.Must(g.ReadRef(ctx, "refs/heads/main"))))
	if sm.Parents[0] != srcMain || sm.Author.Name != "alice" || sm.Message != "add b\n" {
		t.Fatalf("source commit = %+v", sm)
	}
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: resp.DraftId}))
	if d.Status != "landed" || d.LandedAs != resp.Commit || d.Project != "web" {
		t.Fatalf("draft = %+v", d)
	}
}

func TestLandConflictMapsPathsBack(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	e.CommitDirect("core", map[string]string{"src/a.go": "package theirs\n"}, "upstream edit")
	sha := e.Upload(alice, "web", []byte("package ours\n"))[0]
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "edit",
		Changes: []*sgitv1.Change{testenv.Modify("lib/a.go", sha)}})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("conflict = %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	now := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if det == nil || len(det.Files) != 1 || det.Files[0].Path != "lib/a.go" || det.Main != now || det.Partial {
		t.Fatalf("detail = %+v (project main %s)", det, now)
	}
	// The three blobs are readable through the project for the client's merge.
	for _, s := range []string{det.Files[0].BaseSha, det.Files[0].OursSha, det.Files[0].TheirsSha} {
		if _, err := fetch(t, c, "web", s); err != nil {
			t.Fatalf("conflict blob %s not readable: %v", s, err)
		}
	}
}

func TestCatchUpAfterLandThroughOtherProject(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	e.CreateProject("api")
	e.Mount("api", "code", "core", "src", false)
	ac := e.Project(e.AdminToken)
	head := testenv.Must(ac.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "api"})).Sha
	sha := e.Upload(e.AdminToken, "api", []byte("shared\n"))[0]
	testenv.Must(ac.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "api", Base: head, Message: "via api",
		Changes: []*sgitv1.Change{testenv.Add("code/s.go", sha)}}))
	lg := testenv.Must(ac.Log(ctx, &sgitv1.LogRequest{Project: "web", N: 1}))
	if lg.Entries[0].Subject != "via api" {
		t.Fatalf("web did not catch up: %+v", lg.Entries)
	}
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	if f := testenv.Must(g.ListFiles(ctx, "main")); f["lib/s.go"].SHA != sha {
		t.Fatalf("web files = %v", f)
	}
}

func TestDraftBaseMustBeAProjectCommit(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	sha := e.Upload(e.AdminToken, "web", []byte("x\n"))[0]
	_, err := e.Project(e.AdminToken).CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: srcMain, Message: "m",
		Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "base commit") {
		t.Fatalf("CreateDraft on a source base = %v", err)
	}
}

// landWithMountChange lands lib/x through web while change runs after the
// project was opened and before the land takes the project lock (F12).
func landWithMountChange(t *testing.T, change func(e *testenv.Env) error) (*testenv.Env, error) {
	t.Helper()
	ctx := context.Background()
	var e *testenv.Env
	var hookErr error
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeLand = func(int64) { hookErr = change(e) }
	})
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/a.go": "package a\n"}, "seed")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "m",
		Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}})
	if hookErr != nil {
		t.Fatalf("mount change: %v", hookErr)
	}
	return e, err
}

func TestLandRereadsMountsUnderTheLock(t *testing.T) {
	ctx := context.Background()
	e, err := landWithMountChange(t, func(e *testenv.Env) error {
		_, err := e.Admin(e.AdminToken).MountProject(ctx, &sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "src", ReadOnly: true})
		return err
	})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "read-only path: lib/x" {
		t.Fatalf("land after flip to read-only = %v", err)
	}
	if e.Files("core", "main")["src/x"] != "" {
		t.Fatal("land reached a read-only mount")
	}
	e, err = landWithMountChange(t, func(e *testenv.Env) error {
		_, err := e.Admin(e.AdminToken).UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "web", Path: "lib"})
		return err
	})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "path outside project: lib/x" {
		t.Fatalf("land after unmount = %v", err)
	}
	if e.Files("core", "main")["src/x"] != "" {
		t.Fatal("land reached an unmounted folder")
	}
}

func TestDeletedProjectLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	e, _ := coreWeb(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	sha := e.Upload(alice, "web", []byte("x\n"))[0]
	testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m", Changes: []*sgitv1.Change{testenv.Add("lib/x", sha)}}))
	id := e.ProjectID("web")
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "web"}))
	// Writes through a stale reference or id find nothing to write to.
	if _, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "alice", Project: "web", Level: "write"}); status.Code(err) != codes.NotFound {
		t.Fatalf("grant on deleted project = %v", err)
	}
	if _, err := ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "#" + strconv.FormatInt(id, 10), Path: "lib", Repo: "core", Folder: "src"}); status.Code(err) != codes.NotFound {
		t.Fatalf("mount on deleted project = %v", err)
	}
	for _, table := range []string{"acl", "drafts", "project_mounts", "project_commits", "project_commit_sources", "project_cursors", "project_aliases"} {
		var n int
		if err := e.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE project_id=?`, id).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows of deleted project = %d %v", table, n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "projects", strconv.FormatInt(id, 10)+".git")); !os.IsNotExist(err) {
		t.Errorf("project repo of deleted project: %v", err)
	}
}

// A failure inside a source repo reaches the client only as a generic
// Internal error: no repo name, source path or source SHA.
func TestSourceFailuresAreHidden(t *testing.T) {
	ctx := context.Background()
	e, srcMain := coreWeb(t)
	bob := e.NewUser("bob", "web", "write")
	c := e.Project(bob)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	e.CommitDirect("core", map[string]string{"src/new.go": "package a\n"}, "moved on") // forces a catch-up
	src := filepath.Join(e.DataDir, "repos", "core.git")
	if err := os.Rename(src, src+".away"); err != nil {
		t.Fatal(err)
	}
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "m"})
	msg := status.Convert(err).Message()
	if status.Code(err) != codes.Internal || !strings.HasPrefix(msg, "internal error (request ") {
		t.Fatalf("source failure = %v", err)
	}
	for _, leak := range []string{"core", "src/", srcMain[:7], e.DataDir} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q leaks %q", msg, leak)
		}
	}
}
