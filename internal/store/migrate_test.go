package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	want, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}

	// Opening twice must work: the second time there is nothing to do.
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != len(want) {
			t.Errorf("user_version = %d, want %d", version, len(want))
		}
		s.Close()
	}
}

func TestMigrate(t *testing.T) {
	steps := fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")},
		"migrations/0002_b.sql": {Data: []byte("CREATE TABLE b (x INTEGER); INSERT INTO b VALUES (1);")},
	}
	broken := fstest.MapFS{
		"migrations/0001_a.sql": steps["migrations/0001_a.sql"],
		"migrations/0002_b.sql": steps["migrations/0002_b.sql"],
		"migrations/0003_c.sql": {Data: []byte("CREATE TABLE c (x INTEGER); NOT SQL;")},
	}

	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	upgrades := filepath.Join(t.TempDir(), "upgrades")
	if err := migrate(ctx, db, steps, upgrades); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, broken, upgrades); err == nil {
		t.Fatal("migrate with a broken step: no error")
	}

	// The broken step must leave no trace: no table c, version still 2.
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Errorf("user_version = %d, want 2", version)
	}
	var tables int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'c'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Error("table c exists after its step failed")
	}

	// A database newer than the program must not open, and must say
	// which step it has.
	err = migrate(ctx, db, fstest.MapFS{"migrations/0001_a.sql": steps["migrations/0001_a.sql"]}, upgrades)
	want := "this database was written by a newer version of Repo Speaks (migration 0002); run that version or newer"
	if err == nil || err.Error() != want {
		t.Errorf("migrate with fewer steps than the database: error = %v, want %q", err, want)
	}
}

func TestOpenRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	newer := SchemaVersion() + 1
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", newer)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(ctx, path)
	want := fmt.Sprintf("newer version of Repo Speaks (migration %04d); run that version or newer", newer)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Open() error = %v, want it to contain %q", err, want)
	}
}

// openTestDB opens a database file for the migrate tests.
func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateCopiesBeforeUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	upgrades := filepath.Join(dir, "upgrades")
	first := fstest.MapFS{"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")}}
	second := fstest.MapFS{
		"migrations/0001_a.sql": first["migrations/0001_a.sql"],
		"migrations/0002_b.sql": {Data: []byte("CREATE TABLE b (x INTEGER);")},
	}
	db := openTestDB(t, filepath.Join(dir, "test.db"))

	// A new database gets no copy.
	if err := migrate(ctx, db, first, upgrades); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upgrades); !os.IsNotExist(err) {
		t.Errorf("upgrades folder after the first start: %v, want none", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO a VALUES (42)"); err != nil {
		t.Fatal(err)
	}

	// The upgrade to step 2 keeps a copy of the database at step 1.
	if err := migrate(ctx, db, second, upgrades); err != nil {
		t.Fatal(err)
	}
	copies, err := filepath.Glob(filepath.Join(upgrades, "repo-speaks-before-0002-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want one before step 0002", copies)
	}
	info, err := os.Stat(copies[0])
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("copy mode = %v, want 0600", mode)
	}
	old := openTestDB(t, copies[0])
	var version, x int
	if err := old.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := old.QueryRowContext(ctx, "SELECT x FROM a").Scan(&x); err != nil {
		t.Fatal(err)
	}
	if version != 1 || x != 42 {
		t.Errorf("copy has version %d and x = %d, want 1 and 42", version, x)
	}

	// Nothing to apply, so no new copy.
	if err := migrate(ctx, db, second, upgrades); err != nil {
		t.Fatal(err)
	}
	if again, _ := filepath.Glob(filepath.Join(upgrades, "*")); len(again) != 1 {
		t.Errorf("files after a start without changes = %v, want one", again)
	}
}

func TestPruneUpgradeCopies(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var names []string
	for i := range 5 {
		name := filepath.Join(dir, fmt.Sprintf("repo-speaks-before-%04d-x.db", 9-i))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// The first file is the oldest, though its step number is the highest.
		mod := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(name, mod, mod); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := pruneUpgradeCopies(dir, 3); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		_, err := os.Stat(name)
		if kept := err == nil; kept != (i >= 2) {
			t.Errorf("%s kept = %v, want %v", filepath.Base(name), kept, i >= 2)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other file removed: %v", err)
	}
}

func TestLoadMigrationsChecksNumbers(t *testing.T) {
	tests := []struct {
		name  string
		files []string
	}{
		{"gap", []string{"0001_a.sql", "0003_c.sql"}},
		{"does not start at 1", []string{"0002_b.sql"}},
		{"no number", []string{"first.sql"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for _, f := range tt.files {
				fsys["migrations/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
			}
			_, err := loadMigrations(fsys)
			if err == nil || !strings.Contains(err.Error(), "want number") {
				t.Errorf("loadMigrations() error = %v, want a numbering error", err)
			}
		})
	}
}

// TestCopyBeforeUpgradeOncePerStep checks that a step failing on every
// start gets one copy, not one per start, so pruning never removes the
// copy taken before the failing step.
func TestCopyBeforeUpgradeOncePerStep(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	upgrades := filepath.Join(dir, "upgrades")
	db := openTestDB(t, filepath.Join(dir, "test.db"))
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := range keepUpgradeCopies + 2 {
		if err := copyBeforeUpgrade(ctx, db, upgrades, 2, start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	copies, err := filepath.Glob(filepath.Join(upgrades, "repo-speaks-before-0002-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || !strings.Contains(copies[0], "20261014T120000Z") {
		t.Errorf("copies after repeated starts = %v, want only the first", copies)
	}
}
