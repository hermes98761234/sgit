package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var draftIDRe = regexp.MustCompile(`^[a-z2-7]{8}$`)

func headRef(id string) string           { return "refs/sg/drafts/" + id + "/head" }
func versionRef(id string, v int) string { return fmt.Sprintf("refs/sg/drafts/%s/v%d", id, v) }

func newDraftID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l)
}

func draftPB(d store.Draft, project string) *sgitv1.Draft {
	return &sgitv1.Draft{
		Id: d.ID, Project: project, Owner: d.OwnerName, Title: d.Title, Base: d.Base, Head: d.Head,
		Version: int32(d.Version), Status: d.Status, LandedAs: d.LandedAs,
		CreatedAt: d.CreatedAt.Unix(), UpdatedAt: d.UpdatedAt.Unix(),
	}
}

// buildDraftCommit creates (without any ref) a commit = base + changes. The
// base must be a project commit and every path must be writable (§6.1).
func (r *projectService) buildDraftCommit(ctx context.Context, pr *proj, u store.User, base string, changes []*sgitv1.Change, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", status.Error(codes.InvalidArgument, "commit message required")
	}
	if !gitx.IsSHA(base) {
		return "", status.Errorf(codes.InvalidArgument, "invalid base %q", base)
	}
	if ok, err := r.s.store.IsProjectCommit(ctx, pr.p.ID, base); err != nil {
		return "", r.s.toStatus(ctx, err)
	} else if !ok {
		return "", status.Errorf(codes.InvalidArgument, "base commit %s not found", base)
	}
	bc, err := pr.h.Git.ReadCommit(ctx, base)
	if errors.Is(err, gitx.ErrNotFound) || errors.Is(err, gitx.ErrInvalid) {
		return "", status.Errorf(codes.InvalidArgument, "base commit %s not found", base)
	}
	if err != nil {
		return "", err
	}
	gc, err := r.s.convertChanges(ctx, pr.h.Git, changes)
	if err != nil {
		return "", err
	}
	paths := make([]string, len(gc))
	for i, c := range gc {
		paths[i] = c.Path
	}
	if err := checkPaths(pr.mounts, paths); err != nil {
		return "", err
	}
	tree, err := pr.h.Git.BuildTree(ctx, base, gc)
	if err != nil {
		return "", err
	}
	if tree == bc.Tree {
		return "", status.Error(codes.InvalidArgument, "changes produce no difference from base")
	}
	sig := r.s.sig(u)
	return pr.h.Git.CommitTree(ctx, tree, []string{base}, message, sig, sig)
}

func (r *projectService) createDraft(ctx context.Context, pr *proj, u store.User, base string, changes []*sgitv1.Change, title, message string) (store.Draft, error) {
	c, err := r.buildDraftCommit(ctx, pr, u, base, changes, message)
	if err != nil {
		return store.Draft{}, err
	}
	id, err := newDraftID()
	if err != nil {
		return store.Draft{}, err
	}
	// From the first ref write on, a cancelled client must not leave a partial draft.
	bg := context.WithoutCancel(ctx)
	if err := pr.h.Git.UpdateRef(bg, headRef(id), c, ""); err != nil {
		return store.Draft{}, err
	}
	if err := pr.h.Git.UpdateRef(bg, versionRef(id, 1), c, ""); err != nil {
		return store.Draft{}, err
	}
	if title == "" {
		title = firstLine(message)
	}
	now := r.s.cfg.Now()
	d := store.Draft{ID: id, ProjectID: pr.p.ID, OwnerID: u.ID, OwnerName: u.Name, Title: title, Base: base, Head: c,
		Version: 1, Status: store.DraftOpen, CreatedAt: now, UpdatedAt: now}
	// The insert is refused if the project was deleted meanwhile.
	if err := r.s.store.InsertDraft(bg, d); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return d, auth.ProjectNotFound(pr.ref)
		}
		return d, err
	}
	return d, nil
}

func (r *projectService) loadOwnedOpenDraft(ctx context.Context, pr *proj, u store.User, id string) (store.Draft, error) {
	if !draftIDRe.MatchString(id) {
		return store.Draft{}, status.Errorf(codes.InvalidArgument, "invalid draft id %q", id)
	}
	d, err := r.s.store.GetDraft(ctx, pr.p.ID, id)
	if err != nil {
		return d, err
	}
	if d.OwnerID != u.ID && !u.Admin {
		return d, status.Errorf(codes.PermissionDenied, "draft %s belongs to %s", id, d.OwnerName)
	}
	if d.Status != store.DraftOpen {
		return d, status.Errorf(codes.FailedPrecondition, "draft %s is %s", id, d.Status)
	}
	return d, nil
}

func (r *projectService) updateDraft(ctx context.Context, pr *proj, u store.User, id, base string, changes []*sgitv1.Change, message string) (store.Draft, error) {
	unlock, err := r.s.lock(ctx, pr)
	if err != nil {
		return store.Draft{}, err
	}
	defer unlock()
	d, err := r.loadOwnedOpenDraft(ctx, pr, u, id)
	if err != nil {
		return d, err
	}
	c, err := r.buildDraftCommit(ctx, pr, u, base, changes, message)
	if err != nil {
		return d, err
	}
	if err := pr.h.Git.UpdateRef(ctx, headRef(id), c, d.Head); err != nil {
		if errors.Is(err, gitx.ErrRefMoved) {
			return d, status.Errorf(codes.Aborted, "draft %s was updated concurrently", id)
		}
		return d, err
	}
	// The head ref moved; finish the bookkeeping even if the client went away.
	bg := context.WithoutCancel(ctx)
	d.Version++
	if err := pr.h.Git.UpdateRef(bg, versionRef(id, d.Version), c, ""); err != nil {
		return d, err
	}
	d.Base, d.Head, d.UpdatedAt = base, c, r.s.cfg.Now()
	return d, r.s.store.UpdateDraft(bg, d)
}

func (r *projectService) CreateDraft(ctx context.Context, req *sgitv1.CreateDraftRequest) (*sgitv1.Draft, error) {
	pr, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.createDraft(ctx, pr, u, req.Base, req.Changes, req.Title, req.Message)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d, pr.p.Name), nil
}

func (r *projectService) UpdateDraft(ctx context.Context, req *sgitv1.UpdateDraftRequest) (*sgitv1.Draft, error) {
	pr, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.updateDraft(ctx, pr, u, req.Id, req.Base, req.Changes, req.Message)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d, pr.p.Name), nil
}

func (r *projectService) GetDraft(ctx context.Context, req *sgitv1.GetDraftRequest) (*sgitv1.Draft, error) {
	pr, _, err := r.s.open(ctx, req.Project, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if !draftIDRe.MatchString(req.Id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid draft id %q", req.Id)
	}
	d, err := r.s.store.GetDraft(ctx, pr.p.ID, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d, pr.p.Name), nil
}

func (r *projectService) ListDrafts(ctx context.Context, req *sgitv1.ListDraftsRequest) (*sgitv1.ListDraftsResponse, error) {
	pr, u, err := r.s.open(ctx, req.Project, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	switch req.Status {
	case "", store.DraftOpen, store.DraftLanded, store.DraftAbandoned:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid status filter %q", req.Status)
	}
	var owner int64
	if req.Mine {
		owner = u.ID
	}
	ds, err := r.s.store.ListDrafts(ctx, pr.p.ID, owner, req.Status)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.ListDraftsResponse{}
	for _, d := range ds {
		resp.Drafts = append(resp.Drafts, draftPB(d, pr.p.Name))
	}
	return resp, nil
}

func (r *projectService) AbandonDraft(ctx context.Context, req *sgitv1.AbandonDraftRequest) (*sgitv1.Draft, error) {
	pr, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	unlock, err := r.s.lock(ctx, pr)
	if err != nil {
		return nil, err
	}
	defer unlock()
	d, err := r.loadOwnedOpenDraft(ctx, pr, u, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	d.Status, d.UpdatedAt = store.DraftAbandoned, r.s.cfg.Now()
	if err := r.s.store.UpdateDraft(ctx, d); err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d, pr.p.Name), nil
}
