ALTER TABLE threads ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE threads ADD COLUMN forked_from_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN forked_from_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE inputs ADD COLUMN queue_order INTEGER NOT NULL DEFAULT -1;
CREATE INDEX inputs_queue_order ON inputs(thread_id, kind, status, queue_order, created_at, id);
