package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
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

	if err := migrate(ctx, db, steps); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, broken); err == nil {
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

	// A database newer than the program must not open.
	if err := migrate(ctx, db, fstest.MapFS{"migrations/0001_a.sql": steps["migrations/0001_a.sql"]}); err == nil {
		t.Error("migrate with fewer steps than the database: no error")
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
