package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// migrationFiles holds the schema steps. Each file is named NNNN_name.sql;
// the numbers start at 1 and have no gaps. A step never changes once it is
// released: a change to the schema is a new step.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one numbered schema step.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the steps in order and checks their numbering.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	// fs.Glob returns names in lexical order, and the numbers are padded.
	var list []migration
	for i, name := range names {
		base := path.Base(name)
		num, _, _ := strings.Cut(base, "_")
		version, err := strconv.Atoi(num)
		if err != nil || version != i+1 {
			return nil, fmt.Errorf("migration %s: want number %04d", base, i+1)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		list = append(list, migration{version: version, name: base, sql: string(body)})
	}
	return list, nil
}

// migrate applies the steps the database does not have yet. PRAGMA
// user_version records the last step applied. Each step runs in its own
// transaction with its version bump, so a failed step leaves no trace.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	list, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(list) {
		return fmt.Errorf("database schema version %d is newer than this program (%d)", current, len(list))
	}
	for _, m := range list[current:] {
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// apply runs one step and sets user_version in one transaction.
func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // a no-op after Commit
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	// PRAGMA does not take ? placeholders; the version is an int we made.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}
