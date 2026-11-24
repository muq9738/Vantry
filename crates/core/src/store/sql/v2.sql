ALTER TABLE threads ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'controlled';
ALTER TABLE threads ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN parent_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN parent_task_id TEXT NOT NULL DEFAULT '';

ALTER TABLE runs ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'controlled';
ALTER TABLE runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN parent_task_id TEXT NOT NULL DEFAULT '';

ALTER TABLE attempts ADD COLUMN input TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN provider_cursor INTEGER NOT NULL DEFAULT 0;

ALTER TABLE effect_outbox ADD COLUMN input TEXT NOT NULL DEFAULT '';

UPDATE attempts SET input = COALESCE((SELECT r.input FROM runs r WHERE r.id = attempts.run_id), '')
	WHERE input = '';
UPDATE effect_outbox SET input = COALESCE((SELECT r.input FROM runs r WHERE r.id = effect_outbox.run_id), '')
	WHERE input = '';

ALTER TABLE command_receipts ADD COLUMN payload TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN task_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE command_receipts ADD COLUMN delivery_message_id TEXT NOT NULL DEFAULT '';

DROP INDEX runs_one_active_per_thread;
CREATE UNIQUE INDEX runs_one_active_per_thread
	ON runs(thread_id) WHERE status IN ('queued', 'running', 'waiting_children');
CREATE INDEX runs_by_parent_task ON runs(parent_task_id) WHERE parent_task_id <> '';
CREATE INDEX threads_by_parent_task ON threads(parent_task_id) WHERE parent_task_id <> '';

CREATE TABLE tasks (
	id                  TEXT PRIMARY KEY,
	parent_thread_id    TEXT NOT NULL,
	parent_run_id       TEXT NOT NULL REFERENCES runs(id),
	parent_attempt_id   TEXT NOT NULL DEFAULT '',
	child_thread_id     TEXT NOT NULL,
	child_run_id        TEXT NOT NULL,
	title               TEXT NOT NULL,
	generation          INTEGER NOT NULL,
	status              TEXT NOT NULL,
	delivery_status     TEXT NOT NULL,
	delivery_message_id TEXT NOT NULL DEFAULT '',
	result              TEXT NOT NULL DEFAULT '',
	error               TEXT NOT NULL DEFAULT '',
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL
);
CREATE INDEX tasks_by_parent_run ON tasks(parent_run_id, generation, created_at, id);
CREATE UNIQUE INDEX tasks_by_child_thread ON tasks(child_thread_id);
CREATE INDEX tasks_by_child_run ON tasks(child_run_id);

CREATE TABLE cohorts (
	parent_run_id           TEXT NOT NULL REFERENCES runs(id),
	generation              INTEGER NOT NULL,
	resume_policy           TEXT NOT NULL,
	delivery_message_id     TEXT NOT NULL,
	status                  TEXT NOT NULL,
	continuation_attempt_id TEXT NOT NULL DEFAULT '',
	created_at              INTEGER NOT NULL,
	updated_at              INTEGER NOT NULL,
	PRIMARY KEY (parent_run_id, generation)
);
CREATE UNIQUE INDEX cohorts_delivery_message ON cohorts(delivery_message_id);
CREATE INDEX cohorts_by_status ON cohorts(status, created_at);
