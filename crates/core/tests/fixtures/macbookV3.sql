ALTER TABLE threads ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE runs ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE command_receipts ADD COLUMN input_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
CREATE TABLE inputs (
	id         TEXT PRIMARY KEY,
	thread_id  TEXT NOT NULL,
	run_id     TEXT NOT NULL DEFAULT '',
	attempt_id TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL,
	message    TEXT NOT NULL,
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX inputs_by_thread ON inputs(thread_id, kind, status, created_at, id);
CREATE INDEX inputs_by_attempt ON inputs(attempt_id, kind, status);
CREATE TABLE tool_calls (
	id         TEXT PRIMARY KEY,
	thread_id  TEXT NOT NULL,
	run_id     TEXT NOT NULL,
	attempt_id TEXT NOT NULL,
	name       TEXT NOT NULL,
	arguments  BLOB NOT NULL DEFAULT '',
	output     TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX tool_calls_by_attempt ON tool_calls(attempt_id, status, updated_at);
CREATE TABLE provider_requests (
	id                  TEXT PRIMARY KEY,
	provider_request_id TEXT NOT NULL,
	thread_id           TEXT NOT NULL,
	run_id              TEXT NOT NULL,
	attempt_id          TEXT NOT NULL,
	method              TEXT NOT NULL,
	title               TEXT NOT NULL,
	message             TEXT NOT NULL DEFAULT '',
	options             TEXT NOT NULL DEFAULT '',
	prefill             TEXT NOT NULL DEFAULT '',
	status              TEXT NOT NULL,
	error               TEXT NOT NULL DEFAULT '',
	expires_at          INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL
);
CREATE UNIQUE INDEX provider_requests_provider ON provider_requests(attempt_id, provider_request_id);
CREATE INDEX provider_requests_by_attempt ON provider_requests(attempt_id, status);
CREATE INDEX provider_requests_by_status ON provider_requests(status, expires_at);
