package auth

import (
	"sync"
	"time"
)

// Throttle settings for wrong passwords.
const (
	freeTries       = 5                // tries before any wait
	firstWait       = time.Second      // wait after the first try over freeTries
	maxWait         = 15 * time.Minute // the wait never grows past this
	forgetAfter     = 24 * time.Hour   // an idle username is forgotten after this
	cleanupInterval = time.Minute      // how often old usernames are removed
)

// Throttle slows down password guessing for each username. After five
// wrong tries, each new try must wait longer: 1s, 2s, 4s and so on, up
// to 15 minutes. The account is never locked, so an attacker cannot lock
// out its owner.
//
// It lives in memory: there is one process, and a restart resets it.
// It is safe for use by many goroutines.
type Throttle struct {
	now func() time.Time

	mu          sync.Mutex
	tries       map[string]*tries
	lastCleanup time.Time
}

// tries records the failed tries for one username.
type tries struct {
	count int
	last  time.Time
}

// NewThrottle returns an empty throttle that reads the time from now.
// Tests pass a fake clock.
func NewThrottle(now func() time.Time) *Throttle {
	return &Throttle{now: now, tries: map[string]*tries{}}
}

// Attempt asks to try a password for username. If the username must
// still wait, Attempt returns how long, and the caller must refuse the
// try without checking the password. Otherwise it returns 0 and counts
// the try as wrong until Reset says it was right.
//
// Counting before the check means many tries sent at the same moment
// cannot get past the limit.
func (t *Throttle) Attempt(username string) (wait time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.cleanup(now)
	e := t.tries[username]
	if e == nil {
		e = &tries{}
		t.tries[username] = e
	}
	if e.count >= freeTries {
		if wait := e.last.Add(waitAfter(e.count)).Sub(now); wait > 0 {
			return wait
		}
	}
	e.count++
	e.last = now
	return 0
}

// Reset forgets the tries for username, after a right password.
func (t *Throttle) Reset(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tries, username)
}

// waitAfter returns how long the next try must wait after count wrong
// tries. It doubles with each try over freeTries.
func waitAfter(count int) time.Duration {
	if count < freeTries {
		return 0
	}
	wait := firstWait
	for range count - freeTries {
		wait *= 2
		if wait >= maxWait {
			return maxWait
		}
	}
	return wait
}

// cleanup removes usernames with no try for a day, at most once a minute.
// Without it, guesses for many usernames would fill memory.
// The caller holds t.mu.
func (t *Throttle) cleanup(now time.Time) {
	if now.Sub(t.lastCleanup) < cleanupInterval {
		return
	}
	t.lastCleanup = now
	for name, e := range t.tries {
		if now.Sub(e.last) > forgetAfter {
			delete(t.tries, name)
		}
	}
}
