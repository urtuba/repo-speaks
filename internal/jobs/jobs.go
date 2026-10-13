// Package jobs runs Repo Speaks' background work. Workers claim job rows
// from the database and hand each job to the package that knows how to
// do it. A timer queues the syncs that are due and cleans up.
//
// The web handlers only add job rows. The database is the only link
// between them and this loop, so a restart loses no queued work.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

const (
	// workers is how many jobs run at the same time. One keeps the load
	// on the server and on GitHub low; raise it when syncs pile up.
	workers = 1

	// maxAttempts is how many times a job is tried before it fails.
	maxAttempts = 3

	// syncTimeout is how long one sync may take.
	syncTimeout = 10 * time.Minute

	// pollInterval is how long a worker waits when no job is due.
	pollInterval = 2 * time.Second

	// timerInterval is how often the timer queues due syncs and cleans up.
	timerInterval = 5 * time.Minute

	// keepFinished is how long done and failed jobs stay in the database.
	keepFinished = 7 * 24 * time.Hour

	// writeTimeout limits the database write that records how a job
	// ended, which still runs during a shutdown.
	writeTimeout = 5 * time.Second
)

// Syncer syncs one resource and removes the clones of deleted ones.
// *sync.Syncer is the real one; tests use a fake.
type Syncer interface {
	Sync(ctx context.Context, resourceID int64) error
	RemoveDeleted(ctx context.Context) error
}

// Loop runs the workers and the timer.
type Loop struct {
	store       *store.Store
	syncer      Syncer
	logger      *slog.Logger
	now         func() time.Time
	syncTimeout time.Duration
}

// New returns the loop. Run starts it.
func New(st *store.Store, syncer Syncer, logger *slog.Logger) *Loop {
	return &Loop{
		store:       st,
		syncer:      syncer,
		logger:      logger,
		now:         time.Now,
		syncTimeout: syncTimeout,
	}
}

// Run runs the workers and the timer until ctx ends. A job still running
// then is stopped and goes back to the queue. Run returns once every
// worker has stopped.
//
// First it puts back in the queue the jobs an earlier run left running.
// This process runs every job and has not started any yet, so all of
// them were cut off by a crash or a restart.
func (l *Loop) Run(ctx context.Context) error {
	n, err := l.store.RequeueStuckJobs(ctx, l.now())
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if n > 0 {
		l.logger.Info("requeued jobs left running", "jobs", n)
	}

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { l.work(ctx) })
	}
	wg.Go(func() { l.timer(ctx) })
	wg.Wait()
	return nil
}

// work claims and runs jobs until ctx ends. When no job is due, it waits
// before it looks again.
func (l *Loop) work(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := l.runNext(ctx)
		if err != nil && ctx.Err() == nil {
			l.logger.Error("claim job", "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}

// runNext claims the job that has been due the longest and runs it. It
// reports whether there was one.
func (l *Loop) runNext(ctx context.Context) (bool, error) {
	job, err := l.store.ClaimJob(ctx, l.now())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	l.run(ctx, job)
	return true, nil
}

// run runs a claimed job and records how it ended: done, back in the
// queue for a later try, or failed.
func (l *Loop) run(ctx context.Context, job store.Job) {
	log := l.logger.With("job", job.ID, "kind", job.Kind, "resource", job.ResourceID, "attempt", job.Attempts)
	var err error
	if job.Attempts > maxAttempts {
		// The tries ran out while the server was down: the last one was
		// cut off and requeued at start.
		err = jobError{msg: "the server stopped during the last try", permanent: true}
	} else {
		err = l.do(ctx, job)
	}

	// The ending is written even when ctx has ended for a shutdown.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	switch {
	case err == nil:
		if err := l.store.FinishJob(wctx, job.ID); err != nil {
			log.Error("save job end", "err", err)
		}
		log.Info("job done")
	case ctx.Err() != nil:
		// A shutdown, not the job's fault: the try does not count.
		if err := l.store.ReleaseJob(wctx, job.ID); err != nil {
			log.Error("save job end", "err", err)
		}
		log.Info("job stopped by shutdown")
	case retryable(err) && job.Attempts < maxAttempts:
		runAfter := l.now().Add(retryWait(job.Attempts))
		if err := l.store.RetryJob(wctx, job.ID, runAfter, err.Error()); err != nil {
			log.Error("save job end", "err", err)
		}
		log.Warn("job failed, will try again", "err", err, "run_after", runAfter)
	default:
		if err := l.store.FailJob(wctx, job, err.Error()); err != nil {
			log.Error("save job end", "err", err)
		}
		log.Error("job failed", "err", err)
	}
}

// do runs the job with its timeout. A panic in the job fails the job and
// leaves the server running.
func (l *Loop) do(ctx context.Context, job store.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			l.logger.Error("job panicked", "job", job.ID, "panic", p, "stack", string(debug.Stack()))
			err = jobError{msg: "the job stopped with an internal error; the server log has the details", permanent: true}
		}
	}()
	switch job.Kind {
	case store.JobSyncResource:
		ctx, cancel := context.WithTimeout(ctx, l.syncTimeout)
		defer cancel()
		err := l.syncer.Sync(ctx, job.ResourceID)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// A sync this slow will likely be as slow next time.
			return jobError{msg: fmt.Sprintf("the sync took longer than %v and was stopped", l.syncTimeout), permanent: true}
		}
		return err
	default:
		return jobError{msg: fmt.Sprintf("unknown job kind %q", job.Kind), permanent: true}
	}
}

// retryWait is how long a job waits after its nth failed try: 1 minute,
// then 4.
func retryWait(attempt int) time.Duration {
	return time.Duration(attempt*attempt) * time.Minute
}

// permanent is an error that trying again cannot fix. sync.Error is one.
type permanent interface {
	error
	Permanent() bool
}

// retryable reports whether trying again might help.
func retryable(err error) bool {
	p, ok := errors.AsType[permanent](err)
	return !ok || !p.Permanent()
}

// jobError is a failure found by the loop itself.
type jobError struct {
	msg       string
	permanent bool
}

func (e jobError) Error() string   { return e.msg }
func (e jobError) Permanent() bool { return e.permanent }

// timer does the regular work at start and then every timerInterval,
// until ctx ends.
func (l *Loop) timer(ctx context.Context) {
	t := time.NewTicker(timerInterval)
	defer t.Stop()
	for {
		l.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick queues the syncs that are due, deletes old finished jobs and
// removes the clones of deleted resources.
func (l *Loop) tick(ctx context.Context) {
	now := l.now()
	n, err := l.store.QueueDueSyncs(ctx, now)
	l.logError(ctx, "queue due syncs", err)
	if n > 0 {
		l.logger.Info("queued due syncs", "resources", n)
	}
	l.logError(ctx, "delete finished jobs", l.store.DeleteFinishedJobs(ctx, now.Add(-keepFinished)))
	l.logError(ctx, "remove deleted clones", l.syncer.RemoveDeleted(ctx))
}

// logError logs err, unless it is nil or ctx has ended: a shutdown
// cancels work in the middle, and that is not worth an error line.
func (l *Loop) logError(ctx context.Context, msg string, err error) {
	if err != nil && ctx.Err() == nil {
		l.logger.Error(msg, "err", err)
	}
}
