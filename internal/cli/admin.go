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
	repoUsage  = "repo create <name> [--mirror URL] | repo import <name> --from URL [--mirror URL] | repo status <name> | repo list | repo delete <name> --yes  [--server S]"
	userUsage  = "user list | user promote <user> | user demote <user> | user delete <user>  [--server S]"
	tokenUsage = "token create <user> [--admin] [--email E] | token revoke <id> | token list [user]  [--server S]"
	aclUsage   = "acl grant <user> <project> read|write | acl revoke <user> <project> | acl list [--user U] [--project P]  [--server S]"
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
		case "delete":
			return repoDelete(ctx, a, args[1:])
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
		case "list":
			return tokenList(ctx, a, args[1:])
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
	if len(args) > 0 && args[0] == "list" {
		return aclList(ctx, a, args[1:])
	}
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
	user, project := pos[0], pos[1]
	level := ""
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) error {
		if verb == "grant" {
			level = pos[2]
			_, err := ac.GrantACL(ctx, &sgitv1.GrantACLRequest{User: user, Project: project, Level: level})
			return err
		}
		_, err := ac.RevokeACL(ctx, &sgitv1.RevokeACLRequest{User: user, Project: project})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"user": user, "project": project, "level": level}, func(w io.Writer) {
		if verb == "grant" {
			fmt.Fprintf(w, "granted %s %s access to %s\n", user, level, project)
		} else {
			fmt.Fprintf(w, "revoked %s's access to %s\n", user, project)
		}
	})
}

func repoDelete(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("repo delete")
	yes := fs.Bool("yes", false, "confirm: delete the repo (its files are moved aside, not erased)")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(repoUsage)
	}
	if !*yes {
		return exitf(ExitError, "refusing to delete repo %q without --yes (this removes its drafts; the git data is moved to <data>/deleted/ on the server)", pos[0])
	}
	var resp *sgitv1.DeleteRepoResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: pos[0]})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"repo": pos[0], "moved_to": resp.MovedTo}, func(w io.Writer) {
		fmt.Fprintf(w, "deleted repo %s (git data moved to %s on the server)\n", pos[0], resp.MovedTo)
	})
}

func cmdUser(ctx context.Context, a *app, args []string) error {
	if len(args) == 0 {
		return usageErr(userUsage)
	}
	verb := args[0]
	switch verb {
	case "list", "promote", "demote", "delete":
	default:
		return usageErr(userUsage)
	}
	fs := a.newFlags("user " + verb)
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	want := 1
	if verb == "list" {
		want = 0
	}
	if len(pos) != want {
		return usageErr(userUsage)
	}
	switch verb {
	case "list":
		var resp *sgitv1.ListUsersResponse
		err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
			resp, err = ac.ListUsers(ctx, &sgitv1.ListUsersRequest{})
			return err
		})
		if err != nil {
			return err
		}
		type userJSON struct {
			Name    string `json:"name"`
			Email   string `json:"email"`
			Admin   bool   `json:"admin"`
			Tokens  int32  `json:"tokens"`
			Created string `json:"created"`
		}
		users := make([]userJSON, 0, len(resp.Users))
		for _, u := range resp.Users {
			users = append(users, userJSON{u.Name, u.Email, u.Admin, u.ActiveTokens, unixTime(u.CreatedUnix)})
		}
		return a.emit(map[string]any{"users": users}, func(w io.Writer) {
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tEMAIL\tADMIN\tTOKENS\tCREATED")
			for _, u := range users {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", u.Name, u.Email, yesNo(u.Admin), u.Tokens, u.Created)
			}
			tw.Flush()
		})
	case "promote", "demote":
		admin := verb == "promote"
		err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) error {
			_, err := ac.SetUserAdmin(ctx, &sgitv1.SetUserAdminRequest{User: pos[0], Admin: admin})
			return err
		})
		if err != nil {
			return err
		}
		return a.emit(map[string]any{"user": pos[0], "admin": admin}, func(w io.Writer) {
			if admin {
				fmt.Fprintf(w, "promoted %s to admin\n", pos[0])
			} else {
				fmt.Fprintf(w, "demoted %s (no longer admin)\n", pos[0])
			}
		})
	default: // delete
		var resp *sgitv1.DeleteUserResponse
		err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
			resp, err = ac.DeleteUser(ctx, &sgitv1.DeleteUserRequest{User: pos[0]})
			return err
		})
		if err != nil {
			return err
		}
		return a.emit(map[string]any{
			"deleted": pos[0], "tokens_revoked": resp.TokensRevoked,
			"acls_removed": resp.AclsRemoved, "drafts_abandoned": resp.DraftsAbandoned,
		}, func(w io.Writer) {
			fmt.Fprintf(w, "deleted user %s (revoked %d tokens, removed %d ACLs, abandoned %d drafts)\n",
				pos[0], resp.TokensRevoked, resp.AclsRemoved, resp.DraftsAbandoned)
		})
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func tokenList(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("token list")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr(tokenUsage)
	}
	user := ""
	if len(pos) == 1 {
		user = pos[0]
	}
	var resp *sgitv1.ListTokensResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ListTokens(ctx, &sgitv1.ListTokensRequest{User: user})
		return err
	})
	if err != nil {
		return err
	}
	type tokenJSON struct {
		ID      string `json:"id"`
		User    string `json:"user"`
		Created string `json:"created"`
		Status  string `json:"status"`
		Revoked string `json:"revoked,omitempty"`
	}
	toks := make([]tokenJSON, 0, len(resp.Tokens))
	for _, t := range resp.Tokens {
		st := "active"
		if t.RevokedUnix != 0 {
			st = "revoked"
		}
		toks = append(toks, tokenJSON{t.Id, t.User, unixTime(t.CreatedUnix), st, unixTime(t.RevokedUnix)})
	}
	return a.emit(map[string]any{"tokens": toks}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSER\tCREATED\tSTATUS")
		for _, t := range toks {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.ID, t.User, t.Created, t.Status)
		}
		tw.Flush()
	})
}

func aclList(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("acl list")
	user := fs.String("user", "", "only this user's grants")
	project := fs.String("project", "", "only grants on this project")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr(aclUsage)
	}
	var resp *sgitv1.ListACLResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ListACL(ctx, &sgitv1.ListACLRequest{User: *user, Project: *project})
		return err
	})
	if err != nil {
		return err
	}
	type aclJSON struct {
		User    string `json:"user"`
		Project string `json:"project"`
		Level   string `json:"level"`
	}
	entries := make([]aclJSON, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, aclJSON{e.User, e.Project, e.Level})
	}
	return a.emit(map[string]any{"acl": entries}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USER\tPROJECT\tLEVEL")
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", e.User, e.Project, e.Level)
		}
		tw.Flush()
	})
}
