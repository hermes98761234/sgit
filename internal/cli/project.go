package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

const projectUsage = "project create <name> | project mount <project> <path> <repo>:<folder> [--read-only] | project unmount <project> <path> | " +
	"project alias add <project> <alias> | project alias rm <alias> | project rename <old> <new> | project list | project show <project> | " +
	"project delete <project> --yes  [--server S]"

type mountJSON struct {
	Path     string `json:"path"`
	Repo     string `json:"repo"`
	Folder   string `json:"folder"`
	ReadOnly bool   `json:"read_only"`
}

type grantJSON struct {
	User  string `json:"user"`
	Level string `json:"level"`
}

type projectJSON struct {
	ID      int64       `json:"id"`
	Name    string      `json:"name"`
	Aliases []string    `json:"aliases"`
	Head    string      `json:"head"`
	Mounts  []mountJSON `json:"mounts"`
	Grants  []grantJSON `json:"grants"`
}

// toProjectJSON never leaves a slice nil, so JSON prints [] rather than null.
// Aliases and mounts are sorted (by name and path) for stable output.
func toProjectJSON(p *sgitv1.ProjectInfo) projectJSON {
	j := projectJSON{
		ID: p.Id, Name: p.Name, Head: p.Head,
		Aliases: append([]string{}, p.Aliases...),
		Mounts:  make([]mountJSON, 0, len(p.Mounts)),
		Grants:  make([]grantJSON, 0, len(p.Grants)),
	}
	sort.Strings(j.Aliases)
	for _, m := range p.Mounts {
		j.Mounts = append(j.Mounts, mountJSON{Path: m.Path, Repo: m.Repo, Folder: m.Folder, ReadOnly: m.ReadOnly})
	}
	sort.Slice(j.Mounts, func(a, b int) bool { return j.Mounts[a].Path < j.Mounts[b].Path })
	for _, g := range p.Grants {
		j.Grants = append(j.Grants, grantJSON{User: g.User, Level: g.Level})
	}
	return j
}

func cmdProject(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return projectCreate(ctx, a, args[1:])
		case "mount":
			return projectMount(ctx, a, args[1:])
		case "unmount":
			return projectUnmount(ctx, a, args[1:])
		case "alias":
			return projectAlias(ctx, a, args[1:])
		case "rename":
			return projectRename(ctx, a, args[1:])
		case "list":
			return projectList(ctx, a, args[1:])
		case "show":
			return projectShow(ctx, a, args[1:])
		case "delete":
			return projectDelete(ctx, a, args[1:])
		}
	}
	return usageErr(projectUsage)
}

// projectCall parses a subcommand's flags, checks the positional count, and
// runs one admin RPC returning a ProjectInfo.
func projectCall(ctx context.Context, a *app, name string, args []string, npos int,
	call func(context.Context, sgitv1.AdminClient, []string) (*sgitv1.ProjectInfo, error)) (*sgitv1.ProjectInfo, []string, error) {
	fs := a.newFlags(name)
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, nil, err
	}
	if len(pos) != npos {
		return nil, nil, usageErr(projectUsage)
	}
	var info *sgitv1.ProjectInfo
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		info, err = call(ctx, ac, pos)
		return err
	})
	return info, pos, err
}

func projectCreate(ctx context.Context, a *app, args []string) error {
	info, _, err := projectCall(ctx, a, "project create", args, 1, func(ctx context.Context, ac sgitv1.AdminClient, pos []string) (*sgitv1.ProjectInfo, error) {
		return ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: pos[0]})
	})
	if err != nil {
		return err
	}
	return a.emit(toProjectJSON(info), func(w io.Writer) {
		fmt.Fprintf(w, "created project %s (id %d)\n", info.Name, info.Id)
	})
}

func projectMount(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("project mount")
	ro := fs.Bool("read-only", false, "refuse writes to this mount, from everyone")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		return usageErr(projectUsage)
	}
	repo, folder, ok := strings.Cut(pos[2], ":")
	if !ok || repo == "" || folder == "" {
		return usageErr(projectUsage)
	}
	var info *sgitv1.ProjectInfo
	// The first mount imports the folder's whole history.
	err = a.admin(ctx, *srv, importTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		info, err = ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: pos[0], Path: pos[1], Repo: repo, Folder: folder, ReadOnly: *ro})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(toProjectJSON(info), func(w io.Writer) {
		suffix := ""
		if *ro {
			suffix = " (read-only)"
		}
		fmt.Fprintf(w, "mounted %s:%s at %s/ in %s%s\n", repo, folder, pos[1], info.Name, suffix)
	})
}

func projectUnmount(ctx context.Context, a *app, args []string) error {
	info, pos, err := projectCall(ctx, a, "project unmount", args, 2, func(ctx context.Context, ac sgitv1.AdminClient, pos []string) (*sgitv1.ProjectInfo, error) {
		return ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: pos[0], Path: pos[1]})
	})
	if err != nil {
		return err
	}
	return a.emit(toProjectJSON(info), func(w io.Writer) {
		fmt.Fprintf(w, "unmounted %s/ from %s\n", pos[1], info.Name)
	})
}

func projectAlias(ctx context.Context, a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			info, pos, err := projectCall(ctx, a, "project alias add", args[1:], 2, func(ctx context.Context, ac sgitv1.AdminClient, pos []string) (*sgitv1.ProjectInfo, error) {
				return ac.AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: pos[0], Alias: pos[1]})
			})
			if err != nil {
				return err
			}
			return a.emit(toProjectJSON(info), func(w io.Writer) {
				fmt.Fprintf(w, "added alias %s for %s\n", pos[1], info.Name)
			})
		case "rm":
			info, pos, err := projectCall(ctx, a, "project alias rm", args[1:], 1, func(ctx context.Context, ac sgitv1.AdminClient, pos []string) (*sgitv1.ProjectInfo, error) {
				return ac.RemoveProjectAlias(ctx, &sgitv1.RemoveProjectAliasRequest{Alias: pos[0]})
			})
			if err != nil {
				return err
			}
			return a.emit(toProjectJSON(info), func(w io.Writer) {
				fmt.Fprintf(w, "removed alias %s (was %s)\n", pos[0], info.Name)
			})
		}
	}
	return usageErr(projectUsage)
}

func projectRename(ctx context.Context, a *app, args []string) error {
	info, pos, err := projectCall(ctx, a, "project rename", args, 2, func(ctx context.Context, ac sgitv1.AdminClient, pos []string) (*sgitv1.ProjectInfo, error) {
		return ac.RenameProject(ctx, &sgitv1.RenameProjectRequest{Project: pos[0], NewName: pos[1]})
	})
	if err != nil {
		return err
	}
	return a.emit(toProjectJSON(info), func(w io.Writer) {
		fmt.Fprintf(w, "renamed %s to %s (%s is kept as an alias)\n", pos[0], info.Name, pos[0])
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func projectList(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("project list")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr(projectUsage)
	}
	var resp *sgitv1.AdminListProjectsResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.ListProjects(ctx, &sgitv1.AdminListProjectsRequest{})
		return err
	})
	if err != nil {
		return err
	}
	projects := make([]projectJSON, 0, len(resp.Projects))
	for _, p := range resp.Projects {
		projects = append(projects, toProjectJSON(p))
	}
	return a.emit(map[string]any{"projects": projects}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tALIASES\tMOUNTS")
		for _, p := range projects {
			mounts := make([]string, 0, len(p.Mounts))
			for _, m := range p.Mounts {
				s := m.Path + "=" + m.Repo + ":" + m.Folder
				if m.ReadOnly {
					s += "(ro)"
				}
				mounts = append(mounts, s)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, orDash(strings.Join(p.Aliases, ",")), orDash(strings.Join(mounts, ",")))
		}
		tw.Flush()
	})
}

func projectShow(ctx context.Context, a *app, args []string) error {
	info, _, err := projectCall(ctx, a, "project show", args, 1, func(ctx context.Context, ac sgitv1.AdminClient, pos []string) (*sgitv1.ProjectInfo, error) {
		return ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: pos[0]})
	})
	if err != nil {
		return err
	}
	p := toProjectJSON(info)
	return a.emit(p, func(w io.Writer) {
		fmt.Fprintf(w, "project: %s (id %d)\n", p.Name, p.ID)
		fmt.Fprintf(w, "aliases: %s\n", orDash(strings.Join(p.Aliases, ", ")))
		head := "-"
		if p.Head != "" {
			head = short(p.Head)
		}
		fmt.Fprintf(w, "head:    %s\n", head)
		fmt.Fprintln(w, "mounts:")
		for _, m := range p.Mounts {
			ro := ""
			if m.ReadOnly {
				ro = "  read-only"
			}
			fmt.Fprintf(w, "  %s/  %s:%s%s\n", m.Path, m.Repo, m.Folder, ro)
		}
		fmt.Fprintln(w, "grants:")
		for _, g := range p.Grants {
			fmt.Fprintf(w, "  %s  %s\n", g.User, g.Level)
		}
		if len(p.Grants) == 0 {
			fmt.Fprintln(w, "  -")
		}
	})
}

func projectDelete(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("project delete")
	yes := fs.Bool("yes", false, "confirm: delete the project")
	srv := fs.String("server", "", "server (default: the working copy's, else the login default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(projectUsage)
	}
	if !*yes {
		return exitf(ExitError, "refusing to delete project %q without --yes (this removes its aliases, mounts, grants, drafts and derived repo)", pos[0])
	}
	var resp *sgitv1.DeleteProjectResponse
	err = a.admin(ctx, *srv, adminTimeout, func(ctx context.Context, ac sgitv1.AdminClient) (err error) {
		resp, err = ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: pos[0]})
		return err
	})
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"project": pos[0], "drafts_removed": resp.DraftsRemoved, "acls_removed": resp.AclsRemoved}, func(w io.Writer) {
		fmt.Fprintf(w, "deleted project %s (%d drafts, %d grants removed)\n", pos[0], resp.DraftsRemoved, resp.AclsRemoved)
	})
}
