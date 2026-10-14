package server

import (
	"context"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/gitx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validatePath(p string) error {
	bad := func(why string) error { return status.Errorf(codes.InvalidArgument, "invalid path %q: %s", p, why) }
	switch {
	case p == "":
		return bad("empty")
	case len(p) > 4096:
		return bad("too long")
	case !utf8.ValidString(p):
		return bad("not UTF-8")
	case strings.ContainsAny(p, "\x00\r\n"):
		return bad("control character")
	case strings.HasPrefix(p, "/"):
		return bad("absolute")
	case path.Clean(p) != p:
		return bad("not normalized")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return bad("dot segment")
		}
		if strings.EqualFold(seg, ".git") || strings.EqualFold(seg, ".sg") {
			return bad("reserved name " + seg)
		}
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
		if err := validatePath(c.Path); err != nil {
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
