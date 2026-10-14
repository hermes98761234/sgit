package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrMounted is returned when deleting a repo that a project still mounts.
var ErrMounted = errors.New("store: repo is mounted by a project")

// Commit kinds of a derived project commit.
const (
	KindSource = "source"
	KindLayout = "layout"
)

// Project is a named, id-addressed view over folders of source repos.
type Project struct {
	ID        int64
	Name      string
	Aliases   []string
	CreatedAt time.Time
}

// Mount places folder of repo at Path inside a project.
type Mount struct {
	Path, Repo, Folder string
	ReadOnly           bool
	AddedAt            time.Time
}

// LayoutMount is one mount as recorded in a project commit's layout.
type LayoutMount struct {
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Folder string `json:"folder"`
}

// ProjectCommit is the derivation recipe of one project commit.
type ProjectCommit struct {
	SHA  string
	Seq  int64
	Kind string // KindSource or KindLayout

	Repo, SourceSHA string // kind source
	Layout          []LayoutMount

	ActorName, ActorEmail string // kind layout
	ActorWhen             int64
	Message               string

	Sources map[string]string // repo -> source main at this commit (every mounted repo)
}

// MountChange is the mount-table change applied together with a commit batch.
type MountChange struct {
	Add    *Mount
	Remove string
}

// nameTaken reports whether name is a project name or alias (inside tx).
func nameTaken(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE name=?1
		UNION ALL SELECT project_id FROM project_aliases WHERE alias=?1 LIMIT 1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// CreateProject creates a project; the name must be free of names and aliases.
func (s *Store) CreateProject(ctx context.Context, name string, at time.Time) (Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, taken, err := nameTaken(ctx, tx, name); err != nil {
		return Project{}, err
	} else if taken {
		return Project{}, fmt.Errorf("%w: project %q", ErrExists, name)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO projects(name, created_at) VALUES(?,?)`, name, at.Unix())
	if err != nil {
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	return Project{ID: id, Name: name, Aliases: []string{}, CreatedAt: time.Unix(at.Unix(), 0).UTC()}, tx.Commit()
}

// ResolveProject finds a project by "#<id>" (canonical positive decimal) or,
// for any other reference, by name or alias. There is no numeric fallback: a
// bare decimal is only ever a name or alias.
func (s *Store) ResolveProject(ctx context.Context, ref string) (Project, error) {
	notFound := fmt.Errorf("%w: project %q", ErrNotFound, ref)
	if rest, ok := strings.CutPrefix(ref, "#"); ok {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != rest {
			return Project{}, notFound
		}
		return s.GetProject(ctx, n)
	}
	id, ok, err := nameTaken(ctx, s.db, ref)
	if err != nil {
		return Project{}, err
	}
	if !ok {
		return Project{}, notFound
	}
	return s.GetProject(ctx, id)
}

// GetProject returns a project with its aliases.
func (s *Store) GetProject(ctx context.Context, id int64) (Project, error) {
	p := Project{ID: id, Aliases: []string{}}
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT name, created_at FROM projects WHERE id=?`, id).Scan(&p.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: project %d", ErrNotFound, id)
	}
	if err != nil {
		return Project{}, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT alias FROM project_aliases WHERE project_id=? ORDER BY alias`, id)
	if err != nil {
		return Project{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return Project{}, err
		}
		p.Aliases = append(p.Aliases, a)
	}
	return p, rows.Err()
}

// ListProjects returns all projects ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(ids))
	for _, id := range ids {
		p, err := s.GetProject(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// RenameProject renames a project; the old name becomes an alias. Renaming
// onto one of the project's own aliases swaps it with the old name.
func (s *Store) RenameProject(ctx context.Context, id int64, newName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var old string
	err = tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: project %d", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if old == newName {
		return nil
	}
	owner, taken, err := nameTaken(ctx, tx, newName)
	if err != nil {
		return err
	}
	if taken {
		if owner != id {
			return fmt.Errorf("%w: project %q", ErrExists, newName)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_aliases WHERE alias=?`, newName); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_aliases(alias, project_id) VALUES(?,?)`, old, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET name=? WHERE id=?`, newName, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AddProjectAlias adds an alias; it must be free of names and aliases.
func (s *Store) AddProjectAlias(ctx context.Context, id int64, alias string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: project %d", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if _, taken, err := nameTaken(ctx, tx, alias); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: project %q", ErrExists, alias)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_aliases(alias, project_id) VALUES(?,?)`, alias, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveProjectAlias removes an alias and returns the owning project id.
func (s *Store) RemoveProjectAlias(ctx context.Context, alias string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT project_id FROM project_aliases WHERE alias=?`, alias).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: alias %q", ErrNotFound, alias)
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_aliases WHERE alias=?`, alias); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// DeleteProjectRows removes a project and its derived rows, atomically. Ids
// are never reused (AUTOINCREMENT).
func (s *Store) DeleteProjectRows(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, q := range []string{
		`DELETE FROM project_aliases WHERE project_id=?`,
		`DELETE FROM project_mounts WHERE project_id=?`,
		`DELETE FROM project_commits WHERE project_id=?`,
		`DELETE FROM project_commit_sources WHERE project_id=?`,
		`DELETE FROM project_cursors WHERE project_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: project %d", ErrNotFound, id)
	}
	return tx.Commit()
}

// Mounts lists a project's mounts ordered by path.
func (s *Store) Mounts(ctx context.Context, id int64) ([]Mount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mount_path, repo, folder, read_only, added_at
		FROM project_mounts WHERE project_id=? ORDER BY mount_path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mount
	for rows.Next() {
		var m Mount
		var ro, added int64
		if err := rows.Scan(&m.Path, &m.Repo, &m.Folder, &ro, &added); err != nil {
			return nil, err
		}
		m.ReadOnly = ro != 0
		m.AddedAt = time.Unix(added, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMountReadOnly sets the read-only flag of the mount at path.
func (s *Store) SetMountReadOnly(ctx context.Context, id int64, path string, ro bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE project_mounts SET read_only=? WHERE project_id=? AND mount_path=?`, ro, id, path)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: mount %q", ErrNotFound, path)
	}
	return nil
}

// ProjectsMounting returns the names of projects that mount repo, sorted.
func (s *Store) ProjectsMounting(ctx context.Context, repo string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT p.name FROM projects p
		JOIN project_mounts m ON m.project_id=p.id WHERE m.repo=? ORDER BY p.name`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// RecordProjectCommits is the single writer of derived project state. In one
// transaction it applies the mount change, inserts the commits (with their
// per-repo sources) and replaces the cursors. A nil cursors map leaves the
// cursors untouched; an empty non-nil map clears them.
func (s *Store) RecordProjectCommits(ctx context.Context, id int64, commits []ProjectCommit, cursors map[string]string, mc *MountChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if mc != nil && mc.Add != nil {
		m := mc.Add
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_mounts(project_id,mount_path,repo,folder,read_only,added_at) VALUES(?,?,?,?,?,?)`,
			id, m.Path, m.Repo, m.Folder, m.ReadOnly, m.AddedAt.Unix()); err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: mount %q", ErrExists, m.Path)
			}
			return err
		}
	}
	if mc != nil && mc.Remove != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_mounts WHERE project_id=? AND mount_path=?`, id, mc.Remove); err != nil {
			return err
		}
	}
	for _, c := range commits {
		layout, err := json.Marshal(c.Layout)
		if err != nil {
			return err
		}
		if c.Layout == nil {
			layout = []byte("[]")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_commits(project_id,project_sha,seq,kind,repo,source_sha,layout,actor_name,actor_email,actor_when,message)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, c.SHA, c.Seq, c.Kind, c.Repo, c.SourceSHA, string(layout), c.ActorName, c.ActorEmail, c.ActorWhen, c.Message); err != nil {
			return err
		}
		for repo, src := range c.Sources {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_commit_sources(project_id,project_sha,repo,source_sha) VALUES(?,?,?,?)`, id, c.SHA, repo, src); err != nil {
				return err
			}
		}
	}
	if cursors != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_cursors WHERE project_id=?`, id); err != nil {
			return err
		}
		for repo, src := range cursors {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_cursors(project_id,repo,source_sha) VALUES(?,?,?)`, id, repo, src); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ProjectHead returns the newest recorded project commit ("", 0 when none).
func (s *Store) ProjectHead(ctx context.Context, id int64) (string, int64, error) {
	var sha string
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT project_sha, seq FROM project_commits WHERE project_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&sha, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return sha, seq, err
}

// ProjectCommits returns all recorded project commits ordered by seq.
func (s *Store) ProjectCommits(ctx context.Context, id int64) ([]ProjectCommit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_sha,seq,kind,repo,source_sha,layout,actor_name,actor_email,actor_when,message
		FROM project_commits WHERE project_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	var out []ProjectCommit
	for rows.Next() {
		var c ProjectCommit
		var layout string
		if err := rows.Scan(&c.SHA, &c.Seq, &c.Kind, &c.Repo, &c.SourceSHA, &layout, &c.ActorName, &c.ActorEmail, &c.ActorWhen, &c.Message); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(layout), &c.Layout); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: project commit %s layout: %w", c.SHA, err)
		}
		if len(c.Layout) == 0 {
			c.Layout = nil
		}
		c.Sources = map[string]string{}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	idx := make(map[string]int, len(out))
	for i, c := range out {
		idx[c.SHA] = i
	}
	srows, err := s.db.QueryContext(ctx, `SELECT project_sha, repo, source_sha FROM project_commit_sources WHERE project_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var sha, repo, src string
		if err := srows.Scan(&sha, &repo, &src); err != nil {
			return nil, err
		}
		if i, ok := idx[sha]; ok {
			out[i].Sources[repo] = src
		}
	}
	return out, srows.Err()
}

// ProjectSource returns the source main of repo at project commit projectSHA.
func (s *Store) ProjectSource(ctx context.Context, id int64, projectSHA, repo string) (string, error) {
	var src string
	err := s.db.QueryRowContext(ctx, `SELECT source_sha FROM project_commit_sources WHERE project_id=? AND project_sha=? AND repo=?`,
		id, projectSHA, repo).Scan(&src)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: project commit source", ErrNotFound)
	}
	return src, err
}

// IsProjectCommit reports whether sha is a recorded commit of the project.
func (s *Store) IsProjectCommit(ctx context.Context, id int64, sha string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM project_commits WHERE project_id=? AND project_sha=?`, id, sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ProjectCursors returns repo -> last processed source main.
func (s *Store) ProjectCursors(ctx context.Context, id int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, source_sha FROM project_cursors WHERE project_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var repo, src string
		if err := rows.Scan(&repo, &src); err != nil {
			return nil, err
		}
		out[repo] = src
	}
	return out, rows.Err()
}

// RecordLand assigns sha a global land sequence number (idempotent).
func (s *Store) RecordLand(ctx context.Context, repo, sha string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO land_seq(repo,sha,at) VALUES(?,?,?) ON CONFLICT(repo,sha) DO NOTHING`, repo, sha, at.Unix())
	return err
}

// LandSeqs returns the land sequence of each of shas that has one.
func (s *Store) LandSeqs(ctx context.Context, repo string, shas []string) (map[string]int64, error) {
	out := make(map[string]int64, len(shas))
	for start := 0; start < len(shas); start += 500 {
		chunk := shas[start:min(start+500, len(shas))]
		q := `SELECT sha, seq FROM land_seq WHERE repo=? AND sha IN (?` + strings.Repeat(",?", len(chunk)-1) + `)`
		args := []any{repo}
		for _, c := range chunk {
			args = append(args, c)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sha string
			var seq int64
			if err := rows.Scan(&sha, &seq); err != nil {
				rows.Close()
				return nil, err
			}
			out[sha] = seq
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
