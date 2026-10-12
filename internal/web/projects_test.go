package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/permissions"
)

func TestCreateProject(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	app.createUser("ayse", false)
	owner := app.login("samed")
	other := app.login("ayse")

	tests := []struct {
		name       string
		cookie     *http.Cookie
		project    string
		wantStatus int
	}{
		{"owner", owner, "Customer portal", http.StatusSeeOther},
		{"not the owner", other, "Billing", http.StatusForbidden},
		{"not logged in", nil, "Billing", http.StatusSeeOther},
		{"empty name", owner, "   ", http.StatusBadRequest},
		{"long name", owner, strings.Repeat("a", 101), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.post("/projects", url.Values{"name": {tt.project}}, tt.cookie)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}

	all, err := app.store.AllProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "Customer portal" {
		t.Errorf("projects = %+v, want only Customer portal", all)
	}
	rec := app.post("/projects", url.Values{"name": {"Billing"}}, owner)
	if got, want := rec.Header().Get("Location"), "/projects/2"; got != want {
		t.Errorf("location = %q, want %q", got, want)
	}
}

func TestProjectPage(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	app.createUser("samed", true)
	builder := app.createUser("bora", false)
	asker := app.createUser("asli", false)
	app.createUser("omer", false) // has no membership

	p, err := app.store.CreateProject(ctx, "Customer portal", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddMember(ctx, p.ID, builder.ID, permissions.PresetBuilder.Permissions().Strings()); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddMember(ctx, p.ID, asker.ID, permissions.PresetAsker.Permissions().Strings()); err != nil {
		t.Fatal(err)
	}
	path := "/projects/" + strconv.FormatInt(p.ID, 10)

	tests := []struct {
		name       string
		username   string
		path       string
		wantStatus int
		want       []string
		notWant    []string
	}{
		{"owner without membership", "samed", path, http.StatusOK,
			[]string{"Customer portal", "instance owner", "project:edit", "<td>bora", "<td>asli</td>", "Invite links"}, nil},
		{"builder sees members", "bora", path, http.StatusOK,
			[]string{"Customer portal", "agents:edit", "members:view", "<td>bora", "<td>asli</td>", `samed <span class="badge owner">owner</span>`},
			[]string{"members:edit", "cannot see the members", "Invite links"}},
		{"asker does not see members", "asli", path, http.StatusOK,
			[]string{"Customer portal", "resources:view", "You cannot see the members"},
			[]string{"bora", "agents:edit", "badge owner"}},
		{"outsider gets 404", "omer", path, http.StatusNotFound, nil, []string{"Customer portal"}},
		{"missing project", "samed", "/projects/999", http.StatusNotFound, nil, nil},
		{"bad id", "samed", "/projects/abc", http.StatusNotFound, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.get(tt.path, app.login(tt.username))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			body := rec.Body.String()
			for _, s := range tt.want {
				if !strings.Contains(body, s) {
					t.Errorf("body does not contain %q", s)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(body, s) {
					t.Errorf("body contains %q", s)
				}
			}
		})
	}

	// A project the outsider cannot open looks like one that does not exist.
	c := app.login("omer")
	hidden, missing := app.get(path, c), app.get("/projects/999", c)
	if hidden.Code != missing.Code || hidden.Body.String() != missing.Body.String() {
		t.Error("a hidden project answers differently from a missing one")
	}

	if rec := app.get(path, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("not logged in: status = %d, want 303 to /login", rec.Code)
	}
}

func TestHomeListsProjects(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	app.createUser("samed", true)
	member := app.createUser("ayse", false)
	portal, _ := app.store.CreateProject(ctx, "Customer portal", app.clock.now())
	app.store.CreateProject(ctx, "Billing", app.clock.now())
	if err := app.store.AddMember(ctx, portal.ID, member.ID, nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		username string
		want     []string
		notWant  []string
	}{
		{"samed", []string{"Customer portal", "Billing", `action="/projects"`}, nil},
		{"ayse", []string{"Customer portal"}, []string{"Billing", `action="/projects"`}},
	}
	for _, tt := range tests {
		t.Run(tt.username, func(t *testing.T) {
			body := app.get("/", app.login(tt.username)).Body.String()
			for _, s := range tt.want {
				if !strings.Contains(body, s) {
					t.Errorf("body does not contain %q", s)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(body, s) {
					t.Errorf("body contains %q", s)
				}
			}
		})
	}
}

func TestPagesIncludeHTMXConfig(t *testing.T) {
	const meta = `<meta name="htmx-config" content='{"allowEval":false,"includeIndicatorStyles":false}'>`
	app := newTestApp(t)
	pages := []struct {
		name string
		path string
	}{
		{"setup", "/setup"},
	}
	for _, p := range pages {
		if body := app.get(p.path, nil).Body.String(); !strings.Contains(body, meta) {
			t.Errorf("%s: no htmx-config meta", p.name)
		}
	}

	app.createUser("samed", true)
	app.store.CreateProject(context.Background(), "Customer portal", app.clock.now())
	c := app.login("samed")
	for _, p := range []struct {
		name   string
		path   string
		cookie *http.Cookie
	}{
		{"login", "/login", nil},
		{"home, logged in", "/", c},
		{"project", "/projects/1", c},
	} {
		if body := app.get(p.path, p.cookie).Body.String(); !strings.Contains(body, meta) {
			t.Errorf("%s: no htmx-config meta", p.name)
		}
	}
}

// TestStoreKnowsEveryPermission checks that the database's permissions
// table lists every permission the Go code has. A new permission needs a
// migration too, and this test fails until it exists.
func TestStoreKnowsEveryPermission(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	u := app.createUser("ayse", false)
	p, err := app.store.CreateProject(ctx, "Customer portal", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddMember(ctx, p.ID, u.ID, permissions.All().Strings()); err != nil {
		t.Errorf("AddMember with every permission: %v", err)
	}
}
