package daemon

import (
	"context"
	"io"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc"
)

type proxy struct {
	sgitv1.UnimplementedRepoServer
	d *Daemon
}

// pass forwards one unary call upstream with the caller's token. Upstream
// errors (including status details such as ConflictDetail) are returned as is.
func pass[Req, Resp any](ctx context.Context, req Req, call func(context.Context, Req, ...grpc.CallOption) (Resp, error)) (Resp, error) {
	out, _, err := forward(ctx)
	if err != nil {
		var zero Resp
		return zero, err
	}
	return call(out, req)
}

func (p *proxy) ResolveRev(ctx context.Context, r *sgitv1.ResolveRevRequest) (*sgitv1.ResolveRevResponse, error) {
	return pass(ctx, r, p.d.up.ResolveRev)
}

func (p *proxy) HasObjects(ctx context.Context, r *sgitv1.HasObjectsRequest) (*sgitv1.HasObjectsResponse, error) {
	return pass(ctx, r, p.d.up.HasObjects)
}

func (p *proxy) CreateDraft(ctx context.Context, r *sgitv1.CreateDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.CreateDraft)
}

func (p *proxy) UpdateDraft(ctx context.Context, r *sgitv1.UpdateDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.UpdateDraft)
}

func (p *proxy) GetDraft(ctx context.Context, r *sgitv1.GetDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.GetDraft)
}

func (p *proxy) ListDrafts(ctx context.Context, r *sgitv1.ListDraftsRequest) (*sgitv1.ListDraftsResponse, error) {
	return pass(ctx, r, p.d.up.ListDrafts)
}

func (p *proxy) AbandonDraft(ctx context.Context, r *sgitv1.AbandonDraftRequest) (*sgitv1.Draft, error) {
	return pass(ctx, r, p.d.up.AbandonDraft)
}

func (p *proxy) Land(ctx context.Context, r *sgitv1.LandRequest) (*sgitv1.LandResponse, error) {
	return pass(ctx, r, p.d.up.Land)
}

func (p *proxy) CommitAndLand(ctx context.Context, r *sgitv1.CommitAndLandRequest) (*sgitv1.LandResponse, error) {
	return pass(ctx, r, p.d.up.CommitAndLand)
}

func (p *proxy) Log(ctx context.Context, r *sgitv1.LogRequest) (*sgitv1.LogResponse, error) {
	return pass(ctx, r, p.d.up.Log)
}

func (p *proxy) Blame(ctx context.Context, r *sgitv1.BlameRequest) (*sgitv1.BlameResponse, error) {
	return pass(ctx, r, p.d.up.Blame)
}

func (p *proxy) Diff(ctx context.Context, r *sgitv1.DiffRequest) (*sgitv1.DiffResponse, error) {
	return pass(ctx, r, p.d.up.Diff)
}

func (p *proxy) MergeFile(ctx context.Context, r *sgitv1.MergeFileRequest) (*sgitv1.MergeFileResponse, error) {
	return pass(ctx, r, p.d.up.MergeFile)
}

// UploadBlobs relays the client stream to the upstream.
func (p *proxy) UploadBlobs(stream grpc.ClientStreamingServer[sgitv1.BlobChunk, sgitv1.UploadBlobsResponse]) error {
	out, _, err := forward(stream.Context())
	if err != nil {
		return err
	}
	up, err := p.d.up.UploadBlobs(out)
	if err != nil {
		return err
	}
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			resp, err := up.CloseAndRecv()
			if err != nil {
				return err
			}
			return stream.SendAndClose(resp)
		}
		if err != nil {
			return err
		}
		if err := up.Send(c); err != nil {
			if _, rerr := up.CloseAndRecv(); rerr != nil {
				return rerr
			}
			return err
		}
	}
}

// GetObjects relays the server stream. (Task 4 serves it from the cache.)
func (p *proxy) GetObjects(req *sgitv1.GetObjectsRequest, stream grpc.ServerStreamingServer[sgitv1.Object]) error {
	out, _, err := forward(stream.Context())
	if err != nil {
		return err
	}
	up, err := p.d.up.GetObjects(out, req)
	if err != nil {
		return err
	}
	for {
		o, err := up.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(o); err != nil {
			return err
		}
	}
}
