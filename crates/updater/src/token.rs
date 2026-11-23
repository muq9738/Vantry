use anyhow::{Result, ensure};
use std::{
    env, fs,
    io::{self, Read},
    path::{Path, PathBuf},
};

pub fn resolve_current_app_bundle(exe: &Path) -> Option<&Path> {
    let mac = exe.parent()?;
    let contents = mac.parent()?;
    let bundle = contents.parent()?;
    (mac.file_name()? == "MacOS"
        && contents.file_name()? == "Contents"
        && bundle.extension()?.eq_ignore_ascii_case("app"))
    .then_some(bundle)
}
pub fn bundled_token_file(exe: &Path) -> Option<PathBuf> {
    Some(match resolve_current_app_bundle(exe) {
        Some(app) => app.join("Contents/Resources/update-token"),
        None => exe.parent()?.join("update-token"),
    })
}
pub fn update_token_file() -> Option<PathBuf> {
    if let Ok(s) = env::var("VANTRY_UPDATE_TOKEN_FILE")
        && !s.trim().is_empty()
    {
        return Some(s.trim().into());
    }
    let dir = if cfg!(windows) {
        env::var_os("APPDATA").map(PathBuf::from)
    } else if cfg!(target_os = "macos") {
        env::var_os("HOME").map(|h| Path::new(&h).join("Library/Application Support"))
    } else {
        env::var_os("XDG_CONFIG_HOME")
            .map(PathBuf::from)
            .or_else(|| env::var_os("HOME").map(|h| Path::new(&h).join(".config")))
    }?;
    Some(dir.join("Vantry/update-token"))
}
/// Read on each check, preferring the dedicated private override. Errors never contain credential data.
pub fn read_update_credentials(
    local: Option<&Path>,
    bundled: Option<&Path>,
) -> Result<Option<String>> {
    for (path, private) in [(local, true), (bundled, false)] {
        let Some(path) = path else { continue };
        let m = match fs::metadata(path) {
            Err(e) if e.kind() == io::ErrorKind::NotFound => continue,
            other => other?,
        };
        ensure!(
            m.is_file() && m.len() <= 512,
            "invalid update credential file"
        );
        #[cfg(unix)]
        ensure!(
            !private || crate::tree::mode(&m) & 0o077 == 0,
            "update credential requires private permissions (0600)"
        );
        #[cfg(not(unix))]
        let _ = private;
        let mut value = String::new();
        fs::File::open(path)?.take(513).read_to_string(&mut value)?;
        let token = value.trim();
        ensure!(
            value.len() <= 512
                && (8..=255).contains(&token.len())
                && token
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b)),
            "invalid update credential"
        );
        return Ok(Some(token.into()));
    }
    Ok(None)
}
