package store

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"
)

var t0 = time.Unix(1700000000, 0).UTC()

func TestProjectNamesAndAliasesShareANamespace(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	web, err := s.CreateProject(ctx, "web", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, "web", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := s.AddProjectAlias(ctx, web.ID, "site"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, "site", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("name equal to an alias: %v", err)
	}
	api, _ := s.CreateProject(ctx, "api", t0)
	if err := s.AddProjectAlias(ctx, api.ID, "web"); !errors.Is(err, ErrExists) {
		t.Fatalf("alias equal to a name: %v", err)
	}
	for _, ref := range []string{"web", "site", "#" + strconv.FormatInt(web.ID, 10)} {
		p, err := s.ResolveProject(ctx, ref)
		if err != nil || p.ID != web.ID || p.Name != "web" || !reflect.DeepEqual(p.Aliases, []string{"site"}) {
			t.Fatalf("ResolveProject(%q) = %+v, %v", ref, p, err)
		}
	}
	for _, ref := range []string{"nope", strconv.FormatInt(web.ID, 10), "#0" + strconv.FormatInt(web.ID, 10), "#+1", "#", "#x", ""} {
		if _, err := s.ResolveProject(ctx, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ResolveProject(%q) = %v, want ErrNotFound", ref, err)
		}
	}
	// A bare decimal is a name, never an id: project "1" is found by name,
	// "#1" by id (here a different project).
	numeric, err := s.CreateProject(ctx, strconv.FormatInt(api.ID, 10), t0)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.ResolveProject(ctx, numeric.Name); err != nil || p.ID != numeric.ID || p.ID == api.ID {
		t.Fatalf("numeric name resolves to %+v, %v", p, err)
	}
	if p, err := s.ResolveProject(ctx, "#"+strconv.FormatInt(api.ID, 10)); err != nil || p.ID != api.ID {
		t.Fatalf("#id resolves to %+v, %v", p, err)
	}
	// Rename keeps the old name as an alias; renaming onto an own alias swaps.
	if err := s.RenameProject(ctx, web.ID, "www"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx, web.ID)
	if p.Name != "www" || !reflect.DeepEqual(p.Aliases, []string{"site", "web"}) {
		t.Fatalf("after rename: %+v", p)
	}
	if err := s.RenameProject(ctx, web.ID, "site"); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProject(ctx, web.ID)
	if p.Name != "site" || !reflect.DeepEqual(p.Aliases, []string{"web", "www"}) {
		t.Fatalf("after swap: %+v", p)
	}
	if err := s.RenameProject(ctx, web.ID, "api"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto another project: %v", err)
	}
	if id, err := s.RemoveProjectAlias(ctx, "www"); err != nil || id != web.ID {
		t.Fatalf("RemoveProjectAlias = %d, %v", id, err)
	}
	if _, err := s.RemoveProjectAlias(ctx, "site"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing a canonical name as alias: %v", err)
	}
}

func TestProjectIDsAreNeverReused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	a, _ := s.CreateProject(ctx, "a", t0)
	if err := s.DeleteProjectRows(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateProject(ctx, "b", t0)
	if b.ID == a.ID {
		t.Fatalf("project id %d reused after delete", a.ID)
	}
	if _, err := s.GetProject(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted project still readable: %v", err)
	}
}

func TestProjectCommitsCursorsAndMounts(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CreateRepo(ctx, "core", ""); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "web", t0)
	m := Mount{Path: "lib", Repo: "core", Folder: "src", AddedAt: t0}
	c1 := ProjectCommit{SHA: sha('1'), Seq: 1, Kind: KindSource, Repo: "core", SourceSHA: sha('a'),
		Layout: []LayoutMount{{Path: "lib", Repo: "core", Folder: "src"}}, Sources: map[string]string{"core": sha('a')}}
	if err := s.RecordProjectCommits(ctx, p.ID, []ProjectCommit{c1}, map[string]string{"core": sha('a')}, &MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	c2 := ProjectCommit{SHA: sha('2'), Seq: 2, Kind: KindLayout, Layout: nil, ActorName: "admin", ActorEmail: "admin@sgit.local",
		ActorWhen: 1700000100, Message: "Project layout: removed `lib/`\n", Sources: map[string]string{}}
	if err := s.RecordProjectCommits(ctx, p.ID, []ProjectCommit{c2}, map[string]string{}, &MountChange{Remove: "lib"}); err != nil {
		t.Fatal(err)
	}
	if head, seq, err := s.ProjectHead(ctx, p.ID); err != nil || head != sha('2') || seq != 2 {
		t.Fatalf("ProjectHead = %s %d %v", head, seq, err)
	}
	if src, err := s.ProjectSource(ctx, p.ID, sha('1'), "core"); err != nil || src != sha('a') {
		t.Fatalf("ProjectSource = %s, %v", src, err)
	}
	if _, err := s.ProjectSource(ctx, p.ID, sha('2'), "core"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ProjectSource after unmount: %v", err)
	}
	if ok, _ := s.IsProjectCommit(ctx, p.ID, sha('1')); !ok {
		t.Fatal("IsProjectCommit(c1) = false")
	}
	all, err := s.ProjectCommits(ctx, p.ID)
	if err != nil || len(all) != 2 || !reflect.DeepEqual(all[0], c1) || all[1].Message != c2.Message || all[1].Kind != KindLayout {
		t.Fatalf("ProjectCommits = %+v, %v", all, err)
	}
	if cur, _ := s.ProjectCursors(ctx, p.ID); len(cur) != 0 {
		t.Fatalf("cursors after unmount = %v", cur)
	}
	if ms, _ := s.Mounts(ctx, p.ID); len(ms) != 0 {
		t.Fatalf("mounts after unmount = %v", ms)
	}
	// A duplicate seq or SHA rolls back the whole batch.
	if err := s.RecordProjectCommits(ctx, p.ID, []ProjectCommit{{SHA: sha('3'), Seq: 2, Kind: KindLayout, Sources: map[string]string{}}}, nil, nil); err == nil {
		t.Fatal("duplicate seq accepted")
	}
}

func TestMountsAndRepoDeleteGuard(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_ = s.CreateRepo(ctx, "core", "")
	p, _ := s.CreateProject(ctx, "web", t0)
	m := Mount{Path: "lib", Repo: "core", Folder: "src", AddedAt: t0}
	if err := s.RecordProjectCommits(ctx, p.ID, nil, map[string]string{"core": sha('a')}, &MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMountReadOnly(ctx, p.ID, "lib", true); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Mounts(ctx, p.ID)
	if len(ms) != 1 || !ms[0].ReadOnly || ms[0].Folder != "src" {
		t.Fatalf("Mounts = %+v", ms)
	}
	if names, _ := s.ProjectsMounting(ctx, "core"); !reflect.DeepEqual(names, []string{"web"}) {
		t.Fatalf("ProjectsMounting = %v", names)
	}
	if err := s.DeleteRepo(ctx, "core"); !errors.Is(err, ErrMounted) {
		t.Fatalf("DeleteRepo while mounted = %v, want ErrMounted", err)
	}
	if err := s.SetMountReadOnly(ctx, p.ID, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetMountReadOnly(missing) = %v", err)
	}
}

func TestRepoDeleteGuardCoversProjectHistory(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for _, r := range []string{"core", "docs", "free"} {
		if err := s.CreateRepo(ctx, r, ""); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.CreateProject(ctx, "web", t0)
	m := Mount{Path: "lib", Repo: "core", Folder: "src", AddedAt: t0}
	d := Mount{Path: "docs", Repo: "docs", Folder: ".", AddedAt: t0}
	if err := s.RecordProjectCommits(ctx, p.ID, nil, nil, &MountChange{Add: &m}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProjectCommits(ctx, p.ID, nil, nil, &MountChange{Add: &d}); err != nil {
		t.Fatal(err)
	}
	// core: a source commit (project_commits.repo); docs: only in a layout commit's sources.
	commits := []ProjectCommit{
		{SHA: sha('1'), Seq: 1, Kind: KindSource, Repo: "core", SourceSHA: sha('a'), Sources: map[string]string{"core": sha('a')}},
		{SHA: sha('2'), Seq: 2, Kind: KindLayout, Sources: map[string]string{"core": sha('a'), "docs": sha('b')}},
	}
	if err := s.RecordProjectCommits(ctx, p.ID, commits, map[string]string{"core": sha('a')}, &MountChange{Remove: "docs"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProjectCommits(ctx, p.ID, nil, map[string]string{}, &MountChange{Remove: "lib"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"core", "docs"} {
		if err := s.DeleteRepo(ctx, r); !errors.Is(err, ErrMounted) {
			t.Fatalf("DeleteRepo(%s) referenced by project history = %v, want ErrMounted", r, err)
		}
	}
	if err := s.DeleteRepo(ctx, "free"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProjectRows(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRepo(ctx, "core"); err != nil {
		t.Fatalf("DeleteRepo after the project is gone = %v", err)
	}
}

func TestLandSequence(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i, c := range []byte{'a', 'b', 'c'} {
		if err := s.RecordLand(ctx, "core", sha(c), t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordLand(ctx, "core", sha('a'), t0); err != nil { // idempotent
		t.Fatal(err)
	}
	seqs, err := s.LandSeqs(ctx, "core", []string{sha('c'), sha('a'), sha('z')})
	if err != nil || len(seqs) != 2 || seqs[sha('a')] >= seqs[sha('c')] {
		t.Fatalf("LandSeqs = %v, %v", seqs, err)
	}
	if other, _ := s.LandSeqs(ctx, "other", []string{sha('a')}); len(other) != 0 {
		t.Fatalf("LandSeqs leaked across repos: %v", other)
	}
}

func sha(c byte) string {
	b := make([]byte, 40)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
