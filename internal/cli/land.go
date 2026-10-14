package cli

import (
	"context"
	"fmt"
	"io"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
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
	touch := s.wc != nil && s.wc.Config.Server == s.server && s.wc.Config.Repo == s.name && s.wc.Draft == id
	resp, err := s.repo.Land(ctx, &sgitv1.LandRequest{Repo: s.name, Id: id})
	return a.finishLand(ctx, s, resp, err, touch)
}

// commitAndLand creates/updates the working copy's draft and lands it in one
// round trip when a draft exists. A stale .sg/draft is dropped and the call retried once.
func (a *app) commitAndLand(ctx context.Context, s *session, changes []*sgitv1.Change, title, msg string) error {
	w := s.wc
	if w.Draft != "" {
		req := &sgitv1.CommitAndLandRequest{Repo: s.name, DraftId: w.Draft, Base: w.Base, Changes: changes, Title: title, Message: msg}
		resp, err := s.repo.CommitAndLand(ctx, req)
		if err == nil || !isStaleDraft(err) {
			return a.finishLand(ctx, s, resp, err, true)
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
	resp, err := s.repo.Land(ctx, &sgitv1.LandRequest{Repo: s.name, Id: d.Id})
	return a.finishLand(ctx, s, resp, err, true)
}

// finishLand updates the working copy after Land/CommitAndLand. touchWC is
// false when the landed draft does not belong to this working copy.
func (a *app) finishLand(ctx context.Context, s *session, resp *sgitv1.LandResponse, err error, touchWC bool) error {
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
	if w != nil && touchWC {
		w.Draft = ""
		if res, err = syncTo(ctx, s, resp.Commit); err != nil {
			// syncTo left Base and Index alone; persist the cleared draft.
			if serr := w.Save(); serr != nil {
				return serr
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
