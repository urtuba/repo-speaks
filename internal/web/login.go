package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/store"
)

// msgWrongLogin is the only answer to a failed login. It never says which
// part was wrong, so it does not reveal which usernames exist.
const msgWrongLogin = "Wrong username or password."

// formPage is the data for the setup and login pages.
type formPage struct {
	Username string
	Error    string
	Next     string // where to go after login; always a local path
}

// setupPage shows the form that creates the owner account. Once any
// account exists, the page is gone.
func (s *server) setupPage(w http.ResponseWriter, r *http.Request) {
	has, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if has {
		http.NotFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "setup.html", formPage{})
}

// setup creates the owner account and logs it in. The first visitor of a
// fresh instance becomes the owner: a known, accepted risk, since the
// person who starts the server is expected to open it first.
func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Check first, so a finished instance does no hashing for strangers.
	has, err := s.store.HasUsers(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if has {
		http.NotFound(w, r)
		return
	}
	username, password, msg, ok := newAccountForm(w, r)
	if !ok {
		return
	}
	if msg != "" {
		s.render(w, http.StatusBadRequest, "setup.html", formPage{Username: username, Error: msg})
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	owner, err := s.store.CreateOwner(ctx, username, hash, s.now())
	if errors.Is(err, store.ErrSetupDone) {
		// Someone else finished setup while this request was hashing.
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.startSession(w, r, owner.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// loginPage shows the login form. A fresh instance sends the visitor to
// setup instead, and a logged-in visitor goes home.
func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if currentSession(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	has, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !has {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login.html", formPage{Next: localNext(next)})
}

// login checks a username and password and starts a session.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	page := formPage{Username: username, Error: msgWrongLogin, Next: localNext(next)}

	// A name that breaks the username rules cannot exist. It skips the
	// throttle, so made-up names do not fill its memory, but it still
	// pays for the hashing like any other wrong login.
	if auth.ValidateUsername(username) != nil {
		auth.CheckPasswordNoAccount(password)
		s.render(w, http.StatusUnauthorized, "login.html", page)
		return
	}

	if wait := s.throttle.Attempt(username); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(seconds(wait)))
		page.Error = waitMessage(wait)
		s.render(w, http.StatusTooManyRequests, "login.html", page)
		return
	}

	user, err := s.store.UserByUsername(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		auth.CheckPasswordNoAccount(password)
		s.render(w, http.StatusUnauthorized, "login.html", page)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ok, err := auth.CheckPassword(user.PasswordHash, password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.render(w, http.StatusUnauthorized, "login.html", page)
		return
	}

	s.throttle.Reset(username)
	if err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout ends the current session and drops the cookie.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := currentSession(r); sess != nil {
		if err := s.store.DeleteSession(r.Context(), sess.idHash); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// newAccountForm reads and checks the fields for a new account: a
// username and the password twice. msg explains the first problem, or is
// empty. ok is false when the form could not be read; the answer is
// already sent then.
func newAccountForm(w http.ResponseWriter, r *http.Request) (username, password, msg string, ok bool) {
	if !parseForm(w, r) {
		return "", "", "", false
	}
	username = r.PostFormValue("username")
	password = r.PostFormValue("password")
	switch {
	case auth.ValidateUsername(username) != nil:
		msg = sentence(auth.ValidateUsername(username))
	case auth.ValidatePassword(password) != nil:
		msg = sentence(auth.ValidatePassword(password))
	case password != r.PostFormValue("password_again"):
		msg = "The two passwords are not the same."
	}
	return username, password, msg, true
}

// safeNext returns next when it is a path on this site, and "/"
// otherwise. Without the check, a link like /login?next=https://evil.example
// would send people to another site after they log in.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

// localNext returns next for the login form, or "" when it is the home
// page, so the form has no hidden field it does not need.
func localNext(next string) string {
	if next == "/" {
		return ""
	}
	return next
}

// waitMessage tells a visitor how long to wait before the next try.
func waitMessage(wait time.Duration) string {
	n, unit := seconds(wait), "second"
	if n >= 120 {
		n, unit = (n+59)/60, "minute"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("Too many wrong tries. Wait %d %s, then try again.", n, unit)
}

// seconds rounds d up to whole seconds.
func seconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

// sentence turns an error from auth into a sentence for the page.
func sentence(err error) string {
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}
