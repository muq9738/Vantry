//! The `vantry` command line, as the desktop binary takes it.
//!
//! It keeps 0.1.42's entry (`cmd/vantry/main.go`): the first argument picks
//! a mode (`desktop` when left out), flags follow Go's `flag` package
//! (`-data-dir x`, `--data-dir=x`, `-attach-only=true` all work). Modes
//! other than `desktop` belong to `vantry-service` (`core`, `ctl`,
//! `provider-worker`, …): the desktop forwards them to that binary
//! unchanged ([`crate::service`]).

use std::path::{Path, PathBuf};

use clap::{ArgAction, CommandFactory, Parser};

use vantry_protocol::WorkspaceSpec;

use crate::bridge::Endpoint;

/// What the command line asks for.
#[derive(Debug, Clone, PartialEq)]
pub enum Invocation {
    /// Open the desktop.
    Desktop(Box<LaunchConfig>),
    /// Print this text and exit 0.
    Help(String),
    /// A mode `vantry-service` (or `vantry-relay`) runs: forwarded with the
    /// arguments after the mode, as typed.
    Service { mode: String, args: Vec<String> },
}

/// Modes the service binary owns.
const SERVICE_MODES: &[&str] = &[
    "core",
    "ctl",
    "provider-worker",
    "relay",
    "rd-relay",
    "apply-update",
];

/// The desktop's flags (the Go baseline's, plus `--socket` and `--mock`).
#[derive(Debug, Clone, PartialEq, Eq, Parser)]
#[command(
    name = "vantry",
    about = "Vantry 桌面（NSH 原生）",
    disable_help_flag = true,
    disable_version_flag = true
)]
pub struct DesktopArgs {
    /// Core 私有数据目录（权限 0700）
    #[arg(long, value_name = "DIR")]
    pub data_dir: Option<PathBuf>,
    /// 直接连接的 Core socket（默认 <数据目录>/core.sock）
    #[arg(long, value_name = "PATH")]
    pub socket: Option<PathBuf>,
    /// 使用内存演示 Core，不连接真实服务
    #[arg(long, action = ArgAction::Set, num_args = 0..=1, default_missing_value = "true", default_value = "false")]
    pub mock: bool,
    /// 仅连接现有 Core，不自动启动
    #[arg(long, action = ArgAction::Set, num_args = 0..=1, default_missing_value = "true", default_value = "false")]
    pub attach_only: bool,
    /// 通过会话配对码添加的主机 ID
    #[arg(long, value_name = "ID")]
    pub peer: Option<String>,
    /// 远程连接方式：auto、direct 或 relay
    #[arg(long, value_name = "MODE", default_value = "auto")]
    pub peer_mode: String,
    /// 执行器：pi 或 controlled（默认 $VANTRY_PROVIDER 或 pi）
    #[arg(long, value_name = "ID")]
    pub provider: Option<String>,
    /// Pi 模型 provider/model（默认 $VANTRY_MODEL）
    #[arg(long, value_name = "MODEL")]
    pub model: Option<String>,
    /// 受控 Provider 每段输出的间隔（Core 启动参数，原样转交）
    #[arg(long, value_name = "DURATION")]
    pub controlled_delay: Option<String>,
    /// Pi CLI 可执行文件（Core 启动参数，原样转交）
    #[arg(long, value_name = "PATH")]
    pub pi_executable: Option<String>,
    /// 限制为指定 Magpie settings.json 中的 Pi 已选模型（Core 启动参数）
    #[arg(long, value_name = "PATH")]
    pub magpie_settings: Option<String>,
    /// 工作区类型：directory 或 worktree
    #[arg(long, value_name = "KIND")]
    pub workspace_kind: Option<String>,
    /// 工作区目录
    #[arg(long, value_name = "DIR")]
    pub workspace: Option<PathBuf>,
    /// worktree 的 Git 仓库
    #[arg(long, value_name = "DIR")]
    pub repository: Option<PathBuf>,
    /// worktree 分支
    #[arg(long, value_name = "BRANCH")]
    pub branch: Option<String>,
    /// worktree 基准引用（默认 HEAD）
    #[arg(long, value_name = "REF")]
    pub base_ref: Option<String>,
}

/// Where the desktop's Core is, resolved.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    /// The local Core (its socket in [`LaunchConfig::endpoint`]).
    Local,
    /// A paired peer (`--peer`): Wave 2.
    Peer { id: String, mode: String },
}

/// Everything the window needs to start.
#[derive(Debug, Clone, PartialEq)]
pub struct LaunchConfig {
    pub data_dir: PathBuf,
    pub endpoint: Endpoint,
    pub target: Target,
    pub attach_only: bool,
    pub provider_id: String,
    pub model: Option<String>,
    pub workspace: Option<WorkspaceSpec>,
    /// Flags passed through to a Core the desktop starts (`--controlled-delay`, …).
    pub core_flags: Vec<(String, String)>,
}

impl LaunchConfig {
    /// The config for the default data directory (no command line).
    pub fn default_for(data_dir: PathBuf) -> Self {
        LaunchConfig {
            endpoint: Endpoint::Local {
                socket: data_dir.join("core.sock"),
            },
            data_dir,
            target: Target::Local,
            attach_only: false,
            provider_id: "pi".to_owned(),
            model: None,
            workspace: None,
            core_flags: Vec::new(),
        }
    }

    /// Where `desktop-state.json` lives: the data directory for the local
    /// Core, a per-environment folder beside it for a remote one (as
    /// 0.1.42 keeps them), none for the in-memory demo.
    pub fn state_path(&self) -> Option<PathBuf> {
        let file = crate::state::persist::FILE_NAME;
        match (&self.endpoint, &self.target) {
            (Endpoint::Mock, _) => None,
            (_, Target::Local) => Some(self.data_dir.join(file)),
            (_, Target::Peer { id, .. }) => Some(
                self.data_dir
                    .join("environment-state")
                    .join(format!("peer-{}", safe_component(id)))
                    .join(file),
            ),
        }
    }

    /// The workspace directory the dock works in: the command line's, else
    /// the current directory.
    pub fn workspace_root(&self) -> PathBuf {
        self.workspace
            .as_ref()
            .map(|w| if w.path.is_empty() { w.repository.clone() } else { w.path.clone() })
            .filter(|p| !p.is_empty())
            .map(PathBuf::from)
            .or_else(|| std::env::current_dir().ok())
            .unwrap_or_else(|| PathBuf::from("."))
    }

    /// What the header names as the workspace.
    pub fn workspace_label(&self) -> String {
        if let Some(path) = self.workspace.as_ref().map(|w| {
            if w.path.is_empty() {
                w.repository.clone()
            } else {
                w.path.clone()
            }
        }) && !path.is_empty()
        {
            return Path::new(&path)
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or(path);
        }
        match &self.target {
            Target::Local => "本机".to_owned(),
            Target::Peer { id, .. } => id.clone(),
        }
    }
}

/// A command line the desktop cannot run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CliError(pub String);

impl std::fmt::Display for CliError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for CliError {}

/// The process's command line, with the process's environment.
pub fn parse_env() -> Result<Invocation, CliError> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    parse(&args, &|name| {
        std::env::var(name).ok().filter(|v| !v.is_empty())
    })
}

/// `args` (without the program name) with environment `env`.
pub fn parse(
    args: &[String],
    env: &dyn Fn(&str) -> Option<String>,
) -> Result<Invocation, CliError> {
    let (mode, rest) = match args.first() {
        Some(first) if !first.starts_with('-') => (first.as_str(), &args[1..]),
        _ => ("desktop", args),
    };
    match mode {
        "help" | "--help" | "-h" => return Ok(Invocation::Help(usage())),
        "desktop" => {}
        other if SERVICE_MODES.contains(&other) => {
            return Ok(Invocation::Service {
                mode: other.to_owned(),
                args: rest.to_vec(),
            });
        }
        other => {
            return Err(CliError(format!(
                "未知模式 {other:?}；运行 vantry --help 查看用法"
            )));
        }
    }
    if rest
        .iter()
        .any(|a| a == "-h" || a == "-help" || a == "--help")
    {
        return Ok(Invocation::Help(usage()));
    }
    let normalized = std::iter::once("vantry".to_owned()).chain(normalize_go_flags(rest));
    let parsed = DesktopArgs::try_parse_from(normalized).map_err(|e| CliError(e.to_string()))?;
    resolve(parsed, env).map(|config| Invocation::Desktop(Box::new(config)))
}

/// Go's `flag` takes `-name` as well as `--name`: give clap `--name`.
/// Arguments after `--` and values (`-data-dir -weird`) are left alone only
/// when they follow `--`; a value starting with `-` needs `--name=value`,
/// as with Go.
fn normalize_go_flags(args: &[String]) -> Vec<String> {
    let mut out = Vec::with_capacity(args.len());
    let mut literal = false;
    for arg in args {
        if literal {
            out.push(arg.clone());
            continue;
        }
        if arg == "--" {
            literal = true;
            out.push(arg.clone());
        } else if arg.len() > 2 && arg.starts_with('-') && !arg.starts_with("--") {
            out.push(format!("-{arg}"));
        } else {
            out.push(arg.clone());
        }
    }
    out
}

fn resolve(
    args: DesktopArgs,
    env: &dyn Fn(&str) -> Option<String>,
) -> Result<LaunchConfig, CliError> {
    let data_dir = match &args.data_dir {
        Some(dir) => absolute(dir)?,
        None => default_data_dir(env)?,
    };
    let provider_id = args
        .provider
        .clone()
        .or_else(|| env("VANTRY_PROVIDER"))
        .unwrap_or_else(|| "pi".to_owned());
    if provider_id != "pi" && provider_id != "controlled" {
        return Err(CliError("provider 只能为 pi 或 controlled".to_owned()));
    }
    if !matches!(args.peer_mode.as_str(), "auto" | "direct" | "relay") {
        return Err(CliError(
            "peer-mode 只能为 auto、direct 或 relay".to_owned(),
        ));
    }
    let target = match &args.peer {
        Some(id) => Target::Peer { id: id.clone(), mode: args.peer_mode.clone() },
        None => Target::Local,
    };
    if args.mock && target != Target::Local {
        return Err(CliError("--mock 不能与远程连接同时使用".to_owned()));
    }
    if args.socket.is_some() && target != Target::Local {
        return Err(CliError("--socket 只用于本机 Core".to_owned()));
    }
    let endpoint = match &target {
        _ if args.mock => Endpoint::Mock,
        Target::Local => Endpoint::Local {
            socket: match &args.socket {
                Some(socket) => absolute(socket)?,
                None => data_dir.join("core.sock"),
            },
        },
        Target::Peer { id, mode } => Endpoint::Peer {
            id: id.clone(),
            mode: mode.clone(),
        },
    };
    let mut core_flags = Vec::new();
    for (name, value) in [
        ("controlled-delay", &args.controlled_delay),
        ("pi-executable", &args.pi_executable),
        ("magpie-settings", &args.magpie_settings),
    ] {
        if let Some(value) = value {
            core_flags.push((name.to_owned(), value.clone()));
        }
    }
    Ok(LaunchConfig {
        data_dir,
        endpoint,
        target,
        attach_only: args.attach_only,
        provider_id,
        model: args.model.clone().or_else(|| env("VANTRY_MODEL")),
        workspace: workspace_spec(&args)?,
        core_flags,
    })
}

/// `workspaceArguments.spec`.
fn workspace_spec(args: &DesktopArgs) -> Result<Option<WorkspaceSpec>, CliError> {
    if args.workspace_kind.is_none()
        && args.workspace.is_none()
        && args.repository.is_none()
        && args.branch.is_none()
        && args.base_ref.is_none()
    {
        return Ok(None);
    }
    let kind = match args.workspace_kind.as_deref() {
        Some(kind) if !kind.is_empty() => kind.to_owned(),
        _ if args.repository.is_some() => "worktree".to_owned(),
        _ => "directory".to_owned(),
    };
    if kind != "directory" && kind != "worktree" {
        return Err(CliError(
            "workspace-kind 只能为 directory 或 worktree".to_owned(),
        ));
    }
    let path_text = |p: &Option<PathBuf>| -> Result<String, CliError> {
        match p {
            Some(p) => Ok(absolute(p)?.to_string_lossy().into_owned()),
            None => Ok(String::new()),
        }
    };
    let spec = WorkspaceSpec {
        kind,
        path: path_text(&args.workspace)?,
        repository: path_text(&args.repository)?,
        branch: args.branch.clone().unwrap_or_default(),
        base_ref: args.base_ref.clone().unwrap_or_default(),
        ..WorkspaceSpec::default()
    };
    if spec.kind == "worktree" && spec.repository.is_empty() {
        return Err(CliError("worktree 工作区需要 --repository".to_owned()));
    }
    Ok(Some(spec))
}

/// `text` as one safe path component (letters, digits, `-`, `_`, `.`).
fn safe_component(text: &str) -> String {
    let safe: String = text
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.') { c } else { '_' })
        .collect();
    if safe.trim_matches('.').is_empty() {
        "_".to_owned()
    } else {
        safe
    }
}

fn absolute(path: &Path) -> Result<PathBuf, CliError> {
    std::path::absolute(path).map_err(|e| CliError(format!("{}: {e}", path.display())))
}

/// `app.DefaultDataDir`: `$VANTRY_DATA_DIR`, else `<user config dir>/Vantry`.
pub fn default_data_dir(env: &dyn Fn(&str) -> Option<String>) -> Result<PathBuf, CliError> {
    if let Some(dir) = env("VANTRY_DATA_DIR") {
        return absolute(Path::new(&dir));
    }
    user_config_dir(env)
        .map(|base| base.join("Vantry"))
        .ok_or_else(|| CliError("无法确定用户配置目录；请设置 VANTRY_DATA_DIR".to_owned()))
}

/// Go's `os.UserConfigDir`.
fn user_config_dir(env: &dyn Fn(&str) -> Option<String>) -> Option<PathBuf> {
    if cfg!(target_os = "windows") {
        return env("AppData").map(PathBuf::from);
    }
    if cfg!(target_os = "macos") {
        return env("HOME").map(|home| PathBuf::from(home).join("Library/Application Support"));
    }
    env("XDG_CONFIG_HOME")
        .filter(|dir| Path::new(dir).is_absolute())
        .map(PathBuf::from)
        .or_else(|| env("HOME").map(|home| PathBuf::from(home).join(".config")))
}

/// The help text.
pub fn usage() -> String {
    let mut command = DesktopArgs::command();
    let flags = command.render_help().to_string();
    format!(
        "用法：vantry [desktop] [选项]\n\
         \u{20}     vantry core|ctl|provider-worker|relay|rd-relay|apply-update …（转交 vantry-service / vantry-relay 执行）\n\n{flags}"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| (*s).to_owned()).collect()
    }

    fn env_with(pairs: &'static [(&'static str, &'static str)]) -> impl Fn(&str) -> Option<String> {
        move |name| {
            pairs
                .iter()
                .find(|(k, _)| *k == name)
                .map(|(_, v)| (*v).to_owned())
                .or_else(|| (cfg!(windows) && name == "AppData").then(|| String::from(r"C:\Users\test\AppData\Roaming")))
        }
    }

    fn desktop(list: &[&str], env: &dyn Fn(&str) -> Option<String>) -> LaunchConfig {
        match parse(&args(list), env).unwrap() {
            Invocation::Desktop(config) => *config,
            other => panic!("expected the desktop, got {other:?}"),
        }
    }

    #[test]
    fn no_arguments_open_the_local_desktop_in_the_default_data_dir() {
        let config = desktop(
            &[],
            &env_with(&[("HOME", "/Users/u"), ("XDG_CONFIG_HOME", "/x")]),
        );
        let expected = if cfg!(windows) {
            PathBuf::from(r"C:\Users\test\AppData\Roaming\Vantry")
        } else if cfg!(target_os = "macos") {
            PathBuf::from("/Users/u/Library/Application Support/Vantry")
        } else {
            PathBuf::from("/x/Vantry")
        };
        assert_eq!(config.data_dir, expected);
        assert_eq!(
            config.endpoint,
            Endpoint::Local {
                socket: expected.join("core.sock")
            }
        );
        assert_eq!(config.target, Target::Local);
        assert_eq!(config.provider_id, "pi");
        assert!(!config.attach_only);
        assert_eq!(config.workspace_label(), "本机");
    }

    #[test]
    fn vantry_data_dir_wins_over_the_config_dir() {
        let config = desktop(
            &["desktop"],
            &env_with(&[("VANTRY_DATA_DIR", "/srv/vantry")]),
        );
        assert_eq!(config.data_dir, std::path::absolute("/srv/vantry").unwrap());
    }

    #[test]
    fn go_style_single_dash_and_equals_flags_parse() {
        let config = desktop(
            &[
                "desktop",
                "-data-dir",
                "/tmp/v",
                "-attach-only",
                "--model=openai/gpt",
                "-provider=controlled",
                "-controlled-delay",
                "10ms",
            ],
            &env_with(&[]),
        );
        assert_eq!(config.data_dir, std::path::absolute("/tmp/v").unwrap());
        assert!(config.attach_only);
        assert_eq!(config.model.as_deref(), Some("openai/gpt"));
        assert_eq!(config.provider_id, "controlled");
        assert_eq!(
            config.core_flags,
            vec![("controlled-delay".to_owned(), "10ms".to_owned())]
        );
        let config = desktop(&["-attach-only=false"], &env_with(&[("HOME", "/h")]));
        assert!(!config.attach_only);
    }

    #[test]
    fn mock_and_socket_choose_the_endpoint() {
        let config = desktop(&["--mock"], &env_with(&[("HOME", "/h")]));
        assert_eq!(config.endpoint, Endpoint::Mock);
        let config = desktop(
            &["--socket", "/run/core.sock"],
            &env_with(&[("HOME", "/h")]),
        );
        assert_eq!(
            config.endpoint,
            Endpoint::Local {
                socket: std::path::absolute("/run/core.sock").unwrap()
            }
        );
    }

    #[test]
    fn environment_defaults_for_provider_and_model() {
        let config = desktop(
            &[],
            &env_with(&[
                ("HOME", "/h"),
                ("VANTRY_PROVIDER", "controlled"),
                ("VANTRY_MODEL", "m/x"),
            ]),
        );
        assert_eq!(config.provider_id, "controlled");
        assert_eq!(config.model.as_deref(), Some("m/x"));
    }

    #[test]
    fn invalid_values_are_rejected_like_the_go_entry() {
        let env = env_with(&[("HOME", "/h")]);
        assert!(parse(&args(&["-provider", "other"]), &env).is_err());
        assert!(parse(&args(&["-peer-mode", "carrier-pigeon"]), &env).is_err());
        assert!(parse(&args(&["-workspace-kind", "cloud"]), &env).is_err());
        assert!(parse(&args(&["--peer", "p", "--remote", "h"]), &env).is_err());
        assert!(parse(&args(&["--mock", "--remote", "h"]), &env).is_err());
        assert!(parse(&args(&["desktop", "extra"]), &env).is_err());
        let err = parse(&args(&["launch-rockets"]), &env).unwrap_err();
        assert!(err.0.contains("未知模式"));
    }

    #[test]
    fn workspace_flags_build_the_spec() {
        let env = env_with(&[("HOME", "/h")]);
        let config = desktop(&["-workspace", "/code/app"], &env);
        let spec = config.workspace.clone().unwrap();
        assert_eq!(spec.kind, "directory");
        assert_eq!(spec.path, "/code/app");
        assert_eq!(config.workspace_label(), "app");
        let config = desktop(
            &[
                "-repository",
                "/code/repo",
                "-branch",
                "feat/x",
                "-base-ref",
                "main",
            ],
            &env,
        );
        let spec = config.workspace.unwrap();
        assert_eq!(spec.kind, "worktree");
        assert_eq!(spec.repository, "/code/repo");
        assert_eq!(spec.branch, "feat/x");
        assert_eq!(spec.base_ref, "main");
        assert!(parse(&args(&["-workspace-kind", "worktree"]), &env).is_err());
    }

    #[test]
    fn remote_targets_are_recognised() {
        let env = env_with(&[("HOME", "/h")]);
        assert!(parse(&args(&["--remote", "devbox"]), &env).is_err());
        let config = desktop(&["-peer", "p1", "-peer-mode", "relay"], &env);
        assert_eq!(
            config.target,
            Target::Peer {
                id: "p1".into(),
                mode: "relay".into()
            }
        );
        assert_eq!(
            config.endpoint,
            Endpoint::Peer {
                id: "p1".into(),
                mode: "relay".into()
            }
        );
        // The mock keeps no state; the local desktop keeps it in its data dir.
        assert_eq!(desktop(&["--mock"], &env).state_path(), None);
        let local = desktop(&[], &env);
        assert_eq!(local.state_path(), Some(local.data_dir.join("desktop-state.json")));
    }

    #[test]
    fn help_and_service_modes() {
        let env = env_with(&[]);
        for help in [&["help"][..], &["--help"], &["-h"], &["desktop", "-help"]] {
            match parse(&args(help), &env).unwrap() {
                Invocation::Help(text) => {
                    assert!(text.contains("--data-dir"));
                    assert!(text.contains("vantry-service"));
                }
                other => panic!("expected help, got {other:?}"),
            }
        }
        for mode in ["core", "ctl", "provider-worker", "relay", "apply-update"] {
            assert_eq!(
                parse(&args(&[mode, "-x", "--data-dir=/d", "status"]), &env).unwrap(),
                Invocation::Service {
                    mode: mode.into(),
                    args: args(&["-x", "--data-dir=/d", "status"]),
                }
            );
        }
    }
}
