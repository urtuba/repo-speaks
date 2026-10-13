package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

const (
	webTestToken  = "github_pat_11AAAAAAA0secretvalue_wxyz"
	webTestCommit = "0123456789abcdef0123456789abcdef01234567"
)

// resourceApp is the member fixture (owner samed, manager mert, builder
// bora, admin ada, asker asli) plus nur, a member with no permissions,
// and otto, who is in no project. The project has urtuba/demo with a
// token, and another project has a resource of its own.
type resourceApp struct {
	memberApp
	res   store.Resource
	other store.Resource
}

func newResourceApp(t *testing.T) resourceApp {
	t.Helper()
	ctx := context.Background()
	app := resourceApp{memberApp: newMemberApp(t)}
	nur := app.createUser("nur", false)
	if err := app.store.AddMember(ctx, app.project.ID, nur.ID, nil); err != nil {
		t.Fatal(err)
	}
	app.createUser("otto", false)
	var err error
	app.res, err = app.store.AddResource(ctx, store.NewResource{
		ProjectID: app.project.ID, Owner: "urtuba", Repo: "demo", Token: webTestToken,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	billing, err := app.store.CreateProject(ctx, "Billing", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	app.other, err = app.store.AddResource(ctx, store.NewResource{
		ProjectID: billing.ID, Owner: "urtuba", Repo: "billing",
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// load reads the fixture's resource again.
func (a resourceApp) load(t *testing.T) store.Resource {
	t.Helper()
	r, err := a.store.Resource(context.Background(), a.project.ID, a.res.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// token returns the resource's stored token.
func (a resourceApp) token(t *testing.T) string {
	t.Helper()
	src, err := a.store.SyncSource(context.Background(), a.res.ID)
	if err != nil {
		t.Fatal(err)
	}
	return src.Token
}

// finishJob runs the resource's open job to done.
func (a resourceApp) finishJob(t *testing.T) store.Job {
	t.Helper()
	job, err := a.store.ClaimJob(context.Background(), a.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.FinishJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	return job
}

// checkBody checks that body holds every want and no notWant. A page
// must never hold the token.
func checkBody(t *testing.T, body string, want, notWant []string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body does not contain %q", s)
		}
	}
	for _, s := range append(notWant, webTestToken) {
		if strings.Contains(body, s) {
			t.Errorf("body contains %q", s)
		}
	}
}

func TestProjectPageResources(t *testing.T) {
	app := newResourceApp(t)
	path := projectPath(app.project.ID)
	addForm := `action="` + path + `/resources"`

	tests := []struct {
		name       string
		username   string
		wantStatus int
		want       []string
		notWant    []string
	}{
		{"owner", "samed", http.StatusOK,
			[]string{"urtuba/demo", "default branch", "Sync now", "Token set ····wxyz", addForm, `type="password" autocomplete="off"`}, nil},
		{"builder", "bora", http.StatusOK,
			[]string{"urtuba/demo", "Sync now", "Token set ····wxyz", addForm}, nil},
		{"asker sees the list only", "asli", http.StatusOK,
			[]string{"urtuba/demo", "default branch", `<span class="badge">queued</span>`},
			[]string{"Sync now", "Token set", addForm, "/remove"}},
		{"member without resources:view", "nur", http.StatusOK,
			[]string{"You cannot see this project's repositories."},
			[]string{"urtuba/demo", addForm}},
		{"outsider", "otto", http.StatusNotFound, nil, []string{"urtuba/demo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.get(path, app.login(tt.username))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			checkBody(t, rec.Body.String(), tt.want, tt.notWant)
		})
	}
	// The other project's resource never shows here.
	if body := app.get(path, app.login("samed")).Body.String(); strings.Contains(body, "urtuba/billing") {
		t.Error("the page lists another project's resource")
	}
}

func TestResourceStatus(t *testing.T) {
	ctx := context.Background()
	app := newResourceApp(t)
	path := projectPath(app.project.ID)
	c := app.login("asli")
	poll := `hx-get="` + path + `/resources" hx-trigger="every 5s"`

	// Queued: the list reloads itself.
	checkBody(t, app.get(path, c).Body.String(),
		[]string{`<span class="badge">queued</span>`, poll, "never"}, nil)

	job, err := app.store.ClaimJob(ctx, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	checkBody(t, app.get(path, c).Body.String(), []string{`<span class="badge">running</span>`, poll}, nil)

	// Done: the commit and the time show, and the reloading stops.
	if err := app.store.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.store.SaveSync(ctx, app.res.ID, "1", webTestCommit, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	app.clock.advance(5 * time.Minute)
	checkBody(t, app.get(path, c).Body.String(),
		[]string{
			"5 minutes ago",
			`<a class="mono" href="https://github.com/urtuba/demo/commit/` + webTestCommit + `">0123456</a>`,
		},
		[]string{"hx-trigger", `<span class="badge">queued</span>`})

	// A failed sync shows its error. The other project's first sync is
	// older, so it goes first.
	if job := app.finishJob(t); job.ResourceID != app.other.ID {
		t.Fatalf("claimed resource %d, want the other project's", job.ResourceID)
	}
	if _, err := app.store.QueueSync(ctx, app.res.ID, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	job, err = app.store.ClaimJob(ctx, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.FailJob(ctx, job, "repository urtuba/demo not found, or the token has no access to it"); err != nil {
		t.Fatal(err)
	}
	checkBody(t, app.get(path, c).Body.String(),
		[]string{`<span class="error-text">repository urtuba/demo not found, or the token has no access to it</span>`}, nil)
}

func TestResourcesFragment(t *testing.T) {
	app := newResourceApp(t)
	path := projectPath(app.project.ID) + "/resources"

	rec := app.get(path, app.login("asli"))
	if rec.Code != http.StatusOK {
		t.Fatalf("asker: status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, `<div id="resource-list" hx-get=`) || strings.Contains(body, "<html") {
		t.Errorf("fragment = %q, want the list alone", body)
	}

	for _, tt := range []struct {
		username string
		want     int
	}{
		{"nur", http.StatusForbidden},
		{"otto", http.StatusNotFound},
	} {
		if rec := app.get(path, app.login(tt.username)); rec.Code != tt.want {
			t.Errorf("%s: status = %d, want %d", tt.username, rec.Code, tt.want)
		}
	}
	if rec := app.get(path, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("not logged in: status = %d, want 303", rec.Code)
	}
}

func TestAddResource(t *testing.T) {
	tests := []struct {
		name       string
		username   string
		repo       string
		branch     string
		token      string
		wantStatus int
		wantBody   string
		// For a success: the resource that must exist.
		wantOwner, wantRepo string
	}{
		{"owner/repo", "bora", "urtuba/one", "", "", http.StatusSeeOther, "", "urtuba", "one"},
		{"github.com form and branch", "bora", "github.com/urtuba/two", "release/v1.2", "", http.StatusSeeOther, "", "urtuba", "two"},
		{"https form with .git and token", "bora", " https://github.com/urtuba/three.git ", "", webTestToken, http.StatusSeeOther, "", "urtuba", "three"},
		{"https form with a slash", "bora", "https://github.com/Urtuba/.github/", "", "", http.StatusSeeOther, "", "Urtuba", ".github"},
		{"owner adds", "samed", "urtuba/four", "", "", http.StatusSeeOther, "", "urtuba", "four"},
		{"another host", "bora", "https://gitlab.com/urtuba/demo", "", "", http.StatusBadRequest, "Only GitHub", "", ""},
		{"http", "bora", "http://github.com/urtuba/demo2", "", "", http.StatusBadRequest, "Only GitHub", "", ""},
		{"no repository", "bora", "urtuba", "", "", http.StatusBadRequest, "owner/repo", "", ""},
		{"a page inside the repository", "bora", "https://github.com/urtuba/demo/tree/main", "", "", http.StatusBadRequest, "owner/repo", "", ""},
		{"option as owner", "bora", "-x/demo", "", "", http.StatusBadRequest, "owner name", "", ""},
		{"dots as repository", "bora", "urtuba/..", "", "", http.StatusBadRequest, "repository name", "", ""},
		{"option as branch", "bora", "urtuba/five", "--upload-pack=x", "", http.StatusBadRequest, "branch name", "", ""},
		{"dots in branch", "bora", "urtuba/five", "a..b", "", http.StatusBadRequest, "branch name", "", ""},
		{"bad token", "bora", "urtuba/five", "", "ghp_not a token!", http.StatusBadRequest, "does not look like a GitHub token", "", ""},
		{"duplicate", "bora", "urtuba/demo", "", webTestToken, http.StatusConflict, "already has this repository", "", ""},
		{"duplicate in another case", "bora", "https://github.com/URTUBA/Demo", "", "", http.StatusConflict, "already has", "", ""},
		{"asker", "asli", "urtuba/six", "", "", http.StatusForbidden, "", "", ""},
		{"member without permissions", "nur", "urtuba/six", "", "", http.StatusForbidden, "", "", ""},
		{"outsider", "otto", "urtuba/six", "", "", http.StatusNotFound, "", "", ""},
	}
	app := newResourceApp(t)
	cookies := map[string]*http.Cookie{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case starts with only the fixture's resource.
			t.Cleanup(func() {
				list, _ := app.store.Resources(context.Background(), app.project.ID)
				for _, r := range list {
					if r.ID != app.res.ID {
						app.store.DeleteResource(context.Background(), app.project.ID, r.ID)
					}
				}
			})
			if cookies[tt.username] == nil {
				cookies[tt.username] = app.login(tt.username)
			}
			form := url.Values{"repo": {tt.repo}, "branch": {tt.branch}, "token": {tt.token}}
			rec := app.post(projectPath(app.project.ID)+"/resources", form, cookies[tt.username])
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tt.wantStatus, rec.Body)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.wantBody) {
				t.Errorf("body does not contain %q", tt.wantBody)
			}
			// A form sent back after an error holds what was typed, but
			// never the token.
			if tt.token != "" && strings.Contains(body, tt.token) {
				t.Error("the page sent the token back")
			}
			if rec.Code == http.StatusBadRequest && !strings.Contains(body, `value="`+tt.repo+`"`) {
				t.Errorf("the form lost the repository typed")
			}

			list, err := app.store.Resources(context.Background(), app.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantStatus != http.StatusSeeOther {
				if len(list) != 1 {
					t.Errorf("resources = %d, want only the fixture's", len(list))
				}
				return
			}
			if got, want := rec.Header().Get("Location"), projectPath(app.project.ID)+"?notice=added#resources"; got != want {
				t.Errorf("location = %q, want %q", got, want)
			}
			var added *store.Resource
			for i := range list {
				if list[i].ID != app.res.ID {
					added = &list[i]
				}
			}
			if added == nil {
				t.Fatal("no resource added")
			}
			if added.Owner != tt.wantOwner || added.Repo != tt.wantRepo || added.Branch != tt.branch {
				t.Errorf("added %s/%s branch %q, want %s/%s branch %q",
					added.Owner, added.Repo, added.Branch, tt.wantOwner, tt.wantRepo, tt.branch)
			}
			if added.HasToken != (tt.token != "") {
				t.Errorf("HasToken = %v, want %v", added.HasToken, tt.token != "")
			}
			if added.OpenJob != "queued" {
				t.Errorf("OpenJob = %q, want the first sync queued", added.OpenJob)
			}
		})
	}
}

func TestSyncNow(t *testing.T) {
	app := newResourceApp(t)
	path := projectPath(app.project.ID)
	syncPath := resourcePath(app.project.ID, app.res.ID) + "/sync"
	bora := app.login("bora")

	// The first sync from adding is still queued.
	rec := app.post(syncPath, nil, bora)
	if got, want := rec.Header().Get("Location"), path+"?notice=open#resources"; rec.Code != http.StatusSeeOther || got != want {
		t.Fatalf("status = %d, location = %q, want 303 to %q", rec.Code, got, want)
	}
	checkBody(t, app.get(path+"?notice=open", bora).Body.String(), []string{"Sync already queued."}, nil)

	app.finishJob(t)
	rec = app.post(syncPath, nil, bora)
	if got, want := rec.Header().Get("Location"), path+"?notice=queued#resources"; got != want {
		t.Fatalf("location = %q, want %q", got, want)
	}
	checkBody(t, app.get(path+"?notice=queued", bora).Body.String(), []string{"Sync queued."}, nil)
	if got := app.load(t).OpenJob; got != "queued" {
		t.Errorf("OpenJob = %q, want queued", got)
	}

	// Only known notices show; the URL cannot put text on the page.
	checkBody(t, app.get(path+"?notice=%3Cb%3Ehi", bora).Body.String(), nil, []string{"<b>hi"})

	otherPath := resourcePath(app.project.ID, app.other.ID) + "/sync"
	for _, tt := range []struct {
		name     string
		username string
		path     string
		want     int
	}{
		{"asker", "asli", syncPath, http.StatusForbidden},
		{"member without permissions", "nur", syncPath, http.StatusForbidden},
		{"outsider", "otto", syncPath, http.StatusNotFound},
		{"another project's resource", "bora", otherPath, http.StatusNotFound},
		{"another project's URL", "bora", resourcePath(app.other.ProjectID, app.other.ID) + "/sync", http.StatusNotFound},
		{"bad id", "bora", path + "/resources/abc/sync", http.StatusNotFound},
		{"missing resource", "bora", path + "/resources/999/sync", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if rec := app.post(tt.path, nil, app.login(tt.username)); rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
	// The other project's resource got no new job.
	other, err := app.store.Resource(context.Background(), app.other.ProjectID, app.other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.OpenJob != "queued" {
		t.Errorf("other resource OpenJob = %q, want its first sync only", other.OpenJob)
	}
}

func TestResourceTokenPage(t *testing.T) {
	app := newResourceApp(t)
	page := resourcePath(app.project.ID, app.res.ID)
	bora := app.login("bora")

	rec := app.get(page, bora)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	checkBody(t, rec.Body.String(),
		[]string{"urtuba/demo", "Token set <span class=\"mono\">····wxyz</span>", `type="password" autocomplete="off"`, "Replace token", "Clear token"}, nil)

	// Replace.
	const newToken = "ghp_0000000000000000000000000000000000ab"
	rec = app.post(page+"/token", url.Values{"token": {newToken}}, bora)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page+"?saved=set" {
		t.Fatalf("replace: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := app.token(t); got != newToken {
		t.Errorf("token = %q, want the new one", got)
	}
	checkBody(t, app.get(page+"?saved=set", bora).Body.String(), []string{"Token saved.", "····00ab"}, []string{newToken})

	// Bad input changes nothing.
	for _, bad := range []string{"", "  ", "not a token", "ghp_short"} {
		rec := app.post(page+"/token", url.Values{"token": {bad}}, bora)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("token %q: status = %d, want 400", bad, rec.Code)
		}
	}
	if got := app.token(t); got != newToken {
		t.Errorf("token after bad input = %q, want it unchanged", got)
	}

	// Clear.
	rec = app.post(page+"/token/clear", nil, bora)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page+"?saved=cleared" {
		t.Fatalf("clear: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := app.token(t); got != "" {
		t.Errorf("token after clear = %q, want none", got)
	}
	checkBody(t, app.get(page+"?saved=cleared", bora).Body.String(),
		[]string{"Token cleared.", "No token.", "Set token"}, []string{"Clear token", "····"})

	// Set again, as the owner.
	if rec := app.post(page+"/token", url.Values{"token": {webTestToken}}, app.login("samed")); rec.Code != http.StatusSeeOther {
		t.Errorf("owner sets the token: status = %d, want 303", rec.Code)
	}

	otherPage := resourcePath(app.project.ID, app.other.ID)
	for _, tt := range []struct {
		name     string
		username string
		method   string
		path     string
		want     int
	}{
		{"asker opens", "asli", http.MethodGet, page, http.StatusForbidden},
		{"asker sets", "asli", http.MethodPost, page + "/token", http.StatusForbidden},
		{"asker clears", "asli", http.MethodPost, page + "/token/clear", http.StatusForbidden},
		{"member without permissions", "nur", http.MethodPost, page + "/token", http.StatusForbidden},
		{"outsider opens", "otto", http.MethodGet, page, http.StatusNotFound},
		{"outsider sets", "otto", http.MethodPost, page + "/token", http.StatusNotFound},
		{"another project's resource", "bora", http.MethodPost, otherPage + "/token", http.StatusNotFound},
		{"another project's resource, clear", "bora", http.MethodPost, otherPage + "/token/clear", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := app.login(tt.username)
			var rec interface{ Result() *http.Response }
			if tt.method == http.MethodGet {
				rec = app.get(tt.path, c)
			} else {
				rec = app.post(tt.path, url.Values{"token": {"ghp_" + strings.Repeat("9", 36)}}, c)
			}
			if got := rec.Result().StatusCode; got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
	if got := app.token(t); got != webTestToken {
		t.Errorf("token = %q, want it unchanged by refused requests", got)
	}
}

func TestRemoveResource(t *testing.T) {
	ctx := context.Background()
	app := newResourceApp(t)
	remove := resourcePath(app.project.ID, app.res.ID) + "/remove"

	for _, tt := range []struct {
		name     string
		username string
		path     string
		want     int
	}{
		{"asker", "asli", remove, http.StatusForbidden},
		{"member without permissions", "nur", remove, http.StatusForbidden},
		{"outsider", "otto", remove, http.StatusNotFound},
		{"another project's resource", "bora", resourcePath(app.project.ID, app.other.ID) + "/remove", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if rec := app.post(tt.path, nil, app.login(tt.username)); rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
	app.load(t) // still there
	if _, err := app.store.Resource(ctx, app.other.ProjectID, app.other.ID); err != nil {
		t.Errorf("other project's resource: %v", err)
	}

	bora := app.login("bora")
	rec := app.post(remove, nil, bora)
	path := projectPath(app.project.ID)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"?notice=removed#resources" {
		t.Fatalf("remove: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := app.store.Resource(ctx, app.project.ID, app.res.ID); err == nil {
		t.Error("the resource is still there")
	}
	checkBody(t, app.get(path+"?notice=removed", bora).Body.String(),
		[]string{"Repository removed.", "No repositories yet."}, []string{"urtuba/demo"})
}

func TestSyncSettings(t *testing.T) {
	ctx := context.Background()
	app := newResourceApp(t)
	settings := projectPath(app.project.ID) + "/settings"
	ada := app.login("ada")

	checkBody(t, app.get(settings, ada).Body.String(),
		[]string{`<option value="6" selected>`, `name="max_file_mb" type="number" min="1" max="100" step="1" value="5"`}, nil)

	tests := []struct {
		name         string
		username     string
		interval     string
		mb           string
		wantStatus   int
		wantInterval int
		wantMB       int64
	}{
		{"every day, 10 MB", "ada", "24", "10", http.StatusSeeOther, 24, 10},
		{"off", "ada", "0", "1", http.StatusSeeOther, 0, 1},
		{"owner", "samed", "1", "100", http.StatusSeeOther, 1, 100},
		{"interval not in the list", "ada", "2", "5", http.StatusBadRequest, 6, 5},
		{"interval not a number", "ada", "off", "5", http.StatusBadRequest, 6, 5},
		{"0 MB", "ada", "6", "0", http.StatusBadRequest, 6, 5},
		{"101 MB", "ada", "6", "101", http.StatusBadRequest, 6, 5},
		{"part of a MB", "ada", "6", "1.5", http.StatusBadRequest, 6, 5},
		{"manager", "mert", "24", "10", http.StatusForbidden, 6, 5},
		{"asker", "asli", "24", "10", http.StatusForbidden, 6, 5},
		{"outsider", "otto", "24", "10", http.StatusNotFound, 6, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := app.store.SetSyncSettings(ctx, app.project.ID, 6, 5<<20); err != nil {
				t.Fatal(err)
			}
			form := url.Values{"sync_interval": {tt.interval}, "max_file_mb": {tt.mb}}
			rec := app.post(settings+"/sync", form, app.login(tt.username))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tt.wantStatus, rec.Body)
			}
			if rec.Code == http.StatusSeeOther && rec.Header().Get("Location") != settings+"?saved=sync" {
				t.Errorf("location = %q", rec.Header().Get("Location"))
			}
			p, err := app.store.Project(ctx, app.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if p.SyncIntervalHours != tt.wantInterval || p.MaxFileBytes != tt.wantMB<<20 {
				t.Errorf("settings = %d hours, %d bytes, want %d hours, %d MB", p.SyncIntervalHours, p.MaxFileBytes, tt.wantInterval, tt.wantMB)
			}
		})
	}

	// After a save the page shows the new values.
	if rec := app.post(settings+"/sync", url.Values{"sync_interval": {"24"}, "max_file_mb": {"12"}}, ada); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d", rec.Code)
	}
	checkBody(t, app.get(settings+"?saved=sync", ada).Body.String(),
		[]string{"Saved.", `<option value="24" selected>`, `value="12"`}, []string{`<option value="6" selected>`})
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"urtuba/repo-speaks", "urtuba", "repo-speaks", true},
		{"github.com/urtuba/repo-speaks", "urtuba", "repo-speaks", true},
		{"https://github.com/urtuba/repo-speaks", "urtuba", "repo-speaks", true},
		{"https://github.com/urtuba/repo-speaks.git", "urtuba", "repo-speaks", true},
		{"https://github.com/urtuba/repo-speaks/", "urtuba", "repo-speaks", true},
		{"  urtuba/repo.speaks_2  ", "urtuba", "repo.speaks_2", true},
		{"", "", "", false},
		{"urtuba", "", "", false},
		{"urtuba/", "", "", false},
		{"/repo", "", "", false},
		{"a/b/c", "", "", false},
		{"ssh://git@github.com/urtuba/x", "", "", false},
		{"git@github.com:urtuba/x.git", "", "", false},
		{"https://github.com.evil.example/urtuba/x", "", "", false},
		{"https://evil.example/github.com/urtuba/x", "", "", false},
		{"-urtuba/x", "", "", false},
		{"urtuba/-x", "", "", false},
		{"urtuba/..", "", "", false},
		{"urtuba/a b", "", "", false},
		{"urtuba/x?y", "", "", false},
		{"urtuba/" + strings.Repeat("x", 101), "", "", false},
	}
	for _, tt := range tests {
		owner, repo, msg := parseRepo(tt.in)
		if (msg == "") != tt.ok || owner != tt.owner || repo != tt.repo {
			t.Errorf("parseRepo(%q) = %q, %q, %q, want %q, %q, ok %v", tt.in, owner, repo, msg, tt.owner, tt.repo, tt.ok)
		}
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "just now"},
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{59 * time.Minute, "59 minutes ago"},
		{time.Hour, "1 hour ago"},
		{23 * time.Hour, "23 hours ago"},
		{24 * time.Hour, "1 day ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, tt := range tests {
		if got := ago(now.Add(-tt.d), now); got != tt.want {
			t.Errorf("ago(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
