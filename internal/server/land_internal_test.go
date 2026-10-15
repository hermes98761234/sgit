package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/objfmt"
)

// Conflict stages may come from outside the mounts (merge-tree follows
// renames); any stage blob the project repo does not hold is blanked.
func TestProjectStagesBlanksForeignSHAs(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "p.git")
	if err := gitx.InitBare(ctx, dir); err != nil {
		t.Fatal(err)
	}
	g := gitx.Open(dir)
	defer g.Close()
	in, err := g.WriteBlob(ctx, []byte("in the project\n"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := objfmt.BlobSHA([]byte("only in a source repo\n"))
	got := projectStages(ctx, g, []gitx.ConflictFile{
		{Path: "a", Base: foreign, Ours: in, Theirs: in},
		{Path: "b", Base: in, Ours: "", Theirs: foreign},
	})
	want := []gitx.ConflictFile{{Path: "a", Ours: in, Theirs: in}, {Path: "b", Base: in}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("projectStages = %+v, want %+v", got, want)
	}
}
