CREATE TABLE steering_messages (
 id TEXT PRIMARY KEY,
 command_id TEXT NOT NULL UNIQUE,
 thread_id TEXT NOT NULL REFERENCES threads(id),
 run_id TEXT NOT NULL REFERENCES runs(id),
 attempt_id TEXT NOT NULL REFERENCES attempts(id),
 message TEXT NOT NULL,
 attachments TEXT NOT NULL,
 status TEXT NOT NULL,
 disposition TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX steering_by_status ON steering_messages(status, created_at, id);
