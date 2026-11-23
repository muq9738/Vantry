ALTER TABLE threads ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';
ALTER TABLE command_receipts ADD COLUMN message_id TEXT NOT NULL DEFAULT '';
CREATE TABLE queued_messages (
 id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL REFERENCES threads(id),
 message TEXT NOT NULL,
 attachments TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX messages_pending ON queued_messages(status, created_at, id);
