CREATE TABLE thread_watches (
 id TEXT PRIMARY KEY,
 watcher_thread_id TEXT NOT NULL,
 watcher_run_id TEXT NOT NULL DEFAULT '',
 target_thread_id TEXT NOT NULL,
 target_run_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX thread_watches_by_target ON thread_watches(target_thread_id, target_run_id, status);
CREATE UNIQUE INDEX thread_watches_unique_pending ON thread_watches(watcher_thread_id, target_thread_id, target_run_id) WHERE status = 'pending';
