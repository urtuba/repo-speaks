// Package auth handles accounts: usernames, passwords, sessions and the
// throttle for wrong passwords.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password length limits, counted in characters (runes).
// There are no rules for digits or symbols: length matters more.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 1024
)

// Errors returned by ValidatePassword.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
)

// ErrBadHash is returned when a stored hash cannot be read.
var ErrBadHash = errors.New("bad password hash")

// ValidatePassword checks a new password against the length limits.
func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if n > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}

// argonParams are the Argon2id settings stored with every hash.
type argonParams struct {
	memory  uint32 // KiB
	time    uint32 // passes over the memory
	threads uint8
}

// defaultParams follow the OWASP password storage advice for Argon2id.
// Every hash stores its own settings, so these can change later and old
// hashes still verify.
var defaultParams = argonParams{memory: 19456, time: 2, threads: 1}

const (
	saltLength = 16
	keyLength  = 32
)

// HashPassword returns an Argon2id hash of password with a new random salt.
// The result is a string in the PHC format:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	p := defaultParams
	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// CheckPassword reports whether password matches the encoded hash.
// It compares in constant time.
func CheckPassword(encoded, password string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is a hash of a random password, made on first use.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword(rand.Text())
	if err != nil {
		panic(err) // crypto/rand does not fail on supported systems
	}
	return h
})

// CheckPasswordNoAccount does the same work as CheckPassword, against a
// hash no password matches. Call it when the username does not exist, so
// the answer takes as long as for a wrong password and timing does not
// reveal which usernames exist.
func CheckPasswordNoAccount(password string) {
	CheckPassword(dummyHash(), password)
}

// decodeHash reads the settings, salt and key from a PHC string.
func decodeHash(encoded string) (p argonParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, ErrBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, ErrBadHash
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return p, nil, nil, ErrBadHash
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return p, nil, nil, ErrBadHash
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, ErrBadHash
	}
	return p, salt, key, nil
}
