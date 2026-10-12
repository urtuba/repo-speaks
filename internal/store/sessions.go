package store

import (
	"context"
	"fmt"
	"time"
)

// CreateSession stores a new session. idHash is the hash of the cookie's
// token: the store never sees the token itself.
func (s *Store) CreateSession(ctx context.Context, idHash string, userID int64, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions (id_hash, user_id, expires_at) VALUES (?, ?, ?)",
		idHash, userID, expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionUser returns the account of a session that has not expired at
// now, and the session's expiry. It returns ErrNotFound for a missing or
// expired session.
func (s *Store) SessionUser(ctx context.Context, idHash string, now time.Time) (User, time.Time, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.password_hash, u.is_owner, u.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ? AND s.expires_at > ?`,
		idHash, now.Unix())
	var u User
	var createdAt, expiresAt int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsOwner, &createdAt, &expiresAt)
	if err != nil {
		return User{}, time.Time{}, fmt.Errorf("session: %w", noRows(err))
	}
	u.CreatedAt = unix(createdAt)
	return u, unix(expiresAt), nil
}

// ExtendSession sets a new expiry on a session.
func (s *Store) ExtendSession(ctx context.Context, idHash string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET expires_at = ? WHERE id_hash = ?", expiresAt.Unix(), idHash)
	if err != nil {
		return fmt.Errorf("extend session: %w", err)
	}
	return nil
}

// DeleteSession ends one session, as at logout.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash = ?", idHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions that ended before now.
// Expired sessions already fail SessionUser; this only frees space.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now.Unix()); err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}
