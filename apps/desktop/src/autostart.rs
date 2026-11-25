//! Starting the local Core when the desktop opens and it does not answer.
//!
//! 0.1.42's desktop started its Core itself unless told `-attach-only`. The
//! Rust desktop does not link the Core: it asks `vantry-service ensure-core`
//! (which starts it detached, or finds it already starting) and then waits
//! until `GET /v1/health` answers. The connection loop runs afterwards
//! either way; when the start failed, it keeps retrying and the window
//! shows why.

use std::{sync::Arc, time::Duration};

use crate::{
    bridge::{CoreBridge, ErrorCode},
    service::{NotFound, ServiceCommand},
};

/// How long `ensure-core` may take.
pub const ENSURE_TIMEOUT: Duration = Duration::from_secs(60);
/// How long the Core may take to answer after `ensure-core` returned.
pub const READY_TIMEOUT: Duration = Duration::from_secs(20);
const POLL_EVERY: Duration = Duration::from_millis(150);

/// What happened.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Boot {
    /// The Core answered already (or something answers on its socket).
    AlreadyRunning,
    /// `ensure-core` started it and it answers now.
    Started,
}

/// Returns `true` when the running Core reports a different version than this
/// desktop binary and has no active runs, so `ensure-core` should drain and
/// replace it.
pub async fn needs_upgrade_restart(core: Arc<dyn CoreBridge>) -> bool {
    let Ok(Some(status)) = core.status().await else {
        return false;
    };
    status.version != env!("CARGO_PKG_VERSION") && status.active_runs == 0
}

/// Makes sure the local Core answers: probes it, runs `command` when it
/// does not (calling `on_starting` first), then waits for it.
pub async fn ensure_core(
    core: Arc<dyn CoreBridge>,
    command: Result<ServiceCommand, NotFound>,
    on_starting: impl FnOnce(),
) -> Result<Boot, String> {
    match core.health().await {
        Ok(_) => {
            if !needs_upgrade_restart(core.clone()).await {
                return Ok(Boot::AlreadyRunning);
            }
        }
        // Something answers (not ready, a version mismatch, …): the
        // connection loop reports it.
        Err(error) if error.code != ErrorCode::Unavailable => return Ok(Boot::AlreadyRunning),
        Err(_) => {}
    }
    let command = command.map_err(|why| why.0)?;
    on_starting();
    run_ensure(&command).await?;
    wait_ready(&core, READY_TIMEOUT).await?;
    Ok(Boot::Started)
}

/// Runs `ensure-core`; its stderr's end is the error when it fails. The
/// child never outlives the call (killed on timeout or drop).
async fn run_ensure(command: &ServiceCommand) -> Result<(), String> {
    let child = tokio::process::Command::new(&command.program)
        .args(&command.args)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(|e| format!("无法运行 {}：{e}", command.program.display()))?;
    let output = tokio::time::timeout(ENSURE_TIMEOUT, child.wait_with_output())
        .await
        .map_err(|_| format!("启动本地 Core 超过 {} 秒仍未完成", ENSURE_TIMEOUT.as_secs()))?
        .map_err(|e| format!("等待 vantry-service 失败：{e}"))?;
    if output.status.success() {
        return Ok(());
    }
    let stderr = String::from_utf8_lossy(&output.stderr);
    let tail: Vec<&str> = stderr.lines().filter(|l| !l.trim().is_empty()).collect();
    let tail = tail[tail.len().saturating_sub(3)..].join(" / ");
    Err(match output.status.code() {
        Some(code) if tail.is_empty() => format!("vantry-service ensure-core 失败（退出码 {code}）"),
        Some(code) => format!("vantry-service ensure-core 失败（退出码 {code}）：{tail}"),
        None => "vantry-service ensure-core 被信号终止".to_owned(),
    })
}

/// Polls `GET /v1/health` until it answers or `limit` passes.
async fn wait_ready(core: &Arc<dyn CoreBridge>, limit: Duration) -> Result<(), String> {
    let deadline = tokio::time::Instant::now() + limit;
    loop {
        let last = match core.health().await {
            Ok(_) => return Ok(()),
            Err(error) if error.code != ErrorCode::Unavailable => return Ok(()),
            Err(error) => error.message,
        };
        if tokio::time::Instant::now() >= deadline {
            return Err(format!(
                "本地 Core 已启动但 {} 秒内未就绪（{last}）",
                limit.as_secs()
            ));
        }
        tokio::time::sleep(POLL_EVERY).await;
    }
}

#[cfg(all(test, unix))]
mod tests {
    use std::{
        path::PathBuf,
        sync::atomic::{AtomicUsize, Ordering},
    };

    use vantry_protocol::{Command, Health, Receipt, Snapshot, ThreadDetail, ThreadSummaries};

    use super::*;
    use crate::bridge::{BridgeError, BridgeFuture, EventSink};

    /// A Core that answers once `marker` exists.
    struct FakeCore {
        marker: PathBuf,
        probes: AtomicUsize,
    }

    impl CoreBridge for FakeCore {
        fn label(&self) -> String {
            "fake".into()
        }
        fn health(&self) -> BridgeFuture<Health> {
            self.probes.fetch_add(1, Ordering::SeqCst);
            let up = self.marker.exists();
            Box::pin(async move {
                if up {
                    Ok(Health::default())
                } else {
                    Err(BridgeError::unavailable("connection refused"))
                }
            })
        }
        fn snapshot(&self) -> BridgeFuture<Snapshot> {
            Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
        }
        fn thread_summaries(&self) -> BridgeFuture<ThreadSummaries> {
            Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
        }
        fn thread_detail(&self, _: String, _: i64) -> BridgeFuture<ThreadDetail> {
            Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
        }
        fn dispatch(&self, _: Command) -> BridgeFuture<Receipt> {
            Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
        }
        fn subscribe(&self, _: i64, _: EventSink) -> BridgeFuture<()> {
            Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
        }
    }

    fn script(dir: &std::path::Path, body: &str) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let path = dir.join("vantry-service");
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
        path
    }

    fn fake(dir: &std::path::Path) -> Arc<FakeCore> {
        Arc::new(FakeCore {
            marker: dir.join("up"),
            probes: AtomicUsize::new(0),
        })
    }

    #[tokio::test]
    async fn a_running_core_is_left_alone() {
        let dir = tempfile::tempdir().unwrap();
        let core = fake(dir.path());
        std::fs::write(&core.marker, "").unwrap();
        let ran = dir.path().join("ran");
        let command = ServiceCommand {
            program: script(dir.path(), &format!("touch '{}'", ran.display())),
            args: vec![],
        };
        let mut starting = false;
        let boot = ensure_core(core.clone(), Ok(command), || starting = true).await.unwrap();
        assert_eq!(boot, Boot::AlreadyRunning);
        assert!(!starting && !ran.exists());
        dir.close().unwrap();
    }

    #[tokio::test]
    async fn ensure_core_starts_it_with_the_flags_then_waits_for_health() {
        let dir = tempfile::tempdir().unwrap();
        let core = fake(dir.path());
        let args_file = dir.path().join("args");
        let command = ServiceCommand {
            program: script(
                dir.path(),
                &format!(
                    "printf '%s ' \"$@\" > '{}'\n(sleep 0.3; touch '{}') &\nexit 0",
                    args_file.display(),
                    core.marker.display()
                ),
            ),
            args: vec!["ensure-core".into(), "--data-dir".into(), "/d".into()],
        };
        let mut starting = false;
        let boot = ensure_core(core.clone(), Ok(command), || starting = true).await.unwrap();
        assert_eq!(boot, Boot::Started);
        assert!(starting);
        assert_eq!(std::fs::read_to_string(&args_file).unwrap(), "ensure-core --data-dir /d ");
        assert!(core.probes.load(Ordering::SeqCst) >= 2);
        dir.close().unwrap();
    }

    #[tokio::test]
    async fn failures_say_why() {
        let dir = tempfile::tempdir().unwrap();
        let core = fake(dir.path());
        let missing = ensure_core(core.clone(), Err(NotFound("没有找到 vantry-service".into())), || {})
            .await
            .unwrap_err();
        assert!(missing.contains("vantry-service"));
        let command = ServiceCommand {
            program: script(dir.path(), "echo 'data dir locked' >&2\nexit 3"),
            args: vec![],
        };
        let failed = ensure_core(core.clone(), Ok(command), || {}).await.unwrap_err();
        assert!(failed.contains("退出码 3") && failed.contains("data dir locked"), "{failed}");
        let never = wait_ready(&(core.clone() as Arc<dyn CoreBridge>), Duration::from_millis(200))
            .await
            .unwrap_err();
        assert!(never.contains("未就绪"));
        dir.close().unwrap();
    }

    #[tokio::test]
    async fn outdated_idle_core_triggers_ensure_core_while_busy_or_current_stays() {
        struct VersionedCore {
            version: &'static str,
            active_runs: i64,
        }
        impl CoreBridge for VersionedCore {
            fn label(&self) -> String { "v".into() }
            fn health(&self) -> BridgeFuture<Health> {
                Box::pin(async { Ok(Health { version: 1, state: "ready".into() }) })
            }
            fn status(&self) -> BridgeFuture<Option<vantry_protocol::ServerStatus>> {
                let status = vantry_protocol::ServerStatus {
                    version: self.version.into(),
                    active_runs: self.active_runs,
                    ..Default::default()
                };
                Box::pin(async move { Ok(Some(status)) })
            }
            fn snapshot(&self) -> BridgeFuture<Snapshot> {
                Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
            }
            fn thread_summaries(&self) -> BridgeFuture<ThreadSummaries> {
                Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
            }
            fn thread_detail(&self, _: String, _: i64) -> BridgeFuture<ThreadDetail> {
                Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
            }
            fn dispatch(&self, _: Command) -> BridgeFuture<Receipt> {
                Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
            }
            fn subscribe(&self, _: i64, _: EventSink) -> BridgeFuture<()> {
                Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "x")) })
            }
        }

        let dir = tempfile::tempdir().unwrap();
        let ran = dir.path().join("ran");
        let cmd = || ServiceCommand {
            program: script(dir.path(), &format!("touch '{}'", ran.display())),
            args: vec![],
        };

        // Current version stays untouched.
        let current: Arc<dyn CoreBridge> = Arc::new(VersionedCore {
            version: env!("CARGO_PKG_VERSION"),
            active_runs: 0,
        });
        assert_eq!(ensure_core(current, Ok(cmd()), || {}).await.unwrap(), Boot::AlreadyRunning);
        assert!(!ran.exists());

        // Outdated version with active runs stays untouched.
        let busy: Arc<dyn CoreBridge> = Arc::new(VersionedCore {
            version: "0.1.42",
            active_runs: 1,
        });
        assert_eq!(ensure_core(busy, Ok(cmd()), || {}).await.unwrap(), Boot::AlreadyRunning);
        assert!(!ran.exists());

        // Outdated idle Core triggers ensure-core.
        let idle_old: Arc<dyn CoreBridge> = Arc::new(VersionedCore {
            version: "",
            active_runs: 0,
        });
        let mut started = false;
        assert_eq!(
            ensure_core(idle_old, Ok(cmd()), || started = true).await.unwrap(),
            Boot::Started
        );
        assert!(started && ran.exists());
        dir.close().unwrap();
    }
}
