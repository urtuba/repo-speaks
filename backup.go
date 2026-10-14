package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urtuba/repo-speaks/internal/backup"
	"github.com/urtuba/repo-speaks/internal/config"
	"github.com/urtuba/repo-speaks/internal/store"
)

// backupNow makes one backup and prints where it went. Cron can call it
// for a schedule. It works while the server runs: the copy is made with
// VACUUM INTO, which sees a consistent database.
func backupNow(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg := config.Load(getenv)
	if err := cfg.Check(); err != nil {
		return err
	}
	if !cfg.Backup.On() {
		return backup.ErrOff
	}
	path := filepath.Join(cfg.DataDir, dbFile)
	// Opening a missing file would create an empty database, so check first.
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no database at %s: set REPO_SPEAKS_DATA_DIR to the server's data folder", path)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer st.Close()

	backups, err := backup.New(st, cfg.DataDir, cfg.Backup)
	if err != nil {
		return err
	}
	b, err := backups.Run(ctx)
	if b.Targets != "" {
		fmt.Fprintf(stdout, "Backup %s (%d bytes) written to %s.\n", b.Name, b.Size, b.Targets)
	}
	return err
}

// restoreBackup puts the backup in file in place of the database. The
// server must be stopped. The old database stays in the data folder
// under a new name.
func restoreBackup(ctx context.Context, getenv func(string) string, file string, stdout io.Writer, now time.Time) error {
	cfg := config.Load(getenv)
	if err := cfg.Check(); err != nil {
		return err
	}
	if cfg.Backup.Passphrase == "" {
		return errors.New("set REPO_SPEAKS_BACKUP_PASSPHRASE to the passphrase the backup was made with")
	}
	// On a new machine the data folder may not exist yet.
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data folder: %w", err)
	}
	r, err := backup.Restore(ctx, file, filepath.Join(cfg.DataDir, dbFile), []byte(cfg.Backup.Passphrase), now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restored %s (schema %04d).\n", file, r.Version)
	if r.Aside != "" {
		fmt.Fprintf(stdout, "The old database is at %s.\n", r.Aside)
	}
	fmt.Fprintln(stdout, "Start the server. If a repository's clone is missing, press Sync now or wait for its timer.")
	return nil
}
