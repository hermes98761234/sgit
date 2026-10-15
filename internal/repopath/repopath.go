// Package repopath holds the one rule for which file paths sgit can store.
// The server refuses any other path with InvalidArgument, the tree builder
// refuses it too, and the client lists such files as skipped, so a path is
// never accepted on one side and dropped on the other.
package repopath

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// MaxLen is the longest path, in bytes, that can be stored.
const MaxLen = 4096

// Error is a path that cannot be stored.
type Error struct {
	Path   string
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("invalid path %q: %s", e.Path, e.Reason) }

// Check reports whether p (slash-separated, relative to the project root) can
// be stored. symlink is true for mode 120000 entries, which git refuses under
// a few extra names. Besides malformed paths, it refuses every component git
// itself refuses (".git" and its NTFS and HFS+ aliases such as "GIT~1",
// ".git." or ".git "), because git would silently leave such entries out of
// a tree. ".sg" components are refused as the working copy's metadata.
func Check(p string, symlink bool) error {
	if err := wellFormed(p); err != nil {
		return err
	}
	bad := func(why string) error { return &Error{Path: p, Reason: why} }
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if strings.EqualFold(seg, ".git") || strings.EqualFold(seg, ".sg") {
			return bad("reserved name " + seg)
		}
		if ntfsDotGeneric(seg, "git", "") || hfsDotGeneric(seg, "git") || ntfsShortGit(seg) {
			return bad("reserved name " + seg + " (an alias of .git)")
		}
	}
	if last := segs[len(segs)-1]; symlink && (ntfsDotGeneric(last, "gitmodules", "gi7eba") || hfsDotGeneric(last, "gitmodules")) {
		return bad("a symlink cannot be named " + last)
	}
	return nil
}

// CheckDelete reports whether p may be named in a delete. It applies only the
// shape rules of Check, not the reserved names: a file with such a name can
// still sit in imported history, and deleting it must stay possible.
func CheckDelete(p string) error {
	return wellFormed(p)
}

// wellFormed is the part of Check that does not depend on names: a
// normalized, relative UTF-8 path without control characters, backslashes
// or dot segments.
func wellFormed(p string) error {
	bad := func(why string) error { return &Error{Path: p, Reason: why} }
	switch {
	case p == "":
		return bad("empty")
	case len(p) > MaxLen:
		return bad("too long")
	case !utf8.ValidString(p):
		return bad("not UTF-8")
	case strings.ContainsAny(p, "\x00\r\n"):
		return bad("control character")
	case strings.Contains(p, `\`):
		return bad("backslash")
	case strings.HasPrefix(p, "/"):
		return bad("absolute")
	case path.Clean(p) != p:
		return bad("not normalized")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return bad("dot segment")
		}
	}
	return nil
}

// spacesAndPeriods reports whether rest is only spaces and periods up to its
// end or an NTFS stream separator ':' (git's only_spaces_and_periods).
func spacesAndPeriods(rest string) bool {
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case ':':
			return true
		case ' ', '.':
		default:
			return false
		}
	}
	return true
}

// ntfsShortGit matches git's "git~1" check in is_ntfs_dotgit.
func ntfsShortGit(seg string) bool {
	return len(seg) >= 5 && strings.EqualFold(seg[:5], "git~1") && spacesAndPeriods(seg[5:])
}

// ntfsDotGeneric ports git's is_ntfs_dot_generic: "."+name followed by
// spaces or periods, the 8.3 short name (first six letters, "~1".."~4"),
// and, when shortPrefix is set, the hashed fallback short name.
func ntfsDotGeneric(seg, name, shortPrefix string) bool {
	if len(seg) > len(name) && seg[0] == '.' && strings.EqualFold(seg[1:1+len(name)], name) {
		return spacesAndPeriods(seg[1+len(name):])
	}
	if len(name) >= 6 && len(seg) >= 8 && strings.EqualFold(seg[:6], name[:6]) && seg[6] == '~' && seg[7] >= '1' && seg[7] <= '4' {
		return spacesAndPeriods(seg[8:])
	}
	if shortPrefix == "" {
		return false
	}
	sawTilde := false
	i := 0
	for ; i < 8; i++ {
		if i >= len(seg) {
			return false
		}
		c := seg[i]
		switch {
		case sawTilde:
			if c < '0' || c > '9' {
				return false
			}
		case c == '~':
			i++
			if i >= len(seg) || seg[i] < '1' || seg[i] > '9' {
				return false
			}
			sawTilde = true
		case i >= 6, c >= 0x80:
			return false
		case lower(c) != shortPrefix[i]:
			return false
		}
	}
	return spacesAndPeriods(seg[i:])
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// hfsIgnorable lists the code points HFS+ ignores in file names
// (git's next_hfs_char).
func hfsIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return true
	}
	return false
}

// hfsDotGeneric ports git's is_hfs_dot_generic: "."+name, case-insensitive,
// with HFS+-ignorable code points anywhere.
func hfsDotGeneric(seg, name string) bool {
	var rs []rune
	for _, r := range seg {
		if !hfsIgnorable(r) {
			rs = append(rs, r)
		}
	}
	if len(rs) != len(name)+1 || rs[0] != '.' {
		return false
	}
	for i := 0; i < len(name); i++ {
		r := rs[i+1]
		if r > 127 || lower(byte(r)) != name[i] {
			return false
		}
	}
	return true
}
