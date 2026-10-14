package server_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProjectAdminLifecycle(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CommitDirect("core", map[string]string{"src/a.go": "a", "docs/b.md": "b"}, "seed")
	ac := e.Admin(e.AdminToken)

	info := testenv.Must(ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "web"}))
	if info.Name != "web" || info.Id == 0 || info.Head != "" || len(info.Mounts) != 0 {
		t.Fatalf("CreateProject = %+v", info)
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "projects", strconv.FormatInt(info.Id, 10)+".git")); err != nil {
		t.Fatalf("project repo missing: %v", err)
	}
	info = testenv.Must(ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "src"}))
	if len(info.Mounts) != 1 || info.Mounts[0].Path != "lib" || info.Mounts[0].Repo != "core" || info.Mounts[0].Folder != "src" || info.Head == "" {
		t.Fatalf("MountProject = %+v", info)
	}
	g := e.ProjectGit(info.Id)
	defer g.Close()
	files := testenv.Must(g.ListFiles(ctx, "main"))
	if len(files) != 1 || files["lib/a.go"].SHA == "" {
		t.Fatalf("project files = %v", files)
	}
	testenv.Must(ac.AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: "web", Alias: "site"}))
	info = testenv.Must(ac.RenameProject(ctx, &sgitv1.RenameProjectRequest{Project: "site", NewName: "www"}))
	if info.Name != "www" || !reflect.DeepEqual(info.Aliases, []string{"site", "web"}) {
		t.Fatalf("RenameProject = %+v", info)
	}
	byID := testenv.Must(ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: "#" + strconv.FormatInt(info.Id, 10)}))
	if byID.Name != "www" {
		t.Fatalf("GetProject by id = %+v", byID)
	}
	// A bare decimal is only a name or alias, never an id.
	if _, err := ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: strconv.FormatInt(info.Id, 10)}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetProject by bare decimal = %v", err)
	}
	list := testenv.Must(ac.ListProjects(ctx, &sgitv1.AdminListProjectsRequest{}))
	if len(list.Projects) != 1 || list.Projects[0].Name != "www" {
		t.Fatalf("ListProjects = %+v", list)
	}
	info = testenv.Must(ac.RemoveProjectAlias(ctx, &sgitv1.RemoveProjectAliasRequest{Alias: "site"}))
	if !reflect.DeepEqual(info.Aliases, []string{"web"}) {
		t.Fatalf("RemoveProjectAlias = %+v", info)
	}
	info = testenv.Must(ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "www", Path: "lib"}))
	if len(info.Mounts) != 0 {
		t.Fatalf("UnmountProject = %+v", info)
	}
	// The layout commit is stamped by the server clock with the admin's identity.
	lg := testenv.Must(g.Log(ctx, "main", "", 1))
	if len(lg) != 1 || lg[0].AuthorName != "admin" || time.Since(lg[0].Time) > time.Hour || lg[0].Time.After(time.Now().Add(time.Minute)) {
		t.Fatalf("layout commit = %+v", lg)
	}
	testenv.Must(ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "www"}))
	if _, err := ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: "www"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "projects", strconv.FormatInt(info.Id, 10)+".git")); !os.IsNotExist(err) {
		t.Fatalf("project repo not removed: %v", err)
	}
}

func TestMountProjectValidation(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	ac := e.Admin(e.AdminToken)
	testenv.Must(ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "web"}))
	testenv.Must(ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "src"}))
	cases := []struct {
		req  *sgitv1.MountProjectRequest
		code codes.Code
	}{
		{&sgitv1.MountProjectRequest{Project: "web", Path: "../x", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "/abs", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "a/.git", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "x", Repo: "core", Folder: "a//b"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "lib/sub", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: ".", Repo: "core", Folder: "."}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "x", Repo: "core", Folder: ""}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "x/", Repo: "core", Folder: "a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "x", Repo: "core", Folder: "./a"}, codes.InvalidArgument},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "lib", Repo: "core", Folder: "other"}, codes.AlreadyExists},
		{&sgitv1.MountProjectRequest{Project: "web", Path: "y", Repo: "nope", Folder: "a"}, codes.NotFound},
		{&sgitv1.MountProjectRequest{Project: "nope", Path: "y", Repo: "core", Folder: "a"}, codes.NotFound},
	}
	for _, c := range cases {
		if _, err := ac.MountProject(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("MountProject(%+v) = %v, want %v", c.req, err, c.code)
		}
	}
	// "." is allowed only as the sole mount.
	testenv.Must(ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "whole"}))
	testenv.Must(ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "whole", Path: ".", Repo: "core", Folder: "."}))
	if _, err := ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "whole", Path: "x", Repo: "core", Folder: "a"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("mount next to . = %v", err)
	}
	for _, name := range []string{"Web", "-x", "", "a b", "#1"} {
		if _, err := ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: name}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateProject(%q) = %v", name, err)
		}
	}
	if _, err := ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "web"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate CreateProject = %v", err)
	}
}

func TestRepoDeleteRefusedWhileMounted(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	e.CreateRepo("core")
	e.CreateProject("web")
	e.Mount("web", "lib", "core", "src", false)
	ac := e.Admin(e.AdminToken)
	_, err := ac.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "core"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != `repo "core" is mounted by project(s) web; unmount it first` {
		t.Fatalf("DeleteRepo while mounted = %v", err)
	}
	testenv.Must(ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "web", Path: "lib"}))
	// Unmounted, but still in the project's history (a rebuild replays it).
	_, err = ac.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "core"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != `repo "core" is in the history of project(s) web; delete those projects first` {
		t.Fatalf("DeleteRepo while in history = %v", err)
	}
	testenv.Must(ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "web"}))
	testenv.Must(ac.DeleteRepo(ctx, &sgitv1.DeleteRepoRequest{Name: "core"}))
}

func TestProjectAdminNeedsAdmin(t *testing.T) {
	ctx := context.Background()
	e := testenv.Start(t)
	bob := e.NewUser("bob", "", "")
	ac := e.Admin(bob)
	calls := map[string]func() error{
		"CreateProject": func() error { _, err := ac.CreateProject(ctx, &sgitv1.CreateProjectRequest{Name: "x"}); return err },
		"MountProject":  func() error { _, err := ac.MountProject(ctx, &sgitv1.MountProjectRequest{Project: "x"}); return err },
		"UnmountProject": func() error {
			_, err := ac.UnmountProject(ctx, &sgitv1.UnmountProjectRequest{Project: "x"})
			return err
		},
		"AddProjectAlias": func() error {
			_, err := ac.AddProjectAlias(ctx, &sgitv1.AddProjectAliasRequest{Project: "x"})
			return err
		},
		"RemoveProjectAlias": func() error {
			_, err := ac.RemoveProjectAlias(ctx, &sgitv1.RemoveProjectAliasRequest{Alias: "x"})
			return err
		},
		"RenameProject": func() error { _, err := ac.RenameProject(ctx, &sgitv1.RenameProjectRequest{Project: "x"}); return err },
		"ListProjects":  func() error { _, err := ac.ListProjects(ctx, &sgitv1.AdminListProjectsRequest{}); return err },
		"GetProject":    func() error { _, err := ac.GetProject(ctx, &sgitv1.GetProjectRequest{Project: "x"}); return err },
		"DeleteProject": func() error { _, err := ac.DeleteProject(ctx, &sgitv1.DeleteProjectRequest{Project: "x"}); return err },
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s as non-admin = %v", name, err)
		}
	}
}
