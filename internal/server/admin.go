package server

import (
	"context"
	"errors"
	"regexp"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var userNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type adminService struct {
	sgitv1.UnimplementedAdminServer
	s *Server
}

func (a *adminService) requireAdmin(ctx context.Context) (store.User, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return u, status.Error(codes.Unauthenticated, "no user")
	}
	return u, auth.Authorize(ctx, a.s.store, u, "", auth.ActionAdmin)
}

func (a *adminService) WhoAmI(ctx context.Context, _ *sgitv1.WhoAmIRequest) (*sgitv1.WhoAmIResponse, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no user")
	}
	return &sgitv1.WhoAmIResponse{User: u.Name, Email: u.Email, Admin: u.Admin}, nil
}

func (a *adminService) CreateRepo(ctx context.Context, req *sgitv1.CreateRepoRequest) (*sgitv1.CreateRepoResponse, error) {
	u, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	main, err := a.s.repos.create(ctx, req.Name, a.s.sig(u))
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.CreateRepoResponse{Main: main}, nil
}

func (a *adminService) CreateToken(ctx context.Context, req *sgitv1.CreateTokenRequest) (*sgitv1.CreateTokenResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if !userNameRe.MatchString(req.User) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user name %q", req.User)
	}
	u, err := a.s.store.UserByName(ctx, req.User)
	if errors.Is(err, store.ErrNotFound) {
		email := req.Email
		if email == "" {
			email = req.User + "@sgit.local"
		}
		// --admin only applies when the user is created.
		u, err = a.s.store.CreateUser(ctx, req.User, email, req.Admin)
	}
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	id, tok, hash, err := auth.NewToken()
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if err := a.s.store.CreateToken(ctx, u.ID, id, hash); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.CreateTokenResponse{Id: id, Token: tok, User: u.Name}, nil
}

func (a *adminService) RevokeToken(ctx context.Context, req *sgitv1.RevokeTokenRequest) (*sgitv1.RevokeTokenResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := a.s.store.RevokeToken(ctx, req.Id); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.RevokeTokenResponse{}, nil
}

func (a *adminService) setACL(ctx context.Context, user, repo string, lvl store.Level) error {
	if _, err := a.requireAdmin(ctx); err != nil {
		return err
	}
	u, err := a.s.store.UserByName(ctx, user)
	if err != nil {
		return a.s.toStatus(ctx, err)
	}
	if _, err := a.s.store.GetRepo(ctx, repo); err != nil {
		return a.s.toStatus(ctx, err)
	}
	return a.s.toStatus(ctx, a.s.store.SetACL(ctx, u.ID, repo, lvl))
}

func (a *adminService) GrantACL(ctx context.Context, req *sgitv1.GrantACLRequest) (*sgitv1.GrantACLResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	var lvl store.Level
	switch req.Level {
	case "read":
		lvl = store.LevelRead
	case "write":
		lvl = store.LevelWrite
	default:
		return nil, status.Errorf(codes.InvalidArgument, "level must be read or write, got %q", req.Level)
	}
	if err := a.setACL(ctx, req.User, req.Repo, lvl); err != nil {
		return nil, err
	}
	return &sgitv1.GrantACLResponse{}, nil
}

func (a *adminService) RevokeACL(ctx context.Context, req *sgitv1.RevokeACLRequest) (*sgitv1.RevokeACLResponse, error) {
	if err := a.setACL(ctx, req.User, req.Repo, store.LevelNone); err != nil {
		return nil, err
	}
	return &sgitv1.RevokeACLResponse{}, nil
}
