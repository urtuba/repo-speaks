package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KindGitHubRepo is the kind of a resource that is a GitHub repository.
const KindGitHubRepo = "github_repo"

// ErrResourceExists is returned when the project already has the
// repository with the same branch.
var ErrResourceExists = errors.New("resource already exists")

// Resource is a repository in a project, as pages show it. It never holds
// the token: only SyncSource reads that.
type Resource struct {
	ID        int64
	ProjectID int64
	Kind      string
	Owner     string
	Repo      string
	Branch    string // empty for the repository's default branch
	HasToken  bool
	TokenHint string // the token's last 4 characters, so a builder can tell tokens apart
	// CloneDir names the current clone's folder, or is empty before the
	// first good sync.
	CloneDir     string
	LastCommit   string // the full commit ID of the current clone
	LastSyncedAt time.Time
	LastError    string
	CreatedAt    time.Time
	// OpenJob is "queued" or "running" while a sync is open, and empty
	// otherwise.
	OpenJob string
}

// NewResource describes a repository to add to a project.
type NewResource struct {
	ProjectID int64
	Owner     string
	Repo      string
	Branch    string // empty for the default branch
	Token     string // empty for a public repository
}

// AddResource adds a GitHub repository to a project and queues its first
// sync, in one transaction. The caller has checked the names and the
// token. It returns ErrResourceExists when the project already has the
// repository with the same branch.
func (s *Store) AddResource(ctx context.Context, nr NewResource, now time.Time) (Resource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Resource{}, fmt.Errorf("add resource: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	var taken bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM resources
			WHERE project_id = ? AND owner = ? AND repo = ? AND ifnull(branch, '') = ?)`,
		nr.ProjectID, nr.Owner, nr.Repo, nr.Branch).Scan(&taken); err != nil {
		return Resource{}, fmt.Errorf("add resource: %w", err)
	}
	if taken {
		return Resource{}, ErrResourceExists
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO resources (project_id, kind, owner, repo, branch, token, token_hint, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		nr.ProjectID, KindGitHubRepo, nr.Owner, nr.Repo, nullText(nr.Branch),
		nullText(nr.Token), nullText(tokenHint(nr.Token)), now.Unix()).Scan(&id)
	if err != nil {
		return Resource{}, fmt.Errorf("add resource: %w", err)
	}
	if _, err := queueSync(ctx, tx, id, now); err != nil {
		return Resource{}, fmt.Errorf("add resource: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Resource{}, fmt.Errorf("add resource: %w", err)
	}
	return Resource{
		ID:        id,
		ProjectID: nr.ProjectID,
		Kind:      KindGitHubRepo,
		Owner:     nr.Owner,
		Repo:      nr.Repo,
		Branch:    nr.Branch,
		HasToken:  nr.Token != "",
		TokenHint: tokenHint(nr.Token),
		CreatedAt: unix(now.Unix()),
		OpenJob:   "queued",
	}, nil
}

// resourceColumns is the SELECT list scanResource reads. It leaves out
// the token on purpose. The open job comes from a LEFT JOIN on jobs j.
const resourceColumns = `
	r.id, r.project_id, r.kind, r.owner, r.repo, r.branch, r.token IS NOT NULL, r.token_hint,
	r.clone_dir, r.last_commit, r.last_synced_at, r.last_error, r.created_at, j.status
	FROM resources r
	LEFT JOIN jobs j ON j.resource_id = r.id AND j.kind = '` + JobSyncResource + `'
		AND j.status IN ('queued', 'running')`

// scanResource reads one row selected with resourceColumns.
func scanResource(row scanner) (Resource, error) {
	var r Resource
	var branch, hint, cloneDir, commit, lastError, openJob sql.NullString
	var syncedAt sql.NullInt64
	var createdAt int64
	err := row.Scan(&r.ID, &r.ProjectID, &r.Kind, &r.Owner, &r.Repo, &branch, &r.HasToken, &hint,
		&cloneDir, &commit, &syncedAt, &lastError, &createdAt, &openJob)
	if err != nil {
		return Resource{}, noRows(err)
	}
	r.Branch = branch.String
	r.TokenHint = hint.String
	r.CloneDir = cloneDir.String
	r.LastCommit = commit.String
	if syncedAt.Valid {
		r.LastSyncedAt = unix(syncedAt.Int64)
	}
	r.LastError = lastError.String
	r.CreatedAt = unix(createdAt)
	r.OpenJob = openJob.String
	return r, nil
}

// Resources returns a project's resources, by owner, repository and
// branch.
func (s *Store) Resources(ctx context.Context, projectID int64) ([]Resource, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+resourceColumns+`
		WHERE r.project_id = ?
		ORDER BY r.owner, r.repo, ifnull(r.branch, ''), r.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	defer rows.Close()
	var list []Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("list resources: %w", err)
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// Resource returns the project's resource with the id. It returns
// ErrNotFound when the project has no such resource, so one project's
// URL cannot reach another project's resource.
func (s *Store) Resource(ctx context.Context, projectID, id int64) (Resource, error) {
	row := s.db.QueryRowContext(ctx, "SELECT"+resourceColumns+`
		WHERE r.id = ? AND r.project_id = ?`, id, projectID)
	r, err := scanResource(row)
	if err != nil {
		return Resource{}, fmt.Errorf("resource %d: %w", id, err)
	}
	return r, nil
}

// DeleteResource removes a resource from its project. Its jobs go with it
// (ON DELETE CASCADE). The clone on disk is removed later by the jobs
// loop. It returns ErrNotFound when the project has no such resource.
func (s *Store) DeleteResource(ctx context.Context, projectID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM resources WHERE id = ? AND project_id = ?", id, projectID)
	if err != nil {
		return fmt.Errorf("delete resource: %w", err)
	}
	return oneRow(res, fmt.Sprintf("resource %d", id))
}

// SetResourceToken sets or replaces a resource's token. An empty token
// clears it. The caller has checked the token. It returns ErrNotFound
// when the project has no such resource.
func (s *Store) SetResourceToken(ctx context.Context, projectID, id int64, token string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE resources SET token = ?, token_hint = ? WHERE id = ? AND project_id = ?",
		nullText(token), nullText(tokenHint(token)), id, projectID)
	if err != nil {
		return fmt.Errorf("set resource token: %w", err)
	}
	return oneRow(res, fmt.Sprintf("resource %d", id))
}

// ResourceExists reports whether the project has a resource with the id.
func (s *Store) ResourceExists(ctx context.Context, projectID, id int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM resources WHERE id = ? AND project_id = ?)",
		id, projectID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check resource %d: %w", id, err)
	}
	return ok, nil
}

// SyncSource is what a sync needs to know about a resource. Unlike
// Resource, it holds the token, so only the sync uses it.
type SyncSource struct {
	ResourceID int64
	ProjectID  int64
	Kind       string
	Owner      string
	Repo       string
	Branch     string // empty for the default branch
	Token      string // empty for a public repository
	CloneDir   string // the current clone's folder name, or empty
	// MaxFileBytes is the project's limit: larger files are deleted from
	// the clone.
	MaxFileBytes int64
}

// SyncSource returns what a sync needs to know about the resource, or
// ErrNotFound.
func (s *Store) SyncSource(ctx context.Context, resourceID int64) (SyncSource, error) {
	var src SyncSource
	var branch, token, cloneDir sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.project_id, r.kind, r.owner, r.repo, r.branch, r.token,
			r.clone_dir, p.max_file_bytes
		FROM resources r JOIN projects p ON p.id = r.project_id
		WHERE r.id = ?`, resourceID).
		Scan(&src.ResourceID, &src.ProjectID, &src.Kind, &src.Owner, &src.Repo,
			&branch, &token, &cloneDir, &src.MaxFileBytes)
	if err != nil {
		return SyncSource{}, fmt.Errorf("resource %d: %w", resourceID, noRows(err))
	}
	src.Branch = branch.String
	src.Token = token.String
	src.CloneDir = cloneDir.String
	return src, nil
}

// SaveSync records a good sync: the new clone's folder name, its commit
// and the time. It clears the last error. It returns ErrNotFound when
// the resource was deleted during the sync.
func (s *Store) SaveSync(ctx context.Context, resourceID int64, cloneDir, commit string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE resources
		SET clone_dir = ?, last_commit = ?, last_synced_at = ?, last_error = NULL
		WHERE id = ?`, cloneDir, commit, now.Unix(), resourceID)
	if err != nil {
		return fmt.Errorf("save sync: %w", err)
	}
	return oneRow(res, fmt.Sprintf("resource %d", resourceID))
}

// tokenHint returns the last 4 characters of a token, or nothing for a
// token so short that 4 characters would give most of it away.
func tokenHint(token string) string {
	if len(token) < 12 {
		return ""
	}
	return token[len(token)-4:]
}

// nullText stores an empty string as NULL.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
