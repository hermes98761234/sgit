package server_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// twoRepos makes repos "a" and "b" (each with f.txt) and project "web"
// mounting them at a/ and b/.
func twoRepos(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	e := testenv.Start(t)
	for _, r := range []string{"a", "b"} {
		e.CreateRepo(r)
		e.CommitDirect(r, map[string]string{"f.txt": "base " + r + "\n"}, "seed "+r)
	}
	e.CreateProject("web")
	e.Mount("web", "a", "a", ".", false)
	return e, e.Mount("web", "b", "b", ".", false)
}

func countSubject(t *testing.T, e *testenv.Env, repo, subject string) int {
	t.Helper()
	g := e.Git(repo)
	defer g.Close()
	n := 0
	for _, l := range testenv.Must(g.Log(context.Background(), "main", "", 100)) {
		if l.Subject == subject {
			n++
		}
	}
	return n
}

func TestCrossRepoLand(t *testing.T) {
	ctx := context.Background()
	e, head := twoRepos(t)
	c := e.Project(e.AdminToken)
	shas := e.Upload(e.AdminToken, "web", []byte("A\n"), []byte("B\n"))
	resp := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}}))
	if e.Files("a", "main")["f.txt"] != "A\n" || e.Files("b", "main")["f.txt"] != "B\n" {
		t.Fatal("not landed in both repos")
	}
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web", N: 3}))
	if lg.Entries[0].Sha != resp.Commit || lg.Entries[0].Subject != "both" || lg.Entries[1].Subject != "both" {
		t.Fatalf("project log = %+v (one commit per repo)", lg.Entries)
	}
}

func TestPartialLandThenRetryLandsRest(t *testing.T) {
	ctx := context.Background()
	e, head := twoRepos(t)
	alice := e.NewUser("alice", "web", "write")
	c := e.Project(alice)
	e.CommitDirect("b", map[string]string{"f.txt": "theirs b\n"}, "upstream b")
	shas := e.Upload(alice, "web", []byte("ours a\n"), []byte("ours b\n"))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("partial land = %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	now := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if det == nil || !det.Partial || !reflect.DeepEqual(det.Landed, []string{"a"}) || det.Main != now ||
		len(det.Files) != 1 || det.Files[0].Path != "b/f.txt" {
		t.Fatalf("detail = %+v", det)
	}
	if e.Files("a", "main")["f.txt"] != "ours a\n" {
		t.Fatal("repo a did not land")
	}
	// The draft now holds only what did not land, on its original base (the
	// 3-way merge base for a retry), same id; det.Main is the new head.
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: det.DraftId}))
	if d.Base != head || d.Status != "open" {
		t.Fatalf("draft after partial = %+v", d)
	}
	patch := string(testenv.Must(c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: d.Base, B: d.Head})).Patch)
	if !strings.Contains(patch, "b/f.txt") || strings.Contains(patch, "a/f.txt") {
		t.Fatalf("draft diff = %s", patch)
	}
	// Resolve and retry: only b lands; a is never landed twice.
	fixed := e.Upload(alice, "web", []byte("merged b\n"))[0]
	testenv.Must(c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: d.Id, Base: now, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("b/f.txt", fixed)}}))
	testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}))
	if e.Files("b", "main")["f.txt"] != "merged b\n" {
		t.Fatal("retry did not land b")
	}
	if n := countSubject(t, e, "a", "both"); n != 1 {
		t.Fatalf("repo a has %d commits from the draft, want 1", n)
	}
}

func TestFirstRepoConflictIsNotPartial(t *testing.T) {
	ctx := context.Background()
	e, head := twoRepos(t)
	c := e.Project(e.AdminToken)
	e.CommitDirect("a", map[string]string{"f.txt": "theirs a\n"}, "upstream a")
	shas := e.Upload(e.AdminToken, "web", []byte("ours a\n"), []byte("ours b\n"))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}})
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	if det == nil || det.Partial || len(det.Landed) != 0 {
		t.Fatalf("detail = %+v", det)
	}
	if e.Files("b", "main")["f.txt"] != "base b\n" {
		t.Fatal("repo b landed after repo a conflicted")
	}
}

func TestConcurrentLandsThroughTwoProjects(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/seed": "s"}, "seed")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	e.CreateProject("api")
	e.Mount("api", "code", "core", "src", false)
	const n = 10
	c := e.Project(e.AdminToken) // one client, shared by the goroutines
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		for _, pp := range []struct{ project, dir string }{{"web", "lib"}, {"api", "code"}} {
			wg.Add(1)
			go func(i int, project, dir string) {
				defer wg.Done()
				rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: project})
				if err != nil {
					errs <- err
					return
				}
				name := fmt.Sprintf("%s-%d", project, i)
				shas, err := rpc.UploadBlobs(ctx, c, project, [][]byte{[]byte(name)})
				if err != nil {
					errs <- err
					return
				}
				_, err = c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: project, Base: rr.Sha, Message: name,
					Changes: []*sgitv1.Change{testenv.Add(dir+"/"+name, shas[0])}})
				errs <- err
			}(i, pp.project, pp.dir)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent land: %v", err)
		}
	}
	files := e.Files("core", "main")
	if len(files) != 2*n+1 {
		t.Fatalf("source has %d files, want %d", len(files), 2*n+1)
	}
	for _, p := range []string{"web", "api"} {
		g := e.ProjectGit(e.ProjectID(p))
		testenv.Must(e.Project(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: p})) // catch up
		if got := len(testenv.Must(g.ListFiles(ctx, "main"))); got != 2*n+1 {
			t.Errorf("project %s has %d files", p, got)
		}
		g.Close()
	}
}

// A non-conflict error after a repo landed is still a partial land: the
// client gets Aborted + partial detail and the draft keeps only the rest.
func TestPartialLandOnErrorAfterLanding(t *testing.T) {
	ctx := context.Background()
	var e *testenv.Env
	var n atomic.Int32
	e = testenv.Start(t, func(c *server.Config) {
		c.LandRetries = 2
		c.BeforeCAS = func(repo string) {
			if repo == "b" {
				e.CommitDirect(repo, map[string]string{fmt.Sprintf("ext%d.txt", n.Add(1)): "x"}, "external")
			}
		}
	})
	for _, r := range []string{"a", "b"} {
		e.CreateRepo(r)
		e.CommitDirect(r, map[string]string{"f.txt": "base " + r + "\n"}, "seed "+r)
	}
	e.CreateProject("web")
	e.Mount("web", "a", "a", ".", false)
	head := e.Mount("web", "b", "b", ".", false)
	c := e.Project(e.AdminToken)
	shas := e.Upload(e.AdminToken, "web", []byte("A\n"), []byte("B\n"))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}})
	st := status.Convert(err)
	if st.Code() != codes.Aborted || !strings.Contains(st.Message(), "landed a; the rest did not land: main kept moving") {
		t.Fatalf("partial land = %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, d := range st.Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	now := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	if det == nil || !det.Partial || !reflect.DeepEqual(det.Landed, []string{"a"}) || det.Main != now || len(det.Files) != 0 {
		t.Fatalf("detail = %+v", det)
	}
	if e.Files("a", "main")["f.txt"] != "A\n" || e.Files("b", "main")["f.txt"] != "base b\n" {
		t.Fatal("want a landed and b not")
	}
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: det.DraftId}))
	patch := string(testenv.Must(c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: d.Base, B: d.Head})).Patch)
	if d.Base != head || d.Status != "open" || !strings.Contains(patch, "b/f.txt") || strings.Contains(patch, "a/f.txt") {
		t.Fatalf("draft after partial = %+v\n%s", d, patch)
	}
}

// A path unmounted and mounted again from another folder: a draft made on
// the old layout must not write its content into the new folder (Task 7
// carry). The land is refused and names neither the repo nor a folder.
func TestLandRefusesDraftFromOlderLayout(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/f.txt": "src\n", "other/f.txt": "other\n"}, "seed")
	e.CreateProject("web")
	head := e.Mount("web", "lib", "core", "src", false)
	c := e.Project(e.AdminToken)
	sha := e.Upload(e.AdminToken, "web", []byte("mine\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: head, Message: "m",
		Changes: []*sgitv1.Change{testenv.Modify("lib/f.txt", sha)}}))
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "web", Path: "lib"}))
	e.Mount("web", "lib", "core", "other", false)
	_, err := c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id})
	want := "draft " + d.Id + " was made before mount lib/ changed; sync and commit again"
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Fatalf("land on older layout = %v", err)
	}
	if f := e.Files("core", "main"); f["other/f.txt"] != "other\n" || f["src/f.txt"] != "src\n" {
		t.Fatalf("source changed: %v", f)
	}
	// After a sync (a draft on the current head) the same change lands.
	now := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: now, Message: "m",
		Changes: []*sgitv1.Change{testenv.Modify("lib/f.txt", sha)}}))
	if e.Files("core", "main")["other/f.txt"] != "mine\n" {
		t.Fatal("land on the current layout failed")
	}
}

func lines(first, last string) string {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		switch i {
		case 1:
			b.WriteString(first + "\n")
		case 10:
			b.WriteString(last + "\n")
		default:
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	return b.String()
}

// After a partial land the rebuilt draft keeps its merge base: retrying it
// unresolved conflicts again instead of overwriting upstream, and clean
// upstream edits in the repo that conflicted and in a repo after the stop
// point (never attempted) survive the retry.
func TestRetryAfterPartialLandMergesWithUpstream(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("a")
	e.CommitDirect("a", map[string]string{"f.txt": "base a\n"}, "seed a")
	e.CreateRepo("b")
	e.CommitDirect("b", map[string]string{"f.txt": "base b\n", "g.txt": lines("one", "ten")}, "seed b")
	e.CreateRepo("c")
	e.CommitDirect("c", map[string]string{"h.txt": lines("one", "ten")}, "seed c")
	e.CreateProject("web")
	for _, r := range []string{"a", "b", "c"} {
		e.Mount("web", r, r, ".", false)
	}
	pc := e.Project(e.AdminToken)
	head := testenv.Must(pc.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	e.CommitDirect("b", map[string]string{"f.txt": "theirs b\n", "g.txt": lines("THEIRS", "ten")}, "upstream b")
	e.CommitDirect("c", map[string]string{"h.txt": lines("THEIRS", "ten")}, "upstream c")
	shas := e.Upload(e.AdminToken, "web", []byte("ours a\n"), []byte("ours b\n"), []byte(lines("one", "OURS")), []byte("theirs b\n"))
	_, err := pc.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "all",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1]),
			testenv.Modify("b/g.txt", shas[2]), testenv.Modify("c/h.txt", shas[2])}})
	var det *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	if det == nil || !det.Partial || !reflect.DeepEqual(det.Landed, []string{"a"}) {
		t.Fatalf("first land = %v %+v", err, det)
	}
	// (1) Retrying unresolved conflicts again and overwrites nothing.
	_, err = pc.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: det.DraftId})
	var again *sgitv1.ConflictDetail
	for _, d := range status.Convert(err).Details() {
		again, _ = d.(*sgitv1.ConflictDetail)
	}
	if status.Code(err) != codes.Aborted || again == nil || again.Partial || len(again.Files) != 1 || again.Files[0].Path != "b/f.txt" {
		t.Fatalf("unresolved retry = %v %+v", err, again)
	}
	if fb, fc := e.Files("b", "main"), e.Files("c", "main"); fb["f.txt"] != "theirs b\n" || fb["g.txt"] != lines("THEIRS", "ten") || fc["h.txt"] != lines("THEIRS", "ten") {
		t.Fatalf("upstream overwritten: b=%v c=%v", fb, fc)
	}
	// (2)+(3) Resolve b/f.txt against the same base; the retry merges b/g.txt
	// and c/h.txt with upstream's line 1, and a is not landed again.
	testenv.Must(pc.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: "web", Id: det.DraftId, Base: head, Message: "all",
		Changes: []*sgitv1.Change{testenv.Modify("b/f.txt", shas[3]), testenv.Modify("b/g.txt", shas[2]), testenv.Modify("c/h.txt", shas[2])}}))
	testenv.Must(pc.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: det.DraftId}))
	if fb, fc := e.Files("b", "main"), e.Files("c", "main"); fb["g.txt"] != lines("THEIRS", "OURS") || fc["h.txt"] != lines("THEIRS", "OURS") {
		t.Fatalf("retry lost upstream or ours: b=%q c=%q", fb["g.txt"], fc["h.txt"])
	}
	if n := countSubject(t, e, "a", "all"); n != 1 {
		t.Fatalf("repo a has %d commits from the draft, want 1", n)
	}
}

// When rebuilding the draft fails after a partial land, the client still
// gets the partial answer, the draft stays consistent (head ref and store
// agree) and a retry lands only what is left.
func TestPartialLandWithFailedRebuild(t *testing.T) {
	ctx := context.Background()
	var e *testenv.Env
	var moving atomic.Bool
	moving.Store(true)
	var hookErr error
	e = testenv.Start(t, func(c *server.Config) {
		c.LandRetries = 2
		c.BeforeCAS = func(repo string) {
			if repo != "b" || !moving.Load() {
				return
			}
			e.CommitDirect(repo, map[string]string{"ext.txt": time.Now().String()}, "external")
			_, err := e.Store.DB().Exec(`CREATE TRIGGER IF NOT EXISTS fail_draft BEFORE UPDATE ON drafts BEGIN SELECT RAISE(ABORT, 'injected'); END`)
			if err != nil {
				hookErr = err
			}
		}
	})
	for _, r := range []string{"a", "b"} {
		e.CreateRepo(r)
		e.CommitDirect(r, map[string]string{"f.txt": "base " + r + "\n"}, "seed "+r)
	}
	e.CreateProject("web")
	e.Mount("web", "a", "a", ".", false)
	head := e.Mount("web", "b", "b", ".", false)
	c := e.Project(e.AdminToken)
	shas := e.Upload(e.AdminToken, "web", []byte("A\n"), []byte("B\n"))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: head, Message: "both",
		Changes: []*sgitv1.Change{testenv.Modify("a/f.txt", shas[0]), testenv.Modify("b/f.txt", shas[1])}})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	moving.Store(false)
	testenv.Must(e.Store.DB().Exec(`DROP TRIGGER fail_draft`))
	st := status.Convert(err)
	var det *sgitv1.ConflictDetail
	for _, d := range st.Details() {
		det, _ = d.(*sgitv1.ConflictDetail)
	}
	if st.Code() != codes.Aborted || det == nil || !det.Partial || !reflect.DeepEqual(det.Landed, []string{"a"}) {
		t.Fatalf("partial land with failed rebuild = %v %+v", err, det)
	}
	d := testenv.Must(c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: det.DraftId}))
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	if ref := testenv.Must(g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/head")); ref != d.Head || d.Version != 1 {
		t.Fatalf("draft inconsistent: ref %s, store %+v", ref, d)
	}
	if _, err := g.ReadRef(ctx, "refs/sg/drafts/"+d.Id+"/v2"); err == nil {
		t.Fatal("version ref of the failed rebuild left behind")
	}
	testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}))
	if e.Files("b", "main")["f.txt"] != "B\n" || countSubject(t, e, "a", "both") != 1 {
		t.Fatal("retry after failed rebuild did not land exactly the rest")
	}
}

// Cross-repo lands run concurrently through two projects that both mount
// the same two repos: every land touches both repos, nothing is lost and
// no mount lands twice.
func TestConcurrentCrossRepoLandsThroughTwoProjects(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	for _, r := range []string{"core", "docs"} {
		e.CreateRepo(r)
		e.CommitDirect(r, map[string]string{"src/seed": "s"}, "seed")
	}
	for _, p := range []string{"web", "api"} {
		e.CreateProject(p)
		e.Mount(p, "lib", "core", "src", false)
		e.Mount(p, "doc", "docs", "src", false)
	}
	const n = 5
	c := e.Project(e.AdminToken)
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		for _, project := range []string{"web", "api"} {
			wg.Add(1)
			go func(i int, project string) {
				defer wg.Done()
				rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: project})
				if err != nil {
					errs <- err
					return
				}
				name := fmt.Sprintf("%s-%d", project, i)
				shas, err := rpc.UploadBlobs(ctx, c, project, [][]byte{[]byte(name)})
				if err != nil {
					errs <- err
					return
				}
				_, err = c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: project, Base: rr.Sha, Message: name,
					Changes: []*sgitv1.Change{testenv.Add("lib/"+name, shas[0]), testenv.Add("doc/"+name, shas[0])}})
				errs <- err
			}(i, project)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent cross-repo land: %v", err)
		}
	}
	for _, r := range []string{"core", "docs"} {
		if got := len(e.Files(r, "main")); got != 2*n+1 {
			t.Errorf("repo %s has %d files, want %d", r, got, 2*n+1)
		}
		for i := 0; i < n; i++ {
			for _, p := range []string{"web", "api"} {
				if k := countSubject(t, e, r, fmt.Sprintf("%s-%d", p, i)); k != 1 {
					t.Errorf("repo %s has %d commits %s-%d, want 1", r, k, p, i)
				}
			}
		}
	}
	for _, p := range []string{"web", "api"} {
		testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: p})) // catch up
		if got := len(e.ProjectFiles(p, "main")); got != 2*(2*n+1) {
			t.Errorf("project %s has %d files, want %d", p, got, 2*(2*n+1))
		}
	}
}
