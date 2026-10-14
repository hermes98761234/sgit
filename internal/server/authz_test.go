package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAuthorizationMatrix pins the spec section 11 permission matrix.
func TestAuthorizationMatrix(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepo("web")
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("blob\n"))
	d := testenv.Must(e.Repo(e.AdminToken).CreateDraft(ctx, &sgitv1.CreateDraftRequest{
		Repo: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "a"}))
	reader := e.NewUser("bob-reader", "web", "read")
	stranger := e.NewUser("sam", "", "")
	plain := e.NewUser("pat", "web", "write")

	changes := []*sgitv1.Change{testenv.Add("b.txt", shas[0])}
	type tc struct {
		name string
		call func() error
	}
	denied := func(who string, cases []tc) {
		for _, c := range cases {
			if err := c.call(); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s %s: want PermissionDenied, got %v", who, c.name, err)
			}
		}
	}

	rc := e.Repo(reader)
	denied("read-only", []tc{
		{"CreateDraft", func() error {
			_, err := rc.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: "web", Base: main, Changes: changes, Message: "m"})
			return err
		}},
		{"UpdateDraft", func() error {
			_, err := rc.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: "web", Id: d.Id, Base: main, Changes: changes, Message: "m"})
			return err
		}},
		{"AbandonDraft", func() error {
			_, err := rc.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Repo: "web", Id: d.Id})
			return err
		}},
		{"Land", func() error { _, err := rc.Land(ctx, &sgitv1.LandRequest{Repo: "web", Id: d.Id}); return err }},
		{"CommitAndLand", func() error {
			_, err := rc.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Repo: "web", Base: main, Changes: changes, Message: "m"})
			return err
		}},
		{"UploadBlobs", func() error { _, err := rpc.UploadBlobs(ctx, rc, "web", [][]byte{[]byte("x")}); return err }},
	})

	sc := e.Repo(stranger)
	denied("no-ACL", []tc{
		{"ResolveRev", func() error { _, err := sc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}); return err }},
		{"GetObjects", func() error {
			s, err := sc.GetObjects(ctx, &sgitv1.GetObjectsRequest{Repo: "web", Shas: shas})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		}},
		{"HasObjects", func() error {
			_, err := sc.HasObjects(ctx, &sgitv1.HasObjectsRequest{Repo: "web", Shas: shas})
			return err
		}},
		{"Log", func() error { _, err := sc.Log(ctx, &sgitv1.LogRequest{Repo: "web"}); return err }},
		{"Blame", func() error { _, err := sc.Blame(ctx, &sgitv1.BlameRequest{Repo: "web", Path: "a.txt"}); return err }},
		{"Diff", func() error { _, err := sc.Diff(ctx, &sgitv1.DiffRequest{Repo: "web", A: main, B: d.Head}); return err }},
		{"MergeFile", func() error {
			_, err := sc.MergeFile(ctx, &sgitv1.MergeFileRequest{Repo: "web", BaseSha: shas[0], OursSha: shas[0], TheirsSha: shas[0]})
			return err
		}},
		{"GetDraft", func() error { _, err := sc.GetDraft(ctx, &sgitv1.GetDraftRequest{Repo: "web", Id: d.Id}); return err }},
		{"ListDrafts", func() error { _, err := sc.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Repo: "web"}); return err }},
	})

	ac := e.Admin(plain)
	denied("non-admin", []tc{
		{"CreateRepo", func() error { _, err := ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "other"}); return err }},
		{"CreateToken", func() error { _, err := ac.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "zed"}); return err }},
		{"RevokeToken", func() error { _, err := ac.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: "abc"}); return err }},
		{"GrantACL", func() error {
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "bob-reader", Repo: "web", Level: "write"})
			return err
		}},
		{"GrantACL bad level", func() error {
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "bob-reader", Repo: "web", Level: "bogus"})
			return err
		}},
		{"RevokeACL", func() error {
			_, err := ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "bob-reader", Repo: "web"})
			return err
		}},
	})
}
