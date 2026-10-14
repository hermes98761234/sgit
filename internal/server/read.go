package server

import (
	"context"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
)

type repoService struct {
	sgitv1.UnimplementedRepoServer
	s *Server
}

func (r *repoService) ResolveRev(ctx context.Context, req *sgitv1.ResolveRevRequest) (*sgitv1.ResolveRevResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	rev := req.Rev
	if rev == "" {
		rev = "main"
	}
	sha, err := h.git.ResolveRev(ctx, rev)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.ResolveRevResponse{Sha: sha}, nil
}
