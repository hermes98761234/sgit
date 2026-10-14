package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"regexp"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/cache"
	"github.com/hermes98761234/sgit/internal/fetch"
	"github.com/hermes98761234/sgit/internal/objfmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxBatch = 10000

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

type aclKey struct {
	token [32]byte
	repo  string
}

// call is one upstream fetch of an object that other requests may wait on.
type call struct {
	done chan struct{}
	obj  cache.Object
	err  error
}

// authorize checks (or recalls, for ACLTTL) that the caller may read repo by
// probing the upstream with an empty HasObjects using the caller's token.
func (d *Daemon) authorize(ctx context.Context, repo string) (context.Context, error) {
	out, tok, err := forward(ctx)
	if err != nil {
		return nil, err
	}
	key := aclKey{sha256.Sum256([]byte(tok)), repo}
	now := time.Now()
	d.mu.Lock()
	exp, ok := d.acl[key]
	d.mu.Unlock()
	if ok && now.Before(exp) {
		return out, nil
	}
	if _, err := d.up.HasObjects(out, &sgitv1.HasObjectsRequest{Project: repo}); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if len(d.acl) > 1024 {
		for k, e := range d.acl {
			if now.After(e) {
				delete(d.acl, k)
			}
		}
	}
	d.acl[key] = now.Add(d.cfg.ACLTTL)
	d.mu.Unlock()
	return out, nil
}

// verify decompresses o and checks that its content hashes to sha.
func (d *Daemon) verify(sha string, o cache.Object) error {
	if o.Size > fetch.MaxObjectSize {
		return fmt.Errorf("object %s too large (%d bytes)", sha, o.Size)
	}
	data, err := d.dec.DecodeAll(o.ZstdData, make([]byte, 0, o.Size))
	if err != nil {
		return fmt.Errorf("object %s: %w", sha, err)
	}
	if uint64(len(data)) != o.Size || objfmt.ObjectSHA(o.Type, data) != sha {
		return fmt.Errorf("object %s failed verification", sha)
	}
	return nil
}

// cached returns a verified cache entry; bad entries are dropped.
func (d *Daemon) cached(sha string) (cache.Object, bool) {
	o, ok := d.cfg.Cache.Get(sha)
	if !ok {
		return cache.Object{}, false
	}
	if err := d.verify(sha, o); err != nil {
		d.cfg.Log.Warn("dropping bad cache entry", "sha", sha, "err", err)
		d.cfg.Cache.Remove(sha)
		return cache.Object{}, false
	}
	return o, true
}

func toPB(sha string, o cache.Object) *sgitv1.Object {
	return &sgitv1.Object{Sha: sha, Type: o.Type, Size: o.Size, ZstdData: o.ZstdData}
}

// fetch returns the objects for shas from upstream, sharing in-flight fetches
// of the same (repo, sha) with concurrent requests.
func (d *Daemon) fetch(ctx context.Context, repo string, shas []string) (map[string]cache.Object, error) {
	d.mu.Lock()
	calls := make(map[string]*call, len(shas))
	var mine []string
	for _, s := range shas {
		key := repo + "\x00" + s
		if c, ok := d.inflight[key]; ok {
			calls[s] = c
			continue
		}
		c := &call{done: make(chan struct{})}
		d.inflight[key] = c
		calls[s] = c
		mine = append(mine, s)
	}
	d.mu.Unlock()
	if len(mine) > 0 {
		// Detached from this caller's cancellation: other requests may be
		// waiting on these objects. The caller's token travels in the values.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		d.fetchUpstream(fctx, repo, mine)
		cancel()
	}
	out := make(map[string]cache.Object, len(shas))
	for s, c := range calls {
		select {
		case <-c.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c.err != nil {
			return nil, c.err
		}
		out[s] = c.obj
	}
	return out, nil
}

// fetchUpstream fetches shas from upstream, verifies and caches them, and
// completes their in-flight calls.
func (d *Daemon) fetchUpstream(ctx context.Context, repo string, shas []string) {
	want := make(map[string]bool, len(shas))
	for _, s := range shas {
		want[s] = true
	}
	got := make(map[string]cache.Object, len(shas))
	// A previous owner may have cached these between our cache miss and our
	// in-flight registration; do not fetch them again.
	all := shas
	shas = nil
	for _, s := range all {
		if o, ok := d.cached(s); ok {
			got[s] = o
		} else {
			shas = append(shas, s)
		}
	}
	var ferr error
	for start := 0; start < len(shas) && ferr == nil; start += fetch.Batch {
		end := min(start+fetch.Batch, len(shas))
		stream, err := d.up.GetObjects(ctx, &sgitv1.GetObjectsRequest{Project: repo, Shas: shas[start:end]})
		if err != nil {
			ferr = err
			break
		}
		for {
			o, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				ferr = err
				break
			}
			if !want[o.Sha] {
				ferr = status.Errorf(codes.Internal, "upstream returned unrequested object %s", o.Sha)
				break
			}
			obj := cache.Object{Type: o.Type, Size: o.Size, ZstdData: o.ZstdData}
			if err := d.verify(o.Sha, obj); err != nil {
				ferr = status.Errorf(codes.Internal, "%v", err)
				break
			}
			if err := d.cfg.Cache.Put(o.Sha, obj); err != nil {
				d.cfg.Log.Warn("cache put failed", "sha", o.Sha, "err", err)
			}
			got[o.Sha] = obj
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range all {
		key := repo + "\x00" + s
		c := d.inflight[key]
		delete(d.inflight, key)
		switch o, ok := got[s]; {
		case ok:
			c.obj = o
		case ferr != nil:
			c.err = ferr
		default:
			c.err = status.Errorf(codes.Internal, "upstream did not return object %s", s)
		}
		close(c.done)
	}
}
