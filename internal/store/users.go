package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrSetupDone is returned by CreateOwner when an account already exists.
var ErrSetupDone = errors.New("setup already done")

// ErrUsernameTaken is returned when another account has the username.
var ErrUsernameTaken = errors.New("username taken")

// User is an account on the instance.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsOwner      bool // the instance owner, not a project role
	CreatedAt    time.Time
}

// HasUsers reports whether any account exists.
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM users)").Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check for users: %w", err)
	}
	return ok, nil
}

// CreateOwner creates the instance owner, but only while there is no
// account at all. The check and the insert are one statement, so two
// visitors at the same moment cannot both become owner.
func (s *Store) CreateOwner(ctx context.Context, username, passwordHash string, now time.Time) (User, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, is_owner, created_at)
		SELECT ?, ?, 1, ?
		WHERE NOT EXISTS (SELECT 1 FROM users)`,
		username, passwordHash, now.Unix())
	if err != nil {
		return User{}, fmt.Errorf("create owner: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return User{}, fmt.Errorf("create owner: %w", err)
	} else if n == 0 {
		return User{}, ErrSetupDone
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("create owner: %w", err)
	}
	return User{ID: id, Username: username, PasswordHash: passwordHash, IsOwner: true, CreatedAt: unix(now.Unix())}, nil
}

// CreateUser creates an account that is not the owner.
// The caller has checked the username and hashed the password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, now time.Time) (User, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)",
		username, passwordHash, now.Unix())
	if err != nil {
		if exists, _ := s.usernameExists(ctx, username); exists {
			return User{}, ErrUsernameTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return User{ID: id, Username: username, PasswordHash: passwordHash, CreatedAt: unix(now.Unix())}, nil
}

// usernameExists reports whether an account has the username.
func (s *Store) usernameExists(ctx context.Context, username string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM users WHERE username = ?)", username).Scan(&ok)
	return ok, err
}

// UserByUsername returns the account with the username, or ErrNotFound.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_owner, created_at
		FROM users WHERE username = ?`, username)
	u, err := scanUser(row)
	if err != nil {
		return User{}, fmt.Errorf("user %q: %w", username, err)
	}
	return u, nil
}

// Owner returns the instance owner, or ErrNotFound before setup.
func (s *Store) Owner(ctx context.Context) (User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_owner, created_at
		FROM users WHERE is_owner = 1`)
	u, err := scanUser(row)
	if err != nil {
		return User{}, fmt.Errorf("owner: %w", err)
	}
	return u, nil
}

// UserByID returns the account with the id, or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_owner, created_at
		FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if err != nil {
		return User{}, fmt.Errorf("user %d: %w", id, err)
	}
	return u, nil
}

// SetPassword changes an account's password and ends all its sessions,
// in one transaction. Anyone who knew the old password is logged out.
func (s *Store) SetPassword(ctx context.Context, userID int64, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit
	if err := setPassword(ctx, tx, userID, passwordHash); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return tx.Commit()
}

// setPassword changes the password and ends all sessions inside tx.
func setPassword(ctx context.Context, tx *sql.Tx, userID int64, passwordHash string) error {
	res, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", passwordHash, userID)
	if err != nil {
		return err
	}
	if err := oneRow(res, fmt.Sprintf("user %d", userID)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// DeleteUser removes an account. Its sessions and memberships go with it
// (ON DELETE CASCADE).
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID); err != nil {
		return fmt.Errorf("delete user %d: %w", userID, err)
	}
	return nil
}

// scanUser reads one users row. It returns ErrNotFound for no row.
func scanUser(row *sql.Row) (User, error) {
	var u User
	var createdAt int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsOwner, &createdAt); err != nil {
		return User{}, noRows(err)
	}
	u.CreatedAt = unix(createdAt)
	return u, nil
}

// unix turns stored Unix seconds into a UTC time.
func unix(sec int64) time.Time {
	return time.Unix(sec, 0).UTC()
}
