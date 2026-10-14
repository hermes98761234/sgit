// Package servercmd implements the sg-server command line.
package servercmd

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/store"
	"github.com/hermes98761234/sgit/internal/tlsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const usage = `usage: sg-server <command> [flags]

commands:
  init       create data dir, database and initial admin token
  serve      run the gRPC server
  gen-cert   write a self-signed TLS certificate and key
  health     probe a local server (exit 0 when SERVING)
`

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	return os.Getenv(key) == "1" || strings.EqualFold(os.Getenv(key), "true")
}

// Run executes one sg-server subcommand and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "gen-cert":
		return runGenCert(args[1:], stdout, stderr)
	case "health":
		return runHealth(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", env("SG_DATA_DIR", "./sgdata"), "data directory")
	adminName := fs.String("admin-name", "admin", "name of the initial admin user")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := os.MkdirAll(filepath.Join(*data, "repos"), 0o750); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	st, err := store.Open(filepath.Join(*data, "sg.db"))
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	defer st.Close()
	n, err := st.CountUsers(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	if n > 0 {
		fmt.Fprintln(stdout, "already initialized")
		return 0
	}
	id, tok, hash, err := auth.NewToken()
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	// Write the token file first: if the DB insert fails we remove it, so a
	// half-finished init never leaves a user whose token is unrecoverable.
	tokenPath := filepath.Join(*data, "admin-token")
	if err := writeTokenFile(tokenPath, tok+"\n"); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	if _, err := st.CreateUserWithToken(ctx, *adminName, *adminName+"@sgit.local", true, id, hash); err != nil {
		os.Remove(tokenPath)
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	fmt.Fprintf(stdout, "initialized %s\nadmin token (shown once, also saved to %s):\n%s\n", *data, tokenPath, tok)
	return 0
}

func writeTokenFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func runServe(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", env("SG_DATA_DIR", "./sgdata"), "data directory")
	listen := fs.String("listen", env("SG_LISTEN", ":7419"), "listen address")
	certFile := fs.String("tls-cert", env("SG_TLS_CERT", ""), "TLS certificate (PEM)")
	keyFile := fs.String("tls-key", env("SG_TLS_KEY", ""), "TLS key (PEM)")
	insecureDev := fs.Bool("dev-insecure", envBool("SG_DEV_INSECURE"), "plaintext; loopback listen address only")
	maxBlobMB, _ := strconv.Atoi(env("SG_MAX_BLOB_MB", "100"))
	fs.IntVar(&maxBlobMB, "max-blob-mb", maxBlobMB, "max blob size in MiB")
	logLevel := fs.String("log-level", env("SG_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))

	var extra []grpc.ServerOption
	if *insecureDev {
		if !isLoopback(*listen) {
			fmt.Fprintln(stderr, "serve: --dev-insecure is only allowed on a loopback listen address")
			return 2
		}
	} else {
		if *certFile == "" || *keyFile == "" {
			fmt.Fprintln(stderr, "serve: --tls-cert and --tls-key are required (or --dev-insecure on loopback)")
			return 2
		}
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			fmt.Fprintln(stderr, "serve: load tls:", err)
			return 2
		}
		extra = append(extra, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})))
	}

	dbPath := filepath.Join(*data, "sg.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "serve: %s not initialized; run `sg-server init --data %s`\n", *data, *data)
		return 1
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	defer st.Close()
	srv, err := server.New(server.Config{DataDir: *data, MaxBlobBytes: int64(maxBlobMB) << 20}, st, log)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	defer srv.Close()
	g := server.NewGRPC(st, extra...)
	srv.Register(g)
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	log.Info("listening", "addr", lis.Addr().String(), "tls", !*insecureDev, "data", *data)
	if err := g.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

func runGenCert(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen-cert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hosts := fs.String("hosts", env("SG_TLS_HOSTS", "localhost,127.0.0.1"), "comma-separated DNS names / IPs")
	outCert := fs.String("out-cert", "", "certificate output path")
	outKey := fs.String("out-key", "", "key output path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outCert == "" || *outKey == "" {
		fmt.Fprintln(stderr, "gen-cert: --out-cert and --out-key are required")
		return 2
	}
	var hs []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hs = append(hs, h)
		}
	}
	certPEM, keyPEM, err := tlsutil.GenerateSelfSigned(hs, time.Now())
	if err == nil {
		err = os.WriteFile(*outCert, certPEM, 0o644)
	}
	if err == nil {
		err = os.WriteFile(*outKey, keyPEM, 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "gen-cert:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s and %s for %s\n", *outCert, *outKey, strings.Join(hs, ","))
	return 0
}

// healthAddr turns a listen address like ":7419" into a dialable local address.
func healthAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:7419"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func runHealth(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", healthAddr(env("SG_LISTEN", ":7419")), "server address")
	insecureDev := fs.Bool("dev-insecure", envBool("SG_DEV_INSECURE"), "plaintext")
	timeout := fs.Duration("timeout", 3*time.Second, "timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var tlsCfg *tls.Config
	if !*insecureDev {
		// Local liveness probe only: certificate identity is not checked here.
		tlsCfg = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	}
	conn, err := grpc.NewClient(*addr, rpc.DialOptions("", tlsCfg)...)
	if err != nil {
		fmt.Fprintln(stderr, "health:", err)
		return 1
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		fmt.Fprintln(stderr, "health:", err)
		return 1
	}
	fmt.Fprintln(stdout, resp.Status.String())
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		return 1
	}
	return 0
}
