use anyhow::{Context, Result, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde_json::{Value, json};
use std::{
    fs,
    path::{Path, PathBuf},
};
use vantry_platform::hostfs;
use vantry_protocol::*;

pub fn document(path: &Path) -> Result<Value> {
    match hostfs::read_file(
        path.parent().context("missing parent")?,
        Path::new(path.file_name().context("missing filename")?),
        1 << 20,
    ) {
        Ok(b) => {
            let v: Value = serde_json::from_slice(&b)?;
            ensure!(v.is_object(), "expected settings object");
            Ok(v)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(json!({})),
        Err(e) => Err(e.into()),
    }
}
fn change(path: &Path, edit: impl FnOnce(&mut Value) -> Result<()>) -> Result<()> {
    let dir = path.parent().context("missing settings directory")?;
    if !dir.exists() {
        hostfs::ensure_private_dir(dir)?;
    }
    let _lock = hostfs::lock_dir(dir, true)?;
    let mut v = document(path)?;
    edit(&mut v)?;
    let bytes = serde_json::to_vec_pretty(&v)?;
    // Existing project .pi directories need not have a private mode. Replace the file atomically.
    use std::{io::Write, os::unix::fs::OpenOptionsExt};
    let tmp = dir.join(format!(".settings-{}", uuid::Uuid::new_v4()));
    let mut f = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&tmp)?;
    let result = (|| {
        f.write_all(&bytes)?;
        f.sync_all()?;
        fs::rename(&tmp, path)
    })();
    if result.is_err() {
        let _ = fs::remove_file(tmp);
    }
    Ok(result?)
}
pub async fn mcp(r: &WorkbenchRequest, root: Option<PathBuf>) -> Result<MCPCatalog> {
    let agent = vantry_pi_runtime::agent_dir();
    let list = || vantry_pi_runtime::mcp::mcp_catalog(&agent, root.as_deref());
    let input = r.input.as_ref().unwrap_or(&Value::Null);
    match r.action.as_str() {
        "mcp.list" => return list(),
        "mcp.probe" => {
            let catalog = list()?;
            return tokio::task::spawn_blocking(move || crate::mcp_probe::probe(catalog)).await?;
        }
        "mcp.trust" => {
            let trusted = input["trusted"]
                .as_bool()
                .context("trusted must be a boolean")?;
            let root = root
                .as_ref()
                .context("workspace required")?
                .canonicalize()?;
            // Pi's ProjectTrustStore coordinates its read/modify/write using this directory.
            struct TrustLock(PathBuf);
            impl Drop for TrustLock {
                fn drop(&mut self) {
                    let _ = fs::remove_dir(&self.0);
                }
            }
            if !agent.is_dir() {
                hostfs::ensure_private_dir(&agent)?;
            }
            let lock = agent.join("trust.json.lock");
            let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(2);
            loop {
                match fs::create_dir(&lock) {
                    Ok(()) => break,
                    Err(e)
                        if e.kind() == std::io::ErrorKind::AlreadyExists
                            && tokio::time::Instant::now() < deadline =>
                    {
                        tokio::time::sleep(std::time::Duration::from_millis(20)).await
                    }
                    Err(e) => return Err(e.into()),
                }
            }
            let _guard = TrustLock(lock);
            change(&agent.join("trust.json"), |v| {
                v[root.to_str().context("invalid path")?] = trusted.into();
                Ok(())
            })?
        }
        "mcp.upsert" | "mcp.remove" => {
            ensure!(
                !r.name.is_empty()
                    && r.name
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b)),
                "invalid server name"
            );
            let cat = list()?;
            let path = match input["scope"].as_str().unwrap_or("project") {
                "user" => cat.user_file.clone(),
                "project" => {
                    ensure!(!cat.project_file.is_empty(), "workspace required");
                    cat.project_file
                }
                _ => bail!("invalid scope"),
            };
            change(Path::new(&path), |v| {
                let servers = v
                    .as_object_mut()
                    .unwrap()
                    .entry("mcpServers")
                    .or_insert(json!({}))
                    .as_object_mut()
                    .context("invalid mcpServers")?;
                if r.action == "mcp.remove" {
                    servers.remove(&r.name);
                } else {
                    let cfg = input.get("config").unwrap_or(input);
                    ensure!(cfg.is_object(), "expected MCP configuration object");
                    let project = path != cat.user_file;
                    let transport = cfg.get("command").is_some() || cfg.get("url").is_some();
                    if transport {
                        ensure!(
                            cfg.get("command").is_none() || cfg.get("url").is_none(),
                            "conflicting transports"
                        );
                        ensure!(
                            cfg["command"]
                                .as_str()
                                .is_some_and(|s| !s.trim().is_empty())
                                ^ cfg["url"].as_str().is_some_and(
                                    |s| s.starts_with("http://") || s.starts_with("https://")
                                ),
                            "expected exactly one MCP transport"
                        );
                        ensure!(
                            !project || cfg.get("auth").is_none(),
                            "project MCP provider auth unavailable"
                        );
                    } else {
                        ensure!(
                            project
                                && cfg.as_object().unwrap().keys().all(|k| matches!(
                                    k.as_str(),
                                    "enabled" | "exposure" | "toolExposure" | "lifecycle"
                                ))
                                && cat.servers.iter().flatten().any(|s| s.name == r.name),
                            "missing inherited MCP transport"
                        );
                    }
                    servers.insert(r.name.clone(), cfg.clone());
                }
                Ok(())
            })?
        }
        _ => bail!("unknown MCP action"),
    };
    list()
}
pub fn sandbox(data: &Path, r: &WorkbenchRequest) -> Result<String> {
    let path = data.join("sandbox.json");
    let supported = cfg!(target_os = "macos") && Path::new("/usr/bin/sandbox-exec").is_file();
    let normalize = |s: &str| match s.trim().to_ascii_lowercase().as_str() {
        "" | "off" | "disabled" | "none" | "false" => Ok("off"),
        "workspace-write" | "workspace" | "seatbelt" | "on" | "true" => Ok("workspace-write"),
        _ => bail!("unknown sandbox mode"),
    };
    if r.action == "sandbox.set" {
        let mode = normalize(
            r.input
                .as_ref()
                .and_then(|v| v["mode"].as_str())
                .unwrap_or(&r.message),
        )?;
        ensure!(mode == "off" || supported, "workspace sandbox unavailable");
        hostfs::atomic_write(&path, &serde_json::to_vec(&json!({"mode":mode}))?)?;
    }
    let env = std::env::var("VANTRY_SANDBOX_MODE")
        .ok()
        .filter(|s| !s.trim().is_empty());
    let config = document(&path)?;
    let mode = normalize(env.as_deref().or(config["mode"].as_str()).unwrap_or("off"))?;
    Ok(
        json!({"mode":mode,"supported":supported,"envOverride":env.unwrap_or_default()})
            .to_string(),
    )
}
pub fn mime(name: &str, bytes: &[u8]) -> &'static str {
    match Path::new(name)
        .extension()
        .and_then(|s| s.to_str())
        .unwrap_or("")
        .to_lowercase()
        .as_str()
    {
        "pdf" => "application/pdf",
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "json" => "application/json",
        "md" => "text/markdown; charset=utf-8",
        _ if std::str::from_utf8(bytes).is_ok() && !bytes.contains(&0) => {
            "text/plain; charset=utf-8"
        }
        _ => "application/octet-stream",
    }
}
pub fn attachment(data: &Path, r: &WorkbenchRequest) -> Result<WorkbenchResponse> {
    let root = data.join("attachments");
    let mut out = WorkbenchResponse::default();
    if r.action == "attachment.import" {
        let bytes = STANDARD.decode(&r.data)?;
        ensure!(
            (1..=8 << 20).contains(&bytes.len()),
            "invalid attachment size"
        );
        let name = Path::new(&r.name)
            .file_name()
            .and_then(|s| s.to_str())
            .context("invalid attachment name")?;
        ensure!(
            name.len() <= 255 && !name.contains(['\\', ':']) && !name.chars().any(char::is_control),
            "invalid attachment name"
        );
        hostfs::ensure_private_dir(&root)?;
        let a = Attachment {
            id: format!("file_{}", uuid::Uuid::new_v4().simple()),
            name: name.into(),
            size: bytes.len() as i64,
            mime: mime(name, &bytes).into(),
        };
        let dir = root.join(&a.id);
        hostfs::ensure_private_dir(&dir)?;
        let result = (|| -> Result<()> {
            hostfs::atomic_write(&dir.join("content").join(name), &bytes)?;
            hostfs::atomic_write(&dir.join("metadata.json"), &serde_json::to_vec(&a)?)?;
            Ok(())
        })();
        if let Err(e) = result {
            let _ = fs::remove_dir_all(dir);
            return Err(e);
        }
        out.attachment = Some(a);
    } else {
        let a: Attachment = serde_json::from_value(r.input.clone().context("missing attachment")?)?;
        ensure!(
            a.id.strip_prefix("file_").is_some_and(|s| !s.is_empty()
                && s.bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b)))
                && a.name
                    == Path::new(&a.name)
                        .file_name()
                        .and_then(|s| s.to_str())
                        .unwrap_or("")
                && !a.name.is_empty(),
            "invalid attachment"
        );
        let dir = root.join(&a.id);
        let stored: Attachment =
            serde_json::from_slice(&hostfs::read_file(&dir, Path::new("metadata.json"), 4096)?)?;
        ensure!(a == stored, "attachment metadata mismatch");
        let bytes = hostfs::read_file(&dir.join("content"), Path::new(&a.name), 8 << 20)?;
        ensure!(bytes.len() as i64 == a.size, "attachment size mismatch");
        out.data = STANDARD.encode(bytes);
    }
    Ok(out)
}
