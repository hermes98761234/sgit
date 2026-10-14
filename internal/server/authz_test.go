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
	main := e.CreateRepoProject("web")
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("blob\n"))
	d := testenv.Must(e.Project(e.AdminToken).CreateDraft(ctx, &sgitv1.CreateDraftRequest{
		Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("a.txt", shas[0])}, Message: "a"}))
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

	// Without any grant, the project looks exactly like an unknown one.
	hidden := func(who string, cases []tc) {
		for _, c := range cases {
			if err := c.call(); status.Code(err) != codes.NotFound || status.Convert(err).Message() != `project "web" not found or no access` {
				t.Errorf("%s %s: want NotFound project \"web\" not found or no access, got %v", who, c.name, err)
			}
		}
	}

	rc := e.Project(reader)
	denied("read-only", []tc{
		{"CreateDraft", func() error {
			_, err := rc.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: main, Changes: changes, Message: "m"})
			return err
		}},
		{"UpdateDraft", func() error {
			_, err := rc.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: d.Id, Base: main, Changes: changes, Message: "m"})
			return err
		}},
		{"AbandonDraft", func() error {
			_, err := rc.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: "web", Id: d.Id})
			return err
		}},
		{"Land", func() error { _, err := rc.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}); return err }},
		{"CommitAndLand", func() error {
			_, err := rc.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: main, Changes: changes, Message: "m"})
			return err
		}},
		{"UploadBlobs", func() error { _, err := rpc.UploadBlobs(ctx, rc, "web", [][]byte{[]byte("x")}); return err }},
	})

	sc := e.Project(stranger)
	hidden("no-ACL", []tc{
		{"ResolveRev", func() error { _, err := sc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}); return err }},
		{"GetObjects", func() error {
			s, err := sc.GetObjects(ctx, &sgitv1.GetObjectsRequest{Project: "web", Shas: shas})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		}},
		{"HasObjects", func() error {
			_, err := sc.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: shas})
			return err
		}},
		{"Log", func() error { _, err := sc.Log(ctx, &sgitv1.LogRequest{Project: "web"}); return err }},
		{"Blame", func() error { _, err := sc.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "a.txt"}); return err }},
		{"Diff", func() error {
			_, err := sc.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: main, B: d.Head})
			return err
		}},
		{"MergeFile", func() error {
			_, err := sc.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: shas[0], OursSha: shas[0], TheirsSha: shas[0]})
			return err
		}},
		{"GetDraft", func() error {
			_, err := sc.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: d.Id})
			return err
		}},
		{"ListDrafts", func() error { _, err := sc.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: "web"}); return err }},
	})

	ac := e.Admin(plain)
	denied("non-admin", []tc{
		{"CreateRepo", func() error { _, err := ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "other"}); return err }},
		{"CreateToken", func() error { _, err := ac.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "zed"}); return err }},
		{"RevokeToken", func() error { _, err := ac.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: "abc"}); return err }},
		{"GrantACL", func() error {
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "bob-reader", Project: "web", Level: "write"})
			return err
		}},
		{"GrantACL bad level", func() error {
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "bob-reader", Project: "web", Level: "bogus"})
			return err
		}},
		{"RevokeACL", func() error {
			_, err := ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "bob-reader", Project: "web"})
			return err
		}},
	})
}
