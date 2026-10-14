package server

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
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
	main, err := a.s.repos.create(ctx, req.Name, req.MirrorUrl, a.s.sig(u))
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return &sgitv1.CreateRepoResponse{Main: main}, nil
}

func (a *adminService) ImportRepo(ctx context.Context, req *sgitv1.ImportRepoRequest) (*sgitv1.CreateRepoResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	main, err := a.s.repos.importRepo(ctx, req.Name, req.FromUrl, req.MirrorUrl)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return &sgitv1.CreateRepoResponse{Main: main}, nil
}

func (a *adminService) RepoStatus(ctx context.Context, req *sgitv1.RepoStatusRequest) (*sgitv1.RepoInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	row, err := a.s.store.GetRepo(ctx, req.Name)
	if err != nil {
		return nil, a.s.toStatus(ctx, notFound(err, "repo %q not found", req.Name))
	}
	info, err := a.repoInfo(ctx, row)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return info, nil
}

func (a *adminService) ListRepos(ctx context.Context, _ *sgitv1.ListReposRequest) (*sgitv1.ListReposResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := a.s.store.ListRepos(ctx)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	out := &sgitv1.ListReposResponse{}
	for _, row := range rows {
		info, err := a.repoInfo(ctx, row)
		if err != nil {
			if ctx.Err() != nil {
				return nil, a.s.toStatus(ctx, ctx.Err())
			}
			a.s.log.WarnContext(ctx, "list repos: cannot read repo", "repo", row.Name, "err", err)
			info = &sgitv1.RepoInfo{Name: row.Name, CreatedUnix: row.CreatedAt.Unix(), MirrorUrl: row.MirrorURL, Error: gitx.ErrSummary(err)}
		}
		out.Repos = append(out.Repos, info)
	}
	return out, nil
}

// repoInfo reports a repo's main and mirror health. Pending is computed from
// main and last_mirrored, so it is right even if the worker's view lags.
func (a *adminService) repoInfo(ctx context.Context, row store.RepoRow) (*sgitv1.RepoInfo, error) {
	h, err := a.s.repos.get(ctx, row.Name)
	if err != nil {
		return nil, err
	}
	main, err := h.git.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		return nil, err
	}
	info := &sgitv1.RepoInfo{Name: row.Name, Main: main, CreatedUnix: row.CreatedAt.Unix(), MirrorUrl: row.MirrorURL}
	if row.MirrorURL == "" {
		return info, nil
	}
	ms, err := a.s.store.MirrorState(ctx, row.Name)
	if err != nil {
		return nil, err
	}
	info.MirrorLast = ms.LastMirrored
	info.MirrorPending = ms.LastMirrored != main
	info.MirrorError = ms.LastError
	if !ms.LastAttempt.IsZero() {
		info.MirrorAttemptUnix = ms.LastAttempt.Unix()
	}
	return info, nil
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
		return nil, a.s.toStatus(ctx, notFound(err, "token %q not found (or already revoked)", req.Id))
	}
	return &sgitv1.RevokeTokenResponse{}, nil
}

func (a *adminService) setACL(ctx context.Context, user, repo string, lvl store.Level) error {
	if _, err := a.requireAdmin(ctx); err != nil {
		return err
	}
	u, err := a.s.store.UserByName(ctx, user)
	if err != nil {
		return a.s.toStatus(ctx, notFound(err, "user %q not found", user))
	}
	if _, err := a.s.store.GetRepo(ctx, repo); err != nil {
		return a.s.toStatus(ctx, notFound(err, "repo %q not found", repo))
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
	if err := a.setACL(ctx, req.User, req.Project, lvl); err != nil {
		return nil, err
	}
	return &sgitv1.GrantACLResponse{}, nil
}

func (a *adminService) RevokeACL(ctx context.Context, req *sgitv1.RevokeACLRequest) (*sgitv1.RevokeACLResponse, error) {
	if err := a.setACL(ctx, req.User, req.Project, store.LevelNone); err != nil {
		return nil, err
	}
	return &sgitv1.RevokeACLResponse{}, nil
}

func (a *adminService) ListUsers(ctx context.Context, _ *sgitv1.ListUsersRequest) (*sgitv1.ListUsersResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := a.s.store.ListUsers(ctx)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	out := &sgitv1.ListUsersResponse{}
	for _, r := range rows {
		out.Users = append(out.Users, &sgitv1.UserInfo{
			Name: r.Name, Email: r.Email, Admin: r.Admin,
			ActiveTokens: int32(r.ActiveTokens), CreatedUnix: r.CreatedAt.Unix(),
		})
	}
	return out, nil
}

func (a *adminService) SetUserAdmin(ctx context.Context, req *sgitv1.SetUserAdminRequest) (*sgitv1.SetUserAdminResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	err := a.s.store.SetAdmin(ctx, req.User, req.Admin)
	if errors.Is(err, store.ErrLastAdmin) {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot demote %q: they are the last admin; promote another user first", req.User)
	}
	if err != nil {
		return nil, a.s.toStatus(ctx, notFound(err, "user %q not found", req.User))
	}
	return &sgitv1.SetUserAdminResponse{}, nil
}

func (a *adminService) DeleteUser(ctx context.Context, req *sgitv1.DeleteUserRequest) (*sgitv1.DeleteUserResponse, error) {
	me, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	res, err := a.s.store.DeleteUser(ctx, req.User, me.ID)
	switch {
	case errors.Is(err, store.ErrSelf):
		return nil, status.Error(codes.FailedPrecondition, "cannot delete yourself; ask another admin")
	case errors.Is(err, store.ErrLastAdmin):
		return nil, status.Errorf(codes.FailedPrecondition, "cannot delete %q: they are the last admin; promote another user first", req.User)
	case err != nil:
		return nil, a.s.toStatus(ctx, notFound(err, "user %q not found", req.User))
	}
	return &sgitv1.DeleteUserResponse{
		TokensRevoked: int32(res.TokensRevoked), AclsRemoved: int32(res.ACLsRemoved), DraftsAbandoned: int32(res.DraftsAbandoned),
	}, nil
}

func (a *adminService) ListTokens(ctx context.Context, req *sgitv1.ListTokensRequest) (*sgitv1.ListTokensResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.User != "" {
		if _, err := a.s.store.UserByName(ctx, req.User); err != nil {
			return nil, a.s.toStatus(ctx, notFound(err, "user %q not found", req.User))
		}
	}
	rows, err := a.s.store.ListTokens(ctx, req.User)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	out := &sgitv1.ListTokensResponse{}
	for _, r := range rows {
		ti := &sgitv1.TokenInfo{Id: r.ID, User: r.User, CreatedUnix: r.CreatedAt.Unix()}
		if !r.RevokedAt.IsZero() {
			ti.RevokedUnix = r.RevokedAt.Unix()
		}
		out.Tokens = append(out.Tokens, ti)
	}
	return out, nil
}

func (a *adminService) ListACL(ctx context.Context, req *sgitv1.ListACLRequest) (*sgitv1.ListACLResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.User != "" {
		if _, err := a.s.store.UserByName(ctx, req.User); err != nil {
			return nil, a.s.toStatus(ctx, notFound(err, "user %q not found", req.User))
		}
	}
	if req.Project != "" {
		if _, err := a.s.store.GetRepo(ctx, req.Project); err != nil {
			return nil, a.s.toStatus(ctx, notFound(err, "repo %q not found", req.Project))
		}
	}
	rows, err := a.s.store.ListACL(ctx, req.User, req.Project)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	out := &sgitv1.ListACLResponse{}
	for _, r := range rows {
		lvl := "read"
		if r.Level == store.LevelWrite {
			lvl = "write"
		}
		out.Entries = append(out.Entries, &sgitv1.ACLEntry{User: r.User, Project: r.Repo, Level: lvl})
	}
	return out, nil
}

func (a *adminService) DeleteRepo(ctx context.Context, req *sgitv1.DeleteRepoRequest) (*sgitv1.DeleteRepoResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	// Message only: the store refuses inside the delete transaction. Checking
	// first also keeps the repo directory and handle untouched.
	if err := a.refuseInUse(ctx, req.Name); err != nil {
		return nil, err
	}
	dst, err := a.s.repos.deleteRepo(ctx, req.Name, filepath.Join(a.s.cfg.DataDir, "deleted"), a.s.cfg.Now())
	if errors.Is(err, store.ErrMounted) { // mounted since the check above
		if rerr := a.refuseInUse(ctx, req.Name); rerr != nil {
			return nil, rerr
		}
	}
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return &sgitv1.DeleteRepoResponse{MovedTo: dst}, nil
}

// refuseInUse returns FailedPrecondition naming the projects that mount repo
// or, failing that, have it in their history (a rebuild replays it); nil when
// no project uses it.
func (a *adminService) refuseInUse(ctx context.Context, repo string) error {
	names, err := a.s.store.ProjectsMounting(ctx, repo)
	if err != nil {
		return a.s.toStatus(ctx, err)
	}
	if len(names) > 0 {
		sort.Strings(names)
		return status.Errorf(codes.FailedPrecondition, "repo %q is mounted by project(s) %s; unmount it first", repo, strings.Join(names, ", "))
	}
	names, err = a.s.store.ProjectsUsing(ctx, repo)
	if err != nil {
		return a.s.toStatus(ctx, err)
	}
	if len(names) > 0 {
		return status.Errorf(codes.FailedPrecondition, "repo %q is in the history of project(s) %s; delete those projects first", repo, strings.Join(names, ", "))
	}
	return nil
}

// notFound replaces a store.ErrNotFound with a NotFound status that names
// what is missing; other errors pass through.
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, store.ErrNotFound) {
		return status.Errorf(codes.NotFound, format, args...)
	}
	return err
}
