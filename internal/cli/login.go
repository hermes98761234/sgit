package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/client"
	"github.com/hermes98761234/sgit/internal/creds"
)

func cmdLogin(ctx context.Context, a *app, args []string) error {
	const u = "login <host:port> [--token T] [--ca FILE] [--dev-insecure]"
	fs := a.newFlags("login")
	token := fs.String("token", "", "API token (default $SG_TOKEN, else read from stdin)")
	ca := fs.String("ca", "", "PEM file with the server's (self-signed) certificate")
	insecure := fs.Bool("dev-insecure", false, "plaintext connection (loopback servers only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(u)
	}
	server := pos[0]
	if *token == "" {
		*token = os.Getenv("SG_TOKEN")
	}
	tok := strings.TrimSpace(*token)
	if tok == "" {
		fmt.Fprint(a.io.Stderr, "token: ")
		line, _ := bufio.NewReader(a.io.Stdin).ReadString('\n')
		if tok = strings.TrimSpace(line); tok == "" {
			return exitf(ExitError, "no token given (use --token, $SG_TOKEN or stdin)")
		}
	}
	cred := creds.Server{Token: tok, Insecure: *insecure}
	if *ca != "" {
		cred.CAFile = a.abs(*ca)
	}
	conn, err := client.Dial(server, cred)
	if err != nil {
		return exitf(ExitError, "%v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	who, err := sgitv1.NewAdminClient(conn).WhoAmI(ctx, &sgitv1.WhoAmIRequest{})
	if err != nil {
		return err
	}
	f, err := creds.Load()
	if err != nil {
		return err
	}
	f.Servers[server] = cred
	f.Default = server
	if err := creds.Save(f); err != nil {
		return err
	}
	return a.emit(map[string]any{"server": server, "user": who.User, "admin": who.Admin}, func(w io.Writer) {
		fmt.Fprintf(w, "logged in to %s as %s\n", server, who.User)
	})
}
