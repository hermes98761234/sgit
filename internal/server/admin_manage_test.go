package server_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func wantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Errorf("%s: want %v, got %v", what, want, err)
	}
}

func TestManageNeedsAdmin(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	bob := e.NewUser("bob", "web", "write")
	ctx := context.Background()
	c := e.Admin(bob)
	_, err := c.ListUsers(ctx, &sgitv1.ListUsersRequest{})
	wantCode(t, "ListUsers", err, codes.PermissionDenied)
	_, err = c.SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: "bob", Admin: true})
	wantCode(t, "SetUserAdmin", err, codes.PermissionDenied)
	_, err = c.DeleteUser(ctx, &sgitv1.DeleteUserRequest{User: "admin"})
	wantCode(t, "DeleteUser", err, codes.PermissionDenied)
	_, err = c.ListTokens(ctx, &sgitv1.ListTokensRequest{})
	wantCode(t, "ListTokens", err, codes.PermissionDenied)
	_, err = c.ListACL(ctx, &sgitv1.ListACLRequest{})
	wantCode(t, "ListACL", err, codes.PermissionDenied)
	_, err = c.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "web"})
	wantCode(t, "DeleteRepo", err, codes.PermissionDenied)
	if _, err := e.Store.GetRepo(ctx, "web"); err != nil {
		t.Fatalf("repo must survive: %v", err)
	}
}

func TestListUsersPromoteDemote(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	adm := e.Admin(e.AdminToken)
	bob := e.NewUser("bob", "", "")
	rev := testenv.Must(adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "bob"}))
	testenv.Must(adm.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: rev.Id}))
	us := testenv.Must(adm.ListUsers(ctx, &sgitv1.ListUsersRequest{})).Users
	if len(us) != 2 || us[0].Name != "admin" || !us[0].Admin || us[0].ActiveTokens != 1 ||
		us[1].Name != "bob" || us[1].Admin || us[1].ActiveTokens != 1 || us[1].Email != "bob@sgit.local" || us[1].CreatedUnix == 0 {
		t.Fatalf("ListUsers = %v", us)
	}
	_, err := adm.SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: "admin", Admin: false})
	wantCode(t, "demote last admin", err, codes.FailedPrecondition)
	if !strings.Contains(status.Convert(err).Message(), "last admin") {
		t.Errorf("message = %q", status.Convert(err).Message())
	}
	_, err = adm.SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: "ghost", Admin: true})
	wantCode(t, "unknown user", err, codes.NotFound)
	testenv.Must(adm.SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: "bob", Admin: true}))
	// bob is now an admin with an existing token.
	testenv.Must(e.Admin(bob).ListUsers(ctx, &sgitv1.ListUsersRequest{}))
	testenv.Must(adm.SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: "admin", Admin: false}))
	_, err = adm.ListUsers(ctx, &sgitv1.ListUsersRequest{})
	wantCode(t, "demoted admin", err, codes.PermissionDenied)
	_, err = e.Admin(bob).SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: "bob", Admin: false})
	wantCode(t, "demote the last admin (self)", err, codes.FailedPrecondition)
}

func TestDeleteUser(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	main := e.CreateRepoProject("web")
	adm := e.Admin(e.AdminToken)
	bob := e.NewUser("bob", "web", "write")
	rc := e.Project(bob)
	sha := e.Upload(bob, "web", []byte("x\n"))[0]
	landed := testenv.Must(commitAndLand(rc, main, "a.txt", sha, "landed"))
	open := testenv.Must(rc.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: landed.Commit, Title: "wip", Message: "wip", Changes: []*sgitv1.Change{testenv.Add("b.txt", sha)}}))

	_, err := adm.DeleteUser(ctx, &sgitv1.DeleteUserRequest{User: "admin"})
	wantCode(t, "self delete", err, codes.FailedPrecondition)
	if !strings.Contains(status.Convert(err).Message(), "yourself") {
		t.Errorf("message = %q", status.Convert(err).Message())
	}
	_, err = adm.DeleteUser(ctx, &sgitv1.DeleteUserRequest{User: "ghost"})
	wantCode(t, "unknown user", err, codes.NotFound)
	boss := testenv.Must(adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "boss", Admin: true})).Token
	res := testenv.Must(e.Admin(boss).DeleteUser(ctx, &sgitv1.DeleteUserRequest{User: "bob"}))
	if res.TokensRevoked != 1 || res.AclsRemoved != 1 || res.DraftsAbandoned != 1 {
		t.Fatalf("DeleteUser = %v", res)
	}
	_, err = e.Admin(bob).WhoAmI(ctx, &sgitv1.WhoAmIRequest{})
	wantCode(t, "deleted user's token", err, codes.Unauthenticated)
	d := testenv.Must(e.Project(boss).GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: open.Id}))
	if d.Status != "abandoned" {
		t.Fatalf("draft status = %s", d.Status)
	}
	if files := e.Files("web", "main"); files["a.txt"] != "x\n" {
		t.Fatalf("landed history changed: %v", files)
	}
	us := testenv.Must(e.Admin(boss).ListUsers(ctx, &sgitv1.ListUsersRequest{})).Users
	for _, u := range us {
		if strings.HasPrefix(u.Name, "bob") {
			t.Fatalf("deleted user listed: %v", us)
		}
	}
	// Name reuse yields a fresh user without the old access.
	nb := e.NewUser("bob", "", "")
	_, err = e.Project(nb).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main"})
	wantCode(t, "reused name has no ACL", err, codes.NotFound)
}

func TestDeleteLastAdminGuard(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	// A non-admin cannot even try; an admin deleting "the last admin" is only
	// possible as themselves, which the self guard catches first.
	_, err := e.Admin(e.AdminToken).DeleteUser(ctx, &sgitv1.DeleteUserRequest{User: "admin"})
	wantCode(t, "delete self/last admin", err, codes.FailedPrecondition)
	testenv.Must(e.Admin(e.AdminToken).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
}

func TestListTokensAndACL(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	e.CreateRepoProject("web")
	e.CreateRepoProject("api")
	adm := e.Admin(e.AdminToken)
	e.NewUser("bob", "web", "write")
	e.NewUser("carol", "web", "read")
	testenv.Must(adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "bob", Project: "api", Level: "read"}))
	t2 := testenv.Must(adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "bob"}))
	testenv.Must(adm.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: t2.Id}))

	all := testenv.Must(adm.ListTokens(ctx, &sgitv1.ListTokensRequest{})).Tokens
	if len(all) != 4 { // admin, bob x2, carol
		t.Fatalf("ListTokens = %v", all)
	}
	bt := testenv.Must(adm.ListTokens(ctx, &sgitv1.ListTokensRequest{User: "bob"})).Tokens
	if len(bt) != 2 || bt[0].User != "bob" || bt[0].CreatedUnix == 0 {
		t.Fatalf("bob tokens = %v", bt)
	}
	revoked := 0
	for _, tk := range bt {
		if tk.RevokedUnix != 0 {
			revoked++
			if tk.Id != t2.Id {
				t.Errorf("wrong token revoked: %v", tk)
			}
		}
		if strings.Contains(tk.String(), "sgt_") {
			t.Errorf("token secret leaked: %v", tk)
		}
	}
	if revoked != 1 {
		t.Fatalf("revoked = %d", revoked)
	}
	_, err := adm.ListTokens(ctx, &sgitv1.ListTokensRequest{User: "ghost"})
	wantCode(t, "tokens unknown user", err, codes.NotFound)

	es := testenv.Must(adm.ListACL(ctx, &sgitv1.ListACLRequest{})).Entries
	if len(es) != 3 || es[0].User != "bob" || es[0].Project != "api" || es[0].Level != "read" || es[1].Project != "web" || es[1].Level != "write" {
		t.Fatalf("ListACL = %v", es)
	}
	if es = testenv.Must(adm.ListACL(ctx, &sgitv1.ListACLRequest{Project: "web"})).Entries; len(es) != 2 {
		t.Fatalf("by repo = %v", es)
	}
	if es = testenv.Must(adm.ListACL(ctx, &sgitv1.ListACLRequest{User: "carol"})).Entries; len(es) != 1 || es[0].Level != "read" {
		t.Fatalf("by user = %v", es)
	}
	_, err = adm.ListACL(ctx, &sgitv1.ListACLRequest{User: "ghost"})
	wantCode(t, "acl unknown user", err, codes.NotFound)
	_, err = adm.ListACL(ctx, &sgitv1.ListACLRequest{Project: "nope"})
	wantCode(t, "acl unknown repo", err, codes.NotFound)
}

func TestDeleteRepo(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	main := e.CreateRepoProject("web")
	adm := e.Admin(e.AdminToken)
	bob := e.NewUser("bob", "web", "write")
	sha := e.Upload(bob, "web", []byte("x\n"))[0]
	landed := testenv.Must(commitAndLand(e.Project(bob), main, "a.txt", sha, "landed"))

	_, err := adm.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "nope"})
	wantCode(t, "unknown repo", err, codes.NotFound)
	_, err = adm.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "../x"})
	wantCode(t, "bad name", err, codes.InvalidArgument)

	_, err = adm.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "web"})
	wantCode(t, "mounted repo", err, codes.FailedPrecondition)
	del := testenv.Must(adm.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "web"}))
	if del.DraftsRemoved != 1 || del.AclsRemoved != 1 {
		t.Fatalf("DeleteProject = %+v", del)
	}
	resp := testenv.Must(adm.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "web"}))
	if filepath.Dir(resp.MovedTo) != filepath.Join(e.DataDir, "deleted") ||
		!strings.HasPrefix(filepath.Base(resp.MovedTo), "web.") || !strings.HasSuffix(resp.MovedTo, ".git") {
		t.Fatalf("moved to %q", resp.MovedTo)
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "repos", "web.git")); !os.IsNotExist(err) {
		t.Fatalf("repo dir still present: %v", err)
	}
	g := gitx.Open(resp.MovedTo)
	got, err := g.ReadRef(ctx, gitx.MainRef)
	g.Close()
	if err != nil || got != landed.Commit {
		t.Fatalf("moved repo main = %q, %v; want %s", got, err, landed.Commit)
	}
	_, err = adm.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: "web"})
	wantCode(t, "status after delete", err, codes.NotFound)
	_, err = e.Project(bob).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main"})
	if status.Code(err) == codes.OK {
		t.Fatal("deleted repo still readable")
	}
	if es := testenv.Must(adm.ListACL(ctx, &sgitv1.ListACLRequest{User: "bob"})).Entries; len(es) != 0 {
		t.Fatalf("ACLs remain: %v", es)
	}
	// The name is free again, and bob does not inherit access.
	e.CreateRepoProject("web")
	_, err = e.Project(bob).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main"})
	wantCode(t, "recreated repo", err, codes.NotFound)
	// A second delete of the same name in the same second must not collide.
	testenv.Must(adm.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "web"}))
	testenv.Must(adm.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "web"}))
	ents, _ := os.ReadDir(filepath.Join(e.DataDir, "deleted"))
	if len(ents) != 2 {
		t.Fatalf("deleted dir has %d entries", len(ents))
	}
}

func TestDeleteRepoDuringLands(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	main := e.CreateRepoProject("web")
	alice := e.NewUser("alice", "web", "write")
	rc := e.Project(alice)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		sha := e.Upload(alice, "web", []byte{byte('a' + i)})[0]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Errors are fine: a land that loses the race to the delete fails.
			_, _ = commitAndLand(rc, main, string(rune('a'+i))+".txt", sha, "land")
		}()
	}
	close(start)
	// The project goes first (a mounted repo cannot be deleted); its delete
	// waits for the land in flight, and later lands find no project.
	_, err := e.Admin(e.AdminToken).DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "web"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.Admin(e.AdminToken).DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "web"})
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	// Whatever landed before the delete is intact and linear in the moved repo.
	g := gitx.Open(resp.MovedTo)
	defer g.Close()
	log := testenv.Must(g.Log(ctx, "main", "", 100))
	for _, le := range log[:len(log)-1] {
		if c := testenv.Must(g.ReadCommit(ctx, le.SHA)); len(c.Parents) != 1 {
			t.Fatalf("commit %s has %d parents", le.SHA, len(c.Parents))
		}
	}
	if _, err := e.Store.GetRepo(ctx, "web"); err == nil {
		t.Fatal("repo row remains")
	}
}
