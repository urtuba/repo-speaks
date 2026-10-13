package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// fakeSyncer stands in for the sync package.
type fakeSyncer struct {
	mu      sync.Mutex
	sync    func(ctx context.Context, resourceID int64) error
	synced  []int64
	removes int
}

func (f *fakeSyncer) Sync(ctx context.Context, resourceID int64) error {
	f.mu.Lock()
	f.synced = append(f.synced, resourceID)
	fn := f.sync
	f.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, resourceID)
}

func (f *fakeSyncer) RemoveDeleted(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	return nil
}

func (f *fakeSyncer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.synced)
}

// lockedBuffer is a log output that two goroutines may share.
type lockedBuffer struct {
	mu  sync.Mutex
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

// fixture is a loop with a fake syncer and a fake clock, and one project
// with one resource, whose first sync is queued.
type fixture struct {
	l        *Loop
	store    *store.Store
	db       *sql.DB // the same database, for looking at job rows
	syncer   *fakeSyncer
	log      *lockedBuffer
	project  store.Project
	resource store.Resource
	now      time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p, err := st.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.AddResource(ctx, store.NewResource{ProjectID: p.ID, Owner: "urtuba", Repo: "demo"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: st, db: db, syncer: &fakeSyncer{}, log: &lockedBuffer{}, project: p, resource: r, now: testNow}
	f.l = New(st, f.syncer, slog.New(slog.NewTextHandler(f.log, nil)))
	f.l.now = func() time.Time { return f.now }
	return f
}

// job is the resource's latest jobs row.
type job struct {
	id       int64
	status   string
	attempts int
	runAfter time.Time
	err      string
}

func (f *fixture) job(t *testing.T) job {
	t.Helper()
	var j job
	var runAfter int64
	var msg sql.NullString
	err := f.db.QueryRow(`
		SELECT id, status, attempts, run_after, error FROM jobs
		WHERE resource_id = ? ORDER BY id DESC LIMIT 1`, f.resource.ID).
		Scan(&j.id, &j.status, &j.attempts, &runAfter, &msg)
	if err != nil {
		t.Fatal(err)
	}
	j.runAfter = time.Unix(runAfter, 0).UTC()
	j.err = msg.String
	return j
}

func (f *fixture) lastError(t *testing.T) string {
	t.Helper()
	r, err := f.store.Resource(context.Background(), f.project.ID, f.resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r.LastError
}

// runNext runs the next due job and checks whether there was one.
func (f *fixture) runNext(t *testing.T, want bool) {
	t.Helper()
	ran, err := f.l.runNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ran != want {
		t.Fatalf("runNext() ran = %v, want %v", ran, want)
	}
}

// permanentErr is an error that trying again cannot fix, like the
// sync's errors for a missing repository.
type permanentErr struct{}

func (permanentErr) Error() string   { return "repository not found" }
func (permanentErr) Permanent() bool { return true }

func TestJobDone(t *testing.T) {
	f := newFixture(t)
	f.runNext(t, true)
	if got := f.job(t); got.status != "done" || got.attempts != 1 {
		t.Errorf("job = %+v, want done after 1 attempt", got)
	}
	if f.syncer.calls() != 1 || f.syncer.synced[0] != f.resource.ID {
		t.Errorf("synced = %v, want resource %d once", f.syncer.synced, f.resource.ID)
	}
	f.runNext(t, false)
}

func TestRetriesThenFails(t *testing.T) {
	f := newFixture(t)
	f.syncer.sync = func(context.Context, int64) error { return errors.New("network down") }

	// Try 1 fails: the job waits 1 minute. The resource shows no error yet.
	f.runNext(t, true)
	if got := f.job(t); got.status != "queued" || got.attempts != 1 || !got.runAfter.Equal(testNow.Add(time.Minute)) || got.err != "network down" {
		t.Errorf("after try 1: %+v", got)
	}
	if got := f.lastError(t); got != "" {
		t.Errorf("resource error after try 1 = %q, want none", got)
	}
	f.runNext(t, false) // not due yet

	// Try 2 fails: the job waits 4 minutes.
	f.now = f.now.Add(time.Minute)
	f.runNext(t, true)
	if got := f.job(t); got.status != "queued" || got.attempts != 2 || !got.runAfter.Equal(f.now.Add(4*time.Minute)) {
		t.Errorf("after try 2: %+v", got)
	}

	// Try 3 fails for good, and the resource shows the error.
	f.now = f.now.Add(4 * time.Minute)
	f.runNext(t, true)
	if got := f.job(t); got.status != "failed" || got.attempts != 3 || got.err != "network down" {
		t.Errorf("after try 3: %+v", got)
	}
	if got := f.lastError(t); got != "network down" {
		t.Errorf("resource error = %q, want network down", got)
	}
	f.now = f.now.Add(time.Hour)
	f.runNext(t, false)
	if f.syncer.calls() != 3 {
		t.Errorf("syncs = %d, want 3", f.syncer.calls())
	}
	if !strings.Contains(f.log.String(), "network down") {
		t.Error("the failure is not in the log")
	}
}

func TestPermanentErrorFailsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.syncer.sync = func(context.Context, int64) error { return permanentErr{} }
	f.runNext(t, true)
	if got := f.job(t); got.status != "failed" || got.attempts != 1 {
		t.Errorf("job = %+v, want failed after 1 attempt", got)
	}
	if got := f.lastError(t); got != "repository not found" {
		t.Errorf("resource error = %q", got)
	}
}

func TestPanicFailsTheJob(t *testing.T) {
	f := newFixture(t)
	f.syncer.sync = func(context.Context, int64) error { panic("nil map") }
	f.runNext(t, true) // must not panic
	if got := f.job(t); got.status != "failed" {
		t.Errorf("job status = %q, want failed", got.status)
	}
	if got := f.lastError(t); !strings.Contains(got, "internal error") {
		t.Errorf("resource error = %q, want an internal error", got)
	}
	if log := f.log.String(); !strings.Contains(log, "nil map") || !strings.Contains(log, "stack") {
		t.Errorf("log = %q, want the panic and its stack", log)
	}
}

func TestTimeoutFailsTheJob(t *testing.T) {
	f := newFixture(t)
	f.l.syncTimeout = 20 * time.Millisecond
	f.syncer.sync = func(ctx context.Context, _ int64) error {
		<-ctx.Done()
		return errors.New("git clone: stopped: context deadline exceeded")
	}
	f.runNext(t, true)
	if got := f.job(t); got.status != "failed" || !strings.Contains(got.err, "took longer than 20ms") {
		t.Errorf("job = %+v, want failed for taking too long", got)
	}
}

func TestUnknownKindFails(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec("UPDATE jobs SET kind = 'mystery'"); err != nil {
		t.Fatal(err)
	}
	f.runNext(t, true)
	if got := f.job(t); got.status != "failed" || !strings.Contains(got.err, `unknown job kind "mystery"`) {
		t.Errorf("job = %+v", got)
	}
	if f.syncer.calls() != 0 {
		t.Error("an unknown job ran a sync")
	}
}

func TestRunRequeuesStuckJobs(t *testing.T) {
	f := newFixture(t)
	// An earlier run claimed the job and then crashed.
	if _, err := f.store.ClaimJob(context.Background(), testNow); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.l.Run(ctx) }()
	waitFor(t, func() bool { return f.job(t).status == "done" })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run() = %v", err)
	}
	if got := f.job(t); got.attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the cut-off try counts)", got.attempts)
	}
}

func TestStuckJobOutOfTries(t *testing.T) {
	f := newFixture(t)
	// The third try was cut off by a crash, and requeued at start.
	if _, err := f.db.Exec("UPDATE jobs SET attempts = 3"); err != nil {
		t.Fatal(err)
	}
	f.runNext(t, true)
	if got := f.job(t); got.status != "failed" || got.attempts != 4 {
		t.Errorf("job = %+v, want failed", got)
	}
	if got := f.lastError(t); !strings.Contains(got, "server stopped during the last try") {
		t.Errorf("resource error = %q", got)
	}
	if f.syncer.calls() != 0 {
		t.Error("a job out of tries ran again")
	}
}

func TestShutdownPutsTheJobBack(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	f.syncer.sync = func(ctx context.Context, _ int64) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.l.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the shutdown")
	}
	if got := f.job(t); got.status != "queued" || got.attempts != 0 || got.err != "" {
		t.Errorf("job after shutdown = %+v, want queued with no try counted", got)
	}
	if got := f.lastError(t); got != "" {
		t.Errorf("resource error = %q, want none", got)
	}
}

func TestRunStopsWhenIdle(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.l.Run(ctx) }()
	waitFor(t, func() bool { return f.job(t).status == "done" })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if strings.Contains(f.log.String(), "level=ERROR") {
		t.Errorf("a clean stop logged an error:\n%s", f.log)
	}
}

func TestTick(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.runNext(t, true) // the first sync, made at testNow
	if err := f.store.SaveSync(ctx, f.resource.ID, "1", "abc", testNow); err != nil {
		t.Fatal(err)
	}
	first := f.job(t)

	// Within the interval (6 hours by default), nothing is queued.
	f.now = testNow.Add(5 * time.Hour)
	f.l.tick(ctx)
	if got := f.job(t); got.id != first.id {
		t.Errorf("a sync was queued %v after the last one", f.now.Sub(testNow))
	}
	if f.syncer.removes != 1 {
		t.Errorf("RemoveDeleted calls = %d, want 1", f.syncer.removes)
	}

	// After it, a sync is queued.
	f.now = testNow.Add(6 * time.Hour)
	f.l.tick(ctx)
	if got := f.job(t); got.id == first.id || got.status != "queued" {
		t.Errorf("latest job = %+v, want a new queued one", got)
	}

	// After 7 days the done job is deleted; the queued one stays.
	f.now = testNow.Add(8 * 24 * time.Hour)
	f.l.tick(ctx)
	var n int
	if err := f.db.QueryRow("SELECT count(*) FROM jobs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

func TestRetryWait(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: time.Minute, 2: 4 * time.Minute} {
		if got := retryWait(attempt); got != want {
			t.Errorf("retryWait(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// waitFor waits up to 5 seconds for cond to hold.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
