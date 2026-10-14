package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/wc"
	"github.com/pmezard/go-difflib/difflib"
)

func cmdDiff(ctx context.Context, a *app, args []string) error {
	fs := a.newFlags("diff")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	w, err := openWC(a)
	if err != nil {
		return err
	}
	var filters []string
	for _, p := range pos {
		rp, err := w.RepoPath(a.io.Dir, p)
		if err != nil {
			return exitf(ExitError, "%v", err)
		}
		filters = append(filters, rp)
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	var sel []wc.Change
	var need []string
	for _, c := range changes {
		if len(filters) > 0 && !matchesAny(filters, c.Path) {
			continue
		}
		sel = append(sel, c)
		if c.Kind != wc.Added {
			need = append(need, w.Index[c.Path].SHA)
		}
	}
	var blobs map[string][]byte
	if len(need) > 0 {
		s, err := a.connect(target{needWC: true, needRepo: true})
		if err != nil {
			return err
		}
		defer s.Close()
		if blobs, err = fetch.Blobs(ctx, s.repo, s.name, need); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	for _, c := range sel {
		e := w.Index[c.Path]
		var old, cur []byte
		if c.Kind != wc.Added {
			old = blobs[e.SHA]
		}
		if c.Kind != wc.Deleted {
			if cur, _, err = wc.ReadLocal(w.Root, c.Path); err != nil {
				return err
			}
		}
		if err := writePatch(&buf, c.Path, c.Kind, e.Mode, c.Mode, old, cur); err != nil {
			return err
		}
	}
	return a.emit(map[string]any{"patch": buf.String()}, func(out io.Writer) { out.Write(buf.Bytes()) })
}

func matchesAny(filters []string, p string) bool {
	for _, f := range filters {
		if f == "" || p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

// writePatch writes a unified diff for one change.
func writePatch(w io.Writer, p string, kind wc.Kind, oldMode, newMode string, old, cur []byte) error {
	from, to := "a/"+p, "b/"+p
	fmt.Fprintf(w, "diff --sg a/%s b/%s\n", p, p)
	switch kind {
	case wc.Added:
		from = "/dev/null"
		fmt.Fprintf(w, "new file mode %s\n", newMode)
	case wc.Deleted:
		to = "/dev/null"
		fmt.Fprintf(w, "deleted file mode %s\n", oldMode)
	default:
		if oldMode != newMode {
			fmt.Fprintf(w, "old mode %s\nnew mode %s\n", oldMode, newMode)
		}
	}
	if bytes.Equal(old, cur) {
		return nil
	}
	if isBinary(old) || isBinary(cur) {
		_, err := fmt.Fprintf(w, "Binary files %s and %s differ\n", from, to)
		return err
	}
	return difflib.WriteUnifiedDiff(w, difflib.UnifiedDiff{A: splitLines(old), B: splitLines(cur), FromFile: from, ToFile: to, Context: 3})
}

// splitLines splits text into lines that all end in "\n". A last line that
// lacks a newline is followed by git's "No newline" marker, so adding or
// removing a trailing newline shows up as a change.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	} else {
		lines[len(lines)-1] += "\n\\ No newline at end of file\n"
	}
	return lines
}

func isBinary(b []byte) bool { return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 }
