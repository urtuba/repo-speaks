package sync

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/jobs"
	"github.com/urtuba/repo-speaks/internal/store"
)

// lockedBuffer is a log output that two goroutines may share.
type lockedBuffer struct {
	mu  gosync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTokenNeverSavedOrLogged runs real syncs through the jobs loop, with
// a git that prints the token and its whole environment when it fails.
// The token must reach no saved error and no log line: not on a failed
// try, and not on the final failure, which is saved on the resource.
func TestTokenNeverSavedOrLogged(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t, "", testToken)
	final := f.resource
	retried, err := f.store.AddResource(ctx, store.NewResource{
		ProjectID: f.project.ID, Owner: "urtuba", Repo: "demo", Branch: "dev", Token: testToken,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	f.runner.cloneErr = fmt.Errorf("fatal: could not read with %s or %s; env: %s",
		testToken, basicAuth(testToken), strings.Join(f.s.env("/home", testToken), " "))

	db, err := sql.Open("sqlite", "file:"+filepath.Join(filepath.Dir(f.s.reposDir), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The loop runs on the real clock, so make both jobs due now. One
	// resource is on its last try, so its failure is final.
	if _, err := db.Exec("UPDATE jobs SET run_after = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE jobs SET attempts = 2 WHERE resource_id = ?", final.ID); err != nil {
		t.Fatal(err)
	}

	var log lockedBuffer
	loop := jobs.New(f.store, f.s, nil, slog.New(slog.NewTextHandler(&log, nil)))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- loop.Run(runCtx) }()

	// Wait until both jobs have saved an error.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM jobs WHERE error IS NOT NULL").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs with an error = %d, want 2\n%s", n, log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var saved []string
	rows, err := db.Query(`
		SELECT j.status || ': ' || j.error FROM jobs j
		UNION ALL
		SELECT 'resource: ' || ifnull(last_error, '') FROM resources`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(saved, "\n")
	for id, want := range map[int64]string{final.ID: "failed", retried.ID: "queued"} {
		var status string
		if err := db.QueryRow("SELECT status FROM jobs WHERE resource_id = ?", id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Errorf("job of resource %d: status = %q, want %q", id, status, want)
		}
	}
	r, err := f.store.Resource(ctx, f.project.ID, final.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.LastError, "[hidden]") {
		t.Errorf("resource error = %q, want the clone error with the token hidden", r.LastError)
	}
	if !strings.Contains(log.String(), "[hidden]") {
		t.Errorf("log does not show the failures:\n%s", log.String())
	}

	for _, secret := range []string{testToken, basicAuth(testToken), "secretvalue"} {
		if strings.Contains(all, secret) {
			t.Errorf("a saved error holds %q:\n%s", secret, all)
		}
		if strings.Contains(log.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, log.String())
		}
	}
}
