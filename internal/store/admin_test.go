package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestListUsersAndTokens(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	a, _ := s.CreateUser(ctx, "alice", "a@x", true)
	b, _ := s.CreateUser(ctx, "bob", "b@x", false)
	_ = s.CreateToken(ctx, a.ID, "tok1", []byte("h1"))
	_ = s.CreateToken(ctx, a.ID, "tok2", []byte("h2"))
	_ = s.CreateToken(ctx, b.ID, "tok3", []byte("h3"))
	if err := s.RevokeToken(ctx, "tok2"); err != nil {
		t.Fatal(err)
	}
	us, err := s.ListUsers(ctx)
	if err != nil || len(us) != 2 || us[0].Name != "alice" || !us[0].Admin || us[0].ActiveTokens != 1 ||
		us[1].Name != "bob" || us[1].ActiveTokens != 1 || us[0].CreatedAt.IsZero() {
		t.Fatalf("ListUsers = %+v, %v", us, err)
	}
	ts, err := s.ListTokens(ctx, "")
	if err != nil || len(ts) != 3 {
		t.Fatalf("ListTokens = %+v, %v", ts, err)
	}
	ts, _ = s.ListTokens(ctx, "alice")
	if len(ts) != 2 || ts[0].ID != "tok1" || !ts[0].RevokedAt.IsZero() || ts[1].ID != "tok2" || ts[1].RevokedAt.IsZero() || ts[0].User != "alice" {
		t.Fatalf("ListTokens(alice) = %+v", ts)
	}
}

func TestSetAdminLastAdmin(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, _ = s.CreateUser(ctx, "alice", "a@x", true)
	_, _ = s.CreateUser(ctx, "bob", "b@x", false)
	if err := s.SetAdmin(ctx, "alice", false); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := s.SetAdmin(ctx, "bob", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdmin(ctx, "bob", true); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := s.SetAdmin(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByName(ctx, "alice"); u.Admin {
		t.Fatal("alice still admin")
	}
	if err := s.SetAdmin(ctx, "bob", false); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := s.SetAdmin(ctx, "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestDeleteUser(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	adm, _ := s.CreateUser(ctx, "root", "r@x", true)
	bob, _ := s.CreateUser(ctx, "bob", "b@x", false)
	now := time.Unix(1700000000, 0)
	web, _ := s.CreateProject(ctx, "web", now)
	_ = s.CreateToken(ctx, bob.ID, "tok1", []byte("h1"))
	_ = s.CreateToken(ctx, bob.ID, "tok2", []byte("h2"))
	_ = s.SetACL(ctx, bob.ID, web.ID, LevelWrite)
	for id, st := range map[string]string{"open0001": DraftOpen, "open0002": DraftOpen, "land0001": DraftLanded} {
		_ = s.InsertDraft(ctx, Draft{ID: id, ProjectID: web.ID, OwnerID: bob.ID, Title: "t", Base: "b", Head: "h", Version: 1, Status: st, CreatedAt: now, UpdatedAt: now})
	}
	if _, err := s.DeleteUser(ctx, "bob", bob.ID); !errors.Is(err, ErrSelf) {
		t.Fatalf("self delete: %v", err)
	}
	if _, err := s.DeleteUser(ctx, "root", bob.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("last admin delete: %v", err)
	}
	if _, err := s.DeleteUser(ctx, "ghost", adm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	res, err := s.DeleteUser(ctx, "bob", adm.ID)
	if err != nil || res != (DeleteResult{TokensRevoked: 2, ACLsRemoved: 1, DraftsAbandoned: 2}) {
		t.Fatalf("DeleteUser = %+v, %v", res, err)
	}
	if _, err := s.UserByName(ctx, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user visible: %v", err)
	}
	if _, err := s.UserByTokenHash(ctx, []byte("h1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user authenticates: %v", err)
	}
	if us, _ := s.ListUsers(ctx); len(us) != 1 {
		t.Fatalf("ListUsers = %+v", us)
	}
	if ts, _ := s.ListTokens(ctx, ""); len(ts) != 0 {
		t.Fatalf("ListTokens = %+v", ts)
	}
	if as, _ := s.ListACL(ctx, "", 0); len(as) != 0 {
		t.Fatalf("ListACL = %+v", as)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("CountUsers = %d", n)
	}
	// History is kept and attributed to the tombstone; open drafts are abandoned.
	d, err := s.GetDraft(ctx, web.ID, "land0001")
	if err != nil || d.Status != DraftLanded || d.OwnerName != "bob"+DeletedSuffix+"2" {
		t.Fatalf("landed draft = %+v, %v", d, err)
	}
	if d, _ := s.GetDraft(ctx, web.ID, "open0001"); d.Status != DraftAbandoned {
		t.Fatalf("open draft = %+v", d)
	}
	// The name can be reused and the new user is independent.
	nb, err := s.CreateUser(ctx, "bob", "b2@x", false)
	if err != nil || nb.ID == bob.ID {
		t.Fatalf("reuse name: %+v, %v", nb, err)
	}
}

func TestDeleteRepo(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u, _ := s.CreateUser(ctx, "bob", "b@x", false)
	_ = s.CreateRepoWithMirror(ctx, "web", "file:///m", "abc")
	now := time.Unix(1700000000, 0)
	p, _ := s.CreateProject(ctx, "site", now)
	_ = s.SetACL(ctx, u.ID, p.ID, LevelRead)
	_ = s.InsertDraft(ctx, Draft{ID: "d0000001", ProjectID: p.ID, OwnerID: u.ID, Title: "t", Base: "b", Head: "h", Version: 1, Status: DraftOpen, CreatedAt: now, UpdatedAt: now})
	if err := s.DeleteRepo(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRepo(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repo remains: %v", err)
	}
	if ms, _ := s.MirrorState(ctx, "web"); ms.LastMirrored != "" {
		t.Fatalf("mirror state remains: %+v", ms)
	}
	// Grants and drafts belong to projects now; a repo delete leaves them.
	if ds, _ := s.ListDrafts(ctx, p.ID, 0, ""); len(ds) != 1 {
		t.Fatalf("drafts = %+v", ds)
	}
	if as, _ := s.ListACL(ctx, "", 0); len(as) != 1 || as[0].Project != "site" {
		t.Fatalf("ListACL = %+v", as)
	}
	if err := s.DeleteRepo(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.CreateRepo(ctx, "web", ""); err != nil {
		t.Fatalf("recreate: %v", err)
	}
}

func TestListACLFilters(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	a, _ := s.CreateUser(ctx, "alice", "a@x", false)
	b, _ := s.CreateUser(ctx, "bob", "b@x", false)
	now := time.Unix(1700000000, 0)
	r1, _ := s.CreateProject(ctx, "r1", now)
	r2, _ := s.CreateProject(ctx, "r2", now)
	_ = s.SetACL(ctx, a.ID, r1.ID, LevelRead)
	_ = s.SetACL(ctx, a.ID, r2.ID, LevelWrite)
	_ = s.SetACL(ctx, b.ID, r2.ID, LevelRead)
	for _, c := range []struct {
		user    string
		project int64
		n       int
	}{{"", 0, 3}, {"alice", 0, 2}, {"", r2.ID, 2}, {"alice", r2.ID, 1}, {"bob", r1.ID, 0}} {
		got, err := s.ListACL(ctx, c.user, c.project)
		if err != nil || len(got) != c.n {
			t.Errorf("ListACL(%q,%d) = %+v, %v; want %d", c.user, c.project, got, err, c.n)
		}
	}
	got, _ := s.ListACL(ctx, "alice", r2.ID)
	if got[0] != (ACLRow{User: "alice", Project: "r2", Level: LevelWrite}) {
		t.Fatalf("row = %+v", got[0])
	}
}

// An existing database from before soft delete gets the column and keeps its data.
func TestMigrateAddsDeletedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sg.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, email TEXT NOT NULL, admin INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
INSERT INTO users(name,email,admin,created_at) VALUES('old','o@x',1,5);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ { // second Open: migration is a no-op
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		u, err := s.UserByName(context.Background(), "old")
		if err != nil || !u.Admin {
			t.Fatalf("UserByName = %+v, %v", u, err)
		}
		s.Close()
	}
}
