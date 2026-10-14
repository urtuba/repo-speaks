package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrModelExists is returned when the project already has a model with
// the same name.
var ErrModelExists = errors.New("model name already used")

// ErrModelIsDefault is returned when a model to delete is the project's
// default model.
var ErrModelIsDefault = errors.New("model is the project's default")

// ErrModelInUse matches the error DeleteModel returns when agents use
// the model. The error itself is a *ModelInUseError, which names them.
var ErrModelInUse = errors.New("model is used by agents")

// ModelInUseError is returned when agents use a model to delete.
// errors.Is(err, ErrModelInUse) is true for it.
type ModelInUseError struct {
	Agents []string // the names of the agents that use the model, sorted
}

func (e *ModelInUseError) Error() string {
	return fmt.Sprintf("%v: %s", ErrModelInUse, strings.Join(e.Agents, ", "))
}

func (e *ModelInUseError) Is(target error) bool {
	return target == ErrModelInUse
}

// Model is an LLM model of a project, as pages show it. It never holds
// the API key: only ModelEndpoint reads that.
type Model struct {
	ID        int64
	ProjectID int64
	Name      string
	BaseURL   string // the host's OpenAI-compatible API, with no trailing slash
	ModelID   string // the host's name for the model, such as z-ai/glm-5.3-flash
	HasKey    bool
	KeyHint   string // the key's last 4 characters, so an admin can tell keys apart
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewModel describes a model to add to a project.
type NewModel struct {
	ProjectID int64
	Name      string
	BaseURL   string
	ModelID   string
	APIKey    string // empty for a host that needs no key
}

// ModelChange is an edit of a model. An empty APIKey keeps the current
// key, unless the base URL changes: then the key is cleared, so a key
// never goes to a host it was not entered for.
type ModelChange struct {
	Name    string
	BaseURL string
	ModelID string
	APIKey  string
}

// AddModel adds a model to a project. The caller has checked the fields
// and the model ID. It returns ErrModelExists when the project already
// has a model with the name.
func (s *Store) AddModel(ctx context.Context, nm NewModel, now time.Time) (Model, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Model{}, fmt.Errorf("add model: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	if err := checkModelName(ctx, tx, nm.ProjectID, 0, nm.Name); err != nil {
		return Model{}, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO models (project_id, name, base_url, api_key, api_key_hint, model_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		nm.ProjectID, nm.Name, nm.BaseURL, nullText(nm.APIKey), nullText(tokenHint(nm.APIKey)),
		nm.ModelID, now.Unix(), now.Unix()).Scan(&id)
	if err != nil {
		return Model{}, fmt.Errorf("add model: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Model{}, fmt.Errorf("add model: %w", err)
	}
	return Model{
		ID:        id,
		ProjectID: nm.ProjectID,
		Name:      nm.Name,
		BaseURL:   nm.BaseURL,
		ModelID:   nm.ModelID,
		HasKey:    nm.APIKey != "",
		KeyHint:   tokenHint(nm.APIKey),
		CreatedAt: unix(now.Unix()),
		UpdatedAt: unix(now.Unix()),
	}, nil
}

// checkModelName returns ErrModelExists when another model of the
// project, other than exceptID, has the name. Names compare without case.
func checkModelName(ctx context.Context, tx *sql.Tx, projectID, exceptID int64, name string) error {
	var taken bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM models WHERE project_id = ? AND name = ? AND id != ?)`,
		projectID, name, exceptID).Scan(&taken); err != nil {
		return fmt.Errorf("check model name: %w", err)
	}
	if taken {
		return ErrModelExists
	}
	return nil
}

// modelColumns is the SELECT list scanModel reads. It leaves out the key
// on purpose.
const modelColumns = `
	id, project_id, name, base_url, model_id, api_key IS NOT NULL, api_key_hint, created_at, updated_at
	FROM models`

// scanModel reads one row selected with modelColumns.
func scanModel(row scanner) (Model, error) {
	var m Model
	var hint sql.NullString
	var createdAt, updatedAt int64
	err := row.Scan(&m.ID, &m.ProjectID, &m.Name, &m.BaseURL, &m.ModelID, &m.HasKey, &hint,
		&createdAt, &updatedAt)
	if err != nil {
		return Model{}, noRows(err)
	}
	m.KeyHint = hint.String
	m.CreatedAt = unix(createdAt)
	m.UpdatedAt = unix(updatedAt)
	return m, nil
}

// Models returns a project's models, by name.
func (s *Store) Models(ctx context.Context, projectID int64) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+modelColumns+`
		WHERE project_id = ?
		ORDER BY name, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	var list []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("list models: %w", err)
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// Model returns the project's model with the id. It returns ErrNotFound
// when the project has no such model, so one project's URL cannot reach
// another project's model.
func (s *Store) Model(ctx context.Context, projectID, id int64) (Model, error) {
	row := s.db.QueryRowContext(ctx, "SELECT"+modelColumns+`
		WHERE id = ? AND project_id = ?`, id, projectID)
	m, err := scanModel(row)
	if err != nil {
		return Model{}, fmt.Errorf("model %d: %w", id, err)
	}
	return m, nil
}

// UpdateModel changes a model's name, base URL and model ID, and sets
// its key as ModelChange explains. The key is cleared in the same
// statement that changes the URL, so no moment exists where the old key
// belongs to the new URL. The caller has checked the fields and the
// model ID. It returns ErrNotFound when the project has no such model,
// and ErrModelExists when another model has the name.
func (s *Store) UpdateModel(ctx context.Context, projectID, id int64, ch ModelChange, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update model: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	var exists bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM models WHERE id = ? AND project_id = ?)",
		id, projectID).Scan(&exists); err != nil {
		return fmt.Errorf("update model: %w", err)
	}
	if !exists {
		return fmt.Errorf("model %d: %w", id, ErrNotFound)
	}
	if err := checkModelName(ctx, tx, projectID, id, ch.Name); err != nil {
		return err
	}
	// In SET, base_url is the value before this UPDATE.
	res, err := tx.ExecContext(ctx, `
		UPDATE models SET
			api_key = CASE WHEN ?1 IS NOT NULL THEN ?1 WHEN base_url != ?3 THEN NULL ELSE api_key END,
			api_key_hint = CASE WHEN ?1 IS NOT NULL THEN ?2 WHEN base_url != ?3 THEN NULL ELSE api_key_hint END,
			base_url = ?3, name = ?4, model_id = ?5, updated_at = ?6
		WHERE id = ?7 AND project_id = ?8`,
		nullText(ch.APIKey), nullText(tokenHint(ch.APIKey)), ch.BaseURL, ch.Name, ch.ModelID,
		now.Unix(), id, projectID)
	if err != nil {
		return fmt.Errorf("update model: %w", err)
	}
	if err := oneRow(res, fmt.Sprintf("model %d", id)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteModel removes a model from its project. It returns
// ErrModelIsDefault when the model is the project's default, a
// *ModelInUseError when agents use it, and ErrNotFound when the project
// has no such model.
func (s *Store) DeleteModel(ctx context.Context, projectID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	var isDefault bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM projects WHERE id = ? AND default_model_id = ?)",
		projectID, id).Scan(&isDefault); err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	if isDefault {
		return ErrModelIsDefault
	}
	users, err := agentsUsingModel(ctx, tx, projectID, id)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	if len(users) > 0 {
		return &ModelInUseError{Agents: users}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM models WHERE id = ? AND project_id = ?", id, projectID)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	if err := oneRow(res, fmt.Sprintf("model %d", id)); err != nil {
		return err
	}
	return tx.Commit()
}

// agentsUsingModel returns the names of the project's agents that use
// the model as their own, by name.
func agentsUsingModel(ctx context.Context, tx *sql.Tx, projectID, modelID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT name FROM agents WHERE project_id = ? AND model_id = ? ORDER BY name, id",
		projectID, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// SetDefaultModel makes the model the project's default. A modelID of 0
// leaves the project with no default. It returns ErrNotFound when the
// project or the model is missing, or when the model belongs to another
// project.
func (s *Store) SetDefaultModel(ctx context.Context, projectID, modelID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET default_model_id = ?1
		WHERE id = ?2 AND (?1 IS NULL OR EXISTS (SELECT 1 FROM models WHERE id = ?1 AND project_id = ?2))`,
		nullID(modelID), projectID)
	if err != nil {
		return fmt.Errorf("set default model: %w", err)
	}
	return oneRow(res, fmt.Sprintf("model %d in project %d", modelID, projectID))
}

// ModelEndpoint is what a call to a model's host needs. Unlike Model, it
// holds the key, so only code that calls the host uses it.
type ModelEndpoint struct {
	BaseURL string
	APIKey  string // empty when the model has no key
	ModelID string
}

// ModelEndpoint returns what a call to the project's model needs, or
// ErrNotFound.
func (s *Store) ModelEndpoint(ctx context.Context, projectID, id int64) (ModelEndpoint, error) {
	var e ModelEndpoint
	var key sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT base_url, api_key, model_id FROM models WHERE id = ? AND project_id = ?",
		id, projectID).Scan(&e.BaseURL, &key, &e.ModelID)
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("model %d: %w", id, noRows(err))
	}
	e.APIKey = key.String
	return e, nil
}
