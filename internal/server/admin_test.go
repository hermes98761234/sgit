package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestWhoAmIAndHealth(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	who := testenv.Must(e.Admin(e.AdminToken).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
	if who.User != "admin" || !who.Admin {
		t.Fatalf("WhoAmI = %+v", who)
	}
	h := testenv.Must(healthpb.NewHealthClient(e.Conn("")).Check(ctx, &healthpb.HealthCheckRequest{}))
	if h.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v", h.Status)
	}
	if _, err := e.Admin("").WhoAmI(ctx, &sgitv1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: want Unauthenticated, got %v", err)
	}
	if _, err := e.Admin("sgt_bogus").WhoAmI(ctx, &sgitv1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: want Unauthenticated, got %v", err)
	}
}

func TestCreateRepo(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	main := e.CreateRepo("web")
	if len(main) != 40 {
		t.Fatalf("main = %q", main)
	}
	if len(e.Files("web", "main")) != 0 {
		t.Fatal("new repo must be empty")
	}
	adm := e.Admin(e.AdminToken)
	if _, err := adm.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "web"}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("dup: want AlreadyExists, got %v", err)
	}
	for _, bad := range []string{"", "Web", "a/b", "x.git", "a..b", "-x"} {
		if _, err := adm.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateRepo(%q): want InvalidArgument, got %v", bad, err)
		}
	}
	bob := e.NewUser("bob", "", "")
	if _, err := e.Admin(bob).CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "other"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestTokensAndACL(t *testing.T) {
	e := testenv.Start(t)
	ctx := context.Background()
	e.CreateRepo("web")
	adm := e.Admin(e.AdminToken)
	tok := testenv.Must(adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: "carol"}))
	if tok.User != "carol" || len(tok.Id) != 8 {
		t.Fatalf("CreateToken = %+v", tok)
	}
	who := testenv.Must(e.Admin(tok.Token).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
	if who.User != "carol" || who.Admin || who.Email != "carol@sgit.local" {
		t.Fatalf("WhoAmI = %+v", who)
	}
	if _, err := adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "carol", Repo: "web", Level: "admin"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad level: want InvalidArgument, got %v", err)
	}
	if _, err := adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "carol", Repo: "nope", Level: "read"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown repo: want NotFound, got %v", err)
	}
	if _, err := adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "nobody", Repo: "web", Level: "read"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown user: want NotFound, got %v", err)
	}
	testenv.Must(adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: "carol", Repo: "web", Level: "read"}))
	testenv.Must(e.Repo(tok.Token).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web", Rev: "main"}))
	testenv.Must(adm.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: "carol", Repo: "web"}))
	if _, err := e.Repo(tok.Token).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web", Rev: "main"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("after revoke ACL: want PermissionDenied, got %v", err)
	}
	testenv.Must(adm.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: tok.Id}))
	if _, err := e.Admin(tok.Token).WhoAmI(ctx, &sgitv1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked token: want Unauthenticated, got %v", err)
	}
	if _, err := adm.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: tok.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("revoke twice: want NotFound, got %v", err)
	}
}

func TestUnauthenticatedRejectedBeforeBody(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	ctx := context.Background()
	if _, err := e.Repo("").ResolveRev(ctx, &sgitv1.ResolveRevRequest{Repo: "web"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unary: want Unauthenticated, got %v", err)
	}
	if _, err := rpc.UploadBlobs(ctx, e.Repo(""), "web", [][]byte{[]byte("x")}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream: want Unauthenticated, got %v", err)
	}
}
