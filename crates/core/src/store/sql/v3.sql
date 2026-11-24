ALTER TABLE threads ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE threads ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE runs ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';

ALTER TABLE command_receipts ADD COLUMN input_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN message_id TEXT NOT NULL DEFAULT '';

-- inputs is the single durable authority for queued and steering messages. A
-- queue input becomes a Run when the thread slot is free; a steer input is a
-- control intent addressed to exactly one attempt.
CREATE TABLE inputs (
	id          TEXT PRIMARY KEY,
	thread_id   TEXT NOT NULL,
	run_id      TEXT NOT NULL DEFAULT '',
	attempt_id  TEXT NOT NULL DEFAULT '',
	kind        TEXT NOT NULL,
	message     TEXT NOT NULL,
	attachments TEXT NOT NULL DEFAULT '[]',
	command_id  TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	disposition TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX inputs_by_thread ON inputs(thread_id, kind, status, created_at, id);
CREATE INDEX inputs_by_attempt ON inputs(attempt_id, kind, status);

-- tool_calls mirrors real Pi tool executions; output is bounded display text.
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

-- provider_requests records Pi dialogs that block the model until answered.
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
