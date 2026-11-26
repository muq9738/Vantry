CREATE TABLE run_blocks (
 id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL DEFAULT '',
 seq INTEGER NOT NULL,
 kind TEXT NOT NULL,
 ref_id TEXT NOT NULL DEFAULT '',
 text TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 attachments TEXT NOT NULL DEFAULT '[]',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX run_blocks_order ON run_blocks(run_id, seq);
CREATE INDEX run_blocks_by_thread ON run_blocks(thread_id, run_id, seq);
CREATE INDEX run_blocks_by_ref ON run_blocks(run_id, kind, ref_id) WHERE ref_id <> '';
