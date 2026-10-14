package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Draft is a pending change to a project (spec §6.2).
type Draft struct {
	ID        string
	ProjectID int64
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

// ACLRow is one explicit grant.
type ACLRow struct {
	User    string
	Project string // canonical name
	Level   Level
}

// Access is one project a user holds a grant on.
type Access struct {
	Project Project
	Level   Level
}

// SetACL sets userID's level on a project; LevelNone removes the grant. A
// grant on a project that does not exist (or was deleted concurrently) is
// refused with ErrNotFound, so no grant outlives its project.
func (s *Store) SetACL(ctx context.Context, userID, projectID int64, l Level) error {
	if l == LevelNone {
		_, err := s.db.ExecContext(ctx, `DELETE FROM acl WHERE user_id=? AND project_id=?`, userID, projectID)
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO acl(user_id,project_id,level)
		SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM projects WHERE id=?)
		ON CONFLICT(user_id,project_id) DO UPDATE SET level=excluded.level`, userID, projectID, int(l), projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: project %d", ErrNotFound, projectID)
	}
	return nil
}

// ACL returns userID's level on a project (LevelNone without a grant).
func (s *Store) ACL(ctx context.Context, userID, projectID int64) (Level, error) {
	var l int
	err := s.db.QueryRowContext(ctx, `SELECT level FROM acl WHERE user_id=? AND project_id=?`, userID, projectID).Scan(&l)
	if errors.Is(err, sql.ErrNoRows) {
		return LevelNone, nil
	}
	return Level(l), err
}

// ListACL returns explicit grants of live users ordered by user then project
// name; a non-empty user and a non-zero projectID filter the result. Admins'
// implicit access is not listed.
func (s *Store) ListACL(ctx context.Context, user string, projectID int64) ([]ACLRow, error) {
	q := `SELECT u.name, p.name, a.level FROM acl a JOIN users u ON u.id=a.user_id JOIN projects p ON p.id=a.project_id
		WHERE u.deleted_at IS NULL`
	var args []any
	if user != "" {
		q += ` AND u.name=?`
		args = append(args, user)
	}
	if projectID != 0 {
		q += ` AND a.project_id=?`
		args = append(args, projectID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY u.name, p.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ACLRow
	for rows.Next() {
		var r ACLRow
		var l int
		if err := rows.Scan(&r.User, &r.Project, &l); err != nil {
			return nil, err
		}
		r.Level = Level(l)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAccess returns the projects userID holds a grant on, by name, with
// their aliases.
func (s *Store) ListAccess(ctx context.Context, userID int64) ([]Access, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.project_id, a.level FROM acl a JOIN projects p ON p.id=a.project_id
		WHERE a.user_id=? ORDER BY p.name`, userID)
	if err != nil {
		return nil, err
	}
	type grant struct {
		id int64
		l  Level
	}
	var gs []grant
	for rows.Next() {
		var g grant
		var l int
		if err := rows.Scan(&g.id, &l); err != nil {
			rows.Close()
			return nil, err
		}
		g.l = Level(l)
		gs = append(gs, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Access, 0, len(gs))
	for _, g := range gs {
		p, err := s.GetProject(ctx, g.id)
		if errors.Is(err, ErrNotFound) { // deleted since the query
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Access{Project: p, Level: g.l})
	}
	return out, nil
}

// InsertDraft adds a draft. A draft for a project that does not exist (or
// was deleted concurrently) is refused with ErrNotFound.
func (s *Store) InsertDraft(ctx context.Context, d Draft) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO drafts(id,project_id,owner_id,title,base,head,version,status,landed_as,created_at,updated_at)
		SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM projects WHERE id=?)`,
		d.ID, d.ProjectID, d.OwnerID, d.Title, d.Base, d.Head, d.Version, d.Status, d.LandedAs, d.CreatedAt.Unix(), d.UpdatedAt.Unix(), d.ProjectID)
	if isUnique(err) {
		return fmt.Errorf("%w: draft %q", ErrExists, d.ID)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: project %d", ErrNotFound, d.ProjectID)
	}
	return nil
}

const draftCols = `d.id,d.project_id,d.owner_id,u.name,d.title,d.base,d.head,d.version,d.status,d.landed_as,d.created_at,d.updated_at`

type scanner interface{ Scan(...any) error }

func scanDraft(sc scanner) (Draft, error) {
	var d Draft
	var created, updated int64
	err := sc.Scan(&d.ID, &d.ProjectID, &d.OwnerID, &d.OwnerName, &d.Title, &d.Base, &d.Head, &d.Version, &d.Status, &d.LandedAs, &created, &updated)
	d.CreatedAt, d.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return d, err
}

// GetDraft returns draft id of a project. The not-found error names only the
// draft id.
func (s *Store) GetDraft(ctx context.Context, projectID int64, id string) (Draft, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+draftCols+` FROM drafts d JOIN users u ON u.id=d.owner_id WHERE d.project_id=? AND d.id=?`, projectID, id)
	d, err := scanDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, fmt.Errorf("%w: draft %q", ErrNotFound, id)
	}
	return d, err
}

func (s *Store) UpdateDraft(ctx context.Context, d Draft) error {
	res, err := s.db.ExecContext(ctx, `UPDATE drafts SET title=?,base=?,head=?,version=?,status=?,landed_as=?,updated_at=? WHERE project_id=? AND id=?`,
		d.Title, d.Base, d.Head, d.Version, d.Status, d.LandedAs, d.UpdatedAt.Unix(), d.ProjectID, d.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: draft %q", ErrNotFound, d.ID)
	}
	return nil
}

func (s *Store) ListDrafts(ctx context.Context, projectID, ownerID int64, status string) ([]Draft, error) {
	q := `SELECT ` + draftCols + ` FROM drafts d JOIN users u ON u.id=d.owner_id WHERE d.project_id=?`
	args := []any{projectID}
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

// AbandonOpenDrafts marks every open draft of a project abandoned (their
// objects lived only in a project repo that was rebuilt).
func (s *Store) AbandonOpenDrafts(ctx context.Context, projectID int64) (int, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE drafts SET status=?, updated_at=? WHERE project_id=? AND status=?`,
		DraftAbandoned, time.Now().Unix(), projectID, DraftOpen)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}
