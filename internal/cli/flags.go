package cli

import (
	"errors"
	"flag"
	"path/filepath"
	"strings"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// newFlags returns a FlagSet that reports to stderr and binds --json.
func (a *app) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("sg "+name, flag.ContinueOnError)
	fs.SetOutput(a.io.Stderr)
	fs.BoolVar(&a.json, "json", false, "machine-readable JSON output")
	return fs
}

// parse parses flags that may be interleaved with positional arguments and
// returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &exitError{code: ExitError, err: err}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// abs resolves p against the command's working directory.
func (a *app) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(a.io.Dir, p)
}
