// Command githttpd serves the bare repos under a directory over HTTPS with
// git http-backend, fetch and push, no auth. It is the plain git host of the
// Docker end-to-end test (test/e2e): sg-server only accepts https and ssh
// remotes, so migrate, import and mirror need a real HTTPS git server.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	root := flag.String("root", "/srv/git", "directory with the bare repos")
	listen := flag.String("listen", ":443", "listen address")
	hosts := flag.String("hosts", "git", "comma-separated certificate host names")
	certOut := flag.String("cert-out", "/gitca/cert.pem", "where to write the certificate for clients to trust")
	flag.Parse()

	cert, certPEM, err := selfSigned(strings.Split(*hosts, ","))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*certOut, certPEM, 0o644); err != nil {
		log.Fatal(err)
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		log.Fatal(err)
	}
	h := &cgi.Handler{
		Path: strings.TrimSpace(string(execPath)) + "/git-http-backend",
		Root: "/",
		Env: []string{
			"GIT_PROJECT_ROOT=" + *root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=e2e", // enables receive-pack (push)
		},
	}
	srv := &http.Server{
		Addr: *listen,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			h.ServeHTTP(w, r)
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	log.Printf("serving %s on %s", *root, *listen)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func selfSigned(hosts []string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: hosts[0]},
		DNSNames:              hosts,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, certPEM, nil
}
