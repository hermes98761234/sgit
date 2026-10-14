// Package client dials sg-server for the CLI. Phase 3 adds the sgd socket.
package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/hermes98761234/sgit/internal/creds"
	"github.com/hermes98761234/sgit/internal/rpc"
	"google.golang.org/grpc"
)

// Dial connects to addr ("host:port"). Plaintext (cred.Insecure) is only
// allowed to loopback hosts; otherwise TLS is used with the system roots or
// the pinned CA file.
func Dial(addr string, cred creds.Server) (*grpc.ClientConn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("server address %q must be host:port", addr)
	}
	var tlsCfg *tls.Config
	if cred.Insecure {
		if !isLoopback(host) {
			return nil, fmt.Errorf("insecure (plaintext) connections are only allowed to loopback addresses, not %q", host)
		}
	} else {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		if cred.CAFile != "" {
			pem, err := os.ReadFile(cred.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read CA file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("CA file %s contains no certificates", cred.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
	}
	return grpc.NewClient(addr, rpc.DialOptions(cred.Token, tlsCfg)...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
