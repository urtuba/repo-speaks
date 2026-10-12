package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Project holds members, resources and agents.
type Project struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Member is an account in a project, with the permission names it holds.
type Member struct {
	UserID      int64
	Username    string
	Permissions []string // sorted by name
}

// CreateProject creates a project with no members.
func (s *Store) CreateProject(ctx context.Context, name string, now time.Time) (Project, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO projects (name, created_at) VALUES (?, ?)", name, now.Unix())
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return Project{ID: id, Name: name, CreatedAt: unix(now.Unix())}, nil
}

// Project returns the project with the id, or ErrNotFound.
func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	var p Project
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name, created_at FROM projects WHERE id = ?", id).
		Scan(&p.ID, &p.Name, &createdAt)
	if err != nil {
		return Project{}, fmt.Errorf("project %d: %w", id, noRows(err))
	}
	p.CreatedAt = unix(createdAt)
	return p, nil
}

// AllProjects returns every project, by name.
func (s *Store) AllProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, created_at FROM projects ORDER BY name, id")
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return scanProjects(rows)
}

// UserProjects returns the projects the account is a member of, by name.
func (s *Store) UserProjects(ctx context.Context, userID int64) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.created_at
		FROM projects p JOIN memberships m ON m.project_id = p.id
		WHERE m.user_id = ?
		ORDER BY p.name, p.id`, userID)
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
		var p Project
		var createdAt int64
		if err := rows.Scan(&p.ID, &p.Name, &createdAt); err != nil {
			return nil, err
		}
		p.CreatedAt = unix(createdAt)
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
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO memberships (project_id, user_id) VALUES (?, ?)", projectID, userID); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	for _, p := range permissions {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO member_permissions (project_id, user_id, permission) VALUES (?, ?, ?)",
			projectID, userID, p); err != nil {
			return fmt.Errorf("add member: %w", err)
		}
	}
	return tx.Commit()
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
