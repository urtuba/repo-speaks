package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// accounts lists every account on the instance. Only the owner sees it.
func (s *server) accounts(w http.ResponseWriter, r *http.Request) {
	user := currentSession(r).user
	if !user.IsOwner {
		http.Error(w, "Only the instance owner can see all accounts.", http.StatusForbidden)
		return
	}
	list, err := s.store.Accounts(r.Context(), s.now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	memberships, err := s.store.AccountProjects(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]accountRow, len(list))
	for i, a := range list {
		rows[i] = accountRow{Account: a}
		for _, m := range memberships[a.ID] {
			set, err := permissions.ParseSet(m.Permissions)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			rows[i].Projects = append(rows[i].Projects, projectRole{ID: m.ProjectID, Name: m.Name, Role: roleName(set)})
		}
	}
	s.render(w, http.StatusOK, "accounts.html", struct {
		User     *store.User
		Accounts []accountRow
	}{&user, rows})
}

// accountRow is an account on the Accounts page, with its projects.
type accountRow struct {
	store.Account
	Projects []projectRole
}

// projectRole is one project of an account, with the account's role in it.
type projectRole struct {
	ID   int64
	Name string
	Role string
}

// createReset makes a password reset link for an account and shows it
// once. Only the owner may, and not for their own account.
func (s *server) createReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := currentSession(r).user
	if !owner.IsOwner {
		http.Error(w, "Only the instance owner can make reset links.", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := s.store.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if target.IsOwner {
		http.Error(w, "Change your own password on your account page.", http.StatusBadRequest)
		return
	}

	now := s.now()
	token, hash := auth.NewToken()
	expires := auth.ResetExpiry(now)
	if err := s.store.CreatePasswordReset(ctx, target.ID, hash, owner.ID, now, expires); err != nil {
		s.serverError(w, r, err)
		return
	}
	// The page holds a working link, so no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "reset_created.html", struct {
		User      *store.User
		Target    store.User
		Link      string
		ExpiresAt time.Time
	}{&owner, target, linkBase(r) + resetPath(token), expires.UTC()})
}

// resetPage is the data for reset.html.
type resetPage struct {
	Dead     bool
	Username string
	Path     string
	Error    string
}

// showReset shows the form for a new password, or says the link is dead.
func (s *server) showReset(w http.ResponseWriter, r *http.Request) {
	reset, ok, err := s.openReset(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		s.render(w, http.StatusNotFound, "reset.html", resetPage{Dead: true})
		return
	}
	s.render(w, http.StatusOK, "reset.html", resetPage{Username: reset.Username, Path: r.URL.Path})
}

// useReset sets the new password through a reset link. All sessions of
// the account end, and the person is logged in with a new one.
func (s *server) useReset(w http.ResponseWriter, r *http.Request) {
	reset, ok, err := s.openReset(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		s.render(w, http.StatusNotFound, "reset.html", resetPage{Dead: true})
		return
	}
	if !parseForm(w, r) {
		return
	}
	password := r.PostFormValue("password")
	if msg := newPasswordMessage(password, r.PostFormValue("password_again")); msg != "" {
		s.render(w, http.StatusBadRequest, "reset.html", resetPage{Username: reset.Username, Path: r.URL.Path, Error: msg})
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	userID, err := s.store.UsePasswordReset(r.Context(), reset.ID, hash, s.now())
	if errors.Is(err, store.ErrResetGone) {
		s.render(w, http.StatusNotFound, "reset.html", resetPage{Dead: true})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.throttle.Reset(reset.Username)
	if err := s.startSession(w, r, userID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// openReset finds the reset link in the URL and reports whether it
// still works.
func (s *server) openReset(r *http.Request) (store.Reset, bool, error) {
	reset, err := s.store.PasswordResetByHash(r.Context(), auth.HashToken(r.PathValue("token")))
	if errors.Is(err, store.ErrNotFound) {
		return store.Reset{}, false, nil
	}
	if err != nil {
		return store.Reset{}, false, err
	}
	return reset, reset.Open(s.now()), nil
}

// accountPage is the data for account.html.
type accountPage struct {
	User  *store.User
	Saved bool
	Error string
}

// account shows the logged-in person's account page.
func (s *server) account(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "account.html", accountPage{
		User:  &currentSession(r).user,
		Saved: r.URL.Query().Get("saved") == "1",
	})
}

// changePassword changes the logged-in person's password. The current
// password goes through the login throttle, so a stolen session cannot
// guess it quickly. All other sessions end.
func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	user := currentSession(r).user
	page := accountPage{User: &user}
	if !parseForm(w, r) {
		return
	}
	if wait := s.throttle.Attempt(user.Username); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(seconds(wait)))
		page.Error = waitMessage(wait)
		s.render(w, http.StatusTooManyRequests, "account.html", page)
		return
	}
	ok, err := auth.CheckPassword(user.PasswordHash, r.PostFormValue("current_password"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		page.Error = "Your current password is wrong."
		s.render(w, http.StatusBadRequest, "account.html", page)
		return
	}
	s.throttle.Reset(user.Username)

	password := r.PostFormValue("password")
	if msg := newPasswordMessage(password, r.PostFormValue("password_again")); msg != "" {
		page.Error = msg
		s.render(w, http.StatusBadRequest, "account.html", page)
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.SetPassword(r.Context(), user.ID, hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	// SetPassword ended every session, this one too. Start a new one.
	if err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?saved=1", http.StatusSeeOther)
}

// resetPath is the path of a password reset link.
func resetPath(token string) string {
	return "/reset/" + token
}
