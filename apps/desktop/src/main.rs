//! `vantry`: parses the 0.1.42 command line, then runs the NSH window on
//! the machine's native backend.

#![cfg_attr(target_os = "windows", windows_subsystem = "windows")]

use std::process::{ExitCode, Termination};

#[cfg(target_os = "linux")]
use nsh_platform_linux as platform;
#[cfg(target_os = "macos")]
use nsh_platform_macos as platform;
#[cfg(target_os = "windows")]
use nsh_platform_windows as platform;
use vantry_desktop::cli::{self, Invocation};

/// The process's exit: the command line's own, or the window's.
enum Exit<T> {
    Code(ExitCode),
    Window(T),
}

impl<T: Termination> Termination for Exit<T> {
    fn report(self) -> ExitCode {
        match self {
            Exit::Code(code) => code,
            Exit::Window(window) => window.report(),
        }
    }
}

#[cfg(any(target_os = "linux", target_os = "macos", target_os = "windows"))]
fn main() -> impl Termination {
    env_logger::init();
    match cli::parse_env() {
        Ok(Invocation::Help(text)) => {
            print!("{text}");
            Exit::Code(ExitCode::SUCCESS)
        }
        Ok(Invocation::Service { mode }) => {
            eprintln!("vantry: {mode} 模式由 vantry-service 提供；请运行 vantry-service {mode} …");
            Exit::Code(ExitCode::from(2))
        }
        Err(err) => {
            eprintln!("vantry: {err}");
            Exit::Code(ExitCode::from(2))
        }
        Ok(Invocation::Desktop(config)) => {
            // `--remote` / `--peer` open the window on that environment; its
            // transport reports what is not wired yet (bridge::RemoteBridge).
            vantry_desktop::set_launch_config(*config);
            Exit::Window(platform::run_app(
                platform::AppSource::Rust(platform::Root::of::<vantry_desktop::App>()),
                platform::AppOptions {
                    title: "Vantry".to_owned(),
                    #[cfg(any(target_os = "linux", target_os = "windows"))]
                    app_id: "io.github.muq9738.vantry".to_owned(),
                    size: (1280, 820),
                    ..Default::default()
                },
            ))
        }
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
compile_error!("The Vantry desktop runs on macOS, Windows or Linux.");
