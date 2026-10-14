package cli

import (
	"context"
	"strings"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/wc"
	"google.golang.org/grpc"
)

// shortHasObjects answers HasObjects with one flag fewer than asked.
type shortHasObjects struct{ sgitv1.RepoClient }

func (shortHasObjects) HasObjects(_ context.Context, req *sgitv1.HasObjectsRequest, _ ...grpc.CallOption) (*sgitv1.HasObjectsResponse, error) {
	return &sgitv1.HasObjectsResponse{Present: make([]bool, len(req.Shas)-1)}, nil
}

func TestUploadMissingRejectsShortHasObjects(t *testing.T) {
	s := &session{repo: shortHasObjects{}, name: "web", wc: &wc.WC{Root: t.TempDir()}}
	err := uploadMissing(context.Background(), s, map[string]string{"1111111111111111111111111111111111111111": "a", "2222222222222222222222222222222222222222": "b"})
	if err == nil || !strings.Contains(err.Error(), "flags for 2 objects") {
		t.Fatalf("want a HasObjects length error, got %v", err)
	}
}
