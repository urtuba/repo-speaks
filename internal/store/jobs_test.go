package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// jobRow is a jobs row as the tests check it.
type jobRow struct {
	status   string
	attempts int
	runAfter int64
	locked   bool
	err      string
}

func (s *Store) jobRow(t *testing.T, id int64) jobRow {
	t.Helper()
	var j jobRow
	var msg *string
	err := s.db.QueryRowContext(context.Background(), `
		SELECT status, attempts, run_after, locked_at IS NOT NULL, error
		FROM jobs WHERE id = ?`, id).Scan(&j.status, &j.attempts, &j.runAfter, &j.locked, &msg)
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil {
		j.err = *msg
	}
	return j
}

func (s *Store) countJobs(t *testing.T, resourceID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM jobs WHERE resource_id = ?", resourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQueueSyncKeepsOneOpenJob(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", "") // queues the first sync

	if queued, err := f.s.QueueSync(ctx, r.ID, testNow); err != nil || queued {
		t.Errorf("while queued: QueueSync() = %v, %v, want false", queued, err)
	}
	job, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if queued, err := f.s.QueueSync(ctx, r.ID, testNow); err != nil || queued {
		t.Errorf("while running: QueueSync() = %v, %v, want false", queued, err)
	}
	if err := f.s.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if queued, err := f.s.QueueSync(ctx, r.ID, testNow); err != nil || !queued {
		t.Errorf("after done: QueueSync() = %v, %v, want true", queued, err)
	}
	if n := f.s.countJobs(t, r.ID); n != 2 {
		t.Errorf("jobs = %d, want 2", n)
	}
	if _, err := f.s.QueueSync(ctx, 999, testNow); err == nil {
		t.Error("QueueSync() for a missing resource: error = nil, want a foreign key error")
	}
}

func TestClaimJob(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	first := f.add(t, "urtuba", "first", "", "")
	second := f.add(t, "urtuba", "second", "", "")

	// Both are due now; the older one comes first.
	j1, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if j1.ResourceID != first.ID || j1.Kind != JobSyncResource || j1.Attempts != 1 {
		t.Errorf("first claim = %+v, want resource %d, attempt 1", j1, first.ID)
	}
	if row := f.s.jobRow(t, j1.ID); row.status != "running" || !row.locked {
		t.Errorf("claimed job row = %+v, want running and locked", row)
	}

	// A job that waits for a retry is not due yet.
	if err := f.s.RetryJob(ctx, j1.ID, testNow.Add(time.Minute), "network down"); err != nil {
		t.Fatal(err)
	}
	j2, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if j2.ResourceID != second.ID {
		t.Errorf("second claim = resource %d, want %d", j2.ResourceID, second.ID)
	}
	if _, err := f.s.ClaimJob(ctx, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("nothing due: error = %v, want ErrNotFound", err)
	}

	again, err := f.s.ClaimJob(ctx, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != j1.ID || again.Attempts != 2 {
		t.Errorf("claim after the wait = %+v, want job %d, attempt 2", again, j1.ID)
	}
}

func TestJobEndings(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", "")
	job, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.s.RetryJob(ctx, job.ID, testNow.Add(time.Minute), "network down"); err != nil {
		t.Fatal(err)
	}
	want := jobRow{status: "queued", attempts: 1, runAfter: testNow.Add(time.Minute).Unix(), err: "network down"}
	if got := f.s.jobRow(t, job.ID); got != want {
		t.Errorf("after RetryJob: %+v, want %+v", got, want)
	}
	// A retry is not shown on the resource yet.
	if got, _ := f.s.Resource(ctx, f.project.ID, r.ID); got.LastError != "" {
		t.Errorf("LastError after a retry = %q, want none", got.LastError)
	}

	job, err = f.s.ClaimJob(ctx, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReleaseJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.s.jobRow(t, job.ID); got.status != "queued" || got.attempts != 1 || got.locked {
		t.Errorf("after ReleaseJob: %+v, want queued with 1 attempt", got)
	}

	job, err = f.s.ClaimJob(ctx, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.FailJob(ctx, job, "repository too large"); err != nil {
		t.Fatal(err)
	}
	if got := f.s.jobRow(t, job.ID); got.status != "failed" || got.err != "repository too large" {
		t.Errorf("after FailJob: %+v", got)
	}
	if got, _ := f.s.Resource(ctx, f.project.ID, r.ID); got.LastError != "repository too large" || got.OpenJob != "" {
		t.Errorf("resource after FailJob: LastError = %q, OpenJob = %q", got.LastError, got.OpenJob)
	}

	if _, err := f.s.QueueSync(ctx, r.ID, testNow); err != nil {
		t.Fatal(err)
	}
	job, err = f.s.ClaimJob(ctx, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.s.jobRow(t, job.ID); got.status != "done" || got.err != "" {
		t.Errorf("after FinishJob: %+v", got)
	}
}

func TestRequeueStuckJobs(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	f.add(t, "urtuba", "old", "", "")
	f.add(t, "urtuba", "new", "", "")
	stuck, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.s.ClaimJob(ctx, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	n, err := f.s.RequeueStuckJobs(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("requeued = %d, want 1", n)
	}
	if got := f.s.jobRow(t, stuck.ID); got.status != "queued" || got.locked || got.attempts != 1 {
		t.Errorf("stuck job: %+v, want queued, unlocked, the try counted", got)
	}
	if got := f.s.jobRow(t, fresh.ID); got.status != "running" {
		t.Errorf("fresh job: status = %q, want running", got.status)
	}
}

func TestQueueDueSyncs(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	hour := time.Hour
	now := testNow.Add(24 * hour)

	// finish runs the resource's open job to its end at the given time.
	finish := func(r Resource, at time.Time, ok bool) {
		t.Helper()
		job, err := f.s.ClaimJob(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if job.ResourceID != r.ID {
			t.Fatalf("claimed resource %d, want %d", job.ResourceID, r.ID)
		}
		if ok {
			err = f.s.FinishJob(ctx, job.ID)
			if err == nil {
				err = f.s.SaveSync(ctx, r.ID, "1", "abc", at)
			}
		} else {
			err = f.s.FailJob(ctx, job, "failed")
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// The project syncs every 6 hours. Jobs are made at testNow.
	stale := f.add(t, "urtuba", "stale", "", "")
	finish(stale, testNow, true) // last good sync 24 hours ago: due
	fresh := f.add(t, "urtuba", "fresh", "", "")
	finish(fresh, testNow, true)
	if err := f.s.SaveSync(ctx, fresh.ID, "1", "abc", now.Add(-hour)); err != nil {
		t.Fatal(err) // synced an hour ago: not due
	}
	failedLongAgo := f.add(t, "urtuba", "failed", "", "")
	finish(failedLongAgo, testNow, false) // never synced, tried 24 hours ago: due

	// A resource tried an hour ago waits, even though it never synced.
	failedRecently, err := f.s.AddResource(ctx, NewResource{ProjectID: f.project.ID, Owner: "urtuba", Repo: "recent"}, now.Add(-hour))
	if err != nil {
		t.Fatal(err)
	}
	finish(failedRecently, now.Add(-hour), false)

	// A project with the timer off never queues.
	off, err := f.s.AddResource(ctx, NewResource{ProjectID: f.other.ID, Owner: "urtuba", Repo: "off"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	finish(off, testNow, true)
	if err := f.s.SetSyncSettings(ctx, f.other.ID, 0, 5<<20); err != nil {
		t.Fatal(err)
	}

	// Added last, so the claims above do not take its job.
	open := f.add(t, "urtuba", "open", "", "") // still queued: not due

	n, err := f.s.QueueDueSyncs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("queued = %d, want 2", n)
	}
	for _, tt := range []struct {
		name string
		r    Resource
		jobs int
	}{
		{"stale", stale, 2},
		{"fresh", fresh, 1},
		{"open", open, 1},
		{"failed long ago", failedLongAgo, 2},
		{"failed recently", failedRecently, 1},
		{"timer off", off, 1},
	} {
		if got := f.s.countJobs(t, tt.r.ID); got != tt.jobs {
			t.Errorf("%s: jobs = %d, want %d", tt.name, got, tt.jobs)
		}
	}

	// Running it again queues nothing new.
	if n, err := f.s.QueueDueSyncs(ctx, now); err != nil || n != 0 {
		t.Errorf("second run: queued = %d, %v, want 0", n, err)
	}
}

func TestDeleteFinishedJobs(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", "")
	job, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.QueueSync(ctx, r.ID, testNow); err != nil {
		t.Fatal(err)
	}

	if err := f.s.DeleteFinishedJobs(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countJobs(t, r.ID); n != 2 {
		t.Errorf("jobs after deleting older ones = %d, want 2", n)
	}
	if err := f.s.DeleteFinishedJobs(ctx, testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// The done job is gone; the queued one stays.
	if n := f.s.countJobs(t, r.ID); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
	if got := f.s.jobRow(t, job.ID+1); got.status != "queued" {
		t.Errorf("open job: status = %q, want queued", got.status)
	}
}
