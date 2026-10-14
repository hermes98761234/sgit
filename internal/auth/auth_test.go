package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
)

type fakeACL map[int64]store.Level

func (f fakeACL) ACL(_ context.Context, _ int64, project int64) (store.Level, error) {
	return f[project], nil
}

type fakeTokens map[string]store.User

func (f fakeTokens) UserByTokenHash(_ context.Context, hash []byte) (store.User, error) {
	u, ok := f[string(hash)]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func TestNewToken(t *testing.T) {
	id, tok, hash, err := NewToken()
	if err != nil || len(id) != 8 || !strings.HasPrefix(tok, TokenPrefix) || len(tok) < 50 || string(hash) != string(Hash(tok)) || len(hash) != 32 {
		t.Fatalf("NewToken = %q %q %x %v", id, tok, hash, err)
	}
	_, tok2, _, _ := NewToken()
	if tok == tok2 {
		t.Fatal("tokens not random")
	}
}

func TestAuthorize(t *testing.T) {
	ctx := context.Background()
	const r, w, none = 1, 2, 3
	acl := fakeACL{r: store.LevelRead, w: store.LevelWrite}
	user := store.User{ID: 1, Name: "bob"}
	admin := store.User{ID: 2, Name: "admin", Admin: true}
	for _, c := range []struct {
		project int64
		a       Action
	}{{r, ActionRead}, {none, ActionWrite}, {0, ActionAdmin}, {none, ActionRead}} {
		if err := Authorize(ctx, acl, admin, c.project, c.a); err != nil {
			t.Errorf("admin Authorize(%d,%s) = %v", c.project, c.a, err)
		}
	}
	for _, c := range []struct {
		project int64
		a       Action
	}{{r, ActionRead}, {w, ActionWrite}, {w, ActionRead}} {
		if err := Authorize(ctx, acl, user, c.project, c.a); err != nil {
			t.Errorf("Authorize(%d,%s) = %v", c.project, c.a, err)
		}
	}
	for _, a := range []Action{ActionRead, ActionWrite} {
		if err := Authorize(ctx, acl, user, none, a); !errors.Is(err, ErrNoAccess) {
			t.Errorf("Authorize(no grant,%s) = %v, want ErrNoAccess", a, err)
		}
	}
	if err := Authorize(ctx, acl, user, r, ActionWrite); status.Code(err) != codes.PermissionDenied {
		t.Errorf("read grant + write = %v", err)
	}
	if err := Authorize(ctx, acl, user, 0, ActionAdmin); status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "admin role required" {
		t.Errorf("non-admin admin action = %v", err)
	}
	if err := Authorize(ctx, acl, user, w, Action(0)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("unknown action = %v", err)
	}
}

func TestUnaryInterceptor(t *testing.T) {
	_, tok, hash, _ := NewToken()
	lk := fakeTokens{string(hash): {ID: 7, Name: "alice"}}
	icpt := UnaryInterceptor(lk)
	var seen store.User
	var reqID string
	handler := func(ctx context.Context, _ any) (any, error) {
		seen, _ = UserFrom(ctx)
		reqID = RequestID(ctx)
		return "ok", nil
	}
	call := func(md metadata.MD, method string) error {
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := icpt(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		return err
	}
	if err := call(metadata.Pairs("authorization", "Bearer "+tok), "/sgit.v1.Project/ResolveRev"); err != nil || seen.Name != "alice" || len(reqID) != 12 {
		t.Fatalf("valid token: err=%v user=%+v reqID=%q", err, seen, reqID)
	}
	for name, md := range map[string]metadata.MD{
		"missing":   metadata.MD{},
		"malformed": metadata.Pairs("authorization", "Basic xyz"),
		"unknown":   metadata.Pairs("authorization", "Bearer sgt_doesnotexist"),
	} {
		if err := call(md, "/sgit.v1.Project/ResolveRev"); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: want Unauthenticated, got %v", name, err)
		}
	}
	if err := call(metadata.MD{}, "/grpc.health.v1.Health/Check"); err != nil {
		t.Fatalf("health must skip auth: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

func TestStreamInterceptor(t *testing.T) {
	_, tok, hash, _ := NewToken()
	lk := fakeTokens{string(hash): {ID: 7, Name: "alice"}}
	icpt := StreamInterceptor(lk)
	var called bool
	var seen store.User
	var reqID string
	handler := func(_ any, ss grpc.ServerStream) error {
		called = true
		seen, _ = UserFrom(ss.Context())
		reqID = RequestID(ss.Context())
		return nil
	}
	run := func(md metadata.MD, method string) error {
		called, seen, reqID = false, store.User{}, ""
		ss := fakeStream{ctx: metadata.NewIncomingContext(context.Background(), md)}
		return icpt(nil, ss, &grpc.StreamServerInfo{FullMethod: method}, handler)
	}
	if err := run(metadata.MD{}, "/sgit.v1.Project/GetObjects"); status.Code(err) != codes.Unauthenticated || called {
		t.Fatalf("missing token: want Unauthenticated and no handler call, got err=%v called=%v", err, called)
	}
	if err := run(metadata.Pairs("authorization", "Bearer "+tok), "/sgit.v1.Project/GetObjects"); err != nil || !called || seen.Name != "alice" || len(reqID) != 12 {
		t.Fatalf("valid token: err=%v called=%v user=%+v reqID=%q", err, called, seen, reqID)
	}
	if err := run(metadata.MD{}, "/grpc.health.v1.Health/Watch"); err != nil || !called {
		t.Fatalf("health must skip auth: err=%v called=%v", err, called)
	}
}

func TestTapHandle(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		method string
		md     metadata.MD
		ok     bool
	}{
		{"missing", "/sgit.v1.Project/ResolveRev", metadata.MD{}, false},
		{"basic", "/sgit.v1.Project/ResolveRev", metadata.Pairs("authorization", "Basic xyz"), false},
		{"bearer without prefix", "/sgit.v1.Project/ResolveRev", metadata.Pairs("authorization", "Bearer abc"), false},
		{"bearer sgt", "/sgit.v1.Project/ResolveRev", metadata.Pairs("authorization", "Bearer sgt_abc"), true},
		{"health", "/grpc.health.v1.Health/Check", metadata.MD{}, true},
	}
	for _, c := range cases {
		_, err := TapHandle(ctx, &tap.Info{FullMethodName: c.method, Header: c.md})
		if c.ok != (err == nil) || (err != nil && status.Code(err) != codes.Unauthenticated) {
			t.Errorf("%s: err=%v", c.name, err)
		}
	}
}
