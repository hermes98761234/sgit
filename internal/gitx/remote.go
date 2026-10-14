package gitx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrRejected means the remote refused a push, typically because its main no
// longer matched the --force-with-lease expectation.
var ErrRejected = errors.New("gitx: push rejected")

var scpRemoteRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9][A-Za-z0-9.-]*:[^:]+$`)

// ValidateRemote checks a URL that import or mirroring will hand to git. It
// accepts https://, ssh:// and user@host:path remotes, and file:// URLs or
// absolute paths only when allowLocal is set. Credentials in the URL are
// refused: the URL is stored in the database and shown by repo status.
func ValidateRemote(raw string, allowLocal bool) error {
	bad := func(why string) error { return fmt.Errorf("%w: remote %q: %s", ErrInvalid, raw, why) }
	if raw == "" || len(raw) > 2048 {
		return bad("must be 1-2048 bytes")
	}
	if strings.HasPrefix(raw, "-") {
		return bad("must not start with '-'")
	}
	for _, c := range raw {
		if c <= ' ' || c > 0x7e {
			return bad("must be printable ASCII without spaces")
		}
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return bad("not a valid URL")
		}
		if !strings.HasPrefix(raw, u.Scheme+"://") {
			return bad("scheme must be lowercase https or ssh")
		}
		switch u.Scheme {
		case "https":
			if u.User != nil {
				return bad("credentials in the URL are not allowed; use GIT_ASKPASS or an SSH key")
			}
		case "ssh":
			if _, ok := u.User.Password(); ok {
				return bad("passwords in the URL are not allowed; use an SSH key")
			}
			if strings.HasPrefix(u.User.Username(), "-") {
				return bad("invalid user name")
			}
		case "file":
			if !allowLocal {
				return bad("local remotes are disabled")
			}
			return nil
		default:
			return bad("scheme must be https or ssh")
		}
		if u.RawQuery != "" || u.Fragment != "" || strings.Contains(raw, "#") || strings.Contains(raw, "?") {
			return bad("query strings and fragments are not allowed")
		}
		if h := u.Hostname(); h == "" || strings.HasPrefix(h, "-") {
			return bad("missing or invalid host")
		}
		return nil
	}
	if strings.Contains(raw, "::") {
		return bad("remote helpers are not allowed")
	}
	if filepath.IsAbs(raw) {
		if !allowLocal {
			return bad("local remotes are disabled")
		}
		return nil
	}
	if scpRemoteRe.MatchString(raw) {
		return nil
	}
	return bad("use https://, ssh:// or user@host:path")
}

// remoteEnv limits the transports git may use for a remote, including any
// redirects it follows.
func remoteEnv(allowLocal bool) []string {
	p := "https:ssh"
	if allowLocal {
		p += ":file"
	}
	env := []string{"GIT_ALLOW_PROTOCOL=" + p}
	// GIT_SSH_COMMAND would take precedence over an operator's GIT_SSH.
	if os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// InitBare creates an empty bare repo at dir whose HEAD names main.
func InitBare(ctx context.Context, dir string) error {
	_, err := runGit(ctx, nil, nil, "init", "--bare", "--quiet", "--initial-branch=main", dir)
	return err
}

// Import creates a bare repo at dir holding remote's main and release/*
// branches (no tags) and returns main. On error the caller removes dir.
func Import(ctx context.Context, dir, remote string, allowLocal bool) (string, error) {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return "", err
	}
	if err := InitBare(ctx, dir); err != nil {
		return "", err
	}
	r := Open(dir)
	defer r.Close()
	_, err := r.run(ctx, nil, remoteEnv(allowLocal), "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote,
		"+"+MainRef+":"+MainRef, "+refs/heads/release/*:refs/heads/release/*")
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref "+MainRef) {
			return "", fmt.Errorf("%w: %s has no main branch", ErrInvalid, remote)
		}
		return "", err
	}
	return r.ReadRef(ctx, MainRef)
}

// Push sets remote's main to sha, but only while the remote's main is still
// expect ("" = main must not exist yet). A stale expectation returns an error
// wrapping ErrRejected and leaves the remote untouched.
func (r *Repo) Push(ctx context.Context, remote, sha, expect string, allowLocal bool) error {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return err
	}
	if !IsSHA(sha) || (expect != "" && !IsSHA(expect)) {
		return fmt.Errorf("%w: push %q over %q", ErrInvalid, sha, expect)
	}
	out, err := r.run(ctx, nil, remoteEnv(allowLocal), "push", "--porcelain", "--no-verify",
		"--force-with-lease="+MainRef+":"+expect, remote, sha+":"+MainRef)
	var ge *GitError
	if errors.As(err, &ge) && (strings.Contains(string(out), "[rejected]") || strings.Contains(ge.Stderr, "[rejected]")) {
		return fmt.Errorf("%w: %s", ErrRejected, strings.Join(strings.Fields(string(out)), " "))
	}
	return err
}

// RemoteRef returns the SHA that ref (a full ref name) points to on remote,
// or "" when the remote has no such ref.
func RemoteRef(ctx context.Context, remote, ref string, allowLocal bool) (string, error) {
	if err := ValidateRemote(remote, allowLocal); err != nil {
		return "", err
	}
	out, err := runGit(ctx, nil, append(remoteEnv(allowLocal), "GIT_DIR="+os.DevNull), "ls-remote", "--refs", remote, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && name == ref && IsSHA(sha) {
			return sha, nil
		}
	}
	return "", nil
}

// ErrSummary is a one-line description of err for operators: git's stderr
// for a *GitError, err.Error() otherwise, with whitespace collapsed and the
// result capped at 500 bytes.
func ErrSummary(err error) string {
	msg := err.Error()
	var ge *GitError
	if errors.As(err, &ge) && strings.TrimSpace(ge.Stderr) != "" {
		msg = ge.Stderr
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:497], "") + "…"
	}
	return msg
}
