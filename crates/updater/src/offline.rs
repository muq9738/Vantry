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
}
fn processes(target: &Path) -> Result<Vec<u32>> {
    let output = Command::new("/bin/ps")
        .args(["-ww", "-axo", "pid=,comm="])
        .output()?;
    ensure!(
        output.status.success(),
        "could not inspect running application processes"
    );
    let mut live = Vec::new();
    for line in std::str::from_utf8(&output.stdout)?.lines() {
        if let Some((pid, path)) = line.trim().split_once(char::is_whitespace)
            && Path::new(path.trim()).starts_with(target)
        {
            live.push(pid.parse()?);
        }
    }
    Ok(live)
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
    mut processes: impl FnMut() -> Result<Vec<u32>>,
    mut core: impl FnMut(&Path, bool) -> Result<Option<Status>>,
) -> Result<()> {
    let timeout = if opts.wait_pid.is_some() && opts.wait_timeout.is_zero() {
        Duration::from_secs(20)
    } else {
        opts.wait_timeout
    };
    let start = Instant::now();
    loop {
        let live = processes()?;
        if live.is_empty() {
            return Ok(());
        }
        if let Some(dir) = &opts.data_dir
            && let Ok(Some(status)) = core(dir, false)
            && live == [status.pid]
            && Some(status.pid) != opts.wait_pid
        {
            ensure!(
                status.state == "ready" && status.active_runs == 0,
                "Core is busy"
            );
            core(dir, true)?;
            let start = Instant::now();
            while !processes()?.is_empty() {
                ensure!(
                    start.elapsed() < Duration::from_secs(10),
                    "Core did not exit"
                );
                std::thread::sleep(Duration::from_millis(100));
            }
            return Ok(());
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
    let opts = ApplyOptions {
        data_dir: Some("data".into()),
        wait_pid: Some(42),
        wait_timeout: Duration::from_millis(500),
    };
    let mut live = vec![vec![42, 13], vec![13], vec![]].into_iter();
    let mut drained = false;
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
    )?;
    assert!(drained);
    for (pids, active, refuse) in [
        (vec![42, 13], 0, false),
        (vec![13], 1, false),
        (vec![13], 0, true),
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
        );
        assert!(result.is_err());
        assert_eq!(shutdown_calls, usize::from(refuse));
    }
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
        wait_for_exit(opts, || processes(&target), core)?;
        ensure!(processes(&target)?.is_empty(), "application restarted");
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
pub fn launch_apply(plan: &mut InstallPlan, data: &Path, pid: u32) -> Result<()> {
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
