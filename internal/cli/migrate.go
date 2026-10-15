package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

const migrateUsage = "migrate in <url> [name] [--into <repo>:<folder>] [--project P --path DIR] [--mirror URL] | " +
	"migrate out <project> --to URL [--force]  [--server S]"

// repoNameRE is the server's repo-name rule; a derived name must satisfy it.
var repoNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func cmdMigrate(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "in":
			return migrateIn(ctx, a, args[1:])
		case "out":
			return migrateOut(ctx, a, args[1:])
		}
	}
	return usageErr(migrateUsage)
}

func migrateIn(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("migrate in")
	into := fs.String("into", "", "replay main under <repo>:<folder> of an existing repo instead of creating a repo")
	project := fs.String("project", "", "mount into this existing project (needs --path)")
	path := fs.String("path", "", "mount path in --project")
	mirror := fs.String("mirror", "", "push main to this git URL after every land (not with --into)")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 || (*project == "") != (*path == "") {
		return usageErr(migrateUsage)
	}
	req := &sgitv1.MigrateInRequest{FromUrl: pos[0], Project: *project, Path: *path, MirrorUrl: *mirror}
	if len(pos) == 2 {
		req.Name = pos[1]
	} else if req.Name = nameFromURL(pos[0]); !repoNameRE.MatchString(req.Name) {
		return exitf(ExitError, "cannot derive a name from %s; pass one: sg migrate in <url> <name>", pos[0])
	}
	if *into != "" {
		repo, folder, ok := strings.Cut(*into, ":")
		if !ok || repo == "" || folder == "" {
			return usageErr(migrateUsage)
		}
		req.IntoRepo, req.IntoFolder = repo, folder
	}
	var resp *sgitv1.MigrateInResponse
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.MigrateIn(ctx, req)
		return err
	})
	if err != nil {
		return err
	}
	skipped := resp.SkippedBranches
	if skipped == nil {
		skipped = []string{}
	}
	return a.emit(map[string]any{
		"repo": resp.Repo, "folder": resp.Folder, "project": resp.Project, "path": resp.Path,
		"main": resp.Main, "commits": resp.Commits, "skipped_branches": skipped, "project_head": resp.ProjectHead,
	}, func(w io.Writer) {
		target := resp.Repo
		if resp.Folder != "." {
			target += ":" + resp.Folder
		}
		noun := "commits"
		if resp.Commits == 1 {
			noun = "commit"
		}
		fmt.Fprintf(w, "migrated %s into %s: %d %s, main %s\n", pos[0], target, resp.Commits, noun, short(resp.Main))
		at := resp.Path + "/"
		if resp.Path == "." {
			at = "the project root"
		}
		fmt.Fprintf(w, "mounted at %s of project %s\n", at, resp.Project)
		if len(skipped) > 0 {
			fmt.Fprintf(w, "warning: branches not migrated: %s\n", strings.Join(skipped, ", "))
		}
	})
}

func migrateOut(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("migrate out")
	to := fs.String("to", "", "git URL to push the project's main to (required)")
	force := fs.Bool("force", false, "overwrite the target's main when it has diverged")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" {
		return usageErr(migrateUsage)
	}
	var resp *sgitv1.MigrateOutResponse
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.MigrateOut(ctx, &sgitv1.MigrateOutRequest{Project: pos[0], ToUrl: *to, Force: *force})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"project": resp.Project, "pushed": resp.Pushed, "up_to_date": resp.UpToDate}, func(w io.Writer) {
		if resp.UpToDate {
			fmt.Fprintf(w, "%s is up to date with project %s (main %s)\n", *to, resp.Project, short(resp.Pushed))
			return
		}
		fmt.Fprintf(w, "pushed project %s to %s (main %s)\n", resp.Project, *to, short(resp.Pushed))
	})
}

// nameFromURL returns the last path segment of a git URL without ".git":
// https://host/acme/api.git and git@host:acme/api both give "api".
// A URL with a host but no path (https://example.com/) gives "".
func nameFromURL(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" && strings.Trim(p.Path, "/") == "" {
		return ""
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return strings.TrimSuffix(u, ".git")
}
