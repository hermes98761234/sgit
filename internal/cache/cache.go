// Package cache is sgd's content-addressed on-disk object cache with LRU
// eviction. Entries are immutable and keyed by git object SHA; the cache
// stores what it is given (callers verify content).
package cache

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Object is a cached object in the form the server sends it.
type Object struct {
	Type     string
	Size     uint64 // uncompressed size
	ZstdData []byte
}

const magic = "sgc1"

var (
	shaRe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	subdirRe = regexp.MustCompile(`^[0-9a-f]{2}$`)
)

// touchAfter is how old an entry's mtime may get before a read refreshes it.
const touchAfter = time.Minute

type entry struct {
	sha   string
	bytes int64
}

// Cache is safe for concurrent use.
type Cache struct {
	dir   string
	max   int64
	mu    sync.Mutex
	lru   *list.List // front = most recently used
	index map[string]*list.Element
	used  int64
}

func encode(o Object) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s %d\n", magic, o.Type, o.Size)
	b.Write(o.ZstdData)
	return b.Bytes()
}

func decode(data []byte) (Object, error) {
	header, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return Object{}, errors.New("cache: missing header")
	}
	f := strings.Fields(string(header))
	if len(f) != 3 || f[0] != magic {
		return Object{}, errors.New("cache: bad header")
	}
	switch f[1] {
	case "blob", "tree", "commit", "tag":
	default:
		return Object{}, fmt.Errorf("cache: bad object type %q", f[1])
	}
	size, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return Object{}, err
	}
	return Object{Type: f[1], Size: size, ZstdData: body}, nil
}

func (c *Cache) path(sha string) string { return filepath.Join(c.dir, sha[:2], sha[2:]) }

// Open opens (creating if needed) a cache in dir limited to maxBytes. Existing
// entries are indexed in mtime order; leftover temp files from interrupted
// Puts are removed, other files in dir are left alone, and the cache is
// evicted down to the cap.
func Open(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes <= 0 {
		return nil, errors.New("cache: size limit must be positive")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	c := &Cache{dir: dir, max: maxBytes, lru: list.New(), index: map[string]*list.Element{}}
	type found struct {
		sha   string
		size  int64
		mtime time.Time
	}
	var all []found
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		sub := filepath.Dir(p)
		if sub == dir || filepath.Dir(sub) != dir || !subdirRe.MatchString(filepath.Base(sub)) {
			return nil // not inside a cache subdirectory: foreign, leave it alone
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-") {
			_ = os.Remove(p) // leftover temp file from an interrupted Put
			return nil
		}
		key := filepath.Base(sub) + name
		if !shaRe.MatchString(key) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		all = append(all, found{key, fi.Size(), fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].mtime.Equal(all[j].mtime) {
			return all[i].mtime.Before(all[j].mtime)
		}
		return all[i].sha < all[j].sha
	})
	for _, f := range all {
		c.index[f.sha] = c.lru.PushFront(&entry{f.sha, f.size})
		c.used += f.size
	}
	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()
	return c, nil
}

// Has reports whether sha is cached (without reading it).
func (c *Cache) Has(sha string) bool {
	if !shaRe.MatchString(sha) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[sha]
	return ok
}

// Get returns the cached object. Unreadable or malformed entries are removed
// and reported as misses.
func (c *Cache) Get(sha string) (Object, bool) {
	if !shaRe.MatchString(sha) {
		return Object{}, false
	}
	c.mu.Lock()
	el, ok := c.index[sha]
	if ok {
		c.lru.MoveToFront(el)
	}
	c.mu.Unlock()
	if !ok {
		return Object{}, false
	}
	p := c.path(sha)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.mu.Lock()
			if c.index[sha] == el { // the file is gone: forget the entry, touch nothing else
				c.unindexLocked(el)
			}
			c.mu.Unlock()
		}
		return Object{}, false
	}
	o, err := decode(data)
	if err != nil {
		c.mu.Lock()
		if c.index[sha] == el {
			c.removeLocked(el)
		}
		c.mu.Unlock()
		return Object{}, false
	}
	if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > touchAfter {
		now := time.Now()
		_ = os.Chtimes(p, now, now) // persist recency for the next Open
	}
	return o, true
}

// Put stores o under sha (a no-op if already present) and evicts down to the
// cap. An object larger than the cap is not cached and Put returns nil.
func (c *Cache) Put(sha string, o Object) error {
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("cache: invalid key %q", sha)
	}
	data := encode(o)
	if int64(len(data)) > c.max {
		return nil
	}
	c.mu.Lock()
	if el, ok := c.index[sha]; ok {
		c.lru.MoveToFront(el)
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	p := c.path(sha)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+sha[2:]+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.index[sha]; ok { // a concurrent Put of the same object won
		_ = os.Remove(name)
		return nil
	}
	if err := os.Rename(name, p); err != nil {
		_ = os.Remove(name)
		return err
	}
	c.index[sha] = c.lru.PushFront(&entry{sha, int64(len(data))})
	c.used += int64(len(data))
	c.evictLocked()
	return nil
}

// Remove deletes sha from the cache. Only indexed entries are touched.
func (c *Cache) Remove(sha string) {
	if !shaRe.MatchString(sha) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[sha]; ok {
		c.removeLocked(el)
	}
}

// unindexLocked drops el from the index and LRU without touching the file.
func (c *Cache) unindexLocked(el *list.Element) {
	e := el.Value.(*entry)
	c.lru.Remove(el)
	delete(c.index, e.sha)
	c.used -= e.bytes
}

func (c *Cache) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	c.unindexLocked(el)
	_ = os.Remove(c.path(e.sha))
}

func (c *Cache) evictLocked() {
	for c.used > c.max && c.lru.Len() > 0 {
		c.removeLocked(c.lru.Back())
	}
}

// Used is the total size of cached files in bytes.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Max is the size limit in bytes.
func (c *Cache) Max() int64 { return c.max }
