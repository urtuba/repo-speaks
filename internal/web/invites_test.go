package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// invitePathRE finds an invite path in a page.
var invitePathRE = regexp.MustCompile(`/invites/[A-Za-z0-9_-]{20,}`)

// inviteApp is a project with an owner, a manager and a builder.
type inviteApp struct {
	*testApp
	project store.Project
	manager store.User
}

func newInviteApp(t *testing.T) inviteApp {
	t.Helper()
	app := newTestApp(t)
	ctx := context.Background()
	app.createUser("samed", true)
	manager := app.createUser("mert", false)
	builder := app.createUser("bora", false)
	p, err := app.store.CreateProject(ctx, "Clinic app", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddMember(ctx, p.ID, manager.ID, permissions.PresetManager.Permissions().Strings()); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddMember(ctx, p.ID, builder.ID, permissions.PresetBuilder.Permissions().Strings()); err != nil {
		t.Fatal(err)
	}
	return inviteApp{testApp: app, project: p, manager: manager}
}

// createInvite makes an invite as username and returns its path.
func (a inviteApp) createInvite(username string, preset permissions.Preset) string {
	a.t.Helper()
	rec := a.post(a.invitesPath(), url.Values{"preset": {string(preset)}}, a.login(username))
	if rec.Code != http.StatusOK {
		a.t.Fatalf("create invite: status = %d, want 200\n%s", rec.Code, rec.Body)
	}
	path := invitePathRE.FindString(rec.Body.String())
	if path == "" {
		a.t.Fatalf("create invite: no link in the page\n%s", rec.Body)
	}
	return path
}

func (a inviteApp) invitesPath() string {
	return projectPath(a.project.ID) + "/invites"
}

// permsOf returns the permission names username holds in the project.
func (a inviteApp) permsOf(username string) ([]string, error) {
	u, err := a.store.UserByUsername(context.Background(), username)
	if err != nil {
		return nil, err
	}
	return a.store.MemberPermissions(context.Background(), a.project.ID, u.ID)
}

// newAccount is the form for a new account with testPassword.
func newAccount(username string) url.Values {
	return url.Values{"username": {username}, "password": {testPassword}, "password_again": {testPassword}}
}

func TestCreateInvite(t *testing.T) {
	app := newInviteApp(t)
	app.createUser("omer", false) // not a member

	tests := []struct {
		name       string
		username   string
		preset     permissions.Preset
		wantStatus int
	}{
		{"manager invites an asker", "mert", permissions.PresetAsker, http.StatusOK},
		{"manager invites a manager", "mert", permissions.PresetManager, http.StatusOK},
		{"owner invites an admin", "samed", permissions.PresetAdmin, http.StatusOK},
		{"manager cannot invite an admin", "mert", permissions.PresetAdmin, http.StatusForbidden},
		{"unknown preset", "mert", "owner", http.StatusForbidden},
		{"builder cannot invite", "bora", permissions.PresetAsker, http.StatusForbidden},
		{"outsider gets 404", "omer", permissions.PresetAsker, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.post(app.invitesPath(), url.Values{"preset": {string(tt.preset)}}, app.login(tt.username))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if !strings.Contains(rec.Body.String(), "http://example.com/invites/") {
				t.Errorf("page has no full link:\n%s", rec.Body)
			}
		})
	}

	open, err := app.store.OpenInvites(context.Background(), app.project.ID, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 3 {
		t.Errorf("open invites = %d, want 3", len(open))
	}
}

func TestInviteNewAccount(t *testing.T) {
	app := newInviteApp(t)
	path := app.createInvite("mert", permissions.PresetAsker)

	page := app.get(path, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Create account and join") {
		t.Fatalf("GET link: status = %d, want 200 with the account form\n%s", page.Code, page.Body)
	}

	short := url.Values{"username": {"ayse"}, "password": {"short"}, "password_again": {"short"}}
	if rec := app.post(path, short, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("short password: status = %d, want 400", rec.Code)
	}

	rec := app.post(path, newAccount("ayse"), nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != projectPath(app.project.ID) {
		t.Fatalf("accept: status = %d, location = %q, want 303 to the project", rec.Code, rec.Header().Get("Location"))
	}
	if sessionCookie(rec) == nil {
		t.Error("accept did not log the new account in")
	}
	perms, err := app.permsOf("ayse")
	if err != nil {
		t.Fatal(err)
	}
	if want := permissions.PresetAsker.Permissions().Strings(); !reflect.DeepEqual(perms, want) {
		t.Errorf("permissions = %v, want %v", perms, want)
	}

	// The link works once.
	if rec := app.get(path, nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no longer works") {
		t.Errorf("GET used link: status = %d, want 404 with a dead-link page", rec.Code)
	}
	if rec := app.post(path, newAccount("zeynep"), nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST used link: status = %d, want 404", rec.Code)
	}
	if _, err := app.store.UserByUsername(context.Background(), "zeynep"); err == nil {
		t.Error("a used link made an account")
	}
}

func TestInviteExistingAccount(t *testing.T) {
	app := newInviteApp(t)
	app.createUser("ayse", false)
	path := app.createInvite("mert", permissions.PresetBuilder)
	c := app.login("ayse")

	page := app.get(path, c)
	if !strings.Contains(page.Body.String(), "Join as ayse") {
		t.Fatalf("GET link logged in: want a join button\n%s", page.Body)
	}
	rec := app.post(path, nil, c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("join: status = %d, want 303\n%s", rec.Code, rec.Body)
	}
	if _, err := app.permsOf("ayse"); err != nil {
		t.Errorf("ayse is not a member: %v", err)
	}
}

func TestInviteForAMember(t *testing.T) {
	app := newInviteApp(t)
	path := app.createInvite("mert", permissions.PresetAsker)

	// bora is already a builder in the project.
	c := app.login("bora")
	if body := app.get(path, c).Body.String(); !strings.Contains(body, "You are already in Clinic app") {
		t.Errorf("GET link as a member: want the already-a-member page\n%s", body)
	}
	if rec := app.post(path, nil, c); rec.Code != http.StatusConflict {
		t.Errorf("POST link as a member: status = %d, want 409", rec.Code)
	}

	// The owner already has access to every project.
	if body := app.get(path, app.login("samed")).Body.String(); !strings.Contains(body, "You already have access") {
		t.Errorf("GET link as the owner: want the already-have-access page\n%s", body)
	}

	// Neither used the link, so someone else still can.
	if rec := app.post(path, newAccount("ayse"), nil); rec.Code != http.StatusSeeOther {
		t.Errorf("link after the member's visit: status = %d, want 303", rec.Code)
	}
}

func TestInviteUsernameTaken(t *testing.T) {
	app := newInviteApp(t)
	path := app.createInvite("mert", permissions.PresetAsker)

	rec := app.post(path, newAccount("bora"), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "That username is taken") {
		t.Fatalf("taken username: status = %d, want 400 with a message\n%s", rec.Code, rec.Body)
	}
	if rec := app.post(path, newAccount("ayse"), nil); rec.Code != http.StatusSeeOther {
		t.Errorf("link after a taken username: status = %d, want 303", rec.Code)
	}
}

func TestDeadInvites(t *testing.T) {
	tests := []struct {
		name string
		kill func(t *testing.T, app inviteApp, path string)
	}{
		{"unknown token", nil},
		{"expired", func(t *testing.T, app inviteApp, path string) {
			app.clock.advance(auth.InviteLifetime)
		}},
		{"revoked", func(t *testing.T, app inviteApp, path string) {
			invites, err := app.store.OpenInvites(context.Background(), app.project.ID, app.clock.now())
			if err != nil || len(invites) != 1 {
				t.Fatalf("open invites = %v, %v", invites, err)
			}
			revoke := app.invitesPath() + "/" + strconv.FormatInt(invites[0].ID, 10) + "/revoke"
			if rec := app.post(revoke, nil, app.login("mert")); rec.Code != http.StatusSeeOther {
				t.Fatalf("revoke: status = %d, want 303", rec.Code)
			}
		}},
		{"creator left the project", func(t *testing.T, app inviteApp, path string) {
			if err := app.store.RemoveMember(context.Background(), app.project.ID, app.manager.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"creator lost members:edit", func(t *testing.T, app inviteApp, path string) {
			ctx := context.Background()
			if err := app.store.RemoveMember(ctx, app.project.ID, app.manager.ID); err != nil {
				t.Fatal(err)
			}
			if err := app.store.AddMember(ctx, app.project.ID, app.manager.ID, permissions.PresetBuilder.Permissions().Strings()); err != nil {
				t.Fatal(err)
			}
		}},
		{"creator deleted", func(t *testing.T, app inviteApp, path string) {
			if err := app.store.DeleteUser(context.Background(), app.manager.ID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newInviteApp(t)
			path := app.createInvite("mert", permissions.PresetAsker)
			if tt.kill == nil {
				path = "/invites/" + strings.Repeat("x", 43)
			} else {
				tt.kill(t, app, path)
			}
			if rec := app.get(path, nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no longer works") {
				t.Errorf("GET: status = %d, want 404 with a dead-link page", rec.Code)
			}
			if rec := app.post(path, newAccount("ayse"), nil); rec.Code != http.StatusNotFound {
				t.Errorf("POST: status = %d, want 404", rec.Code)
			}
			if _, err := app.store.UserByUsername(context.Background(), "ayse"); err == nil {
				t.Error("a dead link made an account")
			}
		})
	}
}

func TestInviteStillWorksBeforeExpiry(t *testing.T) {
	app := newInviteApp(t)
	path := app.createInvite("mert", permissions.PresetAsker)
	app.clock.advance(auth.InviteLifetime - time.Second)
	if rec := app.post(path, newAccount("ayse"), nil); rec.Code != http.StatusSeeOther {
		t.Errorf("one second before expiry: status = %d, want 303", rec.Code)
	}
}

func TestRevokeNeedsMembersEdit(t *testing.T) {
	app := newInviteApp(t)
	app.createInvite("mert", permissions.PresetAsker)
	invites, err := app.store.OpenInvites(context.Background(), app.project.ID, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	revoke := app.invitesPath() + "/" + strconv.FormatInt(invites[0].ID, 10) + "/revoke"
	if rec := app.post(revoke, nil, app.login("bora")); rec.Code != http.StatusForbidden {
		t.Errorf("builder revokes: status = %d, want 403", rec.Code)
	}
}

func TestLoginNext(t *testing.T) {
	app := newTestApp(t)
	app.createUser("ayse", false)

	tests := []struct {
		next string
		want string
	}{
		{"/invites/abc", "/invites/abc"},
		{"", "/"},
		{"https://evil.example/", "/"},
		{"//evil.example/", "/"},
		{`/\evil.example/`, "/"},
	}
	for _, tt := range tests {
		form := url.Values{"username": {"ayse"}, "password": {testPassword}, "next": {tt.next}}
		rec := app.post("/login", form, nil)
		if got := rec.Header().Get("Location"); got != tt.want {
			t.Errorf("next %q: location = %q, want %q", tt.next, got, tt.want)
		}
	}

	if body := app.get("/login?next=/invites/abc", nil).Body.String(); !strings.Contains(body, `name="next" value="/invites/abc"`) {
		t.Error("login page does not keep next in the form")
	}
}
