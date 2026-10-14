package cli

import (
	"context"
	"fmt"
	"io"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/wc"
)

func cmdLand(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("land")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("land [draft-id] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
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
	resp, err := s.repo.Land(ctx, &sgitv1.LandRequest{Project: s.name, Id: id})
	return a.finishLand(ctx, s, resp, err, touch, nil)
}

// commitAndLand creates/updates the working copy's draft and lands it in one
// round trip when a draft exists. A stale .sg/draft is dropped and the call retried once.
func (a *app) commitAndLand(ctx context.Context, s *session, changes []*sgitv1.Change, title, msg string) error {
	w := s.wc
	if w.Draft != "" {
		req := &sgitv1.CommitAndLandRequest{Project: s.name, DraftId: w.Draft, Base: w.Base, Changes: changes, Title: title, Message: msg}
		resp, err := s.repo.CommitAndLand(ctx, req)
		if err == nil || !isStaleDraft(err) {
			return a.finishLand(ctx, s, resp, err, true, changes)
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
	resp, err := s.repo.Land(ctx, &sgitv1.LandRequest{Project: s.name, Id: d.Id})
	return a.finishLand(ctx, s, resp, err, true, changes)
}

// finishLand updates the working copy after Land/CommitAndLand. touchWC is
// false when the landed draft does not belong to this working copy. sent is
// the change set this command committed against w.Base, or nil.
func (a *app) finishLand(ctx context.Context, s *session, resp *sgitv1.LandResponse, err error, touchWC bool, sent []*sgitv1.Change) error {
	w := s.wc
	if err != nil {
		det := conflictDetail(err)
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
		if err := a.emit(map[string]any{"landed": false, "draft": det.DraftId, "main": det.Main, "sync": res}, func(out io.Writer) {
			fmt.Fprintf(out, "conflict: draft %s does not apply cleanly to main %s; working copy rebased onto it\n", det.DraftId, short(det.Main))
			printSync(out, res)
		}); err != nil {
			return err
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
