use crate::{
    disk,
    worker::{JobRequest, lock_file},
};
use anyhow::{Result, ensure};
use chrono::Utc;
use pi_ai::auth::oauth::base64url_encode;
use serde_json::json;
use std::path::Path;
use vantry_protocol::ControlResponse;

fn job_request(dir: &Path) -> Result<JobRequest> {
    let job: JobRequest = disk::read(&dir.join("request.json"))?;
    ensure!(
        job.dir()?.canonicalize()? == dir.canonicalize()?,
        "control target and job identity differ"
    );
    Ok(job)
}
/// Publish an immutable control. Its provider `control_result` supplies the delivery outcome.
pub fn deliver_control(dir: &Path, control: &ControlResponse) -> Result<()> {
    job_request(dir)?;
    ensure!(
        !control.id.is_empty() && control.id.len() <= 180,
        "invalid control ID"
    );
    match control.kind.as_str() {
        "steer" => ensure!(!control.message.trim().is_empty(), "empty steer"),
        "response" => ensure!(!control.request_id.is_empty(), "missing provider dialog ID"),
        _ => anyhow::bail!("unknown control kind"),
    }
    let lock = lock_file(&dir.join("control.lock"))?;
    lock.lock()?;
    let controls = dir.join("controls");
    let path = controls.join(format!("{}.json", base64url_encode(control.id.as_bytes())));
    if path.try_exists()? {
        ensure!(
            disk::read::<ControlResponse>(&path)? == *control,
            "control ID conflicts with persisted intent"
        );
        return Ok(());
    }
    for marker in ["closing.json", "result.json", "stop.json"] {
        ensure!(
            !dir.join(marker).try_exists()?,
            "attempt is closing or settled"
        );
    }
    disk::directory(&controls)?;
    disk::write_json(&path, control)
}
/// Stop is a durable execution intent. Dropping a JobEvents observer leaves the worker running.
pub fn request_stop(dir: &Path) -> Result<()> {
    let req = job_request(dir)?.request;
    let lock = lock_file(&dir.join("control.lock"))?;
    lock.lock()?;
    if !dir.join("result.json").try_exists()? && !dir.join("stop.json").try_exists()? {
        disk::write_json(
            &dir.join("stop.json"),
            &json!({"attemptId":req.attempt_id,"runId":req.run_id,"threadId":req.thread_id,"updatedAt":Utc::now()}),
        )?;
    }
    Ok(())
}
