package daemon_test

import (
	"bytes"
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/daemontest"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPassThrough(t *testing.T) {
	e := testenv.Start(t)
	main := e.CreateRepoProject("web")
	d := daemontest.Start(t, e.Addr)
	c := d.Project(e.AdminToken)
	ctx := context.Background()

	rr := testenv.Must(c.ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web", Rev: "main"}))
	if rr.Sha != main {
		t.Fatalf("ResolveRev via sgd = %s, want %s", rr.Sha, main)
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 200<<10) // 3.2 MiB, multi-chunk
	shas := testenv.Must(rpc.UploadBlobs(ctx, c, "web", [][]byte{big, []byte("x\n")}))
	if shas[0] != objfmt.BlobSHA(big) || shas[1] != objfmt.BlobSHA([]byte("x\n")) {
		t.Fatalf("upload via sgd = %v", shas)
	}
	has := testenv.Must(c.HasObjects(ctx, &sgitv1.HasObjectsRequest{Project: "web", Shas: shas}))
	if len(has.Present) != 2 || !has.Present[0] || !has.Present[1] {
		t.Fatalf("HasObjects via sgd = %v", has.Present)
	}
	land := testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{
		Project: "web", Base: main, Changes: []*sgitv1.Change{testenv.Add("big.bin", shas[0])}, Message: "via sgd",
	}))
	if e.Files("web", "main")["big.bin"] != string(big) {
		t.Fatal("land via sgd did not reach main")
	}
	lg := testenv.Must(c.Log(ctx, &sgitv1.LogRequest{Project: "web", N: 1}))
	if len(lg.Entries) != 1 || lg.Entries[0].Sha != land.Commit {
		t.Fatalf("log via sgd = %+v", lg.Entries)
	}
	tree := testenv.Must(fetch.Tree(ctx, c, "web", land.Commit, nil))
	if tree["big.bin"].SHA != shas[0] {
		t.Fatalf("tree via sgd = %+v", tree)
	}
}

func TestConflictDetailSurvivesProxy(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	base := e.CommitDirect("web", map[string]string{"f.txt": "1\n2\n3\n"}, "base")
	d := daemontest.Start(t, e.Addr)
	c := d.Project(e.AdminToken)
	ctx := context.Background()
	shas := e.Upload(e.AdminToken, "web", []byte("1\nA\n3\n"), []byte("1\nB\n3\n"))
	testenv.Must(c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[0])}, Message: "a"}))
	_, err := c.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{Project: "web", Base: base, Changes: []*sgitv1.Change{testenv.Modify("f.txt", shas[1])}, Message: "b"})
	st := status.Convert(err)
	if st.Code() != codes.Aborted {
		t.Fatalf("want Aborted, got %v", err)
	}
	var det *sgitv1.ConflictDetail
	for _, x := range st.Details() {
		if cd, ok := x.(*sgitv1.ConflictDetail); ok {
			det = cd
		}
	}
	if det == nil || len(det.Files) != 1 || det.Files[0].Path != "f.txt" {
		t.Fatalf("ConflictDetail lost through sgd: %+v", det)
	}
}

func TestDaemonRequiresToken(t *testing.T) {
	e := testenv.Start(t)
	e.CreateRepoProject("web")
	d := daemontest.Start(t, e.Addr)
	ctx := context.Background()
	if _, err := d.Project("").ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: want Unauthenticated, got %v", err)
	}
	if _, err := d.Project("sgt_nope").ResolveRev(ctx, &sgitv1.ResolveRevRequest{Project: "web"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: want Unauthenticated, got %v", err)
	}
}

func TestInfo(t *testing.T) {
	e := testenv.Start(t)
	d := daemontest.Start(t, e.Addr)
	info := d.Info()
	if info.Upstream != e.Addr || info.Version == "" || info.CacheMaxBytes == 0 {
		t.Fatalf("Info = %+v", info)
	}
}
