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

// sessionTokenBytes is the number of random bytes in a session token.
const sessionTokenBytes = 32

// NewSessionToken returns a new random session token for the cookie, and
// its hash for the database. The database never stores the token itself,
// so a leaked database does not give anyone a working session.
func NewSessionToken() (token, hash string) {
	b := make([]byte, sessionTokenBytes)
	rand.Read(b) // never returns an error
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashSessionToken(token)
}

// HashSessionToken returns the hex SHA-256 hash of a session token, the
// form the database stores. A plain hash is enough: the token is random
// and long, so it cannot be guessed from its hash.
func HashSessionToken(token string) string {
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
