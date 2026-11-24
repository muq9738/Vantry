//! Read-only MCP settings projection. The embedded Pi manager owns transport and trust at execution time.
use anyhow::{Context, Result, ensure};
use pi_coding_agent::{
    app::AppOptions,
    resources::{brand::project_config_path, trust::ProjectTrustStore},
};
use serde::Deserialize;
use serde_json::Value;
use std::{
    collections::{BTreeMap, BTreeSet},
    fs::File,
    io::{ErrorKind, Read},
    path::Path,
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
