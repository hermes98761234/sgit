//go:build monkey

package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/store"
	"github.com/hermes98761234/sgit/internal/testenv"
)

const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// TestMonkeyMigrate drives random `sg migrate in/out` calls, mixed with
// lands, external commits, deletes, unmounts and aliases, over a pool of
// local source repos, monorepos and push targets. It asserts:
//   - no call crashes (exit codes stay within 0-4, no panic);
//   - a successful migrate in holds exactly the source main tree at the
//     reported place, the project mirrors the mounted subtree, and the commit
//     count is the source's first-parent count;
//   - a failed migrate in leaves no new repo, project, repo dir, .incoming
//     entry or refs/sgit/migrate/* ref, and moves no main (unless it says
//     the code landed);
//   - a successful migrate out leaves target main == project main; "up to
//     date" means it already was; a refused push leaves the target alone;
//   - every acknowledged land is in its project's and its source's main
//     history;
//   - `git fsck --strict` is clean everywhere at the end.
//
// Run: go test -tags monkey -run TestMonkeyMigrate -count=1 ./internal/cli/ (SG_MONKEY_STEPS, SG_MONKEY_SEED).
func TestMonkeyMigrate(t *testing.T) {
	steps, _ := strconv.Atoi(os.Getenv("SG_MONKEY_STEPS"))
	if steps == 0 {
		steps = 300
	}
	seed, _ := strconv.ParseUint(os.Getenv("SG_MONKEY_SEED"), 10, 64)
	if seed == 0 {
		seed = uint64(time.Now().UnixNano() % 1000000)
	}
	t.Logf("SG_MONKEY_SEED=%d SG_MONKEY_STEPS=%d", seed, steps)
	rng := rand.New(rand.NewPCG(seed, seed))
	ctx := context.Background()
	h := newHarness(t, func(c *server.Config) { c.AllowLocalRemotes = true })
	data := h.env.DataDir
	root := t.TempDir()
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }

	var oplog []string
	stats := map[string]int{}
	failures := 0
	fail := func(format string, args ...any) {
		t.Helper()
		failures++
		if failures <= 15 {
			t.Errorf("seed %d, op %d: %s", seed, len(oplog)-1, fmt.Sprintf(format, args...))
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			start := max(0, len(oplog)-60)
			if os.Getenv("SG_MONKEY_FULLLOG") != "" {
				start = 0
			}
			t.Logf("seed %d, op log (last %d of %d):\n%s", seed, len(oplog)-start, len(oplog), strings.Join(oplog[start:], "\n"))
		}
	})
	logop := func(format string, args ...any) {
		oplog = append(oplog, fmt.Sprintf("%4d ", len(oplog))+fmt.Sprintf(format, args...))
	}

	codes := map[string]map[int]int{}
	errMsgs := map[string]int{}
	numRE := regexp.MustCompile(`[0-9a-f]{7,40}|/tmp/[^ ")]*|#[0-9]+`)
	// sg runs one call as user; exits outside 0-4 and panics are crashes.
	sg := func(user, dir string, args ...string) (code int, out, errs string) {
		t.Helper()
		func() {
			defer func() {
				if r := recover(); r != nil {
					oplog = append(oplog, fmt.Sprintf("PANIC in sg %q: %v", args, r))
					t.Fatalf("seed %d: sg %q panicked: %v", seed, args, r)
				}
			}()
			code, out, errs = h.runAs(user, dir, args...)
		}()
		name := strings.Join(args[:min(2, len(args))], " ")
		if codes[name] == nil {
			codes[name] = map[int]int{}
		}
		codes[name][code]++
		logop("[%s] sg %q -> %d %s", user, args, code, strings.TrimSpace(firstLine(errs)))
		if code != 0 {
			errMsgs[name+": "+numRE.ReplaceAllString(firstLine(errs), "_")]++
		}
		if code < 0 || code > 4 {
			t.Fatalf("seed %d: sg %q: exit %d\n%s%s", seed, args, code, out, errs)
		}
		return
	}

	// ---- external source repos ----
	type srcT struct {
		dir, url string
		n        int // commits made, for unique content
	}
	sig := gitx.Signature{Name: "Ann", Email: "ann@example.com", When: time.Unix(1700000000, 0)}
	tick := func() gitx.Signature { sig.When = sig.When.Add(time.Minute); return sig }
	var srcs []*srcT
	pathParts := []string{"src", "lib", "ü", "日本", "a b", "deep/er/x", "docs", "Ωmega"}
	fileNames := []string{"a.txt", "b.go", "ñ.md", "c d.txt", "README", "x.y.z", "日本語.txt"}
	// srcChanges returns random adds/modifies/deletes on top of commit base.
	srcChanges := func(r *gitx.Repo, base string, tag string) []gitx.Change {
		files := testenv.Must(r.ListFiles(ctx, base))
		var ch []gitx.Change
		seen := map[string]bool{}
		for k := 1 + rng.IntN(4); k > 0; k-- {
			var p string
			existing := slices.Sorted(maps.Keys(files))
			if len(existing) > 0 && rng.IntN(3) == 0 {
				p = pick(existing)
				if seen[p] {
					continue
				}
				seen[p] = true
				if rng.IntN(3) == 0 && len(files) > 1 {
					ch = append(ch, gitx.Change{Path: p, Op: gitx.OpDelete})
				} else {
					ch = append(ch, gitx.Change{Path: p, Op: gitx.OpModify, BlobSHA: testenv.Must(r.WriteBlob(ctx, []byte(tag+" "+p+"\n")))})
				}
				continue
			}
			if rng.IntN(4) == 0 {
				p = pick(fileNames)
			} else {
				p = pick(pathParts) + "/" + pick(fileNames)
			}
			if _, ok := files[p]; ok || seen[p] || conflictsWithTree(files, p) || conflictsWithChanges(ch, p) {
				continue
			}
			seen[p] = true
			ch = append(ch, gitx.Change{Path: p, Op: gitx.OpAdd, BlobSHA: testenv.Must(r.WriteBlob(ctx, []byte(tag+" "+p+"\n")))})
		}
		return ch
	}
	srcCommit := func(s *srcT, merge bool) {
		r := gitx.Open(s.dir)
		defer r.Close()
		main := testenv.Must(r.ReadRef(ctx, gitx.MainRef))
		s.n++
		parents := []string{main}
		base := main
		if merge {
			// a side branch off main, then a merge commit with the side as 2nd parent
			side := testenv.Must(r.CommitTree(ctx, testenv.Must(r.BuildTree(ctx, main, srcChanges(r, main, fmt.Sprintf("side %d", s.n)))),
				[]string{main}, fmt.Sprintf("side %d\n", s.n), tick(), tick()))
			parents = append(parents, side)
			base = side
		}
		ch := srcChanges(r, base, fmt.Sprintf("c%d", s.n))
		tree := testenv.Must(r.BuildTree(ctx, base, ch))
		msg := fmt.Sprintf("commit %d ü\n", s.n)
		if merge {
			msg = fmt.Sprintf("merge %d\n", s.n)
		}
		c := testenv.Must(r.CommitTree(ctx, tree, parents, msg, tick(), tick()))
		if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		dir := filepath.Join(root, "src", fmt.Sprintf("s%d", i), []string{"app.git", "lib", "svc.git", "core"}[i])
		testenv.Must(gitx.Init(ctx, dir, tick())).Close()
		s := &srcT{dir: dir, url: "file://" + dir}
		srcs = append(srcs, s)
		for k := rng.IntN(5); k > 0; k-- {
			srcCommit(s, rng.IntN(5) == 0)
		}
	}
	noMain := filepath.Join(root, "src", "nomain.git")
	if err := gitx.InitBare(ctx, noMain); err != nil {
		t.Fatal(err)
	}
	notGit := filepath.Join(root, "src", "notgit")
	writeFile(t, filepath.Join(notGit, "f"), "x")
	badURLs := []string{"file://" + noMain, "file://" + notGit, "file://" + filepath.Join(root, "missing.git"),
		"https://u:p@example.com/x.git", "ftp://example.com/x.git", "", "-x", "file://"}
	var targets []string
	srcURL := func() string {
		if rng.IntN(8) == 0 {
			return pick(badURLs)
		}
		if rng.IntN(6) == 0 {
			return "file://" + pick(targets) // round trip: a migrate out target
		}
		return srcs[rng.IntN(len(srcs))].url
	}
	srcByURL := func(u string) *srcT {
		for _, s := range srcs {
			if s.url == u {
				return s
			}
		}
		for _, d := range targets {
			if "file://"+d == u {
				return &srcT{dir: d, url: u}
			}
		}
		return nil
	}

	// ---- push targets ----
	for i := 0; i < 3; i++ {
		d := filepath.Join(root, "tgt", fmt.Sprintf("t%d.git", i))
		if err := gitx.InitBare(ctx, d); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, d)
	}
	remoteMain := func(dir string) string {
		out, err := exec.Command("git", "--git-dir="+dir, "rev-parse", "--verify", "-q", "refs/heads/main").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	// ---- server state ----
	h.ok(root, "repo", "create", "mono")
	h.env.CommitDirect("mono", map[string]string{"README": "mono\n", "svc/keep.txt": "keep\n"}, "seed mono")
	if rng.IntN(2) == 0 {
		h.ok(root, "repo", "create", "mono2")
	}
	bobTok := h.env.NewUser("bob", "", "")
	h.loginAs("bob", bobTok)

	repoGen := map[string]int{} // bumped when a repo with the name is deleted
	names := []string{"app", "lib", "svc", "core", "web", "x1", "mono", "mono2"}
	badNames := []string{"Bad", "-x", "a b", "..", ".git", "日本", strings.Repeat("a", 65), "a/b"}
	folders := []string{"svc", "svc/app", "a", "deep/er/path", "ü/x", "lib/two", "svc/keep.txt", "README", "x", "README/x", "svc/keep.txt/y"}
	badFolders := []string{".", "/abs", "a/../b", "a//b", "trail/", ".git/x", "GIT~1", "a\x00b", ".sg", ""}
	mountPaths := []string{"vendor", "lib", "ext/a", "ü", "x", "app"}

	repoNames := func() []string {
		var out []string
		for _, r := range testenv.Must(h.env.Store.ListRepos(ctx)) {
			out = append(out, r.Name)
		}
		return out
	}
	type projT struct {
		store.Project
		mounts []store.Mount
	}
	projects := func() []projT {
		var out []projT
		for _, p := range testenv.Must(h.env.Store.ListProjects(ctx)) {
			out = append(out, projT{p, testenv.Must(h.env.Store.Mounts(ctx, p.ID))})
		}
		return out
	}
	repoDir := func(r string) string { return filepath.Join(data, "repos", r+".git") }
	projDir := func(id int64) string { return filepath.Join(data, "projects", strconv.FormatInt(id, 10)+".git") }
	dirList := func(d string) []string {
		var out []string
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	type snapT struct {
		repos, repoDirs, incoming, projects []string
		mains                               map[string]string
		migrateRefs                         string
	}
	snapshot := func() snapT {
		s := snapT{repos: repoNames(), incoming: dirList(filepath.Join(data, "repos", ".incoming")), mains: map[string]string{}}
		for _, d := range dirList(filepath.Join(data, "repos")) {
			if d != ".incoming" {
				s.repoDirs = append(s.repoDirs, d)
			}
		}
		for _, p := range projects() {
			s.projects = append(s.projects, fmt.Sprintf("%d:%s", p.ID, p.Name))
		}
		for _, r := range s.repos {
			s.mains[r] = remoteMain(repoDir(r))
			out, err := exec.Command("git", "--git-dir="+repoDir(r), "for-each-ref", "refs/sgit/migrate/").Output()
			if err != nil {
				t.Fatalf("for-each-ref %s: %v", r, err)
			}
			s.migrateRefs += string(out)
		}
		return s
	}
	pc := h.env.Project(h.env.AdminToken)
	// catchUp returns the project's main after a catch-up with its sources.
	catchUp := func(project string) string {
		resp, err := pc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: project})
		if err != nil {
			return ""
		}
		return resp.Sha
	}
	subtree := func(gitDir, rev, folder string) string {
		if rev == "" {
			return ""
		}
		g := gitx.Open(gitDir)
		defer g.Close()
		st := testenv.Must(g.Subtrees(ctx, []string{rev}, folder))[0]
		if st == emptyTreeSHA {
			return ""
		}
		return st
	}
	// checkProject asserts every mount's subtree in project main equals its
	// source folder at the source's main.
	checkProject := func(p projT, why string) {
		if len(p.mounts) == 0 {
			return
		}
		head := catchUp("#" + strconv.FormatInt(p.ID, 10))
		if head == "" {
			fail("%s: project %s has mounts %v but no head", why, p.Name, p.mounts)
			return
		}
		for _, m := range p.mounts {
			got := subtree(projDir(p.ID), head, m.Path)
			want := subtree(repoDir(m.Repo), remoteMain(repoDir(m.Repo)), m.Folder)
			if got != want {
				fail("%s: project %s %s/ = tree %q, source %s:%s = %q", why, p.Name, m.Path, got, m.Repo, m.Folder, want)
			}
		}
	}

	type landT struct {
		projectID      int64
		projPath       string // path in the project
		repo, repoPath string
		gen            int
		content        string
	}
	var lands []landT
	deadProjects := map[int64]bool{}

	// ---- operations ----
	opMigrateIn := func(i int) {
		u := srcURL()
		var groups [][]string
		var name string
		explicitName := rng.IntN(4) != 0
		if explicitName {
			switch r := rng.IntN(8); {
			case r == 0:
				name = pick(badNames)
			case r < 4:
				name = fmt.Sprintf("m%d", i) // fresh
			default:
				name = pick(names)
			}
			groups = append(groups, []string{name})
		}
		form := rng.IntN(3) // 0 plain, 1 --into, 2 --project/--path
		var intoRepo, intoFolder, projArg, pathArg string
		switch form {
		case 1:
			intoRepo = pick(append(repoNames(), "nope"))
			if rng.IntN(8) == 0 {
				intoFolder = pick(badFolders)
			} else {
				intoFolder = pick(folders)
			}
			groups = append(groups, []string{"--into", intoRepo + ":" + intoFolder})
			intoFolder = strings.TrimRight(intoFolder, "/") // sg drops trailing slashes
		}
		if form == 2 || (form == 1 && rng.IntN(3) == 0) {
			ps := projects()
			cands := []string{"nope"}
			for _, p := range ps {
				cands = append(cands, p.Name)
				cands = append(cands, p.Aliases...)
			}
			projArg = pick(cands)
			if rng.IntN(8) == 0 {
				pathArg = pick(append(badFolders, "."))
			} else {
				pathArg = pick(mountPaths)
				if rng.IntN(2) == 0 {
					pathArg = fmt.Sprintf("v/p%d", i)
				}
			}
			switch rng.IntN(12) {
			case 0:
				groups = append(groups, []string{"--project", projArg})
			case 1:
				groups = append(groups, []string{"--path", pathArg})
			default:
				groups = append(groups, []string{"--project", projArg}, []string{"--path", pathArg})
			}
		}
		if rng.IntN(15) == 0 {
			groups = append(groups, []string{"--mirror", "file://" + filepath.Join(root, "mirror.git")})
		}
		if rng.IntN(20) == 0 {
			g := pick([]string{"--bogus", "extra", "--into", "--force"})
			groups = append(groups, []string{g})
			if g == "extra" && !explicitName {
				explicitName, name = true, "extra"
			}
		}
		asJSON := rng.IntN(5) != 0
		if asJSON {
			groups = append(groups, []string{"--json"})
		}
		rng.Shuffle(len(groups), func(a, b int) { groups[a], groups[b] = groups[b], groups[a] })
		args := []string{"migrate", "in", u}
		for _, g := range groups {
			args = append(args, g...)
		}
		src := srcByURL(u)
		var srcMain, srcTree string
		var srcFP int
		var srcRel, srcOther []string // release/* (name, sha pairs); other non-main branches
		if src != nil {
			srcMain = remoteMain(src.dir)
		}
		if srcMain == "" {
			src = nil // a target without main: the import must fail
		}
		if src != nil {
			g := gitx.Open(src.dir)
			srcTree = testenv.Must(g.ReadCommit(ctx, srcMain)).Tree
			if srcTree == emptyTreeSHA {
				srcTree = ""
			}
			srcFP = len(testenv.Must(g.FirstParents(ctx, srcMain, "")))
			g.Close()
			out, _ := exec.Command("git", "--git-dir="+src.dir, "for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads/release/").Output()
			srcRel = strings.Fields(string(out))
			out, _ = exec.Command("git", "--git-dir="+src.dir, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads/").Output()
			for _, b := range strings.Fields(string(out)) {
				if b != "main" && !strings.HasPrefix(b, "release/") {
					srcOther = append(srcOther, b)
				}
			}
		}
		before := snapshot()
		code, out, errs := sg("admin", root, args...)
		after := snapshot()
		if code != 0 {
			if !slices.Equal(before.repos, after.repos) {
				fail("failed migrate in %q changed repos %v -> %v (%s)", args, before.repos, after.repos, errs)
			}
			if !slices.Equal(before.repoDirs, after.repoDirs) {
				fail("failed migrate in %q changed repo dirs %v -> %v (%s)", args, before.repoDirs, after.repoDirs, errs)
			}
			if len(after.incoming) > 0 {
				fail("failed migrate in %q left .incoming entries %v", args, after.incoming)
			}
			if !slices.Equal(before.projects, after.projects) {
				fail("failed migrate in %q changed projects %v -> %v (%s)", args, before.projects, after.projects, errs)
			}
			if after.migrateRefs != "" {
				fail("failed migrate in %q left refs: %s", args, after.migrateRefs)
			}
			landed := strings.Contains(errs, "the code landed at")
			for r, m := range before.mains {
				if after.mains[r] != m && !(landed && r == intoRepo) {
					fail("failed migrate in %q moved main of %s (%s)", args, r, errs)
				}
			}
			return
		}
		stats[fmt.Sprintf("migrate in ok: into=%v project=%v", intoRepo != "", projArg != "")]++
		if src == nil {
			fail("migrate in from bad url %q succeeded: %s", u, out)
			return
		}
		var resp struct {
			Repo, Folder, Project, Path, Main string
			Commits                           int
			Skipped                           []string `json:"skipped_branches"`
			ProjectHead                       string   `json:"project_head"`
		}
		if asJSON {
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				fail("migrate in --json output %q: %v", out, err)
				return
			}
		} else {
			m := regexp.MustCompile(`^migrated (\S+) into (\S+): (\d+) commits?, main ([0-9a-f]+)\nmounted at (.+) of project (\S+)\n`).FindStringSubmatch(out)
			if m == nil {
				fail("migrate in text output %q", out)
				return
			}
			resp.Commits, _ = strconv.Atoi(m[3])
			resp.Project = m[6]
			resp.Repo, resp.Folder, _ = strings.Cut(m[2], ":")
			if resp.Folder == "" {
				resp.Folder = "."
			}
			resp.Main = remoteMain(repoDir(resp.Repo))
		}
		if !explicitName {
			name = strings.TrimSuffix(filepath.Base(src.dir), ".git")
		}
		if resp.Commits != srcFP {
			fail("migrate in %q reported %d commits, source has %d first parents", args, resp.Commits, srcFP)
		}
		if intoRepo == "" {
			if resp.Repo != name || resp.Folder != "." || resp.Main != srcMain {
				fail("plain migrate in %q: repo %q folder %q main %s, want %s . %s", args, resp.Repo, resp.Folder, resp.Main, name, srcMain)
			}
			for k := 0; k+1 < len(srcRel); k += 2 {
				out, _ := exec.Command("git", "--git-dir="+repoDir(name), "rev-parse", "-q", "--verify", "refs/heads/"+srcRel[k]).Output()
				if strings.TrimSpace(string(out)) != srcRel[k+1] {
					fail("plain migrate in %q: branch %s = %q, source %s", args, srcRel[k], strings.TrimSpace(string(out)), srcRel[k+1])
				}
			}
			if asJSON && !slices.Equal(resp.Skipped, srcOther) {
				fail("plain migrate in %q: skipped %v, want %v", args, resp.Skipped, srcOther)
			}
		} else {
			if resp.Repo != intoRepo || resp.Folder != intoFolder {
				fail("migrate in %q: repo %q folder %q", args, resp.Repo, resp.Folder)
			}
			base := before.mains[intoRepo]
			g := h.env.Git(intoRepo)
			fps := testenv.Must(g.FirstParents(ctx, resp.Main, base))
			if len(fps) != srcFP {
				fail("migrate in %q: %s gained %d commits on top of %s, want %d", args, intoRepo, len(fps), base, srcFP)
			}
			newFiles := testenv.Must(g.ListFiles(ctx, resp.Main))
			baseFiles := testenv.Must(g.ListFiles(ctx, base))
			g.Close()
			for p := range newFiles {
				if strings.HasPrefix(p, intoFolder+"/") {
					delete(newFiles, p)
				}
			}
			if !maps.Equal(newFiles, baseFiles) {
				fail("migrate in %q changed files outside %s: %v -> %v", args, intoFolder, baseFiles, newFiles)
			}
			if asJSON {
				want := slices.Clone(srcOther) // --into skips every branch but main
				for k := 0; k < len(srcRel); k += 2 {
					want = append(want, srcRel[k])
				}
				sort.Strings(want)
				if !slices.Equal(want, resp.Skipped) {
					fail("migrate in %q: skipped %v, source has %v", args, resp.Skipped, want)
				}
			}
		}
		if got := subtree(repoDir(resp.Repo), resp.Main, resp.Folder); got != srcTree {
			fail("migrate in %q: %s:%s at %s = tree %q, source main tree %q", args, resp.Repo, resp.Folder, resp.Main, got, srcTree)
		}
		if cur := remoteMain(repoDir(resp.Repo)); cur != resp.Main {
			fail("migrate in %q: reported main %s, repo main is %s", args, resp.Main, cur)
		}
		if asJSON && resp.ProjectHead == "" {
			fail("migrate in %q: empty project_head", args)
		}
		if projArg == "" && resp.Project != name {
			fail("migrate in %q: project %q, want %q", args, resp.Project, name)
		}
		for _, p := range projects() {
			if p.Name == resp.Project {
				found := false
				for _, m := range p.mounts {
					if m.Repo == resp.Repo && m.Folder == resp.Folder {
						found = true
					}
				}
				if !found {
					fail("migrate in %q: project %s has no mount of %s:%s: %v", args, p.Name, resp.Repo, resp.Folder, p.mounts)
				}
				checkProject(p, fmt.Sprintf("after migrate in %q", args))
				return
			}
		}
		fail("migrate in %q: project %q not found", args, resp.Project)
	}

	opLand := func(i int) {
		var cands []projT
		for _, p := range projects() {
			if len(p.mounts) > 0 {
				cands = append(cands, p)
			}
		}
		if len(cands) == 0 {
			return
		}
		p := cands[rng.IntN(len(cands))]
		user := "admin"
		if rng.IntN(2) == 0 {
			user = "bob"
			if rng.IntN(3) != 0 {
				sg("admin", root, "acl", "grant", "bob", p.Name, "write")
			}
		}
		wc := filepath.Join(root, "wc", strconv.Itoa(i))
		if code, _, _ := sg(user, root, "checkout", p.Name, wc); code != 0 {
			return
		}
		m := p.mounts[rng.IntN(len(p.mounts))]
		rel := pick([]string{"land", "ü/land", "src/land", "x"}) + fmt.Sprintf("%d.txt", i)
		content := fmt.Sprintf("land %d by %s\n", i, user)
		pp := rel
		if m.Path != "." {
			pp = m.Path + "/" + rel
		}
		if st, err := os.Stat(filepath.Dir(filepath.Join(wc, pp))); err == nil && !st.IsDir() {
			return
		}
		files := wcFiles(t, wc)
		writeFile(t, filepath.Join(wc, pp), content)
		if rng.IntN(3) == 0 {
			// also edit an existing file in the same mount
			for _, f := range slices.Sorted(maps.Keys(files)) {
				if m.Path == "." || strings.HasPrefix(f, m.Path+"/") {
					writeFile(t, filepath.Join(wc, f), files[f]+"edit "+strconv.Itoa(i)+"\n")
					break
				}
			}
		}
		code, lout, _ := sg(user, wc, "commit", "-m", fmt.Sprintf("land %d", i), "--land")
		if code != 0 {
			return
		}
		logop("     wrote %s in mount %s (%s:%s): %s", pp, m.Path, m.Repo, m.Folder, strings.ReplaceAll(strings.TrimSpace(lout), "\n", " | "))
		rp := rel
		if m.Folder != "." {
			rp = m.Folder + "/" + rel
		}
		l := landT{p.ID, pp, m.Repo, rp, repoGen[m.Repo], content}
		lands = append(lands, l)
		checkLand(t, fail, l.projectID, projDir(p.ID), l.projPath, repoDir(m.Repo), l.repoPath, content, catchUp)
	}

	opSrcCommit := func(i int) {
		s := srcs[rng.IntN(len(srcs))]
		switch rng.IntN(6) {
		case 0:
			srcCommit(s, true)
			logop("source %s: merge commit", s.dir)
		case 1:
			r := gitx.Open(s.dir)
			main := testenv.Must(r.ReadRef(ctx, gitx.MainRef))
			fps := testenv.Must(r.FirstParents(ctx, main, ""))
			at := fps[rng.IntN(len(fps))].SHA
			name := fmt.Sprintf("refs/heads/release/%d", rng.IntN(3))
			old, _ := r.ReadRef(ctx, name)
			if err := r.UpdateRef(ctx, name, at, old); err != nil {
				t.Fatal(err)
			}
			r.Close()
			logop("source %s: %s = %s", s.dir, name, at)
		default:
			srcCommit(s, false)
			logop("source %s: commit", s.dir)
		}
	}

	opServerDirect := func(i int) {
		var cands []store.Mount
		for _, p := range projects() {
			cands = append(cands, p.mounts...)
		}
		var repo, folder string
		if len(cands) > 0 && rng.IntN(4) != 0 {
			m := cands[rng.IntN(len(cands))]
			repo, folder = m.Repo, m.Folder
		} else {
			rs := repoNames()
			if len(rs) == 0 {
				return
			}
			repo, folder = pick(rs), pick(append(folders, "."))
		}
		if remoteMain(repoDir(repo)) == "" {
			return
		}
		p := fmt.Sprintf("ext%d.txt", i)
		if folder != "." {
			p = folder + "/" + p
		}
		g := h.env.Git(repo)
		files := testenv.Must(g.ListFiles(ctx, "main"))
		g.Close()
		if conflictsWithTree(files, p) {
			return
		}
		h.env.CommitDirect(repo, map[string]string{p: fmt.Sprintf("direct %d\n", i)}, fmt.Sprintf("direct %d\n", i))
		logop("server repo %s: direct commit %s", repo, p)
	}

	opMigrateOut := func(i int) {
		ps := projects()
		cands := []string{"nope", "#99999", ""}
		for _, p := range ps {
			cands = append(cands, p.Name)
			cands = append(cands, p.Aliases...)
			cands = append(cands, "#"+strconv.FormatInt(p.ID, 10))
		}
		proj := pick(cands)
		tdir := pick(targets)
		if rng.IntN(8) == 0 {
			tdir = srcs[rng.IntN(len(srcs))].dir // back to an original source, as after a migration
		}
		to := "file://" + tdir
		if rng.IntN(10) == 0 {
			to = pick(badURLs[1:]) // badURLs[0] has no main: a fine push target
		}
		args := []string{"migrate", "out", proj, "--to", to}
		force := rng.IntN(3) == 0
		if force {
			args = append(args, "--force")
		}
		asJSON := rng.IntN(4) != 0
		if asJSON {
			args = append(args, "--json")
		}
		if rng.IntN(25) == 0 {
			args = append(args, pick([]string{"extra", "--into", "--bogus"}))
		}
		r0 := remoteMain(tdir)
		code, out, errs := sg("admin", root, args...)
		r1 := remoteMain(tdir)
		if to != "file://"+tdir {
			if code == 0 && to != "" {
				fail("migrate out to bad url %q succeeded: %s", to, out)
			}
			return
		}
		if code != 0 {
			if r1 != r0 {
				fail("failed migrate out %q moved target main %s -> %s (%s)", args, r0, r1, errs)
			}
			return
		}
		head := catchUp(proj)
		stats[fmt.Sprintf("migrate out ok: force=%v r0empty=%v moved=%v", force, r0 == "", r0 != r1)]++
		var resp struct {
			Project, Pushed string
			UpToDate        bool `json:"up_to_date"`
		}
		if asJSON {
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				fail("migrate out --json output %q: %v", out, err)
				return
			}
		} else {
			resp.Pushed = r1
			resp.UpToDate = strings.Contains(out, "is up to date with project")
			if !resp.UpToDate && !strings.HasPrefix(out, "pushed project ") {
				fail("migrate out text %q", out)
			}
		}
		if r1 != head || resp.Pushed != head {
			fail("migrate out %q: target main %s, pushed %s, project main %s", args, r1, resp.Pushed, head)
		}
		if resp.UpToDate && r0 != r1 {
			fail("migrate out %q said up to date, but target moved %s -> %s", args, r0, r1)
		}
		if !resp.UpToDate && r0 == r1 {
			fail("migrate out %q pushed but target was already %s", args, r0)
		}
		if !force && r0 != "" && r0 != r1 {
			if err := exec.Command("git", "--git-dir="+tdir, "merge-base", "--is-ancestor", r0, r1).Run(); err != nil {
				fail("migrate out %q without --force rewrote target %s -> %s", args, r0, r1)
			}
		}
	}

	opTargetEdit := func(i int) {
		d := targets[rng.IntN(len(targets))]
		r := gitx.Open(d)
		defer r.Close()
		main, _ := r.ReadRef(ctx, gitx.MainRef)
		switch {
		case main == "":
			return
		case rng.IntN(3) == 0:
			// rewind main to an ancestor: a later push fast-forwards
			fps := testenv.Must(r.FirstParents(ctx, main, ""))
			at := fps[rng.IntN(len(fps))].SHA
			if err := r.UpdateRef(ctx, gitx.MainRef, at, main); err != nil {
				t.Fatal(err)
			}
			logop("target %s: rewound to %s", d, at)
		case rng.IntN(5) == 0:
			if err := r.DeleteRef(ctx, gitx.MainRef); err != nil {
				t.Fatal(err)
			}
			logop("target %s: deleted main", d)
		default:
			tree := testenv.Must(r.BuildTree(ctx, main, []gitx.Change{{Path: fmt.Sprintf("theirs%d.txt", i), Op: gitx.OpAdd,
				BlobSHA: testenv.Must(r.WriteBlob(ctx, []byte("theirs\n")))}}))
			c := testenv.Must(r.CommitTree(ctx, tree, []string{main}, "theirs\n", tick(), tick()))
			if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
				t.Fatal(err)
			}
			logop("target %s: diverging commit %s", d, c)
		}
	}

	opAdmin := func(i int) {
		ps := projects()
		var pnames []string
		for _, p := range ps {
			pnames = append(pnames, p.Name)
			pnames = append(pnames, p.Aliases...)
		}
		pnames = append(pnames, "nope")
		switch rng.IntN(5) {
		case 0: // project delete
			pn := pick(pnames)
			p, err := h.env.Store.ResolveProject(ctx, pn)
			if code, _, _ := sg("admin", root, "project", "delete", pn, "--yes"); code == 0 && err == nil {
				deadProjects[p.ID] = true
				if _, err := os.Stat(projDir(p.ID)); err == nil {
					fail("project delete %s left %s", pn, projDir(p.ID))
				}
			}
		case 1: // repo delete, sometimes after deleting every project using it
			rn := pick(append(repoNames(), "nope"))
			if rng.IntN(2) == 0 {
				users, _ := h.env.Store.ProjectsUsing(ctx, rn)
				for _, pn := range users {
					p, err := h.env.Store.ResolveProject(ctx, pn)
					if code, _, _ := sg("admin", root, "project", "delete", pn, "--yes"); code == 0 && err == nil {
						deadProjects[p.ID] = true
					}
				}
			}
			if code, _, _ := sg("admin", root, "repo", "delete", rn, "--yes"); code == 0 {
				repoGen[rn]++
				if _, err := os.Stat(repoDir(rn)); err == nil {
					fail("repo delete %s left %s", rn, repoDir(rn))
				}
			}
		case 2: // unmount
			if len(ps) == 0 {
				return
			}
			p := ps[rng.IntN(len(ps))]
			path := pick(mountPaths)
			if len(p.mounts) > 0 && rng.IntN(4) != 0 {
				path = p.mounts[rng.IntN(len(p.mounts))].Path
			}
			sg("admin", root, "project", "unmount", p.Name, path)
		case 3: // alias
			sg("admin", root, "project", "alias", "add", pick(pnames), pick(append(names, badNames...)))
		case 4: // plain project/repo create taking names
			if rng.IntN(2) == 0 {
				sg("admin", root, "project", "create", pick(names))
			} else {
				sg("admin", root, "repo", "create", pick(names))
			}
		}
	}

	garbage := []string{"in", "out", "--into", ":", "mono:", ":svc", "--project", "--path", "--to", "--force", "--json",
		"--mirror", "x", "app", "file://", "-h", "--", "mono:svc:x", "日本", "--server", "nope:1"}
	opGarbage := func(i int) {
		args := []string{"migrate"}
		for k := rng.IntN(6); k > 0; k-- {
			args = append(args, pick(garbage))
		}
		before := snapshot()
		code, _, _ := sg("admin", root, args...)
		if code != 0 {
			after := snapshot()
			if !slices.Equal(before.repos, after.repos) || !slices.Equal(before.projects, after.projects) || after.migrateRefs != "" || !maps.Equal(before.mains, after.mains) {
				fail("garbage %q changed state", args)
			}
		}
		if rng.IntN(4) == 0 {
			// non-admin migrate must be refused and change nothing
			args := []string{"migrate", "in", srcs[0].url, "bobs"}
			if rng.IntN(2) == 0 {
				args = []string{"migrate", "out", pick(append(names, "nope")), "--to", "file://" + targets[0]}
			}
			r0 := remoteMain(targets[0])
			if code, _, _ := sg("bob", root, args...); code == 0 || remoteMain(targets[0]) != r0 {
				fail("non-admin %q: exit %d", args, code)
			}
		}
	}

	// opConcurrent replays a source --into a repo (by RPC, in a goroutine)
	// while a land goes into a project mounting that repo.
	opConcurrent := func(i int) {
		var cands []projT
		for _, p := range projects() {
			for _, m := range p.mounts {
				if m.Path != "." {
					continue
				}
				cands = append(cands, p)
			}
		}
		if len(cands) == 0 {
			return
		}
		p := cands[rng.IntN(len(cands))]
		m := p.mounts[0]
		src := srcs[rng.IntN(len(srcs))]
		folder := fmt.Sprintf("conc/c%d", i)
		if m.Folder != "." {
			folder = m.Folder + "/" + folder
		}
		g := gitx.Open(src.dir)
		srcMain := testenv.Must(g.ReadRef(ctx, gitx.MainRef))
		srcTree := testenv.Must(g.ReadCommit(ctx, srcMain)).Tree
		srcFP := len(testenv.Must(g.FirstParents(ctx, srcMain, "")))
		g.Close()
		pname := fmt.Sprintf("conc%d", i)
		wc := filepath.Join(root, "wc", strconv.Itoa(i))
		if code, _, _ := sg("admin", root, "checkout", p.Name, wc); code != 0 {
			return
		}
		rel := fmt.Sprintf("cland%d.txt", i)
		content := fmt.Sprintf("concurrent land %d\n", i)
		writeFile(t, filepath.Join(wc, rel), content)
		type res struct {
			r   *sgitv1.MigrateInResponse
			err error
		}
		done := make(chan res, 1)
		ac := h.env.Admin(h.env.AdminToken)
		go func() {
			r, err := ac.MigrateIn(ctx, &sgitv1.MigrateInRequest{FromUrl: src.url, Name: pname, IntoRepo: m.Repo, IntoFolder: folder})
			done <- res{r, err}
		}()
		code, _, _ := sg("admin", wc, "commit", "-m", fmt.Sprintf("conc land %d", i), "--land")
		r := <-done
		logop("concurrent migrate in %s --into %s:%s -> %v", src.url, m.Repo, folder, r.err)
		stats[fmt.Sprintf("concurrent migrate ok=%v land=%d", r.err == nil, code)]++
		if code == 0 {
			rp := rel
			if m.Folder != "." {
				rp = m.Folder + "/" + rel
			}
			lands = append(lands, landT{p.ID, rel, m.Repo, rp, repoGen[m.Repo], content})
			checkLand(t, fail, p.ID, projDir(p.ID), rel, repoDir(m.Repo), rp, content, catchUp)
		}
		if r.err != nil {
			if msg := r.err.Error(); !strings.Contains(msg, "not empty") && !strings.Contains(msg, "a file in repo") {
				fail("concurrent migrate in --into %s:%s failed: %v", m.Repo, folder, r.err)
			}
			return
		}
		if int(r.r.Commits) != srcFP {
			fail("concurrent migrate in: %d commits, want %d", r.r.Commits, srcFP)
		}
		main := remoteMain(repoDir(m.Repo))
		if got, want := subtree(repoDir(m.Repo), main, folder), subtree(src.dir, srcMain, "."); got != want {
			fail("concurrent migrate in: %s:%s = %q, source tree %q (%s)", m.Repo, folder, got, want, srcTree)
		}
		g = h.env.Git(m.Repo)
		if ok, err := g.IsAncestor(ctx, r.r.Main, main); err != nil || !ok {
			fail("concurrent migrate in: reported main %s is not in %s main %s", r.r.Main, m.Repo, main)
		}
		g.Close()
		for _, q := range projects() {
			if q.Name == pname {
				checkProject(q, "after concurrent migrate in")
			}
		}
	}

	for i := 0; i < steps; i++ {
		switch k := rng.IntN(100); {
		case k < 28:
			opMigrateIn(i)
		case k < 46:
			opLand(i)
		case k < 56:
			opSrcCommit(i)
		case k < 63:
			opServerDirect(i)
		case k < 78:
			opMigrateOut(i)
		case k < 83:
			opTargetEdit(i)
		case k < 92:
			opAdmin(i)
		case k < 95:
			opConcurrent(i)
		default:
			opGarbage(i)
		}
		if i%25 == 24 {
			for _, p := range projects() {
				checkProject(p, fmt.Sprintf("periodic check at step %d", i))
			}
		}
		if failures > 15 {
			t.Fatalf("seed %d: too many failures", seed)
		}
	}

	// ---- final checks ----
	for _, p := range projects() {
		checkProject(p, "final check")
	}
	lost := 0
	for _, l := range lands {
		if deadProjects[l.projectID] {
			continue
		}
		pd := projDir(l.projectID)
		if _, err := os.Stat(pd); err != nil {
			continue
		}
		src := ""
		if repoGen[l.repo] == l.gen {
			src = repoDir(l.repo)
		}
		before := failures
		checkLand(t, fail, l.projectID, pd, l.projPath, src, l.repoPath, l.content, catchUp)
		if failures > before {
			lost++
		}
	}
	s := snapshot()
	if s.migrateRefs != "" || len(s.incoming) > 0 {
		fail("leftovers: refs %q, .incoming %v", s.migrateRefs, s.incoming)
	}
	var dirs []string
	for _, r := range s.repoDirs {
		dirs = append(dirs, filepath.Join(data, "repos", r))
	}
	for _, d := range dirList(filepath.Join(data, "projects")) {
		if strings.HasSuffix(d, ".git") {
			dirs = append(dirs, filepath.Join(data, "projects", d))
		}
	}
	dirs = append(dirs, targets...)
	for _, d := range dirs {
		if out, err := exec.Command("git", "--git-dir="+d, "fsck", "--strict").CombinedOutput(); err != nil {
			fail("git fsck --strict %s: %v\n%s", d, err, out)
		}
	}
	var em []string
	for m, n := range errMsgs {
		em = append(em, fmt.Sprintf("%4d  %s", n, m))
	}
	sort.Strings(em)
	t.Logf("seed %d, %d steps, %d lands (%d lost), exit codes: %v\nstats: %v", seed, steps, len(lands), lost, codes, stats)
	if os.Getenv("SG_MONKEY_VERBOSE") != "" {
		t.Logf("error messages:\n%s", strings.Join(em, "\n"))
	}
}

// checkLand asserts content at projPath is in project main's history and,
// if repoDir is set, at repoPath in the repo's main history.
func checkLand(t *testing.T, fail func(string, ...any), id int64, projDir, projPath, repoDir, repoPath, content string, catchUp func(string) string) {
	t.Helper()
	catchUp("#" + strconv.FormatInt(id, 10))
	blob := blobSHA(content)
	if !blobHistoryQ(t, projDir)[projPath+" "+blob] {
		fail("landed %s = %q missing from project #%d main history", projPath, content, id)
	}
	if repoDir != "" && !blobHistoryQ(t, repoDir)[repoPath+" "+blob] {
		fail("landed %s = %q missing from %s main history", repoPath, content, repoDir)
	}
}

// blobHistoryQ is blobHistory with unquoted (UTF-8) paths.
func blobHistoryQ(t *testing.T, gitDir string) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "-c", "core.quotePath=false", "--git-dir="+gitDir, "log", "--raw", "--no-abbrev", "--format=", "-m", "main").Output()
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

// conflictsWithTree reports whether adding p clashes with files: a parent of
// p is a file, or p is a directory prefix of a file.
func conflictsWithTree[V any](files map[string]V, p string) bool {
	for f := range files {
		if strings.HasPrefix(p, f+"/") || strings.HasPrefix(f, p+"/") {
			return true
		}
	}
	return false
}

func conflictsWithChanges(ch []gitx.Change, p string) bool {
	for _, c := range ch {
		if strings.HasPrefix(p, c.Path+"/") || strings.HasPrefix(c.Path, p+"/") {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}
