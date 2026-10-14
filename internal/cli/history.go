package cli

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/fetch"
)

// repoPath resolves a path argument: relative to the cwd inside the matching
// working copy, otherwise taken as a repo path.
func (a *app) repoPath(s *session, arg string) (string, error) {
	if s.wc != nil && s.wc.Config.Repo == s.name {
		p, err := s.wc.RepoPath(a.io.Dir, arg)
		if err != nil {
			return "", exitf(ExitError, "%v", err)
		}
		return p, nil
	}
	p := strings.Trim(path.Clean(filepath.ToSlash(arg)), "/")
	if p == "." {
		p = ""
	}
	return p, nil
}

// splitCountFlag accepts git's attached forms -n5 and -5 as -n 5.
func splitCountFlag(args []string) []string {
	out := make([]string, 0, len(args)+1)
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		num := ""
		switch {
		case strings.HasPrefix(a, "-n") && len(a) > 2 && a[2] != '=':
			num = a[2:]
		case len(a) > 1 && a[0] == '-' && a[1] >= '0' && a[1] <= '9':
			num = a[1:]
		}
		if _, err := strconv.Atoi(num); num != "" && err == nil {
			out = append(out, "-n", num)
			continue
		}
		out = append(out, a)
	}
	return out
}

func cmdLog(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("log")
	n := fs.Int("n", 20, "number of commits")
	rev := fs.String("rev", "main", "revision to start from")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, splitCountFlag(args))
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("log [path] [-n N] [--rev REV] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	p := ""
	if len(pos) == 1 {
		if p, err = a.repoPath(s, pos[0]); err != nil {
			return err
		}
	}
	resp, err := s.repo.Log(ctx, &sgitv1.LogRequest{Project: s.name, Rev: *rev, Path: p, N: int32(*n)})
	if err != nil {
		return err
	}
	type item struct {
		Sha     string `json:"sha"`
		Author  string `json:"author"`
		Email   string `json:"email"`
		Time    string `json:"time"`
		Subject string `json:"subject"`
	}
	items := []item{}
	for _, e := range resp.Entries {
		items = append(items, item{e.Sha, e.AuthorName, e.AuthorEmail, time.Unix(e.Time, 0).UTC().Format(time.RFC3339), e.Subject})
	}
	return a.emit(items, func(out io.Writer) {
		for _, it := range items {
			fmt.Fprintf(out, "%s %s %-12s %s\n", short(it.Sha), it.Time[:10], it.Author, it.Subject)
		}
	})
}

func cmdBlame(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("blame")
	rev := fs.String("rev", "main", "revision")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("blame <path> [--rev REV] [--repo R] [--server S]")
	}
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	p, err := a.repoPath(s, pos[0])
	if err != nil {
		return err
	}
	resp, err := s.repo.Blame(ctx, &sgitv1.BlameRequest{Project: s.name, Rev: *rev, Path: p})
	if err != nil {
		return err
	}
	type item struct {
		Sha    string `json:"sha"`
		Author string `json:"author"`
		Line   int32  `json:"line"`
		Text   string `json:"text"`
	}
	items := []item{}
	for _, l := range resp.Lines {
		items = append(items, item{l.Sha, l.Author, l.Line, string(l.Text)})
	}
	return a.emit(items, func(out io.Writer) {
		for _, it := range items {
			fmt.Fprintf(out, "%s (%-12s %4d) %s\n", short(it.Sha), it.Author, it.Line, it.Text)
		}
	})
}

// cmdCat prints a file at a revision. The path is repo-relative (git
// "<rev>:<path>" syntax); an empty rev means main.
func cmdCat(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("cat")
	repo := fs.String("repo", "", "repository (default: the working copy's)")
	server := fs.String("server", "", "server")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !strings.Contains(pos[0], ":") {
		return usageErr("cat <rev>:<path> [--repo R] [--server S]")
	}
	rev, p, _ := strings.Cut(pos[0], ":")
	if rev == "" {
		rev = "main"
	}
	p = strings.Trim(path.Clean(p), "/")
	s, err := a.connect(target{server: *server, repo: *repo, needRepo: true})
	if err != nil {
		return err
	}
	defer s.Close()
	rr, err := s.repo.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: s.name, Rev: rev})
	if err != nil {
		return err
	}
	tree, err := fetch.Tree(ctx, s.repo, s.name, rr.Sha, []string{p})
	if err != nil {
		return err
	}
	f, ok := tree[p]
	if !ok {
		return exitf(ExitError, "%s: no such file at %s", p, rev)
	}
	blobs, err := fetch.Blobs(ctx, s.repo, s.name, []string{f.SHA})
	if err != nil {
		return err
	}
	data := blobs[f.SHA]
	return a.emit(map[string]any{"path": p, "sha": f.SHA, "mode": f.Mode, "content": data}, func(out io.Writer) { out.Write(data) })
}
