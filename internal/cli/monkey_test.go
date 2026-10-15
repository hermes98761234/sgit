//go:build monkey

package cli_test

import (
	"crypto/sha1"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/testenv"
)

// TestMonkeyProjects drives random sg calls over two projects that share a
// repo and one that spans two repos, then settles every working copy
// (conflicts resolved by overwriting, leftovers landed). It asserts:
//   - no call crashes (exit codes stay within 0-4);
//   - at least 10% of the random lands succeed;
//   - every file version acknowledged by a successful land is in the main
//     history of its source folder and of every project mounting it;
//   - after a catch-up, every mount's subtree in the project equals the
//     source folder's subtree;
//   - every working copy, synced, is clean, based on project main, and holds
//     exactly the files of project main;
//   - `git fsck --strict` is clean on every source and project repo.
//
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
	mountOf := func(project, path string) mountT {
		for _, m := range mounts {
			if m.project == project && m.path == path {
				return m
			}
		}
		t.Fatalf("no mount %s %s", project, path)
		return mountT{}
	}
	type wcT struct{ dir, project, mount string }
	var wcs []wcT
	for i, p := range []struct{ project, mount string }{{"web", "lib"}, {"web", "lib"}, {"api", "code"}, {"api", "doc"}} {
		dir := filepath.Join(root, fmt.Sprintf("w%d", i))
		h.ok(root, "checkout", p.project, dir)
		wcs = append(wcs, wcT{dir, p.project, p.mount})
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
	// land runs `sg commit --land` in w. On exit 0 it records the content of
	// every file in w's mount as acknowledged: each must reach the source
	// folder's history and every project view of that folder.
	type landed struct{ repo, folder, name, content string }
	var lands []landed
	land := func(w wcT, msg string) int {
		snap := map[string]string{}
		for _, e := range testenv.Must(os.ReadDir(filepath.Join(w.dir, w.mount))) {
			snap[e.Name()] = readFile(t, filepath.Join(w.dir, w.mount, e.Name()))
		}
		code := step(w.dir, "commit", "-m", msg, "--land")
		count("commit --land", code)
		if code == 0 {
			m := mountOf(w.project, w.mount)
			for name, content := range snap {
				lands = append(lands, landed{m.repo, m.folder, name, content})
			}
		}
		return code
	}
	for i := 0; i < steps; i++ {
		w := wcs[rng.IntN(len(wcs))]
		switch rng.IntN(6) {
		case 0, 1:
			name := fmt.Sprintf("f%d.txt", rng.IntN(20))
			writeFile(t, filepath.Join(w.dir, w.mount, name), fmt.Sprintf("step %d\n", i))
			count("edit", 0)
		case 2:
			land(w, fmt.Sprintf("step %d", i))
		case 3:
			count("sync", step(w.dir, "sync"))
		case 4:
			count("log", step(w.dir, "log", "-n", "5"))
		case 5:
			count("status", step(w.dir, "status"))
		}
	}
	monkeyLands := codes["commit --land"][0]
	tried := 0
	for _, n := range codes["commit --land"] {
		tried += n
	}
	// Settle: each working copy syncs, resolves its conflicts by overwriting
	// the conflicted files, and lands what is left, until a sync finds it
	// clean.
	for wi, w := range wcs {
		for round := 0; ; round++ {
			if round == 20 {
				t.Errorf("%s did not settle in 20 rounds", w.dir)
				break
			}
			code := step(w.dir, "sync")
			count("sync", code)
			s, changes := h.status(w.dir)
			for _, p := range s.Conflicts {
				writeFile(t, filepath.Join(w.dir, p), fmt.Sprintf("settled w%d round %d\n", wi, round))
			}
			if len(s.Conflicts) == 0 && len(changes) == 0 {
				if code == 0 {
					break
				}
				continue
			}
			land(w, fmt.Sprintf("settle w%d round %d", wi, round))
		}
	}
	t.Logf("seed %d, %d steps, exit codes per call: %v; %d of %d monkey lands succeeded", seed, steps, codes, monkeyLands, tried)
	if tried == 0 || monkeyLands*10 < tried {
		t.Errorf("only %d of %d monkey lands succeeded (want at least 10%%)", monkeyLands, tried)
	}

	ctx := t.Context()
	pc := h.env.Project(h.env.AdminToken)
	heads := map[string]string{}
	for _, m := range mounts {
		// ResolveRev catches the project up with its sources first.
		head := testenv.Must(pc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: m.project})).Sha
		heads[m.project] = head
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
	// No acknowledged land is lost: its content is in the source folder's
	// main history and in the main history of every project mounting it.
	srcHist := map[string]map[string]bool{}
	for _, r := range []string{"core", "docs"} {
		srcHist[r] = blobHistory(t, filepath.Join(h.env.DataDir, "repos", r+".git"))
	}
	projHist := map[string]map[string]bool{}
	for p, id := range ids {
		projHist[p] = blobHistory(t, filepath.Join(h.env.DataDir, "projects", strconv.FormatInt(id, 10)+".git"))
	}
	lost := 0
	for _, l := range lands {
		blob := blobSHA(l.content)
		where := []string{}
		if !srcHist[l.repo][l.folder+"/"+l.name+" "+blob] {
			where = append(where, l.repo+":"+l.folder)
		}
		for _, m := range mounts {
			if m.repo == l.repo && m.folder == l.folder && !projHist[m.project][m.path+"/"+l.name+" "+blob] {
				where = append(where, "project "+m.project+" "+m.path+"/")
			}
		}
		if len(where) > 0 {
			if lost++; lost <= 10 {
				t.Errorf("landed %s/%s = %q missing from the main history of %v", l.folder, l.name, l.content, where)
			}
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d acknowledged file versions lost", lost, len(lands))
	}
	// Every working copy, synced once more, is clean and equals project main.
	for _, w := range wcs {
		if code := step(w.dir, "sync"); code != 0 {
			t.Errorf("final sync in %s: exit %d", w.dir, code)
			continue
		}
		s, changes := h.status(w.dir)
		if s.Base != heads[w.project] || len(changes) != 0 || len(s.Conflicts) != 0 {
			t.Errorf("%s: base %s (project main %s), changes %v, conflicts %v", w.dir, s.Base, heads[w.project], changes, s.Conflicts)
		}
		if got, want := wcFiles(t, w.dir), h.env.ProjectFiles(w.project, heads[w.project]); !maps.Equal(got, want) {
			t.Errorf("%s files differ from project %s main:\n got  %v\n want %v", w.dir, w.project, got, want)
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

// blobHistory returns "path blob" for every file version that a commit on
// main's history in gitDir introduced.
func blobHistory(t *testing.T, gitDir string) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+gitDir, "log", "--raw", "--no-abbrev", "--format=", "-m", "main").Output()
	if err != nil {
		t.Fatalf("git log %s: %v", gitDir, err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if f := strings.Fields(meta); ok && len(f) == 5 && strings.HasPrefix(meta, ":") {
			set[path+" "+f[3]] = true
		}
	}
	return set
}

// blobSHA is the git object id of a blob with content.
func blobSHA(content string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content))))
}

// wcFiles returns path -> content of every file in a working copy, .sg excluded.
func wcFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel == ".sg" {
				return filepath.SkipDir
			}
			return nil
		}
		files[filepath.ToSlash(rel)] = readFile(t, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
