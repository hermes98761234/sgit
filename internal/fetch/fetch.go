// Package fetch implements the client side of the Scion-style fetch protocol:
// walk trees level by level, asking the server only for the objects needed
// (honouring the sparse set), then fetch blobs in batches.
package fetch

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"github.com/hermes98761234/sgit/internal/wc"
	"github.com/klauspost/compress/zstd"
)

// Batch is the maximum number of SHAs per GetObjects request.
const Batch = 1000

// MaxObjectSize caps the decompressed size of one object. It is above the
// server's 100 MiB blob limit, so no legitimate object is rejected.
const MaxObjectSize = 128 << 20

// File is a file in a tree listing.
type File struct {
	Mode string
	SHA  string
}

var (
	decOnce sync.Once
	dec     *zstd.Decoder
	decErr  error
)

func decoder() (*zstd.Decoder, error) {
	decOnce.Do(func() { dec, decErr = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxObjectSize)) })
	return dec, decErr
}

type object struct {
	typ  string
	data []byte
}

func uniq(shas []string) []string {
	seen := make(map[string]bool, len(shas))
	var out []string
	for _, s := range shas {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// objects fetches and decompresses objects, Batch SHAs per request.
func objects(ctx context.Context, c sgitv1.ProjectClient, project string, shas []string) (map[string]object, error) {
	d, err := decoder()
	if err != nil {
		return nil, err
	}
	shas = uniq(shas)
	wanted := make(map[string]bool, len(shas))
	for _, s := range shas {
		wanted[s] = true
	}
	out := make(map[string]object, len(shas))
	for start := 0; start < len(shas); start += Batch {
		end := min(start+Batch, len(shas))
		stream, err := c.GetObjects(ctx, &sgitv1.GetObjectsRequest{Project: project, Shas: shas[start:end]})
		if err != nil {
			return nil, err
		}
		for {
			o, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if !wanted[o.Sha] {
				return nil, fmt.Errorf("server returned unrequested object %s", o.Sha)
			}
			if o.Size > MaxObjectSize {
				return nil, fmt.Errorf("object %s: size %d exceeds limit", o.Sha, o.Size)
			}
			data, err := d.DecodeAll(o.ZstdData, make([]byte, 0, o.Size))
			if err != nil {
				return nil, fmt.Errorf("object %s: %w", o.Sha, err)
			}
			if uint64(len(data)) != o.Size {
				return nil, fmt.Errorf("object %s: size mismatch", o.Sha)
			}
			out[o.Sha] = object{typ: o.Type, data: data}
		}
	}
	for _, s := range shas {
		if _, ok := out[s]; !ok {
			return nil, fmt.Errorf("server did not return object %s", s)
		}
	}
	return out, nil
}

func normalizeMode(m string) string {
	switch m {
	case "100755", "120000":
		return m
	}
	return "100644"
}

// Tree lists the files of commit's tree inside the sparse set (nil = all).
// Submodule entries are skipped.
func Tree(ctx context.Context, c sgitv1.ProjectClient, project, commit string, sparse []string) (map[string]File, error) {
	objs, err := objects(ctx, c, project, []string{commit})
	if err != nil {
		return nil, err
	}
	co := objs[commit]
	if co.typ != "commit" {
		return nil, fmt.Errorf("%s is a %s, not a commit", commit, co.typ)
	}
	first, _, _ := strings.Cut(string(co.data), "\n")
	root, ok := strings.CutPrefix(first, "tree ")
	if !ok {
		return nil, fmt.Errorf("commit %s has no tree", commit)
	}
	type dir struct{ path, sha string }
	files := map[string]File{}
	level := []dir{{"", root}}
	for len(level) > 0 {
		shas := make([]string, 0, len(level))
		for _, d := range level {
			shas = append(shas, d.sha)
		}
		trees, err := objects(ctx, c, project, shas)
		if err != nil {
			return nil, err
		}
		var next []dir
		for _, d := range level {
			t := trees[d.sha]
			if t.typ != "tree" {
				return nil, fmt.Errorf("%s is a %s, not a tree", d.sha, t.typ)
			}
			entries, err := objfmt.ParseTree(t.data)
			if err != nil {
				return nil, err
			}
			for _, en := range entries {
				p := en.Name
				if d.path != "" {
					p = d.path + "/" + en.Name
				}
				if strings.Contains(en.Name, "/") {
					return nil, fmt.Errorf("server tree contains invalid path %q: name contains a slash", p)
				}
				if err := wc.ValidPath(p); err != nil {
					return nil, fmt.Errorf("server tree contains invalid path %q: %w", p, err)
				}
				switch {
				case en.IsTree():
					if wc.TreeNeeded(sparse, p) {
						next = append(next, dir{p, en.SHA})
					}
				case en.Mode == "160000":
					// submodule: not materialized
				default:
					if wc.InSparse(sparse, p) {
						if _, dup := files[p]; dup {
							return nil, fmt.Errorf("server tree lists %q twice", p)
						}
						files[p] = File{Mode: normalizeMode(en.Mode), SHA: en.SHA}
					}
				}
			}
		}
		level = next
	}
	return files, nil
}

// Blobs fetches blob contents by SHA, verifying each against its SHA.
func Blobs(ctx context.Context, c sgitv1.ProjectClient, project string, shas []string) (map[string][]byte, error) {
	objs, err := objects(ctx, c, project, shas)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(objs))
	for sha, o := range objs {
		if o.typ != "blob" {
			return nil, fmt.Errorf("%s is a %s, not a blob", sha, o.typ)
		}
		if objfmt.BlobSHA(o.data) != sha {
			return nil, fmt.Errorf("blob %s failed its integrity check", sha)
		}
		out[sha] = o.data
	}
	return out, nil
}
