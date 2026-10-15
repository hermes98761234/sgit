package cli_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
)

// benchHistory is the number of one-file commits landed on top of the seed,
// so that `sg log -n 100` returns 100 entries.
const benchHistory = 119

// benchRepo builds the 20,000-file layout from the README (20 dirs × 1,000
// files of about 2.7 KB) as repo "big", and project "big" mounting either
// d0..d19 as 20 folder mounts at the same paths (folders) or the whole repo
// at "." (otherwise). benchHistory one-file commits follow the mounts, and
// the project is caught up before timing starts.
func benchRepo(b *testing.B, folders bool) *harness {
	b.Helper()
	h := newHarnessTB(b)
	h.env.CreateRepo("big")
	files := map[string]string{}
	body := string(make([]byte, 2700))
	for d := 0; d < 20; d++ {
		for f := 0; f < 1000; f++ {
			files[fmt.Sprintf("d%d/f%04d.txt", d, f)] = fmt.Sprintf("%d/%d\n", d, f) + body
		}
	}
	h.env.CommitDirect("big", files, "seed")
	h.env.CreateProject("big")
	if folders {
		for d := 0; d < 20; d++ {
			dir := fmt.Sprintf("d%d", d)
			h.env.Mount("big", dir, "big", dir, false)
		}
	} else {
		h.env.Mount("big", ".", "big", ".", false)
	}
	for i := 0; i < benchHistory; i++ {
		h.env.CommitDirect("big", map[string]string{fmt.Sprintf("d%d/f%04d.txt", i%20, i): fmt.Sprintf("history %d\n", i)}, fmt.Sprintf("history %d", i))
	}
	testenv.Must(h.env.Project(h.env.AdminToken).ResolveRev(b.Context(), &sgitv1.ResolveRevRequest{Project: "big"}))
	return h
}

// benchMounts runs fn as two sub-benchmarks: 20 folder mounts and one "." mount.
func benchMounts(b *testing.B, fn func(b *testing.B, h *harness)) {
	for _, c := range []struct {
		name    string
		folders bool
	}{{"mounts=folders", true}, {"mounts=dot", false}} {
		b.Run(c.name, func(b *testing.B) { fn(b, benchRepo(b, c.folders)) })
	}
}

func BenchmarkProjectCheckout(b *testing.B) {
	benchMounts(b, func(b *testing.B, h *harness) {
		for b.Loop() {
			root := b.TempDir()
			h.ok(root, "checkout", "big", filepath.Join(root, "w"))
		}
	})
}

func BenchmarkProjectLand(b *testing.B) {
	benchMounts(b, func(b *testing.B, h *harness) {
		root := b.TempDir()
		h.ok(root, "checkout", "big")
		w := filepath.Join(root, "big")
		for i := 0; b.Loop(); i++ {
			writeFile(b, filepath.Join(w, "d0", "f0000.txt"), fmt.Sprintf("edit %d\n", i))
			h.ok(w, "commit", "-m", fmt.Sprintf("edit %d", i), "--land")
		}
	})
}

func BenchmarkProjectLog(b *testing.B) {
	benchMounts(b, func(b *testing.B, h *harness) {
		var entries []json.RawMessage
		if err := json.Unmarshal(unwrap(b, h.ok(b.TempDir(), "log", "--project", "big", "-n", "100", "--json"), "commits"), &entries); err != nil || len(entries) != 100 {
			b.Fatalf("log -n 100: %d entries, %v", len(entries), err)
		}
		for b.Loop() {
			h.ok(b.TempDir(), "log", "--project", "big", "-n", "100")
		}
	})
}
