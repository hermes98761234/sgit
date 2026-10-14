package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hermes98761234/sgit/internal/store"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func openMirrorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMirrorStateDefaultsToEmpty(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.CreateRepo(ctx, "web", "https://example.com/web.git"); err != nil {
		t.Fatal(err)
	}
	got, err := st.MirrorState(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got != (store.MirrorState{Repo: "web"}) {
		t.Fatalf("MirrorState = %+v, want empty", got)
	}
}

func TestMirrorStateTransitions(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.CreateRepo(ctx, "web", "https://example.com/web.git"); err != nil {
		t.Fatal(err)
	}
	t1 := time.Unix(1700000000, 0).UTC()
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)

	if err := st.SetMirrorPending(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MirrorState(ctx, "web"); !got.Pending || got.LastMirrored != "" || !got.LastAttempt.IsZero() {
		t.Fatalf("after SetMirrorPending: %+v", got)
	}
	if err := st.RecordMirrorFailure(ctx, "web", "boom", t1); err != nil {
		t.Fatal(err)
	}
	want := store.MirrorState{Repo: "web", LastError: "boom", LastAttempt: t1, Pending: true}
	if got, _ := st.MirrorState(ctx, "web"); got != want {
		t.Fatalf("after failure: %+v, want %+v", got, want)
	}
	if err := st.RecordMirrorSuccess(ctx, "web", shaA, t2); err != nil {
		t.Fatal(err)
	}
	want = store.MirrorState{Repo: "web", LastMirrored: shaA, LastAttempt: t2}
	if got, _ := st.MirrorState(ctx, "web"); got != want {
		t.Fatalf("after success: %+v, want %+v", got, want)
	}
	if err := st.RecordMirrorFailure(ctx, "web", "again", t3); err != nil {
		t.Fatal(err)
	}
	want = store.MirrorState{Repo: "web", LastMirrored: shaA, LastError: "again", LastAttempt: t3, Pending: true}
	if got, _ := st.MirrorState(ctx, "web"); got != want {
		t.Fatalf("a failure must keep last_mirrored: %+v, want %+v", got, want)
	}
	if err := st.SetMirrorPending(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MirrorState(ctx, "web"); got.Pending || got.LastError != "again" {
		t.Fatalf("SetMirrorPending(false) must only clear pending: %+v", got)
	}
}

func TestCreateRepoWithMirrorSeedsState(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.CreateRepoWithMirror(ctx, "app", "https://example.com/app.git", shaA); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetRepo(ctx, "app")
	if err != nil || row.MirrorURL != "https://example.com/app.git" {
		t.Fatalf("GetRepo = %+v, %v", row, err)
	}
	if got, _ := st.MirrorState(ctx, "app"); got.LastMirrored != shaA || got.Pending {
		t.Fatalf("MirrorState = %+v, want last_mirrored %s", got, shaA)
	}
	err = st.CreateRepoWithMirror(ctx, "app", "https://example.com/other.git", shaB)
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate = %v, want ErrExists", err)
	}
	if got, _ := st.MirrorState(ctx, "app"); got.LastMirrored != shaA {
		t.Fatalf("a failed duplicate changed mirror state: %+v", got)
	}
}

func TestMirrorStateNeedsRepo(t *testing.T) {
	ctx := context.Background()
	st := openMirrorStore(t)
	if err := st.SetMirrorPending(ctx, "ghost", true); err == nil {
		t.Fatal("SetMirrorPending for a missing repo succeeded; want a foreign-key error")
	}
}
