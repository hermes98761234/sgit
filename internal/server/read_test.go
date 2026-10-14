package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fetch(t *testing.T, c sgitv1.ProjectClient, repo string, shas ...string) ([]*sgitv1.Object, error) {
	t.Helper()
	stream, err := c.GetObjects(context.Background(), &sgitv1.GetObjectsRequest{Project: repo, Shas: shas})
	if err != nil {
		return nil, err
	}
	var out []*sgitv1.Object
	for {
		o, err := stream.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
}

func TestUploadHasGet(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	c := e.Project(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("hello\n"), nil)
	if shas[0] != objfmt.BlobSHA([]byte("hello\n")) || shas[1] != objfmt.BlobSHA(nil) {
		t.Fatalf("shas = %v", shas)
	}
	missing := "1111111111111111111111111111111111111111"
	has := testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: []string{shas[0], missing}}))
	if len(has.Present) != 2 || !has.Present[0] || has.Present[1] {
		t.Fatalf("HasObjects = %v", has.Present)
	}
	testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web"})) // empty list = ACL probe
	objs := testenv.Must(fetch(t, c, "web", shas[0]))
	dec, _ := zstd.NewReader(nil)
	data, err := dec.DecodeAll(objs[0].ZstdData, nil)
	if err != nil || string(data) != "hello\n" || objs[0].Type != "blob" || objs[0].Size != 6 {
		t.Fatalf("object = %+v data=%q err=%v", objs[0], data, err)
	}
	if _, err := fetch(t, c, "web", missing); status.Code(err) != codes.NotFound {
		t.Fatalf("missing object: want NotFound, got %v", err)
	}
	if _, err := fetch(t, c, "web", "zz"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad sha: want InvalidArgument, got %v", err)
	}
}

func TestLargeBlobRoundTrip(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	big := make([]byte, 10<<20)
	_, _ = rand.Read(big)
	shas := e.Upload(e.AdminToken, "web", big)
	objs := testenv.Must(fetch(t, e.Project(e.AdminToken), "web", shas[0]))
	dec, _ := zstd.NewReader(nil)
	data, err := dec.DecodeAll(objs[0].ZstdData, nil)
	if err != nil || !bytes.Equal(data, big) {
		t.Fatalf("10 MiB round trip failed: err=%v len=%d", err, len(data))
	}
}

func TestBlobSizeLimit(t *testing.T) {
	e := testenv.Start(t, func(c *server.Config) { c.MaxBlobBytes = 1 << 20 })
	e.CreateRepo("web")
	_, err := rpc.UploadBlobs(context.Background(), e.Project(e.AdminToken), "web", [][]byte{make([]byte, 2<<20)})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized blob: want InvalidArgument, got %v", err)
	}
}

func TestReadPermissions(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepo("web")
	ctx := context.Background()
	reader := e.NewUser("bob-reader", "web", "read")
	stranger := e.NewUser("sam", "", "")
	testenv.Must(e.Project(reader).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}))
	if _, err := rpc.UploadBlobs(ctx, e.Project(reader), "web", [][]byte{[]byte("x")}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reader upload: want PermissionDenied, got %v", err)
	}
	if _, err := e.Project(stranger).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stranger: want PermissionDenied, got %v", err)
	}
	if _, err := e.Project(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown repo: want NotFound, got %v", err)
	}
	if _, err := e.Project(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown rev: want NotFound, got %v", err)
	}
	if _, err := e.Project(e.AdminToken).ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "Bad/Name"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad repo name: want InvalidArgument, got %v", err)
	}
}

func TestLogBlameDiffMergeFile(t *testing.T) {
	e := testenv.Start(t)
	first := e.CreateRepo("web")
	c := e.Project(e.AdminToken)
	ctx := context.Background()
	c1 := e.CommitDirect("web", map[string]string{"f.txt": "one\n"}, "add f")
	c2 := e.CommitDirect("web", map[string]string{"f.txt": "one\ntwo\n"}, "extend f")

	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web"}))
	if len(lg.Entries) != 3 || lg.Entries[0].Sha != c2 || lg.Entries[0].Subject != "extend f" || lg.Entries[2].Sha != first {
		t.Fatalf("log = %+v", lg.Entries)
	}
	bl := testenv.Must(c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "f.txt"}))
	if len(bl.Lines) != 2 || bl.Lines[0].Sha != c1 || string(bl.Lines[1].Text) != "two" || bl.Lines[1].Author != "direct" {
		t.Fatalf("blame = %+v", bl.Lines)
	}
	if _, err := c.Blame(ctx, &sgitv1.BlameRequest{Project: "web", Path: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("blame missing: want NotFound, got %v", err)
	}
	df := testenv.Must(c.Diff(ctx, &sgitv1.DiffRequest{Project: "web", A: c1, B: c2}))
	if !bytes.Contains(df.Patch, []byte("+two")) {
		t.Fatalf("diff = %s", df.Patch)
	}

	// git treats edits on adjacent lines as conflicting, so the clean case edits lines 1 and 3.
	shas := e.Upload(e.AdminToken, "web", []byte("a\nb\nc\n"), []byte("a\nX\nc\n"), []byte("a\nY\nc\n"), []byte("A\nb\nc\n"), []byte("a\nb\nC\n"))
	clean := testenv.Must(c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: shas[0], OursSha: shas[3], TheirsSha: shas[4]}))
	if clean.Conflicted || string(clean.Content) != "A\nb\nC\n" {
		t.Fatalf("clean merge = %q %v", clean.Content, clean.Conflicted)
	}
	conf := testenv.Must(c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", BaseSha: shas[0], OursSha: shas[1], TheirsSha: shas[2], OursLabel: "main", TheirsLabel: "local"}))
	if !conf.Conflicted || !bytes.Contains(conf.Content, []byte("<<<<<<< main")) {
		t.Fatalf("conflict merge = %q", conf.Content)
	}
	addAdd := testenv.Must(c.MergeFile(ctx, &sgitv1.MergeFileRequest{Project: "web", OursSha: shas[1], TheirsSha: shas[1]}))
	if addAdd.Conflicted || string(addAdd.Content) != "a\nX\nc\n" {
		t.Fatalf("identical add/add = %q %v", addAdd.Content, addAdd.Conflicted)
	}
}
