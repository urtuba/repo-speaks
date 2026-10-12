package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		want     error
	}{
		{"empty", "", ErrPasswordTooShort},
		{"11 characters", strings.Repeat("a", 11), ErrPasswordTooShort},
		{"12 characters", strings.Repeat("a", 12), nil},
		{"12 runes, more bytes", strings.Repeat("ğ", 12), nil},
		{"1024 runes", strings.Repeat("ğ", 1024), nil},
		{"1025 characters", strings.Repeat("a", 1025), ErrPasswordTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidatePassword(tt.password); got != tt.want {
				t.Errorf("ValidatePassword() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHashPassword(t *testing.T) {
	h1, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if want := "$argon2id$v=19$m=19456,t=2,p=1$"; !strings.HasPrefix(h1, want) {
		t.Errorf("hash = %q, want prefix %q", h1, want)
	}
	h2, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("two hashes of one password are equal: the salt is not random")
	}

	tests := []struct {
		password string
		want     bool
	}{
		{"correct horse battery", true},
		{"correct horse batterY", false},
		{"", false},
	}
	for _, tt := range tests {
		got, err := CheckPassword(h1, tt.password)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("CheckPassword(%q) = %v, want %v", tt.password, got, tt.want)
		}
	}
}

func TestCheckPasswordReadsStoredParams(t *testing.T) {
	// A hash made with other settings must still verify after the
	// defaults change. This one uses m=8, t=1, p=1.
	old := defaultParams
	defaultParams = argonParams{memory: 8, time: 1, threads: 1}
	h, err := HashPassword("an older password")
	defaultParams = old
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h, "$m=8,t=1,p=1$") {
		t.Fatalf("hash = %q, want m=8,t=1,p=1", h)
	}
	ok, err := CheckPassword(h, "an older password")
	if err != nil || !ok {
		t.Errorf("CheckPassword() = %v, %v, want true, nil", ok, err)
	}
}

func TestCheckPasswordBadHash(t *testing.T) {
	tests := []string{
		"",
		"plain text",
		"$argon2i$v=19$m=8,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=16$m=8,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=8,t=1,p=1$!!!$a2V5",
		"$argon2id$v=19$m=8,t=1,p=1$c2FsdA$",
	}
	for _, encoded := range tests {
		if _, err := CheckPassword(encoded, "password"); !errors.Is(err, ErrBadHash) {
			t.Errorf("CheckPassword(%q) error = %v, want ErrBadHash", encoded, err)
		}
	}
}
