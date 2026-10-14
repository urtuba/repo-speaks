package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// testPrompt is an agent's instructions in the tests.
const testPrompt = "Answer yes or no, then give one reason."

// addAgent adds an agent of the kind to the fixture's first project,
// with every resource and the project's default model.
func (f resourceFixture) addAgent(t *testing.T, kind, name string) Agent {
	t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.project.ID, kind, AgentSettings{
		Name: name, Description: "Helps the support team.", Prompt: testPrompt,
		RateLimitPerHour: 20, AllResources: true,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// agentIDs returns the IDs of the project's agents, in list order.
func (f resourceFixture) agentIDs(t *testing.T, projectID int64) []int64 {
	t.Helper()
	list, err := f.s.Agents(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestAddAgent(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	r1 := f.add(t, "urtuba", "alpha", "", "")
	r2 := f.add(t, "urtuba", "beta", "", "")

	a, err := f.s.AddAgent(ctx, f.project.ID, AgentAnswer, AgentSettings{
		Name: "Triage", Description: "Sorts requests.", Prompt: testPrompt, ModelID: m.ID,
		RateLimitPerHour: 50, ResourceIDs: []int64{r2.ID, r1.ID, r2.ID},
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := Agent{
		AgentSummary: AgentSummary{
			ID: a.ID, ProjectID: f.project.ID, Kind: AgentAnswer, Name: "Triage",
			Description: "Sorts requests.", Ready: true,
		},
		Prompt: testPrompt, ModelID: m.ID, RateLimitPerHour: 50,
		ResourceIDs: []int64{r1.ID, r2.ID}, CreatedAt: testNow, UpdatedAt: testNow,
	}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("AddAgent() = %+v, want %+v", a, want)
	}
	got, err := f.s.Agent(ctx, f.project.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Agent() = %+v, want %+v", got, want)
	}

	// With every resource, the agent keeps no picks.
	all, err := f.s.AddAgent(ctx, f.project.ID, AgentChat, AgentSettings{
		Name: "Helper", RateLimitPerHour: 20, AllResources: true, ResourceIDs: []int64{r1.ID},
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !all.AllResources || len(all.ResourceIDs) != 0 || all.ModelID != 0 || all.Kind != AgentChat {
		t.Errorf("agent with every resource = %+v", all)
	}
}

func TestAddAgentChecks(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	f.addAgent(t, AgentChat, "Helper")
	otherModel, err := f.s.AddModel(ctx, NewModel{ProjectID: f.other.ID, Name: "Other", BaseURL: openRouter, ModelID: "m"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	otherRes, err := f.s.AddResource(ctx, NewResource{ProjectID: f.other.ID, Owner: "x", Repo: "y"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	mine := f.add(t, "urtuba", "alpha", "", "")

	tests := []struct {
		name    string
		project int64
		kind    string
		set     AgentSettings
		wantErr error
	}{
		{"same name", f.project.ID, AgentChat, AgentSettings{Name: "Helper", RateLimitPerHour: 20, AllResources: true}, ErrAgentExists},
		{"name in another case", f.project.ID, AgentAnswer, AgentSettings{Name: "HELPER", RateLimitPerHour: 20, AllResources: true}, ErrAgentExists},
		{"same name in another project", f.other.ID, AgentChat, AgentSettings{Name: "Helper", RateLimitPerHour: 20, AllResources: true}, nil},
		{"another project's model", f.project.ID, AgentChat, AgentSettings{Name: "A", ModelID: otherModel.ID, RateLimitPerHour: 20, AllResources: true}, ErrNotInProject},
		{"missing model", f.project.ID, AgentChat, AgentSettings{Name: "B", ModelID: 999, RateLimitPerHour: 20, AllResources: true}, ErrNotInProject},
		{"another project's resource", f.project.ID, AgentChat, AgentSettings{Name: "C", RateLimitPerHour: 20, ResourceIDs: []int64{mine.ID, otherRes.ID}}, ErrNotInProject},
		{"missing resource", f.project.ID, AgentChat, AgentSettings{Name: "D", RateLimitPerHour: 20, ResourceIDs: []int64{999}}, ErrNotInProject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.AddAgent(ctx, tt.project, tt.kind, tt.set, testNow)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("AddAgent() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	// Refused agents left nothing behind.
	if ids := f.agentIDs(t, f.project.ID); len(ids) != 1 {
		t.Errorf("agents = %v, want only Helper", ids)
	}
	var picks int
	if err := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM agent_resources").Scan(&picks); err != nil {
		t.Fatal(err)
	}
	if picks != 0 {
		t.Errorf("agent_resources rows = %d, want 0", picks)
	}

	// The database checks the kind and the rate limit, and the name
	// without the check AddAgent makes first.
	for _, set := range []string{
		"'robot', 'X', 20",
		"'chat', 'Y', 0",
		"'chat', 'Z', 1001",
		"'chat', 'helper', 20",
	} {
		_, err := f.s.db.ExecContext(ctx, `
			INSERT INTO agents (project_id, kind, name, rate_limit_per_hour, created_at, updated_at)
			VALUES (?, `+set+`, 0, 0)`, f.project.ID)
		if err == nil {
			t.Errorf("insert (%s): error = nil, want a constraint error", set)
		}
	}
}

func TestAgents(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	z := f.addAgent(t, AgentChat, "zeta")
	a := f.addAgent(t, AgentAnswer, "Alpha")
	if _, err := f.s.AddAgent(ctx, f.other.ID, AgentChat, AgentSettings{Name: "Beta", RateLimitPerHour: 20, AllResources: true}, testNow); err != nil {
		t.Fatal(err)
	}
	if ids := f.agentIDs(t, f.project.ID); !reflect.DeepEqual(ids, []int64{a.ID, z.ID}) {
		t.Errorf("Agents() ids = %v, want %v", ids, []int64{a.ID, z.ID})
	}
	list, err := f.s.Agents(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := AgentSummary{ID: a.ID, ProjectID: f.project.ID, Kind: AgentAnswer, Name: "Alpha", Description: "Helps the support team."}
	if list[0] != want {
		t.Errorf("Agents()[0] = %+v, want %+v", list[0], want)
	}
}

func TestAgentIsScopedToItsProject(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	a := f.addAgent(t, AgentChat, "Helper")

	if _, err := f.s.Agent(ctx, f.other.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Agent() from another project: error = %v, want ErrNotFound", err)
	}
	set := AgentSettings{Name: "Taken", RateLimitPerHour: 5, AllResources: true}
	if err := f.s.UpdateAgent(ctx, f.other.ID, a.ID, set, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateAgent() from another project: error = %v, want ErrNotFound", err)
	}
	if err := f.s.DeleteAgent(ctx, f.other.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteAgent() from another project: error = %v, want ErrNotFound", err)
	}
	got, err := f.s.Agent(ctx, f.project.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a) {
		t.Errorf("agent = %+v, want it unchanged", got)
	}
}

func TestUpdateAgent(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	r1 := f.add(t, "urtuba", "alpha", "", "")
	r2 := f.add(t, "urtuba", "beta", "", "")
	a := f.addAgent(t, AgentAnswer, "Triage")
	f.addAgent(t, AgentChat, "Helper")
	later := testNow.Add(time.Hour)

	set := AgentSettings{
		Name: "Sorter", Description: "New text.", Prompt: "Three bullet points.", ModelID: m.ID,
		RateLimitPerHour: 1000, ResourceIDs: []int64{r2.ID},
	}
	if err := f.s.UpdateAgent(ctx, f.project.ID, a.ID, set, later); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Agent(ctx, f.project.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := Agent{
		AgentSummary: AgentSummary{
			ID: a.ID, ProjectID: f.project.ID, Kind: AgentAnswer, Name: "Sorter",
			Description: "New text.", Ready: true,
		},
		Prompt: "Three bullet points.", ModelID: m.ID, RateLimitPerHour: 1000,
		ResourceIDs: []int64{r2.ID}, CreatedAt: testNow, UpdatedAt: later,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Agent() = %+v, want %+v", got, want)
	}

	// New picks replace the old ones; every resource clears them.
	set.ResourceIDs = []int64{r1.ID}
	if err := f.s.UpdateAgent(ctx, f.project.ID, a.ID, set, later); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.Agent(ctx, f.project.ID, a.ID); !reflect.DeepEqual(got.ResourceIDs, []int64{r1.ID}) {
		t.Errorf("picks = %v, want %v", got.ResourceIDs, []int64{r1.ID})
	}
	set.AllResources, set.ModelID = true, 0
	if err := f.s.UpdateAgent(ctx, f.project.ID, a.ID, set, later); err != nil {
		t.Fatal(err)
	}
	got, _ = f.s.Agent(ctx, f.project.ID, a.ID)
	if !got.AllResources || got.ResourceIDs != nil || got.ModelID != 0 || got.Kind != AgentAnswer {
		t.Errorf("after every resource and the default model: %+v", got)
	}

	// Refused edits change nothing.
	for _, tt := range []struct {
		name    string
		id      int64
		set     AgentSettings
		wantErr error
	}{
		{"taken name", a.ID, AgentSettings{Name: "helper", RateLimitPerHour: 20, AllResources: true}, ErrAgentExists},
		{"missing model", a.ID, AgentSettings{Name: "Sorter", ModelID: 999, RateLimitPerHour: 20, AllResources: true}, ErrNotInProject},
		{"missing resource", a.ID, AgentSettings{Name: "Sorter", RateLimitPerHour: 20, ResourceIDs: []int64{r1.ID, 999}}, ErrNotInProject},
		{"missing agent", 999, AgentSettings{Name: "Sorter", RateLimitPerHour: 20, AllResources: true}, ErrNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := f.s.UpdateAgent(ctx, f.project.ID, tt.id, tt.set, later); !errors.Is(err, tt.wantErr) {
				t.Errorf("UpdateAgent() error = %v, want %v", err, tt.wantErr)
			}
			if after, _ := f.s.Agent(ctx, f.project.ID, a.ID); !reflect.DeepEqual(after, got) {
				t.Errorf("agent = %+v, want %+v", after, got)
			}
		})
	}
	// A change of case in its own name is fine.
	set.Name = "SORTER"
	if err := f.s.UpdateAgent(ctx, f.project.ID, a.ID, set, later); err != nil {
		t.Errorf("rename in another case: %v", err)
	}
}

func TestDeleteResourceDropsAgentPick(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r1 := f.add(t, "urtuba", "alpha", "", "")
	r2 := f.add(t, "urtuba", "beta", "", "")
	a, err := f.s.AddAgent(ctx, f.project.ID, AgentChat, AgentSettings{
		Name: "Helper", RateLimitPerHour: 20, ResourceIDs: []int64{r1.ID, r2.ID},
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteResource(ctx, f.project.ID, r1.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Agent(ctx, f.project.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AllResources || !reflect.DeepEqual(got.ResourceIDs, []int64{r2.ID}) {
		t.Errorf("after the delete: all = %v, picks = %v, want only %d", got.AllResources, got.ResourceIDs, r2.ID)
	}
}

func TestAgentReady(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	plain := f.addAgent(t, AgentChat, "Plain")
	own, err := f.s.AddAgent(ctx, f.project.ID, AgentChat, AgentSettings{
		Name: "Own", ModelID: m.ID, RateLimitPerHour: 20, AllResources: true,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}

	ready := func() map[int64]bool {
		t.Helper()
		list, err := f.s.Agents(ctx, f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int64]bool{}
		for _, a := range list {
			got[a.ID] = a.Ready
			full, err := f.s.Agent(ctx, f.project.ID, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if full.Ready != a.Ready {
				t.Errorf("agent %d: Agent().Ready = %v, Agents() says %v", a.ID, full.Ready, a.Ready)
			}
		}
		return got
	}

	// No default: only the agent with its own model is ready.
	if got := ready(); got[plain.ID] || !got[own.ID] {
		t.Errorf("no default: ready = %v, want only %d", got, own.ID)
	}
	if plain.Ready || !own.Ready {
		t.Errorf("AddAgent() ready: plain %v, own %v", plain.Ready, own.Ready)
	}
	// With a default, both are.
	if err := f.s.SetDefaultModel(ctx, f.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	if got := ready(); !got[plain.ID] || !got[own.ID] {
		t.Errorf("with a default: ready = %v, want both", got)
	}
	// Another project's default does not count.
	if err := f.s.SetDefaultModel(ctx, f.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	om, err := f.s.AddModel(ctx, NewModel{ProjectID: f.other.ID, Name: "Other", BaseURL: openRouter, ModelID: "m"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDefaultModel(ctx, f.other.ID, om.ID); err != nil {
		t.Fatal(err)
	}
	if got := ready(); got[plain.ID] {
		t.Errorf("another project's default: ready = %v, want %d not ready", got, plain.ID)
	}
}

func TestDeleteAgent(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "alpha", "", "")
	a, err := f.s.AddAgent(ctx, f.project.ID, AgentChat, AgentSettings{
		Name: "Helper", RateLimitPerHour: 20, ResourceIDs: []int64{r.ID},
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteAgent(ctx, f.project.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Agent(ctx, f.project.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Agent() after delete: error = %v, want ErrNotFound", err)
	}
	if err := f.s.DeleteAgent(ctx, f.project.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: error = %v, want ErrNotFound", err)
	}
	var picks int
	if err := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM agent_resources").Scan(&picks); err != nil {
		t.Fatal(err)
	}
	if picks != 0 {
		t.Errorf("agent_resources rows = %d, want 0", picks)
	}
	// The resource stays.
	if _, err := f.s.Resource(ctx, f.project.ID, r.ID); err != nil {
		t.Errorf("the resource is gone: %v", err)
	}
}

func TestModelInUse(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	for _, name := range []string{"Triage", "Helper"} {
		if _, err := f.s.AddAgent(ctx, f.project.ID, AgentChat, AgentSettings{
			Name: name, ModelID: m.ID, RateLimitPerHour: 20, AllResources: true,
		}, testNow); err != nil {
			t.Fatal(err)
		}
	}

	err := f.s.DeleteModel(ctx, f.project.ID, m.ID)
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("DeleteModel() error = %v, want ErrModelInUse", err)
	}
	inUse, ok := errors.AsType[*ModelInUseError](err)
	if !ok || !reflect.DeepEqual(inUse.Agents, []string{"Helper", "Triage"}) {
		t.Errorf("DeleteModel() error = %#v, want the agents Helper and Triage", err)
	}
	// The database refuses too, without the check DeleteModel makes.
	if _, err := f.s.db.ExecContext(ctx, "DELETE FROM models WHERE id = ?", m.ID); err == nil {
		t.Error("deleting a model in use in SQL: error = nil, want a foreign key error")
	}
	if _, err := f.s.Model(ctx, f.project.ID, m.ID); err != nil {
		t.Errorf("the model is gone: %v", err)
	}
}

func TestDeleteProjectDeletesAgents(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	r := f.add(t, "urtuba", "alpha", "", "")
	if err := f.s.SetDefaultModel(ctx, f.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddAgent(ctx, f.project.ID, AgentChat, AgentSettings{
		Name: "Helper", ModelID: m.ID, RateLimitPerHour: 20, ResourceIDs: []int64{r.ID},
	}, testNow); err != nil {
		t.Fatal(err)
	}
	other, err := f.s.AddAgent(ctx, f.other.ID, AgentChat, AgentSettings{Name: "Billing helper", RateLimitPerHour: 20, AllResources: true}, testNow)
	if err != nil {
		t.Fatal(err)
	}

	// An agent's model does not block the project's delete.
	if err := f.s.DeleteProject(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT count(*) FROM agents WHERE project_id = ?",
		"SELECT count(*) FROM models WHERE project_id = ?",
		"SELECT count(*) FROM agent_resources WHERE ? > 0",
	} {
		var n int
		if err := f.s.db.QueryRowContext(ctx, query, f.project.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows after project delete, want 0", query, n)
		}
	}
	if _, err := f.s.Agent(ctx, f.other.ID, other.ID); err != nil {
		t.Errorf("the other project's agent is gone: %v", err)
	}
}
