use anyhow::{Context, Result, ensure};
use serde_json::Value;
use std::{fs, io::ErrorKind, path::Path, process::Command, time::Duration};
use vantry_platform::{hostfs, hostprocess, plugins::Host};

fn download(url: &str, limit: usize) -> Result<(u16, Vec<u8>)> {
    let mut command = Command::new("curl");
    command.args([
        "-q",
        "--silent",
        "--location",
        "--proto",
        "=https",
        "--proto-redir",
        "=https",
        "--max-redirs",
        "5",
        "--write-out",
        "%{http_code}",
        "--",
        url,
    ]);
    let out = hostprocess::capture(&mut command, &[], Duration::from_secs(30), limit + 3)?;
    ensure!(
        out.status.success() && out.stdout.len() >= 3,
        "GitHub download failed"
    );
    let mut bytes = out.stdout;
    let suffix = bytes.split_off(bytes.len() - 3);
    Ok((std::str::from_utf8(&suffix)?.parse()?, bytes))
}
pub fn install(host: &mut Host, source: &str, data: &Path) -> Result<()> {
    let path = Path::new(source);
    match fs::symlink_metadata(path) {
        Ok(meta) if meta.is_dir() => {
            let mut tar = Command::new("tar");
            tar.env("COPYFILE_DISABLE", "1").arg("-C").arg(path).args([
                "--exclude=.git",
                "-czf",
                "-",
                ".",
            ]);
            let out = hostprocess::capture(&mut tar, &[], Duration::from_secs(30), 32 << 20)?;
            ensure!(out.status.success(), "failed to read plugin directory");
            host.install(&out.stdout)?;
            return Ok(());
        }
        Ok(meta) => {
            ensure!(meta.is_file(), "expected plugin directory or archive");
            host.install_archive(path)?;
            return Ok(());
        }
        Err(e) if e.kind() == ErrorKind::NotFound => {}
        Err(e) => return Err(e.into()),
    }
    let repo = source
        .trim()
        .trim_end_matches('/')
        .strip_prefix("https://github.com/")
        .unwrap_or(source.trim().trim_end_matches('/'))
        .trim_end_matches(".git");
    let parts: Vec<_> = repo.split('/').collect();
    ensure!(
        parts.len() == 2
            && parts.iter().all(|s| !s.is_empty()
                && !matches!(*s, "." | "..")
                && s.bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))),
        "expected GitHub owner/repository"
    );
    let api = format!("https://api.github.com/repos/{repo}");
    let (status, release) = download(&format!("{api}/releases/latest"), 1 << 20)?;
    ensure!(
        matches!(status, 200 | 404),
        "GitHub release query returned {status}"
    );
    let release: Value = if status == 200 {
        serde_json::from_slice(&release)?
    } else {
        Value::Null
    };
    let fallback = format!("{api}/zipball");
    let url = release["assets"]
        .as_array()
        .into_iter()
        .flatten()
        .find(|a| a["name"].as_str().is_some_and(|s| s.ends_with(".zip")))
        .and_then(|a| a["browser_download_url"].as_str())
        .or(release["zipball_url"].as_str())
        .unwrap_or(&fallback);
    let (status, archive) = download(url, 32 << 20)?;
    ensure!(status == 200, "GitHub archive returned {status}");
    let manifest = host.install(&archive)?;
    let version = release["tag_name"].as_str().unwrap_or(&manifest.version);
    let bytes = serde_json::to_vec(&serde_json::json!({"repo":repo,"version":version}))?;
    hostfs::atomic_write(
        &data.join("plugins").join(manifest.id).join(".origin.json"),
        &bytes,
    )
    .context("save plugin origin")?;
    Ok(())
}
