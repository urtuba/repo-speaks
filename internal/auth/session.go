package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// SessionIdleTimeout is how long a session lives without a request.
// Every request moves the end forward. There is no absolute limit.
const SessionIdleTimeout = 24 * time.Hour

// sessionExtendAfter is how old the stored expiry must be before a request
// writes a new one. It saves a database write on every request.
const sessionExtendAfter = time.Minute

// tokenBytes is the number of random bytes in a token.
const tokenBytes = 32

// NewToken returns a new random token, and its hash for the database.
// Session cookies and invite links both use it. The database never
// stores the token itself, so a leaked database gives nobody a working
// session or link.
func NewToken() (token, hash string) {
	b := make([]byte, tokenBytes)
	rand.Read(b) // never returns an error
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken returns the hex SHA-256 hash of a token, the form the
// database stores. A plain hash is enough: the token is random
// and long, so it cannot be guessed from its hash.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SessionExpiry returns when a session used at now ends.
func SessionExpiry(now time.Time) time.Time {
	return now.Add(SessionIdleTimeout)
}

// ShouldExtendSession reports whether a session with the stored expiry,
// used at now, needs a new expiry written. It does once the stored expiry
// is more than a minute old.
func ShouldExtendSession(expiresAt, now time.Time) bool {
	setAt := expiresAt.Add(-SessionIdleTimeout)
	return now.Sub(setAt) > sessionExtendAfter
}
