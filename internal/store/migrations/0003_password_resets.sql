-- Password reset links, made by the instance owner. Like invites, only
-- the SHA-256 hash of a link's token is stored.
CREATE TABLE password_resets (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	token_hash TEXT    NOT NULL UNIQUE,
	created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
) STRICT;

CREATE INDEX password_resets_user_id ON password_resets (user_id);
