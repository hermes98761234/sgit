package server

import (
	"fmt"
	"testing"

	"github.com/hermes98761234/sgit/internal/store"
)

// A detail quoting user text that contains the suffix still gets it.
func TestUserMessageKeepsSuffix(t *testing.T) {
	for _, c := range []struct {
		err      error
		sentinel error
		suffix   string
		want     string
	}{
		{fmt.Errorf("%w: project %q", store.ErrNotFound, "x not found"), store.ErrNotFound, " not found", `project "x not found" not found`},
		{fmt.Errorf("%w: mount %q", store.ErrExists, "a already exists"), store.ErrExists, " already exists", `mount "a already exists" already exists`},
	} {
		if got := userMessage(c.err, c.sentinel, c.suffix); got != c.want {
			t.Errorf("userMessage(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
