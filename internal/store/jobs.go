package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// JobSyncResource is the kind of a job that syncs one resource.
const JobSyncResource = "sync_resource"

// Job is a piece of background work the jobs loop has claimed.
type Job struct {
	ID         int64
	Kind       string
	ResourceID int64 // 0 for a job that belongs to no resource
	Attempts   int   // tries so far, counting the one that is starting
}

// QueueSync queues a sync of the resource, unless one is already queued
// or running. It reports whether it queued one.
func (s *Store) QueueSync(ctx context.Context, resourceID int64, now time.Time) (bool, error) {
	ok, err := queueSync(ctx, s.db, resourceID, now)
	if err != nil {
		return false, fmt.Errorf("queue sync: %w", err)
	}
	return ok, nil
}

// execer is a *sql.DB or *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// queueSync inserts a sync job. The unique index on open jobs makes the
// insert do nothing when one is already open.
func queueSync(ctx context.Context, db execer, resourceID int64, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO jobs (kind, resource_id, run_after, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		JobSyncResource, resourceID, now.Unix(), now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClaimJob takes the queued job that has been due the longest, marks it
// running and counts the try. It is one statement, so two workers can
// never claim the same job. It returns ErrNotFound when no job is due.
func (s *Store) ClaimJob(ctx context.Context, now time.Time) (Job, error) {
	var j Job
	var resourceID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		UPDATE jobs SET status = 'running', locked_at = ?, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'queued' AND run_after <= ?
			ORDER BY run_after, id
			LIMIT 1)
		RETURNING id, kind, resource_id, attempts`, now.Unix(), now.Unix()).
		Scan(&j.ID, &j.Kind, &resourceID, &j.Attempts)
	if err != nil {
		return Job{}, fmt.Errorf("claim job: %w", noRows(err))
	}
	j.ResourceID = resourceID.Int64
	return j, nil
}

// FinishJob marks a running job done.
func (s *Store) FinishJob(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE jobs SET status = 'done', error = NULL WHERE id = ?", id); err != nil {
		return fmt.Errorf("finish job %d: %w", id, err)
	}
	return nil
}

// RetryJob puts a failed job back in the queue, to run again at
// runAfter. msg says why the try failed.
func (s *Store) RetryJob(ctx context.Context, id int64, runAfter time.Time, msg string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'queued', run_after = ?, locked_at = NULL, error = ?
		WHERE id = ?`, runAfter.Unix(), msg, id); err != nil {
		return fmt.Errorf("retry job %d: %w", id, err)
	}
	return nil
}

// FailJob marks a job failed for good, and saves msg on the job and on
// its resource, where builders see it. Both happen in one transaction.
func (s *Store) FailJob(ctx context.Context, j Job, msg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fail job %d: %w", j.ID, err)
	}
	defer tx.Rollback() // a no-op after Commit
	if _, err := tx.ExecContext(ctx,
		"UPDATE jobs SET status = 'failed', error = ? WHERE id = ?", msg, j.ID); err != nil {
		return fmt.Errorf("fail job %d: %w", j.ID, err)
	}
	if j.ResourceID != 0 {
		if _, err := tx.ExecContext(ctx,
			"UPDATE resources SET last_error = ? WHERE id = ?", msg, j.ResourceID); err != nil {
			return fmt.Errorf("fail job %d: %w", j.ID, err)
		}
	}
	return tx.Commit()
}

// ReleaseJob puts a running job back in the queue without counting the
// try, for a job stopped by a shutdown.
func (s *Store) ReleaseJob(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'queued', locked_at = NULL, attempts = max(attempts - 1, 0)
		WHERE id = ? AND status = 'running'`, id); err != nil {
		return fmt.Errorf("release job %d: %w", id, err)
	}
	return nil
}

// RequeueStuckJobs puts running jobs claimed at or before lockedBy back
// in the queue, and returns how many it moved. Their try still counts,
// so a job that stops the server every time still runs out of tries.
func (s *Store) RequeueStuckJobs(ctx context.Context, lockedBy time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'queued', locked_at = NULL
		WHERE status = 'running' AND locked_at <= ?`, lockedBy.Unix())
	if err != nil {
		return 0, fmt.Errorf("requeue stuck jobs: %w", err)
	}
	return res.RowsAffected()
}

// QueueDueSyncs queues a sync for every resource that is due at now, and
// returns how many it queued. A resource is due when its project's
// timer is on, its last good sync is older than the interval, and it
// has no open job. A resource that was tried within the interval waits
// too, so a failing resource is not tried again on every tick.
func (s *Store) QueueDueSyncs(ctx context.Context, now time.Time) (int64, error) {
	// ?1 is the job kind and ?2 is now.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (kind, resource_id, run_after, created_at)
		SELECT ?1, r.id, ?2, ?2
		FROM resources r JOIN projects p ON p.id = r.project_id
		WHERE p.sync_interval_hours IS NOT NULL
			AND ifnull(r.last_synced_at, 0) <= ?2 - p.sync_interval_hours * 3600
			AND NOT EXISTS (
				SELECT 1 FROM jobs j
				WHERE j.resource_id = r.id AND j.kind = ?1
					AND (j.status IN ('queued', 'running')
						OR j.created_at > ?2 - p.sync_interval_hours * 3600))
		ON CONFLICT DO NOTHING`,
		JobSyncResource, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("queue due syncs: %w", err)
	}
	return res.RowsAffected()
}

// DeleteFinishedJobs removes done and failed jobs created before before.
// The resources keep their last sync time and error, so old job rows
// only take space.
func (s *Store) DeleteFinishedJobs(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM jobs WHERE status IN ('done', 'failed') AND created_at < ?",
		before.Unix()); err != nil {
		return fmt.Errorf("delete finished jobs: %w", err)
	}
	return nil
}
