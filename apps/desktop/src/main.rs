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
use vantry_desktop::{
    cli::{self, Invocation},
    service,
};

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
        Ok(Invocation::Service { mode, args }) => {
            // The 0.1.42 entry points: the service binary runs them, with
            // the same arguments and exit code.
            let locator = service::Locator::from_env();
            match service::forward_command(&mode, &args, &locator) {
                Ok(command) => match service::exec_or_wait(&command) {
                    Ok(code) => Exit::Code(ExitCode::from(service::exit_byte(code))),
                    Err(error) => {
                        eprintln!(
                            "vantry: 无法启动 {}：{error}",
                            command.program.display()
                        );
                        Exit::Code(ExitCode::from(126))
                    }
                },
                Err(why) => {
                    eprintln!("vantry: {why}");
                    Exit::Code(ExitCode::from(127))
                }
            }
        }
        Err(err) => {
            eprintln!("vantry: {err}");
            Exit::Code(ExitCode::from(2))
        }
        Ok(Invocation::Desktop(config)) => {
            // An explicit peer is the initial conversation scope in the shared window.
            vantry_desktop::set_launch_config(*config);
            Exit::Window(platform::run_app(
                platform::AppSource::Rust(platform::Root::of::<vantry_desktop::App>()),
                platform::AppOptions {
                    title: "Vantry".to_owned(),
                    #[cfg(any(target_os = "linux", target_os = "windows"))]
                    app_id: "io.github.muq9738.vantry".to_owned(),
                    ..Default::default()
                },
            ))
        }
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
compile_error!("The Vantry desktop runs on macOS, Windows or Linux.");
