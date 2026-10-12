package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Errors returned by AcceptInvite.
var (
	// ErrInviteGone means the invite was used, revoked or has expired.
	ErrInviteGone = errors.New("invite no longer works")
	// ErrAlreadyMember means the account is already in the project.
	ErrAlreadyMember = errors.New("already a member")
)

// Invite is a link that adds one person to a project with a preset.
// The store keeps the preset as a plain name; the permissions package
// knows what it means.
type Invite struct {
	ID            int64
	ProjectID     int64
	ProjectName   string
	Preset        string
	CreatedBy     int64  // 0 when the creator's account is gone
	CreatedByName string // empty when the creator's account is gone
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Used          bool
	Revoked       bool
}

// Open reports whether the invite can still be used at now: not used,
// not revoked and not expired. Whether its creator may still give the
// preset is checked outside the store.
func (inv Invite) Open(now time.Time) bool {
	return !inv.Used && !inv.Revoked && now.Before(inv.ExpiresAt)
}

// CreateInvite stores a new invite. tokenHash is the hash of the link's
// token: the store never sees the token itself.
func (s *Store) CreateInvite(ctx context.Context, projectID int64, tokenHash, preset string, createdBy int64, now, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO invites (project_id, token_hash, preset, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, tokenHash, preset, createdBy, now.Unix(), expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("create invite: %w", err)
	}
	return nil
}

// inviteColumns is the SELECT list scanInvite reads.
const inviteColumns = `
	i.id, i.project_id, p.name, i.preset, i.created_by, u.username,
	i.created_at, i.expires_at, i.used_at IS NOT NULL, i.revoked_at IS NOT NULL
	FROM invites i
	JOIN projects p ON p.id = i.project_id
	LEFT JOIN users u ON u.id = i.created_by`

// scanner is a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanInvite reads one row selected with inviteColumns.
func scanInvite(row scanner) (Invite, error) {
	var inv Invite
	var createdBy sql.NullInt64
	var createdByName sql.NullString
	var createdAt, expiresAt int64
	err := row.Scan(&inv.ID, &inv.ProjectID, &inv.ProjectName, &inv.Preset,
		&createdBy, &createdByName, &createdAt, &expiresAt, &inv.Used, &inv.Revoked)
	if err != nil {
		return Invite{}, noRows(err)
	}
	inv.CreatedBy = createdBy.Int64
	inv.CreatedByName = createdByName.String
	inv.CreatedAt = unix(createdAt)
	inv.ExpiresAt = unix(expiresAt)
	return inv, nil
}

// InviteByHash returns the invite with the token hash, or ErrNotFound.
// It also returns used, revoked and expired invites: check Open.
func (s *Store) InviteByHash(ctx context.Context, tokenHash string) (Invite, error) {
	row := s.db.QueryRowContext(ctx, "SELECT"+inviteColumns+" WHERE i.token_hash = ?", tokenHash)
	inv, err := scanInvite(row)
	if err != nil {
		return Invite{}, fmt.Errorf("invite: %w", err)
	}
	return inv, nil
}

// OpenInvites returns a project's invites that can still be used at now,
// the newest first.
func (s *Store) OpenInvites(ctx context.Context, projectID int64, now time.Time) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+inviteColumns+`
		WHERE i.project_id = ? AND i.used_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > ?
		ORDER BY i.created_at DESC, i.id DESC`, projectID, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()
	var list []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("list invites: %w", err)
		}
		list = append(list, inv)
	}
	return list, rows.Err()
}

// RevokeInvite stops an open invite of the project from working. It
// returns ErrNotFound when the project has no such open invite.
func (s *Store) RevokeInvite(ctx context.Context, projectID, inviteID int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE invites SET revoked_at = ?
		WHERE id = ? AND project_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		now.Unix(), inviteID, projectID)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	} else if n == 0 {
		return fmt.Errorf("revoke invite %d: %w", inviteID, ErrNotFound)
	}
	return nil
}

// Acceptance describes who accepts an invite and what they get.
type Acceptance struct {
	Invite      Invite
	Permissions []string // the preset's permission names
	Now         time.Time

	// UserID is the accepting account. When it is 0, a new account is
	// made from Username and PasswordHash.
	UserID       int64
	Username     string
	PasswordHash string
}

// AcceptInvite adds the accepting account to the invite's project and
// marks the invite used, in one transaction. It makes the account first
// when the acceptance asks for a new one, and returns the account's id.
//
// Marking the invite used only succeeds while it is still open, so two
// people who use the same link at once cannot both join. On any error
// nothing changes, and the invite stays unused.
func (s *Store) AcceptInvite(ctx context.Context, a Acceptance) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("accept invite: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	userID := a.UserID
	if userID == 0 {
		var taken bool
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM users WHERE username = ?)", a.Username).Scan(&taken); err != nil {
			return 0, fmt.Errorf("accept invite: %w", err)
		}
		if taken {
			return 0, ErrUsernameTaken
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)",
			a.Username, a.PasswordHash, a.Now.Unix())
		if err != nil {
			return 0, fmt.Errorf("accept invite: %w", err)
		}
		if userID, err = res.LastInsertId(); err != nil {
			return 0, fmt.Errorf("accept invite: %w", err)
		}
	}

	var member bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM memberships WHERE project_id = ? AND user_id = ?)",
		a.Invite.ProjectID, userID).Scan(&member); err != nil {
		return 0, fmt.Errorf("accept invite: %w", err)
	}
	if member {
		return 0, ErrAlreadyMember
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE invites SET used_at = ?, used_by = ?
		WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		a.Now.Unix(), userID, a.Invite.ID, a.Now.Unix())
	if err != nil {
		return 0, fmt.Errorf("accept invite: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("accept invite: %w", err)
	} else if n == 0 {
		return 0, ErrInviteGone
	}

	if err := insertMember(ctx, tx, a.Invite.ProjectID, userID, a.Invite.CreatedBy, a.Permissions); err != nil {
		return 0, fmt.Errorf("accept invite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("accept invite: %w", err)
	}
	return userID, nil
}

// nullID stores 0 as NULL, for optional references to another row.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
