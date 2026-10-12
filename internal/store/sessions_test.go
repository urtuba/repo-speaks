package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, err := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	expires := testNow.Add(time.Hour)
	if err := s.CreateSession(ctx, "h1", u.ID, expires); err != nil {
		t.Fatal(err)
	}

	got, gotExpires, err := s.SessionUser(ctx, "h1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID || !gotExpires.Equal(expires) {
		t.Errorf("SessionUser() = user %d, expires %v, want user %d, expires %v", got.ID, gotExpires, u.ID, expires)
	}

	tests := []struct {
		name string
		hash string
		at   time.Time
	}{
		{"unknown hash", "nope", testNow},
		{"at the expiry", "h1", expires},
		{"after the expiry", "h1", expires.Add(time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.SessionUser(ctx, tt.hash, tt.at); !errors.Is(err, ErrNotFound) {
				t.Errorf("SessionUser() error = %v, want ErrNotFound", err)
			}
		})
	}

	later := expires.Add(time.Hour)
	if err := s.ExtendSession(ctx, "h1", later); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "h1", expires.Add(time.Second)); err != nil {
		t.Errorf("after ExtendSession: %v", err)
	}

	if err := s.DeleteSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "h1", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("after DeleteSession: error = %v, want ErrNotFound", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, err := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "old", u.ID, testNow.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "live", u.ID, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteExpiredSessions(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	var hashes []string
	rows, err := s.db.QueryContext(ctx, "SELECT id_hash FROM sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	if len(hashes) != 1 || hashes[0] != "live" {
		t.Errorf("sessions left = %v, want [live]", hashes)
	}
}
