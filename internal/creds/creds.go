// Package creds stores sg login credentials in $SG_CONFIG_DIR/credentials
// (default: <user config dir>/sg/credentials) as TOML readable only by the user.
package creds

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/hermes98761234/sgit/internal/fsutil"
)

// Server is how to reach and authenticate to one sg-server.
type Server struct {
	Token    string `toml:"token"`
	CAFile   string `toml:"ca_file,omitempty"`
	Insecure bool   `toml:"insecure,omitempty"` // plaintext; loopback only
}

// File is the whole credentials file.
type File struct {
	Default string            `toml:"default"`
	Servers map[string]Server `toml:"servers"`
}

// Dir returns the sg configuration directory.
func Dir() (string, error) {
	if d := os.Getenv("SG_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sg"), nil
}

func filePath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "credentials"), nil
}

// Load reads the credentials file; a missing file yields an empty File.
func Load() (File, error) {
	p, err := filePath()
	if err != nil {
		return File{}, err
	}
	f := File{}
	if _, err := toml.DecodeFile(p, &f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return File{}, fmt.Errorf("read %s: %w", p, err)
	}
	if f.Servers == nil {
		f.Servers = map[string]Server{}
	}
	return f, nil
}

// Save writes the credentials file atomically with mode 0600.
func Save(f File) error {
	p, err := filePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return err
	}
	return fsutil.WriteAtomic(p, buf.Bytes(), 0o600)
}

// Lookup returns the entry for server, or for the default server when server is "".
func (f File) Lookup(server string) (string, Server, bool) {
	if server == "" {
		server = f.Default
	}
	s, ok := f.Servers[server]
	return server, s, ok && server != ""
}
