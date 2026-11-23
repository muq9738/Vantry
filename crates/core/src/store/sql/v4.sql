CREATE TABLE IF NOT EXISTS steering_messages (
 id TEXT PRIMARY KEY,
 command_id TEXT NOT NULL UNIQUE,
 thread_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 message TEXT NOT NULL,
 attachments TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 disposition TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS steering_by_status ON steering_messages(status, created_at, id);
