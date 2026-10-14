-- Backups this server made, and one open backup job at a time.
-- Times are Unix seconds (UTC).

-- One row per backup, from the Backups page or the backup command.
-- name is the encrypted file's name, size its size in bytes. targets
-- lists the targets that got the file, such as "folder /backups". error
-- is empty when every target got it. A backup that failed before it
-- reached a target has size 0 and no targets.
CREATE TABLE backups (
	id          INTEGER PRIMARY KEY,
	name        TEXT    NOT NULL,
	size        INTEGER NOT NULL DEFAULT 0,
	targets     TEXT    NOT NULL DEFAULT '',
	error       TEXT    NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL,
	finished_at INTEGER NOT NULL
) STRICT;

CREATE INDEX backups_started_at ON backups (started_at);

-- A job that belongs to no resource, such as a backup, has a NULL
-- resource_id. jobs_one_open sees two NULLs as different, so this index
-- keeps one open job per kind among them. Pressing "Back up now" twice
-- queues one backup.
CREATE UNIQUE INDEX jobs_one_open_without_resource
	ON jobs (kind) WHERE resource_id IS NULL AND status IN ('queued', 'running');
