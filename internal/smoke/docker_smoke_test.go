//go:build docker

// Package smoke runs an end-to-end check against a running sg-server container.
// Run via scripts/docker-smoke.sh.
package smoke

import (
	"context"
	"crypto/tls"
	"os"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc"
)

func TestDockerSmoke(t *testing.T) {
	addr, tok := os.Getenv("SG_SMOKE_ADDR"), os.Getenv("SG_SMOKE_TOKEN")
	if addr == "" || tok == "" {
		t.Skip("SG_SMOKE_ADDR / SG_SMOKE_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The container uses a self-signed certificate; identity is not under test here.
	conn := testenv.Must(grpc.NewClient(addr, rpc.DialOptions(tok, &tls.Config{InsecureSkipVerify: true})...))
	defer conn.Close()
	admin := sgitv1.NewAdminClient(conn)
	repo := sgitv1.NewRepoClient(conn)

	who := testenv.Must(admin.WhoAmI(ctx, &sgitv1.WhoAmIRequest{}))
	if !who.Admin {
		t.Fatalf("WhoAmI = %+v", who)
	}
	main := testenv.Must(admin.CreateRepo(ctx, &sgitv1.CreateRepoRequest{Name: "smoke"})).Main
	shas := testenv.Must(rpc.UploadBlobs(ctx, repo, "smoke", [][]byte{[]byte("hello from docker\n")}))
	landed := testenv.Must(repo.CommitAndLand(ctx, &sgitv1.CommitAndLandRequest{
		Repo: "smoke", Base: main, Changes: []*sgitv1.Change{testenv.Add("hello.txt", shas[0])}, Message: "smoke commit",
	}))
	lg := testenv.Must(repo.Log(ctx, &sgitv1.LogRequest{Repo: "smoke", N: 5}))
	if len(lg.Entries) != 2 || lg.Entries[0].Sha != landed.Commit || lg.Entries[0].Subject != "smoke commit" {
		t.Fatalf("log = %+v", lg.Entries)
	}
}
