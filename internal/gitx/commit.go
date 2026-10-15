package gitx

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signature is an author or committer identity.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

// Commit is a parsed commit object.
type Commit struct {
	SHA          string
	Tree         string
	Parents      []string
	Author       Signature
	Committer    Signature
	AuthorRaw    string // header value after "author ", byte for byte
	CommitterRaw string // header value after "committer ", byte for byte
	Encoding     string // value of the "encoding" header, "" when absent
	Message      string
}

func gitDate(t time.Time) string { return fmt.Sprintf("%d %s", t.Unix(), t.Format("-0700")) }

// CommitTree creates a commit object (no ref is moved) and returns its SHA.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, msg string, author, committer Signature) (string, error) {
	if err := r.ensureEmptyTree(ctx); err != nil {
		return "", err
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	args := []string{"-c", "commit.gpgSign=false", "commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-")
	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email, "GIT_AUTHOR_DATE=" + gitDate(author.When),
		"GIT_COMMITTER_NAME=" + committer.Name, "GIT_COMMITTER_EMAIL=" + committer.Email, "GIT_COMMITTER_DATE=" + gitDate(committer.When),
	}
	out, err := r.run(ctx, strings.NewReader(msg), env, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadCommit reads and parses a commit object.
func (r *Repo) ReadCommit(ctx context.Context, sha string) (Commit, error) {
	obj, err := r.ReadObject(ctx, sha)
	if err != nil {
		return Commit{}, err
	}
	if obj.Type != "commit" {
		return Commit{}, fmt.Errorf("%w: %s is a %s, not a commit", ErrInvalid, sha, obj.Type)
	}
	return parseCommit(sha, obj.Data)
}

func parseCommit(sha string, data []byte) (Commit, error) {
	header, msg, _ := bytes.Cut(data, []byte("\n\n"))
	c := Commit{SHA: sha, Message: string(msg)}
	for _, line := range strings.Split(string(header), "\n") {
		k, v, _ := strings.Cut(line, " ")
		var err error
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.AuthorRaw = v
			c.Author, err = parseSig(v)
		case "committer":
			c.CommitterRaw = v
			c.Committer, err = parseSig(v)
		case "encoding":
			c.Encoding = v
		}
		if err != nil {
			return Commit{}, fmt.Errorf("gitx: commit %s: %w", sha, err)
		}
	}
	return c, nil
}

// parseSig parses "Name <email> 1700000000 +0100".
func parseSig(v string) (Signature, error) {
	lt, gt := strings.LastIndex(v, "<"), strings.LastIndex(v, ">")
	if lt < 0 || gt < lt {
		return Signature{}, fmt.Errorf("bad signature %q", v)
	}
	rest := strings.Fields(v[gt+1:])
	if len(rest) != 2 || len(rest[1]) != 5 {
		return Signature{}, fmt.Errorf("bad signature date %q", v)
	}
	secs, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return Signature{}, err
	}
	hh, err1 := strconv.Atoi(rest[1][1:3])
	mm, err2 := strconv.Atoi(rest[1][3:5])
	if err1 != nil || err2 != nil {
		return Signature{}, fmt.Errorf("bad tz %q", rest[1])
	}
	off := hh*3600 + mm*60
	if rest[1][0] == '-' {
		off = -off
	}
	return Signature{
		Name:  strings.TrimSpace(v[:lt]),
		Email: v[lt+1 : gt],
		When:  time.Unix(secs, 0).In(time.FixedZone(rest[1], off)),
	}, nil
}
