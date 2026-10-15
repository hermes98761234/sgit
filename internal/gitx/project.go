package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FPCommit is one commit on a first-parent chain.
type FPCommit struct {
	SHA           string
	CommitterUnix int64
}

// MountTree places Tree at Path in a composed root tree. Tree "" is absent.
type MountTree struct {
	Path string
	Tree string
}

// FirstParents lists the first-parent chain of tip, oldest first, stopping
// before stop and its ancestors. stop "" lists the whole chain.
func (r *Repo) FirstParents(ctx context.Context, tip, stop string) ([]FPCommit, error) {
	if !IsSHA(tip) || (stop != "" && !IsSHA(stop)) {
		return nil, fmt.Errorf("%w: first-parents %q %q", ErrInvalid, tip, stop)
	}
	args := []string{"log", "--first-parent", "--reverse", "--no-show-signature", "--format=%H %ct", tip}
	if stop != "" {
		args = append(args, "^"+stop)
	}
	out, err := r.run(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var list []FPCommit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		sha, ts, ok := strings.Cut(line, " ")
		n, perr := strconv.ParseInt(ts, 10, 64)
		if !ok || perr != nil || !IsSHA(sha) {
			return nil, fmt.Errorf("gitx: bad log line %q", line)
		}
		list = append(list, FPCommit{SHA: sha, CommitterUnix: n})
	}
	return list, nil
}

// Subtrees returns, for each commit, the tree at folder ("." = root tree),
// or "" when folder is missing or not a directory. One git process serves
// the whole batch.
func (r *Repo) Subtrees(ctx context.Context, commits []string, folder string) ([]string, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	res, err := r.SubtreesMulti(ctx, commits, []string{folder})
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// SubtreesMulti returns res[f][c]: the tree at folders[f] ("." = root tree)
// in commits[c], or "" when that folder is missing or not a directory. One
// git process serves every folder and commit.
func (r *Repo) SubtreesMulti(ctx context.Context, commits, folders []string) ([][]string, error) {
	res := make([][]string, len(folders))
	if len(commits) == 0 || len(folders) == 0 {
		return res, nil
	}
	var in bytes.Buffer
	for _, f := range folders {
		if strings.ContainsAny(f, "\n\x00") {
			return nil, fmt.Errorf("%w: folder %q", ErrInvalid, f)
		}
		for _, c := range commits {
			if !IsSHA(c) {
				return nil, fmt.Errorf("%w: commit %q", ErrInvalid, c)
			}
			if f == "." {
				fmt.Fprintf(&in, "%s^{tree}\n", c)
			} else {
				fmt.Fprintf(&in, "%s:%s\n", c, f)
			}
		}
	}
	out, err := r.run(ctx, &in, nil, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(commits)*len(folders) {
		return nil, fmt.Errorf("gitx: cat-file returned %d lines for %d commits x %d folders", len(lines), len(commits), len(folders))
	}
	for fi := range folders {
		res[fi] = make([]string, len(commits))
		for ci := range commits {
			if f := strings.Fields(lines[fi*len(commits)+ci]); len(f) == 2 && f[1] == "tree" && IsSHA(f[0]) {
				res[fi][ci] = f[0]
			}
		}
	}
	return res, nil
}

type dirNode struct {
	tree     string // set when a mount sits exactly here
	children map[string]*dirNode
}

// ComposeTree builds a root tree from mounts: every non-empty Tree is placed
// unchanged at its Path; intermediate directories are created with mktree.
// A "." mount is the root itself. All trees must already exist in r.
func (r *Repo) ComposeTree(ctx context.Context, mounts []MountTree) (string, error) {
	root := &dirNode{children: map[string]*dirNode{}}
	for _, m := range mounts {
		if m.Path == "." {
			if m.Tree == "" {
				return r.emptyTree(ctx)
			}
			return m.Tree, nil
		}
		if m.Tree == "" {
			continue
		}
		n := root
		for _, seg := range strings.Split(m.Path, "/") {
			c, ok := n.children[seg]
			if !ok {
				c = &dirNode{children: map[string]*dirNode{}}
				n.children[seg] = c
			}
			n = c
		}
		n.tree = m.Tree
	}
	return r.writeNode(ctx, root)
}

func (r *Repo) writeNode(ctx context.Context, n *dirNode) (string, error) {
	if n.tree != "" {
		return n.tree, nil
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	var in bytes.Buffer
	for _, name := range names {
		sha, err := r.writeNode(ctx, n.children[name])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&in, "040000 tree %s\t%s\x00", sha, name)
	}
	out, err := r.run(ctx, &in, nil, "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SpliceTree returns tree base ("" = empty) with the entry at each folder of
// repl replaced by the given tree; "" or the empty tree removes it, and the
// key "." replaces the whole tree. Only the directories on the path to each
// folder are read and rewritten (one ls-tree and one mktree per directory),
// so the cost does not grow with the size of the rest of the tree. Folders
// must not nest within each other. Missing parent directories are created
// and directories left empty are dropped, as BuildTree does.
func (r *Repo) SpliceTree(ctx context.Context, base string, repl map[string]string) (string, error) {
	if t, ok := repl["."]; ok {
		if len(repl) != 1 {
			return "", fmt.Errorf("%w: splice of \".\" with other folders", ErrInvalid)
		}
		if t == "" || t == EmptyTree {
			return r.emptyTree(ctx)
		}
		return t, nil
	}
	for f, t := range repl {
		if f == "" || strings.ContainsAny(f, "\x00\n") || strings.HasPrefix(f, "/") || strings.HasSuffix(f, "/") || (t != "" && !IsSHA(t)) {
			return "", fmt.Errorf("%w: splice %q -> %q", ErrInvalid, f, t)
		}
		for o := range repl {
			if strings.HasPrefix(o, f+"/") {
				return "", fmt.Errorf("%w: splice folders %q and %q nest", ErrInvalid, f, o)
			}
		}
	}
	return r.spliceDir(ctx, base, repl)
}

func (r *Repo) spliceDir(ctx context.Context, base string, repl map[string]string) (string, error) {
	type entry struct{ mode, typ, sha string }
	entries := map[string]entry{}
	if base != "" && base != EmptyTree {
		out, err := r.run(ctx, nil, nil, "ls-tree", "-z", base)
		if err != nil {
			return "", err
		}
		for _, rec := range strings.Split(string(out), "\x00") {
			if rec == "" {
				continue
			}
			meta, name, ok := strings.Cut(rec, "\t")
			f := strings.Fields(meta) // mode type sha
			if !ok || len(f) != 3 {
				return "", fmt.Errorf("gitx: bad ls-tree record %q", rec)
			}
			entries[name] = entry{f[0], f[1], f[2]}
		}
	}
	sub := map[string]map[string]string{}
	for p, t := range repl {
		name, rest, nested := strings.Cut(p, "/")
		if !nested {
			if t == "" || t == EmptyTree {
				delete(entries, name)
			} else {
				entries[name] = entry{"040000", "tree", t}
			}
			continue
		}
		if sub[name] == nil {
			sub[name] = map[string]string{}
		}
		sub[name][rest] = t
	}
	for name, m := range sub {
		old, had := entries[name]
		child := ""
		if had && old.typ == "tree" {
			child = old.sha
		}
		t, err := r.spliceDir(ctx, child, m)
		if err != nil {
			return "", err
		}
		switch {
		case t != EmptyTree:
			entries[name] = entry{"040000", "tree", t}
		case had && old.typ != "tree":
			// nothing to place under a file: leave the file alone
		default:
			delete(entries, name)
		}
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var in bytes.Buffer
	for _, name := range names {
		e := entries[name]
		fmt.Fprintf(&in, "%s %s %s\t%s\x00", e.mode, e.typ, e.sha, name)
	}
	out, err := r.run(ctx, &in, nil, "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) emptyTree(ctx context.Context) (string, error) {
	out, err := r.run(ctx, strings.NewReader(""), nil, "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CopyObjects copies into dst every object reachable from want (commits,
// trees or blobs) that is not reachable from have and that dst lacks. have
// must exist in src. It returns the number of objects copied.
func CopyObjects(ctx context.Context, src, dst *Repo, want, have []string) (int, error) {
	if len(want) == 0 {
		return 0, nil
	}
	args := []string{"rev-list", "--objects", "--no-object-names"}
	for _, w := range want {
		if !IsSHA(w) {
			return 0, fmt.Errorf("%w: want %q", ErrInvalid, w)
		}
		args = append(args, w)
	}
	if len(have) > 0 {
		args = append(args, "--not")
		for _, h := range have {
			if !IsSHA(h) {
				return 0, fmt.Errorf("%w: have %q", ErrInvalid, h)
			}
			args = append(args, h)
		}
	}
	out, err := src.run(ctx, nil, nil, args...)
	if err != nil {
		return 0, err
	}
	var list bytes.Buffer
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		sha := strings.TrimSpace(sc.Text())
		if sha == "" {
			continue
		}
		switch _, _, err := dst.ObjectInfo(ctx, sha); {
		case err == nil:
			continue
		case !errors.Is(err, ErrNotFound):
			return 0, err
		}
		list.WriteString(sha + "\n")
		n++
	}
	if n == 0 {
		return 0, nil
	}
	pack, err := src.run(ctx, &list, nil, "pack-objects", "--stdout", "-q")
	if err != nil {
		return 0, err
	}
	// Like git's fetch.unpackLimit: a few objects are written loose, which
	// costs no pack fsync and leaves no tiny pack behind per call. Objects
	// are still checked one by one above, so a copy that stops part way is
	// completed by the next one.
	cmd := []string{"index-pack", "--stdin"}
	if n < unpackLimit {
		cmd = []string{"unpack-objects", "-q"}
	}
	if _, err := dst.run(ctx, bytes.NewReader(pack), nil, cmd...); err != nil {
		return 0, err
	}
	return n, nil
}

// unpackLimit is the object count from which CopyObjects keeps a pack.
const unpackLimit = 100

// FormatIdent renders a signature as a raw ident line in UTC.
func FormatIdent(s Signature) string {
	return fmt.Sprintf("%s <%s> %d +0000", s.Name, s.Email, s.When.Unix())
}

// WriteCommit writes a commit object from raw parts (ident lines as stored
// after "author "/"committer "), so a copy of a commit is byte-identical.
func (r *Repo) WriteCommit(ctx context.Context, tree string, parents []string, author, committer, message string) (string, error) {
	if !IsSHA(tree) || strings.ContainsAny(author+committer, "\n\x00") || author == "" || committer == "" {
		return "", fmt.Errorf("%w: commit header", ErrInvalid)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		if !IsSHA(p) {
			return "", fmt.Errorf("%w: parent %q", ErrInvalid, p)
		}
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s", author, committer, message)
	out, err := r.run(ctx, strings.NewReader(b.String()), nil, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DiffTree lists file-level changes from commit a to commit b, sorted by
// path, as Changes that BuildTree can apply on a.
func (r *Repo) DiffTree(ctx context.Context, a, b string) ([]Change, error) {
	if !IsSHA(a) || !IsSHA(b) {
		return nil, fmt.Errorf("%w: diff-tree %q %q", ErrInvalid, a, b)
	}
	out, err := r.run(ctx, nil, nil, "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", a, b)
	if err != nil {
		return nil, err
	}
	recs := strings.Split(string(out), "\x00")
	var res []Change
	for i := 0; i+1 < len(recs); i += 2 {
		meta, p := recs[i], recs[i+1]
		f := strings.Fields(strings.TrimPrefix(meta, ":")) // oldmode newmode oldsha newsha status
		if len(f) != 5 {
			return nil, fmt.Errorf("gitx: bad diff-tree record %q", meta)
		}
		switch f[4][0] {
		case 'D':
			res = append(res, Change{Path: p, Op: OpDelete})
		case 'A':
			res = append(res, Change{Path: p, Op: OpAdd, BlobSHA: f[3], Mode: f[1]})
		default: // M, T
			res = append(res, Change{Path: p, Op: OpModify, BlobSHA: f[3], Mode: f[1]})
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Path < res[j].Path })
	return res, nil
}
