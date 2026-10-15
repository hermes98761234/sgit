package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/project"
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
	if err := a.checkMigrateIn(ctx, req); err != nil {
		return nil, err
	}
	t, err := a.migrateTarget(ctx, req)
	if err != nil {
		return nil, err
	}
	var resp *sgitv1.MigrateInResponse
	if req.IntoRepo != "" {
		resp, err = a.migrateInto(ctx, req, t)
	} else {
		resp, err = a.migratePlain(ctx, req, t)
	}
	if err != nil {
		a.dropTarget(ctx, t)
		return nil, err
	}
	return resp, nil
}

// migratePlain imports from_url as the new repo req.Name (main and
// release/*, SHAs kept) and mounts its root.
func (a *adminService) migratePlain(ctx context.Context, req *sgitv1.MigrateInRequest, t *migrateTarget) (*sgitv1.MigrateInResponse, error) {
	skipped, err := a.skippedBranches(ctx, req.FromUrl, true)
	if err != nil {
		return nil, err
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
		a.rollbackImport(ctx, req.Name, dir)
		return nil, a.s.toStatus(ctx, err)
	}
	resp := &sgitv1.MigrateInResponse{Repo: req.Name, Folder: ".", Main: main, Commits: int32(len(fps)), SkippedBranches: skipped}
	if err := a.migrateMount(ctx, t, resp, req.Name, "."); err != nil {
		a.dropTarget(ctx, t) // first, so its mount no longer keeps the repo
		if kept := a.rollbackImport(ctx, req.Name, dir); kept {
			st := status.Convert(err)
			err = status.Errorf(st.Code(), "%s (repo %q was kept because a project mount references it)", st.Message(), req.Name)
		}
		return nil, err
	}
	if req.MirrorUrl != "" {
		a.s.mirror.Notify(req.Name)
	}
	return resp, nil
}

// rollbackImport undoes the import of name and reports whether the repo had
// to be kept because the store still references it.
func (a *adminService) rollbackImport(ctx context.Context, name, dir string) (kept bool) {
	if err := a.s.repos.rollbackNew(name, dir); err != nil {
		a.s.log.ErrorContext(ctx, "migrate: rolling back the import failed; repo kept", "repo", name, "err", err)
		return true
	}
	return false
}

// checkMigrateIn validates the request shape before anything is fetched.
// The source URL comes first, so a URL with credentials is reported as such
// whatever the name.
func (a *adminService) checkMigrateIn(ctx context.Context, req *sgitv1.MigrateInRequest) error {
	if req.FromUrl == "" {
		return status.Error(codes.InvalidArgument, "migrate in needs a source URL")
	}
	if err := gitx.ValidateRemote(req.FromUrl, a.s.repos.allowLocal); err != nil {
		return a.s.toStatus(ctx, err)
	}
	// The name is the new repo's (plain form) or the new project's (no
	// --project); --into with --project does not use it.
	if req.IntoRepo == "" || req.Project == "" {
		if err := validProjectName("name", req.Name); err != nil {
			return err
		}
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

// migrateTarget is the project and path MigrateIn mounts the code at.
type migrateTarget struct {
	project string // "#<id>"
	path    string
	created bool // the project was created by this migration
}

// migrateTarget fails early, before anything is fetched, when the project
// step cannot succeed. --project must exist and have room for the mount
// (project.CheckMount; the mount checks again under the project lock).
// Without --project the new project req.Name is created now, empty, which
// reserves the name against a concurrent migration; dropTarget deletes it
// when a later step fails.
func (a *adminService) migrateTarget(ctx context.Context, req *sgitv1.MigrateInRequest) (*migrateTarget, error) {
	if req.Project != "" {
		p, err := a.resolveProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		mounts, err := a.s.store.Mounts(ctx, p.ID)
		if err != nil {
			return nil, a.s.toStatus(ctx, err)
		}
		m := store.Mount{Path: req.Path, Repo: req.Name, Folder: "."}
		if req.IntoRepo != "" {
			m.Repo, m.Folder = req.IntoRepo, req.IntoFolder
		}
		if _, err := project.CheckMount(mounts, m); err != nil {
			return nil, a.s.toStatus(ctx, err)
		}
		return &migrateTarget{project: "#" + strconv.FormatInt(p.ID, 10), path: req.Path}, nil
	}
	info, err := a.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: req.Name})
	if status.Code(err) == codes.AlreadyExists {
		return nil, status.Errorf(codes.AlreadyExists, "project %q exists; pass another name or use --project", req.Name)
	}
	if err != nil {
		return nil, err
	}
	return &migrateTarget{project: "#" + strconv.FormatInt(info.Id, 10), path: ".", created: true}, nil
}

// dropTarget deletes the project t when this migration created it.
func (a *adminService) dropTarget(ctx context.Context, t *migrateTarget) {
	if !t.created {
		return
	}
	t.created = false
	if _, err := a.DeleteProject(context.WithoutCancel(ctx), &sgitv1.DeleteProjectRequest{Project: t.project}); err != nil {
		a.s.log.ErrorContext(ctx, "migrate: removing the new project failed", "project", t.project, "err", err)
	}
}

// migrateMount mounts repo:folder at t and fills the project fields of resp.
func (a *adminService) migrateMount(ctx context.Context, t *migrateTarget, resp *sgitv1.MigrateInResponse, repo, folder string) error {
	info, err := a.MountProject(ctx, &sgitv1.MountProjectRequest{Project: t.project, Path: t.path, Repo: repo, Folder: folder})
	if err != nil {
		return err
	}
	resp.Project, resp.Path, resp.ProjectHead = info.Name, t.path, info.Head
	return nil
}

// skippedBranches lists the branches of url a migration does not bring: all
// but main, and release/* too unless keepRelease (the plain form).
func (a *adminService) skippedBranches(ctx context.Context, url string, keepRelease bool) ([]string, error) {
	all, err := gitx.RemoteBranches(ctx, url, "", a.s.repos.allowLocal)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "import from", url, err)
	}
	var skipped []string
	for _, b := range all {
		if b == "main" || keepRelease && strings.HasPrefix(b, "release/") {
			continue
		}
		skipped = append(skipped, b)
	}
	return skipped, nil
}

// migrateInto replays from_url's main under into_folder of into_repo
// (spec §3.2), then mounts that folder.
func (a *adminService) migrateInto(ctx context.Context, req *sgitv1.MigrateInRequest, t *migrateTarget) (*sgitv1.MigrateInResponse, error) {
	h, err := a.s.repos.get(ctx, req.IntoRepo)
	if err != nil {
		return nil, a.s.toStatus(ctx, err)
	}
	skipped, err := a.skippedBranches(ctx, req.FromUrl, false)
	if err != nil {
		return nil, err
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
	tip, err := h.git.FetchMain(ctx, req.FromUrl, ref, a.s.repos.allowLocal)
	if err != nil {
		return nil, a.s.remoteErr(ctx, "import from", req.FromUrl, err)
	}
	main, n, err := a.replayInto(ctx, h, tip, req.IntoFolder)
	if err != nil {
		return nil, err
	}
	resp := &sgitv1.MigrateInResponse{Repo: h.name, Folder: req.IntoFolder, Main: main, Commits: int32(n), SkippedBranches: skipped}
	if err := a.migrateMount(ctx, t, resp, h.name, req.IntoFolder); err != nil {
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
	var nf *gitx.NotAFolderError
	if errors.As(err, &nf) {
		what := "is a file"
		if nf.Path != folder {
			what = "is under a file"
		}
		return "", 0, status.Errorf(codes.FailedPrecondition, "%q %s in repo %q", folder, what, h.name)
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
		if req.Force {
			return nil, status.Errorf(codes.FailedPrecondition, "main of %s moved while pushing; retry", req.ToUrl)
		}
		return nil, status.Errorf(codes.FailedPrecondition,
			"main of %s has diverged from project %q; use --force to overwrite it", req.ToUrl, p.Name)
	}
	if err != nil {
		return nil, a.s.remoteErr(ctx, "pushing to", req.ToUrl, err)
	}
	return resp, nil
}
