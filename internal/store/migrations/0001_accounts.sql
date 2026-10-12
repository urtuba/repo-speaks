-- Accounts, sessions, projects and memberships.
-- Times are Unix seconds (UTC).

CREATE TABLE users (
	id            INTEGER PRIMARY KEY,
	username      TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	is_owner      INTEGER NOT NULL DEFAULT 0 CHECK (is_owner IN (0, 1)),
	created_at    INTEGER NOT NULL
) STRICT;

-- An instance has at most one owner.
CREATE UNIQUE INDEX users_one_owner ON users (is_owner) WHERE is_owner = 1;

-- id_hash is the SHA-256 hash of the cookie's token, never the token.
CREATE TABLE sessions (
	id_hash    TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
) STRICT;

CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE TABLE projects (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;

-- A membership can hold no permissions: membership alone lets a person
-- chat with the project's agents.
CREATE TABLE memberships (
	project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	PRIMARY KEY (project_id, user_id)
) STRICT;

CREATE INDEX memberships_user_id ON memberships (user_id);

-- One row per permission a member holds, such as 'members:view'.
CREATE TABLE member_permissions (
	project_id INTEGER NOT NULL,
	user_id    INTEGER NOT NULL,
	permission TEXT    NOT NULL,
	PRIMARY KEY (project_id, user_id, permission),
	FOREIGN KEY (project_id, user_id)
		REFERENCES memberships (project_id, user_id) ON DELETE CASCADE
) STRICT;
