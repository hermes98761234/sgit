package cli

import "testing"

func TestNameFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/api.git": "api",
		"https://github.com/acme/api/":    "api",
		"git@github.com:acme/api.git":     "api",
		"ssh://git@example.com/srv/web":   "web",
		"file:///tmp/x/api.git":           "api",
		"/srv/git/tool.git/":              "tool",
		"https://example.com/":            "",
		"https://example.com":             "",
		"":                                "",
	} {
		if got := nameFromURL(in); got != want {
			t.Errorf("nameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
