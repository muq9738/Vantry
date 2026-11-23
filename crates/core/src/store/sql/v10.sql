ALTER TABLE threads ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX project_coordinator_path ON threads(project_path) WHERE project_path <> '';
CREATE TABLE project_updates (
 id TEXT PRIMARY KEY,
 source_sequence INTEGER NOT NULL UNIQUE,
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 source_thread_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL DEFAULT '',
 request_id TEXT NOT NULL DEFAULT '',
 title TEXT NOT NULL,
 status TEXT NOT NULL,
 summary TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX project_updates_thread ON project_updates(thread_id, source_sequence);
CREATE UNIQUE INDEX project_updates_milestone ON project_updates(thread_id, run_id, attempt_id, status, request_id);
