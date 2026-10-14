package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// openTest opens a new database in a temporary folder.
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenSetsPragmas(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	tests := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"synchronous", "2"}, // FULL
	}
	for _, tt := range tests {
		t.Run(tt.pragma, func(t *testing.T) {
			var got string
			if err := s.db.QueryRowContext(ctx, "PRAGMA "+tt.pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("PRAGMA %s = %q, want %q", tt.pragma, got, tt.want)
			}
		})
	}
}

// TestOpenRestrictsFileMode checks that only the app's user can read the
// database, which holds API keys and tokens.
func TestOpenRestrictsFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo-speaks.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(name), mode)
		}
	}
}
