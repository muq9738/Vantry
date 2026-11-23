CREATE TABLE threads (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE runs (
	id                TEXT PRIMARY KEY,
	thread_id         TEXT NOT NULL REFERENCES threads(id),
	attempt_id        TEXT NOT NULL,
	active_attempt_id TEXT NOT NULL,
	status            TEXT NOT NULL,
	input             TEXT NOT NULL,
	output            TEXT NOT NULL DEFAULT '',
	error             TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL
);

-- A thread has at most one queued/running run. This is the durable backstop for
-- the in-transaction check done by message.send.
CREATE UNIQUE INDEX runs_one_active_per_thread
	ON runs(thread_id) WHERE status IN ('queued', 'running');
CREATE INDEX runs_by_thread ON runs(thread_id, created_at, id);

CREATE TABLE attempts (
	id         TEXT PRIMARY KEY,
	run_id     TEXT NOT NULL REFERENCES runs(id),
	ordinal    INTEGER NOT NULL,
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX attempts_ordinal ON attempts(run_id, ordinal);

CREATE TABLE command_receipts (
	command_id  TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	thread_id   TEXT NOT NULL,
	run_id      TEXT NOT NULL DEFAULT '',
	sequence    INTEGER NOT NULL,
	accepted    INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE events (
	sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
	id         TEXT NOT NULL,
	type       TEXT NOT NULL,
	thread_id  TEXT NOT NULL,
	run_id     TEXT NOT NULL DEFAULT '',
	attempt_id TEXT NOT NULL DEFAULT '',
	timestamp  INTEGER NOT NULL,
	data       BLOB NOT NULL
);

CREATE TABLE effect_outbox (
	id            TEXT PRIMARY KEY,
	kind          TEXT NOT NULL,
	thread_id     TEXT NOT NULL,
	run_id        TEXT NOT NULL,
	attempt_id    TEXT NOT NULL,
	status        TEXT NOT NULL,
	attempt_count INTEGER NOT NULL DEFAULT 0,
	claim_token   TEXT NOT NULL DEFAULT '',
	lease_until   INTEGER NOT NULL DEFAULT 0,
	last_error    TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX effect_outbox_by_status ON effect_outbox(status, created_at, id);
