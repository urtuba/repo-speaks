package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
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

// SchemaVersion is the number of the newest schema step this program
// knows. A database at a higher number was written by a newer version.
func SchemaVersion() int {
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		panic(err) // the tests check the embedded steps; a failure here is a bug
	}
	return len(list)
}

// keepUpgradeCopies is how many copies from before a schema change stay
// in the upgrades folder.
const keepUpgradeCopies = 3

// migrate applies the steps the database does not have yet. PRAGMA
// user_version records the last step applied. Each step runs in its own
// transaction with its version bump, so a failed step leaves no trace.
//
// Before it changes a database that already has data, it writes a copy
// into upgradesDir, so an upgrade can be rolled back. A new, empty
// database gets no copy.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS, upgradesDir string) error {
	list, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(list) {
		return newerError(current)
	}
	if current > 0 && current < len(list) {
		if err := copyBeforeUpgrade(ctx, db, upgradesDir, current+1, time.Now()); err != nil {
			return err
		}
	}
	for _, m := range list[current:] {
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// newerError says that a newer version wrote the database. version is
// the database's schema version.
func newerError(version int) error {
	return fmt.Errorf("this database was written by a newer version of Repo Speaks (migration %04d); run that version or newer", version)
}

// copyBeforeUpgrade writes a copy of the database into dir, named after
// the first step it is about to apply, and keeps only the newest
// copies. When a copy before that step exists already, an earlier start
// made it and the step failed since. No new copy is made, so a server
// that fails on every start cannot prune away the copy that matters.
func copyBeforeUpgrade(ctx context.Context, db *sql.DB, dir string, next int, now time.Time) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("copy before upgrade: %w", err)
	}
	earlier, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("repo-speaks-before-%04d-*.db", next)))
	if err != nil {
		return fmt.Errorf("copy before upgrade: %w", err)
	}
	if len(earlier) > 0 {
		return nil
	}
	name := fmt.Sprintf("repo-speaks-before-%04d-%s.db", next, now.UTC().Format("20060102T150405Z"))
	if err := copyDB(ctx, db, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("copy before upgrade: %w", err)
	}
	if err := pruneUpgradeCopies(dir, keepUpgradeCopies); err != nil {
		return fmt.Errorf("copy before upgrade: %w", err)
	}
	return nil
}

// pruneUpgradeCopies deletes all but the newest keep copies in dir. A
// copy's age is its modification time, since a restored backup can
// start a second upgrade from an older step.
func pruneUpgradeCopies(dir string, keep int) error {
	names, err := filepath.Glob(filepath.Join(dir, "repo-speaks-before-*.db"))
	if err != nil {
		return err
	}
	type copyFile struct {
		path    string
		modTime time.Time
	}
	var copies []copyFile
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			return err
		}
		copies = append(copies, copyFile{name, info.ModTime()})
	}
	// Newest first; the name breaks a tie, since it holds the time too.
	slices.SortFunc(copies, func(a, b copyFile) int {
		return cmp.Or(b.modTime.Compare(a.modTime), cmp.Compare(b.path, a.path))
	})
	for _, c := range copies[min(keep, len(copies)):] {
		if err := os.Remove(c.path); err != nil {
			return err
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

// copyDB writes a consistent copy of the database to path with VACUUM
// INTO. A plain file copy could catch a write halfway. path must not
// exist, or be an empty file. The copy gets mode 0600, since it holds
// every key and token. On failure, path is removed, unless it held data
// before.
func copyDB(ctx context.Context, db *sql.DB, path string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && info.Size() > 0 {
		f.Close()
		return fmt.Errorf("%s is not empty", path)
	}
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	if err == nil {
		// An existing empty file keeps its mode, so set it here.
		err = f.Chmod(0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	return nil
}
