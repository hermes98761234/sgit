package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func cmdLand(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("land")
	project := fs.String("project", "", "project name, alias or #id (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("land [draft-id] [--project P] [--server S]")
	}
	s, err := a.connect(target{server: *server, project: *project, needProject: true})
	if err != nil {
		return err
	}
	defer s.Close()
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	} else if s.wc != nil {
		id = s.wc.Draft
	}
	if id == "" {
		return exitf(ExitError, "no draft to land (commit first, or pass a draft id)")
	}
	touch := s.ownsDraft(id)
	if touch {
		unlock, err := s.lock()
		if err != nil {
			return err
		}
		defer unlock()
		touch = s.ownsDraft(id)
	}
	resp, err := s.proj.Land(ctx, &sgitv1.LandRequest{Project: s.project, Id: id})
	return a.finishLand(ctx, s, resp, err, touch, nil)
}

// commitAndLand creates/updates the working copy's draft and lands it in one
// round trip when a draft exists. A stale .sg/draft is dropped and the call retried once.
func (a *app) commitAndLand(ctx context.Context, s *session, changes []*sgitv1.Change, title, msg string) error {
	w := s.wc
	if w.Draft != "" {
		req := &sgitv1.CommitAndLandRequest{Project: s.project, DraftId: w.Draft, Base: w.Base, Changes: changes, Title: title, Message: msg}
		resp, err := s.proj.CommitAndLand(ctx, req)
		if err == nil || !isStaleDraft(err) {
			return a.finishLand(ctx, s, resp, err, true, changes)
		}
		if isLandedDraft(err) {
			// It landed, but this working copy never heard (a lost answer,
			// or `sg land` elsewhere): catch up first, so only edits made
			// since are committed and no duplicate draft is opened.
			return a.landAfterLandedDraft(ctx, s, title, msg)
		}
		w.Draft = ""
	}
	// Create the draft first and keep its id, so a failed landing (for
	// example exhausted retries) leaves one draft that a retry reuses.
	d, err := saveDraft(ctx, s, changes, title, msg)
	if err != nil {
		return err
	}
	w.Draft = d.Id
	if err := w.Save(); err != nil {
		return err
	}
	resp, err := s.proj.Land(ctx, &sgitv1.LandRequest{Project: s.project, Id: d.Id})
	return a.finishLand(ctx, s, resp, err, true, changes)
}

// isLandedDraft reports whether err says the draft has already landed.
func isLandedDraft(err error) bool {
	return status.Code(err) == codes.FailedPrecondition && strings.HasSuffix(status.Convert(err).Message(), " is landed")
}

// isAlreadyOnMain reports whether err says every change of the draft is
// already on main (the server closed the draft as landed).
func isAlreadyOnMain(err error) bool {
	return status.Code(err) == codes.FailedPrecondition && strings.HasSuffix(status.Convert(err).Message(), " changes are already on main")
}

// landAfterLandedDraft syncs the working copy to main after its draft
// landed without it noticing, then commits and lands whatever is left.
func (a *app) landAfterLandedDraft(ctx context.Context, s *session, title, msg string) error {
	w := s.wc
	id := w.Draft
	w.Draft = ""
	res, main, err := syncToMain(ctx, s)
	if err != nil {
		return exitf(ExitError, "draft %s already landed; updating the working copy failed: %v; run `sg sync`", id, errorMessage(err))
	}
	if len(res.Conflicts) > 0 {
		return a.reportAlreadyOnMain(id, main, res)
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return a.reportAlreadyOnMain(id, main, res)
	}
	pb, err := uploadChanges(ctx, s, changes)
	if err != nil {
		return err
	}
	return a.commitAndLand(ctx, s, pb, title, msg)
}

// syncToMain syncs the working copy to the project's current main.
func syncToMain(ctx context.Context, s *session) (syncResult, string, error) {
	rr, err := s.proj.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: s.project, Rev: "main"})
	if err != nil {
		return syncResult{}, "", err
	}
	s.wc.Config.Name = rr.ProjectName
	res, err := syncTo(ctx, s, rr.Sha)
	return res, rr.Sha, err
}

// reportAlreadyOnMain tells the user that draft id had nothing left to land
// and that the working copy now matches main. When that sync conflicted with
// later local edits, nothing of this command landed: "landed" is false and
// the exit code is 3.
func (a *app) reportAlreadyOnMain(id, main string, res syncResult) error {
	conflicted := len(res.Conflicts) > 0
	if err := a.emit(map[string]any{"landed": !conflicted, "already_on_main": true, "commit": main, "draft": id, "sync": res}, func(out io.Writer) {
		synced := "working copy synced"
		if conflicted {
			synced = "syncing the working copy to it conflicted with your later edits"
		}
		fmt.Fprintf(out, "draft %s: these changes are already on main %s (an earlier land went through); %s\n", id, short(main), synced)
		printSync(out, res)
	}); err != nil {
		return err
	}
	if conflicted {
		return exitf(ExitConflict, "%d file(s) have conflicts with main; edit them, then commit", len(res.Conflicts))
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// finishLand updates the working copy after Land/CommitAndLand. touchWC is
// false when the landed draft does not belong to this working copy. sent is
// the change set this command committed against w.Base, or nil.
func (a *app) finishLand(ctx context.Context, s *session, resp *sgitv1.LandResponse, err error, touchWC bool, sent []*sgitv1.Change) error {
	w := s.wc
	if err != nil && isAlreadyOnMain(err) && w != nil && touchWC {
		// Nothing left to land: the server closed the draft. Catch up.
		id := w.Draft
		w.Draft = ""
		res, main, serr := syncToMain(ctx, s)
		if serr != nil {
			return exitf(ExitError, "%s; updating the working copy failed: %v; run `sg sync`", errorMessage(err), errorMessage(serr))
		}
		return a.reportAlreadyOnMain(id, main, res)
	}
	if err != nil {
		det := conflictDetail(err)
		if det != nil && det.Partial && (w == nil || !touchWC) {
			// No working copy to rebase, but some mounts did land.
			if eerr := a.emit(map[string]any{"landed": false, "partial": true, "landed_mounts": nonNil(det.Landed), "draft": det.DraftId, "main": det.Main}, func(out io.Writer) {
				fmt.Fprintf(out, "partially landed: %s/ landed on main %s; the rest stays in draft %s\n", strings.Join(det.Landed, "/, "), short(det.Main), det.DraftId)
			}); eerr != nil {
				return eerr
			}
			return exitf(ExitConflict, "%s", errorMessage(err))
		}
		if det == nil || w == nil || !touchWC {
			return err
		}
		w.Draft = det.DraftId
		if err := w.Save(); err != nil {
			return err
		}
		res, serr := syncTo(ctx, s, det.Main)
		if serr != nil {
			return exitf(ExitConflict, "landing conflicted on main %s (draft %s kept); could not rebase the working copy: %v", short(det.Main), det.DraftId, serr)
		}
		if eerr := a.emit(map[string]any{"landed": false, "partial": det.Partial, "landed_mounts": nonNil(det.Landed), "draft": det.DraftId, "main": det.Main, "sync": res}, func(out io.Writer) {
			if det.Partial {
				fmt.Fprintf(out, "partially landed: %s/ landed; the rest stays in draft %s\n", strings.Join(det.Landed, "/, "), det.DraftId)
			}
			if len(det.Files) > 0 || !det.Partial {
				fmt.Fprintf(out, "conflict: draft %s does not apply cleanly to main %s; working copy rebased onto it\n", det.DraftId, short(det.Main))
			} else {
				fmt.Fprintf(out, "working copy rebased onto main %s\n", short(det.Main))
			}
			printSync(out, res)
		}); eerr != nil {
			return eerr
		}
		if det.Partial && len(det.Files) == 0 { // the rest failed for another reason
			return exitf(ExitConflict, "%s; run `sg land` again", errorMessage(err))
		}
		return exitf(ExitConflict, "landing conflicted: fix the conflicted files, then run `sg commit -m MSG --land` again")
	}
	res := newSyncResult("", resp.Commit)
	if w != nil && touchWC && sent != nil && resp.Parent != "" && resp.Parent == w.Base {
		// Landed directly on our base: the new main is exactly w.Base plus
		// the changes just sent, which are already on disk. Record them
		// instead of walking and fetching the whole tree.
		res = newSyncResult(w.Base, resp.Commit)
		w.Draft = ""
		for _, c := range sent {
			if c.Op == sgitv1.ChangeOp_CHANGE_OP_DELETE {
				delete(w.Index, c.Path)
				continue
			}
			// Size -1: the next status re-hashes the file, so an edit made
			// after it was uploaded is never mistaken for the landed one.
			w.Index[c.Path] = wc.Entry{Path: c.Path, Mode: c.Mode, SHA: c.BlobSha, Size: -1}
		}
		w.Base = resp.Commit
		if err := w.Save(); err != nil {
			return err
		}
	} else if w != nil && touchWC {
		w.Draft = ""
		if res, err = syncTo(ctx, s, resp.Commit); err != nil {
			// The in-memory w may hold a half-applied sync; never save it.
			// Clear the draft on top of what is on disk instead.
			disk, oerr := wc.Open(w.Root)
			if oerr != nil {
				return oerr
			}
			disk.Draft = ""
			if serr := disk.Save(); serr != nil {
				return serr
			}
			msg := errorMessage(err)
			if eerr := a.emit(map[string]any{"landed": true, "commit": resp.Commit, "draft": resp.DraftId, "sync_error": msg}, func(out io.Writer) {
				fmt.Fprintf(out, "landed %s (draft %s); the working copy was not updated: %s\n", short(resp.Commit), resp.DraftId, msg)
			}); eerr != nil {
				return eerr
			}
			return exitf(ExitError, "landed %s (draft %s), but the working copy was not updated: %v; fix that, then run `sg sync`", short(resp.Commit), resp.DraftId, err)
		}
	}
	if err := a.emit(map[string]any{"landed": true, "commit": resp.Commit, "draft": resp.DraftId, "sync": res}, func(out io.Writer) {
		fmt.Fprintf(out, "landed %s (draft %s)\n", short(resp.Commit), resp.DraftId)
		printSync(out, res)
	}); err != nil {
		return err
	}
	if len(res.Conflicts) > 0 {
		return exitf(ExitConflict, "landed, but your uncommitted edits conflict with main in %d file(s)", len(res.Conflicts))
	}
	return nil
}
