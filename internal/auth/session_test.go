package auth

import (
	"testing"
	"time"
)

func TestNewSessionToken(t *testing.T) {
	token, hash := NewSessionToken()
	if len(token) != 43 { // 32 bytes in unpadded base64
		t.Errorf("token length = %d, want 43", len(token))
	}
	if hash != HashSessionToken(token) {
		t.Error("returned hash does not match HashSessionToken(token)")
	}
	if hash == token {
		t.Error("hash equals the token")
	}
	if other, _ := NewSessionToken(); other == token {
		t.Error("two tokens are equal")
	}
}

func TestShouldExtendSession(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		setAt time.Time
		want  bool
	}{
		{"just set", now, false},
		{"set a minute ago", now.Add(-time.Minute), false},
		{"set over a minute ago", now.Add(-time.Minute - time.Second), true},
		{"set an hour ago", now.Add(-time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldExtendSession(SessionExpiry(tt.setAt), now); got != tt.want {
				t.Errorf("ShouldExtendSession() = %v, want %v", got, tt.want)
			}
		})
	}
}
