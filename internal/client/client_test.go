package client

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/testenv"
	"github.com/hermes98761234/sgit/internal/tlsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestDialInsecureLoopback(t *testing.T) {
	e := testenv.Start(t)
	conn := testenv.Must(Dial(e.Addr, creds.Server{Token: e.AdminToken, Insecure: true}))
	defer conn.Close()
	who := testenv.Must(sgitv1.NewAdminClient(conn).WhoAmI(context.Background(), &sgitv1.WhoAmIRequest{}))
	if who.User != "admin" {
		t.Fatalf("WhoAmI = %+v", who)
	}
}

func TestDialInsecureRefusesRemote(t *testing.T) {
	for _, addr := range []string{"10.1.2.3:7419", "sg.example.com:7419"} {
		if _, err := Dial(addr, creds.Server{Token: "sgt_x", Insecure: true}); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("Dial(%s, insecure) = %v, want loopback error", addr, err)
		}
	}
	if _, err := Dial("no-port", creds.Server{}); err == nil {
		t.Error("address without port must fail")
	}
}

func startTLSHealth(t *testing.T, certPEM, keyPEM []byte) string {
	t.Helper()
	pair := testenv.Must(tls.X509KeyPair(certPEM, keyPEM))
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})))
	healthpb.RegisterHealthServer(g, health.NewServer())
	lis := testenv.Must(net.Listen("tcp", "127.0.0.1:0"))
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	return lis.Addr().String()
}

func TestDialTLSPinnedCA(t *testing.T) {
	certPEM, keyPEM := mustCert(t)
	addr := startTLSHealth(t, certPEM, keyPEM)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := testenv.Must(Dial(addr, creds.Server{CAFile: ca}))
	defer conn.Close()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("pinned CA: %v %v", resp, err)
	}

	otherPEM, _ := mustCert(t)
	wrong := filepath.Join(t.TempDir(), "wrong.pem")
	_ = os.WriteFile(wrong, otherPEM, 0o644)
	conn2 := testenv.Must(Dial(addr, creds.Server{CAFile: wrong}))
	defer conn2.Close()
	if _, err := healthpb.NewHealthClient(conn2).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("wrong CA must fail the TLS handshake")
	}
	if _, err := Dial(addr, creds.Server{CAFile: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Fatal("missing CA file must fail")
	}
}

func mustCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	c, k, err := tlsutil.GenerateSelfSigned([]string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c, k
}
