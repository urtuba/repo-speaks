package auth

import "errors"

// Username length limits, in characters.
const (
	MinUsernameLength = 3
	MaxUsernameLength = 32
)

// ErrBadUsername is returned for a username that breaks the rules.
var ErrBadUsername = errors.New("username must be 3 to 32 characters: lowercase letters, digits, - or _")

// ValidateUsername checks the username rules: 3 to 32 characters, each a
// lowercase letter (a to z), a digit, - or _. Only one spelling is
// allowed, so two accounts cannot differ only in case.
func ValidateUsername(username string) error {
	if len(username) < MinUsernameLength || len(username) > MaxUsernameLength {
		return ErrBadUsername
	}
	for _, c := range username {
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return ErrBadUsername
		}
	}
	return nil
}
