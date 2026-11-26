use crate::{
    InstallPlan, UpdateLock,
    install::{swap, verify},
    tree,
};
use anyhow::{Context, Result, bail, ensure};
use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    time::{Duration, Instant},
};

#[derive(Default, Clone)]
pub struct ApplyOptions {
    pub data_dir: Option<PathBuf>,
    pub wait_pid: Option<u32>,
    pub wait_timeout: Duration,
    /// When true, leave a busy local Core (and its active `provider-worker`
    /// subprocesses) running untouched while replacing the `.app` bundle after
    /// the desktop GUI exits. An idle Core is still drained and replaced.
    pub allow_busy_core: bool,
}
#[derive(Debug, Clone, PartialEq, Eq)]
struct LiveProcess {
    pid: u32,
    kind: ProcessKind,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ProcessKind {
    CoreWorker,
    ModelPluginHost,
    Other,
}
fn service_subcommand<'a>(comm: &str, args: &'a str) -> Option<&'a str> {
    let rest = args
        .strip_prefix(comm)
        .or_else(|| args.split_once(char::is_whitespace).map(|(_, rest)| rest))?;
    rest.split_whitespace().next()
}
fn processes(target: &Path) -> Result<Vec<LiveProcess>> {
    let output = Command::new("/bin/ps")
        .args(["-ww", "-axo", "pid=,comm="])
        .output()?;
    ensure!(
        output.status.success(),
        "could not inspect running application processes"
    );
    let mut matched: Vec<(u32, String)> = Vec::new();
    for line in std::str::from_utf8(&output.stdout)?.lines() {
        if let Some((pid, path)) = line.trim().split_once(char::is_whitespace) {
            let comm = path.trim();
            if Path::new(comm).starts_with(target) {
                matched.push((pid.parse()?, comm.to_owned()));
            }
        }
    }
    let service_pids: Vec<String> = matched
        .iter()
        .filter(|(_, comm)| Path::new(comm).file_name().is_some_and(|n| n == "vantry-service"))
        .map(|(pid, _)| pid.to_string())
        .collect();
    let mut subcommands = std::collections::HashMap::new();
    if !service_pids.is_empty() {
        let args_out = Command::new("/bin/ps")
            .args(["-ww", "-p", &service_pids.join(","), "-o", "pid=,args="])
            .output()?;
        if args_out.status.success() {
            for line in std::str::from_utf8(&args_out.stdout)?.lines() {
                if let Some((pid_str, args)) = line.trim().split_once(char::is_whitespace)
                    && let Ok(pid) = pid_str.parse::<u32>()
                    && let Some((_, comm)) = matched.iter().find(|(p, _)| *p == pid)
                    && let Some(sub) = service_subcommand(comm, args.trim())
                {
                    subcommands.insert(pid, sub.to_owned());
                }
            }
        }
    }
    Ok(matched
        .into_iter()
        .map(|(pid, _)| LiveProcess {
            pid,
            kind: match subcommands.get(&pid).map(String::as_str) {
                Some("provider-worker") => ProcessKind::CoreWorker,
                Some("model-plugin-host") => ProcessKind::ModelPluginHost,
                _ => ProcessKind::Other,
            },
        })
        .collect())
}
fn terminate_orphan_plugin_host(pid: u32) {
    #[cfg(unix)]
    {
        let _ = Command::new("/bin/kill")
            .args(["-TERM", &pid.to_string()])
            .status();
    }
    #[cfg(not(unix))]
    {
        let _ = pid;
    }
}
#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Status {
    pid: u32,
    state: String,
    active_runs: u64,
}
#[cfg(unix)]
fn core(data: &Path, shutdown: bool) -> Result<Option<Status>> {
    std::thread::scope(|s| {
        s.spawn(|| -> Result<_> {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()?;
            rt.block_on(async {
                let client = reqwest::Client::builder()
                    .unix_socket(data.join("core.sock"))
                    .timeout(Duration::from_secs(5))
                    .build()?;
                let req = if shutdown {
                    client.post("http://localhost/v1/admin/shutdown-if-idle")
                } else {
                    client.get("http://localhost/v1/status")
                };
                let resp = req.send().await?.error_for_status()?;
                if shutdown {
                    Ok(None)
                } else {
                    Ok(Some(resp.json().await?))
                }
            })
        })
        .join()
        .map_err(|_| anyhow::anyhow!("Core request interrupted"))?
    })
}
#[cfg(not(unix))]
fn core(_: &Path, _: bool) -> Result<Option<Status>> {
    bail!("Unix socket required")
}
fn wait_for_exit(
    opts: &ApplyOptions,
    mut processes: impl FnMut() -> Result<Vec<LiveProcess>>,
    mut core: impl FnMut(&Path, bool) -> Result<Option<Status>>,
    mut stop_orphan_plugin_host: impl FnMut(u32),
) -> Result<Option<u32>> {
    let timeout = if opts.wait_pid.is_some() && opts.wait_timeout.is_zero() {
        Duration::from_secs(20)
    } else {
        opts.wait_timeout
    };
    let start = Instant::now();
    let mut signaled_orphans = std::collections::HashSet::new();
    loop {
        let live = processes()?;
        if live.is_empty() {
            return Ok(None);
        }
        let desktop_alive = opts
            .wait_pid
            .is_some_and(|wait| live.iter().any(|p| p.pid == wait));
        if !desktop_alive
            && let Some(dir) = &opts.data_dir
            && let Ok(Some(status)) = core(dir, false)
            && Some(status.pid) != opts.wait_pid
        {
            let only_core_and_plugins = live
                .iter()
                .all(|p| p.pid == status.pid || p.kind == ProcessKind::ModelPluginHost);
            let only_core_managed = live.iter().all(|p| {
                p.pid == status.pid
                    || matches!(p.kind, ProcessKind::CoreWorker | ProcessKind::ModelPluginHost)
            });
            if only_core_and_plugins && status.state == "ready" && status.active_runs == 0 {
                match core(dir, true) {
                    Ok(_) => {
                        let start = Instant::now();
                        loop {
                            let remaining = processes()?;
                            if remaining.is_empty() {
                                return Ok(None);
                            }
                            let core_gone = !remaining.iter().any(|p| p.pid == status.pid);
                            if core_gone
                                && remaining
                                    .iter()
                                    .all(|p| p.kind == ProcessKind::ModelPluginHost)
                            {
                                // Older Cores did not stop detached model-plugin-host processes on
                                // shutdown; terminate any remaining orphans once Core has exited.
                                for p in &remaining {
                                    if signaled_orphans.insert(p.pid) {
                                        stop_orphan_plugin_host(p.pid);
                                    }
                                }
                            }
                            ensure!(
                                start.elapsed() < Duration::from_secs(10),
                                "Core did not exit"
                            );
                            std::thread::sleep(Duration::from_millis(100));
                        }
                    }
                    Err(_) if opts.allow_busy_core => return Ok(Some(status.pid)),
                    Err(e) => return Err(e),
                }
            }
            if only_core_managed {
                if opts.allow_busy_core {
                    return Ok(Some(status.pid));
                }
                ensure!(
                    status.state == "ready" && status.active_runs == 0,
                    "Core is busy"
                );
            }
        } else if !desktop_alive
            && live
                .iter()
                .all(|p| p.kind == ProcessKind::ModelPluginHost)
        {
            // Core is already gone and only orphaned model-plugin-host processes remain.
            for p in &live {
                if signaled_orphans.insert(p.pid) {
                    stop_orphan_plugin_host(p.pid);
                }
            }
        }
        ensure!(
            start.elapsed() < timeout,
            "quit the application before installing"
        );
        std::thread::sleep(Duration::from_millis(100));
    }
}
#[test]
fn offline_core_drains_only_after_desktop_exit() -> Result<()> {
    let proc = |pid| LiveProcess {
        pid,
        kind: ProcessKind::Other,
    };
    let worker = |pid| LiveProcess {
        pid,
        kind: ProcessKind::CoreWorker,
    };
    let plugin_host = |pid| LiveProcess {
        pid,
        kind: ProcessKind::ModelPluginHost,
    };
    let opts = ApplyOptions {
        data_dir: Some("data".into()),
        wait_pid: Some(42),
        wait_timeout: Duration::from_millis(500),
        allow_busy_core: false,
    };
    let mut live = vec![vec![proc(42), proc(13)], vec![proc(13)], vec![]].into_iter();
    let mut drained = false;
    assert_eq!(
        wait_for_exit(
            &opts,
            || Ok(live.next().unwrap()),
            |_, shutdown| {
                if shutdown {
                    drained = true;
                    Ok(None)
                } else {
                    Ok(Some(Status {
                        pid: 13,
                        state: "ready".into(),
                        active_runs: 0,
                    }))
                }
            },
            |_| {},
        )?,
        None
    );
    assert!(drained);
    // Idle Core with a detached model-plugin-host (e.g. older Core before shutdown cleanup):
    // Core drains first, then the orphaned plugin host is signaled and exits cleanly.
    let mut orphan_live = vec![
        vec![proc(42), proc(13), plugin_host(77)],
        vec![proc(13), plugin_host(77)],
        vec![plugin_host(77)],
        vec![],
    ]
    .into_iter();
    let mut orphan_drained = false;
    let mut terminated = Vec::new();
    assert_eq!(
        wait_for_exit(
            &opts,
            || Ok(orphan_live.next().unwrap()),
            |_, shutdown| {
                if shutdown {
                    orphan_drained = true;
                    Ok(None)
                } else {
                    Ok(Some(Status {
                        pid: 13,
                        state: "ready".into(),
                        active_runs: 0,
                    }))
                }
            },
            |pid| terminated.push(pid),
        )?,
        None
    );
    assert!(orphan_drained);
    assert_eq!(terminated, vec![77]);
    for (pids, active, refuse) in [
        (vec![proc(42), proc(13)], 0, false),
        (vec![proc(13)], 1, false),
        (vec![proc(13)], 0, true),
    ] {
        let mut shutdown_calls = 0;
        let opts = ApplyOptions {
            data_dir: Some("data".into()),
            ..Default::default()
        };
        let result = wait_for_exit(
            &opts,
            || Ok(pids.clone()),
            |_, shutdown| {
                if shutdown {
                    shutdown_calls += 1;
                    bail!("Core refused shutdown");
                }
                Ok(Some(Status {
                    pid: 13,
                    state: "ready".into(),
                    active_runs: active,
                }))
            },
            |_| {},
        );
        assert!(result.is_err());
        assert_eq!(shutdown_calls, usize::from(refuse));
    }
    // With allow_busy_core: wait for desktop to exit, then leave busy Core + worker + plugin host running.
    let busy_opts = ApplyOptions {
        data_dir: Some("data".into()),
        wait_pid: Some(42),
        wait_timeout: Duration::from_millis(500),
        allow_busy_core: true,
    };
    let mut busy_live = vec![
        vec![proc(42), proc(13), worker(99), plugin_host(77)],
        vec![proc(13), worker(99), plugin_host(77)],
    ]
    .into_iter();
    let mut busy_shutdown_calls = 0;
    assert_eq!(
        wait_for_exit(
            &busy_opts,
            || Ok(busy_live.next().unwrap()),
            |_, shutdown| {
                if shutdown {
                    busy_shutdown_calls += 1;
                }
                Ok(Some(Status {
                    pid: 13,
                    state: "ready".into(),
                    active_runs: 1,
                }))
            },
            |_| panic!("must not kill plugin host while Core stays running"),
        )?,
        Some(13)
    );
    assert_eq!(busy_shutdown_calls, 0);
    // With allow_busy_core, if Core reports active_runs==0 but refuses drain (e.g. pending approval),
    // leave Core running instead of failing the restart.
    let refused_opts = ApplyOptions {
        data_dir: Some("data".into()),
        allow_busy_core: true,
        ..Default::default()
    };
    assert_eq!(
        wait_for_exit(
            &refused_opts,
            || Ok(vec![proc(13), plugin_host(77)]),
            |_, shutdown| {
                if shutdown {
                    bail!("Core still has active work");
                }
                Ok(Some(Status {
                    pid: 13,
                    state: "ready".into(),
                    active_runs: 0,
                }))
            },
            |_| panic!("must not kill plugin host when Core stays running"),
        )?,
        Some(13)
    );
    // Non-worker bundle processes (e.g. peer-proxy) must still exit even with allow_busy_core.
    assert!(
        wait_for_exit(
            &refused_opts,
            || Ok(vec![proc(13), proc(88)]),
            |_, _| {
                Ok(Some(Status {
                    pid: 13,
                    state: "ready".into(),
                    active_runs: 1,
                }))
            },
            |_| {},
        )
        .is_err()
    );
    assert_eq!(
        service_subcommand(
            "/Applications/Vantry.app/Contents/MacOS/vantry-service",
            "/Applications/Vantry.app/Contents/MacOS/vantry-service provider-worker --job-dir /tmp/x"
        ),
        Some("provider-worker")
    );
    assert_eq!(
        service_subcommand(
            "/Applications/Vantry.app/Contents/MacOS/vantry-service",
            "/Applications/Vantry.app/Contents/MacOS/vantry-service model-plugin-host --dir /tmp/q"
        ),
        Some("model-plugin-host")
    );
    assert_eq!(
        service_subcommand(
            "/Applications/Vantry.app/Contents/MacOS/vantry-service",
            "/Applications/Vantry.app/Contents/MacOS/vantry-service peer-proxy --peer p1"
        ),
        Some("peer-proxy")
    );
    Ok(())
}
/// Verify the staged payload, drain an idle macOS Core and rename on the target volume.
pub fn apply_staged(staged: &Path, target: &Path, opts: &ApplyOptions) -> Result<()> {
    ensure!(
        staged.is_absolute() && target.is_absolute(),
        "absolute paths required"
    );
    ensure!(
        fs::symlink_metadata(staged)?.is_dir(),
        "invalid staged directory"
    );
    let staged = staged.canonicalize()?;
    let target = target
        .parent()
        .context("missing parent")?
        .canonicalize()?
        .join(target.file_name().context("target name")?);
    ensure!(
        !staged.starts_with(&target) && !target.starts_with(&staged),
        "overlapping update paths"
    );
    let _lock = UpdateLock::acquire(&target)?;
    verify(&staged, &target)?;
    if target
        .extension()
        .is_some_and(|e| e.eq_ignore_ascii_case("app"))
    {
        let kept_core = wait_for_exit(
            opts,
            || processes(&target),
            core,
            terminate_orphan_plugin_host,
        )?;
        let remaining = processes(&target)?;
        let blocking = match kept_core {
            Some(core_pid) => remaining.iter().any(|p| {
                p.pid != core_pid
                    && !matches!(p.kind, ProcessKind::CoreWorker | ProcessKind::ModelPluginHost)
            }),
            None => !remaining.is_empty(),
        };
        ensure!(!blocking, "application restarted");
    }
    swap(&staged, &target)
}
fn service(staged: &Path) -> Result<PathBuf> {
    let dir = staged.join("Contents/MacOS");
    let service = dir.join("vantry-service");
    if service.is_file() {
        return Ok(service);
    }
    let out = Command::new("/usr/libexec/PlistBuddy")
        .args(["-c", "Print :CFBundleExecutable"])
        .arg(staged.join("Contents/Info.plist"))
        .output()?;
    ensure!(out.status.success(), "missing bundle executable");
    let name = std::str::from_utf8(&out.stdout)?.trim();
    ensure!(
        !name.contains('/') && tree::relative(name).is_ok(),
        "invalid bundle executable"
    );
    let exe = dir.join(name);
    ensure!(exe.is_file(), "bundle executable missing");
    Ok(exe)
}
pub(crate) fn installer(root: &Path, staged: &Path, target: &Path, data: &Path) -> Result<PathBuf> {
    let quote = |p: &Path| -> Result<String> {
        Ok(format!(
            "'{}'",
            p.to_str()
                .context("non-UTF-8 installer path")?
                .replace('\'', "'\\''")
        ))
    };
    let command = format!(
        "#!/bin/sh\nset -eu\nexec {} apply-update --staged {} --target {} --data-dir {}\n",
        quote(&service(staged)?)?,
        quote(staged)?,
        quote(target)?,
        quote(data)?
    );
    let path = root.join("安装更新.command");
    fs::write(&path, command)?;
    tree::set_mode(&path, 0o700)?;
    Ok(path)
}
/// Transfer ownership of the staging directory to a detached apply-update process.
pub fn launch_apply(
    plan: &mut InstallPlan,
    data: &Path,
    pid: u32,
    allow_busy_core: bool,
) -> Result<()> {
    verify(plan.staged(), plan.target())?;
    ensure!(
        cfg!(target_os = "macos"),
        "automatic relaunch requires macOS"
    );
    ensure!(
        plan.target().is_absolute()
            && plan
                .target()
                .extension()
                .is_some_and(|e| e.eq_ignore_ascii_case("app")),
        "automatic relaunch requires a target .app"
    );
    let mut cmd = Command::new(service(plan.staged())?);
    cmd.arg("apply-update")
        .arg("--staged")
        .arg(plan.staged())
        .arg("--target")
        .arg(plan.target());
    if !data.as_os_str().is_empty() {
        cmd.arg("--data-dir").arg(data);
    }
    if pid > 0 {
        cmd.arg("--wait-pid")
            .arg(pid.to_string())
            .args(["--wait-timeout", "20s"]);
    }
    if allow_busy_core {
        cmd.arg("--allow-busy-core");
    }
    let log_path = if data.as_os_str().is_empty() {
        std::env::temp_dir().join("vantry-update-apply.log")
    } else {
        fs::create_dir_all(data)?;
        data.join("update-apply.log")
    };
    let mut options = fs::File::options();
    options.create(true).append(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let log = options.open(log_path)?;
    cmd.stdin(Stdio::null())
        .stdout(log.try_clone()?)
        .stderr(log);
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        cmd.process_group(0);
    }
    let mut child = cmd.spawn()?;
    plan.retain_for_worker();
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}
/// Service entry point for the legacy `apply-update` command.
pub fn run_apply(args: &[String], mut output: impl Write) -> Result<()> {
    ensure!(cfg!(target_os = "macos"), "apply-update requires macOS");
    let mut opts = ApplyOptions::default();
    let (mut staged, mut target) = (None, None);
    let mut it = args.iter();
    while let Some(key) = it.next() {
        if key == "--allow-busy-core" {
            opts.allow_busy_core = true;
            continue;
        }
        let value = it.next().context("missing option value")?;
        match key.as_str() {
            "--staged" => staged = Some(Path::new(value)),
            "--target" => target = Some(Path::new(value)),
            "--data-dir" => opts.data_dir = Some(value.into()),
            "--wait-pid" => opts.wait_pid = Some(value.parse()?),
            "--wait-timeout" => {
                opts.wait_timeout = if let Some(ms) = value.strip_suffix("ms") {
                    Duration::from_millis(ms.parse()?)
                } else {
                    Duration::try_from_secs_f64(
                        value
                            .strip_suffix('s')
                            .context("expected seconds")?
                            .parse()?,
                    )
                    .context("invalid duration")?
                };
            }
            _ => bail!("unknown apply-update option {key}"),
        }
    }
    let (staged, target) = (
        staged.context("missing --staged")?,
        target.context("missing --target")?,
    );
    ensure!(
        target.extension().is_some_and(|x| x == "app"),
        "target must be an app"
    );
    apply_staged(staged, target, &opts)?;
    // Only clean the exact staging layout created by InstallPlan.
    if let Some(root) = staged.parent()
        && root
            .file_name()
            .is_some_and(|p| p.to_string_lossy().starts_with(".vantry-update-"))
        && !target.starts_with(root)
    {
        let _ = fs::remove_file(root.join("安装更新.command"));
        let _ = fs::remove_dir(root);
    }
    writeln!(output, "更新已安装，正在重新打开 Vantry。")?;
    if !Command::new("/usr/bin/open")
        .arg(target)
        .status()?
        .success()
    {
        ensure!(
            Command::new("/usr/bin/open")
                .arg("-na")
                .arg(target)
                .status()?
                .success(),
            "update installed, reopen manually"
        );
    }
    Ok(())
}
