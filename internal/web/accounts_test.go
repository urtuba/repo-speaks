package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/permissions"
)

// resetPathRE finds a reset path in a page.
var resetPathRE = regexp.MustCompile(`/reset/[A-Za-z0-9_-]{20,}`)

// newPassword is a second valid password for the tests.
const newPassword = "another long passphrase"

// makeReset has the owner samed make a reset link for the account and
// returns its path.
func makeReset(t *testing.T, app *testApp, userID int64) string {
	t.Helper()
	rec := app.post("/accounts/"+strconv.FormatInt(userID, 10)+"/reset", nil, app.login("samed"))
	if rec.Code != http.StatusOK {
		t.Fatalf("make reset link: status = %d, want 200\n%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	path := resetPathRE.FindString(rec.Body.String())
	if path == "" {
		t.Fatalf("no reset link in the page\n%s", rec.Body)
	}
	return path
}

func passwordForm(password string) url.Values {
	return url.Values{"password": {password}, "password_again": {password}}
}

// canLogin reports whether username can log in with password.
func canLogin(app *testApp, username, password string) bool {
	rec := app.post("/login", url.Values{"username": {username}, "password": {password}}, nil)
	return rec.Code == http.StatusSeeOther
}

func TestAccountsPage(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	ayse := app.createUser("ayse", false)
	app.createUser("bora", false)
	p, err := app.store.CreateProject(context.Background(), "Clinic app", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddMember(context.Background(), p.ID, ayse.ID, permissions.PresetBuilder.Permissions().Strings()); err != nil {
		t.Fatal(err)
	}

	rec := app.get("/accounts", app.login("samed"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<a href="/projects/1">Clinic app</a> <span class="muted small">builder</span>`,
		"No projects",   // bora
		"Every project", // the owner
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if n := strings.Count(body, "Reset link</button>"); n != 2 {
		t.Errorf("reset buttons = %d, want 2 (none for the owner)", n)
	}
	if rec := app.get("/accounts", app.login("ayse")); rec.Code != http.StatusForbidden {
		t.Errorf("non-owner: status = %d, want 403", rec.Code)
	}
}

func TestCreateResetRules(t *testing.T) {
	app := newTestApp(t)
	owner := app.createUser("samed", true)
	ayse := app.createUser("ayse", false)
	bora := app.createUser("bora", false)

	path := func(id int64) string { return "/accounts/" + strconv.FormatInt(id, 10) + "/reset" }
	if rec := app.post(path(bora.ID), nil, app.login("ayse")); rec.Code != http.StatusForbidden {
		t.Errorf("non-owner makes a link: status = %d, want 403", rec.Code)
	}
	if rec := app.post(path(owner.ID), nil, app.login("samed")); rec.Code != http.StatusBadRequest {
		t.Errorf("owner makes a link for themselves: status = %d, want 400", rec.Code)
	}
	if rec := app.post(path(999), nil, app.login("samed")); rec.Code != http.StatusNotFound {
		t.Errorf("unknown account: status = %d, want 404", rec.Code)
	}

	// A new link stops the old one.
	first := makeReset(t, app, ayse.ID)
	second := makeReset(t, app, ayse.ID)
	if rec := app.get(first, nil); rec.Code != http.StatusNotFound {
		t.Errorf("old link after a new one: status = %d, want 404", rec.Code)
	}
	if rec := app.get(second, nil); rec.Code != http.StatusOK {
		t.Errorf("new link: status = %d, want 200", rec.Code)
	}
}

func TestResetFlow(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	ayse := app.createUser("ayse", false)
	oldSession := app.login("ayse")
	link := makeReset(t, app, ayse.ID)

	page := app.get(link, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<strong>ayse</strong>") {
		t.Fatalf("GET link: status = %d, want 200 naming ayse\n%s", page.Code, page.Body)
	}
	if rec := app.post(link, passwordForm("short"), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("short password: status = %d, want 400", rec.Code)
	}

	rec := app.post(link, passwordForm(newPassword), nil)
	if rec.Code != http.StatusSeeOther || sessionCookie(rec) == nil {
		t.Fatalf("POST link: status = %d, want 303 with a session cookie", rec.Code)
	}
	if !canLogin(app, "ayse", newPassword) || canLogin(app, "ayse", testPassword) {
		t.Error("after a reset, only the new password should work")
	}
	if rec := app.get("/", oldSession); rec.Code != http.StatusSeeOther {
		t.Errorf("old session after a reset: status = %d, want 303 to login", rec.Code)
	}
	if rec := app.post(link, passwordForm("yet another passphrase"), nil); rec.Code != http.StatusNotFound {
		t.Errorf("second use: status = %d, want 404", rec.Code)
	}
}

func TestResetLinkExpires(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	ayse := app.createUser("ayse", false)
	link := makeReset(t, app, ayse.ID)
	app.clock.advance(auth.ResetLifetime)
	if rec := app.post(link, passwordForm(newPassword), nil); rec.Code != http.StatusNotFound {
		t.Errorf("expired link: status = %d, want 404", rec.Code)
	}
	if !canLogin(app, "ayse", testPassword) {
		t.Error("an expired link changed the password")
	}
}

func TestChangePassword(t *testing.T) {
	app := newTestApp(t)
	app.createUser("ayse", false)
	other := app.login("ayse") // another device
	c := app.login("ayse")

	form := func(current, next, again string) url.Values {
		return url.Values{"current_password": {current}, "password": {next}, "password_again": {again}}
	}
	if rec := app.post("/account", form("wrong password!", newPassword, newPassword), c); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "current password is wrong") {
		t.Errorf("wrong current password: status = %d, want 400 with a message", rec.Code)
	}
	if rec := app.post("/account", form(testPassword, newPassword, "something else!!"), c); rec.Code != http.StatusBadRequest {
		t.Errorf("passwords differ: status = %d, want 400", rec.Code)
	}

	rec := app.post("/account", form(testPassword, newPassword, newPassword), c)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account?saved=1" {
		t.Fatalf("change: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	fresh := sessionCookie(rec)
	if fresh == nil {
		t.Fatal("change did not start a new session")
	}
	if rec := app.get("/account", fresh); rec.Code != http.StatusOK {
		t.Errorf("new session: status = %d, want 200", rec.Code)
	}
	if rec := app.get("/account", other); rec.Code != http.StatusSeeOther {
		t.Errorf("other device after the change: status = %d, want 303 to login", rec.Code)
	}
	if !canLogin(app, "ayse", newPassword) {
		t.Error("the new password does not work")
	}
}

func TestChangePasswordThrottle(t *testing.T) {
	app := newTestApp(t)
	app.createUser("ayse", false)
	c := app.login("ayse")
	wrong := url.Values{"current_password": {"wrong password!"}, "password": {newPassword}, "password_again": {newPassword}}
	for i := range 5 {
		if rec := app.post("/account", wrong, c); rec.Code != http.StatusBadRequest {
			t.Fatalf("try %d: status = %d, want 400", i+1, rec.Code)
		}
	}
	if rec := app.post("/account", wrong, c); rec.Code != http.StatusTooManyRequests {
		t.Errorf("sixth try: status = %d, want 429", rec.Code)
	}
}
