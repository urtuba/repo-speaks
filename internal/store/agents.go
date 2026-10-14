package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The kinds of agent. The kind is set when the agent is created and
// never changes.
const (
	AgentChat   = "chat"   // holds conversations: many questions in one chat
	AgentAnswer = "answer" // takes one question and gives one answer
)

// ErrAgentExists is returned when the project already has an agent with
// the same name.
var ErrAgentExists = errors.New("agent name already used")

// ErrNotInProject is returned when an agent's model or a picked
// resource does not belong to the agent's project.
var ErrNotInProject = errors.New("model or resource is not in the project")

// AgentSummary is an agent as every member of its project sees it. It
// never holds the prompt: only Agent reads that, for agents:edit.
type AgentSummary struct {
	ID          int64
	ProjectID   int64
	Kind        string // AgentChat or AgentAnswer
	Name        string
	Description string
	// Ready is false when the agent has no model of its own and the
	// project has no default model. Such an agent cannot answer.
	Ready bool
}

// Agent is an agent with every setting, the prompt too. Only pages for
// members with agents:edit use it.
type Agent struct {
	AgentSummary
	Prompt  string // the instructions, with the answer format of an answer agent
	ModelID int64  // the agent's own model, or 0 for the project's default
	// RateLimitPerHour is how many questions one member may ask the
	// agent in an hour. The agent package counts them, in memory.
	RateLimitPerHour int
	// AllResources is true when the agent reads every resource of the
	// project. When false, it reads only ResourceIDs.
	AllResources bool
	ResourceIDs  []int64 // sorted; empty when AllResources is true
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AgentSettings are the fields of an agent that a builder sets, on
// create and on edit. The kind is not here, since it never changes.
// The caller has checked the fields.
type AgentSettings struct {
	Name             string
	Description      string
	Prompt           string
	ModelID          int64 // 0 for the project's default
	RateLimitPerHour int
	AllResources     bool
	ResourceIDs      []int64 // used only when AllResources is false
}

// AddAgent adds an agent of the kind to a project, with its picked
// resources, in one transaction. It returns ErrAgentExists when the
// project already has an agent with the name, and ErrNotInProject when
// the model or a picked resource belongs to no or another project.
func (s *Store) AddAgent(ctx context.Context, projectID int64, kind string, set AgentSettings, now time.Time) (Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, fmt.Errorf("add agent: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	if err := checkAgent(ctx, tx, projectID, 0, set); err != nil {
		return Agent{}, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agents (project_id, kind, name, description, prompt, all_resources, model_id,
			rate_limit_per_hour, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		projectID, kind, set.Name, set.Description, set.Prompt, set.AllResources, nullID(set.ModelID),
		set.RateLimitPerHour, now.Unix(), now.Unix()).Scan(&id)
	if err != nil {
		return Agent{}, fmt.Errorf("add agent: %w", err)
	}
	if err := setAgentResources(ctx, tx, projectID, id, set); err != nil {
		return Agent{}, err
	}
	a, err := agent(ctx, tx, projectID, id)
	if err != nil {
		return Agent{}, fmt.Errorf("add agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, fmt.Errorf("add agent: %w", err)
	}
	return a, nil
}

// UpdateAgent changes an agent's settings and its picked resources, in
// one transaction. The kind stays. It returns ErrNotFound when the
// project has no such agent, ErrAgentExists when another agent has the
// name, and ErrNotInProject as AddAgent does.
func (s *Store) UpdateAgent(ctx context.Context, projectID, id int64, set AgentSettings, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	var exists bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM agents WHERE id = ? AND project_id = ?)",
		id, projectID).Scan(&exists); err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	if !exists {
		return fmt.Errorf("agent %d: %w", id, ErrNotFound)
	}
	if err := checkAgent(ctx, tx, projectID, id, set); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE agents SET
			name = ?, description = ?, prompt = ?, all_resources = ?, model_id = ?,
			rate_limit_per_hour = ?, updated_at = ?
		WHERE id = ? AND project_id = ?`,
		set.Name, set.Description, set.Prompt, set.AllResources, nullID(set.ModelID),
		set.RateLimitPerHour, now.Unix(), id, projectID)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	if err := setAgentResources(ctx, tx, projectID, id, set); err != nil {
		return err
	}
	return tx.Commit()
}

// checkAgent returns ErrAgentExists when another agent of the project,
// other than exceptID, has the name. Names compare without case. It
// returns ErrNotInProject when the model is not one of the project's.
func checkAgent(ctx context.Context, tx *sql.Tx, projectID, exceptID int64, set AgentSettings) error {
	var taken, modelOK bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM agents WHERE project_id = ?1 AND name = ?2 AND id != ?3),
			?4 IS NULL OR EXISTS (SELECT 1 FROM models WHERE id = ?4 AND project_id = ?1)`,
		projectID, set.Name, exceptID, nullID(set.ModelID)).Scan(&taken, &modelOK); err != nil {
		return fmt.Errorf("check agent: %w", err)
	}
	if taken {
		return ErrAgentExists
	}
	if !modelOK {
		return fmt.Errorf("model %d: %w", set.ModelID, ErrNotInProject)
	}
	return nil
}

// setAgentResources replaces the agent's picked resources. With
// AllResources, the agent keeps no picks. The INSERT takes only
// resources of the project, so a resource of another project is never
// added: when one is missing, it returns ErrNotInProject.
func setAgentResources(ctx context.Context, tx *sql.Tx, projectID, agentID int64, set AgentSettings) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM agent_resources WHERE agent_id = ?", agentID); err != nil {
		return fmt.Errorf("set agent resources: %w", err)
	}
	if set.AllResources {
		return nil
	}
	ids := slices.Clone(set.ResourceIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO agent_resources (agent_id, resource_id)
			SELECT ?, id FROM resources WHERE id = ? AND project_id = ?`,
			agentID, id, projectID)
		if err != nil {
			return fmt.Errorf("set agent resources: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("set agent resources: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("resource %d: %w", id, ErrNotInProject)
		}
	}
	return nil
}

// agentSummaryColumns is the SELECT list scanAgentSummary reads. It
// leaves out the prompt on purpose. The last column is Ready: the agent
// has a model of its own, or the project has a default model.
const agentSummaryColumns = `
	a.id, a.project_id, a.kind, a.name, a.description,
	a.model_id IS NOT NULL OR p.default_model_id IS NOT NULL`

// agentFrom joins each agent to its project, for the default model.
const agentFrom = `
	FROM agents a JOIN projects p ON p.id = a.project_id`

// scanAgentSummary reads one row selected with agentSummaryColumns.
func scanAgentSummary(row scanner) (AgentSummary, error) {
	var a AgentSummary
	if err := row.Scan(&a.ID, &a.ProjectID, &a.Kind, &a.Name, &a.Description, &a.Ready); err != nil {
		return AgentSummary{}, noRows(err)
	}
	return a, nil
}

// Agents returns a project's agents, by name, without their prompts.
func (s *Store) Agents(ctx context.Context, projectID int64) ([]AgentSummary, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+agentSummaryColumns+agentFrom+`
		WHERE a.project_id = ?
		ORDER BY a.name, a.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var list []AgentSummary
	for rows.Next() {
		a, err := scanAgentSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// Agent returns the project's agent with the id, with every setting. It
// returns ErrNotFound when the project has no such agent, so one
// project's URL cannot reach another project's agent.
func (s *Store) Agent(ctx context.Context, projectID, id int64) (Agent, error) {
	a, err := agent(ctx, s.db, projectID, id)
	if err != nil {
		return Agent{}, fmt.Errorf("agent %d: %w", id, err)
	}
	return a, nil
}

// AgentSummary returns the project's agent with the id, without its
// prompt, for pages every member sees. It returns ErrNotFound when the
// project has no such agent.
func (s *Store) AgentSummary(ctx context.Context, projectID, id int64) (AgentSummary, error) {
	row := s.db.QueryRowContext(ctx, "SELECT"+agentSummaryColumns+agentFrom+`
		WHERE a.id = ? AND a.project_id = ?`, id, projectID)
	a, err := scanAgentSummary(row)
	if err != nil {
		return AgentSummary{}, fmt.Errorf("agent %d: %w", id, err)
	}
	return a, nil
}

// querier is a *sql.DB or *sql.Tx, so agent can run inside a
// transaction or outside one.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// agent reads one agent with its picked resources.
func agent(ctx context.Context, q querier, projectID, id int64) (Agent, error) {
	var a Agent
	var modelID sql.NullInt64
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx, "SELECT"+agentSummaryColumns+`,
		a.prompt, a.model_id, a.rate_limit_per_hour, a.all_resources, a.created_at, a.updated_at`+agentFrom+`
		WHERE a.id = ? AND a.project_id = ?`, id, projectID).
		Scan(&a.ID, &a.ProjectID, &a.Kind, &a.Name, &a.Description, &a.Ready,
			&a.Prompt, &modelID, &a.RateLimitPerHour, &a.AllResources, &createdAt, &updatedAt)
	if err != nil {
		return Agent{}, noRows(err)
	}
	a.ModelID = modelID.Int64
	a.CreatedAt = unix(createdAt)
	a.UpdatedAt = unix(updatedAt)

	rows, err := q.QueryContext(ctx,
		"SELECT resource_id FROM agent_resources WHERE agent_id = ? ORDER BY resource_id", id)
	if err != nil {
		return Agent{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return Agent{}, err
		}
		a.ResourceIDs = append(a.ResourceIDs, rid)
	}
	return a, rows.Err()
}

// DeleteAgent removes an agent from its project, with its picked
// resources. It returns ErrNotFound when the project has no such agent.
func (s *Store) DeleteAgent(ctx context.Context, projectID, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM agents WHERE id = ? AND project_id = ?", id, projectID)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return oneRow(res, fmt.Sprintf("agent %d", id))
}
