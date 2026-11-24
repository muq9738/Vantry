//! The service binaries beside the desktop: `vantry-service` (Core,
//! provider worker, `ctl`, updates) and `vantry-relay` (the standalone
//! relay).
//!
//! - [`forward_command`]: `vantry core|ctl|provider-worker|relay|rd-relay|
//!   apply-update …` runs the service binary with the same arguments and
//!   exits with its code (0.1.42's `vantry …` entry points keep working);
//! - [`ensure_core_command`]: what the desktop runs when the local Core does
//!   not answer (`vantry-service ensure-core --data-dir … `), with the
//!   desktop's Core flags passed through.
//!
//! Lookup order: the desktop's own directory, a `libexec` beside it (or one
//! level up, as the Windows ZIP lays it out), then `PATH`. A candidate that
//! is the desktop itself is skipped, so a misnamed link cannot loop.

use std::{
    ffi::OsString,
    path::{Path, PathBuf},
};

use crate::{
    bridge::Endpoint,
    cli::{LaunchConfig, Target},
};

/// The service binary's name.
pub const SERVICE_BIN: &str = "vantry-service";
/// The standalone relay's name.
pub const RELAY_BIN: &str = "vantry-relay";

/// A program and its arguments.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServiceCommand {
    pub program: PathBuf,
    pub args: Vec<String>,
}

/// Where binaries are looked for.
#[derive(Debug, Clone, Default)]
pub struct Locator {
    /// The running desktop binary (its directory is searched first).
    pub current_exe: Option<PathBuf>,
    /// `PATH`.
    pub path: Option<OsString>,
}

impl Locator {
    /// The process's own.
    pub fn from_env() -> Self {
        Locator {
            current_exe: std::env::current_exe().ok(),
            path: std::env::var_os("PATH"),
        }
    }

    /// The directories searched, in order.
    pub fn dirs(&self) -> Vec<PathBuf> {
        let mut dirs = Vec::new();
        if let Some(dir) = self.current_exe.as_deref().and_then(Path::parent) {
            dirs.push(dir.to_path_buf());
            dirs.push(dir.join("libexec"));
            if let Some(up) = dir.parent() {
                dirs.push(up.join("libexec"));
            }
        }
        if let Some(path) = &self.path {
            dirs.extend(std::env::split_paths(path));
        }
        dirs
    }

    /// The first executable named `name` (`.exe` on Windows).
    pub fn find(&self, name: &str) -> Option<PathBuf> {
        let file = format!("{name}{}", std::env::consts::EXE_SUFFIX);
        let me = self
            .current_exe
            .as_deref()
            .and_then(|p| std::fs::canonicalize(p).ok());
        self.dirs().into_iter().map(|d| d.join(&file)).find(|candidate| {
            is_executable(candidate)
                && me.as_ref().is_none_or(|me| {
                    std::fs::canonicalize(candidate).map_or(true, |c| &c != me)
                })
        })
    }
}

#[cfg(unix)]
fn is_executable(path: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(path).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
}

#[cfg(not(unix))]
fn is_executable(path: &Path) -> bool {
    path.is_file()
}

/// Why a service mode cannot run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NotFound(pub String);

impl std::fmt::Display for NotFound {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

fn missing(mode: &str, names: &[&str]) -> NotFound {
    NotFound(format!(
        "{mode} 模式由服务程序提供，但在本程序目录、libexec 与 PATH 中都没有找到 {}。\n\
         请确认安装包完整（服务程序与桌面程序位于同一目录），或将其所在目录加入 PATH 后重试。",
        names.join(" 或 ")
    ))
}

/// The command `vantry <mode> <args…>` forwards to. `args` are the
/// arguments after the mode, as typed.
pub fn forward_command(
    mode: &str,
    args: &[String],
    locator: &Locator,
) -> Result<ServiceCommand, NotFound> {
    if mode == "relay" {
        if let Some(program) = locator.find(RELAY_BIN) {
            return Ok(ServiceCommand {
                program,
                args: args.to_vec(),
            });
        }
        return match locator.find(SERVICE_BIN) {
            Some(program) => Ok(ServiceCommand {
                program,
                args: std::iter::once(mode.to_owned()).chain(args.iter().cloned()).collect(),
            }),
            None => Err(missing(mode, &[RELAY_BIN, SERVICE_BIN])),
        };
    }
    match locator.find(SERVICE_BIN) {
        Some(program) => Ok(ServiceCommand {
            program,
            args: std::iter::once(mode.to_owned()).chain(args.iter().cloned()).collect(),
        }),
        None => Err(missing(mode, &[SERVICE_BIN])),
    }
}

/// Runs `command` with this process's stdio and waits: its exit code, or
/// 128 + the signal that ended it (Unix).
pub fn run_and_wait(command: &ServiceCommand) -> std::io::Result<i32> {
    let status = std::process::Command::new(&command.program)
        .args(&command.args)
        .status()?;
    if let Some(code) = status.code() {
        return Ok(code);
    }
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(signal) = status.signal() {
            return Ok(128 + signal);
        }
    }
    Ok(1)
}

/// Becomes `command` (Unix: `exec`, so signals, the process id and the exit
/// code are the service's), or runs it and waits (elsewhere). Returns only
/// when it could not run, or with the code to exit with.
pub fn exec_or_wait(command: &ServiceCommand) -> std::io::Result<i32> {
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        // `exec` only returns on failure.
        let error = std::process::Command::new(&command.program)
            .args(&command.args)
            .exec();
        Err(error)
    }
    #[cfg(not(unix))]
    {
        run_and_wait(command)
    }
}

/// An exit code as the process's (`0..=255`; anything else is a failure).
pub fn exit_byte(code: i32) -> u8 {
    u8::try_from(code).unwrap_or(1)
}

/// Whether the desktop should start the local Core itself when it does not
/// answer: the local target, not `--mock`, not `--attach-only`.
pub fn should_autostart(config: &LaunchConfig) -> bool {
    !config.attach_only
        && config.target == Target::Local
        && matches!(config.endpoint, Endpoint::Local { .. })
}

/// `ensure-core`'s arguments for `config`: the data directory, a socket
/// other than `<data dir>/core.sock`, the provider and model, the Core
/// flags (`--controlled-delay`, `--pi-executable`, `--magpie-settings`) and
/// the workspace.
pub fn ensure_core_args(config: &LaunchConfig) -> Vec<String> {
    let mut args = vec![
        "ensure-core".to_owned(),
        "--data-dir".to_owned(),
        config.data_dir.to_string_lossy().into_owned(),
    ];
    if let Endpoint::Local { socket } = &config.endpoint
        && *socket != config.data_dir.join("core.sock")
    {
        args.push("--socket".into());
        args.push(socket.to_string_lossy().into_owned());
    }
    args.push("--provider".into());
    args.push(config.provider_id.clone());
    if let Some(model) = config.model.as_ref().filter(|m| !m.is_empty()) {
        args.push("--model".into());
        args.push(model.clone());
    }
    for (name, value) in &config.core_flags {
        args.push(format!("--{name}"));
        args.push(value.clone());
    }
    if let Some(spec) = &config.workspace {
        for (name, value) in [
            ("workspace-kind", &spec.kind),
            ("workspace", &spec.path),
            ("repository", &spec.repository),
            ("branch", &spec.branch),
            ("base-ref", &spec.base_ref),
        ] {
            if !value.is_empty() {
                args.push(format!("--{name}"));
                args.push(value.clone());
            }
        }
    }
    args
}

/// The `ensure-core` command for `config`, when the service binary is found.
pub fn ensure_core_command(
    config: &LaunchConfig,
    locator: &Locator,
) -> Result<ServiceCommand, NotFound> {
    let program = locator.find(SERVICE_BIN).ok_or_else(|| {
        NotFound(format!(
            "本地 Core 未运行，且没有找到 {SERVICE_BIN}（已查找本程序目录、libexec 与 PATH）。\
             请重新安装 Vantry，或手动运行 {SERVICE_BIN} core 后点击重试。"
        ))
    })?;
    Ok(ServiceCommand {
        program,
        args: ensure_core_args(config),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cli::{self, Invocation};

    fn write_script(dir: &Path, name: &str, body: &str) -> PathBuf {
        let path = dir.join(format!("{name}{}", std::env::consts::EXE_SUFFIX));
        std::fs::write(&path, body).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        path
    }

    fn launch(args: &[&str]) -> LaunchConfig {
        let args: Vec<String> = args.iter().map(|s| (*s).to_owned()).collect();
        match cli::parse(&args, &|name| matches!(name, "HOME" | "AppData").then(|| "/h".to_owned())).unwrap() {
            Invocation::Desktop(config) => *config,
            other => panic!("expected the desktop, got {other:?}"),
        }
    }

    #[cfg(unix)]
    #[test]
    fn service_modes_find_the_binary_beside_the_desktop_then_path() {
        let app = tempfile::tempdir().unwrap();
        let elsewhere = tempfile::tempdir().unwrap();
        let desktop = write_script(app.path(), "vantry", "#!/bin/sh\n");
        let locator = Locator {
            current_exe: Some(desktop.clone()),
            path: Some(elsewhere.path().as_os_str().to_owned()),
        };
        let err = forward_command("ctl", &[], &locator).unwrap_err();
        assert!(err.0.contains("vantry-service"), "{err}");
        assert!(err.0.contains("PATH"));

        // On PATH only.
        let on_path = write_script(elsewhere.path(), SERVICE_BIN, "#!/bin/sh\n");
        let command = forward_command("ctl", &["status".into(), "--json".into()], &locator).unwrap();
        assert_eq!(command.program, on_path);
        assert_eq!(command.args, ["ctl", "status", "--json"]);

        // Beside the desktop wins.
        let beside = write_script(app.path(), SERVICE_BIN, "#!/bin/sh\n");
        let command = forward_command("core", &["-data-dir".into(), "/d".into()], &locator).unwrap();
        assert_eq!(command.program, beside);
        assert_eq!(command.args, ["core", "-data-dir", "/d"]);

        // relay: vantry-relay first (without the mode), else the service.
        let relay = forward_command("relay", &["serve".into()], &locator).unwrap();
        assert_eq!((relay.program.clone(), relay.args.clone()), (beside.clone(), vec!["relay".to_owned(), "serve".to_owned()]));
        let relay_bin = write_script(elsewhere.path(), RELAY_BIN, "#!/bin/sh\n");
        let relay = forward_command("relay", &["serve".into(), "--config".into(), "/etc/r.json".into()], &locator).unwrap();
        assert_eq!(relay.program, relay_bin);
        assert_eq!(relay.args, ["serve", "--config", "/etc/r.json"]);

        // A file that is not executable is not a candidate.
        std::fs::remove_file(&beside).unwrap();
        std::fs::write(&beside, "not a program").unwrap();
        assert_eq!(forward_command("ctl", &[], &locator).unwrap().program, on_path);
        drop((app, elsewhere));
    }

    #[cfg(unix)]
    #[test]
    fn the_desktop_never_forwards_to_itself() {
        let app = tempfile::tempdir().unwrap();
        let desktop = write_script(app.path(), "vantry", "#!/bin/sh\n");
        let link = app.path().join(SERVICE_BIN);
        std::os::unix::fs::symlink(&desktop, &link).unwrap();
        let locator = Locator {
            current_exe: Some(desktop),
            path: None,
        };
        assert!(forward_command("core", &[], &locator).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn forwarding_passes_arguments_and_the_exit_code() {
        let dir = tempfile::tempdir().unwrap();
        let out = dir.path().join("args.txt");
        let script = write_script(
            dir.path(),
            SERVICE_BIN,
            &format!("#!/bin/sh\nprintf '%s\\n' \"$@\" > '{}'\nexit 7\n", out.display()),
        );
        let command = ServiceCommand {
            program: script,
            args: vec!["ctl".into(), "send".into(), "hello world".into()],
        };
        assert_eq!(run_and_wait(&command).unwrap(), 7);
        assert_eq!(std::fs::read_to_string(&out).unwrap(), "ctl\nsend\nhello world\n");
        let killed = ServiceCommand {
            program: write_script(dir.path(), "self-kill", "#!/bin/sh\nkill -TERM $$\n"),
            args: vec![],
        };
        assert_eq!(run_and_wait(&killed).unwrap(), 128 + 15);
        assert_eq!(exit_byte(7), 7);
        assert_eq!(exit_byte(300), 1);
        assert_eq!(exit_byte(-1), 1);
        dir.close().unwrap();
    }

    #[test]
    fn ensure_core_passes_the_core_flags_through() {
        let config = launch(&[
            "-data-dir",
            "/tmp/vd",
            "-provider",
            "controlled",
            "-model",
            "openai/gpt",
            "-controlled-delay",
            "5ms",
            "-magpie-settings",
            "/m/settings.json",
            "-workspace",
            "/code/app",
        ]);
        assert!(should_autostart(&config));
        let expected_workspace = std::path::absolute("/code/app").unwrap();
        let expected_data = std::path::absolute("/tmp/vd").unwrap();
        assert_eq!(
            ensure_core_args(&config),
            [
                "ensure-core",
                "--data-dir",
                expected_data.to_str().unwrap(),
                "--provider",
                "controlled",
                "--model",
                "openai/gpt",
                "--controlled-delay",
                "5ms",
                "--magpie-settings",
                "/m/settings.json",
                "--workspace-kind",
                "directory",
                "--workspace",
                expected_workspace.to_str().unwrap(),
            ]
        );
        let custom = launch(&["-data-dir", "/tmp/vd", "--socket", "/run/c.sock"]);
        let args = ensure_core_args(&custom);
        let expected_socket = std::path::absolute("/run/c.sock").unwrap();
        assert_eq!(&args[3..5], ["--socket", expected_socket.to_str().unwrap()]);

        assert!(!should_autostart(&launch(&["--mock"])));
        assert!(!should_autostart(&launch(&["--attach-only"])));
        let err = ensure_core_command(&config, &Locator::default()).unwrap_err();
        assert!(err.0.contains("vantry-service"));
    }
}
