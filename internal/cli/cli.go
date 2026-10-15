// Package cli implements the sg command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Exit codes (spec §4/§10).
const (
	ExitOK       = 0
	ExitError    = 1
	ExitAuth     = 2
	ExitConflict = 3
	ExitCAS      = 4
)

// IO is the process environment a command runs in.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string // working directory; "" = os.Getwd()
}

type app struct {
	io      IO
	json    bool
	emitted bool
	daemon  string // sgd socket the command talks through, if any
}

type command struct {
	name  string
	usage string
	run   func(ctx context.Context, a *app, args []string) error
}

func commandTable() []command {
	return []command{
		{"login", "login <host:port> [--token T] [--ca FILE] [--dev-insecure]", cmdLogin},
		{"checkout", "checkout <project|alias> [dir] [--path P]... [--at REV] [--server S]", cmdCheckout},
		{"projects", "projects [--server S]", cmdProjects},
		{"status", "status", cmdStatus},
		{"diff", "diff [path...]", cmdDiff},
		{"commit", "commit -m MSG [--title T] [--land] [--draft ID]", cmdCommit},
		{"land", "land [draft-id] [--project P] [--server S]", cmdLand},
		{"drafts", "drafts [--mine] [--status open|landed|abandoned|all] [--project P] [--server S]", cmdDrafts},
		{"show", "show <draft-id|rev> [--project P] [--server S]", cmdShow},
		{"abandon", "abandon [draft-id] [--project P] [--server S]", cmdAbandon},
		{"sync", "sync", cmdSync},
		{"log", "log [path] [-n N] [--rev REV] [--project P] [--server S]", cmdLog},
		{"blame", "blame <path> [--rev REV] [--project P] [--server S]", cmdBlame},
		{"cat", "cat <rev>:<path> [--project P] [--server S]", cmdCat},
		{"sparse", "sparse list | add <path>... | rm <path>...", cmdSparse},
		{"repo", repoUsage, cmdRepo},
		{"project", projectUsage, cmdProject},
		{"user", userUsage, cmdUser},
		{"token", tokenUsage, cmdToken},
		{"acl", aclUsage, cmdACL},
	}
}

// groupCommands take a subcommand as their first argument.
var groupCommands = map[string]bool{"repo": true, "project": true, "user": true, "token": true, "acl": true, "sparse": true}

// hoistGlobalFlags moves --server and --json given before the command to
// the end of the arguments, where the command's own flag parsing reads them:
// `sg --server S repo list` is `sg repo list --server S`.
func hoistGlobalFlags(args []string) ([]string, error) {
	var flags []string
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-h" && args[i] != "--help"; i++ {
		name, _, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case name == "json":
			flags = append(flags, args[i])
		case name == "server" && hasValue:
			flags = append(flags, args[i])
		case name == "server" && i+1 < len(args):
			flags = append(flags, args[i], args[i+1])
			i++
		default:
			return nil, fmt.Errorf("unknown flag %q before the command (only --server and --json may come first)", args[i])
		}
	}
	if len(flags) == 0 {
		return args, nil
	}
	if i == len(args) {
		return nil, fmt.Errorf("no command after %s", strings.Join(flags, " "))
	}
	return append(append([]string{}, args[i:]...), flags...), nil
}

// Run executes one sg command and returns the process exit code.
func Run(ctx context.Context, args []string, stdio IO) int {
	if stdio.Dir == "" {
		if d, err := os.Getwd(); err == nil {
			stdio.Dir = d
		}
	}
	if stdio.Stdin == nil {
		stdio.Stdin = strings.NewReader("")
	}
	a := &app{io: stdio}
	if len(args) == 0 {
		usage(stdio.Stderr)
		return ExitError
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdio.Stdout)
		return ExitOK
	}
	args, err := hoistGlobalFlags(args)
	if err != nil {
		fmt.Fprintf(stdio.Stderr, "sg: %v\n\n", err)
		usage(stdio.Stderr)
		return ExitError
	}
	table := commandTable()
	idx := -1
	for i := range table {
		if table[i].name == args[0] {
			idx = i
		}
	}
	if idx < 0 {
		fmt.Fprintf(stdio.Stderr, "sg: unknown command %q\n\n", args[0])
		usage(stdio.Stderr)
		return ExitError
	}
	if groupCommands[args[0]] && len(args) > 1 && (args[1] == "help" || args[1] == "-h" || args[1] == "--help") {
		fmt.Fprintf(stdio.Stdout, "usage: sg %s\n", table[idx].usage)
		return ExitOK
	}
	err = table[idx].run(ctx, a, args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	code := exitCode(err)
	msg := errorMessage(err)
	if a.daemon != "" && status.Code(err) == codes.Unavailable {
		msg += fmt.Sprintf(" (through sgd at %s: check that sgd reaches the server, with --ca for a self-signed certificate, or set SG_NO_DAEMON=1 to connect directly)", a.daemon)
	}
	if a.json && !a.emitted {
		_ = json.NewEncoder(stdio.Stdout).Encode(map[string]any{"error": msg, "exit_code": code})
	}
	fmt.Fprintf(stdio.Stderr, "sg: %s\n", msg)
	return code
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: sg <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, c := range commandTable() {
		fmt.Fprintf(w, "  sg %s\n", c.usage)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Admin commands need an admin token. acl list shows explicit grants only; admins have implicit access to every project.")
	fmt.Fprintln(w, "Every command accepts --json. Exit codes: 0 ok, 1 error, 2 auth, 3 conflict, 4 main kept moving.")
}

// emit prints v as JSON when --json was given, otherwise calls text.
func (a *app) emit(v any, text func(w io.Writer)) error {
	a.emitted = true
	if a.json {
		enc := json.NewEncoder(a.io.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if text != nil {
		text(a.io.Stdout)
	}
	return nil
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitf(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

func usageErr(u string) error { return exitf(ExitError, "usage: sg %s", u) }

// conflictDetail extracts the land-conflict detail from a gRPC error.
func conflictDetail(err error) *sgitv1.ConflictDetail {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, d := range st.Details() {
		if cd, ok := d.(*sgitv1.ConflictDetail); ok {
			return cd
		}
	}
	return nil
}

func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	st, ok := status.FromError(err)
	if !ok {
		return ExitError
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		return ExitAuth
	case codes.Aborted:
		if conflictDetail(err) != nil {
			return ExitConflict
		}
	case codes.ResourceExhausted:
		return ExitCAS
	case codes.NotFound:
		if strings.HasSuffix(st.Message(), " not found or no access") {
			return ExitAuth
		}
	}
	return ExitError
}

func errorMessage(err error) string {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Error()
	}
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}
