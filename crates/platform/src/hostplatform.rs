//! OS paths, executable discovery and passive permission probes.
use std::{
    env,
    ffi::OsString,
    io,
    path::{Path, PathBuf},
};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Paths {
    pub data: PathBuf,
    pub cache: PathBuf,
    pub config: PathBuf,
    pub runtime: PathBuf,
    pub socket: PathBuf,
}
impl Paths {
    pub fn discover() -> io::Result<Self> {
        Self::from_env(|k| env::var_os(k))
    }
    pub fn from_env(get: impl Fn(&str) -> Option<OsString>) -> io::Result<Self> {
        let var = |key| get(key).filter(|v| !v.is_empty()).map(PathBuf::from);
        let home = || {
            var(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
                .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "missing home directory"))
        };
        let default_config = || -> io::Result<PathBuf> {
            #[cfg(target_os = "macos")]
            {
                Ok(home()?.join("Library/Application Support/Vantry"))
            }
            #[cfg(windows)]
            {
                Ok(var("APPDATA")
                    .map(Ok)
                    .unwrap_or_else(|| home().map(|p| p.join("AppData/Roaming")))?
                    .join("Vantry"))
            }
            #[cfg(all(unix, not(target_os = "macos")))]
            {
                Ok(var("XDG_CONFIG_HOME")
                    .filter(|p| p.is_absolute())
                    .map(Ok)
                    .unwrap_or_else(|| home().map(|p| p.join(".config")))?
                    .join("Vantry"))
            }
        };
        let resolve = |key, default: &dyn Fn() -> io::Result<PathBuf>| {
            std::path::absolute(match var(key) {
                Some(v) => v,
                None => default()?,
            })
        };
        let data = resolve("VANTRY_DATA_DIR", &default_config)?;
        let config = resolve("VANTRY_CONFIG_DIR", &|| {
            Ok(default_config().unwrap_or_else(|_| data.clone()))
        })?;
        let cache = resolve("VANTRY_CACHE_DIR", &|| {
            #[cfg(target_os = "macos")]
            {
                Ok(home()
                    .map(|p| p.join("Library/Caches/Vantry"))
                    .unwrap_or_else(|_| data.join("cache")))
            }
            #[cfg(windows)]
            {
                Ok(var("LOCALAPPDATA")
                    .or_else(|| home().ok().map(|p| p.join("AppData/Local")))
                    .map(|p| p.join("Vantry/Cache"))
                    .unwrap_or_else(|| data.join("cache")))
            }
            #[cfg(all(unix, not(target_os = "macos")))]
            {
                Ok(var("XDG_CACHE_HOME")
                    .filter(|p| p.is_absolute())
                    .or_else(|| home().ok().map(|p| p.join(".cache")))
                    .map(|p| p.join("Vantry"))
                    .unwrap_or_else(|| data.join("cache")))
            }
        })?;
        let runtime = resolve("VANTRY_RUNTIME_DIR", &|| Ok(data.clone()))?;
        let socket = resolve("VANTRY_SOCKET_PATH", &|| Ok(runtime.join("core.sock")))?;
        Ok(Self {
            data,
            cache,
            config,
            runtime,
            socket,
        })
    }
}
pub fn executable(path: &Path) -> bool {
    path.metadata().is_ok_and(|m| {
        if !m.is_file() {
            return false;
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            m.permissions().mode() & 0o111 != 0
        }
        #[cfg(windows)]
        {
            path.extension()
                .is_some_and(|e| e.eq_ignore_ascii_case("exe") || e.eq_ignore_ascii_case("com"))
        }
    })
}
pub fn which(name: &str) -> Option<PathBuf> {
    if crate::hostfs::component(name).is_err() {
        return None;
    }
    for dir in env::split_paths(&env::var_os("PATH")?) {
        if !dir.is_absolute() {
            continue;
        }
        let path = dir.join(name);
        if executable(&path) {
            return Some(path);
        }
        #[cfg(windows)]
        if path.extension().is_none() {
            let p = path.with_extension("exe");
            if executable(&p) {
                return Some(p);
            }
        }
    }
    None
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Permission {
    Granted,
    Denied,
    Unavailable,
    Unknown,
}
#[derive(Debug)]
pub struct Capabilities {
    pub os: &'static str,
    pub pty: bool,
    pub git: bool,
    pub ssh: bool,
    pub screen_capture: Permission,
    pub accessibility: Permission,
}
impl Capabilities {
    pub fn probe() -> Self {
        #[cfg(target_os = "macos")]
        let (screen_capture, accessibility) = {
            #[link(name = "CoreGraphics", kind = "framework")]
            unsafe extern "C" {
                fn CGPreflightScreenCaptureAccess() -> bool;
            }
            #[link(name = "ApplicationServices", kind = "framework")]
            unsafe extern "C" {
                fn AXIsProcessTrusted() -> bool;
            }
            let state = |yes| {
                if yes {
                    Permission::Granted
                } else {
                    Permission::Denied
                }
            };
            unsafe {
                (
                    state(CGPreflightScreenCaptureAccess()),
                    state(AXIsProcessTrusted()),
                )
            }
        };
        #[cfg(target_os = "linux")]
        let (screen_capture, accessibility) = {
            let display =
                env::var_os("DISPLAY").is_some() || env::var_os("WAYLAND_DISPLAY").is_some();
            (
                if display {
                    Permission::Unknown
                } else {
                    Permission::Unavailable
                },
                Permission::Unknown,
            )
        };
        #[cfg(windows)]
        let (screen_capture, accessibility) = (Permission::Unknown, Permission::Unknown);
        Self {
            os: env::consts::OS,
            pty: cfg!(unix),
            git: which("git").is_some(),
            ssh: which("ssh").is_some(),
            screen_capture,
            accessibility,
        }
    }
}
