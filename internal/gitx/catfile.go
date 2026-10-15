package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Object is a raw git object.
type Object struct {
	SHA  string
	Type string
	Data []byte
}

type catFile struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	gen  int // Repo.gen when started
	once sync.Once
}

func startCatFile(dir string) (*catFile, error) {
	cmd := exec.Command("git", "--git-dir="+dir, "cat-file", "--batch-command")
	cmd.Env = baseEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 64<<10)}, nil
}

func (c *catFile) close() {
	c.once.Do(func() {
		_ = c.in.Close()
		_ = c.cmd.Wait()
	})
}

// request sends "<command> <sha>" and parses the reply. command is "info" or "contents".
func (c *catFile) request(command, sha string, withBody bool) (typ string, size int64, body []byte, err error) {
	if _, err = fmt.Fprintf(c.in, "%s %s\n", command, sha); err != nil {
		return "", 0, nil, err
	}
	var line string
	line, err = c.out.ReadString('\n')
	if err != nil {
		return "", 0, nil, err
	}
	f := strings.Fields(line)
	if len(f) == 2 && f[1] == "missing" {
		return "", 0, nil, fmt.Errorf("%w: object %s", ErrNotFound, sha)
	}
	if len(f) != 3 {
		return "", 0, nil, fmt.Errorf("gitx: unexpected cat-file header %q", line)
	}
	size, err = strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return "", 0, nil, err
	}
	if !withBody {
		return f[1], size, nil, nil
	}
	buf := make([]byte, size+1) // body + trailing LF
	if _, err = io.ReadFull(c.out, buf); err != nil {
		return "", 0, nil, err
	}
	return f[1], size, buf[:size], nil
}

// acquire takes a slot token, then reuses an idle process or starts one.
// Every failure path returns the token, so a waiter is never stranded.
func (r *Repo) acquire(ctx context.Context) (*catFile, error) {
	select {
	case <-r.slots:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.slots <- struct{}{} // never blocks: a token was just taken
		return nil, errors.New("gitx: repo closed")
	}
	if n := len(r.idle); n > 0 {
		c := r.idle[n-1]
		r.idle = r.idle[:n-1]
		r.mu.Unlock()
		return c, nil
	}
	c, err := startCatFile(r.dir)
	if err != nil {
		r.mu.Unlock()
		r.slots <- struct{}{}
		return nil, err
	}
	c.gen = r.gen
	r.procs = append(r.procs, c)
	r.mu.Unlock()
	return c, nil
}

// release returns a process to the idle list when it is healthy and the repo
// is open. Otherwise the process is closed and forgotten. The token always goes
// back, so a waiter blocked in acquire wakes up.
func (r *Repo) release(c *catFile, healthy bool) {
	r.mu.Lock()
	if healthy && !r.closed && c.gen == r.gen {
		r.idle = append(r.idle, c)
		r.mu.Unlock()
	} else {
		for i, p := range r.procs {
			if p == c {
				r.procs = append(r.procs[:i], r.procs[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
		c.close()
	}
	r.slots <- struct{}{} // never blocks: this caller held a token
}

func (r *Repo) withCatFile(ctx context.Context, fn func(*catFile) error) error {
	c, err := r.acquire(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	r.release(c, err == nil || errors.Is(err, ErrNotFound))
	return err
}

// ReadObject returns the object's type and content, or ErrNotFound.
func (r *Repo) ReadObject(ctx context.Context, sha string) (Object, error) {
	if !IsSHA(sha) {
		return Object{}, fmt.Errorf("%w: bad object id %q", ErrInvalid, sha)
	}
	var obj Object
	err := r.withCatFile(ctx, func(c *catFile) error {
		typ, _, body, err := c.request("contents", sha, true)
		obj = Object{SHA: sha, Type: typ, Data: body}
		return err
	})
	return obj, err
}

// ObjectInfo returns the object's type and size without reading its content.
func (r *Repo) ObjectInfo(ctx context.Context, sha string) (string, int64, error) {
	if !IsSHA(sha) {
		return "", 0, fmt.Errorf("%w: bad object id %q", ErrInvalid, sha)
	}
	var typ string
	var size int64
	err := r.withCatFile(ctx, func(c *catFile) error {
		var err error
		typ, size, _, err = c.request("info", sha, false)
		return err
	})
	return typ, size, err
}
