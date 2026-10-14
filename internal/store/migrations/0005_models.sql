-- LLM models and each project's default model.
-- Times are Unix seconds (UTC).

-- A model a project's agents can use: a name the project picks, the base
-- URL of an OpenAI-compatible host, an API key and the host's model ID.
-- Names compare without case, so "Fast" and "fast" cannot both exist.
-- The same model ID may appear twice, with other hosts or keys.
--
-- api_key is in plain text. Only the code that calls the host reads it.
-- Pages show api_key_hint, its last 4 characters.
CREATE TABLE models (
	id           INTEGER PRIMARY KEY,
	project_id   INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	name         TEXT    NOT NULL COLLATE NOCASE,
	base_url     TEXT    NOT NULL,
	api_key      TEXT,
	api_key_hint TEXT,
	model_id     TEXT    NOT NULL,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	UNIQUE (project_id, name)
) STRICT;

-- The model a project uses unless an agent picks another. NULL when the
-- project has none. The app refuses to delete the default model and
-- checks that it belongs to the project. RESTRICT makes the database
-- refuse the delete too, should a code path forget the check. Deleting
-- the project still works: the project row goes first, then its models.
ALTER TABLE projects ADD COLUMN default_model_id INTEGER
	REFERENCES models (id) ON DELETE RESTRICT;

-- Deleting a model looks up the projects that point at it.
CREATE INDEX projects_default_model_id ON projects (default_model_id);
