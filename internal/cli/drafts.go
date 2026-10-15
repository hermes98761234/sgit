package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var draftIDRe = regexp.MustCompile(`^[a-z2-7]{8}$`)

func cmdDrafts(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("drafts")
	mine := fs.Bool("mine", false, "only my drafts")
	st := fs.String("status", "open", "open, landed, abandoned or all")
	project := fs.String("project", "", "project name, alias or #id (default: the working copy's)")
	server := fs.String("server", "", "server (default: the working copy's or the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("drafts [--mine] [--status S] [--project P] [--server S]")
	}
	want := *st
	if want == "all" {
		want = ""
	}
	s, err := a.connect(target{server: *server, project: *project, needProject: true})
	if err != nil {
		return err
	}
	defer s.Close()
	resp, err := s.proj.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: s.project, Mine: *mine, Status: want})
	if err != nil {
		return err
	}
	type item struct {
		ID      string `json:"id"`
		Version int32  `json:"version"`
		Status  string `json:"status"`
		Owner   string `json:"owner"`
		Title   string `json:"title"`
		Head    string `json:"head"`
		Updated string `json:"updated"`
	}
	items := []item{}
	for _, d := range resp.Drafts {
		items = append(items, item{d.Id, d.Version, d.Status, d.Owner, d.Title, d.Head, time.Unix(d.UpdatedAt, 0).UTC().Format(time.RFC3339)})
	}
	return a.emit(map[string]any{"drafts": items}, func(out io.Writer) {
		if len(items) == 0 {
			fmt.Fprintln(out, "no drafts")
		}
		for _, it := range items {
			fmt.Fprintf(out, "%s v%-3d %-9s %-12s %s\n", it.ID, it.Version, it.Status, it.Owner, it.Title)
		}
	})
}

func cmdShow(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("show")
	project := fs.String("project", "", "project name, alias or #id (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("show <draft-id|rev> [--project P] [--server S]")
	}
	s, err := a.connect(target{server: *server, project: *project, needProject: true})
	if err != nil {
		return err
	}
	defer s.Close()
	arg := pos[0]
	if draftIDRe.MatchString(arg) {
		d, err := s.proj.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: s.project, Id: arg})
		if err == nil {
			df, err := s.proj.Diff(ctx, &sgitv1.DiffRequest{Project: s.project, A: d.Base, B: d.Head})
			if err != nil {
				return err
			}
			res := map[string]any{"draft": d.Id, "version": d.Version, "status": d.Status, "owner": d.Owner,
				"title": d.Title, "base": d.Base, "head": d.Head, "patch": string(df.Patch)}
			return a.emit(res, func(out io.Writer) {
				fmt.Fprintf(out, "draft %s v%d (%s) by %s\n%s\nbase %s head %s\n\n%s", d.Id, d.Version, d.Status, d.Owner, d.Title, short(d.Base), short(d.Head), df.Patch)
			})
		}
		if status.Code(err) != codes.NotFound {
			return err
		}
	}
	lg, err := s.proj.Log(ctx, &sgitv1.LogRequest{Project: s.project, Rev: arg, N: 1})
	if err != nil {
		return err
	}
	if len(lg.Entries) == 0 {
		return exitf(ExitError, "no such revision %q", arg)
	}
	e := lg.Entries[0]
	patch := ""
	if df, err := s.proj.Diff(ctx, &sgitv1.DiffRequest{Project: s.project, A: e.Sha + "^", B: e.Sha}); err == nil {
		patch = string(df.Patch)
	}
	when := time.Unix(e.Time, 0).UTC().Format(time.RFC3339)
	res := map[string]any{"commit": e.Sha, "author": e.AuthorName, "email": e.AuthorEmail, "time": when, "subject": e.Subject, "patch": patch}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "commit %s\nAuthor: %s <%s>\nDate:   %s\n\n    %s\n\n%s", e.Sha, e.AuthorName, e.AuthorEmail, when, e.Subject, patch)
	})
}

func cmdAbandon(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("abandon")
	project := fs.String("project", "", "project name, alias or #id (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("abandon [draft-id] [--project P] [--server S]")
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
		return exitf(ExitError, "no draft to abandon")
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
	d, err := s.proj.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: s.project, Id: id})
	if err != nil {
		return err
	}
	if touch {
		s.wc.Draft = ""
		if err := s.wc.Save(); err != nil {
			return err
		}
	}
	return a.emit(map[string]any{"draft": d.Id, "status": d.Status}, func(out io.Writer) {
		fmt.Fprintf(out, "abandoned draft %s\n", d.Id)
	})
}
