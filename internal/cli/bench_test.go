package cli_test

import (
	"fmt"
	"path/filepath"
	"testing"
)

// benchRepo builds the 20,000-file layout from the README (20 dirs × 1,000
// files of about 2.7 KB) as repo "big" and project "big" mounting d0..d19
// as 20 folder mounts at the same paths.
func benchRepo(b *testing.B) *harness {
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
	for d := 0; d < 20; d++ {
		dir := fmt.Sprintf("d%d", d)
		h.env.Mount("big", dir, "big", dir, false)
	}
	return h
}

func BenchmarkProjectCheckout(b *testing.B) {
	h := benchRepo(b)
	for i := 0; b.Loop(); i++ {
		h.ok(b.TempDir(), "checkout", "big", filepath.Join(b.TempDir(), "w"))
	}
}

func BenchmarkProjectLand(b *testing.B) {
	h := benchRepo(b)
	root := b.TempDir()
	h.ok(root, "checkout", "big")
	w := filepath.Join(root, "big")
	for i := 0; b.Loop(); i++ {
		writeFile(b, filepath.Join(w, "d0", "f0000.txt"), fmt.Sprintf("edit %d\n", i))
		h.ok(w, "commit", "-m", fmt.Sprintf("edit %d", i), "--land")
	}
}

func BenchmarkProjectLog(b *testing.B) {
	h := benchRepo(b)
	for b.Loop() {
		h.ok(b.TempDir(), "log", "--project", "big", "-n", "100")
	}
}
