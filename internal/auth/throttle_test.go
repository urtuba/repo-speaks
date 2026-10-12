package auth

import (
	"testing"
	"time"
)

// fakeClock is a clock that tests move by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestThrottle(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	th := NewThrottle(clock.now)

	for i := range 5 {
		if wait := th.Attempt("samed"); wait != 0 {
			t.Fatalf("try %d: wait = %v, want 0", i+1, wait)
		}
	}

	steps := []struct {
		name     string
		advance  time.Duration
		wantWait time.Duration
	}{
		{"sixth try at once", 0, time.Second},
		{"after half a second", 500 * time.Millisecond, 500 * time.Millisecond},
		{"after the first wait", 500 * time.Millisecond, 0},
		{"seventh try at once", 0, 2 * time.Second},
		{"after the second wait", 2 * time.Second, 0},
		{"eighth try at once", 0, 4 * time.Second},
	}
	for _, s := range steps {
		clock.advance(s.advance)
		if got := th.Attempt("samed"); got != s.wantWait {
			t.Errorf("%s: wait = %v, want %v", s.name, got, s.wantWait)
		}
	}

	if wait := th.Attempt("other"); wait != 0 {
		t.Errorf("another username: wait = %v, want 0", wait)
	}

	th.Reset("samed")
	if wait := th.Attempt("samed"); wait != 0 {
		t.Errorf("after Reset: wait = %v, want 0", wait)
	}
}

func TestThrottleForgetsOldUsernames(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	th := NewThrottle(clock.now)
	for range 10 {
		th.Attempt("samed")
	}
	clock.advance(forgetAfter + time.Second)
	if wait := th.Attempt("other"); wait != 0 {
		t.Fatalf("wait = %v, want 0", wait)
	}
	if _, ok := th.tries["samed"]; ok {
		t.Error("username with no try for a day is still in memory")
	}
}

func TestWaitAfter(t *testing.T) {
	tests := []struct {
		count int
		want  time.Duration
	}{
		{4, 0},
		{5, time.Second},
		{6, 2 * time.Second},
		{14, 512 * time.Second},
		{15, maxWait},
		{1000, maxWait},
	}
	for _, tt := range tests {
		if got := waitAfter(tt.count); got != tt.want {
			t.Errorf("waitAfter(%d) = %v, want %v", tt.count, got, tt.want)
		}
	}
}
