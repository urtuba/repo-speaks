package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Project holds members, resources and agents.
type Project struct {
	ID   int64
	Name string
	// SyncIntervalHours is how often the timer syncs the project's
	// resources, or 0 when the timer is off.
	SyncIntervalHours int
	// MaxFileBytes is the largest file a sync keeps in a clone.
	MaxFileBytes int64
	CreatedAt    time.Time
}

// projectColumns is the SELECT list scanProject reads.
const projectColumns = "id, name, sync_interval_hours, max_file_bytes, created_at"

// scanProject reads one row selected with projectColumns.
func scanProject(row scanner) (Project, error) {
	var p Project
	var interval sql.NullInt64 // NULL when the timer is off
	var createdAt int64
	if err := row.Scan(&p.ID, &p.Name, &interval, &p.MaxFileBytes, &createdAt); err != nil {
		return Project{}, noRows(err)
	}
	p.SyncIntervalHours = int(interval.Int64)
	p.CreatedAt = unix(createdAt)
	return p, nil
}

// Member is an account in a project, with the permission names it holds.
type Member struct {
	UserID      int64
	Username    string
	Permissions []string // sorted by name
}

// CreateProject creates a project with no members and the default
// settings, which the database fills in.
func (s *Store) CreateProject(ctx context.Context, name string, now time.Time) (Project, error) {
	row := s.db.QueryRowContext(ctx,
		"INSERT INTO projects (name, created_at) VALUES (?, ?) RETURNING "+projectColumns,
		name, now.Unix())
	p, err := scanProject(row)
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// Project returns the project with the id, or ErrNotFound.
func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+projectColumns+" FROM projects WHERE id = ?", id)
	p, err := scanProject(row)
	if err != nil {
		return Project{}, fmt.Errorf("project %d: %w", id, err)
	}
	return p, nil
}

// AllProjects returns every project, by name.
func (s *Store) AllProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+projectColumns+" FROM projects ORDER BY name, id")
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return scanProjects(rows)
}

// UserProjects returns the projects the account is a member of, by name.
func (s *Store) UserProjects(ctx context.Context, userID int64) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+projectColumns+`
		FROM projects WHERE id IN (SELECT project_id FROM memberships WHERE user_id = ?)
		ORDER BY name, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list projects of user %d: %w", userID, err)
	}
	return scanProjects(rows)
}

// scanProjects reads projects rows and closes them.
func scanProjects(rows *sql.Rows) ([]Project, error) {
	defer rows.Close()
	var list []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// AddMember adds an account to a project with the permission names given.
// The caller has checked that the granter may give them; the store does
// not know the permission rules.
func (s *Store) AddMember(ctx context.Context, projectID, userID int64, permissions []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit
	if err := insertMember(ctx, tx, projectID, userID, 0, permissions); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	return tx.Commit()
}

// insertMember adds a membership and its permissions inside tx.
// invitedBy is 0 when nobody invited the member.
func insertMember(ctx context.Context, tx *sql.Tx, projectID, userID, invitedBy int64, permissions []string) error {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO memberships (project_id, user_id, invited_by) VALUES (?, ?, ?)",
		projectID, userID, nullID(invitedBy)); err != nil {
		return err
	}
	for _, p := range permissions {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO member_permissions (project_id, user_id, permission) VALUES (?, ?, ?)",
			projectID, userID, p); err != nil {
			return err
		}
	}
	return nil
}

// RemoveMember takes an account out of a project. Its permissions go
// with the membership (ON DELETE CASCADE). Its chats will stay.
func (s *Store) RemoveMember(ctx context.Context, projectID, userID int64) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM memberships WHERE project_id = ? AND user_id = ?", projectID, userID); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	return nil
}

// SetMemberPermissions replaces a member's permissions, in one
// transaction. It returns ErrNotFound when the account is not a member.
// The caller has checked the rules; the store does not know them.
func (s *Store) SetMemberPermissions(ctx context.Context, projectID, userID int64, permissions []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set member permissions: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit
	var member bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM memberships WHERE project_id = ? AND user_id = ?)",
		projectID, userID).Scan(&member); err != nil {
		return fmt.Errorf("set member permissions: %w", err)
	}
	if !member {
		return fmt.Errorf("membership of user %d in project %d: %w", userID, projectID, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM member_permissions WHERE project_id = ? AND user_id = ?", projectID, userID); err != nil {
		return fmt.Errorf("set member permissions: %w", err)
	}
	for _, p := range permissions {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO member_permissions (project_id, user_id, permission) VALUES (?, ?, ?)",
			projectID, userID, p); err != nil {
			return fmt.Errorf("set member permissions: %w", err)
		}
	}
	return tx.Commit()
}

// RenameProject changes a project's name. It returns ErrNotFound for a
// missing project.
func (s *Store) RenameProject(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE projects SET name = ? WHERE id = ?", name, id)
	if err != nil {
		return fmt.Errorf("rename project: %w", err)
	}
	return oneRow(res, fmt.Sprintf("project %d", id))
}

// SetSyncSettings changes how often the project's resources sync, in
// hours (0 turns the timer off), and the largest file a sync keeps. The
// caller has checked the values. It returns ErrNotFound for a missing
// project.
func (s *Store) SetSyncSettings(ctx context.Context, id int64, intervalHours int, maxFileBytes int64) error {
	var interval any // NULL turns the timer off
	if intervalHours != 0 {
		interval = intervalHours
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE projects SET sync_interval_hours = ?, max_file_bytes = ? WHERE id = ?",
		interval, maxFileBytes, id)
	if err != nil {
		return fmt.Errorf("set sync settings: %w", err)
	}
	return oneRow(res, fmt.Sprintf("project %d", id))
}

// DeleteProject deletes a project and everything in it: memberships,
// permissions, invites, resources and their jobs go with it (ON DELETE
// CASCADE). The clones on disk are removed later by the jobs loop.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM projects WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return oneRow(res, fmt.Sprintf("project %d", id))
}

// MemberPermissions returns the permission names an account holds in a
// project. It returns ErrNotFound when the account is not a member, so
// the caller can tell "no permissions" from "not a member".
func (s *Store) MemberPermissions(ctx context.Context, projectID, userID int64) ([]string, error) {
	var isMember bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM memberships WHERE project_id = ? AND user_id = ?)",
		projectID, userID).Scan(&isMember)
	if err != nil {
		return nil, fmt.Errorf("member permissions: %w", err)
	}
	if !isMember {
		return nil, fmt.Errorf("membership of user %d in project %d: %w", userID, projectID, ErrNotFound)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT permission FROM member_permissions
		WHERE project_id = ? AND user_id = ?
		ORDER BY permission`, projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("member permissions: %w", err)
	}
	defer rows.Close()
	perms := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("member permissions: %w", err)
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

// Members returns a project's members with their permission names,
// by username.
func (s *Store) Members(ctx context.Context, projectID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, mp.permission
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		LEFT JOIN member_permissions mp
			ON mp.project_id = m.project_id AND mp.user_id = m.user_id
		WHERE m.project_id = ?
		ORDER BY u.username, mp.permission`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	var list []Member
	for rows.Next() {
		var id int64
		var username string
		var perm sql.NullString // NULL for a member with no permissions
		if err := rows.Scan(&id, &username, &perm); err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		if len(list) == 0 || list[len(list)-1].UserID != id {
			list = append(list, Member{UserID: id, Username: username, Permissions: []string{}})
		}
		if perm.Valid {
			last := &list[len(list)-1]
			last.Permissions = append(last.Permissions, perm.String)
		}
	}
	return list, rows.Err()
}
