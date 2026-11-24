use crate::{
    commands,
    store::{Store, projection::*},
    *,
};
use rusqlite::{Connection, OptionalExtension, Transaction, params};
use std::{
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;
use vantry_protocol::*;

#[derive(Debug, Clone)]
pub struct CoreOptions {
    pub provider_id: String,
    pub model: String,
    pub workspace: Option<WorkspaceSpec>,
    pub event_retention: i64,
}
impl Default for CoreOptions {
    fn default() -> Self {
        Self {
            provider_id: "pi".into(),
            model: String::new(),
            workspace: None,
            event_retention: 5000,
        }
    }
}
pub(crate) struct Inner {
    pub store: Store,
    pub changed: watch::Sender<i64>,
    pub shutdown: CancellationToken,
    pub scheduler: AtomicBool,
    pub options: CoreOptions,
}
#[derive(Clone)]
pub struct Core(pub(crate) Arc<Inner>);
impl Core {
    pub fn open(directory: impl AsRef<Path>) -> Result<Self> {
        Self::with_retention(directory, 5000)
    }
    pub fn with_retention(directory: impl AsRef<Path>, keep: i64) -> Result<Self> {
        Self::open_with_options(
            directory,
            CoreOptions {
                event_retention: keep,
                ..Default::default()
            },
        )
    }
    pub fn open_with_options(directory: impl AsRef<Path>, options: CoreOptions) -> Result<Self> {
        let store = Store::open(directory)?;
        let (changed, _) = watch::channel(sequence(&store.db.lock().unwrap())?);
        Ok(Self(Arc::new(Inner {
            store,
            changed,
            shutdown: CancellationToken::new(),
            scheduler: AtomicBool::new(false),
            options,
        })))
    }
    pub fn directory(&self) -> &Path {
        &self.0.store.directory
    }
    pub fn backup_to(&self, path: impl AsRef<Path>) -> Result<()> {
        self.0.store.backup_to(path)
    }
    pub(crate) fn read<T>(&self, f: impl FnOnce(&Connection) -> Result<T>) -> Result<T> {
        let db = self.0.store.db.lock().unwrap();
        let tx = db.unchecked_transaction()?;
        let result = f(&tx)?;
        tx.commit()?;
        Ok(result)
    }
    pub(crate) fn write<T>(&self, f: impl FnOnce(&Transaction) -> Result<T>) -> Result<T> {
        let mut db = self.0.store.db.lock().unwrap();
        session::flush(&db, self.directory())?;
        let tx = db.transaction()?;
        if self.0.shutdown.is_cancelled() {
            return Err(Error::Api(ErrorCode::NotReady, "core is draining".into()));
        }
        let out = f(&tx)?;
        let seq = sequence(&tx)?;
        if self.0.options.event_retention >= 0 {
            prune(&tx, seq - self.0.options.event_retention)?;
        }
        tx.commit()?;
        session::flush(&db, self.directory())?;
        // The command is committed. Leave failed ledger exports pending for retry.
        if let Err(err) = telemetry::flush(&db, self.directory()) {
            tracing::warn!(%err,"usage ledger export pending");
        }
        self.0.changed.send_if_modified(|old| {
            let changed = *old != seq;
            *old = seq;
            changed
        });
        Ok(out)
    }
    pub fn dispatch(&self, cmd: &Command) -> Result<Receipt> {
        if cmd.version != 1 {
            return Err(Error::Api(
                ErrorCode::VersionMismatch,
                "expected protocol version 1".into(),
            ));
        }
        if cmd.command_id.trim().is_empty() {
            return Err(invalid("commandId is required"));
        }
        let mut normalized;
        let cmd = if let Some(raw) = &cmd.snoozed_until {
            let time = chrono::DateTime::parse_from_rfc3339(raw)
                .map_err(|_| invalid("invalid snoozedUntil"))?;
            normalized = cmd.clone();
            normalized.snoozed_until = Some(go_timestamp(time));
            &normalized
        } else {
            cmd
        };
        self.write(|db| {
            let prior: Option<String> = db.query_row("SELECT payload FROM command_receipts WHERE command_id=?", [&cmd.command_id], |r| r.get(0)).optional()?;
            if let Some(payload) = prior {
                if serde_json::from_str::<serde_json::Value>(&payload).ok() != Some(serde_json::to_value(cmd)?) {
                    return Err(conflict("commandId already used with different or unverifiable content"));
                }
                return one(db, "SELECT * FROM command_receipts WHERE command_id=?", [&cmd.command_id]);
            }
            let before = sequence(db)?;
            attachments::validate(self.directory(), &cmd.attachments)?;
            let mut receipt = commands::apply(db, cmd, &self.0.options)?;
            if matches!(cmd.kind, CommandKind::ForkThread | CommandKind::RewindThread) {
                let count: i64 = db.query_row("SELECT COUNT(*) FROM runs WHERE thread_id=? AND status='completed'", [&receipt.thread_id], |r| r.get(0))?;
                session::stage(db, self.directory(), &cmd.thread_id, &receipt.thread_id, count)?;
            }
            let seq = sequence(db)?;
            if receipt.sequence == 0 && before != seq {
                receipt.sequence = seq;
            }
            receipt.command_id = cmd.command_id.clone();
            receipt.accepted = true;
            // Go encoding/json escapes HTML and JS line separators. The older reader compares bytes.
            let payload = serde_json::to_string(cmd)?.replace('&', "\\u0026").replace('<', "\\u003c").replace('>', "\\u003e").replace('\u{2028}', "\\u2028").replace('\u{2029}', "\\u2029");
            db.execute(
                "INSERT INTO command_receipts(command_id,kind,fingerprint,thread_id,run_id,sequence,
             accepted,created_at,payload,task_id,generation,delivery_message_id,input_id,request_id,
             message_id,delivery) VALUES(?,?,'',?,?,?,1,?,?,?,?,?,?,?,?,?)",
                params![receipt.command_id, serde_json::to_value(cmd.kind)?.as_str(), receipt.thread_id, receipt.run_id, receipt.sequence, now(), payload, receipt.task_id, receipt.generation, receipt.delivery_message_id, receipt.input_id, receipt.request_id, receipt.message_id, receipt.delivery],
            )?;
            Ok(receipt)
        })
    }
    pub fn needs_initial_title(&self, request: &ProviderRequest) -> Result<bool> {
        self.read(|db| {
            Ok(db.query_row(
                "SELECT EXISTS(SELECT 1 FROM threads WHERE id=?1 AND title='' AND
                 (SELECT id FROM runs WHERE thread_id=?1 AND status NOT IN ('cancelled','failed','interrupted')
                  ORDER BY created_at,id LIMIT 1)=?2)",
                params![request.thread_id, request.run_id],
                |r| r.get(0),
            )?)
        })
    }

    pub fn set_initial_title(&self, request: &ProviderRequest, title: &str) -> Result<()> {
        self.write(|db| {
            if db.execute(
                "UPDATE threads SET title=?3 WHERE id=?1 AND title='' AND
                 (SELECT id FROM runs WHERE thread_id=?1 AND status NOT IN ('cancelled','failed','interrupted')
                  ORDER BY created_at,id LIMIT 1)=?2",
                params![request.thread_id, request.run_id, title],
            )? > 0 {
                thread_event(db, &request.thread_id, "thread.updated")?;
            }
            Ok(())
        })
    }

    pub fn snapshot(&self) -> Result<Snapshot> {
        self.read(snapshot)
    }
    pub fn thread_summaries(&self) -> Result<ThreadSummaries> {
        self.read(thread_summaries)
    }
    pub fn thread_detail(&self, thread_id: &str, limit_runs: i64) -> Result<ThreadDetail> {
        self.read(|db| thread_detail(db, thread_id, limit_runs))
    }
    pub fn status(&self) -> Result<ServerStatus> {
        self.read(|db| {
            let active_runs = db.query_row(
                "SELECT count(*) FROM runs WHERE status IN ('queued','running','waiting_children')",
                [],
                |row| row.get(0),
            )?;
            Ok(ServerStatus {
                protocol_version: 1,
                schema_version: 1,
                pid: i64::from(std::process::id()),
                state: if self.0.shutdown.is_cancelled() {
                    "draining"
                } else {
                    "ready"
                }
                .into(),
                active_runs,
                capabilities: Some(vec![
                    "status.v1".into(),
                    "admin.drain-shutdown.v1".into(),
                    EMBEDDED_PI_CAPABILITY.into(),
                ]),
            })
        })
    }
    pub fn prune_events(&self, keep: i64) -> Result<usize> {
        if keep < 0 {
            return Err(invalid("negative retention"));
        }
        self.write(|db| prune(db, sequence(db)? - keep))
    }
    pub fn events_after(&self, after: i64, limit: u32) -> Result<Vec<CoreEvent>> {
        if after < 0 {
            return Err(invalid("negative cursor"));
        }
        self.read(|db| {
            let boundary: i64 = db.query_row(
                "SELECT int_value FROM core_metadata WHERE key='pruned_through_sequence'",
                [],
                |r| r.get(0),
            )?;
            // Cursor zero means replay the entire retained window in v1.
            if after > 0 && after < boundary {
                return Err(Error::Api(
                    ErrorCode::ResyncRequired,
                    "cursor outside retained event history".into(),
                ));
            }
            rows(
                db,
                "SELECT * FROM events WHERE sequence>? ORDER BY sequence LIMIT ?",
                params![after, limit],
            )
        })
    }
    /// On a busy Core callers may continue working and retry the drain later.
    pub fn drain_and_shutdown(&self) -> Result<()> {
        self.read(|db| {
            let busy: bool = db.query_row(
                "SELECT EXISTS(SELECT 1 FROM runs WHERE status IN ('queued','running',
             'waiting_children')) OR EXISTS(SELECT 1 FROM inputs WHERE status IN ('queued',
             'pending')) OR EXISTS(SELECT 1 FROM provider_requests WHERE status IN ('pending',
             'responding')) OR EXISTS(SELECT 1 FROM tasks WHERE delivery_status IN ('pending',
             'delivered')) OR EXISTS(SELECT 1 FROM cohorts WHERE status='acknowledged' AND
             continuation_attempt_id='') OR EXISTS(SELECT 1 FROM effect_outbox WHERE status IN ('pending',
             'claimed'))",
                [],
                |r| r.get(0),
            )?;
            if busy {
                return Err(conflict("Core still has active work or undelivered results"));
            }
            self.0.shutdown.cancel();
            Ok(())
        })
    }
    pub fn scheduler_running(&self) -> bool {
        self.0.scheduler.load(Ordering::Acquire)
    }
    /// Register a durable completion watch so `watcher_thread_id` is woken with
    /// an inter-thread message when `target_thread_id` (optionally filtered to
    /// `target_run_id`) reaches a terminal run state.
    pub fn register_watch(
        &self,
        watcher_thread_id: &str,
        target_thread_id: &str,
        target_run_id: &str,
    ) -> Result<String> {
        self.write(|db| {
            tasks::watches::register(db, watcher_thread_id, target_thread_id, target_run_id)
        })
    }
}
fn prune(db: &Connection, cut: i64) -> Result<usize> {
    let n = db.execute("DELETE FROM events WHERE sequence<=?", [cut])?;
    if n > 0 {
        db.execute("UPDATE core_metadata SET int_value=MAX(int_value,?) WHERE key='pruned_through_sequence'", [cut])?;
    }
    Ok(n)
}
