-- Agents and the resources each one reads.
-- Times are Unix seconds (UTC).

-- An agent of a project. kind is 'chat' or 'answer', and the app never
-- changes it after the agent is created. Names compare without case, so
-- "Helper" and "helper" cannot both exist in one project.
--
-- prompt holds the agent's instructions, with the answer format of an
-- answer agent. Only members with agents:edit see it.
--
-- all_resources is 1 when the agent reads every resource of the
-- project, ones added later too. With 0, it reads only the resources
-- listed in agent_resources.
--
-- model_id is the agent's own model, or NULL to use the project's
-- default model. The app refuses to delete a model an agent uses. The
-- foreign key refuses too, should a code path forget the check. It is
-- NO ACTION, not RESTRICT: SQLite checks NO ACTION at the end of the
-- statement, and RESTRICT at once. Deleting a project deletes its
-- agents and its models in an order SQLite does not promise. If the
-- models went first, RESTRICT would refuse the delete.
--
-- rate_limit_per_hour is how many questions one member may ask the
-- agent in an hour. The chat will count them in memory.
CREATE TABLE agents (
	id                  INTEGER PRIMARY KEY,
	project_id          INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	kind                TEXT    NOT NULL CHECK (kind IN ('chat', 'answer')),
	name                TEXT    NOT NULL COLLATE NOCASE,
	description         TEXT    NOT NULL DEFAULT '',
	prompt              TEXT    NOT NULL DEFAULT '',
	all_resources       INTEGER NOT NULL DEFAULT 1 CHECK (all_resources IN (0, 1)),
	model_id            INTEGER REFERENCES models (id),
	rate_limit_per_hour INTEGER NOT NULL DEFAULT 20
		CHECK (rate_limit_per_hour BETWEEN 1 AND 1000),
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL,
	UNIQUE (project_id, name)
) STRICT;

-- Deleting a model looks up the agents that use it.
CREATE INDEX agents_model_id ON agents (model_id);

-- The resources an agent reads when all_resources is 0. The app adds
-- only resources of the agent's own project. A deleted resource drops
-- out of every agent's list.
CREATE TABLE agent_resources (
	agent_id    INTEGER NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
	resource_id INTEGER NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
	PRIMARY KEY (agent_id, resource_id)
) STRICT;

-- Deleting a resource looks up the agents that read it.
CREATE INDEX agent_resources_resource_id ON agent_resources (resource_id);
