package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
)

const (
	modelKey    = "sk-or-v1-0000secretvalue0000-k3y9"
	newModelKey = "sk-or-v1-1111othersecret1111-n3w2"
	glm         = "z-ai/glm-5.3-flash"
)

// fakeLLMHost is an OpenAI-compatible host. It serves the same models
// under any path prefix, so a test can change a model's base URL and
// stay on this host. Paths under /missing get 404. The chat call passes only with modelKey or
// newModelKey; otherwise it answers 401 and echoes the key it got, as a
// careless host might.
type fakeLLMHost struct {
	mu    sync.Mutex
	calls int
	auth  []string // the Authorization header of each request
}

func (h *fakeLLMHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	auth := r.Header.Get("Authorization")
	h.auth = append(h.auth, auth)
	switch {
	case strings.HasPrefix(r.URL.Path, "/missing/"):
		http.NotFound(w, r)
	case strings.HasSuffix(r.URL.Path, "/models"):
		io.WriteString(w, `{"data": [
			{"id": "`+glm+`", "supported_parameters": ["tools", "max_tokens"]},
			{"id": "old/no-tools", "supported_parameters": ["max_tokens"]}
		]}`)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		if auth != "Bearer "+modelKey && auth != "Bearer "+newModelKey {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error": {"message": "No auth credentials found for `+auth+`"}}`)
			return
		}
		io.WriteString(w, `{"choices": [{"message": {"tool_calls": [{"type": "function",
			"function": {"name": "get_time", "arguments": "{}"}}]}, "finish_reason": "tool_calls"}]}`)
	default:
		http.NotFound(w, r)
	}
}

// lastAuth returns the Authorization header of the last request.
func (h *fakeLLMHost) lastAuth() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.auth) == 0 {
		return ""
	}
	return h.auth[len(h.auth)-1]
}

// modelApp is the member fixture (owner samed, manager mert, builder
// bora, admin ada, asker asli) plus otto, who is in no project. The
// project has the model "Fast" on the fake host, with modelKey. Another
// project has a model of its own. The app allows private addresses, so
// it can reach the fake host on 127.0.0.1.
type modelApp struct {
	memberApp
	host     *fakeLLMHost
	base     string // the fake host's base URL
	model    store.Model
	other    store.Model
	otherPID int64
}

func newModelApp(t *testing.T) modelApp {
	t.Helper()
	ctx := context.Background()
	app := modelApp{memberApp: newMemberApp(t), host: &fakeLLMHost{}}
	app.createUser("otto", false)
	srv := httptest.NewServer(app.host)
	t.Cleanup(srv.Close)
	app.base = srv.URL + "/v1"
	app.useLLM(llm.New(true))

	var err error
	app.model, err = app.store.AddModel(ctx, store.NewModel{
		ProjectID: app.project.ID, Name: "Fast", BaseURL: app.base, ModelID: glm, APIKey: modelKey,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	billing, err := app.store.CreateProject(ctx, "Billing", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	app.otherPID = billing.ID
	app.other, err = app.store.AddModel(ctx, store.NewModel{
		ProjectID: billing.ID, Name: "Billing model", BaseURL: app.base, ModelID: glm, APIKey: modelKey,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// key returns the stored key of a model of the fixture's project.
func (a modelApp) key(t *testing.T, id int64) string {
	t.Helper()
	e, err := a.store.ModelEndpoint(context.Background(), a.project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return e.APIKey
}

// checkNoKey checks that body holds every want, and neither key.
func checkNoKey(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body does not contain %q", s)
		}
	}
	for _, key := range []string{modelKey, newModelKey, "secret"} {
		if strings.Contains(body, key) {
			t.Errorf("body contains %q", key)
		}
	}
}

// fillModelForm is a filled add or edit form.
func fillModelForm(name, base, id, key string) url.Values {
	return url.Values{"name": {name}, "base_url": {base}, "model_id": {id}, "api_key": {key}}
}

func TestModelsSection(t *testing.T) {
	app := newModelApp(t)
	settings := projectPath(app.project.ID) + "/settings"

	rec := app.get(settings, app.login("ada"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	checkNoKey(t, rec.Body.String(),
		"<h2>Models</h2>",
		"<td>Fast</td>",
		`<td class="mono">`+glm+`</td>`,
		`<span class="mono">····k3y9</span>`,
		`hx-post="`+modelPath(app.project.ID, app.model.ID)+`/test" hx-target="#model-test"`,
		`<option value="0" selected>None</option>`,
		`value="`+llm.DefaultBaseURL+`"`,
		`name="api_key" type="password" autocomplete="off"`,
	)
	if strings.Contains(rec.Body.String(), "Billing model") {
		t.Error("the page lists another project's model")
	}
}

func TestModelPermissions(t *testing.T) {
	app := newModelApp(t)
	settings := projectPath(app.project.ID) + "/settings"
	page := modelPath(app.project.ID, app.model.ID)
	otherHere := modelPath(app.project.ID, app.other.ID)
	otherThere := modelPath(app.otherPID, app.other.ID)

	tests := []struct {
		name     string
		username string
		method   string
		path     string
		want     int
	}{
		{"manager opens settings", "mert", http.MethodGet, settings, http.StatusForbidden},
		{"builder adds", "bora", http.MethodPost, projectPath(app.project.ID) + "/models", http.StatusForbidden},
		{"asker adds", "asli", http.MethodPost, projectPath(app.project.ID) + "/models", http.StatusForbidden},
		{"outsider adds", "otto", http.MethodPost, projectPath(app.project.ID) + "/models", http.StatusNotFound},
		{"manager opens a model", "mert", http.MethodGet, page, http.StatusForbidden},
		{"manager edits", "mert", http.MethodPost, page, http.StatusForbidden},
		{"manager tests", "mert", http.MethodPost, page + "/test", http.StatusForbidden},
		{"manager deletes", "mert", http.MethodPost, page + "/delete", http.StatusForbidden},
		{"manager sets the default", "mert", http.MethodPost, settings + "/default-model", http.StatusForbidden},
		{"outsider opens a model", "otto", http.MethodGet, page, http.StatusNotFound},
		{"outsider tests", "otto", http.MethodPost, page + "/test", http.StatusNotFound},
		{"outsider deletes", "otto", http.MethodPost, page + "/delete", http.StatusNotFound},
		{"another project's model, opened", "ada", http.MethodGet, otherHere, http.StatusNotFound},
		{"another project's model, edited", "ada", http.MethodPost, otherHere, http.StatusNotFound},
		{"another project's model, tested", "ada", http.MethodPost, otherHere + "/test", http.StatusNotFound},
		{"another project's model, deleted", "ada", http.MethodPost, otherHere + "/delete", http.StatusNotFound},
		{"another project's URL", "ada", http.MethodGet, otherThere, http.StatusNotFound},
		{"bad id", "ada", http.MethodGet, projectPath(app.project.ID) + "/models/abc", http.StatusNotFound},
		{"missing model", "ada", http.MethodGet, projectPath(app.project.ID) + "/models/999", http.StatusNotFound},
		{"admin opens a model", "ada", http.MethodGet, page, http.StatusOK},
		{"owner opens a model", "samed", http.MethodGet, page, http.StatusOK},
	}
	calls := app.host.calls
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := app.login(tt.username)
			var rec *httptest.ResponseRecorder
			if tt.method == http.MethodGet {
				rec = app.get(tt.path, c)
			} else {
				rec = app.post(tt.path, fillModelForm("Taken", "https://evil.example/v1", glm, newModelKey), c)
			}
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			checkNoKey(t, rec.Body.String())
		})
	}
	if app.host.calls != calls {
		t.Errorf("refused requests called the host %d times", app.host.calls-calls)
	}
	// Nothing changed.
	other, err := app.store.Model(context.Background(), app.otherPID, app.other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Name != "Billing model" {
		t.Errorf("other project's model = %+v, want it unchanged", other)
	}
	list, _ := app.store.Models(context.Background(), app.project.ID)
	if len(list) != 1 || list[0].Name != "Fast" || app.key(t, app.model.ID) != modelKey {
		t.Errorf("models = %+v, want only Fast, unchanged", list)
	}
}

func TestAddModel(t *testing.T) {
	app := newModelApp(t)
	tests := []struct {
		name       string
		form       url.Values
		wantStatus int
		wantBody   string
	}{
		{"with a key", fillModelForm("Quick", app.base, glm, newModelKey), http.StatusSeeOther, ""},
		{"no key, trailing slash", fillModelForm("Keyless", app.base+"/", glm, ""), http.StatusSeeOther, ""},
		{"unknown model ID", fillModelForm("Typo", app.base, "z-ai/glm-5.3", newModelKey), http.StatusBadRequest,
			`Not saved. Checking the model ID failed: the host does not list a model with the ID &#34;z-ai/glm-5.3&#34;`},
		{"model without tools", fillModelForm("Old", app.base, "old/no-tools", newModelKey), http.StatusBadRequest,
			"does not support tool use, which agents need"},
		{"host cannot be reached", fillModelForm("Down", "http://127.0.0.1:1/v1", glm, newModelKey), http.StatusBadRequest,
			"Checking the model ID failed: the host refused the connection"},
		{"wrong path on the host", fillModelForm("Path", strings.TrimSuffix(app.base, "/v1")+"/missing", glm, newModelKey), http.StatusBadRequest,
			"the host answered 404 Not Found"},
		{"duplicate name", fillModelForm("fast", app.base, glm, newModelKey), http.StatusConflict,
			"already has a model with this name"},
		{"no name", fillModelForm(" ", app.base, glm, ""), http.StatusBadRequest, "A model name must be"},
		{"no model ID", fillModelForm("X", app.base, "", ""), http.StatusBadRequest, "A model ID must be"},
		{"space in model ID", fillModelForm("X", app.base, "z-ai/glm 5", ""), http.StatusBadRequest, "may not contain spaces"},
		{"user info in URL", fillModelForm("X", "https://user:pass@openrouter.ai/api/v1", glm, ""), http.StatusBadRequest,
			"may not hold a user name or password"},
		{"query in URL", fillModelForm("X", "https://openrouter.ai/api/v1?key=x", glm, ""), http.StatusBadRequest, "may not have a query"},
		{"not http", fillModelForm("X", "file:///etc/passwd", glm, ""), http.StatusBadRequest, "must start with https:// or http://"},
		{"space in key", fillModelForm("X", app.base, glm, "sk or v1"), http.StatusBadRequest, "does not look like an API key"},
	}
	ada := app.login("ada")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := app.store.Models(context.Background(), app.project.ID)
			rec := app.post(projectPath(app.project.ID)+"/models", tt.form, ada)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tt.wantStatus, rec.Body)
			}
			body := rec.Body.String()
			checkNoKey(t, body, tt.wantBody)
			after, _ := app.store.Models(context.Background(), app.project.ID)
			if rec.Code != http.StatusSeeOther {
				if len(after) != len(before) {
					t.Errorf("models = %d, want %d", len(after), len(before))
				}
				// The form keeps what was typed, except the key.
				if !strings.Contains(body, `value="`+tt.form.Get("name")+`"`) {
					t.Error("the form lost the name typed")
				}
				return
			}
			if got, want := rec.Header().Get("Location"), projectPath(app.project.ID)+"/settings?saved=model-added#models"; got != want {
				t.Errorf("location = %q, want %q", got, want)
			}
			if len(after) != len(before)+1 {
				t.Fatalf("models = %d, want %d", len(after), len(before)+1)
			}
		})
	}

	list, err := app.store.Models(context.Background(), app.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]store.Model{}
	for _, m := range list {
		byName[m.Name] = m
	}
	if m := byName["Quick"]; m.BaseURL != app.base || m.ModelID != glm || m.KeyHint != "n3w2" || app.key(t, m.ID) != newModelKey {
		t.Errorf("Quick = %+v", m)
	}
	if m := byName["Keyless"]; m.BaseURL != app.base || m.HasKey {
		t.Errorf("Keyless = %+v, want the URL without the slash and no key", m)
	}
	checkNoKey(t, app.get(projectPath(app.project.ID)+"/settings?saved=model-added", ada).Body.String(),
		"Model added.", "<td>Quick</td>", "····n3w2")
}

// TestPrivateHostRefused uses the app as the server runs by default:
// with the guard on, a model on 127.0.0.1 is refused, by address or by
// name, and the host gets no request.
func TestPrivateHostRefused(t *testing.T) {
	app := newModelApp(t)
	app.useLLM(llm.New(false))
	ada := app.login("ada")
	calls := app.host.calls

	for _, base := range []string{app.base, strings.Replace(app.base, "127.0.0.1", "localhost", 1)} {
		rec := app.post(projectPath(app.project.ID)+"/models", fillModelForm("Local", base, glm, newModelKey), ada)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", base, rec.Code)
		}
		// localhost may resolve to ::1 first.
		checkNoKey(t, rec.Body.String(), "the host&#39;s address ", " is private, and this server blocks private addresses")
	}
	// The Test button of a saved model is refused the same way.
	rec := app.post(modelPath(app.project.ID, app.model.ID)+"/test", nil, ada)
	checkNoKey(t, rec.Body.String(), "is private, and this server blocks private addresses")
	if app.host.calls != calls {
		t.Errorf("the host got %d requests, want none", app.host.calls-calls)
	}
}

func TestEditModel(t *testing.T) {
	ctx := context.Background()
	app := newModelApp(t)
	page := modelPath(app.project.ID, app.model.ID)
	ada := app.login("ada")
	v2 := strings.TrimSuffix(app.base, "/v1") + "/v2"

	checkNoKey(t, app.get(page, ada).Body.String(),
		`value="Fast"`, `value="`+app.base+`"`, `value="`+glm+`"`,
		"New API key <span class=\"muted\">(empty keeps ····k3y9)</span>",
		`name="api_key" type="password" autocomplete="off"`)

	// A rename with an empty key keeps the key, and the check uses it.
	rec := app.post(page, fillModelForm("Faster", app.base, glm, ""), ada)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page+"?saved=1" {
		t.Fatalf("rename: status = %d, location = %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got := app.host.lastAuth(); got != "Bearer "+modelKey {
		t.Errorf("the check sent %q, want the stored key", got)
	}
	if m, _ := app.store.Model(ctx, app.project.ID, app.model.ID); m.Name != "Faster" || app.key(t, m.ID) != modelKey {
		t.Errorf("after rename: %+v", m)
	}
	checkNoKey(t, app.get(page+"?saved=1", ada).Body.String(), "Saved.", "····k3y9")

	// A new URL with an empty key clears the key, and the check sends none.
	rec = app.post(page, fillModelForm("Faster", v2+"/", glm, ""), ada)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page+"?saved=cleared" {
		t.Fatalf("new URL: status = %d, location = %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got := app.host.lastAuth(); got != "" {
		t.Errorf("the check for a new URL sent %q, want no key", got)
	}
	m, _ := app.store.Model(ctx, app.project.ID, app.model.ID)
	if m.BaseURL != v2 || m.HasKey || app.key(t, m.ID) != "" {
		t.Errorf("after the URL change: %+v, want %s with no key", m, v2)
	}
	checkNoKey(t, app.get(page+"?saved=cleared", ada).Body.String(),
		"the key was cleared", "API key <span class=\"muted\">(none set)</span>")

	// A new URL with a new key sets the key.
	rec = app.post(page, fillModelForm("Faster", app.base, glm, newModelKey), ada)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page+"?saved=1" {
		t.Fatalf("new URL and key: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if app.key(t, app.model.ID) != newModelKey {
		t.Error("the new key was not saved")
	}

	// Refused edits change nothing, and the form keeps what was typed.
	if _, err := app.store.AddModel(ctx, store.NewModel{ProjectID: app.project.ID, Name: "Slow", BaseURL: app.base, ModelID: glm}, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		form   url.Values
		status int
		want   string
	}{
		{"unknown model ID", fillModelForm("Faster", v2, "nope/nope", modelKey), http.StatusBadRequest, "does not list a model"},
		{"model without tools", fillModelForm("Faster", app.base, "old/no-tools", ""), http.StatusBadRequest, "does not support tool use"},
		{"taken name", fillModelForm("SLOW", app.base, glm, ""), http.StatusConflict, "already has a model with this name"},
		{"bad URL", fillModelForm("Faster", "openrouter.ai", glm, ""), http.StatusBadRequest, "full address"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.post(page, tt.form, ada)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			checkNoKey(t, rec.Body.String(), tt.want, `value="`+tt.form.Get("model_id")+`"`)
			m, _ := app.store.Model(ctx, app.project.ID, app.model.ID)
			if m.Name != "Faster" || m.BaseURL != app.base || m.ModelID != glm || app.key(t, m.ID) != newModelKey {
				t.Errorf("model = %+v, want it unchanged", m)
			}
		})
	}
}

func TestTestModel(t *testing.T) {
	ctx := context.Background()
	app := newModelApp(t)
	test := modelPath(app.project.ID, app.model.ID) + "/test"
	ada := app.login("ada")

	htmx := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, test, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("HX-Request", "true")
		req.AddCookie(ada)
		rec := httptest.NewRecorder()
		app.h.ServeHTTP(rec, req)
		return rec
	}

	rec := htmx()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != `<p class="alert success" role="status">Test of Fast passed: the model called the tool.</p>` {
		t.Errorf("fragment = %q", body)
	}

	// A key the host refuses. The host echoes it; the page must not.
	const badKey = "sk-or-v1-9999wrongsecret9999-b4d1"
	if err := app.store.UpdateModel(ctx, app.project.ID, app.model.ID, store.ModelChange{
		Name: "Fast", BaseURL: app.base, ModelID: glm, APIKey: badKey,
	}, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	rec = htmx()
	body = rec.Body.String()
	checkNoKey(t, body, "Test of Fast failed: the host answered 401 Unauthorized: No auth credentials found for Bearer [hidden]")
	if strings.Contains(body, badKey) || strings.Contains(body, "<html") {
		t.Errorf("fragment = %q, want the error alone, without the key", body)
	}

	// Without htmx, the result comes on the model page.
	rec = app.post(test, nil, ada)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	checkNoKey(t, rec.Body.String(), "<html", "Test of Fast failed")
	if strings.Contains(rec.Body.String(), badKey) {
		t.Error("the page holds the key")
	}
}

func TestDefaultModel(t *testing.T) {
	ctx := context.Background()
	app := newModelApp(t)
	settings := projectPath(app.project.ID) + "/settings"
	del := modelPath(app.project.ID, app.model.ID) + "/delete"
	ada := app.login("ada")
	id := func(n int64) url.Values { return url.Values{"model": {itoa(n)}} }

	rec := app.post(settings+"/default-model", id(app.model.ID), ada)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != settings+"?saved=default#models" {
		t.Fatalf("set default: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	checkNoKey(t, app.get(settings+"?saved=default", ada).Body.String(),
		"Default model saved.",
		`<td>Fast <span class="badge">default</span></td>`,
		`<option value="`+itoa(app.model.ID)+`" selected>Fast</option>`)

	// The default cannot be deleted.
	rec = app.post(del, nil, ada)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete the default: status = %d, want 409", rec.Code)
	}
	checkNoKey(t, rec.Body.String(), "Not deleted: Fast is the default model. Choose another default model first.")
	if _, err := app.store.Model(ctx, app.project.ID, app.model.ID); err != nil {
		t.Fatalf("the default model is gone: %v", err)
	}

	// Refused values change nothing.
	for _, v := range []string{"abc", "-1", itoa(app.other.ID), "999", ""} {
		rec := app.post(settings+"/default-model", url.Values{"model": {v}}, ada)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("default %q: status = %d, want 400", v, rec.Code)
		}
	}
	if p, _ := app.store.Project(ctx, app.project.ID); p.DefaultModelID != app.model.ID {
		t.Errorf("default = %d, want %d", p.DefaultModelID, app.model.ID)
	}
	if p, _ := app.store.Project(ctx, app.otherPID); p.DefaultModelID != 0 {
		t.Errorf("other project's default = %d, want none", p.DefaultModelID)
	}

	// With no default, the model can go.
	if rec := app.post(settings+"/default-model", id(0), ada); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear default: status = %d", rec.Code)
	}
	rec = app.post(del, nil, ada)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != settings+"?saved=model-deleted#models" {
		t.Fatalf("delete: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	checkNoKey(t, app.get(settings+"?saved=model-deleted", ada).Body.String(), "Model deleted.", "No models yet.")
}

// TestModelKeyNeverLogged runs every model action, the failing ones
// too, and checks that no log line holds a key.
func TestModelKeyNeverLogged(t *testing.T) {
	app := newModelApp(t)
	ada := app.login("ada")
	page := modelPath(app.project.ID, app.model.ID)
	app.post(projectPath(app.project.ID)+"/models", fillModelForm("Quick", app.base, glm, newModelKey), ada)
	app.post(projectPath(app.project.ID)+"/models", fillModelForm("Typo", app.base, "nope", newModelKey), ada)
	app.post(projectPath(app.project.ID)+"/models", fillModelForm("Down", "http://127.0.0.1:1/v1", glm, newModelKey), ada)
	app.post(page, fillModelForm("Fast", app.base, glm, newModelKey), ada)
	app.post(page, fillModelForm("Fast", app.base+"x", glm, ""), ada)
	app.post(page+"/test", nil, ada)
	app.useLLM(llm.New(false))
	app.post(page+"/test", nil, ada)
	app.post(projectPath(app.project.ID)+"/models", fillModelForm("Local", app.base, glm, newModelKey), ada)

	logs := app.logs.String()
	if strings.Count(logs, `route="POST /projects/{id}/models`) < 8 {
		t.Fatalf("the log shows no model requests:\n%s", logs)
	}
	for _, key := range []string{modelKey, newModelKey, "secret"} {
		if strings.Contains(logs, key) {
			t.Errorf("the log holds %q:\n%s", key, logs)
		}
	}
}

// itoa writes an ID for a form or a page.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
