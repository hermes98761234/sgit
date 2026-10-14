package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LogEntry is one commit in a history listing.
type LogEntry struct {
	SHA         string
	AuthorName  string
	AuthorEmail string
	Time        time.Time
	Subject     string
}

// Log lists up to n commits reachable from rev (newest first), optionally
// limited to those touching path.
func (r *Repo) Log(ctx context.Context, rev, path string, n int) ([]LogEntry, error) {
	sha, err := r.ResolveRev(ctx, rev)
	if err != nil {
		return nil, err
	}
	args := []string{"log", "-z", "--format=%H%x1f%an%x1f%ae%x1f%at%x1f%s", "-n", strconv.Itoa(n), sha}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := r.run(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []LogEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		rec = strings.TrimPrefix(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) != 5 {
			return nil, fmt.Errorf("gitx: bad log record %q", rec)
		}
		secs, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, LogEntry{SHA: f[0], AuthorName: f[1], AuthorEmail: f[2], Time: time.Unix(secs, 0).UTC(), Subject: f[4]})
	}
	return entries, nil
}

// BlameLine attributes one line of a file to a commit.
type BlameLine struct {
	SHA    string
	Author string
	Line   int
	Text   string
}

// Blame attributes each line of path at rev.
func (r *Repo) Blame(ctx context.Context, rev, path string) ([]BlameLine, error) {
	sha, err := r.ResolveRev(ctx, rev)
	if err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, nil, "blame", "--porcelain", sha, "--", path)
	if err != nil {
		if ge, ok := err.(*GitError); ok && strings.Contains(ge.Stderr, "no such path") {
			return nil, fmt.Errorf("%w: path %q at %s", ErrNotFound, path, rev)
		}
		return nil, err
	}
	return parseBlame(out), nil
}

func parseBlame(out []byte) []BlameLine {
	authors := map[string]string{}
	var lines []BlameLine
	var cur BlameLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "\t") {
			cur.Text = l[1:]
			cur.Author = authors[cur.SHA]
			lines = append(lines, cur)
			continue
		}
		if f := strings.Fields(l); len(f) >= 3 && IsSHA(f[0]) {
			n, _ := strconv.Atoi(f[2])
			cur = BlameLine{SHA: f[0], Line: n}
			continue
		}
		if a, ok := strings.CutPrefix(l, "author "); ok {
			authors[cur.SHA] = a
		}
	}
	return lines
}

// Diff returns a unified diff between revisions a and b, optionally limited to paths.
func (r *Repo) Diff(ctx context.Context, a, b string, paths []string) ([]byte, error) {
	sa, err := r.ResolveRev(ctx, a)
	if err != nil {
		return nil, err
	}
	sb, err := r.ResolveRev(ctx, b)
	if err != nil {
		return nil, err
	}
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", sa, sb, "--"}, paths...)
	return r.run(ctx, nil, nil, args...)
}
