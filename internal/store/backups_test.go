package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueBackupKeepsOneOpenJob(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if status, err := s.OpenBackupJob(ctx); err != nil || status != "" {
		t.Fatalf("before: OpenBackupJob() = %q, %v, want none", status, err)
	}
	if queued, err := s.QueueBackup(ctx, testNow); err != nil || !queued {
		t.Fatalf("first QueueBackup() = %v, %v, want true", queued, err)
	}
	if queued, err := s.QueueBackup(ctx, testNow); err != nil || queued {
		t.Errorf("while queued: QueueBackup() = %v, %v, want false", queued, err)
	}
	if status, _ := s.OpenBackupJob(ctx); status != "queued" {
		t.Errorf("OpenBackupJob() = %q, want queued", status)
	}

	job, err := s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != JobBackup || job.ResourceID != 0 {
		t.Errorf("claimed %+v, want a backup job without a resource", job)
	}
	if queued, err := s.QueueBackup(ctx, testNow); err != nil || queued {
		t.Errorf("while running: QueueBackup() = %v, %v, want false", queued, err)
	}
	if status, _ := s.OpenBackupJob(ctx); status != "running" {
		t.Errorf("OpenBackupJob() = %q, want running", status)
	}

	if err := s.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.QueueBackup(ctx, testNow); err != nil || !queued {
		t.Errorf("after the job: QueueBackup() = %v, %v, want true", queued, err)
	}
}

func TestBackupsNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for i, name := range []string{"first", "second", "third"} {
		start := testNow.Add(time.Duration(i) * time.Hour)
		b := Backup{Name: name, Size: int64(i), Targets: "folder /backups", StartedAt: start, FinishedAt: start.Add(time.Second)}
		if name == "second" {
			b.Error = "disk full"
		}
		if err := s.AddBackup(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Backups(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "third" || list[1].Name != "second" {
		t.Fatalf("Backups(2) = %+v, want third and second", list)
	}
	if got := list[1]; got.Error != "disk full" || got.Size != 1 || !got.StartedAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("second = %+v", got)
	}
}

func TestCopyTo(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.CreateOwner(ctx, "samed", "hash", testNow); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	path := filepath.Join(dir, "copy.db")
	if err := s.CopyTo(ctx, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %v, want 0600", mode)
	}
	if version, err := CheckFile(ctx, path); err != nil || version != SchemaVersion() {
		t.Errorf("CheckFile(copy) = %d, %v, want %d", version, err, SchemaVersion())
	}
	c, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.UserByUsername(ctx, "samed"); err != nil {
		t.Errorf("copy has no owner: %v", err)
	}

	// A file that is not empty is never written over, and stays.
	full := filepath.Join(dir, "full.db")
	if err := os.WriteFile(full, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyTo(ctx, full); err == nil {
		t.Error("CopyTo(a file with data): no error")
	}
	if got, err := os.ReadFile(full); err != nil || string(got) != "data" {
		t.Errorf("file with data after CopyTo = %q, %v, want it unchanged", got, err)
	}
}

func TestCheckFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	damaged := filepath.Join(dir, "damaged.db")
	if err := os.WriteFile(damaged, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFile(ctx, damaged); err == nil {
		t.Error("CheckFile(damaged) = nil, want an error")
	}

	if _, err := CheckFile(ctx, filepath.Join(dir, "missing.db")); err == nil {
		t.Error("CheckFile(missing) = nil, want an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("CheckFile created a missing file")
	}

	empty := filepath.Join(dir, "empty.db")
	db := openTestDB(t, empty)
	if _, err := db.ExecContext(ctx, "CREATE TABLE a (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFile(ctx, empty); err == nil || !strings.Contains(err.Error(), "no Repo Speaks data") {
		t.Errorf("CheckFile(other database) = %v, want no Repo Speaks data", err)
	}

	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion()+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFile(ctx, empty); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("CheckFile(newer) = %v, want newer version", err)
	}
}

func TestPrepareMove(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwner(ctx, "samed", "hash", testNow); err != nil {
		t.Fatal(err)
	}

	if err := PrepareMove(ctx, path); !errors.Is(err, ErrInUse) {
		t.Errorf("while open: PrepareMove() = %v, want ErrInUse", err)
	}
	s.Close()
	if err := PrepareMove(ctx, path); err != nil {
		t.Fatalf("after close: PrepareMove() = %v", err)
	}
	// The main file holds everything alone now.
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		t.Errorf("WAL file has %d bytes after PrepareMove", info.Size())
	}
	if _, err := CheckFile(ctx, path); err != nil {
		t.Error(err)
	}
}
