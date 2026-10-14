package llm

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// DefaultBaseURL is OpenRouter's API, where one key reaches many models.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// maxBaseURLLen caps a base URL.
const maxBaseURLLen = 500

// ParseBaseURL checks a host's base URL, as an admin typed it, and
// returns it with no trailing slash. It must be an absolute http or https
// URL with a host, and may have a path. User info, a query and a fragment
// are refused: a key must go only in the Authorization header, and the
// app adds paths such as /models to the end. The error message is safe
// to show.
//
// Whether the host is a private address is not checked here: a name can
// resolve to another address later. The client checks each address it
// connects to.
func ParseBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("enter the base URL, such as " + DefaultBaseURL)
	}
	if len(s) > maxBaseURLLen {
		return "", errors.New("the base URL is too long")
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", errors.New("the base URL may not contain spaces")
	}
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return "", errors.New("the base URL must be a full address, such as " + DefaultBaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("the base URL must start with https:// or http://")
	}
	if u.Hostname() == "" {
		return "", errors.New("the base URL has no host name")
	}
	if u.User != nil {
		return "", errors.New("the base URL may not hold a user name or password")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(s, "?#") {
		return "", errors.New("the base URL may not have a query (?) or a fragment (#)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	return u.String(), nil
}
