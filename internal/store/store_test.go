package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsersAndTokens(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("CountUsers = %d", n)
	}
	u, err := s.CreateUser(ctx, "alice", "alice@sgit.local", false)
	if err != nil || u.ID == 0 || u.Name != "alice" {
		t.Fatalf("CreateUser = %+v, %v", u, err)
	}
	if _, err := s.CreateUser(ctx, "alice", "x", false); !errors.Is(err, ErrExists) {
		t.Fatalf("dup user: want ErrExists, got %v", err)
	}
	if got, err := s.UserByName(ctx, "alice"); err != nil || got != u {
		t.Fatalf("UserByName = %+v, %v", got, err)
	}
	if _, err := s.UserByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := s.CreateToken(ctx, u.ID, "tokid001", hash); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserByTokenHash(ctx, hash); err != nil || got != u {
		t.Fatalf("UserByTokenHash = %+v, %v", got, err)
	}
	if err := s.RevokeToken(ctx, "tokid001"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByTokenHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token: want ErrNotFound, got %v", err)
	}
	if err := s.RevokeToken(ctx, "tokid001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: want ErrNotFound, got %v", err)
	}
}

func TestReposAndACL(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u, _ := s.CreateUser(ctx, "bob", "bob@sgit.local", false)
	if err := s.CreateRepo(ctx, "web", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRepo(ctx, "web", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("dup repo: want ErrExists, got %v", err)
	}
	if r, err := s.GetRepo(ctx, "web"); err != nil || r.Name != "web" || r.CreatedAt.IsZero() {
		t.Fatalf("GetRepo = %+v, %v", r, err)
	}
	if _, err := s.GetRepo(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if rs, _ := s.ListRepos(ctx); len(rs) != 1 {
		t.Fatalf("ListRepos = %+v", rs)
	}
	if l, _ := s.ACL(ctx, u.ID, "web"); l != LevelNone {
		t.Fatalf("default ACL = %d", l)
	}
	_ = s.SetACL(ctx, u.ID, "web", LevelRead)
	_ = s.SetACL(ctx, u.ID, "web", LevelWrite)
	if l, _ := s.ACL(ctx, u.ID, "web"); l != LevelWrite {
		t.Fatalf("upserted ACL = %d", l)
	}
	_ = s.SetACL(ctx, u.ID, "web", LevelNone)
	if l, _ := s.ACL(ctx, u.ID, "web"); l != LevelNone {
		t.Fatalf("revoked ACL = %d", l)
	}
}

func TestDrafts(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	alice, _ := s.CreateUser(ctx, "alice", "a@sgit.local", false)
	bob, _ := s.CreateUser(ctx, "bob", "b@sgit.local", false)
	now := time.Unix(1700000000, 0).UTC()
	d := Draft{ID: "aaaaaaaa", Repo: "web", OwnerID: alice.ID, Title: "t", Base: "b", Head: "h", Version: 1, Status: DraftOpen, CreatedAt: now, UpdatedAt: now}
	if err := s.InsertDraft(ctx, d); err != nil {
		t.Fatal(err)
	}
	_ = s.InsertDraft(ctx, Draft{ID: "bbbbbbbb", Repo: "web", OwnerID: bob.ID, Title: "u", Base: "b", Head: "h", Version: 1, Status: DraftOpen, CreatedAt: now, UpdatedAt: now.Add(time.Second)})
	got, err := s.GetDraft(ctx, "web", "aaaaaaaa")
	if err != nil || got.OwnerName != "alice" || got.Head != "h" || !got.CreatedAt.Equal(now) {
		t.Fatalf("GetDraft = %+v, %v", got, err)
	}
	if _, err := s.GetDraft(ctx, "other", "aaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong repo: want ErrNotFound, got %v", err)
	}
	got.Status, got.LandedAs, got.Version = DraftLanded, "c", 2
	if err := s.UpdateDraft(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDraft(ctx, "web", "aaaaaaaa")
	if got.Status != DraftLanded || got.LandedAs != "c" || got.Version != 2 {
		t.Fatalf("after update = %+v", got)
	}
	all, _ := s.ListDrafts(ctx, "web", 0, "")
	mine, _ := s.ListDrafts(ctx, "web", bob.ID, "")
	openDrafts, _ := s.ListDrafts(ctx, "web", 0, DraftOpen)
	if len(all) != 2 || all[0].ID != "bbbbbbbb" || len(mine) != 1 || mine[0].ID != "bbbbbbbb" || len(openDrafts) != 1 {
		t.Fatalf("list: all=%v mine=%v open=%v", all, mine, openDrafts)
	}
}
