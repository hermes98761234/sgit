package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
)

// cmdProjects lists the projects the caller can read, with their aliases and
// the caller's access level.
func cmdProjects(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("projects")
	server := fs.String("server", "", "server (default: from sg login)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("projects [--server S]")
	}
	s, err := a.connect(target{server: *server})
	if err != nil {
		return err
	}
	defer s.Close()
	resp, err := s.proj.ListProjects(ctx, &sgitv1.ListProjectsRequest{})
	if err != nil {
		return err
	}
	type pj struct {
		ID      int64    `json:"id"`
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
		Level   string   `json:"level"`
	}
	out := make([]pj, 0, len(resp.Projects))
	for _, p := range resp.Projects {
		al := p.Aliases
		if al == nil {
			al = []string{}
		}
		out = append(out, pj{p.Id, p.Name, al, p.Level})
	}
	return a.emit(map[string]any{"projects": out}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tALIASES\tLEVEL")
		for _, p := range out {
			al := strings.Join(p.Aliases, ",")
			if al == "" {
				al = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, al, p.Level)
		}
		tw.Flush()
	})
}
