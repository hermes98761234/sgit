package server

import (
	"context"
	"errors"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/project"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// resolveProject finds a project by name, alias or "#<id>" in the store. It
// never opens (or rebuilds) the project repo, so a project whose repo cannot
// be rebuilt can still be renamed, inspected or deleted.
func (a *adminService) resolveProject(ctx context.Context, ref string) (store.Project, error) {
	p, err := a.s.store.ResolveProject(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return p, auth.ProjectNotFound(ref)
	}
	if err != nil {
		return p, a.s.toStatus(ctx, err)
	}
	return p, nil
}

// lockProject resolves ref, opens its repo and takes its land lock, so a
// mount change never runs in the middle of a land (which checks the mounts
// once, under the same lock). The caller must call the returned unlock.
func (a *adminService) lockProject(ctx context.Context, ref string) (store.Project, *project.Handle, func(), error) {
	p, err := a.resolveProject(ctx, ref)
	if err != nil {
		return p, nil, nil, err
	}
	h, err := a.s.projects.Get(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return p, nil, nil, auth.ProjectNotFound(ref)
	}
	if err != nil {
		return p, nil, nil, a.s.toStatus(ctx, err)
	}
	unlock, err := a.s.lock(ctx, &proj{h: h, p: p, ref: ref})
	if err != nil {
		return p, nil, nil, err
	}
	return p, h, unlock, nil
}

func (a *adminService) projectInfo(ctx context.Context, p store.Project) (*sgitv1.ProjectInfo, error) {
	p, err := a.s.store.GetProject(ctx, p.ID) // fresh name and aliases
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	head, _, err := a.s.store.ProjectHead(ctx, p.ID)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	mounts, err := a.s.store.Mounts(ctx, p.ID)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	grants, err := a.s.store.ListACL(ctx, "", p.ID)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	info := &sgitv1.ProjectInfo{Id: p.ID, Name: p.Name, Aliases: p.Aliases, Head: head, CreatedUnix: p.CreatedAt.Unix()}
	for _, m := range mounts {
		info.Mounts = append(info.Mounts, &sgitv1.MountInfo{Path: m.Path, Repo: m.Repo, Folder: m.Folder, ReadOnly: m.ReadOnly, AddedUnix: m.AddedAt.Unix()})
	}
	for _, g := range grants {
		lvl := "read"
		if g.Level == store.LevelWrite {
			lvl = "write"
		}
		info.Grants = append(info.Grants, &sgitv1.GrantInfo{User: g.User, Level: lvl})
	}
	return info, nil
}

// validMountPath accepts a clean relative path, or "." for the repo root
// (Folder) or the project root (Path). The engine rejects overlapping mount
// paths, which also makes a "." Path the sole mount of its project.
func validMountPath(p string) error {
	if p == "." {
		return nil
	}
	return validatePath(p)
}

func validProjectName(kind, name string) error {
	if !ValidRepoName(name) {
		return status.Errorf(codes.InvalidArgument, "invalid %s %q", kind, name)
	}
	return nil
}

func (a *adminService) CreateProject(ctx context.Context, req *sgitv1.CreateProjectRequest) (*sgitv1.ProjectInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := validProjectName("project name", req.Name); err != nil {
		return nil, err
	}
	p, err := a.s.store.CreateProject(ctx, req.Name, a.s.cfg.Now())
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if _, err := a.s.projects.Get(ctx, p.ID); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) MountProject(ctx context.Context, req *sgitv1.MountProjectRequest) (*sgitv1.ProjectInfo, error) {
	u, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	if err := validMountPath(req.Path); err != nil {
		return nil, err
	}
	if err := validMountPath(req.Folder); err != nil {
		return nil, err
	}
	p, h, unlock, err := a.lockProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := a.s.store.GetRepo(ctx, req.Repo); err != nil {
		return nil, a.s.toStatus(ctx, notFound(err, "repo %q not found", req.Repo))
	}
	m := store.Mount{Path: req.Path, Repo: req.Repo, Folder: req.Folder, ReadOnly: req.ReadOnly}
	if _, err := a.s.projects.Mount(ctx, h, m, a.s.sig(u)); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) UnmountProject(ctx context.Context, req *sgitv1.UnmountProjectRequest) (*sgitv1.ProjectInfo, error) {
	u, err := a.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	p, h, unlock, err := a.lockProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := a.s.projects.Unmount(ctx, h, req.Path, a.s.sig(u)); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) AddProjectAlias(ctx context.Context, req *sgitv1.AddProjectAliasRequest) (*sgitv1.ProjectInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := validProjectName("alias", req.Alias); err != nil {
		return nil, err
	}
	p, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	if err := a.s.store.AddProjectAlias(ctx, p.ID, req.Alias); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) RemoveProjectAlias(ctx context.Context, req *sgitv1.RemoveProjectAliasRequest) (*sgitv1.ProjectInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := a.s.store.RemoveProjectAlias(ctx, req.Alias)
	if err != nil {
		return nil, a.s.toStatus(ctx, notFound(err, "alias %q not found", req.Alias))
	}
	return a.projectInfo(ctx, store.Project{ID: id})
}

func (a *adminService) RenameProject(ctx context.Context, req *sgitv1.RenameProjectRequest) (*sgitv1.ProjectInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := validProjectName("project name", req.NewName); err != nil {
		return nil, err
	}
	p, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	if err := a.s.store.RenameProject(ctx, p.ID, req.NewName); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) ListProjects(ctx context.Context, _ *sgitv1.AdminListProjectsRequest) (*sgitv1.AdminListProjectsResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := a.s.store.ListProjects(ctx)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	out := &sgitv1.AdminListProjectsResponse{}
	for _, p := range rows {
		info, err := a.projectInfo(ctx, p)
		if err != nil {
			return nil, err
		}
		out.Projects = append(out.Projects, info)
	}
	return out, nil
}

func (a *adminService) GetProject(ctx context.Context, req *sgitv1.GetProjectRequest) (*sgitv1.ProjectInfo, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	return a.projectInfo(ctx, p)
}

func (a *adminService) DeleteProject(ctx context.Context, req *sgitv1.DeleteProjectRequest) (*sgitv1.DeleteProjectResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	// The engine deletes the rows while no open, draft change, land, mount or
	// catch-up of the project runs; each of those re-checks the project after.
	var drafts, acls int
	var rowsErr error
	err = a.s.projects.Delete(p.ID, func() error {
		drafts, acls, rowsErr = a.s.store.DeleteProjectRows(context.WithoutCancel(ctx), p.ID)
		return rowsErr
	})
	if errors.Is(rowsErr, store.ErrNotFound) { // deleted concurrently
		return nil, auth.ProjectNotFound(req.Project)
	}
	if rowsErr != nil {
		return nil, a.s.toStatus(ctx, rowsErr)
	}
	if err != nil {
		a.s.log.ErrorContext(ctx, "project rows deleted but repo not removed", "project", p.ID, "err", err)
	}
	return &sgitv1.DeleteProjectResponse{DraftsRemoved: int32(drafts), AclsRemoved: int32(acls)}, nil
}
