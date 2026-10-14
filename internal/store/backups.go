package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// JobBackup is the kind of a job that backs up the database.
const JobBackup = "backup"

// ErrInUse is returned when another process, such as a running server,
// has the database open.
var ErrInUse = errors.New("the database is in use")

// Backup is one backup this server made.
type Backup struct {
	ID         int64
	Name       string // the encrypted file's name
	Size       int64  // bytes; 0 when the backup failed before a target
	Targets    string // the targets that got the file
	Error      string // empty when every target got the file
	StartedAt  time.Time
	FinishedAt time.Time
}

// CopyTo writes a consistent copy of the database to path, while the
// server keeps running. path must not exist, or be an empty file. The
// copy gets mode 0600. On failure, path is removed.
func (s *Store) CopyTo(ctx context.Context, path string) error {
	return copyDB(ctx, s.db, path)
}

// QueueBackup queues a backup, unless one is already queued or
// running. It reports whether it queued one.
func (s *Store) QueueBackup(ctx context.Context, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (kind, run_after, created_at) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`,
		JobBackup, now.Unix(), now.Unix())
	if err != nil {
		return false, fmt.Errorf("queue backup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue backup: %w", err)
	}
	return n == 1, nil
}

// OpenBackupJob returns "queued" or "running" while a backup job is
// open, and "" otherwise.
func (s *Store) OpenBackupJob(ctx context.Context) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT status FROM jobs
		WHERE kind = ? AND resource_id IS NULL AND status IN ('queued', 'running')`,
		JobBackup).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open backup job: %w", err)
	}
	return status, nil
}

// AddBackup records a finished backup.
func (s *Store) AddBackup(ctx context.Context, b Backup) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO backups (name, size, targets, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		b.Name, b.Size, b.Targets, b.Error, b.StartedAt.Unix(), b.FinishedAt.Unix()); err != nil {
		return fmt.Errorf("add backup: %w", err)
	}
	return nil
}

// Backups returns the newest backups, at most limit of them.
func (s *Store) Backups(ctx context.Context, limit int) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, size, targets, error, started_at, finished_at
		FROM backups ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	defer rows.Close()
	var list []Backup
	for rows.Next() {
		var b Backup
		var started, finished int64
		if err := rows.Scan(&b.ID, &b.Name, &b.Size, &b.Targets, &b.Error, &started, &finished); err != nil {
			return nil, fmt.Errorf("list backups: %w", err)
		}
		b.StartedAt = time.Unix(started, 0).UTC()
		b.FinishedAt = time.Unix(finished, 0).UTC()
		list = append(list, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	return list, nil
}

// CheckFile checks the database file at path, such as a decrypted
// backup, without changing its schema. It runs SQLite's integrity check
// and returns the file's schema version. A file from a newer version of
// Repo Speaks is refused.
func CheckFile(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return 0, fmt.Errorf("check database: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return 0, fmt.Errorf("check database: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return 0, fmt.Errorf("check database: %w", err)
		}
		if line != "ok" && len(problems) < 3 {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("check database: %w", err)
	}
	if len(problems) > 0 {
		return 0, fmt.Errorf("check database: the file is damaged: %v", problems)
	}

	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("check database: %w", err)
	}
	if version == 0 {
		return 0, errors.New("check database: the file holds no Repo Speaks data")
	}
	if version > SchemaVersion() {
		return 0, newerError(version)
	}
	return version, nil
}

// PrepareMove readies the database at path to be moved aside, with the
// server stopped. It fails with ErrInUse when another process, such as
// a running server, has the database open. Otherwise it writes the
// write-ahead log back into the main file, so the file holds all the
// data alone.
func PrepareMove(ctx context.Context, path string) error {
	// Exclusive locking mode makes BEGIN EXCLUSIVE fail at once while
	// another connection has the database open, even an idle one.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return fmt.Errorf("prepare database move: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		if code, ok := errors.AsType[*sqlite.Error](err); ok && code.Code()&0xff == sqlite3.SQLITE_BUSY {
			return ErrInUse
		}
		return fmt.Errorf("prepare database move: %w", err)
	}
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("prepare database move: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("prepare database move: %w", err)
	}
	return nil
}
