// Package store persists users, tokens, ACLs, repos and drafts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")

	// ErrLastAdmin is returned when an operation would leave no admin.
	ErrLastAdmin = errors.New("store: would leave no admin")
	// ErrSelf is returned when an operation may not target the acting user.
	ErrSelf = errors.New("store: cannot target yourself")
)

// DeletedSuffix marks the name of a soft-deleted user: "alice" becomes
// "alice~deleted-<id>". '~' is not valid in user names, so a tombstone never
// collides with a live user and the original name can be reused.
const DeletedSuffix = "~deleted-"

// Level is a per-project access level. Write implies read.
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

// MirrorState is the background mirror's bookkeeping for one repo (spec §8).
type MirrorState struct {
	Repo         string
	LastMirrored string    // last commit pushed to the mirror; "" = never
	LastError    string    // last push error; "" after a success
	LastAttempt  time.Time // zero = never attempted
	Pending      bool      // the worker saw main ahead of LastMirrored
}

type Store struct {
	db *sql.DB
	// PermWarning is set when Open could not tighten the database files
	// to 0600 (for example, they belong to another user). It is not fatal;
	// the caller should log it.
	PermWarning error
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  email TEXT NOT NULL,
  admin INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  deleted_at INTEGER
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
  project_id INTEGER NOT NULL,
  level INTEGER NOT NULL,
  PRIMARY KEY (user_id, project_id)
);
CREATE TABLE IF NOT EXISTS drafts (
  id TEXT PRIMARY KEY,
  project_id INTEGER NOT NULL,
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
CREATE INDEX IF NOT EXISTS drafts_project_status ON drafts(project_id, status);
CREATE TABLE IF NOT EXISTS mirror_state (
  repo TEXT PRIMARY KEY REFERENCES repos(name),
  last_mirrored TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  last_attempt INTEGER NOT NULL DEFAULT 0,
  pending INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS projects (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT UNIQUE NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS project_aliases (
  alias TEXT PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id)
);
CREATE TABLE IF NOT EXISTS project_mounts (
  project_id INTEGER NOT NULL REFERENCES projects(id),
  mount_path TEXT NOT NULL,
  repo TEXT NOT NULL REFERENCES repos(name),
  folder TEXT NOT NULL,
  read_only INTEGER NOT NULL DEFAULT 0,
  added_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, mount_path)
);
CREATE INDEX IF NOT EXISTS project_mounts_repo ON project_mounts(repo);
CREATE TABLE IF NOT EXISTS project_commits (
  project_id INTEGER NOT NULL,
  project_sha TEXT NOT NULL,
  seq INTEGER NOT NULL,
  kind TEXT NOT NULL,
  repo TEXT NOT NULL DEFAULT '',
  source_sha TEXT NOT NULL DEFAULT '',
  layout TEXT NOT NULL DEFAULT '[]',
  actor_name TEXT NOT NULL DEFAULT '',
  actor_email TEXT NOT NULL DEFAULT '',
  actor_when INTEGER NOT NULL DEFAULT 0,
  message TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, project_sha),
  UNIQUE (project_id, seq)
);
CREATE TABLE IF NOT EXISTS project_commit_sources (
  project_id INTEGER NOT NULL,
  project_sha TEXT NOT NULL,
  repo TEXT NOT NULL,
  source_sha TEXT NOT NULL,
  PRIMARY KEY (project_id, project_sha, repo)
);
CREATE TABLE IF NOT EXISTS project_cursors (
  project_id INTEGER NOT NULL,
  repo TEXT NOT NULL,
  source_sha TEXT NOT NULL,
  PRIMARY KEY (project_id, repo)
);
CREATE TABLE IF NOT EXISTS land_seq (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  repo TEXT NOT NULL,
  sha TEXT NOT NULL,
  at INTEGER NOT NULL,
  UNIQUE (repo, sha)
);
`

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	permWarning, err := ownerOnly(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serializing avoids SQLITE_BUSY on upgrades.
	if err := preMigrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db, PermWarning: permWarning}, nil
}

// chmod is os.Chmod; tests replace it to simulate a refused chmod.
var chmod = os.Chmod

// ownerOnly creates the database file with mode 0600 before SQLite opens it
// (SQLite gives the -wal and -shm files the database file's mode) and
// tightens an existing database and its WAL files to 0600: they hold token
// hashes and the user list. Failing to create the file is an error; a
// refused chmod is only returned as a warning, so a server running as
// another user than the files' owner still starts.
func ownerOnly(path string) (warning, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			warning = errors.Join(warning, fmt.Errorf("cannot restrict %s to mode 0600 (it holds token hashes): %w", p, err))
		}
	}
	return warning, nil
}

// preMigrate drops the per-repo acl and drafts tables of databases created
// before projects (spec §1: no migration, no production data).
func preMigrate(db *sql.DB) error {
	for _, t := range []string{"acl", "drafts"} {
		has, err := hasColumn(db, t, "repo")
		if err != nil {
			return err
		}
		if has {
			if _, err := db.Exec(`DROP INDEX IF EXISTS drafts_repo_status; DROP TABLE ` + t); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrate upgrades databases created by older versions in place.
func migrate(db *sql.DB) error {
	// users.deleted_at (soft delete) was added after the first release.
	has, err := hasColumn(db, "users", "deleted_at")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN deleted_at INTEGER`); err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
		if n == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the database handle for tests that inspect raw tables.
func (s *Store) DB() *sql.DB { return s.db }

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
	err := s.db.QueryRowContext(ctx, `SELECT id,name,email,admin FROM users WHERE name=? AND deleted_at IS NULL`, name).Scan(&u.ID, &u.Name, &u.Email, &u.Admin)
	return u, notFound(err)
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&n)
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
		WHERE t.hash=? AND t.revoked_at IS NULL AND u.deleted_at IS NULL`, hash).Scan(&u.ID, &u.Name, &u.Email, &u.Admin)
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

func (s *Store) CreateRepo(ctx context.Context, name, mirrorURL string) error {
	return s.CreateRepoWithMirror(ctx, name, mirrorURL, "")
}

// CreateRepoWithMirror inserts a repo. A non-empty lastMirrored records that
// the mirror already holds that commit (a repo imported from its own mirror).
func (s *Store) CreateRepoWithMirror(ctx context.Context, name, mirrorURL, lastMirrored string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	_, err = tx.ExecContext(ctx, `INSERT INTO repos(name,mirror_url,created_at) VALUES(?,?,?)`, name, mirrorURL, time.Now().Unix())
	if isUnique(err) {
		return fmt.Errorf("%w: repo %q", ErrExists, name)
	}
	if err != nil {
		return err
	}
	if lastMirrored != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mirror_state(repo,last_mirrored) VALUES(?,?)`, name, lastMirrored); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MirrorState returns repo's mirror bookkeeping; a repo never mirrored has
// the zero state.
func (s *Store) MirrorState(ctx context.Context, repo string) (MirrorState, error) {
	m := MirrorState{Repo: repo}
	var attempt, pending int64
	err := s.db.QueryRowContext(ctx, `SELECT last_mirrored,last_error,last_attempt,pending FROM mirror_state WHERE repo=?`, repo).
		Scan(&m.LastMirrored, &m.LastError, &attempt, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if attempt != 0 {
		m.LastAttempt = time.Unix(attempt, 0).UTC()
	}
	m.Pending = pending != 0
	return m, nil
}

func (s *Store) SetMirrorPending(ctx context.Context, repo string, pending bool) error {
	p := 0
	if pending {
		p = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO mirror_state(repo,pending) VALUES(?,?)
ON CONFLICT(repo) DO UPDATE SET pending=excluded.pending`, repo, p)
	return err
}

// RecordMirrorSuccess notes that the mirror's main is now sha.
func (s *Store) RecordMirrorSuccess(ctx context.Context, repo, sha string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mirror_state(repo,last_mirrored,last_error,last_attempt,pending) VALUES(?,?,'',?,0)
ON CONFLICT(repo) DO UPDATE SET last_mirrored=excluded.last_mirrored,last_error='',last_attempt=excluded.last_attempt,pending=0`,
		repo, sha, at.Unix())
	return err
}

// RecordMirrorFailure notes a failed push; last_mirrored is kept.
func (s *Store) RecordMirrorFailure(ctx context.Context, repo, msg string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mirror_state(repo,last_error,last_attempt,pending) VALUES(?,?,?,1)
ON CONFLICT(repo) DO UPDATE SET last_error=excluded.last_error,last_attempt=excluded.last_attempt,pending=1`,
		repo, msg, at.Unix())
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

// UserRow is a user as listed by ListUsers.
type UserRow struct {
	User
	ActiveTokens int
	CreatedAt    time.Time
}

// ListUsers returns every live (not deleted) user ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.name,u.email,u.admin,u.created_at,
		(SELECT COUNT(*) FROM tokens t WHERE t.user_id=u.id AND t.revoked_at IS NULL)
		FROM users u WHERE u.deleted_at IS NULL ORDER BY u.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var r UserRow
		var created int64
		if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Admin, &created, &r.ActiveTokens); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAdmin sets or clears the global admin flag of a live user. Clearing it
// from the last admin fails with ErrLastAdmin. It is idempotent.
func (s *Store) SetAdmin(ctx context.Context, name string, admin bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id int64
	var cur bool
	err = tx.QueryRowContext(ctx, `SELECT id,admin FROM users WHERE name=? AND deleted_at IS NULL`, name).Scan(&id, &cur)
	if err != nil {
		return fmt.Errorf("%w: user %q", ErrNotFound, name)
	}
	if cur && !admin {
		if err := lastAdminCheck(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET admin=? WHERE id=?`, admin, id); err != nil {
		return err
	}
	return tx.Commit()
}

// lastAdminCheck returns ErrLastAdmin if at most one live admin exists.
func lastAdminCheck(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE admin=1 AND deleted_at IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}

// DeleteResult counts what DeleteUser changed.
type DeleteResult struct {
	TokensRevoked   int
	ACLsRemoved     int
	DraftsAbandoned int
}

// DeleteUser soft-deletes name: it revokes the user's tokens, removes their
// ACLs, abandons their open drafts and tombstones the row. The row stays
// because landed and abandoned drafts reference it; the name becomes
// "<name>~deleted-<id>" (see DeletedSuffix) so it can be reused. It fails
// with ErrSelf if name is actingID and ErrLastAdmin if the user is the only
// admin. All of it happens in one transaction.
func (s *Store) DeleteUser(ctx context.Context, name string, actingID int64) (DeleteResult, error) {
	var res DeleteResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id int64
	var admin bool
	if err := tx.QueryRowContext(ctx, `SELECT id,admin FROM users WHERE name=? AND deleted_at IS NULL`, name).Scan(&id, &admin); err != nil {
		return res, fmt.Errorf("%w: user %q", ErrNotFound, name)
	}
	if id == actingID {
		return res, ErrSelf
	}
	if admin {
		if err := lastAdminCheck(ctx, tx); err != nil {
			return res, err
		}
	}
	now := time.Now().Unix()
	count := func(dst *int, q string, args ...any) error {
		r, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		*dst = int(n)
		return nil
	}
	if err := count(&res.TokensRevoked, `UPDATE tokens SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, id); err != nil {
		return res, err
	}
	if err := count(&res.ACLsRemoved, `DELETE FROM acl WHERE user_id=?`, id); err != nil {
		return res, err
	}
	if err := count(&res.DraftsAbandoned, `UPDATE drafts SET status=?,updated_at=? WHERE owner_id=? AND status=?`, DraftAbandoned, now, id, DraftOpen); err != nil {
		return res, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET admin=0,deleted_at=?,name=name||?||id WHERE id=?`, now, DeletedSuffix, id); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// TokenRow describes a token without its secret.
type TokenRow struct {
	ID        string
	User      string
	CreatedAt time.Time
	RevokedAt time.Time // zero = active
}

// ListTokens returns the tokens of live users, oldest first. A non-empty user
// restricts the list to that user.
func (s *Store) ListTokens(ctx context.Context, user string) ([]TokenRow, error) {
	q := `SELECT t.id,u.name,t.created_at,t.revoked_at FROM tokens t JOIN users u ON u.id=t.user_id WHERE u.deleted_at IS NULL`
	var args []any
	if user != "" {
		q += ` AND u.name=?`
		args = append(args, user)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY t.created_at, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var r TokenRow
		var created int64
		var revoked sql.NullInt64
		if err := rows.Scan(&r.ID, &r.User, &created, &revoked); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		if revoked.Valid {
			r.RevokedAt = time.Unix(revoked.Int64, 0).UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRepo removes a repo's row together with its land sequence and mirror
// state, atomically (grants and drafts belong to projects). It does not
// touch the git directory.
func (s *Store) DeleteRepo(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	// A project rebuild replays every recorded commit, so a repo stays in use
	// while any project mounts it or has it in its history.
	var mounted int
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM project_mounts WHERE repo=?) OR
		EXISTS(SELECT 1 FROM project_commits WHERE repo=?) OR
		EXISTS(SELECT 1 FROM project_commit_sources WHERE repo=?)`, name, name, name).Scan(&mounted); err != nil {
		return err
	}
	if mounted > 0 {
		return fmt.Errorf("%w: %q", ErrMounted, name)
	}
	for _, q := range []string{
		`DELETE FROM land_seq WHERE repo=?`,
		`DELETE FROM mirror_state WHERE repo=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: repo %q", ErrNotFound, name)
	}
	return tx.Commit()
}
