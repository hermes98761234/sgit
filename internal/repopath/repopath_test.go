package repopath

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	good := []string{"a", "a/b.txt", "dir with space/файл.txt", ".gitignore", ".github/workflows/ci.yml", "a/.sgx",
		"lib/.gitx", "lib/x.git/c.txt", "git~2/a", "GIT~1x/a", ".gitmodules", "gitmod~1", "a/.git~1", ".g/a"}
	bad := []string{"", "/abs", "a/", "./a", "a/../b", "..", ".", "a//b", ".git/config", "x/.GIT/y", ".sg/base", "a/.SG/b",
		"a\nb", "a\rb", "a\x00b", string([]byte{0xff, 'a'}), `a\b`, strings.Repeat("a", MaxLen+1),
		"lib/GIT~1/a.txt", "lib/git~1", "lib/.git./b.txt", "lib/.git /b", ".git../a", ".git. . /a", ".git:x/a", "Git~1. /a",
		".g\u200cit/a", ".\ufeffGIT/a", ".gi\u206at"}
	for _, p := range good {
		if err := Check(p, false); err != nil {
			t.Errorf("Check(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range bad {
		if err := Check(p, false); err == nil {
			t.Errorf("Check(%q) = nil, want error", p)
		}
	}
	for _, p := range []string{".gitmodules", "a/.GITMODULES. ", "gitmod~1", "GITMOD~4", "gi7eba~1", "gi7eb~12", ".gitmodules:x", ".g\u200citmodules"} {
		if err := Check(p, true); err == nil {
			t.Errorf("Check(%q, symlink) = nil, want error", p)
		}
	}
	for _, p := range []string{"link", ".gitmodulesx", "gitmod~5", "gi7ebb~1"} {
		if err := Check(p, true); err != nil {
			t.Errorf("Check(%q, symlink) = %v, want nil", p, err)
		}
	}
}

// Every path Check accepts must survive git's own index checks: git drops a
// refused entry with only a warning, which would lose the file silently.
// Every NTFS alias Check refuses must also be one git drops (HFS+ aliases are
// refused on top of what git on Linux checks).
func TestCheckAgreesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(stdin []byte, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_INDEX_FILE="+filepath.Join(dir, "idx"))
		cmd.Stdin = bytes.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run(nil, "init", "-q", "--bare", ".")
	blob := strings.TrimSpace(run([]byte("x"), "hash-object", "-w", "--stdin"))
	cases := []string{"GIT~1/a", "git~1", ".git./a", ".git /a", ".git:x/a", ".git. ./a", "git~1../a", "a/.gitx", "x.git/a", "git~2/a",
		".gitmodules", "gitmod~1", "gi7eba~1", "gi7eba~12", ".gitmodules. ", "plain"}
	for _, p := range cases {
		for _, mode := range []string{"100644", "120000"} {
			_ = os.Remove(filepath.Join(dir, "idx"))
			run([]byte(fmt.Sprintf("%s %s\t%s\x00", mode, blob, p)), "update-index", "-z", "--index-info")
			kept := strings.TrimSpace(run(nil, "ls-files")) != ""
			ok := Check(p, mode == "120000") == nil
			if ok != kept {
				t.Errorf("%s %q: Check ok=%v, git kept=%v", mode, p, ok, kept)
			}
		}
	}
}
