package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
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
	if !parseForm(w, r) {
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	page := formPage{Username: username}

	if err := auth.ValidateUsername(username); err != nil {
		page.Error = sentence(err)
	} else if err := auth.ValidatePassword(password); err != nil {
		page.Error = sentence(err)
	} else if password != r.PostFormValue("password_again") {
		page.Error = "The two passwords are not the same."
	}
	if page.Error != "" {
		s.render(w, http.StatusBadRequest, "setup.html", page)
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
	if currentSession(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
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
	s.render(w, http.StatusOK, "login.html", formPage{})
}

// login checks a username and password and starts a session.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	page := formPage{Username: username, Error: msgWrongLogin}

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
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
