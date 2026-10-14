package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

const (
	testKey      = "sk-or-v1-0000secretvalue0000-k3y9"
	openRouter   = "https://openrouter.ai/api/v1"
	testModelID  = "z-ai/glm-5.3-flash"
	otherModelID = "openai/gpt-5.4-mini"
)

// addModel adds a model to the fixture's first project.
func (f resourceFixture) addModel(t *testing.T, name, baseURL, key string) Model {
	t.Helper()
	m, err := f.s.AddModel(context.Background(), NewModel{
		ProjectID: f.project.ID, Name: name, BaseURL: baseURL, ModelID: testModelID, APIKey: key,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// endpoint returns the model's stored endpoint, key included.
func (f resourceFixture) endpoint(t *testing.T, id int64) ModelEndpoint {
	t.Helper()
	e, err := f.s.ModelEndpoint(context.Background(), f.project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAddModel(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)

	want := Model{
		ID:        m.ID,
		ProjectID: f.project.ID,
		Name:      "Fast",
		BaseURL:   openRouter,
		ModelID:   testModelID,
		HasKey:    true,
		KeyHint:   "k3y9",
		CreatedAt: testNow,
		UpdatedAt: testNow,
	}
	if m != want {
		t.Errorf("AddModel() = %+v, want %+v", m, want)
	}
	got, err := f.s.Model(ctx, f.project.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Model() = %+v, want %+v", got, want)
	}
	if e := f.endpoint(t, m.ID); e != (ModelEndpoint{BaseURL: openRouter, APIKey: testKey, ModelID: testModelID}) {
		t.Errorf("ModelEndpoint() = %+v", e)
	}

	// A model with no key, such as one on a local host.
	local := f.addModel(t, "Local", "http://127.0.0.1:11434/v1", "")
	if local.HasKey || local.KeyHint != "" || f.endpoint(t, local.ID).APIKey != "" {
		t.Errorf("model without a key = %+v", local)
	}
}

func TestAddModelRejectsDuplicateNames(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	f.addModel(t, "Fast", openRouter, "")

	tests := []struct {
		name    string
		project int64
		model   string
		wantErr error
	}{
		{"same name", f.project.ID, "Fast", ErrModelExists},
		{"name in another case", f.project.ID, "fast", ErrModelExists},
		{"another name, same model ID", f.project.ID, "Fast 2", nil},
		{"same name in another project", f.other.ID, "Fast", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.AddModel(ctx, NewModel{
				ProjectID: tt.project, Name: tt.model, BaseURL: openRouter, ModelID: testModelID,
			}, testNow)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("AddModel() error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	// The unique constraint holds without the check AddModel makes first.
	_, err := f.s.db.ExecContext(ctx, `
		INSERT INTO models (project_id, name, base_url, model_id, created_at, updated_at)
		VALUES (?, 'FAST', 'https://x.example', 'm', 0, 0)`, f.project.ID)
	if err == nil {
		t.Error("second row named FAST: error = nil, want a unique constraint error")
	}
}

func TestModels(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	b := f.addModel(t, "zeta", openRouter, testKey)
	a := f.addModel(t, "Alpha", openRouter, "")
	if _, err := f.s.AddModel(ctx, NewModel{ProjectID: f.other.ID, Name: "Beta", BaseURL: openRouter, ModelID: "m"}, testNow); err != nil {
		t.Fatal(err)
	}

	list, err := f.s.Models(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	if want := []int64{a.ID, b.ID}; !reflect.DeepEqual(ids, want) {
		t.Errorf("Models() ids = %v, want %v", ids, want)
	}
}

func TestModelIsScopedToItsProject(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)

	if _, err := f.s.Model(ctx, f.other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Model() from another project: error = %v, want ErrNotFound", err)
	}
	if _, err := f.s.ModelEndpoint(ctx, f.other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("ModelEndpoint() from another project: error = %v, want ErrNotFound", err)
	}
	ch := ModelChange{Name: "Taken", BaseURL: "https://evil.example/v1", ModelID: "x"}
	if err := f.s.UpdateModel(ctx, f.other.ID, m.ID, ch, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateModel() from another project: error = %v, want ErrNotFound", err)
	}
	if err := f.s.SetDefaultModel(ctx, f.other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDefaultModel() with another project's model: error = %v, want ErrNotFound", err)
	}
	// The model is its own project's default, but the other project
	// must still get ErrNotFound, not a hint that the model exists.
	if err := f.s.SetDefaultModel(ctx, f.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteModel(ctx, f.other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteModel() from another project: error = %v, want ErrNotFound", err)
	}
	if err := f.s.SetDefaultModel(ctx, f.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteModel(ctx, f.other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteModel() from another project: error = %v, want ErrNotFound", err)
	}

	// Nothing changed.
	got, err := f.s.Model(ctx, f.project.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Fast" || got.BaseURL != openRouter || f.endpoint(t, m.ID).APIKey != testKey {
		t.Errorf("model = %+v, want it unchanged", got)
	}
	if p, _ := f.s.Project(ctx, f.other.ID); p.DefaultModelID != 0 {
		t.Errorf("other project's default = %d, want none", p.DefaultModelID)
	}
}

func TestUpdateModel(t *testing.T) {
	ctx := context.Background()
	const newKey = "sk-or-v1-1111othervalue1111-n3w2"
	const otherHost = "https://api.example.com/v1"
	later := testNow.Add(time.Hour)

	tests := []struct {
		name     string
		change   ModelChange
		wantKey  string
		wantHint string
	}{
		{"rename keeps the key",
			ModelChange{Name: "Quick", BaseURL: openRouter, ModelID: testModelID}, testKey, "k3y9"},
		{"new model ID keeps the key",
			ModelChange{Name: "Fast", BaseURL: openRouter, ModelID: otherModelID}, testKey, "k3y9"},
		{"new key replaces the key",
			ModelChange{Name: "Fast", BaseURL: openRouter, ModelID: testModelID, APIKey: newKey}, newKey, "n3w2"},
		{"new URL clears the key",
			ModelChange{Name: "Fast", BaseURL: otherHost, ModelID: testModelID}, "", ""},
		{"new URL with a new key",
			ModelChange{Name: "Fast", BaseURL: otherHost, ModelID: testModelID, APIKey: newKey}, newKey, "n3w2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResourceFixture(t)
			m := f.addModel(t, "Fast", openRouter, testKey)
			if err := f.s.UpdateModel(ctx, f.project.ID, m.ID, tt.change, later); err != nil {
				t.Fatal(err)
			}
			got, err := f.s.Model(ctx, f.project.ID, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := Model{
				ID: m.ID, ProjectID: f.project.ID,
				Name: tt.change.Name, BaseURL: tt.change.BaseURL, ModelID: tt.change.ModelID,
				HasKey: tt.wantKey != "", KeyHint: tt.wantHint,
				CreatedAt: testNow, UpdatedAt: later,
			}
			if got != want {
				t.Errorf("Model() = %+v, want %+v", got, want)
			}
			if key := f.endpoint(t, m.ID).APIKey; key != tt.wantKey {
				t.Errorf("key = %q, want %q", key, tt.wantKey)
			}
		})
	}
}

func TestUpdateModelErrors(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	f.addModel(t, "Slow", openRouter, "")

	ch := ModelChange{Name: "slow", BaseURL: openRouter, ModelID: testModelID}
	if err := f.s.UpdateModel(ctx, f.project.ID, m.ID, ch, testNow); !errors.Is(err, ErrModelExists) {
		t.Errorf("rename to a taken name: error = %v, want ErrModelExists", err)
	}
	// A change of case in its own name is fine.
	ch.Name = "FAST"
	if err := f.s.UpdateModel(ctx, f.project.ID, m.ID, ch, testNow); err != nil {
		t.Errorf("rename in another case: error = %v", err)
	}
	if err := f.s.UpdateModel(ctx, f.project.ID, 999, ch, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing model: error = %v, want ErrNotFound", err)
	}
}

func TestDefaultModel(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	spare := f.addModel(t, "Spare", openRouter, "")

	if p, _ := f.s.Project(ctx, f.project.ID); p.DefaultModelID != 0 {
		t.Errorf("new project's default = %d, want none", p.DefaultModelID)
	}
	if err := f.s.SetDefaultModel(ctx, f.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.s.Project(ctx, f.project.ID); p.DefaultModelID != m.ID {
		t.Errorf("default = %d, want %d", p.DefaultModelID, m.ID)
	}

	// The default cannot be deleted; another model can.
	if err := f.s.DeleteModel(ctx, f.project.ID, m.ID); !errors.Is(err, ErrModelIsDefault) {
		t.Errorf("DeleteModel() of the default: error = %v, want ErrModelIsDefault", err)
	}
	if err := f.s.DeleteModel(ctx, f.project.ID, spare.ID); err != nil {
		t.Errorf("DeleteModel() of another model: %v", err)
	}
	if _, err := f.s.Model(ctx, f.project.ID, m.ID); err != nil {
		t.Errorf("the default model is gone: %v", err)
	}

	// The database refuses too, without the check DeleteModel makes.
	if _, err := f.s.db.ExecContext(ctx, "DELETE FROM models WHERE id = ?", m.ID); err == nil {
		t.Error("deleting the default in SQL: error = nil, want a foreign key error")
	}

	if err := f.s.SetDefaultModel(ctx, f.project.ID, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDefaultModel() with a missing model: error = %v, want ErrNotFound", err)
	}
	if err := f.s.SetDefaultModel(ctx, 999, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDefaultModel() on a missing project: error = %v, want ErrNotFound", err)
	}

	// With no default, the model can go.
	if err := f.s.SetDefaultModel(ctx, f.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteModel(ctx, f.project.ID, m.ID); err != nil {
		t.Errorf("DeleteModel() after clearing the default: %v", err)
	}
	if err := f.s.DeleteModel(ctx, f.project.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: error = %v, want ErrNotFound", err)
	}
}

func TestDeleteProjectDeletesModels(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	m := f.addModel(t, "Fast", openRouter, testKey)
	if err := f.s.SetDefaultModel(ctx, f.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	// The default model does not block the project's delete.
	if err := f.s.DeleteProject(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM models").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("models after project delete = %d, want 0", n)
	}
}
