use crate::{
    runner::{Emit, StartMode},
    store::projection::*,
    *,
};
use rusqlite::{Connection, OptionalExtension, params};
use std::{
    sync::{Arc, atomic::Ordering},
    time::Duration,
};
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;
use vantry_protocol::*;
mod ingest;
pub(crate) fn enqueue_effect(
    db: &Connection,
    kind: &str,
    thread: &str,
    run: &str,
    att: &str,
    payload: &str,
) -> Result<()> {
    db.execute(
        "INSERT INTO effect_outbox(id,kind,thread_id,run_id,attempt_id,status,input,created_at,
             updated_at) VALUES(?,?,?,?,?,'pending',?,?,?)",
        params![id("eff"), kind, thread, run, att, payload, now(), now()],
    )?;
    Ok(())
}
pub(crate) fn provider_request(db: &Connection, r: &Run) -> Result<ProviderRequest> {
    let (message, after_sequence) = db.query_row(
        "SELECT input,provider_cursor FROM attempts WHERE id=?",
        [&r.attempt_id],
        |r| Ok((r.get(0)?, r.get(1)?)),
    )?;
    Ok(ProviderRequest {
        message,
        after_sequence,
        run_id: r.id.clone(),
        thread_id: r.thread_id.clone(),
        attempt_id: r.attempt_id.clone(),
        sender_thread_id: r.sender_thread_id.clone(),
        sender_title: db
            .query_row(
                "SELECT title FROM threads WHERE id=?",
                [&r.sender_thread_id],
                |r| r.get(0),
            )
            .optional()?
            .unwrap_or_default(),
        parent_task_id: db.query_row(
            "SELECT parent_task_id FROM runs WHERE id=?",
            [&r.id],
            |r| r.get(0),
        )?,
        attachments: (!r.attachments.is_empty()).then(|| r.attachments.clone()),
        provider_id: r.provider_id.clone(),
        model: r.model.clone(),
        workspace: r.workspace.clone(),
        approval_policy: r.approval_policy.clone(),
        interaction_mode: r.interaction_mode.clone(),
    })
}
struct Owner(Core);
impl Drop for Owner {
    fn drop(&mut self) {
        self.0.0.scheduler.store(false, Ordering::Release);
    }
}
struct Claim {
    id: String,
    token: String,
    request: ProviderRequest,
    mode: StartMode,
}
impl Core {
    fn own_scheduler(&self) -> Result<Owner> {
        self.0
            .scheduler
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .map_err(|_| conflict("scheduler already active"))?;
        Ok(Owner(self.clone()))
    }
    pub async fn recover_on_startup(&self, provider: &dyn DurableProvider) -> Result<()> {
        let _owner = self.own_scheduler()?;
        self.recover(provider).await
    }
    async fn recover(&self, provider: &dyn DurableProvider) -> Result<()> {
        let runs: Vec<Run> = self.read(|db| {
            rows(
                db,
                "SELECT * FROM runs WHERE status IN ('running','queued') AND NOT EXISTS(SELECT 1 FROM
             effect_outbox e WHERE e.attempt_id=runs.active_attempt_id AND e.kind='provider.run' AND
             e.status='pending' AND e.attempt_count=0 AND runs.status='queued')",
                [],
            )
        })?;
        for r in runs {
            let keep = match self.read(|db| provider_request(db, &r)) {
                Ok(mut request) => {
                    request.message.clear();
                    request.attachments = None;
                    provider.recoverable(&request).await
                }
                Err(e) if e.code() == ErrorCode::NotFound => false,
                Err(e) => return Err(e),
            };
            self.write(|db| {
                let current = run(db, &r.id)?;
                if current.attempt_id != r.attempt_id || current.status.terminal() {
                    return Ok(());
                }
                if keep {
                    let n = db.execute(
                        "UPDATE effect_outbox SET status='pending',claim_token='',lease_until=0,
             attempt_count=MAX(1,attempt_count) WHERE attempt_id=? AND kind='provider.run' AND
             status IN ('claimed','pending')",
                        [&r.attempt_id],
                    )?;
                    if n > 0 {
                        return Ok(());
                    }
                }
                db.execute(
                    "UPDATE effect_outbox SET status='abandoned',claim_token='',last_error='delivery unknown'
             WHERE attempt_id=? AND kind='provider.run' AND status IN ('claimed','pending')",
                    [&r.attempt_id],
                )?;
                finish(db, &r, "interrupted", "worker is unrecoverable or submission state is unknown")
            })?;
        }
        self.write(|db| {
            db.execute(
                "UPDATE effect_outbox SET status='pending',claim_token='' WHERE kind IN ('provider.stop',
             'provider.control') AND status='claimed'",
                [],
            )?;
            let groups: Vec<Cohort> = rows(
                db,
                "SELECT * FROM cohorts WHERE status IN ('pending','delivered') OR (status='acknowledged'
             AND continuation_attempt_id='') ORDER BY created_at",
                [],
            )?;
            for g in groups {
                tasks::sync(db, &run(db, &g.parent_run_id)?, g.generation)?;
            }
            db.execute("UPDATE effect_outbox SET status='abandoned',claim_token='',last_error='stale attempt' WHERE kind='provider.run' AND status IN ('claimed','pending') AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.id=effect_outbox.run_id AND r.active_attempt_id=effect_outbox.attempt_id AND r.status IN ('queued','running'))", [])?;
            tasks::watches::recover(db)?;
            for t in strings(db, "SELECT DISTINCT thread_id FROM inputs WHERE kind='queue' AND status='queued'", [])? {
                commands::dequeue(db, &t)?;
            }
            Ok(())
        })
    }
    /// Owns observers until cancellation. Dropping an observer never sends Stop.
    pub async fn run_scheduler(
        &self,
        provider: Arc<dyn DurableProvider>,
        stop: CancellationToken,
    ) -> Result<()> {
        let _owner = self.own_scheduler()?;
        self.recover(provider.as_ref()).await?;
        self.run_loop(provider, stop).await
    }
    #[cfg(unix)]
    /// Recover outstanding attempts before accepting the first HTTP request.
    pub async fn serve(
        &self,
        provider: Arc<dyn DurableProvider>,
        stop: CancellationToken,
    ) -> Result<()> {
        let _owner = self.own_scheduler()?;
        self.recover(provider.as_ref()).await?;
        let server = crate::server::Server::bind(self.clone()).await?;
        let lifetime = stop.child_token();
        let serving = server.serve(lifetime.clone());
        let working = self.run_loop(provider, lifetime.clone());
        tokio::pin!(serving, working);
        tokio::select! {
            result=&mut serving => { lifetime.cancel(); result.and(working.await) },
            result=&mut working => { lifetime.cancel(); result.and(serving.await) },
        }
    }
    async fn run_loop(
        &self,
        provider: Arc<dyn DurableProvider>,
        stop: CancellationToken,
    ) -> Result<()> {
        let mut jobs = JoinSet::new();
        let result = self.schedule(&provider, &stop, &mut jobs).await;
        jobs.shutdown().await;
        match result {
            Err(e) if e.code() == ErrorCode::NotReady && self.0.shutdown.is_cancelled() => Ok(()),
            other => other,
        }
    }
    async fn schedule(
        &self,
        provider: &Arc<dyn DurableProvider>,
        stop: &CancellationToken,
        jobs: &mut JoinSet<Result<()>>,
    ) -> Result<()> {
        let mut changes = self.0.changed.subscribe();
        loop {
            if stop.is_cancelled() || self.0.shutdown.is_cancelled() {
                break;
            }
            self.write(expire_requests)?;
            for _ in 0..32 {
                let side = self.claim_side()?;
                if let Some(side) = side {
                    let core = self.clone();
                    let p = provider.clone();
                    jobs.spawn(async move { core.handoff(p.as_ref(), side).await });
                    continue;
                }
                let Some(claim) = self.claim()? else { break };
                let core = self.clone();
                let p = provider.clone();
                jobs.spawn(async move { core.execute(p.as_ref(), claim).await });
            }
            tokio::select! {
                _=stop.cancelled()=>break,
                _=self.0.shutdown.cancelled()=>break,
                _=changes.changed()=>{},
                _=tokio::time::sleep(Duration::from_millis(100))=>{},
                Some(result)=jobs.join_next(),if !jobs.is_empty()=>{
                    result.map_err(|e|Error::Api(ErrorCode::InternalError,e.to_string()))??;
                }
            }
        }
        Ok(())
    }
    fn claim(&self) -> Result<Option<Claim>> {
        self.write(|db| {
            let row = db
                .query_row(
                    "SELECT e.id,e.run_id,e.attempt_count FROM effect_outbox e JOIN runs r ON r.id=e.run_id
             AND r.active_attempt_id=e.attempt_id WHERE e.kind='provider.run' AND e.status='pending'
             AND r.status IN ('running','queued') ORDER BY e.created_at,e.id LIMIT 1",
                    [],
                    |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?, r.get::<_, i64>(2)?)),
                )
                .optional()?;
            let Some((key, run_id, count)) = row else { return Ok(None) };
            let r = run(db, &run_id)?;
            let mut request = provider_request(db, &r)?;
            let token = id("claim");
            let mode = if count > 0 || r.status == RunStatus::Running {
                request.message.clear();
                request.attachments = None;
                StartMode::Attach
            } else {
                StartMode::Submit
            };
            db.execute(
                "UPDATE effect_outbox SET status='claimed',claim_token=?,attempt_count=attempt_count+1,
             updated_at=? WHERE id=?",
                params![token, now(), key],
            )?;
            if r.status != RunStatus::Running {
                db.execute("UPDATE runs SET status='running',updated_at=? WHERE id=?", params![now(), r.id])?;
                db.execute("UPDATE attempts SET status='running',updated_at=? WHERE id=?", params![now(), r.attempt_id])?;
                run_event(db, &r.id, "run.started")?;
                tasks::sync_child(db, &r.id)?;
            }
            Ok(Some(Claim { id: key, token, request, mode }))
        })
    }
    async fn execute(&self, p: &dyn DurableProvider, claim: Claim) -> Result<()> {
        let claim = Arc::new(claim);
        let core = self.clone();
        let observer = claim.clone();
        let emit: Emit = Arc::new(move |e| core.ingest(&observer, e));
        if !self.read(|db| current(db, &claim))? {
            return Ok(());
        }
        let result = p.run_events(claim.request.clone(), claim.mode, emit).await;
        if matches!(result, Err(ProviderError::Detached)) {
            return Ok(());
        }
        self.write(|db| {
            if !current(db, &claim)? {
                return Ok(());
            }
            let (status, reason) = match result {
                Ok(()) => ("completed", String::new()),
                Err(ProviderError::Interrupted(reason)) => ("interrupted", reason),
                Err(ProviderError::ExecutionCancelled) => ("cancelled", String::new()),
                Err(e) => ("failed", e.to_string()),
            };
            db.execute(
                "UPDATE effect_outbox SET status='settled',updated_at=?,last_error=? WHERE id=? AND
             claim_token=?",
                params![now(), reason, claim.id, claim.token],
            )?;
            finish(db, &run(db, &claim.request.run_id)?, status, &reason)
        })
    }
    fn ingest(&self, claim: &Claim, e: ProviderEvent) -> Result<()> {
        self.write(|db| {
            if !current(db, claim)? {
                return Err(conflict("stale attempt claim"));
            }
            let r = &claim.request;
            let cursor: i64 = db.query_row(
                "SELECT provider_cursor FROM attempts WHERE id=?",
                [&r.attempt_id],
                |r| r.get(0),
            )?;
            if e.sequence <= 0 {
                return Err(invalid("provider sequence must be positive"));
            }
            if e.sequence <= cursor {
                return Ok(());
            }
            if e.sequence != cursor + 1 {
                return Err(conflict("gap in provider event sequence"));
            }
            ingest::project(db, r, &e)?;
            telemetry::observe(db, r, &e)?;
            db.execute(
                "UPDATE attempts SET provider_cursor=?,updated_at=? WHERE id=?",
                params![e.sequence, now(), r.attempt_id],
            )?;
            Ok(())
        })
    }
    fn claim_side(&self) -> Result<Option<SideEffect>> {
        self.write(|db| {
            let row = db
                .query_row(
                    "SELECT id,kind,input FROM effect_outbox WHERE kind IN ('provider.stop',
             'provider.control') AND status='pending' AND lease_until<=? ORDER BY created_at,id
             LIMIT 1",
                    [now()],
                    |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?, r.get::<_, String>(2)?)),
                )
                .optional()?;
            let Some((key, kind, payload)) = row else { return Ok(None) };
            let (request, control) = if kind == "provider.stop" {
                (serde_json::from_str(&payload)?, None)
            } else {
                let intent: ControlIntent = serde_json::from_str(&payload)?;
                (intent.request, Some(intent.control))
            };
            let token = id("claim");
            db.execute(
                "UPDATE effect_outbox SET status='claimed',claim_token=?,attempt_count=attempt_count+1
             WHERE id=?",
                params![token, key],
            )?;
            Ok(Some(SideEffect { key, token, request, control }))
        })
    }
    async fn handoff(&self, p: &dyn DurableProvider, s: SideEffect) -> Result<()> {
        let result = if let Some(c) = &s.control {
            let current = self.read(|db| Ok(db.query_row("SELECT EXISTS(SELECT 1 FROM effect_outbox e JOIN runs r ON r.id=e.run_id AND r.active_attempt_id=e.attempt_id WHERE e.id=? AND e.status='claimed' AND e.claim_token=? AND r.status='running')", params![s.key, s.token], |r| r.get::<_, bool>(0))?))?;
            if !current {
                self.write(|db| {
                    db.execute("UPDATE effect_outbox SET status='cancelled',last_error='attempt no longer active' WHERE id=? AND status='claimed' AND claim_token=?", params![s.key, s.token])?;
                    Ok(())
                })?;
                return Ok(());
            }
            p.control(&s.request, c).await
        } else {
            p.stop(&s.request).await
        };
        self.write(|db| {
            let (state, reason) = match result {
                Ok(()) => ("settled", String::new()),
                Err(e) => ("pending", e.to_string()),
            };
            db.execute(
                "UPDATE effect_outbox SET status=?,last_error=?,lease_until=?,updated_at=? WHERE id=?
             AND claim_token=? AND status='claimed'",
                params![state, reason, now() + 500_000_000, now(), s.key, s.token],
            )?;
            Ok(())
        })
    }
}
#[derive(serde::Deserialize)]
struct ControlIntent {
    request: ProviderRequest,
    control: ControlResponse,
}
struct SideEffect {
    key: String,
    token: String,
    request: ProviderRequest,
    control: Option<ControlResponse>,
}
fn current(db: &Connection, c: &Claim) -> Result<bool> {
    Ok(db.query_row(
        "SELECT EXISTS(SELECT 1 FROM effect_outbox e JOIN runs r ON r.id=e.run_id AND
             r.active_attempt_id=e.attempt_id WHERE e.id=? AND e.status='claimed' AND
             e.claim_token=? AND r.status='running')",
        params![c.id, c.token],
        |r| r.get(0),
    )?)
}
fn expire_requests(db: &rusqlite::Transaction) -> Result<()> {
    for key in strings(
        db,
        "SELECT id FROM provider_requests WHERE status='pending' AND expires_at>0 AND
             expires_at<=?",
        [now()],
    )? {
        let req: PendingRequest = one(db, "SELECT * FROM provider_requests WHERE id=?", [&key])?;
        db.execute(
            "UPDATE provider_requests SET status='expired',updated_at=?,error='dialog deadline elapsed'
             WHERE id=?",
            params![now(), key],
        )?;
        request_event(db, &key)?;
        let r = run(db, &req.run_id)?;
        if r.status == RunStatus::Running && r.attempt_id == req.attempt_id {
            commands::control(
                db,
                &r,
                ControlResponse {
                    id: key,
                    kind: "response".into(),
                    request_id: req.provider_request_id,
                    cancelled: true,
                    ..Default::default()
                },
            )?;
        }
    }
    Ok(())
}
pub(crate) fn finish(db: &Connection, r: &Run, status: &str, reason: &str) -> Result<()> {
    let projected = if status == "completed" && tasks::has_open(db, &r.id)? {
        "waiting_children"
    } else {
        status
    };
    db.execute(
        "UPDATE runs SET status=?,active_attempt_id='',error=?,updated_at=? WHERE id=?",
        params![projected, reason, now(), r.id],
    )?;
    db.execute(
        "UPDATE attempts SET status=?,updated_at=? WHERE id=? AND status IN ('running','queued')",
        params![status, now(), r.attempt_id],
    )?;
    db.execute(
        "UPDATE effect_outbox SET status='cancelled' WHERE attempt_id=? AND kind='provider.control'
             AND status IN ('pending','claimed')",
        [&r.attempt_id],
    )?;
    for key in strings(
        db,
        "SELECT id FROM provider_requests WHERE attempt_id=? AND status IN ('pending',
             'responding')",
        [&r.attempt_id],
    )? {
        db.execute(
            "UPDATE provider_requests SET status='expired',updated_at=?,error='attempt ended' WHERE
             id=?",
            params![now(), key],
        )?;
        request_event(db, &key)?;
    }
    for key in strings(
        db,
        "SELECT id FROM inputs WHERE attempt_id=? AND status='pending' AND kind='steer'",
        [&r.attempt_id],
    )? {
        db.execute(
            "UPDATE inputs SET status='unknown',updated_at=?,error='delivery was not confirmed'
             WHERE id=?",
            params![now(), key],
        )?;
        input_event(db, &key)?;
    }
    for key in strings(
        db,
        "SELECT id FROM tool_calls WHERE attempt_id=? AND status='running'",
        [&r.attempt_id],
    )? {
        db.execute(
            "UPDATE tool_calls SET status='interrupted',updated_at=? WHERE id=?",
            params![now(), key],
        )?;
        ingest::tool_event(db, &key)?;
    }
    if matches!(r.status, RunStatus::Running | RunStatus::Queued) {
        match provider_request(db, r) {
            Ok(req) => telemetry::finish(db, &req, status)?,
            Err(e) if e.code() == ErrorCode::NotFound => {}
            Err(e) => return Err(e),
        }
    }
    tasks::continuation(db, &r.id)?;
    let attempt: String =
        db.query_row("SELECT attempt_id FROM runs WHERE id=?", [&r.id], |row| {
            row.get(0)
        })?;
    if attempt == r.attempt_id {
        run_event(db, &r.id, &format!("run.{projected}"))?;
    }
    tasks::watches::deliver(db, &run(db, &r.id)?)?;
    tasks::sync_child(db, &r.id)?;
    commands::dequeue(db, &r.thread_id)?;
    Ok(())
}
