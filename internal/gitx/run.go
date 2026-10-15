// Package gitx wraps the git plumbing commands sg-server needs. It never
// touches a working tree: all repos are bare and all trees are built through
// temporary index files.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("gitx: not found")
	ErrInvalid  = errors.New("gitx: invalid argument")
	ErrRefMoved = errors.New("gitx: ref moved")
)

const (
	ZeroSHA   = "0000000000000000000000000000000000000000"
	EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	MainRef   = "refs/heads/main"
)

// GitError is a git command that exited non-zero.
type GitError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.ExitCode, strings.TrimSpace(e.Stderr))
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsSHA reports whether s is a full lowercase hex SHA-1.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// baseEnv isolates git from host system/global config (hooks, signing,
// helpers) and from refs/replace, so objects always read as stored.
func baseEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LC_ALL=C",
	)
}

// runGit runs git and returns stdout. On a non-zero exit it returns stdout
// together with a *GitError, because some commands (merge-tree) report
// results through their exit code.
func runGit(ctx context.Context, stdin io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(baseEnv(), extraEnv...)
	cmd.Stdin = stdin
	// A cancelled git may leave children (ssh, git-remote-https) holding the
	// output pipes; WaitDelay stops that from blocking Run forever.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &GitError{Args: args, ExitCode: ee.ExitCode(), Stderr: stderr.String()}
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func exitCode(err error) int {
	var ge *GitError
	if errors.As(err, &ge) {
		return ge.ExitCode
	}
	return -1
}
