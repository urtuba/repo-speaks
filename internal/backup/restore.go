package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

// Restored says what Restore did.
type Restored struct {
	Version int    // the restored database's schema version
	Aside   string // where the old database went, or "" when there was none
}

// Restore puts the backup in file in place of the database at dbPath.
// The server must be stopped; Restore refuses while another process has
// the database open.
//
// It decrypts the backup next to the database and runs SQLite's
// integrity check on it. A backup from a newer version is refused. Only
// then does the current database move aside, to
// <dbPath>.before-restore-<time>, with its WAL files if any. A backup
// from an older version is fine: the next start brings it up to date.
func Restore(ctx context.Context, file, dbPath string, passphrase []byte, now time.Time) (Restored, error) {
	// Check first, so a running server is found before the slow part.
	hasDB := true
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		hasDB = false
	} else if err != nil {
		return Restored{}, err
	}
	if hasDB {
		err := store.PrepareMove(ctx, dbPath)
		if errors.Is(err, store.ErrInUse) {
			return Restored{}, errors.New("the database is in use: stop the server first")
		}
		// Any other error means the current database cannot be read,
		// such as a damaged file. It still moves aside as it is.
	}

	tmp, err := decryptTo(ctx, file, filepath.Dir(dbPath), passphrase)
	if err != nil {
		return Restored{}, err
	}
	defer removeDB(tmp) // a no-op once tmp is in place
	version, err := store.CheckFile(ctx, tmp)
	if err != nil {
		return Restored{}, fmt.Errorf("restore: %w", err)
	}

	r := Restored{Version: version}
	if hasDB {
		r.Aside = dbPath + ".before-restore-" + now.UTC().Format(stamp)
		if err := moveDB(dbPath, r.Aside); err != nil {
			return Restored{}, fmt.Errorf("restore: move the current database aside: %w", err)
		}
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		if hasDB {
			// Put the old database back, so the server can start as before.
			if berr := moveDB(r.Aside, dbPath); berr != nil {
				return Restored{}, fmt.Errorf("restore: %w; the old database is at %s", err, r.Aside)
			}
		}
		return Restored{}, fmt.Errorf("restore: %w", err)
	}
	syncDir(filepath.Dir(dbPath))
	return r, nil
}

// decryptTo decrypts the backup in file into a new temporary file in
// dir, with mode 0600, and returns its path. On failure, nothing is
// left behind.
func decryptTo(ctx context.Context, file, dir string, passphrase []byte) (path string, err error) {
	in, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, "repo-speaks-restore-*.db.tmp")
	if err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	if err := decrypt(ctx, out, in, passphrase); err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	return out.Name(), nil
}

// sidecars are the files SQLite keeps next to a database.
var sidecars = []string{"-wal", "-shm", "-journal"}

// moveDB renames a database file and the files SQLite keeps next to
// it. The new names keep the same endings, so SQLite still finds them.
func moveDB(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	for _, s := range sidecars {
		err := os.Rename(from+s, to+s)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeDB removes a database file and the files SQLite keeps next to
// it.
func removeDB(path string) {
	os.Remove(path)
	for _, s := range sidecars {
		os.Remove(path + s)
	}
}
