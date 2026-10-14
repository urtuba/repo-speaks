package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/backup"
	"github.com/urtuba/repo-speaks/internal/store"
)

const testPassphrase = "correct horse battery staple"

// backupEnv returns a data folder with the owner samed, a backup
// folder, and the settings for both.
func backupEnv(t *testing.T) (dataDir, backupDir string, getenv func(string) string) {
	t.Helper()
	dataDir, _ = newDataDir(t)
	backupDir = t.TempDir()
	env := map[string]string{
		"REPO_SPEAKS_DATA_DIR":          dataDir,
		"REPO_SPEAKS_BACKUP_DIR":        backupDir,
		"REPO_SPEAKS_BACKUP_PASSPHRASE": testPassphrase,
	}
	return dataDir, backupDir, func(key string) string { return env[key] }
}

func TestBackupAndRestoreCommands(t *testing.T) {
	ctx := context.Background()
	dataDir, backupDir, getenv := backupEnv(t)

	var out bytes.Buffer
	if err := backupNow(ctx, getenv, &out); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(backupDir, "repo-speaks-*.db.gpg"))
	if len(files) != 1 {
		t.Fatalf("backup files = %v, want one", files)
	}
	if want := filepath.Base(files[0]) + " ("; !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "folder "+backupDir) {
		t.Errorf("output = %q, want the file and the folder", out.String())
	}

	// An account made after the backup is gone after the restore.
	st, err := store.Open(ctx, filepath.Join(dataDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "later", "hash", time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out.Reset()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	if err := restoreBackup(ctx, getenv, files[0], &out, now); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(dataDir, dbFile+".before-restore-20261014T120000Z")
	if !strings.Contains(out.String(), "The old database is at "+aside) {
		t.Errorf("output = %q, want the old database's place", out.String())
	}
	st, err = store.Open(ctx, filepath.Join(dataDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UserByUsername(ctx, "samed"); err != nil {
		t.Errorf("owner after restore: %v", err)
	}
	if _, err := st.UserByUsername(ctx, "later"); err == nil {
		t.Error("an account made after the backup is still there")
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("old database: %v", err)
	}
}

func TestRestoreWhileServerRuns(t *testing.T) {
	ctx := context.Background()
	dataDir, backupDir, getenv := backupEnv(t)
	if err := backupNow(ctx, getenv, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(backupDir, "*.gpg"))

	// The server holds the database open.
	st, err := store.Open(ctx, filepath.Join(dataDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = restoreBackup(ctx, getenv, files[0], new(bytes.Buffer), time.Now())
	if err == nil || !strings.Contains(err.Error(), "stop the server first") {
		t.Errorf("got %v, want stop the server first", err)
	}
}

func TestBackupCommandRefusals(t *testing.T) {
	ctx := context.Background()
	dataDir, _ := newDataDir(t)
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"backups off", map[string]string{"REPO_SPEAKS_DATA_DIR": dataDir}, "backups are off"},
		{"no passphrase", map[string]string{"REPO_SPEAKS_DATA_DIR": dataDir, "REPO_SPEAKS_BACKUP_DIR": t.TempDir()}, "REPO_SPEAKS_BACKUP_PASSPHRASE is empty"},
		{"short passphrase", map[string]string{"REPO_SPEAKS_DATA_DIR": dataDir, "REPO_SPEAKS_BACKUP_DIR": t.TempDir(), "REPO_SPEAKS_BACKUP_PASSPHRASE": "short secret"}, "too short"},
		{"folder in the data folder", map[string]string{"REPO_SPEAKS_DATA_DIR": dataDir, "REPO_SPEAKS_BACKUP_DIR": filepath.Join(dataDir, "backups"), "REPO_SPEAKS_BACKUP_PASSPHRASE": testPassphrase}, "inside the data folder"},
		{"no database", map[string]string{"REPO_SPEAKS_DATA_DIR": t.TempDir(), "REPO_SPEAKS_BACKUP_DIR": t.TempDir(), "REPO_SPEAKS_BACKUP_PASSPHRASE": testPassphrase}, "no database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := backupNow(ctx, func(key string) string { return tt.env[key] }, &out)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error()+out.String(), "short secret") {
				t.Error("output shows the passphrase")
			}
		})
	}
	if err := backupNow(ctx, func(key string) string {
		return map[string]string{"REPO_SPEAKS_DATA_DIR": dataDir}[key]
	}, new(bytes.Buffer)); !errors.Is(err, backup.ErrOff) {
		t.Errorf("backups off: got %v, want ErrOff", err)
	}
}

func TestRestoreCommandNeedsPassphrase(t *testing.T) {
	dataDir := t.TempDir()
	err := restoreBackup(context.Background(), func(key string) string {
		if key == "REPO_SPEAKS_DATA_DIR" {
			return dataDir
		}
		return ""
	}, "backup.db.gpg", new(bytes.Buffer), time.Now())
	if err == nil || !strings.Contains(err.Error(), "REPO_SPEAKS_BACKUP_PASSPHRASE") {
		t.Errorf("got %v, want a missing passphrase", err)
	}
}

func TestRestoreIntoNewDataFolder(t *testing.T) {
	ctx := context.Background()
	_, backupDir, getenv := backupEnv(t)
	if err := backupNow(ctx, getenv, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(backupDir, "*.gpg"))

	// A new machine: the data folder does not exist yet.
	newData := filepath.Join(t.TempDir(), "data")
	newEnv := func(key string) string {
		switch key {
		case "REPO_SPEAKS_DATA_DIR":
			return newData
		case "REPO_SPEAKS_BACKUP_PASSPHRASE":
			return testPassphrase
		}
		return ""
	}
	var out bytes.Buffer
	if err := restoreBackup(ctx, newEnv, files[0], &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "old database") {
		t.Errorf("output = %q, but there was no old database", out.String())
	}
	if _, err := store.CheckFile(ctx, filepath.Join(newData, dbFile)); err != nil {
		t.Error(err)
	}
}
