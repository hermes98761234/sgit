package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const uploadFlushBytes = 32 << 20

func cmdCommit(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("commit")
	msg := fs.String("m", "", "commit message (required)")
	title := fs.String("title", "", "draft title (default: first line of the message)")
	land := fs.Bool("land", false, "land on main right away (rebase on the server)")
	draft := fs.String("draft", "", "update this draft instead of the one in .sg/draft")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("commit -m MSG [--title T] [--land] [--draft ID]")
	}
	if strings.TrimSpace(*msg) == "" {
		return exitf(ExitError, "a commit message is required (-m)")
	}
	if *draft != "" && !draftIDRe.MatchString(*draft) {
		return exitf(ExitError, "invalid draft id %q", *draft)
	}
	s, err := a.connect(target{needWC: true, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	w := s.wc
	if err := checkConflicts(w); err != nil {
		return err
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	for _, p := range w.Skipped {
		fmt.Fprintf(a.io.Stderr, "sg: warning: skipping %q: the server cannot store this file name\n", p)
	}
	if len(changes) == 0 {
		return exitf(ExitError, "nothing to commit")
	}
	pb, err := uploadChanges(ctx, s, changes)
	if err != nil {
		return err
	}
	if *draft != "" {
		w.Draft = *draft
	}
	if *land {
		return a.commitAndLand(ctx, s, pb, *title, *msg)
	}
	d, err := saveDraft(ctx, s, pb, *title, *msg)
	if err != nil {
		return err
	}
	w.Draft = d.Id
	if err := w.Save(); err != nil {
		return err
	}
	return a.emit(map[string]any{"draft": d.Id, "version": d.Version, "changes": len(pb)}, func(out io.Writer) {
		fmt.Fprintf(out, "draft %s v%d (%d changes)\n", d.Id, d.Version, len(pb))
	})
}

func hasMarkers(b []byte) bool {
	return bytes.HasPrefix(b, []byte("<<<<<<< ")) || bytes.Contains(b, []byte("\n<<<<<<< "))
}

// checkConflicts drops resolved paths from w.Conflicts and fails while any
// listed file still contains conflict markers. Only paths recorded by sync
// are checked: other files may contain marker-like lines on purpose, and
// sync records its conflicts before writing any markers.
func checkConflicts(w *wc.WC) error {
	var still []string
	for _, p := range w.Conflicts {
		if data, _, err := wc.ReadLocal(w.Root, p); err == nil && hasMarkers(data) {
			still = append(still, p)
		}
	}
	if len(still) != len(w.Conflicts) {
		w.Conflicts = still
		if err := w.Save(); err != nil {
			return err
		}
	}
	if len(still) > 0 {
		return exitf(ExitConflict, "unresolved conflicts in %s (remove the conflict markers first)", strings.Join(still, ", "))
	}
	return nil
}

// uploadChanges uploads local content the server lacks and returns the
// change list (the full delta of the working copy against its base).
func uploadChanges(ctx context.Context, s *session, changes []wc.Change) ([]*sgitv1.Change, error) {
	pb := make([]*sgitv1.Change, 0, len(changes))
	want := map[string]string{}
	for _, ch := range changes {
		c := &sgitv1.Change{Path: ch.Path}
		switch ch.Kind {
		case wc.Added:
			c.Op, c.BlobSha, c.Mode = sgitv1.ChangeOp_CHANGE_OP_ADD, ch.SHA, ch.Mode
			want[ch.SHA] = ch.Path
		case wc.Modified:
			c.Op, c.BlobSha, c.Mode = sgitv1.ChangeOp_CHANGE_OP_MODIFY, ch.SHA, ch.Mode
			want[ch.SHA] = ch.Path
		case wc.Deleted:
			c.Op = sgitv1.ChangeOp_CHANGE_OP_DELETE
		}
		pb = append(pb, c)
	}
	return pb, uploadMissing(ctx, s, want)
}

// uploadMissing uploads the blobs (sha -> working-copy path) the server
// does not have yet, flushing every uploadFlushBytes.
func uploadMissing(ctx context.Context, s *session, paths map[string]string) error {
	shas := make([]string, 0, len(paths))
	for sha := range paths {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	var missing []string
	for start := 0; start < len(shas); start += 1000 {
		chunk := shas[start:min(start+1000, len(shas))]
		resp, err := s.repo.HasObjects(ctx, &sgitv1.HasObjectsRequest{Repo: s.name, Shas: chunk})
		if err != nil {
			return err
		}
		if len(resp.Present) != len(chunk) {
			return fmt.Errorf("server answered HasObjects with %d flags for %d objects", len(resp.Present), len(chunk))
		}
		for i, ok := range resp.Present {
			if !ok {
				missing = append(missing, chunk[i])
			}
		}
	}
	var batch [][]byte
	var expect []string
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		got, err := rpc.UploadBlobs(ctx, s.repo, s.name, batch)
		if err != nil {
			return err
		}
		for i := range expect {
			if i >= len(got) || got[i] != expect[i] {
				return fmt.Errorf("server stored an unexpected blob for %s", paths[expect[i]])
			}
		}
		batch, expect, size = nil, nil, 0
		return nil
	}
	for _, sha := range missing {
		data, _, err := wc.ReadLocal(s.wc.Root, paths[sha])
		if err != nil {
			return err
		}
		if objfmt.BlobSHA(data) != sha {
			return exitf(ExitError, "%s changed while committing; run the command again", paths[sha])
		}
		batch, expect, size = append(batch, data), append(expect, sha), size+len(data)
		if size >= uploadFlushBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// isStaleDraft reports whether err says the draft can no longer be updated
// (deleted, landed or abandoned elsewhere).
func isStaleDraft(err error) bool {
	switch status.Code(err) {
	case codes.NotFound:
		return true
	case codes.FailedPrecondition:
		m := status.Convert(err).Message()
		return strings.HasSuffix(m, " is landed") || strings.HasSuffix(m, " is abandoned")
	}
	return false
}

// saveDraft updates the working copy's open draft, or creates one. A stale
// .sg/draft starts a new draft.
func saveDraft(ctx context.Context, s *session, changes []*sgitv1.Change, title, msg string) (*sgitv1.Draft, error) {
	w := s.wc
	if w.Draft != "" {
		d, err := s.repo.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Repo: s.name, Id: w.Draft, Base: w.Base, Changes: changes, Message: msg})
		if err == nil {
			return d, nil
		}
		if !isStaleDraft(err) {
			return nil, err
		}
		w.Draft = ""
	}
	return s.repo.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Repo: s.name, Base: w.Base, Changes: changes, Title: title, Message: msg})
}
