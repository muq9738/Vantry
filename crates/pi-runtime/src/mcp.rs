//! Read-only MCP settings projection. The embedded Pi manager owns transport and trust at execution time.
use anyhow::{Context, Result, ensure};
use pi_coding_agent::{
    app::AppOptions,
    resources::{brand::project_config_path, trust::ProjectTrustStore},
    settings::{FileSettingsStorage, SettingsEdit, SettingsManager, SettingsOptions, SettingsScope, SettingsStorage},
};
use serde::Deserialize;
use serde_json::Value;
use std::{
    collections::{BTreeMap, BTreeSet},
    fs::File,
    io::{ErrorKind, Read},
    path::{Path, PathBuf},
    sync::Arc,
};
use vantry_protocol::{MCPCatalog, MCPServerInfo};

// Pi strips the Win32 verbatim path prefix before looking up a saved project trust decision.
pub use pi_coding_agent::resources::paths::canonicalize as project_trust_path;

fn flag(key: &str, fallback: bool) -> bool {
    match std::env::var(key)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "1" | "true" | "yes" | "on" => true,
        "0" | "false" | "no" | "off" => false,
        _ => fallback,
    }
}
pub(crate) fn configure(options: &mut AppOptions, job: &crate::JobRequest) {
    let user = flag("VANTRY_PI_USER_EXTENSIONS", job.enable_user_extensions);
    options.resources.no_extensions = !user;
    if user && !flag("VANTRY_PI_NATIVE_SUBAGENTS", job.enable_native_subagents) {
        options.exclude_tools.extend(
            [
                "subagent",
                "subagent_supervisor",
                "bg_wait",
                "contact_supervisor",
            ]
            .map(String::from),
        );
    }
    if flag("VANTRY_PI_USER_MCP", job.enable_user_mcp) {
        options
            .resources
            .extension_sources
            .push("builtin:mcp".into());
        // Cap first-prompt MCP wait to 800ms instead of Pi's 10s default so slow
        // or unreachable MCP servers never stall turn startup.
        options.mcp.startup_wait = Some(std::time::Duration::from_millis(800));
    } else {
        options
            .resources
            .disabled_builtin_extensions
            .push("mcp".into());
    }
    options
        .resources
        .extension_sources
        .extend(["builtin:codemode", "builtin:tool-search"].map(String::from));
    options.tools = Some(vec!["+codemode".into(), "+tool_search".into()]);
}

fn package_source(entry: &Value) -> Option<&str> {
    entry
        .as_str()
        .or_else(|| entry.as_object().and_then(|obj| obj.get("source")).and_then(Value::as_str))
}

fn should_skip_worker_package(source: &str, disable_native_subagents: bool) -> bool {
    let lower = source.trim().to_ascii_lowercase();
    // `@ff-labs/pi-fff` depends on Bun's `bun:ffi` or native `.node` addons unavailable
    // in embedded `pi-rust`, stalling every turn for ~2.1s before failing and hijacking
    // native Rust `grep`/`find`.
    let is_pi_fff = lower.contains("pi-fff") || lower.contains("fff-bun") || lower.contains("fff-node");
    if is_pi_fff {
        return true;
    }
    // When Vantry disables Pi-native subagents, skip loading `pi-subagents` altogether
    // instead of paying its full startup cost only to exclude its tools afterward.
    disable_native_subagents && lower.contains("pi-subagents")
}

fn filter_packages_json(raw: &str, disable_native_subagents: bool) -> String {
    let stripped = raw.strip_prefix('\u{feff}').unwrap_or(raw);
    let Ok(mut value) = serde_json::from_str::<Value>(stripped) else {
        return raw.to_owned();
    };
    let Some(packages) = value.get_mut("packages").and_then(Value::as_array_mut) else {
        return raw.to_owned();
    };
    let before = packages.len();
    packages.retain(|entry| {
        !package_source(entry)
            .is_some_and(|src| should_skip_worker_package(src, disable_native_subagents))
    });
    if packages.len() == before {
        return raw.to_owned();
    }
    serde_json::to_string_pretty(&value).unwrap_or_else(|_| raw.to_owned())
}

fn restore_packages_json(original: Option<&str>, updated: String) -> String {
    let Some(orig_raw) = original else {
        return updated;
    };
    let orig_stripped = orig_raw.strip_prefix('\u{feff}').unwrap_or(orig_raw);
    let Ok(orig_val) = serde_json::from_str::<Value>(orig_stripped) else {
        return updated;
    };
    let Some(orig_packages) = orig_val.get("packages") else {
        return updated;
    };
    let Ok(mut next_val) = serde_json::from_str::<Value>(&updated) else {
        return updated;
    };
    if let Some(obj) = next_val.as_object_mut() {
        obj.insert("packages".into(), orig_packages.clone());
        return serde_json::to_string_pretty(&next_val).unwrap_or(updated);
    }
    updated
}

struct WorkerSettingsStorage {
    inner: FileSettingsStorage,
    disable_native_subagents: bool,
}

impl SettingsStorage for WorkerSettingsStorage {
    fn with_lock(&self, scope: SettingsScope, edit: SettingsEdit<'_>) -> Result<(), String> {
        let disable_native_subagents = self.disable_native_subagents;
        self.inner.with_lock(scope, &mut |current| {
            let filtered = current.map(|text| filter_packages_json(text, disable_native_subagents));
            match edit(filtered.as_deref())? {
                Some(next) => Ok(Some(restore_packages_json(current, next))),
                None => Ok(None),
            }
        })
    }

    fn path(&self, scope: SettingsScope) -> Option<PathBuf> {
        self.inner.path(scope)
    }
}

/// Build an in-memory filtered `SettingsManager` for embedded Pi workers so incompatible
/// packages (`pi-fff`) and disabled `pi-subagents` never stall turn startup, without
/// mutating the user's `~/.pi/agent/settings.json` on disk.
pub(crate) fn worker_settings(
    cwd: &Path,
    agent: &Path,
    job: &crate::JobRequest,
) -> Arc<SettingsManager> {
    let canonical_cwd = cwd.canonicalize().unwrap_or_else(|_| cwd.to_path_buf());
    let project_trusted = match ProjectTrustStore::new(agent).get(&canonical_cwd) {
        Ok(Some(value)) => value,
        _ => crate::config::object(&agent.join("settings.json"))
            .ok()
            .and_then(|obj| obj.get("defaultProjectTrust").cloned())
            .is_some_and(|v| v == "always"),
    };
    let disable_native_subagents =
        !flag("VANTRY_PI_NATIVE_SUBAGENTS", job.enable_native_subagents);
    let storage = WorkerSettingsStorage {
        inner: FileSettingsStorage::new(cwd, agent),
        disable_native_subagents,
    };
    Arc::new(SettingsManager::from_storage(
        Arc::new(storage),
        SettingsOptions { project_trusted },
    ))
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Document {
    #[serde(default)]
    mcp_servers: BTreeMap<String, Value>,
    auto_enable_codemode: Option<bool>,
}
fn document(path: &Path) -> Result<Document> {
    let mut bytes = vec![];
    match File::open(path) {
        Ok(file) => file.take(1_048_577).read_to_end(&mut bytes)?,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(Document::default()),
        Err(e) => return Err(e.into()),
    };
    ensure!(bytes.len() <= 1_048_576, "MCP configuration exceeds 1 MiB");
    serde_json::from_slice(bytes.strip_prefix(b"\xef\xbb\xbf").unwrap_or(&bytes))
        .map_err(|_| anyhow::anyhow!("invalid MCP configuration"))
}
fn exposure(value: &str) -> Result<String> {
    Ok(match value {
        "codemode-deferred" | "codemode" => "codemode",
        "deferred" | "direct" | "hidden" => value,
        _ => anyhow::bail!("invalid MCP tool exposure"),
    }
    .into())
}
fn project(name: &str, scope: &str, raw: &Value) -> Result<MCPServerInfo> {
    #[derive(Deserialize, Default)]
    #[serde(default, rename_all = "camelCase")]
    struct Server {
        command: String,
        url: String,
        r#type: String,
        enabled: Option<bool>,
        exposure: Option<String>,
        tool_exposure: BTreeMap<String, String>,
        description: String,
        args: Vec<String>,
        env: BTreeMap<String, String>,
        headers: BTreeMap<String, String>,
        oauth: Option<BTreeMap<String, Value>>,
        timeout: Option<f64>,
        cwd: String,
    }
    ensure!(
        !name.is_empty()
            && name
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || b"_-".contains(&c)),
        "invalid MCP server name"
    );
    let s: Server = serde_json::from_value(raw.clone())
        .map_err(|_| anyhow::anyhow!("invalid MCP server fields"))?;
    ensure!(s.timeout.is_none_or(|t| t > 0.0), "invalid MCP timeout");
    ensure!(
        s.command.is_empty() != s.url.is_empty(),
        "expected exactly one MCP transport"
    );
    let (transport, endpoint) = if s.command.is_empty() {
        ensure!(
            matches!(s.r#type.as_str(), "" | "http" | "streamable-http"),
            "MCP transport mismatch"
        );
        let url = url::Url::parse(&s.url).ok().context("invalid MCP URL")?;
        ensure!(
            matches!(url.scheme(), "http" | "https") && url.host().is_some(),
            "invalid MCP URL"
        );
        // Paths can also carry tokens. Only the network origin is a safe display field.
        ("http", url.origin().ascii_serialization())
    } else {
        ensure!(
            matches!(s.r#type.as_str(), "" | "stdio"),
            "MCP transport mismatch"
        );
        ("stdio", s.command)
    };
    Ok(MCPServerInfo {
        name: name.into(),
        scope: scope.into(),
        transport: transport.into(),
        endpoint,
        enabled: s.enabled.unwrap_or(true),
        description: s.description,
        exposure: exposure(s.exposure.as_deref().unwrap_or("codemode"))?,
        tool_exposure: s
            .tool_exposure
            .iter()
            .map(|(k, v)| Ok((k.clone(), exposure(v)?)))
            .collect::<Result<_>>()?,
        ..Default::default()
    })
}
/// Lists project settings even before trust, as the settings panel needs to display them for review.
pub fn mcp_catalog(agent: &Path, cwd: Option<&Path>) -> Result<MCPCatalog> {
    let user_file = agent.join("mcp.json");
    let global = document(&user_file)?;
    let mut local = Document::default();
    let mut out = MCPCatalog {
        user_file: user_file.display().to_string(),
        ..Default::default()
    };
    if let Some(cwd) = cwd {
        ensure!(cwd.is_absolute() && cwd.is_dir(), "invalid MCP workspace");
        let cwd = cwd.canonicalize()?;
        let path = project_config_path(&cwd, "mcp.json");
        local = document(&path)?;
        out.project_trusted = match ProjectTrustStore::new(agent).get(&cwd)? {
            Some(value) => value,
            None => crate::config::object(&agent.join("settings.json"))?
                .get("defaultProjectTrust")
                .is_some_and(|v| v == "always"),
        };
        out.workspace = cwd.display().to_string();
        out.project_file = path.display().to_string();
    }
    out.codemode_default = local
        .auto_enable_codemode
        .or(global.auto_enable_codemode)
        .unwrap_or(true);
    let names: BTreeSet<_> = global
        .mcp_servers
        .keys()
        .chain(local.mcp_servers.keys())
        .collect();
    let mut servers = vec![];
    let mut namespaces = BTreeSet::new();
    for name in names {
        let base = global.mcp_servers.get(name);
        let mut scope = "user";
        let result = (|| {
            ensure!(
                namespaces.insert(name.replace('-', "_")),
                "MCP namespace collision"
            );
            let Some(local) = local.mcp_servers.get(name) else {
                return project(name, scope, base.unwrap());
            };
            scope = "project";
            let fields = local.as_object().context("invalid MCP server")?;
            if ["command", "url", "type"]
                .iter()
                .any(|k| fields.contains_key(*k))
            {
                ensure!(
                    !fields.contains_key("auth"),
                    "project MCP provider auth is unavailable"
                );
                return project(name, scope, local);
            }
            scope = "project-override";
            ensure!(
                fields.keys().all(|k| matches!(
                    k.as_str(),
                    "enabled" | "exposure" | "toolExposure" | "lifecycle"
                )),
                "invalid MCP override fields"
            );
            let mut merged = base
                .and_then(Value::as_object)
                .context("missing inherited MCP server")?
                .clone();
            merged.extend(fields.clone());
            project(name, scope, &Value::Object(merged))
        })();
        servers.push(result.unwrap_or_else(|e| MCPServerInfo {
            name: name.clone(),
            scope: scope.into(),
            error: e.to_string(),
            ..Default::default()
        }));
    }
    out.servers = Some(servers);
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn worker_package_filter_skips_pi_fff_and_disabled_subagents() {
        let raw = r#"{
            "packages": [
                "npm:pi-context-pruning@1.1.0",
                "npm:pi-subagents",
                "npm:@ff-labs/pi-fff",
                {"source": "npm:@ff-labs/fff-bun"}
            ],
            "theme": "dark"
        }"#;
        let filtered = filter_packages_json(raw, true);
        let parsed: Value = serde_json::from_str(&filtered).unwrap();
        assert_eq!(
            parsed["packages"],
            serde_json::json!(["npm:pi-context-pruning@1.1.0"])
        );
        assert_eq!(parsed["theme"], "dark");

        let keep_subagents = filter_packages_json(raw, false);
        let parsed_keep: Value = serde_json::from_str(&keep_subagents).unwrap();
        assert_eq!(
            parsed_keep["packages"],
            serde_json::json!(["npm:pi-context-pruning@1.1.0", "npm:pi-subagents"])
        );
    }

    #[test]
    fn worker_package_restore_preserves_disk_packages_on_write() {
        let original = r#"{"packages":["npm:@ff-labs/pi-fff","npm:pi-subagents"],"theme":"dark"}"#;
        let updated = r#"{"packages":[],"theme":"light"}"#.to_owned();
        let restored = restore_packages_json(Some(original), updated);
        let parsed: Value = serde_json::from_str(&restored).unwrap();
        assert_eq!(
            parsed["packages"],
            serde_json::json!(["npm:@ff-labs/pi-fff", "npm:pi-subagents"])
        );
        assert_eq!(parsed["theme"], "light");
    }
}
