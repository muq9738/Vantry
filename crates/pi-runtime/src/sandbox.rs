//! Seatbelt policy for a dedicated worker, inherited by Pi and every spawned tool.
use anyhow::{Context, Result, ensure};
use std::{
    path::{Path, PathBuf},
    process::Command,
};
fn literal(path: &Path) -> Result<String> {
    let s = path.to_str().context("invalid sandbox path encoding")?;
    ensure!(
        path.is_absolute() && !s.chars().any(char::is_control),
        "invalid sandbox path"
    );
    Ok(format!(
        "\"{}\"",
        s.replace('\\', "\\\\").replace('"', "\\\"")
    ))
}
pub struct WorkspaceSandbox {
    profile: String,
    workspace: PathBuf,
}
impl WorkspaceSandbox {
    pub fn new(workspace: &Path, writable: &[&Path]) -> Result<Self> {
        let workspace = workspace.canonicalize()?;
        ensure!(workspace.is_dir(), "invalid sandbox workspace");
        let mut profile = String::from(
            "(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/tty\") (literal \"/dev/dtracehelper\") (regex #\"^/dev/fd/\") (regex #\"^/dev/tty\"))\n",
        );
        for dir in std::iter::once(workspace.as_path())
            .chain(writable.iter().copied())
            .chain([Path::new("/tmp"), std::env::temp_dir().as_path()])
        {
            let dir = match dir.canonicalize() {
                Ok(path) => path,
                Err(_) if !dir.exists() => dir
                    .parent()
                    .context("missing sandbox path")?
                    .canonicalize()?
                    .join(dir.file_name().context("missing sandbox name")?),
                Err(e) => return Err(e.into()),
            };
            profile.push_str(&format!(
                "(allow file-write* (subpath {}))\n",
                literal(&dir)?
            ));
        }
        let git = workspace.join(".git");
        profile.push_str(&format!(
            "(deny file-write* (subpath {}) (literal {}))\n",
            literal(&git.join("hooks"))?,
            literal(&git.join("config"))?
        ));
        if git.is_file() {
            profile.push_str(&format!(
                "(deny file-write* (literal {}))\n",
                literal(&git)?
            ));
        }
        Ok(Self { profile, workspace })
    }
    /// Wrap an external command before spawn. Arguments never pass through a shell.
    pub fn command(&self, program: &Path) -> Result<Command> {
        ensure!(
            cfg!(target_os = "macos"),
            "workspace-write sandbox requires macOS"
        );
        let mut cmd = Command::new("/usr/bin/sandbox-exec");
        cmd.args(["-p", &self.profile])
            .arg(program)
            .current_dir(&self.workspace);
        if std::env::var_os("GOCACHE").is_none() {
            cmd.env("GOCACHE", std::env::temp_dir().join("vantry-gocache"));
        }
        Ok(cmd)
    }
    /// Only used inside the dedicated provider-worker process, before loading any extension.
    #[cfg(target_os = "macos")]
    pub(crate) fn enter(&self) -> Result<()> {
        use std::ffi::{CString, c_char};
        #[link(name = "sandbox")]
        unsafe extern "C" {
            fn sandbox_init(profile: *const c_char, flags: u64, error: *mut *mut c_char) -> i32;
            fn sandbox_free_error(error: *mut c_char);
        }
        let profile = CString::new(self.profile.as_str())?;
        let mut error = std::ptr::null_mut();
        let code = unsafe { sandbox_init(profile.as_ptr(), 0, &mut error) };
        if !error.is_null() {
            unsafe { sandbox_free_error(error) }
        }
        ensure!(
            code == 0,
            "could not activate workspace-write Seatbelt policy"
        );
        Ok(())
    }
    #[cfg(not(target_os = "macos"))]
    pub(crate) fn enter(&self) -> Result<()> {
        anyhow::bail!("workspace-write sandbox requires macOS")
    }
}
pub(crate) fn mode(job: &crate::JobRequest) -> Result<String> {
    let normalize = |mode: &str| match mode.trim().to_ascii_lowercase().as_str() {
        "" | "off" | "disabled" | "none" | "false" => Some("off"),
        "workspace-write" | "workspace" | "seatbelt" | "on" | "true" => Some("workspace-write"),
        _ => None,
    };
    if let Ok(s) = std::env::var("VANTRY_SANDBOX_MODE")
        && !s.trim().is_empty()
    {
        return Ok(normalize(&s).unwrap_or("off").into());
    }
    let config = job.data_dir.join("sandbox.json");
    if config.try_exists()?
        && let Ok(value) = crate::disk::read::<serde_json::Value>(&config)
        && let Some(selected) = value["mode"].as_str().and_then(normalize)
    {
        return Ok(selected.into());
    }
    Ok(normalize(&job.sandbox_mode).unwrap_or("off").into())
}

#[cfg(all(test, target_os = "macos"))]
mod tests {
    use super::*;
    use std::fs;

    #[test]
    fn wrapped_children_inherit_workspace_and_git_write_rules() -> Result<()> {
        let tmp = tempfile::tempdir()?;
        let ws = tmp.path().join("a \"quoted\" workspace");
        fs::create_dir_all(ws.join(".git/hooks"))?;
        let sandbox = WorkspaceSandbox::new(&ws, &[])?;
        assert!(
            sandbox
                .command(Path::new("/bin/sh"))?
                .args(["-c", "/usr/bin/touch permitted"])
                .status()?
                .success()
        );
        let denied = sandbox
            .command(Path::new("/bin/sh"))?
            .args(["-c", "/usr/bin/touch .git/config .git/hooks/hook"])
            .output()?;
        assert!(!denied.status.success());
        assert!(ws.join("permitted").exists());
        assert!(!ws.join(".git/config").exists() && !ws.join(".git/hooks/hook").exists());
        Ok(())
    }
}
