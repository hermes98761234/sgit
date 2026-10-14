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

func draftPB(d store.Draft) *sgitv1.Draft {
	return &sgitv1.Draft{
		Id: d.ID, Project: d.Repo, Owner: d.OwnerName, Title: d.Title, Base: d.Base, Head: d.Head,
		Version: int32(d.Version), Status: d.Status, LandedAs: d.LandedAs,
		CreatedAt: d.CreatedAt.Unix(), UpdatedAt: d.UpdatedAt.Unix(),
	}
}

// buildDraftCommit creates (without any ref) a commit = base + changes.
func (r *repoService) buildDraftCommit(ctx context.Context, h *repoHandle, u store.User, base string, changes []*sgitv1.Change, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", status.Error(codes.InvalidArgument, "commit message required")
	}
	if !gitx.IsSHA(base) {
		return "", status.Errorf(codes.InvalidArgument, "invalid base %q", base)
	}
	bc, err := h.git.ReadCommit(ctx, base)
	if errors.Is(err, gitx.ErrNotFound) || errors.Is(err, gitx.ErrInvalid) {
		return "", status.Errorf(codes.InvalidArgument, "base commit %s not found", base)
	}
	if err != nil {
		return "", err
	}
	gc, err := r.s.convertChanges(ctx, h, changes)
	if err != nil {
		return "", err
	}
	tree, err := h.git.BuildTree(ctx, base, gc)
	if err != nil {
		return "", err
	}
	if tree == bc.Tree {
		return "", status.Error(codes.InvalidArgument, "changes produce no difference from base")
	}
	sig := r.s.sig(u)
	return h.git.CommitTree(ctx, tree, []string{base}, message, sig, sig)
}

func (r *repoService) createDraft(ctx context.Context, h *repoHandle, u store.User, base string, changes []*sgitv1.Change, title, message string) (store.Draft, error) {
	c, err := r.buildDraftCommit(ctx, h, u, base, changes, message)
	if err != nil {
		return store.Draft{}, err
	}
	id, err := newDraftID()
	if err != nil {
		return store.Draft{}, err
	}
	// From the first ref write on, a cancelled client must not leave a partial draft.
	bg := context.WithoutCancel(ctx)
	if err := h.git.UpdateRef(bg, headRef(id), c, ""); err != nil {
		return store.Draft{}, err
	}
	if err := h.git.UpdateRef(bg, versionRef(id, 1), c, ""); err != nil {
		return store.Draft{}, err
	}
	if title == "" {
		title = firstLine(message)
	}
	now := r.s.cfg.Now()
	d := store.Draft{ID: id, Repo: h.name, OwnerID: u.ID, OwnerName: u.Name, Title: title, Base: base, Head: c,
		Version: 1, Status: store.DraftOpen, CreatedAt: now, UpdatedAt: now}
	return d, r.s.store.InsertDraft(bg, d)
}

func (r *repoService) loadOwnedOpenDraft(ctx context.Context, h *repoHandle, u store.User, id string) (store.Draft, error) {
	if !draftIDRe.MatchString(id) {
		return store.Draft{}, status.Errorf(codes.InvalidArgument, "invalid draft id %q", id)
	}
	d, err := r.s.store.GetDraft(ctx, h.name, id)
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

func (r *repoService) updateDraft(ctx context.Context, h *repoHandle, u store.User, id, base string, changes []*sgitv1.Change, message string) (store.Draft, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()
	d, err := r.loadOwnedOpenDraft(ctx, h, u, id)
	if err != nil {
		return d, err
	}
	c, err := r.buildDraftCommit(ctx, h, u, base, changes, message)
	if err != nil {
		return d, err
	}
	if err := h.git.UpdateRef(ctx, headRef(id), c, d.Head); err != nil {
		if errors.Is(err, gitx.ErrRefMoved) {
			return d, status.Errorf(codes.Aborted, "draft %s was updated concurrently", id)
		}
		return d, err
	}
	// The head ref moved; finish the bookkeeping even if the client went away.
	bg := context.WithoutCancel(ctx)
	d.Version++
	if err := h.git.UpdateRef(bg, versionRef(id, d.Version), c, ""); err != nil {
		return d, err
	}
	d.Base, d.Head, d.UpdatedAt = base, c, r.s.cfg.Now()
	return d, r.s.store.UpdateDraft(bg, d)
}

func (r *repoService) CreateDraft(ctx context.Context, req *sgitv1.CreateDraftRequest) (*sgitv1.Draft, error) {
	h, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.createDraft(ctx, h, u, req.Base, req.Changes, req.Title, req.Message)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}

func (r *repoService) UpdateDraft(ctx context.Context, req *sgitv1.UpdateDraftRequest) (*sgitv1.Draft, error) {
	h, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.updateDraft(ctx, h, u, req.Id, req.Base, req.Changes, req.Message)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}

func (r *repoService) GetDraft(ctx context.Context, req *sgitv1.GetDraftRequest) (*sgitv1.Draft, error) {
	h, _, err := r.s.open(ctx, req.Project, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if !draftIDRe.MatchString(req.Id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid draft id %q", req.Id)
	}
	d, err := r.s.store.GetDraft(ctx, h.name, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}

func (r *repoService) ListDrafts(ctx context.Context, req *sgitv1.ListDraftsRequest) (*sgitv1.ListDraftsResponse, error) {
	h, u, err := r.s.open(ctx, req.Project, auth.ActionRead)
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
	ds, err := r.s.store.ListDrafts(ctx, h.name, owner, req.Status)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.ListDraftsResponse{}
	for _, d := range ds {
		resp.Drafts = append(resp.Drafts, draftPB(d))
	}
	return resp, nil
}

func (r *repoService) AbandonDraft(ctx context.Context, req *sgitv1.AbandonDraftRequest) (*sgitv1.Draft, error) {
	h, u, err := r.s.open(ctx, req.Project, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	h.landMu.Lock()
	defer h.landMu.Unlock()
	d, err := r.loadOwnedOpenDraft(ctx, h, u, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	d.Status, d.UpdatedAt = store.DraftAbandoned, r.s.cfg.Now()
	if err := r.s.store.UpdateDraft(ctx, d); err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return draftPB(d), nil
}
