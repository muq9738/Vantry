//! Use Pi's per-profile tool catalog for local HTTP discovery across short-lived attempts.
use std::{path::Path, time::Duration};

use pi_mcp::{
    LoadedMcpConfig, McpConfigScope, McpExposure, McpLifecycle, McpManager, McpManagerOptions,
    McpServerEntry, load_mcp_config, runtime::McpOAuthCredentialStore,
};
use tokio_util::sync::CancellationToken;
use url::{Host, Url};

fn shared_catalog(entry: &McpServerEntry, credentials: Option<&McpOAuthCredentialStore>) -> bool {
    let config = &entry.config;
    if entry.scope != Some(McpConfigScope::Global)
        || entry.override_path.is_some()
        || config.enabled == Some(false)
        || config.lifecycle.is_some()
        || config.auth.is_some()
        || config.oauth.is_some()
        || config
            .headers
            .as_ref()
            .is_some_and(|headers| !headers.is_empty())
    {
        return false;
    }
    // Direct tools must be advertised on the first prompt, including on a cold cache.
    if !matches!(
        config.exposure.unwrap_or_default(),
        McpExposure::Codemode | McpExposure::Deferred
    ) || config
        .tool_exposures()
        .iter()
        .any(|(_, exposure)| *exposure == McpExposure::Direct)
    {
        return false;
    }
    let Some(url) = config.url.as_ref().and_then(|raw| Url::parse(raw).ok()) else {
        return false;
    };
    matches!(url.scheme(), "http" | "https")
        && url.username().is_empty()
        && url.password().is_none()
        && url.query().is_none()
        && match url.host() {
            Some(Host::Domain(name)) => name == "localhost",
            Some(Host::Ipv4(ip)) => ip.is_loopback(),
            Some(Host::Ipv6(ip)) => ip.is_loopback(),
            None => false,
        }
        && credentials.is_none_or(|store| store.tokens(&entry.name, url.as_str()).is_none())
}

fn stored_credentials(agent: &Path) -> Option<McpOAuthCredentialStore> {
    agent
        .join("mcp-auth.json")
        .exists()
        .then(|| McpOAuthCredentialStore::new(agent))
}

pub(super) fn worker_config(agent: &Path, cwd: &Path, trusted: bool) -> LoadedMcpConfig {
    let mut config = load_mcp_config(agent, cwd, trusted);
    if config.lifecycle.is_none() {
        let credentials = stored_credentials(agent);
        for entry in &mut config.servers {
            if shared_catalog(entry, credentials.as_ref()) {
                entry.config.lifecycle = Some(McpLifecycle::Lazy);
            }
        }
    }
    config
}

/// Populate Pi's own discovery cache after the Core listener binds. Workers use that cache
/// for ordinary turns and negotiate their own authorized connection when a tool is called.
pub async fn warm_local_discovery(agent: &Path, enabled: bool, stop: CancellationToken) {
    if !super::flag("VANTRY_PI_USER_MCP", enabled) || stop.is_cancelled() {
        return;
    }
    let mut config = load_mcp_config(agent, agent, false);
    if config.lifecycle.is_some() {
        return;
    }
    let credentials = stored_credentials(agent);
    config
        .servers
        .retain(|entry| shared_catalog(entry, credentials.as_ref()));
    if config.servers.is_empty() {
        return;
    }
    let manager = McpManager::new(McpManagerOptions::new(agent));
    manager.start(agent.to_owned(), config);
    tokio::select! {
        _ = stop.cancelled() => {}
        _ = tokio::time::timeout(Duration::from_secs(5), manager.pending()) => {}
    }
    manager.shutdown().await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::{collections::BTreeMap, fs};

    #[test]
    fn only_default_user_loopback_servers_defer_discovery() {
        let temp = tempfile::tempdir().unwrap();
        let agent = temp.path().join("agent");
        let cwd = temp.path().join("project");
        fs::create_dir(&agent).unwrap();
        fs::create_dir_all(cwd.join(".pi")).unwrap();
        let local = "http://127.0.0.1:42/mcp";
        let mut global = json!({"mcpServers":{
            "catalog":{"url":local}, "overridden":{"url":local}, "ipv6":{"url":"http://[::1]/mcp","exposure":"deferred"},
            "remote":{"url":"https://example.org/mcp"}, "direct":{"url":local,"exposure":"direct"},
            "some_direct":{"url":local,"toolExposure":{"ping":"direct"}}, "disabled":{"url":local,"enabled":false},
            "header":{"url":local,"headers":{"Authorization":"fixture"}}, "stdio":{"command":"fixture"},
            "explicit":{"url":local,"lifecycle":"eager"}
        }});
        let write_global =
            |v: &serde_json::Value| fs::write(agent.join("mcp.json"), v.to_string()).unwrap();
        write_global(&global);
        fs::write(
            cwd.join(".pi/mcp.json"),
            json!({"mcpServers":{
                "overridden":{"exposure":"deferred"}, "project":{"url":local}
            }})
            .to_string(),
        )
        .unwrap();
        let lifecycle = |trusted| {
            let loaded = worker_config(&agent, &cwd, trusted);
            assert!(loaded.errors.is_empty(), "{:?}", loaded.errors);
            loaded
                .servers
                .into_iter()
                .map(|e| (e.name, e.config.lifecycle))
                .collect::<BTreeMap<_, _>>()
        };
        let trusted = lifecycle(true);
        for (name, value) in &trusted {
            let expected = match name.as_str() {
                "catalog" | "ipv6" => Some(McpLifecycle::Lazy),
                "explicit" => Some(McpLifecycle::Eager),
                _ => None,
            };
            assert_eq!(*value, expected, "{name}");
        }
        assert_eq!(trusted.len(), 11);
        let untrusted = lifecycle(false);
        assert!(!untrusted.contains_key("project"));
        assert_eq!(untrusted["overridden"], Some(McpLifecycle::Lazy));
        fs::write(
            agent.join("mcp-auth.json"),
            json!({format!("{}|{local}", pi_mcp::config::mcp_namespace("catalog")): {
                "serverUrl": local, "tokens":{"access_token":"fixture","token_type":"Bearer"}
            }})
            .to_string(),
        )
        .unwrap();
        assert_eq!(lifecycle(false)["catalog"], None);
        assert_eq!(lifecycle(false)["overridden"], Some(McpLifecycle::Lazy));
        fs::remove_file(agent.join("mcp-auth.json")).unwrap();
        global["lifecycle"] = json!("eager");
        write_global(&global);
        assert_eq!(lifecycle(false)["catalog"], None);
    }
}
