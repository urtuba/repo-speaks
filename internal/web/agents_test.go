package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/store"
)

// agentPrompt is the fixture agent's instructions. No page for a member
// without agents:edit may hold it.
const agentPrompt = "Answer in three bullet points. Internal note: hidden-prompt-7f3a."

// agentApp is the resource fixture (owner samed, manager mert, builder
// bora, admin ada, asker asli, nur with no permissions, outsider otto,
// and urtuba/demo) plus the model Fast and the chat agent Helper, which
// uses the project's default model. The project has no default model,
// so Helper is not ready. The Billing project has a model, a resource
// and an agent of its own.
type agentApp struct {
	resourceApp
	model      store.Model
	agent      store.Agent
	otherModel store.Model
	otherAgent store.Agent
}

func newAgentApp(t *testing.T) agentApp {
	t.Helper()
	ctx := context.Background()
	app := agentApp{resourceApp: newResourceApp(t)}
	billing := app.other.ProjectID
	var err error
	app.model, err = app.store.AddModel(ctx, store.NewModel{
		ProjectID: app.project.ID, Name: "Fast", BaseURL: "https://openrouter.ai/api/v1", ModelID: glm,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	app.otherModel, err = app.store.AddModel(ctx, store.NewModel{
		ProjectID: billing, Name: "Billing model", BaseURL: "https://openrouter.ai/api/v1", ModelID: glm,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	app.agent, err = app.store.AddAgent(ctx, app.project.ID, store.AgentChat, store.AgentSettings{
		Name: "Helper", Description: "Answers questions about the portal.", Prompt: agentPrompt,
		RateLimitPerHour: 20, AllResources: true,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	app.otherAgent, err = app.store.AddAgent(ctx, billing, store.AgentAnswer, store.AgentSettings{
		Name: "Billing triage", Prompt: "billing-prompt", RateLimitPerHour: 20, AllResources: true,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// loadAgent reads the fixture's agent again.
func (a agentApp) loadAgent(t *testing.T) store.Agent {
	t.Helper()
	got, err := a.store.Agent(context.Background(), a.project.ID, a.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// fillAgentForm is a filled form that passes every check: a chat agent
// with every repository and the project's default model.
func fillAgentForm(name string) url.Values {
	return url.Values{
		"kind":        {store.AgentChat},
		"name":        {name},
		"description": {"Helps the support team."},
		"prompt":      {"Be short."},
		"model":       {"0"},
		"resources":   {"all"},
		"rate_limit":  {"20"},
	}
}

// with returns a copy of form with key set to values.
func with(form url.Values, key string, values ...string) url.Values {
	c := url.Values{}
	for k, v := range form {
		c[k] = v
	}
	c[key] = values
	return c
}

func TestAgentPermissions(t *testing.T) {
	app := newAgentApp(t)
	agents := projectPath(app.project.ID) + "/agents"
	page := agentPath(app.project.ID, app.agent.ID)
	otherHere := agentPath(app.project.ID, app.otherAgent.ID)
	otherThere := agentPath(app.other.ProjectID, app.otherAgent.ID)

	tests := []struct {
		name     string
		username string
		method   string
		path     string
		want     int
	}{
		{"asker opens the new page", "asli", http.MethodGet, agents + "/new", http.StatusForbidden},
		{"asker creates", "asli", http.MethodPost, agents, http.StatusForbidden},
		{"asker opens an agent", "asli", http.MethodGet, page, http.StatusForbidden},
		{"asker edits", "asli", http.MethodPost, page, http.StatusForbidden},
		{"asker deletes", "asli", http.MethodPost, page + "/delete", http.StatusForbidden},
		{"member without permissions opens an agent", "nur", http.MethodGet, page, http.StatusForbidden},
		{"outsider opens the new page", "otto", http.MethodGet, agents + "/new", http.StatusNotFound},
		{"outsider creates", "otto", http.MethodPost, agents, http.StatusNotFound},
		{"outsider opens an agent", "otto", http.MethodGet, page, http.StatusNotFound},
		{"outsider edits", "otto", http.MethodPost, page, http.StatusNotFound},
		{"outsider deletes", "otto", http.MethodPost, page + "/delete", http.StatusNotFound},
		{"another project's agent, opened", "bora", http.MethodGet, otherHere, http.StatusNotFound},
		{"another project's agent, edited", "bora", http.MethodPost, otherHere, http.StatusNotFound},
		{"another project's agent, deleted", "bora", http.MethodPost, otherHere + "/delete", http.StatusNotFound},
		{"another project's URL", "ada", http.MethodGet, otherThere, http.StatusNotFound},
		{"another project's URL, deleted", "ada", http.MethodPost, otherThere + "/delete", http.StatusNotFound},
		{"bad id", "bora", http.MethodGet, agents + "/abc", http.StatusNotFound},
		{"missing agent", "bora", http.MethodGet, agents + "/999", http.StatusNotFound},
		{"builder opens the new page", "bora", http.MethodGet, agents + "/new", http.StatusOK},
		{"builder opens an agent", "bora", http.MethodGet, page, http.StatusOK},
		{"manager opens an agent", "mert", http.MethodGet, page, http.StatusOK},
		{"admin opens an agent", "ada", http.MethodGet, page, http.StatusOK},
		{"owner opens an agent", "samed", http.MethodGet, page, http.StatusOK},
		{"owner opens another project's agent", "samed", http.MethodGet, otherThere, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := app.login(tt.username)
			var rec *http.Response
			if tt.method == http.MethodGet {
				rec = app.get(tt.path, c).Result()
			} else {
				rec = app.post(tt.path, fillAgentForm("Taken"), c).Result()
			}
			if rec.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", rec.StatusCode, tt.want)
			}
		})
	}

	// Nothing changed.
	if got := app.loadAgent(t); !reflect.DeepEqual(got, app.agent) {
		t.Errorf("agent = %+v, want it unchanged", got)
	}
	for _, pid := range []int64{app.project.ID, app.other.ProjectID} {
		if list, _ := app.store.Agents(context.Background(), pid); len(list) != 1 {
			t.Errorf("project %d has %d agents, want 1", pid, len(list))
		}
	}
}

// TestPromptHiddenWithoutAgentsEdit opens every page a member without
// agents:edit can reach, and checks that none holds the prompt.
func TestPromptHiddenWithoutAgentsEdit(t *testing.T) {
	app := newAgentApp(t)
	pages := []string{
		projectPath(app.project.ID),
		projectPath(app.project.ID) + "/resources",
		projectPath(app.project.ID) + "/settings",
		projectPath(app.project.ID) + "/agents/new",
		agentPath(app.project.ID, app.agent.ID),
		"/",
	}
	for _, username := range []string{"asli", "nur"} {
		c := app.login(username)
		for _, path := range pages {
			rec := app.get(path, c)
			if strings.Contains(rec.Body.String(), "hidden-prompt") {
				t.Errorf("%s, GET %s (status %d): the page holds the prompt", username, path, rec.Code)
			}
		}
	}
	// The project page shows no prompt to anyone, builders included.
	for _, username := range []string{"bora", "samed"} {
		if strings.Contains(app.get(projectPath(app.project.ID), app.login(username)).Body.String(), "hidden-prompt") {
			t.Errorf("%s: the project page holds the prompt", username)
		}
	}
	// The agent page does, for agents:edit.
	if !strings.Contains(app.get(agentPath(app.project.ID, app.agent.ID), app.login("bora")).Body.String(), "hidden-prompt") {
		t.Error("the builder's agent page does not show the prompt")
	}
}

func TestProjectPageListsAgents(t *testing.T) {
	app := newAgentApp(t)
	edit := `href="` + agentPath(app.project.ID, app.agent.ID) + `">Edit</a>`
	newLink := `href="` + projectPath(app.project.ID) + `/agents/new">New agent</a>`
	shared := []string{
		`<h2>Agents</h2>`,
		`<td>Helper <span class="badge warn" title="The agent has no model yet, so it cannot answer.">not ready</span></td>`,
		`<td><span class="badge">chat</span></td>`,
		`<td>Answers questions about the portal.</td>`,
	}

	for _, tt := range []struct {
		username string
		canEdit  bool
	}{
		{"asli", false}, {"nur", false}, {"bora", true}, {"mert", true}, {"ada", true}, {"samed", true},
	} {
		t.Run(tt.username, func(t *testing.T) {
			rec := app.get(projectPath(app.project.ID), app.login(tt.username))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, s := range shared {
				if !strings.Contains(body, s) {
					t.Errorf("page does not contain %q", s)
				}
			}
			for _, s := range []string{edit, newLink} {
				if strings.Contains(body, s) != tt.canEdit {
					t.Errorf("page contains %q: %v, want %v", s, !tt.canEdit, tt.canEdit)
				}
			}
			if strings.Contains(body, "Billing triage") {
				t.Error("the page lists another project's agent")
			}
		})
	}

	// With a default model, the agent is ready.
	if err := app.store.SetDefaultModel(context.Background(), app.project.ID, app.model.ID); err != nil {
		t.Fatal(err)
	}
	body := app.get(projectPath(app.project.ID), app.login("asli")).Body.String()
	if strings.Contains(body, "not ready") || !strings.Contains(body, "<td>Helper</td>") {
		t.Error("with a default model, the page still says not ready")
	}

	// A project with no agents says so.
	if err := app.store.DeleteAgent(context.Background(), app.project.ID, app.agent.ID); err != nil {
		t.Fatal(err)
	}
	body = app.get(projectPath(app.project.ID), app.login("asli")).Body.String()
	if !strings.Contains(body, `<p class="empty">No agents yet.</p>`) {
		t.Error("the page does not say there are no agents")
	}
}

func TestNewAgentPage(t *testing.T) {
	app := newAgentApp(t)
	body := app.get(projectPath(app.project.ID)+"/agents/new", app.login("bora")).Body.String()
	for _, want := range []string{
		`<option value="chat" selected>Chat: a conversation, with follow-up questions</option>`,
		`<option value="answer">Answer: one question, one answer in a set format</option>`,
		`<option value="0" selected>Project default (none set)</option>`,
		`<option value="` + itoa(app.model.ID) + `">Fast</option>`,
		`<input type="radio" name="resources" value="all" checked>`,
		`<input type="checkbox" name="resource" value="` + itoa(app.res.ID) + `">`,
		`urtuba/demo`,
		`value="20" required`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	for _, s := range []string{"Billing model", "urtuba/billing", "Not ready", glm} {
		if strings.Contains(body, s) {
			t.Errorf("page contains %q", s)
		}
	}

	// With a default model, the choice names it.
	if err := app.store.SetDefaultModel(context.Background(), app.project.ID, app.model.ID); err != nil {
		t.Fatal(err)
	}
	body = app.get(projectPath(app.project.ID)+"/agents/new", app.login("bora")).Body.String()
	if !strings.Contains(body, `<option value="0" selected>Project default (Fast)</option>`) {
		t.Error("the default choice does not name the default model")
	}
}

func TestCreateAgent(t *testing.T) {
	ctx := context.Background()
	app := newAgentApp(t)
	agents := projectPath(app.project.ID) + "/agents"
	bora := app.login("bora")
	base := fillAgentForm("Triage")
	res := itoa(app.res.ID)
	// 20000 characters with CRLF line breaks, as a browser sends them.
	longPrompt := strings.Repeat("abcdefghi\r\n", 2000)

	tests := []struct {
		name       string
		form       url.Values
		wantStatus int
		wantBody   string
	}{
		{"name taken, in another case", with(base, "name", "helper"), http.StatusConflict, "already has an agent with this name"},
		{"no name", with(base, "name", "  "), http.StatusBadRequest, "An agent name must be 1 to 100 characters."},
		{"long name", with(base, "name", strings.Repeat("a", 101)), http.StatusBadRequest, "An agent name must be 1 to 100 characters."},
		{"long description", with(base, "description", strings.Repeat("d", 301)), http.StatusBadRequest, "at most 300 characters"},
		{"long instructions", with(base, "prompt", strings.Repeat("p", 20001)), http.StatusBadRequest, "at most 20000 characters"},
		{"unknown kind", with(base, "kind", "robot"), http.StatusBadRequest, "Pick a kind: chat or answer."},
		{"no kind", with(base, "kind", ""), http.StatusBadRequest, "Pick a kind"},
		{"another project's model", with(base, "model", itoa(app.otherModel.ID)), http.StatusBadRequest, "Pick the project default or one of the project&#39;s models."},
		{"bad model", with(base, "model", "abc"), http.StatusBadRequest, "Pick the project default"},
		{"rate limit 0", with(base, "rate_limit", "0"), http.StatusBadRequest, "from 1 to 1000"},
		{"rate limit 1001", with(base, "rate_limit", "1001"), http.StatusBadRequest, "from 1 to 1000"},
		{"rate limit not a number", with(base, "rate_limit", "ten"), http.StatusBadRequest, "from 1 to 1000"},
		{"no resource choice", with(base, "resources", ""), http.StatusBadRequest, "Choose whether the agent reads"},
		{"picked none", with(base, "resources", "picked"), http.StatusBadRequest, "Tick at least one repository"},
		{"picked another project's", with(with(base, "resources", "picked"), "resource", res, itoa(app.other.ID)),
			http.StatusBadRequest, "Pick repositories of this project only."},
		{"chat agent, every repository", with(base, "name", "Triage"), http.StatusSeeOther, ""},
		{"answer agent, own model, picked", with(with(with(with(with(base, "name", "Sorter"), "kind", store.AgentAnswer),
			"model", itoa(app.model.ID)), "resources", "picked"), "resource", res), http.StatusSeeOther, ""},
		{"long instructions with CRLF", with(with(base, "name", "Long"), "prompt", longPrompt), http.StatusSeeOther, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := app.store.Agents(ctx, app.project.ID)
			rec := app.post(agents, tt.form, bora)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tt.wantStatus, rec.Body)
			}
			after, _ := app.store.Agents(ctx, app.project.ID)
			if rec.Code != http.StatusSeeOther {
				body := rec.Body.String()
				if !strings.Contains(body, tt.wantBody) {
					t.Errorf("body does not contain %q", tt.wantBody)
				}
				if len(after) != len(before) {
					t.Errorf("agents = %d, want %d", len(after), len(before))
				}
				// The form keeps what was typed.
				if !strings.Contains(body, `value="`+tt.form.Get("name")+`"`) {
					t.Error("the form lost the name typed")
				}
				return
			}
			if len(after) != len(before)+1 {
				t.Fatalf("agents = %d, want %d", len(after), len(before)+1)
			}
		})
	}

	byName := map[string]store.Agent{}
	list, _ := app.store.Agents(ctx, app.project.ID)
	for _, s := range list {
		a, err := app.store.Agent(ctx, app.project.ID, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		byName[a.Name] = a
	}
	if a := byName["Triage"]; a.Kind != store.AgentChat || !a.AllResources || a.ModelID != 0 || a.Ready ||
		a.Prompt != "Be short." || a.Description != "Helps the support team." || a.RateLimitPerHour != 20 {
		t.Errorf("Triage = %+v", a)
	}
	if a := byName["Sorter"]; a.Kind != store.AgentAnswer || a.AllResources || a.ModelID != app.model.ID || !a.Ready ||
		!reflect.DeepEqual(a.ResourceIDs, []int64{app.res.ID}) {
		t.Errorf("Sorter = %+v", a)
	}
	if a := byName["Long"]; len([]rune(a.Prompt)) != 19999 || strings.Contains(a.Prompt, "\r") {
		t.Errorf("Long prompt: %d characters, CR kept: %v", len([]rune(a.Prompt)), strings.Contains(a.Prompt, "\r"))
	}

	// Creating opens the agent's page, which warns that it is not ready.
	rec := app.post(agents, fillAgentForm("Fresh"), bora)
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, agents+"/") || !strings.HasSuffix(loc, "?saved=created") {
		t.Fatalf("location = %q, want the agent's page", loc)
	}
	body := app.get(loc, bora).Body.String()
	for _, want := range []string{"Agent created.", "Not ready: this agent has no model", `<span class="badge">chat</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestEditAgent(t *testing.T) {
	ctx := context.Background()
	app := newAgentApp(t)
	page := agentPath(app.project.ID, app.agent.ID)
	bora := app.login("bora")

	body := app.get(page, bora).Body.String()
	for _, want := range []string{
		`value="Helper"`,
		`value="Answers questions about the portal."`,
		`>` + agentPrompt + `</textarea>`,
		`<option value="0" selected>Project default (none set)</option>`,
		`<span class="badge">chat</span>`,
		"Not ready: this agent has no model, and the project has no default model.",
		`data-confirm="Delete the agent Helper?"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, `name="kind"`) {
		t.Error("the edit page offers to change the kind")
	}

	// A save changes every field but the kind, even when one is sent.
	form := with(with(with(with(fillAgentForm("Portal helper"), "kind", store.AgentAnswer), "model", itoa(app.model.ID)),
		"resources", "picked"), "resource", itoa(app.res.ID))
	form.Set("rate_limit", "5")
	rec := app.post(page, form, bora)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page+"?saved=1" {
		t.Fatalf("save: status = %d, location = %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	got := app.loadAgent(t)
	if got.Name != "Portal helper" || got.Kind != store.AgentChat || got.ModelID != app.model.ID || got.AllResources ||
		!reflect.DeepEqual(got.ResourceIDs, []int64{app.res.ID}) || got.RateLimitPerHour != 5 || got.Prompt != "Be short." {
		t.Errorf("after save: %+v", got)
	}
	body = app.get(page+"?saved=1", bora).Body.String()
	for _, want := range []string{
		"Saved.",
		`<option value="` + itoa(app.model.ID) + `" selected>Fast</option>`,
		`<input type="radio" name="resources" value="picked" checked>`,
		`<input type="checkbox" name="resource" value="` + itoa(app.res.ID) + `" checked>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, "Not ready") {
		t.Error("an agent with its own model is shown as not ready")
	}

	// Refused edits change nothing, and the form keeps what was typed.
	if _, err := app.store.AddAgent(ctx, app.project.ID, store.AgentChat, store.AgentSettings{
		Name: "Other", RateLimitPerHour: 20, AllResources: true,
	}, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		form   url.Values
		status int
		want   string
	}{
		{"taken name", fillAgentForm("OTHER"), http.StatusConflict, "already has an agent with this name"},
		{"rate limit", with(fillAgentForm("Typed name"), "rate_limit", "2000"), http.StatusBadRequest, "from 1 to 1000"},
		{"another project's resource", with(with(fillAgentForm("Typed name"), "resources", "picked"), "resource", itoa(app.other.ID)),
			http.StatusBadRequest, "Pick repositories of this project only."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.post(page, tt.form, bora)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			body := rec.Body.String()
			for _, want := range []string{tt.want, `value="` + tt.form.Get("name") + `"`, `<span class="badge">chat</span>`} {
				if !strings.Contains(body, want) {
					t.Errorf("body does not contain %q", want)
				}
			}
			if after := app.loadAgent(t); !reflect.DeepEqual(after, got) {
				t.Errorf("agent = %+v, want %+v", after, got)
			}
		})
	}
}

func TestDeleteAgent(t *testing.T) {
	app := newAgentApp(t)
	bora := app.login("bora")
	rec := app.post(agentPath(app.project.ID, app.agent.ID)+"/delete", nil, bora)
	want := projectPath(app.project.ID) + "?notice=agent-deleted#agents"
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("status = %d, location = %q, want 303 to %s", rec.Code, rec.Header().Get("Location"), want)
	}
	if list, _ := app.store.Agents(context.Background(), app.project.ID); len(list) != 0 {
		t.Errorf("agents = %+v, want none", list)
	}
	body := app.get(projectPath(app.project.ID)+"?notice=agent-deleted", bora).Body.String()
	if !strings.Contains(body, "Agent deleted.") || strings.Contains(body, "<td>Helper") {
		t.Error("the project page does not show the delete")
	}
	if rec := app.post(agentPath(app.project.ID, app.agent.ID)+"/delete", nil, bora); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404", rec.Code)
	}
}
