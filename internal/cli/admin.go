package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

const (
	repoUsage  = "repo create <name> [--mirror URL] | repo import <name> --from URL [--mirror URL] | repo status <name> | repo list  [--server S]"
	tokenUsage = "token create <user> [--admin] [--email E] | token revoke <id>  [--server S]"
	aclUsage   = "acl grant <user> <repo> read|write | acl revoke <user> <repo>  [--server S]"
)

const (
	adminTimeout  = 30 * time.Second
	importTimeout = 30 * time.Minute
)

// admin dials the server directly (never sgd) and runs fn with a deadline.
func (a *app) admin(ctx context.Context, server string, timeout time.Duration, fn func(context.Context, sgitv1.AdminClient) error) error {
	s, err := a.connect(target{server: server, adminOnly: true})
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(ctx, s.admin)
}

func cmdRepo(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return repoCreate(ctx, a, args[1:])
		case "import":
			return repoImport(ctx, a, args[1:])
		case "status":
			return repoStatus(ctx, a, args[1:])
		case "list":
			return repoList(ctx, a, args[1:])
		}
	}
	return usageErr(repoUsage)
}

func repoCreate(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo create")
	mirror := fs.String("mirror", "", "push main to this git URL after every land")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(repoUsage)
	}
	var resp *sgitv1.CreateRepoResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: pos[0], MirrorUrl: *mirror})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"repo": pos[0], "main": resp.Main}, func(w io.Writer) {
		fmt.Fprintf(w, "created %s (main %s)\n", pos[0], short(resp.Main))
	})
}

func repoImport(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo import")
	from := fs.String("from", "", "git URL to import main and release/* from (required)")
	mirror := fs.String("mirror", "", "push main to this git URL after every land; use exactly the --from URL to mirror back to the source")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *from == "" {
		return usageErr(repoUsage)
	}
	var resp *sgitv1.CreateRepoResponse
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: pos[0], FromUrl: *from, MirrorUrl: *mirror})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"repo": pos[0], "main": resp.Main}, func(w io.Writer) {
		fmt.Fprintf(w, "imported %s from %s (main %s)\n", pos[0], *from, short(resp.Main))
	})
}

func repoStatus(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo status")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(repoUsage)
	}
	var info *sgitv1.RepoInfo
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		info, err = ac.RepoStatus(ctx, &sgitv1.RepoStatusRequest{Name: pos[0]})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(toRepoJSON(info), func(w io.Writer) { printRepoStatus(w, info) })
}

func repoList(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo list")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr(repoUsage)
	}
	var resp *sgitv1.ListReposResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ListRepos(ctx, &sgitv1.ListReposRequest{})
		return err
	})
	if err != nil {
		return err
	}
	repos := make([]repoJSON, 0, len(resp.Repos))
	for _, r := range resp.Repos {
		repos = append(repos, toRepoJSON(r))
	}
	return a.emit(map[string]any{"repos": repos}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMAIN\tMIRROR")
		for _, r := range resp.Repos {
			main := short(r.Main)
			if r.Error != "" {
				main = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, main, mirrorSummary(r))
		}
		tw.Flush()
	})
}

type repoJSON struct {
	Name          string `json:"name"`
	Main          string `json:"main"`
	Created       string `json:"created"`
	MirrorURL     string `json:"mirror_url,omitempty"`
	MirrorLast    string `json:"mirror_last,omitempty"`
	MirrorPending bool   `json:"mirror_pending"`
	MirrorError   string `json:"mirror_error,omitempty"`
	MirrorAttempt string `json:"mirror_attempt,omitempty"`
	Error         string `json:"error,omitempty"`
}

func toRepoJSON(r *sgitv1.RepoInfo) repoJSON {
	return repoJSON{
		Name: r.Name, Main: r.Main, Created: unixTime(r.CreatedUnix),
		MirrorURL: r.MirrorUrl, MirrorLast: r.MirrorLast, MirrorPending: r.MirrorPending,
		MirrorError: r.MirrorError, MirrorAttempt: unixTime(r.MirrorAttemptUnix), Error: r.Error,
	}
}

func unixTime(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func mirrorSummary(r *sgitv1.RepoInfo) string {
	switch {
	case r.Error != "":
		return "broken"
	case r.MirrorUrl == "":
		return "-"
	case r.MirrorError != "":
		return "error"
	case r.MirrorPending:
		return "pending"
	default:
		return "ok"
	}
}

func printRepoStatus(w io.Writer, r *sgitv1.RepoInfo) {
	fmt.Fprintf(w, "repo:     %s\n", r.Name)
	if r.Error != "" {
		fmt.Fprintf(w, "error:    %s\n", r.Error)
	}
	fmt.Fprintf(w, "main:     %s\n", r.Main)
	fmt.Fprintf(w, "created:  %s\n", unixTime(r.CreatedUnix))
	if r.MirrorUrl == "" {
		fmt.Fprintln(w, "mirror:   none")
		return
	}
	fmt.Fprintf(w, "mirror:   %s\n", r.MirrorUrl)
	last := r.MirrorLast
	if last == "" {
		last = "never"
	}
	fmt.Fprintf(w, "  last:    %s\n", last)
	pending := "no"
	if r.MirrorPending {
		pending = "yes"
	}
	fmt.Fprintf(w, "  pending: %s\n", pending)
	if r.MirrorAttemptUnix != 0 {
		fmt.Fprintf(w, "  attempt: %s\n", unixTime(r.MirrorAttemptUnix))
	}
	if r.MirrorError != "" {
		fmt.Fprintf(w, "  error:   %s\n", r.MirrorError)
	}
}

func cmdToken(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return tokenCreate(ctx, a, args[1:])
		case "revoke":
			return tokenRevoke(ctx, a, args[1:])
		}
	}
	return usageErr(tokenUsage)
}

func tokenCreate(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("token create")
	admin := fs.Bool("admin", false, "make a new user an admin (ignored if the user exists)")
	email := fs.String("email", "", "email for a new user (default <user>@sgit.local; ignored if the user exists)")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(tokenUsage)
	}
	var resp *sgitv1.CreateTokenResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: pos[0], Email: *email, Admin: *admin})
		return err
	})
	if err != nil {
		return err
	}
	if !a.json {
		fmt.Fprintf(a.io.Stderr, "token %s for %s (shown only once):\n", resp.Id, resp.User)
	}
	return a.emit(map[string]any{"id": resp.Id, "user": resp.User, "token": resp.Token}, func(w io.Writer) {
		fmt.Fprintln(w, resp.Token)
	})
}

func tokenRevoke(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("token revoke")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(tokenUsage)
	}
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) error {
		_, err := ac.RevokeToken(ctx, &sgitv1.RevokeTokenRequest{Id: pos[0]})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"revoked": pos[0]}, func(w io.Writer) {
		fmt.Fprintf(w, "revoked token %s\n", pos[0])
	})
}

func cmdACL(ctx context.Context, a *app, args []string) error {
	if len(args) == 0 || (args[0] != "grant" && args[0] != "revoke") {
		return usageErr(aclUsage)
	}
	verb := args[0]
	fs := a.newFlags("acl " + verb)
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	want := 2
	if verb == "grant" {
		want = 3
	}
	if len(pos) != want {
		return usageErr(aclUsage)
	}
	user, repo := pos[0], pos[1]
	level := ""
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) error {
		if verb == "grant" {
			level = pos[2]
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: user, Repo: repo, Level: level})
			return err
		}
		_, err := ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: user, Repo: repo})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"user": user, "repo": repo, "level": level}, func(w io.Writer) {
		if verb == "grant" {
			fmt.Fprintf(w, "granted %s %s access to %s\n", user, level, repo)
		} else {
			fmt.Fprintf(w, "revoked %s's access to %s\n", user, repo)
		}
	})
}
