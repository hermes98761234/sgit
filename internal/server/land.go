package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (r *repoService) Land(ctx context.Context, req *sgitv1.LandRequest) (*sgitv1.LandResponse, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	d, err := r.loadOwnedOpenDraft(ctx, h, u, req.Id)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	commit, parent, err := r.land(ctx, h, d.ID)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.LandResponse{Commit: commit, DraftId: d.ID, Parent: parent}, nil
}

func (r *repoService) CommitAndLand(ctx context.Context, req *sgitv1.CommitAndLandRequest) (*sgitv1.LandResponse, error) {
	h, u, err := r.s.open(ctx, req.Repo, auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	var d store.Draft
	if req.DraftId == "" {
		d, err = r.createDraft(ctx, h, u, req.Base, req.Changes, req.Title, req.Message)
	} else {
		d, err = r.updateDraft(ctx, h, u, req.DraftId, req.Base, req.Changes, req.Message)
	}
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	commit, parent, err := r.land(ctx, h, d.ID)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.LandResponse{Commit: commit, DraftId: d.ID, Parent: parent}, nil
}

// land rebases the draft onto main as a single linear commit and moves main
// with compare-and-swap, retrying if main moved underneath. It returns the
// new commit and its parent (the main it was placed on).
func (r *repoService) land(ctx context.Context, h *repoHandle, id string) (string, string, error) {
	h.landMu.Lock()
	defer h.landMu.Unlock()

	// Re-read under the lock: a concurrent Land of the same draft may have won.
	d, err := r.s.store.GetDraft(ctx, h.name, id)
	if err != nil {
		return "", "", err
	}
	if d.Status != store.DraftOpen {
		return "", "", status.Errorf(codes.FailedPrecondition, "draft %s is %s", id, d.Status)
	}
	if cur, err := h.git.ReadRef(ctx, headRef(d.ID)); err != nil || cur != d.Head {
		return "", "", status.Errorf(codes.Aborted, "draft %s is inconsistent (head ref moved); update it and retry", id)
	}
	dc, err := h.git.ReadCommit(ctx, d.Head)
	if err != nil {
		return "", "", err
	}
	for attempt := 0; attempt < r.s.cfg.LandRetries; attempt++ {
		mainOld, err := h.git.ReadRef(ctx, gitx.MainRef)
		if err != nil {
			return "", "", err
		}
		tree := dc.Tree
		if mainOld != d.Base {
			mr, err := h.git.MergeTree(ctx, d.Base, mainOld, d.Head)
			if err != nil {
				return "", "", err
			}
			if !mr.Clean {
				return "", "", conflictStatus(d.ID, mainOld, mr.Conflicts)
			}
			tree = mr.Tree
		}
		mc, err := h.git.ReadCommit(ctx, mainOld)
		if err != nil {
			return "", "", err
		}
		if tree == mc.Tree {
			return "", "", status.Errorf(codes.FailedPrecondition, "draft %s changes are already on main", id)
		}
		committer := dc.Committer
		committer.When = r.s.cfg.Now()
		newC, err := h.git.CommitTree(ctx, tree, []string{mainOld}, dc.Message, dc.Author, committer)
		if err != nil {
			return "", "", err
		}
		// Once main moves, the draft bookkeeping must not be cancelled by the
		// client; the sequence row is recorded under the same context.
		bg := context.WithoutCancel(ctx)
		// Recorded before main moves, so a catch-up never sees this commit
		// on main without its sequence number. A row for a commit that loses
		// the CAS is never looked up.
		if err := r.s.store.RecordLand(bg, h.name, newC, r.s.cfg.Now()); err != nil {
			r.s.log.ErrorContext(bg, "recording land sequence", "repo", h.name, "commit", newC, "err", err)
			return "", "", status.Error(codes.Internal, "recording the land sequence failed")
		}
		if r.s.cfg.BeforeCAS != nil {
			r.s.cfg.BeforeCAS(h.name)
		}
		err = h.git.UpdateRef(bg, gitx.MainRef, newC, mainOld)
		if errors.Is(err, gitx.ErrRefMoved) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		d.Status, d.LandedAs, d.UpdatedAt = store.DraftLanded, newC, r.s.cfg.Now()
		if err := r.s.store.UpdateDraft(bg, d); err != nil {
			// main already moved; report success but log the bookkeeping failure.
			r.s.log.ErrorContext(bg, "landed but failed to mark draft", "draft", d.ID, "commit", newC, "err", err)
		}
		r.s.mirror.Notify(h.name)
		return newC, mainOld, nil
	}
	return "", "", status.Errorf(codes.ResourceExhausted, "main kept moving; gave up after %d attempts", r.s.cfg.LandRetries)
}

func conflictStatus(draftID, main string, files []gitx.ConflictFile) error {
	st := status.New(codes.Aborted, fmt.Sprintf("conflict in %d file(s)", len(files)))
	det := &sgitv1.ConflictDetail{DraftId: draftID, Main: main}
	for _, f := range files {
		det.Files = append(det.Files, &sgitv1.ConflictFile{
			Path: strings.ToValidUTF8(f.Path, "�"), BaseSha: f.Base, OursSha: f.Ours, TheirsSha: f.Theirs,
		})
	}
	withDet, err := st.WithDetails(det)
	if err != nil {
		return st.Err()
	}
	return withDet.Err()
}
