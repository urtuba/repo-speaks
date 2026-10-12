package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// memberApp is the invite fixture (owner samed, manager mert, builder
// bora) plus an admin ada and an asker asli.
type memberApp struct {
	inviteApp
	ids map[string]int64
}

func newMemberApp(t *testing.T) memberApp {
	t.Helper()
	app := memberApp{inviteApp: newInviteApp(t), ids: map[string]int64{}}
	ctx := context.Background()
	for name, preset := range map[string]permissions.Preset{"ada": permissions.PresetAdmin, "asli": permissions.PresetAsker} {
		u := app.createUser(name, false)
		if err := app.store.AddMember(ctx, app.project.ID, u.ID, preset.Permissions().Strings()); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"samed", "mert", "bora", "ada", "asli"} {
		u, err := app.store.UserByUsername(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		app.ids[name] = u.ID
	}
	return app
}

func (a memberApp) memberPath(username string) string {
	return projectPath(a.project.ID) + "/members/" + strconv.FormatInt(a.ids[username], 10)
}

func permForm(perms ...permissions.Permission) url.Values {
	form := url.Values{}
	for _, p := range perms {
		form.Add("permission", string(p))
	}
	return form
}

func TestMemberPage(t *testing.T) {
	app := newMemberApp(t)
	rec := app.get(app.memberPath("bora"), app.login("mert"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`value="resources:edit" data-requires="resources:view" checked`,
		`value="members:edit" data-requires="resources:edit agents:edit members:view">`, // direct needs; the page follows the chain
		`value="project:edit" data-requires="members:edit" disabled`,                    // mert lacks it
		"Role: builder",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestSaveMember(t *testing.T) {
	tests := []struct {
		name       string
		editor     string
		target     string
		form       url.Values
		wantStatus int
		wantPerms  []permissions.Permission // nil: the target's permissions must not change
		wantBody   string
	}{
		{"manager lowers a builder to asker", "mert", "bora",
			permForm(permissions.PermResourcesView), http.StatusSeeOther,
			[]permissions.Permission{permissions.PermResourcesView}, ""},
		{"manager gives an asker chats:view", "mert", "asli",
			permForm(permissions.PermResourcesView, permissions.PermChatsView), http.StatusSeeOther,
			[]permissions.Permission{permissions.PermResourcesView, permissions.PermChatsView}, ""},
		{"empty set means asks only", "mert", "asli", url.Values{}, http.StatusSeeOther,
			[]permissions.Permission{}, ""},
		{"a set that breaks a rule", "mert", "bora",
			permForm(permissions.PermResourcesEdit), http.StatusBadRequest, nil,
			"Not saved: resources:edit needs resources:view."},
		{"unknown permission", "mert", "bora",
			url.Values{"permission": {"agents:use"}}, http.StatusBadRequest, nil, "does not know"},
		{"manager cannot give project:edit", "mert", "bora",
			permForm(permissions.PresetAdmin.Permissions().List()...), http.StatusForbidden, nil,
			"only give permissions you hold"},
		{"manager cannot change an admin", "mert", "ada",
			permForm(permissions.PermResourcesView), http.StatusForbidden, nil, ""},
		{"nobody changes themselves", "mert", "mert",
			permForm(permissions.PresetAdmin.Permissions().List()...), http.StatusForbidden, nil, ""},
		{"builder cannot change anyone", "bora", "asli",
			url.Values{}, http.StatusForbidden, nil, ""},
		{"owner changes an admin", "samed", "ada",
			permForm(permissions.PermResourcesView), http.StatusSeeOther,
			[]permissions.Permission{permissions.PermResourcesView}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newMemberApp(t)
			before, err := app.permsOf(tt.target)
			if err != nil {
				t.Fatal(err)
			}
			rec := app.post(app.memberPath(tt.target), tt.form, app.login(tt.editor))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tt.wantStatus, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body does not contain %q\n%s", tt.wantBody, rec.Body)
			}
			got, err := app.permsOf(tt.target)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			if tt.wantPerms != nil {
				set := permissions.Set{}
				for _, p := range tt.wantPerms {
					set[p] = true
				}
				// The store returns names sorted, not in display order.
				want = append([]string{}, set.Strings()...)
				slices.Sort(want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("permissions = %v, want %v", got, want)
			}
		})
	}
}

func TestRemoveMember(t *testing.T) {
	tests := []struct {
		name       string
		editor     string
		target     string
		wantStatus int
	}{
		{"manager removes a builder", "mert", "bora", http.StatusSeeOther},
		{"manager cannot remove an admin", "mert", "ada", http.StatusForbidden},
		{"builder cannot remove", "bora", "asli", http.StatusForbidden},
		{"owner removes an admin", "samed", "ada", http.StatusSeeOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newMemberApp(t)
			rec := app.post(app.memberPath(tt.target)+"/remove", nil, app.login(tt.editor))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			_, err := app.permsOf(tt.target)
			removed := errors.Is(err, store.ErrNotFound)
			if removed != (tt.wantStatus == http.StatusSeeOther) {
				t.Errorf("removed = %v, want %v", removed, tt.wantStatus == http.StatusSeeOther)
			}
		})
	}
}

func TestRemovedManagersLinksDie(t *testing.T) {
	app := newMemberApp(t)
	link := app.createInvite("mert", permissions.PresetAsker)
	if rec := app.post(app.memberPath("mert")+"/remove", nil, app.login("ada")); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: status = %d, want 303", rec.Code)
	}
	if rec := app.get(link, nil); rec.Code != http.StatusNotFound {
		t.Errorf("link of a removed manager: status = %d, want 404", rec.Code)
	}
}

func TestLeaveProject(t *testing.T) {
	app := newMemberApp(t)
	c := app.login("bora")
	rec := app.post(projectPath(app.project.ID)+"/leave", nil, c)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("leave: status = %d, location = %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if rec := app.get(projectPath(app.project.ID), c); rec.Code != http.StatusNotFound {
		t.Errorf("project after leaving: status = %d, want 404", rec.Code)
	}
	if rec := app.post(projectPath(app.project.ID)+"/leave", nil, app.login("samed")); rec.Code != http.StatusBadRequest {
		t.Errorf("owner leaves: status = %d, want 400", rec.Code)
	}
}

func TestSettings(t *testing.T) {
	app := newMemberApp(t)
	settings := projectPath(app.project.ID) + "/settings"
	ada := app.login("ada")

	page := app.get(settings, ada)
	if page.Code != http.StatusOK {
		t.Fatalf("admin opens settings: status = %d, want 200", page.Code)
	}
	if strings.Contains(page.Body.String(), "Delete project") {
		t.Error("an admin sees the delete form; only the owner may delete")
	}
	if rec := app.get(settings, app.login("mert")); rec.Code != http.StatusForbidden {
		t.Errorf("manager opens settings: status = %d, want 403", rec.Code)
	}

	if rec := app.post(settings, url.Values{"name": {"  "}}, ada); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: status = %d, want 400", rec.Code)
	}
	rec := app.post(settings, url.Values{"name": {" Clinic "}}, ada)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != settings+"?saved=1" {
		t.Fatalf("rename: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if p, _ := app.store.Project(context.Background(), app.project.ID); p.Name != "Clinic" {
		t.Errorf("name = %q, want Clinic", p.Name)
	}
}

func TestDeleteProject(t *testing.T) {
	app := newMemberApp(t)
	del := projectPath(app.project.ID) + "/delete"

	if rec := app.post(del, url.Values{"confirm": {"Clinic app"}}, app.login("ada")); rec.Code != http.StatusForbidden {
		t.Errorf("admin deletes: status = %d, want 403", rec.Code)
	}
	owner := app.login("samed")
	if rec := app.post(del, url.Values{"confirm": {"clinic app"}}, owner); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong name: status = %d, want 400", rec.Code)
	}
	if _, err := app.store.Project(context.Background(), app.project.ID); err != nil {
		t.Fatalf("project gone after a wrong name: %v", err)
	}
	rec := app.post(del, url.Values{"confirm": {"Clinic app"}}, owner)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("delete: status = %d, location = %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := app.store.Project(context.Background(), app.project.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("project after delete: error = %v, want ErrNotFound", err)
	}
}

func TestProjectPageMemberActions(t *testing.T) {
	app := newMemberApp(t)
	body := app.get(projectPath(app.project.ID), app.login("mert")).Body.String()
	for _, want := range []string{
		`href="` + app.memberPath("bora") + `"`, // mert may edit a builder
		"Leave project",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	for _, notWant := range []string{
		`href="` + app.memberPath("ada") + `"`,  // an admin is above mert
		`href="` + app.memberPath("mert") + `"`, // nobody edits themselves
		"/settings",                             // mert lacks project:edit
	} {
		if strings.Contains(body, notWant) {
			t.Errorf("page contains %q", notWant)
		}
	}
}
