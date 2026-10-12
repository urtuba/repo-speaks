package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/store"
)

// sessionCookieName names the cookie that holds the session token.
const sessionCookieName = "session"

// session is the logged-in account of a request.
type session struct {
	user   store.User
	idHash string // hash of the cookie's token, the key in the database
}

// sessionKey is the context key for *session.
type sessionKey struct{}

// currentSession returns the request's session, or nil when nobody is
// logged in.
func currentSession(r *http.Request) *session {
	sess, _ := r.Context().Value(sessionKey{}).(*session)
	return sess
}

// loadSession puts the session from the cookie into the request context.
// A missing, unknown or expired session means nobody is logged in.
//
// It also moves the session's expiry forward. To save a write on every
// request, it writes only when the stored expiry is over a minute old.
func (s *server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		now := s.now()
		idHash := auth.HashSessionToken(c.Value)
		user, expiresAt, err := s.store.SessionUser(r.Context(), idHash, now)
		if errors.Is(err, store.ErrNotFound) {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if auth.ShouldExtendSession(expiresAt, now) {
			if err := s.store.ExtendSession(r.Context(), idHash, auth.SessionExpiry(now)); err != nil {
				// The session is still valid; the next request tries again.
				s.logger.Error("extend session", "err", err)
			} else {
				setSessionCookie(w, c.Value)
			}
		}
		ctx := context.WithValue(r.Context(), sessionKey{}, &session{user: user, idHash: idHash})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireLogin sends visitors who are not logged in to the login page.
func requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentSession(r) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// startSession logs the account in with a new session and sets the cookie.
// A login always gets a new token, and the request's old session ends, so
// a token planted before login is worthless after it.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	ctx := r.Context()
	now := s.now()
	if old := currentSession(r); old != nil {
		if err := s.store.DeleteSession(ctx, old.idHash); err != nil {
			return err
		}
	}
	// Logins are rare, so this is a good moment to drop old rows.
	if err := s.store.DeleteExpiredSessions(ctx, now); err != nil {
		s.logger.Error("delete expired sessions", "err", err)
	}
	token, idHash := auth.NewSessionToken()
	if err := s.store.CreateSession(ctx, idHash, userID, auth.SessionExpiry(now)); err != nil {
		return err
	}
	setSessionCookie(w, token)
	return nil
}

// setSessionCookie sets the session cookie. HttpOnly keeps it from
// scripts. Secure keeps it off plain HTTP, except on localhost, where
// browsers allow it for development. SameSite=Lax keeps it off most
// cross-site requests.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.SessionIdleTimeout / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie tells the browser to drop the session cookie.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
