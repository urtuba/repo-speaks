package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrResetGone means a reset link was used, replaced or has expired.
var ErrResetGone = errors.New("reset link no longer works")

// Reset is a password reset link for one account.
type Reset struct {
	ID        int64
	UserID    int64
	Username  string
	ExpiresAt time.Time
	Used      bool
}

// Open reports whether the link can still be used at now.
func (r Reset) Open(now time.Time) bool {
	return !r.Used && now.Before(r.ExpiresAt)
}

// CreatePasswordReset stores a new reset link for an account and removes
// the account's older unused links, in one transaction, so only the
// newest link works. tokenHash is the hash of the link's token.
func (s *Store) CreatePasswordReset(ctx context.Context, userID int64, tokenHash string, createdBy int64, now, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create reset link: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM password_resets WHERE user_id = ? AND used_at IS NULL", userID); err != nil {
		return fmt.Errorf("create reset link: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO password_resets (user_id, token_hash, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		userID, tokenHash, nullID(createdBy), now.Unix(), expiresAt.Unix()); err != nil {
		return fmt.Errorf("create reset link: %w", err)
	}
	return tx.Commit()
}

// PasswordResetByHash returns the reset link with the token hash, or
// ErrNotFound. It also returns used and expired links: check Open.
func (s *Store) PasswordResetByHash(ctx context.Context, tokenHash string) (Reset, error) {
	var r Reset
	var expiresAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT pr.id, pr.user_id, u.username, pr.expires_at, pr.used_at IS NOT NULL
		FROM password_resets pr JOIN users u ON u.id = pr.user_id
		WHERE pr.token_hash = ?`, tokenHash).
		Scan(&r.ID, &r.UserID, &r.Username, &expiresAt, &r.Used)
	if err != nil {
		return Reset{}, fmt.Errorf("reset link: %w", noRows(err))
	}
	r.ExpiresAt = unix(expiresAt)
	return r, nil
}

// UsePasswordReset sets a new password through a reset link, in one
// transaction: it marks the link used (only while it is still open),
// changes the password and ends all sessions of the account. It returns
// ErrResetGone when the link was used or expired in the meantime.
func (s *Store) UsePasswordReset(ctx context.Context, resetID int64, passwordHash string, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("use reset link: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit
	var userID int64
	err = tx.QueryRowContext(ctx, `
		UPDATE password_resets SET used_at = ?
		WHERE id = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id`, now.Unix(), resetID, now.Unix()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrResetGone
	}
	if err != nil {
		return 0, fmt.Errorf("use reset link: %w", err)
	}
	if err := setPassword(ctx, tx, userID, passwordHash); err != nil {
		return 0, fmt.Errorf("use reset link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("use reset link: %w", err)
	}
	return userID, nil
}

// Account is a row of the owner's account list.
type Account struct {
	User
	// ResetOpenUntil is when the account's open reset link stops
	// working, or the zero time when there is none.
	ResetOpenUntil time.Time
}

// Accounts returns every account by username, each with its open reset
// link's expiry, as seen at now.
func (s *Store) Accounts(ctx context.Context, now time.Time) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.is_owner, u.created_at, MAX(pr.expires_at)
		FROM users u
		LEFT JOIN password_resets pr
			ON pr.user_id = u.id AND pr.used_at IS NULL AND pr.expires_at > ?
		GROUP BY u.id
		ORDER BY u.username`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var list []Account
	for rows.Next() {
		var a Account
		var createdAt int64
		var resetUntil sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Username, &a.IsOwner, &createdAt, &resetUntil); err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		a.CreatedAt = unix(createdAt)
		if resetUntil.Valid {
			a.ResetOpenUntil = unix(resetUntil.Int64)
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
