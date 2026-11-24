//! Query configured MCP servers using initialize and tools/list, without running a model turn.
use anyhow::{Context, Result, bail, ensure};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    io::{BufRead, BufReader, Read, Write},
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::mpsc,
    time::{Duration, Instant},
};
use vantry_platform::hostprocess::{self, Process};
use vantry_protocol::{MCPCatalog, MCPServerInfo};

const LIMIT: u64 = 1 << 20;
fn call(method: &str, id: Option<i32>, params: Value) -> Value {
    let mut v = json!({"jsonrpc":"2.0","method":method,"params":params});
    if let Some(id) = id {
        v["id"] = id.into();
    }
    v
}
fn query(mut exchange: impl FnMut(Value, Option<i32>) -> Result<Value>) -> Result<Vec<String>> {
    let init = exchange(
        call(
            "initialize",
            Some(1),
            json!({"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"vantry-service","version":env!("CARGO_PKG_VERSION")}}),
        ),
        Some(1),
    )?;
    ensure!(
        init["protocolVersion"].is_string(),
        "invalid MCP initialization"
    );
    exchange(call("notifications/initialized", None, json!({})), None)?;
    let mut names = vec![];
    let mut params = json!({});
    for id in 2..=101 {
        let v = exchange(call("tools/list", Some(id), params), Some(id))?;
        for tool in v["tools"].as_array().context("invalid tool listing")? {
            names.push(
                tool["name"]
                    .as_str()
                    .context("missing tool name")?
                    .to_owned(),
            );
        }
        ensure!(names.len() <= 10000, "tool listing too large");
        let Some(cursor) = v["nextCursor"].as_str().filter(|s| !s.is_empty()) else {
            return Ok(names);
        };
        params = json!({"cursor":cursor});
    }
    bail!("MCP pagination limit reached")
}
fn reply(v: Value, id: i32) -> Result<Value> {
    ensure!(v["id"] == id && v["error"].is_null(), "MCP request failed");
    v.get("result").cloned().context("missing MCP result")
}
fn stdio(cfg: &Value, root: &Path, deadline: Instant) -> Result<Vec<String>> {
    let mut cmd = Command::new(cfg["command"].as_str().context("missing MCP command")?);
    let args: Vec<String> = serde_json::from_value(cfg.get("args").cloned().unwrap_or(json!([])))?;
    let env: HashMap<String, String> =
        serde_json::from_value(cfg.get("env").cloned().unwrap_or(json!({})))?;
    cmd.args(args)
        .envs(env)
        .current_dir(root.join(cfg["cwd"].as_str().unwrap_or("")))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    let mut child = Process::spawn(&mut cmd)?;
    let mut input = child.take_stdin().context("missing stdin")?;
    let mut output = BufReader::new(child.take_stdout().context("missing stdout")?);
    std::thread::scope(|scope| {
        let (tx, rx) = mpsc::sync_channel(1);
        let read = scope.spawn(move || {
            loop {
                let mut line = Vec::new();
                let r = (&mut output).take(LIMIT + 1).read_until(b'\n', &mut line);
                if !matches!(r,Ok(n) if n>0 && n<=LIMIT as usize) || tx.send(line).is_err() {
                    break;
                }
            }
        });
        let result = query(|v, id| {
            ensure!(Instant::now() < deadline, "MCP probe timed out");
            serde_json::to_writer(&mut input, &v)?;
            input.write_all(b"\n")?;
            input.flush()?;
            let Some(id) = id else { return Ok(Value::Null) };
            loop {
                let line = rx
                    .recv_timeout(deadline.saturating_duration_since(Instant::now()))
                    .context("MCP response unavailable")?;
                let received: Value = serde_json::from_slice(&line)?;
                if received["id"] == id {
                    return reply(received, id);
                }
            }
        });
        drop(rx);
        drop(input);
        child.stop(Duration::from_millis(100))?;
        read.join()
            .map_err(|_| anyhow::anyhow!("MCP reader failed"))?;
        result
    })
}
// curl reads headers and body from stdin so credentials are absent from process arguments.
fn curl_string(s: &str) -> Result<String> {
    ensure!(!s.chars().any(char::is_control), "invalid HTTP option");
    Ok(format!(
        "\"{}\"",
        s.replace('\\', "\\\\").replace('"', "\\\"")
    ))
}
fn http(cfg: &Value, deadline: Instant) -> Result<Vec<String>> {
    let url = curl_string(cfg["url"].as_str().context("missing URL")?)?;
    let headers: HashMap<String, String> =
        serde_json::from_value(cfg.get("headers").cloned().unwrap_or(json!({})))?;
    let mut session = None;
    let mut version = String::from("2025-03-26");
    query(|message, id| {
        let mut options = format!(
            "url = {url}\nheader = \"Content-Type: application/json\"\nheader = \"Accept: application/json, text/event-stream\"\n"
        );
        for (k, v) in &headers {
            options.push_str(&format!(
                "header = {}\n",
                curl_string(&format!("{k}: {v}"))?
            ));
        }
        if let Some(s) = &session {
            options.push_str(&format!(
                "header = {}\n",
                curl_string(&format!("Mcp-Session-Id: {s}"))?
            ));
        }
        if id != Some(1) {
            options.push_str(&format!(
                "header = {}\n",
                curl_string(&format!("MCP-Protocol-Version: {version}"))?
            ));
        }
        options.push_str(&format!(
            "data-binary = {}\n",
            curl_string(&message.to_string())?
        ));
        let remaining = deadline.saturating_duration_since(Instant::now());
        ensure!(!remaining.is_zero(), "MCP probe timed out");
        let out = hostprocess::capture(
            Command::new("curl").args([
                "-q",
                "--silent",
                "--fail",
                "--include",
                "--proto",
                "=http,https",
                "--config",
                "-",
            ]),
            options.as_bytes(),
            remaining,
            LIMIT as usize,
        )?;
        ensure!(out.status.success(), "MCP HTTP request failed");
        let raw = std::str::from_utf8(&out.stdout)?;
        let mut response = raw;
        let body = loop {
            let (head, body) = response
                .split_once("\r\n\r\n")
                .context("invalid MCP HTTP response")?;
            for line in head.lines().skip(1) {
                if let Some((k, v)) = line.split_once(':')
                    && k.eq_ignore_ascii_case("mcp-session-id")
                {
                    session = Some(v.trim().to_string());
                }
            }
            if body.starts_with("HTTP/") {
                response = body;
            } else {
                break body;
            }
        };
        let Some(id) = id else { return Ok(Value::Null) };
        let v = if body.trim_start().starts_with('{') {
            serde_json::from_str(body)?
        } else {
            body.lines()
                .filter_map(|s| s.strip_prefix("data:"))
                .filter_map(|s| serde_json::from_str::<Value>(s).ok())
                .find(|v| v["id"] == id)
                .context("missing MCP response event")?
        };
        let result = reply(v, id)?;
        if id == 1 {
            version = result["protocolVersion"]
                .as_str()
                .context("missing protocol version")?
                .into();
        }
        Ok(result)
    })
}
fn probe_one(
    s: &MCPServerInfo,
    user: &Value,
    project: &Value,
    trusted: bool,
    root: &Path,
    deadline: Instant,
) -> Result<Vec<String>> {
    let mut cfg = user["mcpServers"][&s.name].clone();
    if s.scope.starts_with("project") && trusted {
        let local = &project["mcpServers"][&s.name];
        if s.scope == "project-override" {
            let fields = cfg.as_object_mut().context("missing user server")?;
            fields.extend(local.as_object().context("invalid override")?.clone());
        } else {
            cfg = local.clone();
        }
    } else if s.scope.starts_with("project") {
        ensure!(!cfg.is_null(), "project is not trusted");
    }
    ensure!(cfg["enabled"] != false, "server disabled");
    ensure!(
        cfg.get("auth").is_none() && cfg.get("oauth").is_none(),
        "interactive MCP authentication required"
    );
    if cfg["command"].is_string() {
        stdio(&cfg, root, deadline)
    } else {
        http(&cfg, deadline)
    }
}
pub fn probe(mut catalog: MCPCatalog) -> Result<MCPCatalog> {
    let user = crate::settings::document(Path::new(&catalog.user_file))?;
    let project = if catalog.project_file.is_empty() {
        Value::Null
    } else {
        crate::settings::document(Path::new(&catalog.project_file))?
    };
    let root = if catalog.workspace.is_empty() {
        std::env::var_os("HOME")
            .map(PathBuf::from)
            .context("missing home")?
    } else {
        PathBuf::from(&catalog.workspace)
    };
    let deadline = Instant::now() + Duration::from_secs(25);
    for s in catalog.servers.iter_mut().flatten() {
        if !s.error.is_empty() || !s.enabled {
            continue;
        }
        let result = probe_one(s, &user, &project, catalog.project_trusted, &root, deadline);
        match result {
            Ok(names) => {
                s.live_state = "connected".into();
                s.tools = names
            }
            Err(e) => {
                s.live_state = "error".into();
                s.live_error = e.to_string();
            }
        }
    }
    catalog.probed = true;
    Ok(catalog)
}
