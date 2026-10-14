package cli

import (
	"context"
	"fmt"
	"io"
	"maps"

	"github.com/hermes98761234/sgit/internal/wc"
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
	before := maps.Clone(w.Index)
	changes, err := w.Status()
	if err != nil {
		return err
	}
	if !maps.Equal(before, w.Index) {
		saveStatCache(w, before)
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

// saveStatCache persists the stat cache that Status refreshed, as a best
// effort: only when the working-copy lock is free right now, and only when
// the index on disk is still the one Status started from (before). Otherwise
// it skips silently; the next status re-hashes.
func saveStatCache(w *wc.WC, before map[string]wc.Entry) {
	unlock, err := w.Lock()
	if err != nil {
		return
	}
	defer unlock()
	disk, err := wc.Open(w.Root)
	if err != nil || disk.Base != w.Base || !maps.Equal(disk.Index, before) {
		return
	}
	_ = w.SaveIndex()
}
