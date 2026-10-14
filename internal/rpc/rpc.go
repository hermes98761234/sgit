// Package rpc holds gRPC options and helpers shared by server, daemon and CLI.
package rpc

import (
	"context"
	"crypto/tls"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	MaxMsgSize = 256 << 20
	// MaxRecvMsgSize bounds what the server will read per message.
	MaxRecvMsgSize = 64 << 20
	ChunkSize      = 1 << 20
)

func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(MaxRecvMsgSize), grpc.MaxSendMsgSize(MaxMsgSize), grpc.MaxConcurrentStreams(256)}
}

// TokenCreds sends "authorization: Bearer <token>" on every call.
type TokenCreds struct {
	Token  string
	Secure bool
}

func (c TokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.Token}, nil
}

func (c TokenCreds) RequireTransportSecurity() bool { return c.Secure }

// DialOptions builds client options. tlsCfg == nil dials plaintext (tests, unix socket).
func DialOptions(token string, tlsCfg *tls.Config) []grpc.DialOption {
	opts := []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMsgSize), grpc.MaxCallSendMsgSize(MaxMsgSize))}
	if tlsCfg != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(TokenCreds{Token: token, Secure: tlsCfg != nil}))
	}
	return opts
}

func chunks(data []byte) [][]byte {
	if len(data) == 0 {
		return [][]byte{nil}
	}
	var out [][]byte
	for len(data) > ChunkSize {
		out = append(out, data[:ChunkSize])
		data = data[ChunkSize:]
	}
	return append(out, data)
}

// UploadBlobs streams blobs to the server and returns their SHAs in order.
func UploadBlobs(ctx context.Context, c sgitv1.ProjectClient, project string, blobs [][]byte) ([]string, error) {
	stream, err := c.UploadBlobs(ctx)
	if err != nil {
		return nil, err
	}
	first := true
	for _, b := range blobs {
		parts := chunks(b)
		for i, part := range parts {
			msg := &sgitv1.BlobChunk{Data: part, EndOfBlob: i == len(parts)-1}
			if first {
				msg.Project, first = project, false
			}
			if err := stream.Send(msg); err != nil {
				_, rerr := stream.CloseAndRecv() // surface the server's status
				if rerr != nil {
					return nil, rerr
				}
				return nil, err
			}
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	return resp.Shas, nil
}
