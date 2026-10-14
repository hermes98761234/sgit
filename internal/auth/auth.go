// Package auth implements API tokens, the single Authorize decision point and
// the gRPC interceptors that attach the calling user to the context.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
)

const TokenPrefix = "sgt_"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func randB32(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(b32.EncodeToString(b)), nil
}

// NewToken returns a public token id, the secret token, and the hash to store.
func NewToken() (id, token string, hash []byte, err error) {
	if id, err = randB32(5); err != nil {
		return
	}
	var secret string
	if secret, err = randB32(32); err != nil {
		return
	}
	token = TokenPrefix + secret
	return id, token, Hash(token), nil
}

// Hash is the value stored in the database for a token.
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

type Action int

const (
	ActionRead Action = iota + 1
	ActionWrite
	ActionAdmin
)

func (a Action) String() string {
	switch a {
	case ActionRead:
		return "read"
	case ActionWrite:
		return "write"
	case ActionAdmin:
		return "admin"
	}
	return "unknown"
}

type ACLReader interface {
	ACL(ctx context.Context, userID int64, repo string) (store.Level, error)
}

type TokenLookup interface {
	UserByTokenHash(ctx context.Context, hash []byte) (store.User, error)
}

// Authorize is the single permission decision point. Per-directory ACLs
// would be added here.
func Authorize(ctx context.Context, acl ACLReader, u store.User, repo string, a Action) error {
	if u.Admin {
		return nil
	}
	if a != ActionRead && a != ActionWrite && a != ActionAdmin {
		return status.Error(codes.PermissionDenied, "unknown action")
	}
	if a == ActionAdmin {
		return status.Error(codes.PermissionDenied, "admin role required")
	}
	lvl, err := acl.ACL(ctx, u.ID, repo)
	if err != nil {
		return status.Errorf(codes.Internal, "acl lookup: %v", err)
	}
	need := store.LevelRead
	if a == ActionWrite {
		need = store.LevelWrite
	}
	if lvl < need {
		return status.Errorf(codes.PermissionDenied, "%s access to repo %q denied", a, repo)
	}
	return nil
}

type ctxKey int

const (
	userKey ctxKey = iota
	reqIDKey
)

func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// RequestID returns the per-call id used to correlate client errors with logs.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey).(string)
	return id
}

func withRequestID(ctx context.Context) context.Context {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return context.WithValue(ctx, reqIDKey, hex.EncodeToString(b))
}

const healthPrefix = "/grpc.health.v1.Health/"

// TapHandle rejects, before any message body is read, calls that carry no
// well-formed bearer token. Token validity is still checked by the interceptors.
func TapHandle(ctx context.Context, info *tap.Info) (context.Context, error) {
	if strings.HasPrefix(info.FullMethodName, healthPrefix) {
		return ctx, nil
	}
	for _, v := range info.Header.Get("authorization") {
		if strings.HasPrefix(v, "Bearer "+TokenPrefix) {
			return ctx, nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "missing or malformed token")
}

func authenticate(ctx context.Context, lk TokenLookup) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing token")
	}
	tok, ok := strings.CutPrefix(vals[0], "Bearer ")
	if !ok || !strings.HasPrefix(tok, TokenPrefix) {
		return nil, status.Error(codes.Unauthenticated, "malformed authorization header")
	}
	u, err := lk.UserByTokenHash(ctx, Hash(tok))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.Unauthenticated, "invalid or revoked token")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "token lookup: %v", err)
	}
	return WithUser(ctx, u), nil
}

func UnaryInterceptor(lk TokenLookup) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = withRequestID(ctx)
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(ctx, req)
		}
		ctx, err := authenticate(ctx, lk)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func StreamInterceptor(lk TokenLookup) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := withRequestID(ss.Context())
		if !strings.HasPrefix(info.FullMethod, healthPrefix) {
			var err error
			if ctx, err = authenticate(ctx, lk); err != nil {
				return err
			}
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}
