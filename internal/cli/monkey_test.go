//go:build monkey

package cli_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/testenv"
)

// TestMonkeyProjects drives random sg calls over two projects that share a
// repo and one that spans two repos. It asserts that no call crashes (exit
// codes stay within 0-4), that after a catch-up every mount's subtree in the
// project equals the source folder's subtree, and that `git fsck --strict`
// is clean on every source and project repo.
// Run: go test -tags monkey -run Monkey -count=1 ./internal/cli/ (SG_MONKEY_STEPS, SG_MONKEY_SEED).
func TestMonkeyProjects(t *testing.T) {
	steps, _ := strconv.Atoi(os.Getenv("SG_MONKEY_STEPS"))
	if steps == 0 {
		steps = 400
	}
	seed, _ := strconv.ParseUint(os.Getenv("SG_MONKEY_SEED"), 10, 64)
	if seed == 0 {
		seed = 1
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	h := newHarness(t)
	for _, r := range []string{"core", "docs"} {
		h.env.CreateRepo(r)
		h.env.CommitDirect(r, map[string]string{"src/seed.txt": "seed\n"}, "seed")
	}
	type mountT struct{ project, path, repo, folder string }
	mounts := []mountT{{"web", "lib", "core", "src"}, {"api", "code", "core", "src"}, {"api", "doc", "docs", "src"}}
	ids := map[string]int64{"web": h.env.CreateProject("web"), "api": h.env.CreateProject("api")}
	for _, m := range mounts {
		h.env.Mount(m.project, m.path, m.repo, m.folder, false)
	}
	root := t.TempDir()
	type wcT struct{ dir, mount string }
	var wcs []wcT
	for i, p := range []struct{ project, mount string }{{"web", "lib"}, {"web", "lib"}, {"api", "code"}, {"api", "doc"}} {
		dir := filepath.Join(root, fmt.Sprintf("w%d", i))
		h.ok(root, "checkout", p.project, dir)
		wcs = append(wcs, wcT{dir, p.mount})
	}
	// step runs one sg call; any exit code outside the documented 0-4 is a crash.
	step := func(dir string, args ...string) int {
		code, out, errs := h.run(dir, args...)
		if code < 0 || code > 4 {
			t.Fatalf("sg %v in %s: exit %d\n%s%s", args, dir, code, out, errs)
		}
		return code
	}
	codes := map[string]map[int]int{}
	count := func(name string, code int) {
		if codes[name] == nil {
			codes[name] = map[int]int{}
		}
		codes[name][code]++
	}
	for i := 0; i < steps; i++ {
		w := wcs[rng.IntN(len(wcs))]
		switch rng.IntN(6) {
		case 0, 1:
			name := fmt.Sprintf("f%d.txt", rng.IntN(20))
			writeFile(t, filepath.Join(w.dir, w.mount, name), fmt.Sprintf("step %d\n", i))
			count("edit", 0)
		case 2:
			count("commit --land", step(w.dir, "commit", "-m", fmt.Sprintf("step %d", i), "--land"))
		case 3:
			count("sync", step(w.dir, "sync"))
		case 4:
			count("log", step(w.dir, "log", "-n", "5"))
		case 5:
			count("status", step(w.dir, "status"))
		}
	}
	// Settle: every working copy syncs and reports a status (clean, or with
	// recorded conflicts) without crashing.
	for _, w := range wcs {
		count("sync", step(w.dir, "sync"))
		count("status", step(w.dir, "status", "--json"))
	}
	t.Logf("seed %d, %d steps, exit codes per call: %v", seed, steps, codes)

	ctx := t.Context()
	pc := h.env.Project(h.env.AdminToken)
	for _, m := range mounts {
		// ResolveRev catches the project up with its sources first.
		head := testenv.Must(pc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: m.project})).Sha
		pg := h.env.ProjectGit(ids[m.project])
		got := testenv.Must(pg.Subtrees(ctx, []string{head}, m.path))[0]
		pg.Close()
		sg := h.env.Git(m.repo)
		main := testenv.Must(sg.ReadRef(ctx, gitx.MainRef))
		want := testenv.Must(sg.Subtrees(ctx, []string{main}, m.folder))[0]
		sg.Close()
		if got == "" || got != want {
			t.Errorf("project %s %s/ = tree %q, source %s:%s = tree %q", m.project, m.path, got, m.repo, m.folder, want)
		}
	}
	for _, r := range []string{"core", "docs"} {
		g := h.env.Git(r)
		if _, err := g.Log(ctx, "main", "", 10000); err != nil {
			t.Fatalf("%s log: %v", r, err)
		}
		g.Close()
	}
	dirs := []string{filepath.Join(h.env.DataDir, "repos", "core.git"), filepath.Join(h.env.DataDir, "repos", "docs.git")}
	for _, id := range ids {
		dirs = append(dirs, filepath.Join(h.env.DataDir, "projects", strconv.FormatInt(id, 10)+".git"))
	}
	for _, d := range dirs {
		if out, err := exec.Command("git", "--git-dir="+d, "fsck", "--strict").CombinedOutput(); err != nil {
			t.Errorf("git fsck --strict %s: %v\n%s", d, err, out)
		}
	}
}
