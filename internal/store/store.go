// Package store persists users, tokens, ACLs, repos and drafts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
)

// Level is a per-repo access level. Write implies read.
type Level int

const (
	LevelNone  Level = 0
	LevelRead  Level = 1
	LevelWrite Level = 2
)

const (
	DraftOpen      = "open"
	DraftLanded    = "landed"
	DraftAbandoned = "abandoned"
)

type User struct {
	ID    int64
	Name  string
	Email string
	Admin bool
}

type RepoRow struct {
	Name      string
	MirrorURL string
	CreatedAt time.Time
}

type Draft struct {
	ID        string
	Repo      string
	OwnerID   int64
	OwnerName string // filled on reads
	Title     string
	Base      string
	Head      string
	Version   int
	Status    string
	LandedAs  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  email TEXT NOT NULL,
  admin INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
  id TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  hash BLOB UNIQUE NOT NULL,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS repos (
  name TEXT PRIMARY KEY,
  mirror_url TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS acl (
  user_id INTEGER NOT NULL REFERENCES users(id),
  repo TEXT NOT NULL,
  level INTEGER NOT NULL,
  PRIMARY KEY (user_id, repo)
);
CREATE TABLE IF NOT EXISTS drafts (
  id TEXT PRIMARY KEY,
  repo TEXT NOT NULL,
  owner_id INTEGER NOT NULL REFERENCES users(id),
  title TEXT NOT NULL,
  base TEXT NOT NULL,
  head TEXT NOT NULL,
  version INTEGER NOT NULL,
  status TEXT NOT NULL,
  landed_as TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS drafts_repo_status ON drafts(repo, status);
`

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serializing avoids SQLITE_BUSY on upgrades.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func isUnique(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY"))
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) CreateUser(ctx context.Context, name, email string, admin bool) (User, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users(name,email,admin,created_at) VALUES(?,?,?,?)`, name, email, admin, time.Now().Unix())
	if isUnique(err) {
		return User{}, fmt.Errorf("%w: user %q", ErrExists, name)
	}
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	return User{ID: id, Name: name, Email: email, Admin: admin}, err
}

// CreateUserWithToken inserts a user and its first token atomically.
func (s *Store) CreateUserWithToken(ctx context.Context, name, email string, admin bool, tokenID string, tokenHash []byte) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `INSERT INTO users(name,email,admin,created_at) VALUES(?,?,?,?)`, name, email, admin, now)
	if isUnique(err) {
		return User{}, fmt.Errorf("%w: user %q", ErrExists, name)
	}
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tokens(id,user_id,hash,created_at) VALUES(?,?,?,?)`, tokenID, id, tokenHash, now)
	if isUnique(err) {
		return User{}, fmt.Errorf("%w: token %q", ErrExists, tokenID)
	}
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Name: name, Email: email, Admin: admin}, nil
}

func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id,name,email,admin FROM users WHERE name=?`, name).Scan(&u.ID, &u.Name, &u.Email, &u.Admin)
	return u, notFound(err)
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateToken(ctx context.Context, userID int64, id string, hash []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens(id,user_id,hash,created_at) VALUES(?,?,?,?)`, id, userID, hash, time.Now().Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: token %q", ErrExists, id)
	}
	return err
}

func (s *Store) UserByTokenHash(ctx context.Context, hash []byte) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.name,u.email,u.admin FROM tokens t JOIN users u ON u.id=t.user_id
		WHERE t.hash=? AND t.revoked_at IS NULL`, hash).Scan(&u.ID, &u.Name, &u.Email, &u.Admin)
	return u, notFound(err)
}

func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: token %q", ErrNotFound, id)
	}
	return nil
}

func (s *Store) SetACL(ctx context.Context, userID int64, repo string, l Level) error {
	if l == LevelNone {
		_, err := s.db.ExecContext(ctx, `DELETE FROM acl WHERE user_id=? AND repo=?`, userID, repo)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO acl(user_id,repo,level) VALUES(?,?,?)
		ON CONFLICT(user_id,repo) DO UPDATE SET level=excluded.level`, userID, repo, int(l))
	return err
}

func (s *Store) ACL(ctx context.Context, userID int64, repo string) (Level, error) {
	var l int
	err := s.db.QueryRowContext(ctx, `SELECT level FROM acl WHERE user_id=? AND repo=?`, userID, repo).Scan(&l)
	if errors.Is(err, sql.ErrNoRows) {
		return LevelNone, nil
	}
	return Level(l), err
}

func (s *Store) CreateRepo(ctx context.Context, name, mirrorURL string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repos(name,mirror_url,created_at) VALUES(?,?,?)`, name, mirrorURL, time.Now().Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: repo %q", ErrExists, name)
	}
	return err
}

func (s *Store) GetRepo(ctx context.Context, name string) (RepoRow, error) {
	var r RepoRow
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT name,mirror_url,created_at FROM repos WHERE name=?`, name).Scan(&r.Name, &r.MirrorURL, &created)
	r.CreatedAt = time.Unix(created, 0).UTC()
	return r, notFound(err)
}

func (s *Store) ListRepos(ctx context.Context) ([]RepoRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,mirror_url,created_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoRow
	for rows.Next() {
		var r RepoRow
		var created int64
		if err := rows.Scan(&r.Name, &r.MirrorURL, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InsertDraft(ctx context.Context, d Draft) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO drafts(id,repo,owner_id,title,base,head,version,status,landed_as,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, d.ID, d.Repo, d.OwnerID, d.Title, d.Base, d.Head, d.Version, d.Status, d.LandedAs, d.CreatedAt.Unix(), d.UpdatedAt.Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: draft %q", ErrExists, d.ID)
	}
	return err
}

const draftCols = `d.id,d.repo,d.owner_id,u.name,d.title,d.base,d.head,d.version,d.status,d.landed_as,d.created_at,d.updated_at`

type scanner interface{ Scan(...any) error }

func scanDraft(sc scanner) (Draft, error) {
	var d Draft
	var created, updated int64
	err := sc.Scan(&d.ID, &d.Repo, &d.OwnerID, &d.OwnerName, &d.Title, &d.Base, &d.Head, &d.Version, &d.Status, &d.LandedAs, &created, &updated)
	d.CreatedAt, d.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return d, err
}

func (s *Store) GetDraft(ctx context.Context, repo, id string) (Draft, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+draftCols+` FROM drafts d JOIN users u ON u.id=d.owner_id WHERE d.repo=? AND d.id=?`, repo, id)
	d, err := scanDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, fmt.Errorf("%w: draft %q in repo %q", ErrNotFound, id, repo)
	}
	return d, err
}

func (s *Store) UpdateDraft(ctx context.Context, d Draft) error {
	res, err := s.db.ExecContext(ctx, `UPDATE drafts SET title=?,base=?,head=?,version=?,status=?,landed_as=?,updated_at=? WHERE repo=? AND id=?`,
		d.Title, d.Base, d.Head, d.Version, d.Status, d.LandedAs, d.UpdatedAt.Unix(), d.Repo, d.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: draft %q", ErrNotFound, d.ID)
	}
	return nil
}

func (s *Store) ListDrafts(ctx context.Context, repo string, ownerID int64, status string) ([]Draft, error) {
	q := `SELECT ` + draftCols + ` FROM drafts d JOIN users u ON u.id=d.owner_id WHERE d.repo=?`
	args := []any{repo}
	if ownerID != 0 {
		q += ` AND d.owner_id=?`
		args = append(args, ownerID)
	}
	if status != "" {
		q += ` AND d.status=?`
		args = append(args, status)
	}
	q += ` ORDER BY d.updated_at DESC, d.id LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
