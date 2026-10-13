-- Resources, the jobs that sync them, and each project's sync settings.
-- Times are Unix seconds (UTC).

-- How often the timer syncs a project's resources, in hours: 1, 6 or
-- 24. NULL turns the timer off. The choices are checked in Go, so a new
-- one does not need a table rebuild.
ALTER TABLE projects ADD COLUMN sync_interval_hours INTEGER DEFAULT 6;

-- A sync deletes every file larger than this from the clone.
ALTER TABLE projects ADD COLUMN max_file_bytes INTEGER NOT NULL DEFAULT 5242880;

-- A repository in a project. kind is 'github_repo' for now; markdown
-- folders will get their own kind. owner and repo compare without case,
-- like GitHub does. branch is NULL for the repository's default branch.
--
-- token is a GitHub token for a private repository, in plain text. Only
-- the sync reads it. Pages show token_hint, its last 4 characters.
--
-- clone_dir names the current clone, a folder inside
-- <data>/repos/<project_id>/<id>. last_commit is the full commit ID of
-- that clone.
CREATE TABLE resources (
	id             INTEGER PRIMARY KEY,
	project_id     INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	kind           TEXT    NOT NULL,
	owner          TEXT    NOT NULL COLLATE NOCASE,
	repo           TEXT    NOT NULL COLLATE NOCASE,
	branch         TEXT,
	token          TEXT,
	token_hint     TEXT,
	clone_dir      TEXT,
	last_commit    TEXT,
	last_synced_at INTEGER,
	last_error     TEXT,
	created_at     INTEGER NOT NULL
) STRICT;

-- One row per repository and branch in a project. A UNIQUE constraint
-- sees two NULLs as different, so the default branch is indexed as ''.
CREATE UNIQUE INDEX resources_repo_branch
	ON resources (project_id, owner, repo, ifnull(branch, ''));

-- Background work, one row per job. The jobs loop claims the oldest due
-- 'queued' row and sets it to 'running'. A failed try goes back to
-- 'queued' with a later run_after, until the tries run out.
-- resource_id is NULL for jobs that belong to no resource.
CREATE TABLE jobs (
	id          INTEGER PRIMARY KEY,
	kind        TEXT    NOT NULL,
	resource_id INTEGER REFERENCES resources (id) ON DELETE CASCADE,
	status      TEXT    NOT NULL DEFAULT 'queued'
		CHECK (status IN ('queued', 'running', 'done', 'failed')),
	attempts    INTEGER NOT NULL DEFAULT 0,
	run_after   INTEGER NOT NULL,
	locked_at   INTEGER,
	error       TEXT,
	created_at  INTEGER NOT NULL
) STRICT;

-- A resource never has two open jobs of one kind, so pressing
-- "Sync now" twice queues one sync.
CREATE UNIQUE INDEX jobs_one_open
	ON jobs (resource_id, kind) WHERE status IN ('queued', 'running');

CREATE INDEX jobs_resource_id ON jobs (resource_id);
CREATE INDEX jobs_due ON jobs (run_after, id) WHERE status = 'queued';
