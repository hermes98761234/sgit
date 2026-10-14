package server

import (
	"strings"

	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mountFor returns the mount holding project path p: the longest mount path
// that is a proper prefix of p ("." holds everything). A path equal to a
// mount path is the mount itself, not a file inside it.
func mountFor(mounts []store.Mount, p string) (store.Mount, bool) {
	best, ok := store.Mount{}, false
	for _, m := range mounts {
		if m.Path == "." || strings.HasPrefix(p, m.Path+"/") {
			if !ok || len(m.Path) > len(best.Path) || best.Path == "." {
				best, ok = m, true
			}
		}
	}
	return best, ok
}

// sourcePath maps project path p (inside m) to its path in m.Repo.
func sourcePath(m store.Mount, p string) string {
	rest := p
	if m.Path != "." {
		rest = strings.TrimPrefix(p, m.Path+"/")
	}
	if m.Folder == "." {
		return rest
	}
	return m.Folder + "/" + rest
}

// projectPath maps source path sp of repo back into the project (the mount
// with the longest matching folder wins).
func projectPath(mounts []store.Mount, repo, sp string) (string, bool) {
	best, ok := store.Mount{}, false
	for _, m := range mounts {
		if m.Repo != repo || !(m.Folder == "." || strings.HasPrefix(sp, m.Folder+"/")) {
			continue
		}
		if !ok || len(m.Folder) > len(best.Folder) || best.Folder == "." {
			best, ok = m, true
		}
	}
	if !ok {
		return "", false
	}
	rest := sp
	if best.Folder != "." {
		rest = strings.TrimPrefix(sp, best.Folder+"/")
	}
	if best.Path == "." {
		return rest, true
	}
	return best.Path + "/" + rest, true
}

// checkPaths enforces §6.1 for every path a write touches.
func checkPaths(mounts []store.Mount, paths []string) error {
	for _, p := range paths {
		m, ok := mountFor(mounts, p)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "path outside project: %s", p)
		}
		if m.ReadOnly {
			return status.Errorf(codes.PermissionDenied, "read-only path: %s", p)
		}
	}
	return nil
}
