package cache

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func sha(i int) string { return fmt.Sprintf("%040x", i+1) }

func obj(n int) Object {
	return Object{Type: "blob", Size: uint64(n), ZstdData: bytes.Repeat([]byte{'z'}, n)}
}

func entrySize(o Object) int64 { return int64(len(encode(o))) }

func TestPutGetPersist(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	o := obj(10)
	if err := c.Put(sha(1), o); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get(sha(1))
	if !ok || got.Type != "blob" || got.Size != 10 || !bytes.Equal(got.ZstdData, o.ZstdData) || !c.Has(sha(1)) {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if c.Used() != entrySize(o) {
		t.Fatalf("Used = %d, want %d", c.Used(), entrySize(o))
	}
	fi, err := os.Stat(filepath.Join(dir, sha(1)[:2], sha(1)[2:]))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi, err)
	}
	c2, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Get(sha(1)); !ok || c2.Used() != c.Used() {
		t.Fatal("entry did not survive reopening")
	}
	if _, ok := c2.Get(sha(2)); ok {
		t.Fatal("unknown key must miss")
	}
}

func TestInvalidKeys(t *testing.T) {
	c, _ := Open(t.TempDir(), 1<<20)
	for _, k := range []string{"", "../etc/passwd", "ABCDEF0123456789abcdef0123456789abcdef01", "abc", sha(1) + "\n"} {
		if err := c.Put(k, obj(1)); err == nil {
			t.Errorf("Put(%q) must fail", k)
		}
		if _, ok := c.Get(k); ok || c.Has(k) {
			t.Errorf("Get(%q) must miss", k)
		}
	}
}

func TestLRUEviction(t *testing.T) {
	o := obj(100)
	c, _ := Open(t.TempDir(), 2*entrySize(o))
	_ = c.Put(sha(1), o)
	_ = c.Put(sha(2), o)
	if _, ok := c.Get(sha(1)); !ok { // 1 is now most recent
		t.Fatal("1 missing")
	}
	_ = c.Put(sha(3), o)
	if c.Has(sha(2)) || !c.Has(sha(1)) || !c.Has(sha(3)) {
		t.Fatalf("wrong eviction: has1=%v has2=%v has3=%v", c.Has(sha(1)), c.Has(sha(2)), c.Has(sha(3)))
	}
	if c.Used() > c.Max() {
		t.Fatalf("over cap: %d > %d", c.Used(), c.Max())
	}
}

func TestCorruptEntryIsAMiss(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 1<<20)
	_ = c.Put(sha(1), obj(10))
	p := filepath.Join(dir, sha(1)[:2], sha(1)[2:])
	if err := os.WriteFile(p, []byte("garbage without header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(sha(1)); ok {
		t.Fatal("corrupt entry must miss")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) || c.Has(sha(1)) || c.Used() != 0 {
		t.Fatal("corrupt entry must be removed")
	}
}

func TestOpenEvictsAndCleans(t *testing.T) {
	dir := t.TempDir()
	o := obj(100)
	c, _ := Open(dir, 10*entrySize(o))
	for i := 0; i < 3; i++ {
		_ = c.Put(sha(i), o)
		old := time.Now().Add(time.Duration(i-10) * time.Minute) // sha(0) oldest
		p := filepath.Join(dir, sha(i)[:2], sha(i)[2:])
		_ = os.Chtimes(p, old, old)
	}
	stray := filepath.Join(dir, sha(0)[:2], "."+sha(0)[2:]+".tmp-123")
	_ = os.WriteFile(stray, []byte("x"), 0o600)
	c2, err := Open(dir, 2*entrySize(o))
	if err != nil {
		t.Fatal(err)
	}
	if c2.Has(sha(0)) || !c2.Has(sha(1)) || !c2.Has(sha(2)) {
		t.Fatal("Open must evict the least recently used entries down to the cap")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("stray temp files must be removed on Open")
	}
	if _, err := Open(dir, 0); err == nil {
		t.Fatal("a non-positive cap must be rejected")
	}
}

func TestOpenLeavesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := []string{"notes.txt", filepath.Join("photos", "img.jpg"), filepath.Join("ab", "readme")}
	for _, f := range foreign {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	o := obj(10)
	if err := c.Put(sha(1), o); err != nil {
		t.Fatal(err)
	}
	for _, f := range foreign {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("foreign file %s was removed: %v", f, err)
		}
	}
	if c.Used() != entrySize(o) {
		t.Fatalf("Used = %d, want %d (foreign files must not count)", c.Used(), entrySize(o))
	}
}

func TestPutOversizeSkipped(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 100)
	if err := c.Put(sha(1), obj(1000)); err != nil {
		t.Fatal(err)
	}
	if c.Has(sha(1)) || c.Used() != 0 {
		t.Fatalf("oversize object cached: has=%v used=%d", c.Has(sha(1)), c.Used())
	}
	if _, err := os.Stat(filepath.Join(dir, sha(1)[:2], sha(1)[2:])); !os.IsNotExist(err) {
		t.Fatalf("oversize object written to disk: %v", err)
	}
}

func TestRemoveUpdatesUsed(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 1<<20)
	o := obj(10)
	_ = c.Put(sha(1), o)
	_ = c.Put(sha(2), o)
	before := c.Used()
	c.Remove(sha(1))
	if c.Has(sha(1)) || c.Used() != before-entrySize(o) {
		t.Fatalf("after Remove: has=%v used=%d, want used=%d", c.Has(sha(1)), c.Used(), before-entrySize(o))
	}
	if _, err := os.Stat(filepath.Join(dir, sha(1)[:2], sha(1)[2:])); !os.IsNotExist(err) {
		t.Fatalf("removed entry's file still exists: %v", err)
	}
	if !c.Has(sha(2)) {
		t.Fatal("other entry was lost")
	}
}

func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	o := obj(100)
	c, _ := Open(dir, 5*entrySize(o))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 200; i++ {
				k := sha(r.Intn(20))
				switch r.Intn(3) {
				case 0:
					if err := c.Put(k, o); err != nil {
						t.Errorf("Put: %v", err)
					}
				case 1:
					c.Get(k)
				default:
					c.Remove(k)
				}
			}
		}(int64(g))
	}
	wg.Wait()
	var onDisk int64
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			onDisk += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Used() != onDisk {
		t.Fatalf("Used = %d, files on disk total %d", c.Used(), onDisk)
	}
	if c.Used() > c.Max() {
		t.Fatalf("over cap: %d > %d", c.Used(), c.Max())
	}
}
