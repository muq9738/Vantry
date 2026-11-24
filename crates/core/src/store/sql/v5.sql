CREATE TABLE IF NOT EXISTS queued_messages (
 id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL,
 message TEXT NOT NULL,
 attachments TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_pending ON queued_messages(status, created_at, id);
