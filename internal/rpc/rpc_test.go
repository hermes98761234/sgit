package rpc

import (
	"context"
	"testing"
)

func TestTokenCreds(t *testing.T) {
	md, err := TokenCreds{Token: "sgt_x", Secure: true}.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer sgt_x" {
		t.Fatalf("md = %v, %v", md, err)
	}
	if !(TokenCreds{Secure: true}).RequireTransportSecurity() || (TokenCreds{}).RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity mismatch")
	}
}

func TestChunks(t *testing.T) {
	data := make([]byte, 2*ChunkSize+5)
	got := chunks(data)
	if len(got) != 3 || len(got[0]) != ChunkSize || len(got[2]) != 5 {
		t.Fatalf("chunks lens = %d", len(got))
	}
	if got := chunks(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("empty blob must yield one empty chunk, got %d", len(got))
	}
}
