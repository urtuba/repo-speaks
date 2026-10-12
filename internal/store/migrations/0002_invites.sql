-- Invite links. Only the SHA-256 hash of a link's token is stored, so a
-- copy of the database holds no working links.
--
-- The preset name is checked in Go, not with a CHECK constraint, so a
-- new preset does not need a table rebuild.
CREATE TABLE invites (
	id         INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	token_hash TEXT    NOT NULL UNIQUE,
	preset     TEXT    NOT NULL,
	created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER,
	used_by    INTEGER REFERENCES users (id) ON DELETE SET NULL,
	revoked_at INTEGER
) STRICT;

CREATE INDEX invites_project_id ON invites (project_id);

-- Who invited a member. NULL for members added another way, or when the
-- inviter's account is gone.
ALTER TABLE memberships ADD COLUMN invited_by INTEGER REFERENCES users (id) ON DELETE SET NULL;
