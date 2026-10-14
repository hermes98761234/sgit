package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

const maxBatch = 10000 // max SHAs per GetObjects/HasObjects request

func checkBatch(shas []string) error {
	if len(shas) > maxBatch {
		return status.Errorf(codes.InvalidArgument, "too many objects: %d > %d", len(shas), maxBatch)
	}
	for _, s := range shas {
		if !gitx.IsSHA(s) {
			return status.Errorf(codes.InvalidArgument, "invalid object id %q", s)
		}
	}
	return nil
}

func (r *repoService) GetObjects(req *sgitv1.GetObjectsRequest, stream grpc.ServerStreamingServer[sgitv1.Object]) error {
	ctx := stream.Context()
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return err
	}
	if err := checkBatch(req.Shas); err != nil {
		return err
	}
	for _, sha := range req.Shas {
		obj, err := h.git.ReadObject(ctx, sha)
		if err != nil {
			return r.s.toStatus(ctx, err)
		}
		msg := &sgitv1.Object{Sha: sha, Type: obj.Type, Size: uint64(len(obj.Data)), ZstdData: r.s.zenc.EncodeAll(obj.Data, nil)}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

func (r *repoService) HasObjects(ctx context.Context, req *sgitv1.HasObjectsRequest) (*sgitv1.HasObjectsResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if err := checkBatch(req.Shas); err != nil {
		return nil, err
	}
	present := make([]bool, len(req.Shas))
	for i, sha := range req.Shas {
		_, _, err := h.git.ObjectInfo(ctx, sha)
		switch {
		case err == nil:
			present[i] = true
		case errors.Is(err, gitx.ErrNotFound):
		default:
			return nil, r.s.toStatus(ctx, err)
		}
	}
	return &sgitv1.HasObjectsResponse{Present: present}, nil
}

func (r *repoService) UploadBlobs(stream grpc.ClientStreamingServer[sgitv1.BlobChunk, sgitv1.UploadBlobsResponse]) error {
	ctx := stream.Context()
	var h *repoHandle
	var buf bytes.Buffer
	var shas []string
	pending := false
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			if pending {
				return status.Error(codes.InvalidArgument, "stream ended inside a blob (missing end_of_blob)")
			}
			return stream.SendAndClose(&sgitv1.UploadBlobsResponse{Shas: shas})
		}
		if err != nil {
			return err
		}
		if h == nil {
			if h, _, err = r.s.open(ctx, c.Repo, auth.ActionWrite); err != nil {
				return err
			}
		}
		if int64(buf.Len()+len(c.Data)) > r.s.cfg.MaxBlobBytes {
			return status.Errorf(codes.InvalidArgument, "blob exceeds %d bytes", r.s.cfg.MaxBlobBytes)
		}
		buf.Write(c.Data)
		pending = true
		if c.EndOfBlob {
			sha, err := h.git.WriteBlob(ctx, buf.Bytes())
			if err != nil {
				return r.s.toStatus(ctx, err)
			}
			shas = append(shas, sha)
			buf.Reset()
			pending = false
		}
	}
}

func clean(s string) string { return strings.ToValidUTF8(s, "�") }

func (r *repoService) Log(ctx context.Context, req *sgitv1.LogRequest) (*sgitv1.LogResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	rev, n := req.Rev, int(req.N)
	if rev == "" {
		rev = "main"
	}
	if n <= 0 {
		n = 50
	}
	if n > 1000 {
		n = 1000
	}
	entries, err := h.git.Log(ctx, rev, req.Path, n)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.LogResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &sgitv1.LogEntry{
			Sha: e.SHA, AuthorName: clean(e.AuthorName), AuthorEmail: clean(e.AuthorEmail), Time: e.Time.Unix(), Subject: clean(e.Subject),
		})
	}
	return resp, nil
}

func (r *repoService) Blame(ctx context.Context, req *sgitv1.BlameRequest) (*sgitv1.BlameResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if err := validatePath(req.Path); err != nil {
		return nil, err
	}
	rev := req.Rev
	if rev == "" {
		rev = "main"
	}
	lines, err := h.git.Blame(ctx, rev, req.Path)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	resp := &sgitv1.BlameResponse{}
	for _, l := range lines {
		resp.Lines = append(resp.Lines, &sgitv1.BlameLine{Sha: l.SHA, Author: clean(l.Author), Line: int32(l.Line), Text: []byte(l.Text)})
	}
	return resp, nil
}

func (r *repoService) Diff(ctx context.Context, req *sgitv1.DiffRequest) (*sgitv1.DiffResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	for _, p := range req.Paths {
		if err := validatePath(p); err != nil {
			return nil, err
		}
	}
	patch, err := h.git.Diff(ctx, req.A, req.B, req.Paths)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.DiffResponse{Patch: patch}, nil
}

func (r *repoService) readBlobOrEmpty(ctx context.Context, h *repoHandle, sha string) ([]byte, error) {
	if sha == "" {
		return nil, nil
	}
	obj, err := h.git.ReadObject(ctx, sha)
	if err != nil {
		return nil, err
	}
	if obj.Type != "blob" {
		return nil, status.Errorf(codes.InvalidArgument, "%s is a %s, not a blob", sha, obj.Type)
	}
	return obj.Data, nil
}

func (r *repoService) MergeFile(ctx context.Context, req *sgitv1.MergeFileRequest) (*sgitv1.MergeFileResponse, error) {
	h, _, err := r.s.open(ctx, req.Repo, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	var parts [3][]byte
	for i, sha := range []string{req.BaseSha, req.OursSha, req.TheirsSha} {
		if parts[i], err = r.readBlobOrEmpty(ctx, h, sha); err != nil {
			return nil, r.s.toStatus(ctx, err)
		}
	}
	ol, tl := req.OursLabel, req.TheirsLabel
	if ol == "" {
		ol = "ours"
	}
	if tl == "" {
		tl = "theirs"
	}
	out, conflicted, err := gitx.MergeFile(ctx, parts[0], parts[1], parts[2], ol, "base", tl)
	if err != nil {
		return nil, r.s.toStatus(ctx, err)
	}
	return &sgitv1.MergeFileResponse{Content: out, Conflicted: conflicted}, nil
}
