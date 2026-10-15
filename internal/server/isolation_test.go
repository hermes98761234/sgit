package server_test

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

const hiddenRepo = "zz-hidden-src"

// isoEnv: repo zz-hidden-src with pub/ and secret/, project "web" mounting
// pub at "app", bob with write on web. Returns env, bob's token, and every
// source commit SHA.
func isoEnv(t *testing.T) (*testenv.Env, string, []string) {
	t.Helper()
	e := testenv.Start(t)
	e.CreateRepo(hiddenRepo)
	e.CommitDirect(hiddenRepo, map[string]string{"pub/a.txt": "public a\n", "secret/k.txt": "top secret\n"}, "seed")
	e.CommitDirect(hiddenRepo, map[string]string{"secret/k.txt": "rotated secret\n"}, "rotate secret only")
	e.CreateProject("web")
	e.Mount("web", "app", hiddenRepo, "pub", false)
	bob := e.NewUser("bob", "web", "write")
	return e, bob, sourceSHAs(t, e)
}

func sourceSHAs(t *testing.T, e *testenv.Env) []string {
	t.Helper()
	g := e.Git(hiddenRepo)
	defer g.Close()
	var out []string
	for _, l := range testenv.Must(g.Log(context.Background(), "main", "", 1000)) {
		out = append(out, l.SHA)
	}
	return out
}

// assertClean fails if a client-facing message mentions the hidden repo, a
// source path outside the mount, or a source commit SHA.
func assertClean(t *testing.T, what string, m proto.Message, srcSHAs []string) {
	t.Helper()
	text := prototext.Format(m)
	for _, bad := range append([]string{hiddenRepo, "secret/", "pub/"}, srcSHAs...) {
		if strings.Contains(text, bad) {
			t.Fatalf("%s leaks %q:\n%s", what, bad, text)
		}
	}
}

func TestProjectRootListsOnlyMounts(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	c := e.Project(bob)
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	g := e.ProjectGit(e.ProjectID("web"))
	defer g.Close()
	root := testenv.Must(g.Subtrees(ctx, []string{head}, "."))[0]
	// The root tree, as a client fetches it, has exactly one entry: the mount.
	obj := testenv.Must(fetch(t, c, "web", root))[0]
	if obj.Type != "tree" {
		t.Fatalf("root = %+v", obj)
	}
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	data := testenv.Must(dec.DecodeAll(obj.ZstdData, nil))
	entries := testenv.Must(objfmt.ParseTree(data))
	if len(entries) != 1 || entries[0].Name != "app" || !entries[0].IsTree() {
		t.Fatalf("root tree entries = %+v, want exactly the tree app", entries)
	}
	files := testenv.Must(g.ListFiles(ctx, head))
	if len(files) != 1 || files["app/a.txt"].SHA == "" {
		t.Fatalf("project files = %v, want only app/a.txt", files)
	}
	if app := testenv.Must(g.Subtrees(ctx, []string{head}, "app"))[0]; app == "" {
		t.Fatal("mount path missing from the root")
	}
}

func TestForeignSHAsAreNotFound(t *testing.T) {
	ctx := context.Background()
	e, bob, src := isoEnv(t)
	c := e.Project(bob)
	g := e.Git(hiddenRepo)
	defer g.Close()
	secretTree := testenv.Must(g.Subtrees(ctx, []string{src[0]}, "secret"))[0]
	secretBlob := objfmt.BlobSHA([]byte("rotated secret\n"))
	// Positive controls: the same calls with project objects succeed, so a
	// NotFound below comes from the foreign SHA and nothing else.
	head := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	pubBlob := objfmt.BlobSHA([]byte("public a\n"))
	if objs, err := fetch(t, c, "web", head, pubBlob); err != nil || len(objs) != 2 {
		t.Fatalf("GetObjects(project head, project blob) = %v %v", objs, err)
	}
	if rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: head}); err != nil || rr.Sha != head {
		t.Fatalf("ResolveRev(project head) = %v %v", rr, err)
	}
	if lg, err := c.Log(ctx, &sgitv1.LogRequest{Project: "web", Rev: head}); err != nil || len(lg.Entries) == 0 {
		t.Fatalf("Log(project head) = %v %v", lg, err)
	}
	if _, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: head, B: "main"}); err != nil {
		t.Fatalf("Diff(project head) = %v", err)
	}
	if bl, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Rev: head, Path: "app/a.txt"}); err != nil || len(bl.Lines) != 1 {
		t.Fatalf("Blame(project head) = %v %v", bl, err)
	}
	if mf, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: pubBlob, OursSha: pubBlob, TheirsSha: pubBlob}); err != nil || string(mf.Content) != "public a\n" {
		t.Fatalf("MergeFile(project blobs) = %v %v", mf, err)
	}
	for _, sha := range append([]string{secretTree, secretBlob}, src...) {
		if _, err := fetch(t, c, "web", sha); status.Code(err) != codes.NotFound {
			t.Errorf("GetObjects(%s) = %v", sha, err)
		}
		has := testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: []string{pubBlob, sha}}))
		if !has.Present[0] || has.Present[1] {
			t.Errorf("HasObjects(project blob, %s) = %v, want [true false]", sha, has.Present)
		}
		for name, err := range map[string]error{
			"ResolveRev": func() error {
				_, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: sha})
				return err
			}(),
			"Log": func() error { _, err := c.Log(ctx, &sgitv1.LogRequest{Project: "web", Rev: sha}); return err }(),
			"Diff": func() error {
				_, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: sha, B: "main"})
				return err
			}(),
			"DiffB": func() error {
				_, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: head, B: sha})
				return err
			}(),
			"Blame": func() error {
				_, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Rev: sha, Path: "app/a.txt"})
				return err
			}(),
			"MergeFile(base)": func() error {
				_, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: sha, OursSha: pubBlob, TheirsSha: pubBlob})
				return err
			}(),
			"MergeFile(ours)": func() error {
				_, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: pubBlob, OursSha: sha, TheirsSha: pubBlob})
				return err
			}(),
			"MergeFile(theirs)": func() error {
				_, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: pubBlob, OursSha: pubBlob, TheirsSha: sha})
				return err
			}(),
		} {
			if status.Code(err) != codes.NotFound {
				t.Errorf("%s(%s) = %v", name, sha[:8], err)
			}
		}
	}
}

func TestUnmountedOnlyCommitNotInLog(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	e.CommitDirect(hiddenRepo, map[string]string{"pub/b.txt": "public b\n"}, "public after rotate")
	lg := testenv.Must(e.Project(bob).Log(ctx, &sgitv1.LogRequest{Project: "web", N: 1000}))
	var subjects []string
	for _, l := range lg.Entries {
		subjects = append(subjects, l.Subject)
	}
	// The source has seed, rotate secret only, public after rotate (3 commits,
	// far below the limit); the project has exactly the two touching pub/.
	if strings.Join(subjects, "|") != "public after rotate|seed" {
		t.Fatalf("project log subjects = %q, want [public after rotate seed]", subjects)
	}
}

func TestResponsesNeverLeakSources(t *testing.T) {
	ctx := context.Background()
	e, bob, src := isoEnv(t)
	c := e.Project(bob)
	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}))
	assertClean(t, "ResolveRev", rr, src)
	assertClean(t, "Log", testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web"})), src)
	assertClean(t, "Blame", testenv.Must(c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "app/a.txt"})), src)
	assertClean(t, "ListProjects", testenv.Must(c.ListProjects(ctx, &sgitv1.ListProjectsRequest{})), src)
	sha := e.Upload(bob, "web", []byte("new\n"))[0]
	d := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: rr.Sha, Message: "m",
		Changes: []*sgitv1.Change{testenv.Add("app/n.txt", sha)}}))
	assertClean(t, "CreateDraft", d, src)
	assertClean(t, "Diff", testenv.Must(c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: d.Base, B: d.Head})), src)
	resp := testenv.Must(c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}))
	assertClean(t, "Land", resp, append(src, sourceSHAs(t, e)...))
	assertClean(t, "ListDrafts", testenv.Must(c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: "web"})), sourceSHAs(t, e))
	// A conflict detail carries project paths and the project head only.
	e.CommitDirect(hiddenRepo, map[string]string{"pub/a.txt": "upstream\n"}, "upstream")
	ours := e.Upload(bob, "web", []byte("mine\n"))[0]
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: resp.Commit, Message: "m",
		Changes: []*sgitv1.Change{testenv.Modify("app/a.txt", ours)}})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("conflicting CommitAndLand = %v, want Aborted", err)
	}
	assertClean(t, "conflict", status.Convert(err).Proto(), sourceSHAs(t, e))
}

func TestIsolationThroughWarmDaemon(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	e.CreateProject("sec")
	e.Mount("sec", "s", hiddenRepo, "secret", false)
	d := daemontest.Start(t, e.Addr)
	secret := objfmt.BlobSHA([]byte("rotated secret\n"))
	if _, err := fetch(t, d.Project(e.AdminToken), "sec", secret); err != nil {
		t.Fatal(err)
	}
	objs, err := fetch(t, d.Project(bob), "web", secret)
	if status.Code(err) != codes.NotFound || len(objs) != 0 {
		t.Fatalf("warm sgd served a foreign object: %v %v", objs, err)
	}
	// Asking the warm daemon by the other project's name or id is the same
	// existence-hiding error as an unknown project.
	idRef := "#" + strconv.FormatInt(e.ProjectID("sec"), 10)
	for _, ref := range []string{"sec", idRef} {
		objs, err := fetch(t, d.Project(bob), ref, secret)
		if len(objs) != 0 || status.Convert(err).Message() != "project "+strconv.Quote(ref)+" not found or no access" {
			t.Errorf("warm sgd fetch from %s: %v %v", ref, objs, err)
		}
	}
	if has, err := d.Project(e.AdminToken).HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "sec", Shas: []string{secret}}); err != nil || !has.Present[0] {
		t.Fatalf("control: warm sgd HasObjects(sec, secret) = %v %v", has, err)
	}
	if has, err := d.Project(bob).HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: []string{secret}}); err != nil || has.Present[0] {
		t.Fatalf("warm sgd HasObjects(foreign) = %v %v", has, err)
	}
}

// secEnv extends isoEnv with project "sec" (mounting the secret folder) that
// bob cannot read, an open admin draft in it, and returns sec's id, the
// draft and sec's head.
func secEnv(t *testing.T) (*testenv.Env, string, int64, *sgitv1.Draft, string) {
	t.Helper()
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	id := e.CreateProject("sec")
	head := e.Mount("sec", "s", hiddenRepo, "secret", false)
	adm := e.Project(e.AdminToken)
	blob := e.Upload(e.AdminToken, "sec", []byte("draft secret\n"))[0]
	d := testenv.Must(adm.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "sec", Base: head, Message: "secret draft",
		Changes: []*sgitv1.Change{testenv.Add("s/new.txt", blob)}}))
	return e, bob, id, d, head
}

func uploadTo(ctx context.Context, c sgitv1.ProjectClient, project string, data []byte) error {
	st, err := c.UploadBlobs(ctx)
	if err != nil {
		return err
	}
	if err := st.Send(&sgitv1.BlobChunk{Project: project, Data: data, EndOfBlob: true}); err != nil && err != io.EOF {
		return err
	}
	_, err = st.CloseAndRecv()
	return err
}

func TestOtherProjectIsUnknownForEveryRPC(t *testing.T) {
	ctx := context.Background()
	e, bob, id, d, head := secEnv(t)
	c := e.Project(bob)
	idRef := "#" + strconv.FormatInt(id, 10)
	for _, ref := range []string{"sec", idRef, "nope", "#999"} {
		want := "project " + strconv.Quote(ref) + " not found or no access"
		calls := map[string]error{
			"ResolveRev":  func() error { _, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: ref}); return err }(),
			"GetObjects":  func() error { _, err := fetch(t, c, ref, head); return err }(),
			"HasObjects":  func() error { _, err := c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: ref}); return err }(),
			"UploadBlobs": uploadTo(ctx, c, ref, []byte("planted\n")),
			"CreateDraft": func() error {
				_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: ref, Base: head, Message: "m"})
				return err
			}(),
			"UpdateDraft": func() error {
				_, err := c.UpdateDraft(ctx, &sgitv1.UpdateDraftRequest{Project: ref, Id: d.Id, Base: head, Message: "m"})
				return err
			}(),
			"GetDraft":   func() error { _, err := c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: ref, Id: d.Id}); return err }(),
			"ListDrafts": func() error { _, err := c.ListDrafts(ctx, &sgitv1.ListDraftsRequest{Project: ref}); return err }(),
			"AbandonDraft": func() error {
				_, err := c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: ref, Id: d.Id})
				return err
			}(),
			"Land": func() error { _, err := c.Land(ctx, &sgitv1.LandRequest{Project: ref, Id: d.Id}); return err }(),
			"CommitAndLand": func() error {
				_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: ref, Base: head, Message: "m"})
				return err
			}(),
			"Log":       func() error { _, err := c.Log(ctx, &sgitv1.LogRequest{Project: ref}); return err }(),
			"Blame":     func() error { _, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: ref, Path: "s/k.txt"}); return err }(),
			"Diff":      func() error { _, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: ref, A: head, B: "main"}); return err }(),
			"MergeFile": func() error { _, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: ref}); return err }(),
		}
		for name, err := range calls {
			if st := status.Convert(err); st.Code() != codes.NotFound || st.Message() != want {
				t.Errorf("%s on %s = %v, want NotFound %q", name, ref, err, want)
			}
		}
	}
	got := testenv.Must(e.Project(e.AdminToken).GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "sec", Id: d.Id}))
	if got.Status != "open" || got.Version != 1 {
		t.Fatalf("the other project's draft changed: %+v", got)
	}
	has := testenv.Must(e.Project(e.AdminToken).HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "sec",
		Shas: []string{objfmt.BlobSHA([]byte("planted\n")), objfmt.BlobSHA([]byte("draft secret\n"))}}))
	if !has.Present[1] {
		t.Fatal("control: the admin's draft blob is not in sec")
	}
	if has.Present[0] {
		t.Fatal("a blob uploaded by a non-member reached the other project")
	}
	for _, p := range testenv.Must(c.ListProjects(ctx, &sgitv1.ListProjectsRequest{})).Projects {
		if p.Name != "web" {
			t.Fatalf("ListProjects shows %+v", p)
		}
	}
}

func TestOtherProjectsDraftAndObjectsAreUnknown(t *testing.T) {
	ctx := context.Background()
	e, bob, _, d, head := secEnv(t)
	c := e.Project(bob)
	draftBlob := objfmt.BlobSHA([]byte("draft secret\n"))
	secret := objfmt.BlobSHA([]byte("rotated secret\n"))
	pubBlob := objfmt.BlobSHA([]byte("public a\n"))
	base := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	// Control: a draft id of our own project works as a revision everywhere.
	own := testenv.Must(c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: base, Message: "own",
		Changes: []*sgitv1.Change{testenv.Add("app/own.txt", e.Upload(bob, "web", []byte("own\n"))[0])}}))
	if rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: own.Id}); err != nil || rr.Sha != own.Head {
		t.Fatalf("ResolveRev(own draft) = %v %v", rr, err)
	}
	if _, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Rev: own.Id, Path: "app/a.txt"}); err != nil {
		t.Fatalf("Blame(own draft) = %v", err)
	}
	if _, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: "main", B: own.Id}); err != nil {
		t.Fatalf("Diff(own draft) = %v", err)
	}
	if _, err := c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: own.Id}); err != nil {
		t.Fatalf("GetDraft(own draft) = %v", err)
	}
	// The other project's draft id, used against our project, names nothing.
	for name, err := range map[string]error{
		"GetDraft": func() error { _, err := c.GetDraft(ctx, &sgitv1.GetDraftRequest{Project: "web", Id: d.Id}); return err }(),
		"Land":     func() error { _, err := c.Land(ctx, &sgitv1.LandRequest{Project: "web", Id: d.Id}); return err }(),
		"AbandonDraft": func() error {
			_, err := c.AbandonDraft(ctx, &sgitv1.AbandonDraftRequest{Project: "web", Id: d.Id})
			return err
		}(),
		"ResolveRev": func() error {
			_, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: d.Id})
			return err
		}(),
		"Log": func() error { _, err := c.Log(ctx, &sgitv1.LogRequest{Project: "web", Rev: d.Id}); return err }(),
		"Diff": func() error {
			_, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: "main", B: d.Id})
			return err
		}(),
		"Blame": func() error {
			_, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Rev: d.Id, Path: "app/a.txt"})
			return err
		}(),
	} {
		if status.Code(err) != codes.NotFound {
			t.Errorf("%s(other project's draft) = %v, want NotFound", name, err)
		}
	}
	// The other project's commits, draft head and blobs are not in ours.
	for _, sha := range []string{head, d.Head, draftBlob, secret} {
		if _, err := fetch(t, c, "web", sha); status.Code(err) != codes.NotFound {
			t.Errorf("GetObjects(%s) = %v", sha[:8], err)
		}
		if has := testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: []string{sha}})); has.Present[0] {
			t.Errorf("HasObjects(%s) = true", sha[:8])
		}
	}
	if _, err := c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: pubBlob, OursSha: pubBlob, TheirsSha: pubBlob}); err != nil {
		t.Fatalf("MergeFile(project blobs) = %v", err)
	}
	for _, mf := range []*sgitv1.MergeFileRequest{
		{Project: "web", BaseSha: pubBlob, OursSha: secret, TheirsSha: pubBlob},
		{Project: "web", BaseSha: pubBlob, OursSha: pubBlob, TheirsSha: secret},
		{Project: "web", BaseSha: draftBlob, OursSha: pubBlob, TheirsSha: pubBlob},
	} {
		if resp, err := c.MergeFile(ctx, mf); status.Code(err) != codes.NotFound {
			t.Errorf("MergeFile(%v) = %q %v, want NotFound", mf, resp.GetContent(), err)
		}
	}
	// A foreign blob SHA cannot be committed into our project (that would
	// copy it into an object store we can read).
	for _, blob := range []string{secret, draftBlob} {
		_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Message: "steal",
			Changes: []*sgitv1.Change{testenv.Add("app/stolen.txt", blob)}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CommitAndLand with foreign blob %s = %v, want InvalidArgument", blob[:8], err)
		}
		_, err = c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: base, Message: "steal",
			Changes: []*sgitv1.Change{testenv.Add("app/stolen.txt", blob)}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateDraft with foreign blob %s = %v, want InvalidArgument", blob[:8], err)
		}
	}
	// A foreign commit is not a valid base.
	for _, b := range []string{head, d.Head} {
		_, err := c.CreateDraft(ctx, &sgitv1.CreateDraftRequest{Project: "web", Base: b, Message: "m",
			Changes: []*sgitv1.Change{testenv.Add("app/x.txt", e.Upload(bob, "web", []byte("x\n"))[0])}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateDraft on foreign base %s = %v", b[:8], err)
		}
	}
}

func TestWritesCannotEscapeTheMount(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	c := e.Project(bob)
	before := e.Files(hiddenRepo, "main")
	base := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	blob := e.Upload(bob, "web", []byte("pwned\n"))[0]
	for _, p := range []string{
		"secret/k.txt", "pub/a.txt", "app/../secret/k.txt", "../secret/k.txt", "app/../../secret/k.txt",
		"/secret/k.txt", "./app/a.txt", "app//a.txt", "app/./a.txt", "app", ".", "..", "app/.git/config",
		"app/.sg/config", "app/a.txt/", "x.txt", "App/a.txt",
	} {
		_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Message: "escape",
			Changes: []*sgitv1.Change{testenv.Add(p, blob)}})
		if code := status.Code(err); code != codes.InvalidArgument {
			t.Errorf("CommitAndLand(%q) = %v, want InvalidArgument", p, err)
		} else if msg := status.Convert(err).Message(); strings.Contains(msg, hiddenRepo) || strings.Contains(msg, "pub/") && !strings.Contains(p, "pub/") {
			// The error may echo the caller's own path, nothing else.
			t.Errorf("CommitAndLand(%q) error names the source: %s", p, msg)
		}
		_, err = c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Message: "escape",
			Changes: []*sgitv1.Change{testenv.Delete(p)}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CommitAndLand(delete %q) = %v, want InvalidArgument", p, err)
		}
	}
	// Control: the same base and blob land fine inside the mount.
	if _, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Message: "ok",
		Changes: []*sgitv1.Change{testenv.Add("app/ok.txt", blob)}}); err != nil {
		t.Fatalf("CommitAndLand(app/ok.txt) = %v", err)
	}
	before["pub/ok.txt"] = "pwned\n"
	after := e.Files(hiddenRepo, "main")
	if len(after) != len(before) {
		t.Fatalf("source files changed: %v -> %v", before, after)
	}
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("source file %s changed: %q -> %q", p, v, after[p])
		}
	}
}

func TestRevisionsCannotReachOutsideTheProjectRepo(t *testing.T) {
	ctx := context.Background()
	e, bob, src := isoEnv(t)
	c := e.Project(bob)
	srcRepo := filepath.Join(e.DataDir, "repos", hiddenRepo+".git")
	revs := []string{
		"../../repos/" + hiddenRepo + ".git/refs/heads/main",
		"refs/../../../repos/" + hiddenRepo + ".git/refs/heads/main",
		srcRepo + "/refs/heads/main",
		"../" + hiddenRepo + ".git/HEAD",
		"main..." + src[0], src[0] + "..main", src[0][:7], src[0][:12],
		":/rotate", ":/seed", "main^{/rotate}", "--all", "--git-dir=" + srcRepo,
		"@{upstream}", "main@{1}",
	}
	for _, rev := range revs {
		rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: rev})
		if err == nil {
			assertClean(t, "ResolveRev("+rev+")", rr, src)
			if lg, err := c.Log(ctx, &sgitv1.LogRequest{Project: "web", Rev: rev}); err == nil {
				assertClean(t, "Log("+rev+")", lg, src)
			}
			continue
		}
		if code := status.Code(err); code != codes.NotFound && code != codes.InvalidArgument {
			t.Errorf("ResolveRev(%q) = %v, want NotFound or InvalidArgument", rev, err)
		}
		if strings.Contains(err.Error(), hiddenRepo) && !strings.Contains(rev, hiddenRepo) {
			t.Errorf("ResolveRev(%q) error names the source: %v", rev, err)
		}
	}
	// Commits touching only unmounted folders cannot be found by message,
	// while a translated commit can (so the search itself works).
	if rr, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main^{/seed}"}); err != nil {
		t.Errorf("ResolveRev(main^{/seed}) = %v", err)
	} else {
		assertClean(t, "ResolveRev(main^{/seed})", rr, src)
	}
	for _, rev := range []string{":/rotate", "main^{/rotate}", "main^{/rotated}"} {
		if _, err := c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: rev}); status.Code(err) != codes.NotFound {
			t.Errorf("ResolveRev(%q) = %v, want NotFound", rev, err)
		}
	}
	// Blame with paths outside every mount: not in the project tree, or invalid.
	for p, want := range map[string]codes.Code{
		"secret/k.txt": codes.NotFound, "pub/a.txt": codes.NotFound, "app/../secret/k.txt": codes.InvalidArgument,
	} {
		if _, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: p}); status.Code(err) != want {
			t.Errorf("Blame(%q) = %v, want %v", p, err, want)
		}
	}
	// A diff across the range holding the source-only "rotate secret only"
	// commit shows the mounted change and nothing of the secret folder.
	e.CommitDirect(hiddenRepo, map[string]string{"pub/b.txt": "public b\n"}, "public after rotate")
	first := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main^{/seed}"})).Sha
	for _, paths := range [][]string{nil, {"secret/k.txt"}, {"secret"}, {"app"}} {
		df, err := c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: first, B: "main", Paths: paths})
		if err != nil {
			t.Fatalf("Diff(seed..main, %v) = %v", paths, err)
		}
		patch := string(df.Patch)
		if strings.Contains(patch, "secret") || strings.Contains(patch, "rotated") {
			t.Fatalf("Diff(seed..main, %v) shows the unmounted folder:\n%s", paths, patch)
		}
		if wantB := paths == nil || paths[0] == "app"; wantB != strings.Contains(patch, "+public b") {
			t.Fatalf("Diff(seed..main, %v) = %q", paths, patch)
		}
		assertClean(t, "Diff", df, sourceSHAs(t, e))
	}
}

// TestFetchedCommitsNeverNameSources reads every project commit as a client
// receives it (raw object bytes) and checks that no tree, parent, header or
// message line names a source commit, the repo, or an unmounted folder.
func TestFetchedCommitsNeverNameSources(t *testing.T) {
	ctx := context.Background()
	e, bob, _ := isoEnv(t)
	c := e.Project(bob)
	base := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"})).Sha
	testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Message: "landed through web",
		Changes: []*sgitv1.Change{testenv.Add("app/n.txt", e.Upload(bob, "web", []byte("n\n"))[0])}}))
	e.CommitDirect(hiddenRepo, map[string]string{"secret/k.txt": "again\n"}, "secret again")
	e.CommitDirect(hiddenRepo, map[string]string{"pub/c.txt": "c\n"}, "upstream pub")
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web", N: 1000}))
	if len(lg.Entries) != 3 {
		t.Fatalf("project log = %v, want seed, landed through web, upstream pub", lg.Entries)
	}
	src := sourceSHAs(t, e)
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	for _, l := range lg.Entries {
		obj := testenv.Must(fetch(t, c, "web", l.Sha))[0]
		if obj.Type != "commit" {
			t.Fatalf("%s is a %s", l.Sha, obj.Type)
		}
		raw := string(testenv.Must(dec.DecodeAll(obj.ZstdData, nil)))
		for _, bad := range append([]string{hiddenRepo, "secret", "pub/"}, src...) {
			if strings.Contains(raw, bad) {
				t.Fatalf("commit %s leaks %q:\n%s", l.Sha, bad, raw)
			}
		}
		// Every tree and parent it names is a project object the client can fetch.
		for _, line := range strings.Split(raw, "\n") {
			k, v, _ := strings.Cut(line, " ")
			if k == "" {
				break // end of headers
			}
			if k == "tree" || k == "parent" {
				if _, err := fetch(t, c, "web", v); err != nil {
					t.Fatalf("commit %s names %s %s that the project lacks: %v", l.Sha, k, v, err)
				}
			}
		}
	}
}
