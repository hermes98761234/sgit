package fetch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTreeAndBlobs(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	commit := e.CommitDirect("web", map[string]string{
		"a.txt": "A\n", "src/x.go": "X\n", "src/sub/y.go": "Y\n", "docs/z.md": "Z\n",
	}, "seed")
	c := e.Project(e.AdminToken)
	ctx := context.Background()

	full := testenv.Must(Tree(ctx, c, "web", commit, nil))
	if len(full) != 4 || full["src/sub/y.go"].SHA != objfmt.BlobSHA([]byte("Y\n")) || full["a.txt"].Mode != "100644" {
		t.Fatalf("full tree = %+v", full)
	}
	for sparse, want := range map[string]int{"src": 2, "src/sub": 1, "docs": 1, "nope": 0, "a.txt": 1} {
		got := testenv.Must(Tree(ctx, c, "web", commit, []string{sparse}))
		if len(got) != want {
			t.Errorf("sparse %q: %d files, want %d (%v)", sparse, len(got), want, got)
		}
	}
	blobs := testenv.Must(Blobs(ctx, c, "web", []string{full["a.txt"].SHA, full["docs/z.md"].SHA, full["a.txt"].SHA}))
	if string(blobs[full["a.txt"].SHA]) != "A\n" || string(blobs[full["docs/z.md"].SHA]) != "Z\n" || len(blobs) != 2 {
		t.Fatalf("blobs = %v", blobs)
	}
	if got := testenv.Must(Blobs(ctx, c, "web", nil)); len(got) != 0 {
		t.Fatalf("no shas must fetch nothing: %v", got)
	}
	if _, err := Blobs(ctx, c, "web", []string{"1111111111111111111111111111111111111111"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing blob: want NotFound, got %v", err)
	}
	if _, err := Blobs(ctx, c, "web", []string{commit}); err == nil {
		t.Fatal("a commit is not a blob")
	}
	if _, err := Tree(ctx, c, "web", full["a.txt"].SHA, nil); err == nil {
		t.Fatal("a blob is not a commit")
	}
}

func TestBatching(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	files := map[string]string{}
	for i := 0; i < Batch+150; i++ {
		files[fmt.Sprintf("d%02d/f%04d.txt", i%7, i)] = fmt.Sprintf("content %d\n", i)
	}
	commit := e.CommitDirect("web", files, "many")
	c := e.Project(e.AdminToken)
	ctx := context.Background()
	tree := testenv.Must(Tree(ctx, c, "web", commit, nil))
	if len(tree) != len(files) {
		t.Fatalf("tree has %d files, want %d", len(tree), len(files))
	}
	var shas []string
	for _, f := range tree {
		shas = append(shas, f.SHA)
	}
	blobs := testenv.Must(Blobs(ctx, c, "web", shas))
	if len(blobs) != len(files) {
		t.Fatalf("got %d blobs, want %d", len(blobs), len(files))
	}
}

// fakeRepo serves GetObjects from reply; every other RPC panics via the nil embed.
type fakeRepo struct {
	sgitv1.ProjectClient
	reply func(shas []string) []*sgitv1.Object
}

func (f *fakeRepo) GetObjects(_ context.Context, req *sgitv1.GetObjectsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[sgitv1.Object], error) {
	return &fakeStream{objs: f.reply(req.Shas)}, nil
}

type fakeStream struct {
	grpc.ClientStream
	objs []*sgitv1.Object
}

func (s *fakeStream) Recv() (*sgitv1.Object, error) {
	if len(s.objs) == 0 {
		return nil, io.EOF
	}
	o := s.objs[0]
	s.objs = s.objs[1:]
	return o, nil
}

// serve returns the objects whose SHA was requested.
func serve(objs ...*sgitv1.Object) func([]string) []*sgitv1.Object {
	return func(shas []string) []*sgitv1.Object {
		var out []*sgitv1.Object
		for _, s := range shas {
			for _, o := range objs {
				if o.Sha == s {
					out = append(out, o)
				}
			}
		}
		return out
	}
}

// encodeObj builds a zstd-compressed object as the server would send it.
func encodeObj(sha, typ string, data []byte) *sgitv1.Object {
	enc := testenv.Must(zstd.NewWriter(nil))
	return &sgitv1.Object{Sha: sha, Type: typ, Size: uint64(len(data)), ZstdData: enc.EncodeAll(data, nil)}
}

func TestOversizedObject(t *testing.T) {
	sha := "2222222222222222222222222222222222222222"
	c := &fakeRepo{reply: serve(&sgitv1.Object{Sha: sha, Type: "blob", Size: MaxObjectSize + 1})}
	if _, err := Blobs(context.Background(), c, "web", []string{sha}); err == nil {
		t.Fatal("an object above MaxObjectSize must be rejected")
	}
}

func TestTreeRejectsBadEntries(t *testing.T) {
	commit := "3333333333333333333333333333333333333333"
	root := "4444444444444444444444444444444444444444"
	raw := strings.Repeat("\x01", 20)
	cases := map[string]string{
		"reserved name":  "40000 .sg\x00" + raw,
		"slash in name":  "100644 a/b\x00" + raw,
		"duplicate file": "100644 f\x00" + raw + "100644 f\x00" + raw,
	}
	for name, body := range cases {
		c := &fakeRepo{reply: serve(
			encodeObj(commit, "commit", []byte("tree "+root+"\n\nmsg\n")),
			encodeObj(root, "tree", []byte(body)),
		)}
		if _, err := Tree(context.Background(), c, "web", commit, nil); err == nil {
			t.Errorf("%s: Tree must reject the entry", name)
		}
	}
}

func TestUnrequestedObject(t *testing.T) {
	want := "5555555555555555555555555555555555555555"
	c := &fakeRepo{reply: func([]string) []*sgitv1.Object {
		return []*sgitv1.Object{
			encodeObj(want, "blob", []byte("w")),
			encodeObj("6666666666666666666666666666666666666666", "blob", []byte("x")),
		}
	}}
	_, err := Blobs(context.Background(), c, "web", []string{want})
	if err == nil || !strings.Contains(err.Error(), "unrequested") {
		t.Fatalf("want unrequested-object error, got %v", err)
	}
}

// recordingRepo records every SHA requested through GetObjects and delegates
// to the real client.
type recordingRepo struct {
	sgitv1.ProjectClient
	mu        sync.Mutex
	requested map[string]bool
}

func (r *recordingRepo) GetObjects(ctx context.Context, req *sgitv1.GetObjectsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[sgitv1.Object], error) {
	r.mu.Lock()
	for _, s := range req.Shas {
		r.requested[s] = true
	}
	r.mu.Unlock()
	return r.ProjectClient.GetObjects(ctx, req, opts...)
}

func TestSparseTreeSkipsOtherSubtrees(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	commit := e.CommitDirect("web", map[string]string{"src/x.go": "X\n", "docs/z.md": "Z\n", "docs/deep/w.md": "W\n"}, "seed")
	ctx := context.Background()
	g := e.Git("web")
	defer g.Close()
	// subtree returns the SHA of the tree entry name inside tree, read with git.
	subtree := func(tree, name string) string {
		t.Helper()
		obj := testenv.Must(g.ReadObject(ctx, tree))
		for _, e := range testenv.Must(objfmt.ParseTree(obj.Data)) {
			if e.Name == name && e.IsTree() {
				return e.SHA
			}
		}
		t.Fatalf("no tree %q in %s", name, tree)
		return ""
	}
	root := testenv.Must(g.ReadCommit(ctx, commit)).Tree
	docsTree, srcTree := subtree(root, "docs"), subtree(root, "src")
	deepTree := subtree(docsTree, "deep")

	r := &recordingRepo{ProjectClient: e.Project(e.AdminToken), requested: map[string]bool{}}
	files := testenv.Must(Tree(ctx, r, "web", commit, []string{"src"}))
	if len(files) != 1 || files["src/x.go"].SHA == "" {
		t.Fatalf("sparse tree = %v", files)
	}
	if !r.requested[srcTree] {
		t.Fatal("the src tree must be fetched")
	}
	if r.requested[docsTree] || r.requested[deepTree] || r.requested[objfmt.BlobSHA([]byte("Z\n"))] {
		t.Fatalf("a sparse walk of src requested objects under docs: %v", r.requested)
	}
}
