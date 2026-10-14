package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidatePath(t *testing.T) {
	good := []string{"a", "a/b.txt", "dir with space/файл.txt", ".gitignore", ".github/workflows/ci.yml", "a/.sgx"}
	bad := []string{"", "/abs", "a/", "./a", "a/../b", "..", ".", "a//b", ".git/config", "x/.GIT/y", ".sg/base", "a\nb", "a\x00b", string([]byte{0xff, 'a'})}
	for _, p := range good {
		if err := validatePath(p); err != nil {
			t.Errorf("validatePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range bad {
		if err := validatePath(p); status.Code(err) != codes.InvalidArgument {
			t.Errorf("validatePath(%q) = %v, want InvalidArgument", p, err)
		}
	}
}
