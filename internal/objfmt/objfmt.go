// Package objfmt implements the parts of git's object format that clients need
// without a git binary: blob hashing and raw tree parsing.
package objfmt

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
)

// BlobSHA returns the git object id of data stored as a blob.
func BlobSHA(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// TreeEntry is one entry of a raw git tree object.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "40000", "160000"
	Name string
	SHA  string
}

// IsTree reports whether the entry is a subdirectory.
func (e TreeEntry) IsTree() bool { return e.Mode == "40000" }

var errTruncated = errors.New("objfmt: truncated tree")

// ParseTree parses the binary content of a tree object.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errTruncated
		}
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errTruncated
		}
		nameEnd := sp + 1 + nul
		if len(data) < nameEnd+1+20 {
			return nil, errTruncated
		}
		out = append(out, TreeEntry{
			Mode: string(data[:sp]),
			Name: string(data[sp+1 : nameEnd]),
			SHA:  hex.EncodeToString(data[nameEnd+1 : nameEnd+21]),
		})
		data = data[nameEnd+21:]
	}
	return out, nil
}
