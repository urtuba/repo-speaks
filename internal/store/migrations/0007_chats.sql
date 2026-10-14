-- Chats, their messages, and the sources under each answer.
-- Times are Unix seconds (UTC).

-- A conversation between one member and one agent. With an answer
-- agent, it holds one question and one answer. Chats stay when the
-- agent or the account is deleted: agent_id or user_id becomes NULL,
-- and pages show "deleted agent" or "deleted account". title is the
-- first question, cut short.
CREATE TABLE chats (
	id         INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	agent_id   INTEGER REFERENCES agents (id) ON DELETE SET NULL,
	user_id    INTEGER REFERENCES users (id) ON DELETE SET NULL,
	title      TEXT    NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;

-- A member's chats with one agent, and every chat of a project.
CREATE INDEX chats_project_user ON chats (project_id, user_id, agent_id);
-- Deleting an agent or an account looks up its chats.
CREATE INDEX chats_agent_id ON chats (agent_id);
CREATE INDEX chats_user_id ON chats (user_id);

-- A question (role 'user') or an answer (role 'assistant') in a chat.
--
-- Only answers have a status: 'running' while the agent works, then
-- 'done' or 'failed'. error says why an answer failed. progress holds
-- what the agent did so far, one line per step, such as "Reading
-- owner/repo/path". model_id is the host's ID of the model that
-- answered. The token totals and steps add up every call of the answer.
CREATE TABLE messages (
	id                INTEGER PRIMARY KEY,
	chat_id           INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
	role              TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
	content           TEXT    NOT NULL DEFAULT '',
	status            TEXT    CHECK (status IN ('running', 'done', 'failed')),
	error             TEXT,
	progress          TEXT    NOT NULL DEFAULT '',
	model_id          TEXT,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	steps             INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	finished_at       INTEGER,
	CHECK ((role = 'assistant') = (status IS NOT NULL))
) STRICT;

CREATE INDEX messages_chat_id ON messages (chat_id, id);
-- On start, the server marks the answers still running as failed.
CREATE INDEX messages_running ON messages (status) WHERE status = 'running';

-- A file an answer cites, in the order the answer cites them. commit_id
-- is the commit the clone had when the question started, so the link
-- to GitHub never shifts after a sync. Deleting the resource deletes
-- its sources: the answer keeps its text but loses those links.
CREATE TABLE message_sources (
	message_id  INTEGER NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	resource_id INTEGER NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
	path        TEXT    NOT NULL,
	line_from   INTEGER NOT NULL,
	line_to     INTEGER NOT NULL,
	commit_id   TEXT    NOT NULL,
	PRIMARY KEY (message_id, position),
	CHECK (line_from >= 1 AND line_to >= line_from)
) STRICT;

-- Deleting a resource looks up its sources.
CREATE INDEX message_sources_resource_id ON message_sources (resource_id);
