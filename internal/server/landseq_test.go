package server_test

import (
	"context"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/testenv"
)

func TestLandSequenceIsRecordedBeforeMainMoves(t *testing.T) {
	ctx := context.Background()
	var seenAtCAS int
	var e *testenv.Env
	e = testenv.Start(t, func(c *server.Config) {
		c.BeforeCAS = func(repo string) {
			// The row for the commit about to land must already exist.
			var n int
			if err := e.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM land_seq WHERE repo=?`, repo).Scan(&n); err != nil {
				t.Error(err)
			}
			seenAtCAS = n
		}
	})
	main := e.CreateRepo("web")
	seqs := testenv.Must(e.Store.LandSeqs(ctx, "web", []string{main}))
	if len(seqs) != 1 {
		t.Fatalf("create did not record main: %v", seqs)
	}
	sha := e.Upload(e.AdminToken, "web", []byte("a\n"))[0]
	resp := testenv.Must(commitAndLand(e.Repo(e.AdminToken), main, "a.txt", sha, "add a"))
	if seenAtCAS != 2 {
		t.Fatalf("land_seq rows at CAS time = %d, want 2", seenAtCAS)
	}
	seqs = testenv.Must(e.Store.LandSeqs(ctx, "web", []string{main, resp.Commit}))
	if len(seqs) != 2 || seqs[main] >= seqs[resp.Commit] {
		t.Fatalf("land seqs = %v", seqs)
	}
}

func TestImportRecordsMain(t *testing.T) {
	ctx := context.Background()
	e := mirrorEnv(t)
	src, main := sourceRepo(t)
	got := testenv.Must(e.Admin(e.AdminToken).ImportRepo(ctx, &sgitv1.ImportRepoRequest{Name: "imp", FromUrl: "file://" + src}))
	if got.Main != main {
		t.Fatalf("import main = %s", got.Main)
	}
	if seqs := testenv.Must(e.Store.LandSeqs(ctx, "imp", []string{main})); len(seqs) != 1 {
		t.Fatalf("import did not record main: %v", seqs)
	}
}
