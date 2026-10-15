package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
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

// migrateInto replays from_url's main under into_folder of into_repo
// (spec §3.2), then mounts that folder.
func (a *adminService) migrateInto(ctx context.Context, req *sgitv1.MigrateInRequest) (*sgitv1.MigrateInResponse, error) {
	h, err := a.s.repos.get(ctx, req.IntoRepo)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	allow := a.s.repos.allowLocal
	if err := gitx.ValidateRemote(req.FromUrl, allow); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	skipped, err := gitx.RemoteBranches(ctx, req.FromUrl, "release/", allow)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "import from", req.FromUrl, err)
	}
	ref, err := migrateRef()
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	defer func() {
		if err := h.git.DeleteRef(context.WithoutCancel(ctx), ref); err != nil {
			a.s.log.ErrorContext(ctx, "migrate: removing temporary ref failed", "repo", h.name, "ref", ref, "err", err)
		}
	}()
	tip, err := h.git.FetchMain(ctx, req.FromUrl, ref, allow)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "import from", req.FromUrl, err)
	}
	main, n, err := a.replayInto(ctx, h, tip, req.IntoFolder)
	if err != nil {
		return nil, err
	}
	resp := &sgitv1.MigrateInResponse{Repo: h.name, Folder: req.IntoFolder, Main: main, Commits: int32(n), SkippedBranches: skipped}
	if err := a.migrateMount(ctx, req, resp, h.name, req.IntoFolder); err != nil {
		st := status.Convert(err)
		return nil, status.Errorf(st.Code(), "%s (the code landed at %s:%s; mount it with sg project mount)", st.Message(), h.name, req.IntoFolder)
	}
	return resp, nil
}

// replayInto rewrites tip's history under folder on top of the repo's main
// and moves main, holding the repo's land lock throughout.
func (a *adminService) replayInto(ctx context.Context, h *repoHandle, tip, folder string) (string, int, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()
	base, err := h.git.ReadRef(ctx, gitx.MainRef)
	if err != nil {
		return "", 0, a.s.toStatus(ctx, err)
	}
	main, n, err := h.git.ReplayUnder(ctx, base, tip, folder)
	if errors.Is(err, gitx.ErrFolderNotEmpty) {
		return "", 0, status.Errorf(codes.FailedPrecondition, "folder %q is not empty in repo %q", folder, h.name)
	}
	if err != nil {
		return "", 0, a.s.toStatus(ctx, err)
	}
	// Once main moves the bookkeeping must finish; the sequence row goes
	// first, as in landSource.
	bg := context.WithoutCancel(ctx)
	if err := a.s.store.RecordLand(bg, h.name, main, a.s.cfg.Now()); err != nil {
		return "", 0, a.s.toStatus(ctx, fmt.Errorf("recording the land sequence: %w", err))
	}
	if err := h.git.UpdateRef(bg, gitx.MainRef, main, base); err != nil {
		if errors.Is(err, gitx.ErrRefMoved) {
			return "", 0, status.Errorf(codes.Aborted, "main of repo %q moved during the migration; retry", h.name)
		}
		return "", 0, a.s.toStatus(ctx, err)
	}
	a.s.mirror.Notify(h.name)
	return main, n, nil
}

// migrateRef returns a fresh temporary ref for fetched history.
func migrateRef() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "refs/sgit/migrate/" + hex.EncodeToString(b[:]), nil
}

// remoteErr maps a failed fetch, ls-remote or push to a client status that
// carries git's own one-line reason.
func (s *Server) remoteErr(ctx context.Context, what, url string, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	var ge *gitx.GitError
	if errors.As(err, &ge) && slices.ContainsFunc([]string{"fetch", "ls-remote", "push"}, func(c string) bool { return slices.Contains(ge.Args, c) }) {
		return status.Errorf(codes.FailedPrecondition, "%s %s failed: %s", what, url, gitx.ErrSummary(err))
	}
	return s.toStatus(ctx, err)
}

// MigrateOut pushes a project's main to a standalone git repo (spec §3.4).
// sgit itself is not changed.
func (a *adminService) MigrateOut(ctx context.Context, req *sgitv1.MigrateOutRequest) (*sgitv1.MigrateOutResponse, error) {
	if _, err := a.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.ToUrl == "" {
		return nil, status.Error(codes.InvalidArgument, "migrate out needs a target URL")
	}
	allow := a.s.repos.allowLocal
	if err := gitx.ValidateRemote(req.ToUrl, allow); err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	p, err := a.resolveProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	h, err := a.s.projects.Get(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, auth.ProjectNotFound(req.Project)
	}
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	head, err := a.s.projects.CatchUp(ctx, h)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	if head == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "project %q has no commits", p.Name)
	}
	remote, err := gitx.RemoteRef(ctx, req.ToUrl, gitx.MainRef, allow)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "reading", req.ToUrl, err)
	}
	resp := &sgitv1.MigrateOutResponse{Project: p.Name, Pushed: head}
	if remote == head {
		resp.UpToDate = true
		return resp, nil
	}
	if req.Force {
		err = h.Git.Push(ctx, req.ToUrl, head, remote, allow)
	} else {
		err = h.Git.PushFF(ctx, req.ToUrl, head, allow)
	}
	if errors.Is(err, gitx.ErrRejected) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"main of %s has diverged from project %q; use --force to overwrite it", req.ToUrl, p.Name)
	}
	if err != nil {
		return nil, a.s.remoteErr(ctx, "pushing to", req.ToUrl, err)
	}
	return resp, nil
}
