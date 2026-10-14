// Package wc manages an sg working copy: plain files plus the .sg/ metadata
// directory (config, base, index, draft, conflicts). It never runs git.
package wc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/hermes98761234/sgit/internal/fsutil"
)

const metaDir = ".sg"

// ErrNotWorkingCopy means no complete .sg/ directory was found.
var ErrNotWorkingCopy = errors.New("not inside an sg working copy (no .sg directory found)")

// Config is .sg/config.
type Config struct {
	Server  string   `toml:"server"`
	Project int64    `toml:"project"`        // project id; 0 = a pre-project working copy
	Name    string   `toml:"name,omitempty"` // display cache of the canonical project name
	Sparse  []string `toml:"sparse,omitempty"`
}

// Entry is the index record of a tracked file. SHA and Mode describe the file
// as of Base; Size and MtimeNs cache the stat of the working file at the time
// it was known to match (Size -1 = unknown, always re-hash).
type Entry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	SHA     string `json:"sha"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
}

// WC is an open working copy.
type WC struct {
	Root      string
	Config    Config
	Base      string
	Draft     string
	Index     map[string]Entry
	Conflicts []string
	// Skipped lists the untracked files the last Status left out because the
	// server cannot store their names. It is not saved.
	Skipped []string
}

func realDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Create prepares a new working copy rooted at root (which must exist). It
// refuses when root already holds a complete working copy. Nothing is final
// until Save writes .sg/base.
func Create(root string, cfg Config) (*WC, error) {
	r, err := realDir(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(r, metaDir, "base")); err == nil {
		return nil, fmt.Errorf("%s is already an sg working copy", r)
	}
	if err := os.MkdirAll(filepath.Join(r, metaDir), 0o755); err != nil {
		return nil, err
	}
	return &WC{Root: r, Config: cfg, Index: map[string]Entry{}}, nil
}

// Open finds the working copy containing start by walking up the directories.
func Open(start string) (*WC, error) {
	dir, err := realDir(start)
	if err != nil {
		return nil, err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, metaDir, "base")); err == nil && !fi.IsDir() {
			return load(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotWorkingCopy
		}
		dir = parent
	}
}

func readOptional(p string) (string, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

func load(root string) (*WC, error) {
	m := filepath.Join(root, metaDir)
	w := &WC{Root: root, Index: map[string]Entry{}}
	if _, err := toml.DecodeFile(filepath.Join(m, "config"), &w.Config); err != nil {
		return nil, fmt.Errorf("read .sg/config: %w", err)
	}
	base, err := os.ReadFile(filepath.Join(m, "base"))
	if err != nil {
		return nil, err
	}
	w.Base = strings.TrimSpace(string(base))
	draft, err := readOptional(filepath.Join(m, "draft"))
	if err != nil {
		return nil, err
	}
	w.Draft = strings.TrimSpace(draft)
	conflicts, err := readOptional(filepath.Join(m, "conflicts"))
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(conflicts, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			w.Conflicts = append(w.Conflicts, l)
		}
	}
	f, err := os.Open(filepath.Join(m, "index"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		defer f.Close()
		dec := json.NewDecoder(f)
		for {
			var e Entry
			if err := dec.Decode(&e); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("read .sg/index: %w", err)
			}
			if ValidPath(e.Path) == nil {
				w.Index[e.Path] = e
			}
		}
	}
	return w, nil
}

// SortedPaths returns the tracked paths in order.
func (w *WC) SortedPaths() []string {
	out := make([]string, 0, len(w.Index))
	for p := range w.Index {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Abs returns the absolute filesystem path of repo path p.
func (w *WC) Abs(p string) string { return filepath.Join(w.Root, filepath.FromSlash(p)) }

func (w *WC) writeOrRemove(name string, data []byte) error {
	p := filepath.Join(w.Root, metaDir, name)
	if len(data) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return fsutil.WriteAtomic(p, data, 0o644)
}

// Save writes all metadata. .sg/base is written last: its presence marks a
// complete working copy.
func (w *WC) Save() error {
	m := filepath.Join(w.Root, metaDir)
	if err := os.MkdirAll(m, 0o755); err != nil {
		return err
	}
	var cfg bytes.Buffer
	if err := toml.NewEncoder(&cfg).Encode(w.Config); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(filepath.Join(m, "config"), cfg.Bytes(), 0o644); err != nil {
		return err
	}
	if err := w.SaveIndex(); err != nil {
		return err
	}
	var draft []byte
	if w.Draft != "" {
		draft = []byte(w.Draft + "\n")
	}
	if err := w.writeOrRemove("draft", draft); err != nil {
		return err
	}
	sort.Strings(w.Conflicts)
	var conflicts []byte
	if len(w.Conflicts) > 0 {
		conflicts = []byte(strings.Join(w.Conflicts, "\n") + "\n")
	}
	if err := w.writeOrRemove("conflicts", conflicts); err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(m, "base"), []byte(w.Base+"\n"), 0o644)
}

// SaveIndex writes only .sg/index (atomically). Status uses it to persist a
// refreshed stat cache without rewriting the rest of the metadata.
func (w *WC) SaveIndex() error {
	var idx bytes.Buffer
	enc := json.NewEncoder(&idx)
	for _, p := range w.SortedPaths() {
		if err := enc.Encode(w.Index[p]); err != nil {
			return err
		}
	}
	return fsutil.WriteAtomic(filepath.Join(w.Root, metaDir, "index"), idx.Bytes(), 0o644)
}

// ErrLocked means another sg command holds the working-copy lock.
var ErrLocked = errors.New("another sg command is running in this working copy")

// Lock takes an exclusive, non-blocking lock on .sg/lock. It fails with
// ErrLocked while another process (or another open of the same working copy)
// holds it. The returned func releases the lock.
func (w *WC) Lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(w.Root, metaDir, "lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock .sg/lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
