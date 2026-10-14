package agent

import (
	"fmt"
	"sync"
	"time"
)

// rateWindow is the sliding window of the rate limit.
const rateWindow = time.Hour

// RateLimitError is returned when a member has asked an agent as many
// questions as its rate limit allows in the last hour.
type RateLimitError struct {
	Limit   int       // questions per hour
	RetryAt time.Time // when the member can ask again
}

func (e *RateLimitError) Error() string {
	n := fmt.Sprintf("%d questions", e.Limit)
	if e.Limit == 1 {
		n = "1 question"
	}
	return fmt.Sprintf("You can ask this agent %s per hour. You can ask again at %s.",
		n, e.RetryAt.UTC().Format("15:04 UTC"))
}

// limitKey is one member's count with one agent.
type limitKey struct {
	agentID int64
	userID  int64
}

// limiter counts questions in memory. A restart resets every count; the
// app runs as one process, so one count is enough.
type limiter struct {
	mu    sync.Mutex
	asked map[limitKey][]time.Time // the times of the questions in the window, oldest first
}

func newLimiter() *limiter {
	return &limiter{asked: map[limitKey][]time.Time{}}
}

// take counts a question at now, unless the key already has limit
// questions in the window. Then it returns false and when the oldest of
// them leaves the window.
func (l *limiter) take(k limitKey, limit int, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	times := l.recent(k, now)
	if len(times) >= limit {
		return times[0].Add(rateWindow), false
	}
	l.asked[k] = append(times, now)
	// Members who stopped asking leave empty or old lists behind. Clear
	// them out now and then, so the map does not grow forever.
	if len(l.asked) > 10_000 {
		for key := range l.asked {
			l.recent(key, now)
		}
	}
	return time.Time{}, true
}

// giveBack takes back the question take counted at now, when it could
// not be asked after all.
func (l *limiter) giveBack(k limitKey, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	times := l.asked[k]
	for i := len(times) - 1; i >= 0; i-- {
		if times[i].Equal(now) {
			l.asked[k] = append(times[:i], times[i+1:]...)
			return
		}
	}
}

// recent drops the key's questions older than the window, and returns
// the rest. The caller holds mu.
func (l *limiter) recent(k limitKey, now time.Time) []time.Time {
	times := l.asked[k]
	i := 0
	for i < len(times) && !times[i].After(now.Add(-rateWindow)) {
		i++
	}
	times = times[i:]
	if len(times) == 0 {
		delete(l.asked, k)
		return nil
	}
	l.asked[k] = times
	return times
}
