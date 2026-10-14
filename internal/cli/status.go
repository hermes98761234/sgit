package cli

import (
	"context"
	"fmt"
	"io"
)

func cmdStatus(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("status")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("status")
	}
	w, err := openWC(a)
	if err != nil {
		return err
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	if err := w.Save(); err != nil { // persist the refreshed stat cache
		return err
	}
	type item struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	res := struct {
		Repo      string   `json:"repo"`
		Base      string   `json:"base"`
		Draft     string   `json:"draft,omitempty"`
		Changes   []item   `json:"changes"`
		Conflicts []string `json:"conflicts"`
	}{Repo: w.Config.Repo, Base: w.Base, Draft: w.Draft, Changes: []item{}, Conflicts: append([]string{}, w.Conflicts...)}
	for _, c := range changes {
		res.Changes = append(res.Changes, item{c.Path, c.Kind.String()})
	}
	return a.emit(res, func(out io.Writer) {
		fmt.Fprintf(out, "On %s@%s", w.Config.Repo, short(w.Base))
		if w.Draft != "" {
			fmt.Fprintf(out, " (draft %s)", w.Draft)
		}
		fmt.Fprintln(out)
		if len(changes) == 0 && len(w.Conflicts) == 0 {
			fmt.Fprintln(out, "nothing changed")
			return
		}
		for _, c := range changes {
			fmt.Fprintf(out, "  %-9s %s\n", c.Kind.String()+":", c.Path)
		}
		for _, p := range w.Conflicts {
			fmt.Fprintf(out, "  %-9s %s\n", "conflict:", p)
		}
	})
}
