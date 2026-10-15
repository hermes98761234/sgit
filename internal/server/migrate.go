package server

import (
	"context"
	"errors"
	"strconv"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MigrateIn brings a git repo into sgit and mounts it in a project
// (docs/superpowers/specs/2026-10-15-sgit-migrate-design.md §3).
func (a *adminService) MigrateIn(ctx context.Context, req *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := checkMigrateIn(req); err != nil {
		return nil, err
	}
	if err := a.checkMigrateProject(ctx, req); err != nil {
		return nil, err
	}
	if req.IntoRepo != "" {
		return a.migrateInto(ctx, req)
	}
	main, err := a.s.repos.importRepo(ctx, req.Name, req.FromUrl, req.MirrorUrl)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	dir := a.s.repos.path(req.Name)
	g := gitx.Open(dir)
	fps, err := g.FirstParents(ctx, main, "")
	g.Close()
	if err != nil {
		a.s.repos.rollbackNew(req.Name, dir)
		return nil, a.s.toStatus(ctx, err)
	}
	resp := &sgitv1.MigrateInResponse{Repo: req.Name, Folder: ".", Main: main, Commits: int32(len(fps))}
	if err := a.migrateMount(ctx, req, resp, req.Name, "."); err != nil {
		a.s.repos.rollbackNew(req.Name, dir)
		return nil, err
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return resp, nil
}

// checkMigrateIn validates the request shape before anything is fetched.
func checkMigrateIn(req *sgitv1.MigrateInRequest) error {
	if req.FromUrl == "" {
		return status.Error(codes.InvalidArgument, "migrate in needs a source URL")
	}
	if err := validProjectName("name", req.Name); err != nil {
		return err
	}
	if (req.Project == "") != (req.Path == "") {
		return status.Error(codes.InvalidArgument, "--project and --path go together")
	}
	if req.Path != "" {
		if err := validMountPath(req.Path); err != nil {
			return err
		}
	}
	if req.IntoRepo == "" {
		if req.IntoFolder != "" {
			return status.Error(codes.InvalidArgument, "a folder needs --into <repo>:<folder>")
		}
		return nil
	}
	if req.MirrorUrl != "" {
		return status.Error(codes.InvalidArgument, "--mirror cannot be used with --into; set the mirror on the target repo")
	}
	if req.IntoFolder == "" || req.IntoFolder == "." {
		return status.Error(codes.InvalidArgument, "--into needs a folder: <repo>:<folder>")
	}
	return validatePath(req.IntoFolder)
}

// checkMigrateProject fails early, before any import, when the project step
// cannot succeed: --project must exist, and the default project name must be free.
func (a *adminService) checkMigrateProject(ctx context.Context, req *sgitv1.MigrateInRequest) error {
	if req.Project != "" {
		_, err := a.resolveProject(ctx, req.Project)
		return err
	}
	_, err := a.s.store.ResolveProject(ctx, req.Name)
	switch {
	case err == nil:
		return status.Errorf(codes.AlreadyExists, "project %q exists; pass another name or use --project", req.Name)
	case errors.Is(err, store.ErrNotFound):
		return nil
	default:
		return a.s.toStatus(ctx, err)
	}
}

// migrateMount mounts repo:folder in a new project req.Name at ".", or in
// req.Project at req.Path, and fills the project fields of resp. A project
// it created is deleted again when the mount fails.
func (a *adminService) migrateMount(ctx context.Context, req *sgitv1.MigrateInRequest, resp *sgitv1.MigrateInResponse, repo, folder string) error {
	project, path, created := req.Project, req.Path, false
	if project == "" {
		info, err := a.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: req.Name})
		if err != nil {
			return err
		}
		project, path, created = "#"+strconv.FormatInt(info.Id, 10), ".", true
	}
	info, err := a.MountProject(ctx, &sgitv1.MountProjectRequest{Project: project, Path: path, Repo: repo, Folder: folder})
	if err != nil {
		if created {
			if _, derr := a.DeleteProject(context.WithoutCancel(ctx), &sgitv1.DeleteProjectRequest{Project: project}); derr != nil {
				a.s.log.ErrorContext(ctx, "migrate: removing the new project failed", "project", project, "err", derr)
			}
		}
		return err
	}
	resp.Project, resp.Path, resp.ProjectHead = info.Name, path, info.Head
	return nil
}

func (a *adminService) migrateInto(ctx context.Context, req *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error) {
	return nil, status.Error(codes.Unimplemented, "--into is not implemented yet")
}
