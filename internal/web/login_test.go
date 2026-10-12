package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/store"
)

func TestSetupRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		form     url.Values
		wantBody string
	}{
		{"bad username", url.Values{"username": {"Samed"}, "password": {testPassword}, "password_again": {testPassword}}, "Username must be"},
		{"short password", url.Values{"username": {"samed"}, "password": {"short"}, "password_again": {"short"}}, "at least 12 characters"},
		{"passwords differ", url.Values{"username": {"samed"}, "password": {testPassword}, "password_again": {testPassword + "!"}}, "not the same"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t)
			rec := app.post("/setup", tt.form, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body does not contain %q:\n%s", tt.wantBody, rec.Body)
			}
			if has, _ := app.store.HasUsers(context.Background()); has {
				t.Error("an account was created")
			}
		})
	}
}

func TestSetupOnlyOnce(t *testing.T) {
	app := newTestApp(t)

	if rec := app.get("/setup", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /setup on a fresh instance: status = %d, want 200", rec.Code)
	}
	// A fresh instance sends visitors from the home page to login, and
	// from login to setup.
	if rec := app.get("/", nil); rec.Header().Get("Location") != "/login" {
		t.Errorf("home page of a fresh instance: location = %q, want /login", rec.Header().Get("Location"))
	}
	if rec := app.get("/login", nil); rec.Header().Get("Location") != "/setup" {
		t.Errorf("login page of a fresh instance: location = %q, want /setup", rec.Header().Get("Location"))
	}

	form := url.Values{"username": {"samed"}, "password": {testPassword}, "password_again": {testPassword}}
	rec := app.post("/setup", form, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("POST /setup: status = %d, location = %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("POST /setup did not log the owner in")
	}
	if body := app.get("/", c).Body.String(); !strings.Contains(body, "samed (owner)") {
		t.Errorf("home page after setup:\n%s", body)
	}
	owner, err := app.store.UserByUsername(context.Background(), "samed")
	if err != nil || !owner.IsOwner {
		t.Fatalf("owner = %+v, %v, want an owner", owner, err)
	}

	// Once an account exists, the setup page is gone.
	if rec := app.get("/setup", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /setup after setup: status = %d, want 404", rec.Code)
	}
	form.Set("username", "intruder")
	if rec := app.post("/setup", form, nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST /setup after setup: status = %d, want 404", rec.Code)
	}
	if _, err := app.store.UserByUsername(context.Background(), "intruder"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second setup created an account: %v", err)
	}
	if rec := app.get("/", nil); !strings.Contains(rec.Body.String(), `href="/login"`) {
		t.Error("home page after setup does not link to /login")
	}
}

func TestLoginSetsSessionCookie(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)

	c := app.login("samed")
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie = %+v, want HttpOnly, Secure, SameSite=Lax, Path=/", c)
	}
	if c.MaxAge != int(auth.SessionIdleTimeout/time.Second) {
		t.Errorf("cookie MaxAge = %d, want %d", c.MaxAge, int(auth.SessionIdleTimeout/time.Second))
	}
	if len(c.Value) != 43 {
		t.Errorf("token length = %d, want 43 (32 bytes in base64url)", len(c.Value))
	}

	// The database keeps the hash, never the token.
	ctx := context.Background()
	if _, _, err := app.store.SessionUser(ctx, c.Value, app.clock.now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session found by the raw token: %v", err)
	}
	if _, _, err := app.store.SessionUser(ctx, auth.HashToken(c.Value), app.clock.now()); err != nil {
		t.Errorf("session not found by the token's hash: %v", err)
	}

	// A new login gets a new token and ends the old session.
	rec := app.post("/login", url.Values{"username": {"samed"}, "password": {testPassword}}, c)
	again := sessionCookie(rec)
	if again == nil || again.Value == c.Value {
		t.Fatalf("second login: cookie = %v, want a new token", again)
	}
	if body := app.get("/", c).Body.String(); strings.Contains(body, "Logged in") {
		t.Error("the old session still works after a new login")
	}
}

func TestLoginFailuresLookTheSame(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)

	tests := []struct {
		name     string
		username string
		password string
	}{
		{"wrong password", "samed", "not the password"},
		{"unknown user", "nobody", testPassword},
		{"username that breaks the rules", "No Body", testPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.post("/login", url.Values{"username": {tt.username}, "password": {tt.password}}, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if !strings.Contains(rec.Body.String(), msgWrongLogin) {
				t.Errorf("body does not contain %q", msgWrongLogin)
			}
			if sessionCookie(rec) != nil {
				t.Error("a failed login set a session cookie")
			}
		})
	}
}

func TestLoginThrottle(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	wrong := url.Values{"username": {"samed"}, "password": {"not the password"}}
	right := url.Values{"username": {"samed"}, "password": {testPassword}}

	for i := range 5 {
		if rec := app.post("/login", wrong, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong try %d: status = %d, want 401", i+1, rec.Code)
		}
	}

	// The sixth try must wait, even with the right password.
	rec := app.post("/login", right, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("try during the wait: status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Wait 1 second,") {
		t.Errorf("body does not ask to wait 1 second:\n%s", rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if sessionCookie(rec) != nil {
		t.Error("a refused try set a session cookie")
	}

	// After the wait, the right password works and resets the count.
	app.clock.advance(time.Second)
	if rec := app.post("/login", right, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("right password after the wait: status = %d, want 303", rec.Code)
	}
	for i := range 5 {
		if rec := app.post("/login", wrong, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong try %d after a reset: status = %d, want 401", i+1, rec.Code)
		}
	}
}

func TestLogout(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	c := app.login("samed")

	rec := app.post("/logout", nil, c)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("POST /logout: status = %d, location = %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if cleared := sessionCookie(rec); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("logout cookie = %v, want one that deletes the cookie", cleared)
	}

	// The old token no longer works, even if the browser kept it.
	if rec := app.get("/projects/1", c); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("GET /projects/1 after logout: status = %d, want 303 to /login", rec.Code)
	}
	if _, _, err := app.store.SessionUser(context.Background(), auth.HashToken(c.Value), app.clock.now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session row after logout: %v, want ErrNotFound", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	c := app.login("samed")

	loggedIn := func() bool {
		return app.get("/", c).Code == http.StatusOK // logged out: 303 to /login
	}

	// Each request moves the end forward, so regular use keeps the
	// session alive well past 24 hours.
	app.clock.advance(23 * time.Hour)
	if !loggedIn() {
		t.Fatal("logged out after 23 hours")
	}
	app.clock.advance(23 * time.Hour)
	if !loggedIn() {
		t.Fatal("logged out 23 hours after the last request")
	}

	// 24 hours without a request ends it.
	app.clock.advance(24*time.Hour + time.Second)
	if loggedIn() {
		t.Error("still logged in after 24 hours without a request")
	}
	if rec := app.get("/projects/1", c); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("GET /projects/1 with an expired session: status = %d, want 303 to /login", rec.Code)
	}
}

func TestSessionExtendWritesAtMostOncePerMinute(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	c := app.login("samed")

	// Within the first minute the stored expiry is fresh: no new cookie.
	app.clock.advance(30 * time.Second)
	if rec := app.get("/", c); sessionCookie(rec) != nil {
		t.Error("session extended within its first minute")
	}
	app.clock.advance(31 * time.Second)
	rec := app.get("/", c)
	if got := sessionCookie(rec); got == nil || got.Value != c.Value {
		t.Errorf("cookie after a minute = %v, want the same token sent again", got)
	}
}

func TestLoginPageRedirects(t *testing.T) {
	app := newTestApp(t)
	if rec := app.get("/login", nil); rec.Header().Get("Location") != "/setup" {
		t.Errorf("GET /login on a fresh instance: location = %q, want /setup", rec.Header().Get("Location"))
	}
	app.createUser("samed", true)
	if rec := app.get("/login", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /login: status = %d, want 200", rec.Code)
	}
	c := app.login("samed")
	if rec := app.get("/login", c); rec.Header().Get("Location") != "/" {
		t.Errorf("GET /login when logged in: location = %q, want /", rec.Header().Get("Location"))
	}
}

func TestWaitMessage(t *testing.T) {
	tests := []struct {
		wait time.Duration
		want string
	}{
		{time.Second, "Wait 1 second,"},
		{1500 * time.Millisecond, "Wait 2 seconds,"},
		{119 * time.Second, "Wait 119 seconds,"},
		{15 * time.Minute, "Wait 15 minutes,"},
	}
	for _, tt := range tests {
		if got := waitMessage(tt.wait); !strings.Contains(got, tt.want) {
			t.Errorf("waitMessage(%v) = %q, want it to contain %q", tt.wait, got, tt.want)
		}
	}
}
