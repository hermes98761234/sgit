package server

import (
	"context"
	"errors"
	"path"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/repopath"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// validatePath applies repopath.Check, the rule the client uses to decide
// which files it can send, so the two never disagree.
func validatePath(p string) error {
	return pathStatus(repopath.Check(p, false))
}

func pathStatus(err error) error {
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func validMode(m string) bool {
	switch m {
	case "", "100644", "100755", "120000":
		return true
	}
	return false
}

// convertChanges validates a client change list and checks referenced blobs exist.
func (s *Server) convertChanges(ctx context.Context, g *gitx.Repo, in []*sgitv1.Change) ([]gitx.Change, error) {
	if len(in) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty change list")
	}
	if len(in) > s.cfg.MaxChanges {
		return nil, status.Errorf(codes.InvalidArgument, "too many changes: %d > %d", len(in), s.cfg.MaxChanges)
	}
	seen := make(map[string]bool, len(in))
	upserts := map[string]bool{}
	out := make([]gitx.Change, 0, len(in))
	for _, c := range in {
		// A delete may name a reserved path that is already stored (imported
		// history); only adds and modifies are held to the full rule.
		check := repopath.Check(c.Path, false)
		if c.Op == sgitv1.ChangeOp_CHANGE_OP_DELETE {
			check = repopath.CheckDelete(c.Path)
		}
		if err := pathStatus(check); err != nil {
			return nil, err
		}
		if seen[c.Path] {
			return nil, status.Errorf(codes.InvalidArgument, "duplicate path %q", c.Path)
		}
		seen[c.Path] = true
		switch c.Op {
		case sgitv1.ChangeOp_CHANGE_OP_DELETE:
			out = append(out, gitx.Change{Path: c.Path, Op: gitx.OpDelete})
		case sgitv1.ChangeOp_CHANGE_OP_ADD, sgitv1.ChangeOp_CHANGE_OP_MODIFY:
			if !validMode(c.Mode) {
				return nil, status.Errorf(codes.InvalidArgument, "invalid mode %q for %q", c.Mode, c.Path)
			}
			if err := pathStatus(repopath.Check(c.Path, c.Mode == "120000")); err != nil {
				return nil, err
			}
			if !gitx.IsSHA(c.BlobSha) {
				return nil, status.Errorf(codes.InvalidArgument, "invalid blob id %q for %q", c.BlobSha, c.Path)
			}
			typ, _, err := g.ObjectInfo(ctx, c.BlobSha)
			if errors.Is(err, gitx.ErrNotFound) || (err == nil && typ != "blob") {
				return nil, status.Errorf(codes.InvalidArgument, "blob %s for %q not uploaded", c.BlobSha, c.Path)
			}
			if err != nil {
				return nil, s.toStatus(ctx, err)
			}
			op := gitx.OpAdd
			if c.Op == sgitv1.ChangeOp_CHANGE_OP_MODIFY {
				op = gitx.OpModify
			}
			upserts[c.Path] = true
			out = append(out, gitx.Change{Path: c.Path, Op: op, BlobSHA: c.BlobSha, Mode: c.Mode})
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown op for %q", c.Path)
		}
	}
	for p := range upserts {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if upserts[d] {
				return nil, status.Errorf(codes.InvalidArgument, "path %q conflicts with file %q in the same change list", p, d)
			}
		}
	}
	return out, nil
}
