package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	certPEM, keyPEM, err := GenerateSelfSigned([]string{"localhost", "127.0.0.1", "sg.example"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("keypair: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.IPAddresses) != 1 || len(cert.DNSNames) != 2 || !cert.IsCA {
		t.Fatalf("SANs ip=%v dns=%v ca=%v", cert.IPAddresses, cert.DNSNames, cert.IsCA)
	}
	if err := cert.VerifyHostname("sg.example"); err != nil {
		t.Fatal(err)
	}
}
