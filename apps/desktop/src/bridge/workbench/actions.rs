//! The Core's workbench actions (`POST /v1/workbench`) as the desktop uses
//! them: request builders, the 0.1.42 names an older Core answers to
//! ([`baseline_alias`]), and tolerant readers for the JSON some actions put
//! in `output` (sandbox, usage, remote access, remote desktop, plugin
//! panels, environments, projects).
//!
//! Readers accept missing and unknown fields: a newer or older Core still
//! shows what it has.

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use vantry_protocol::{WorkbenchRequest, WorkbenchResponse};

use crate::bridge::{BridgeError, ErrorCode};

/// A request for `action`.
pub fn request(action: &str) -> WorkbenchRequest {
    WorkbenchRequest {
        action: action.to_owned(),
        ..WorkbenchRequest::default()
    }
}

/// Action names.
pub mod names {
    pub const MODELS: &str = "models";
    pub const PI_STATUS: &str = "pi.status";
    pub const PI_SETUP_SAVE: &str = "pi.setup.save";
    pub const MCP_LIST: &str = "mcp.list";
    pub const MCP_UPSERT: &str = "mcp.upsert";
    pub const MCP_REMOVE: &str = "mcp.remove";
    pub const MCP_PROBE: &str = "mcp.probe";
    pub const MCP_TRUST: &str = "mcp.trust";
    pub const SANDBOX_GET: &str = "sandbox.get";
    pub const SANDBOX_SET: &str = "sandbox.set";
    pub const PLUGINS_LIST: &str = "plugins.list";
    pub const PLUGINS_ENABLE: &str = "plugins.enable";
    pub const PLUGINS_DISABLE: &str = "plugins.disable";
    pub const PLUGINS_RUN: &str = "plugins.run";
    pub const PLUGINS_PANEL: &str = "plugins.panel";
    pub const REMOTE_STATUS: &str = "remote.status";
    pub const REMOTE_PAIR_CREATE: &str = "remote.pair.create";
    pub const REMOTE_PAIR_REDEEM: &str = "remote.pair.redeem";
    pub const REMOTE_PEERS_LIST: &str = "remote.peers.list";
    pub const REMOTE_PEERS_REVOKE: &str = "remote.peers.revoke";
    pub const RD_STATUS: &str = "rd.status";
    pub const RD_SESSION_START: &str = "rd.session.start";
    pub const RD_SESSION_STOP: &str = "rd.session.stop";
    pub const UPDATE_STATUS: &str = "update.status";
    pub const UPDATE_CHECK: &str = "update.check";
    pub const UPDATE_DOWNLOAD: &str = "update.download";
    pub const UPDATE_INSTALL: &str = "update.install";
    pub const USAGE_SUMMARY: &str = "usage.summary";
    pub const ENVIRONMENTS_LIST: &str = "environments.list";
    pub const ENVIRONMENTS_UPSERT: &str = "environments.upsert";
    pub const ENVIRONMENTS_REMOVE: &str = "environments.remove";
    pub const PROJECTS_LIST: &str = "projects.list";
    pub const PROJECTS_ADD: &str = "projects.add";
    pub const PROJECTS_REMOVE: &str = "projects.remove";
    pub const ATTACHMENT_IMPORT: &str = "attachment.import";
    pub const ATTACHMENT_READ: &str = "attachment.read";
}

/// The same request under the 0.1.42 Core's action name, for a Core that
/// does not know the new one (`未知工作台操作`).
pub fn baseline_alias(request: &WorkbenchRequest) -> Option<WorkbenchRequest> {
    let mut alias = request.clone();
    alias.action = match request.action.as_str() {
        names::REMOTE_STATUS | names::REMOTE_PEERS_LIST => "access.status",
        names::REMOTE_PAIR_CREATE => "access.code",
        names::REMOTE_PAIR_REDEEM => "access.pair",
        names::REMOTE_PEERS_REVOKE => "access.remove",
        names::RD_SESSION_START => "rd.host_start",
        names::RD_SESSION_STOP => "rd.host_stop",
        names::PLUGINS_PANEL => "plugins.page",
        names::UPDATE_DOWNLOAD => "update.install",
        names::UPDATE_INSTALL => "update.apply",
        names::PLUGINS_ENABLE | names::PLUGINS_DISABLE => {
            alias.input = Some(json!({
                "key": "enabled",
                "value": if request.action == names::PLUGINS_ENABLE { "true" } else { "false" },
            }));
            "plugins.setting"
        }
        _ => return None,
    }
    .to_owned();
    Some(alias)
}

/// Whether `error` says the Core does not serve this action (or any
/// workbench action): the desktop then falls back to what it can do itself.
pub fn unsupported(error: &BridgeError) -> bool {
    let message = error.message.to_lowercase();
    match error.code {
        ErrorCode::NotReady | ErrorCode::Unavailable => true,
        ErrorCode::NotFound => message.contains("unknown endpoint"),
        ErrorCode::InvalidCommand => {
            message.contains("未知工作台操作")
                || message.contains("未知")
                || message.contains("unknown action")
                || message.contains("unknown workbench")
                || message.contains("unsupported")
        }
        _ => false,
    }
}

/// Whether `error` says only this action's name is unknown (an alias may
/// still work), rather than the whole workbench being absent.
pub fn unknown_action(error: &BridgeError) -> bool {
    error.code == ErrorCode::InvalidCommand && unsupported(error)
}

/// `output` as `T`, from JSON; an empty output is `T::default()`.
pub fn read_output<T: for<'de> Deserialize<'de> + Default>(
    response: &WorkbenchResponse,
) -> Result<T, BridgeError> {
    let text = response.output.trim();
    if text.is_empty() {
        return Ok(T::default());
    }
    serde_json::from_str(text)
        .map_err(|e| BridgeError::protocol(format!("Core 返回的内容无法识别：{e}")))
}

/// Go's zero time (`0001-01-01T00:00:00Z`) is no time.
fn clean_time(time: &mut String) {
    if time.starts_with("0001-01-01") {
        time.clear();
    }
}

// ------------------------------------------------------------------ sandbox

/// `sandbox.get` / `sandbox.set`.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct SandboxInfo {
    /// off | workspace-write | …
    pub mode: String,
    pub supported: bool,
    /// Set by `VANTRY_SANDBOX` (the setting cannot change it).
    #[serde(default, deserialize_with = "read_env_override")]
    pub env_override: bool,
}

fn read_env_override<'de, D: serde::Deserializer<'de>>(d: D) -> Result<bool, D::Error> {
    #[derive(serde::Deserialize)]
    #[serde(untagged)]
    enum Override {
        Flag(bool),
        Mode(String),
    }
    Ok(match Override::deserialize(d)? {
        Override::Flag(v) => v,
        Override::Mode(s) => !s.is_empty(),
    })
}

/// The sandbox modes the settings offer: wire value, label, hint.
pub const SANDBOX_MODES: &[(&str, &str, &str)] = &[
    ("off", "关闭", "工具以当前用户权限运行"),
    ("workspace-write", "工作区可写", "只允许写入工作区与临时目录（macOS Seatbelt）"),
    ("read-only", "只读", "工具不能写入任何文件"),
];

pub fn sandbox_set(mode: &str) -> WorkbenchRequest {
    let mut r = request(names::SANDBOX_SET);
    r.message = mode.to_owned();
    r.input = Some(json!({ "mode": mode }));
    r
}

// -------------------------------------------------------------------- usage

/// One model's line of `usage.summary`.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct ModelUsage {
    pub model: String,
    pub runs: i64,
    pub completed: i64,
    pub failed: i64,
    pub input_tokens: i64,
    pub output_tokens: i64,
    pub cache_read_tokens: i64,
    pub total_tokens: i64,
    pub cache_hit_rate: f64,
    pub cost_usd: f64,
    pub avg_ttft_ms: i64,
}

/// `usage.summary`.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct UsageSummary {
    pub threads: i64,
    pub runs: i64,
    pub active_runs: i64,
    pub completed_runs: i64,
    pub failed_runs: i64,
    pub tasks: i64,
    pub input_tokens: i64,
    pub output_tokens: i64,
    pub cache_read_tokens: i64,
    pub cache_write_tokens: i64,
    pub total_tokens: i64,
    pub cache_hit_rate: f64,
    pub total_cost_usd: f64,
    pub avg_ttft_ms: i64,
    pub models: Vec<ModelUsage>,
}

impl UsageSummary {
    /// Models by total tokens, most first.
    pub fn models_by_tokens(&self) -> Vec<ModelUsage> {
        let mut models = self.models.clone();
        models.sort_by(|a, b| {
            b.total_tokens
                .cmp(&a.total_tokens)
                .then_with(|| b.runs.cmp(&a.runs))
                .then_with(|| a.model.cmp(&b.model))
        });
        models
    }
}

/// `1234567` → `1.23M`, `12345` → `12.3k`.
pub fn compact_count(n: i64) -> String {
    let f = n as f64;
    if n.abs() >= 1_000_000 {
        format!("{:.2}M", f / 1_000_000.0)
    } else if n.abs() >= 10_000 {
        format!("{:.1}k", f / 1_000.0)
    } else {
        n.to_string()
    }
}

// ------------------------------------------------------------ remote access

/// A paired machine (`remote.status` peers).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RemotePeer {
    pub id: String,
    pub name: String,
    pub relay: String,
    pub nearby: bool,
    pub reachable: Option<bool>,
    pub mode: String,
    pub latency_ms: i64,
    pub last_error: String,
}

/// A client allowed into this machine.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RemoteClient {
    pub id: String,
    pub name: String,
    pub created_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct DeviceApproval {
    pub id: String,
    pub name: String,
    pub verification_code: String,
}

/// `remote.status` (0.1.42 `access.status`).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RemoteStatus {
    pub require_approval: Option<bool>,
    pub requests: Vec<DeviceApproval>,
    pub enabled: bool,
    pub connected: bool,
    pub state: String,
    pub error: String,
    pub name: String,
    pub relay: String,
    #[serde(alias = "code")]
    pub pairing_code: String,
    #[serde(alias = "link")]
    pub pairing_link: String,
    pub t3_pairing_link: String,
    pub pairing_state: String,
    #[serde(alias = "expiresAt")]
    pub pairing_expires_at: String,
    pub paired_with: String,
    pub peers: Option<Vec<RemotePeer>>,
    pub clients: Option<Vec<RemoteClient>>,
    pub relay_connected: bool,
}

impl RemoteStatus {
    /// Read from an action's output (zero times cleared).
    pub fn read(response: &WorkbenchResponse) -> Result<Self, BridgeError> {
        let mut status: RemoteStatus = read_output(response)?;
        clean_time(&mut status.pairing_expires_at);
        for client in status.clients.iter_mut().flatten() {
            clean_time(&mut client.created_at);
        }
        Ok(status)
    }

    pub fn peers(&self) -> &[RemotePeer] {
        self.peers.as_deref().unwrap_or_default()
    }

    pub fn clients(&self) -> &[RemoteClient] {
        self.clients.as_deref().unwrap_or_default()
    }

    /// What the settings' status line says.
    pub fn summary(&self) -> String {
        if !self.error.is_empty() {
            return format!("远端访问异常：{}", self.error);
        }
        match (self.enabled, self.connected || self.relay_connected) {
            (false, _) => "远端访问未开启".to_owned(),
            (true, true) => format!(
                "远端访问已开启 · {}",
                if self.relay.is_empty() { "直连" } else { &self.relay }
            ),
            (true, false) => format!(
                "远端访问已开启 · {}",
                if self.state.is_empty() { "等待连接" } else { &self.state }
            ),
        }
    }
}

/// `remote.pair.redeem`: the code (or link) and a name for the machine.
pub fn pair_redeem(code: &str, name: &str) -> WorkbenchRequest {
    let mut r = request(names::REMOTE_PAIR_REDEEM);
    r.message = code.trim().to_owned();
    r.name = name.trim().to_owned();
    r
}

/// `remote.peers.revoke` for peer `id`.
pub fn peer_revoke(id: &str) -> WorkbenchRequest {
    let mut r = request(names::REMOTE_PEERS_REVOKE);
    r.name = id.to_owned();
    r
}

// ----------------------------------------------------------- remote desktop

/// Screen and input permissions of the machine that shares its desktop.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RdPermissions {
    pub screen: Option<bool>,
    pub accessibility: Option<bool>,
}

/// `rd.status` (0.1.42 `RemoteDesktopHostStatus`, plus the viewer session).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RdStatus {
    /// This machine shares its desktop.
    pub hosting: bool,
    pub clients: i64,
    pub share_code: String,
    pub share_expires_at: String,
    pub pending_viewer: String,
    pub connect_url: String,
    pub relay_ssh_host: String,
    /// A viewer session this desktop runs: its peer and state.
    pub session_peer: String,
    pub session_state: String,
    pub session_error: String,
    pub width: i64,
    pub height: i64,
    pub fps: i64,
    pub permissions: Option<RdPermissions>,
}

impl RdStatus {
    pub fn read(response: &WorkbenchResponse) -> Result<Self, BridgeError> {
        let mut status: RdStatus = read_output(response)?;
        clean_time(&mut status.share_expires_at);
        Ok(status)
    }

    /// Permissions the user still has to grant, as labels.
    pub fn missing_permissions(&self) -> Vec<&'static str> {
        let Some(p) = &self.permissions else {
            return Vec::new();
        };
        let mut missing = Vec::new();
        if p.screen == Some(false) {
            missing.push("屏幕录制");
        }
        if p.accessibility == Some(false) {
            missing.push("辅助功能（键鼠控制）");
        }
        missing
    }
}

/// The viewer session's settings sent with `rd.session.start`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RdSessionOptions {
    pub peer: String,
    /// auto | 1280x720 | 1920x1080 | native
    pub resolution: String,
    /// low | balanced | high
    pub quality: String,
    pub control: bool,
    pub clipboard: bool,
}

pub fn rd_session_start(options: &RdSessionOptions) -> WorkbenchRequest {
    let mut r = request(names::RD_SESSION_START);
    r.name = options.peer.clone();
    r.input = serde_json::to_value(options).ok();
    r
}

pub fn rd_session_stop(peer: &str) -> WorkbenchRequest {
    let mut r = request(names::RD_SESSION_STOP);
    r.name = peer.to_owned();
    r
}

// ------------------------------------------------------------ plugin panels

/// A key / value row of a panel.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct PanelKv {
    pub key: String,
    pub value: String,
}

/// A button a panel offers: pressing it sends `plugins.run` with `input`.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct PanelAction {
    pub id: String,
    pub label: String,
    /// default | primary | danger
    pub tone: String,
    pub input: Value,
    /// Ask before running it.
    pub confirm: String,
}

/// A form field: its value goes into the action's input under `key`.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct PanelField {
    pub key: String,
    pub label: String,
    pub value: String,
    pub placeholder: String,
    /// text | secret | multiline
    #[serde(rename = "type")]
    pub kind: String,
}

/// A subscription's allowance windows share one account header and refresh time.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct QuotaCard {
    pub account: String,
    pub plan: String,
    pub expires: String,
    pub updated: String,
    pub message: String,
    pub windows: Vec<QuotaWindow>,
    /// Models the account offers; the card links to the model catalog.
    pub models: Option<u64>,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct QuotaWindow {
    pub name: String,
    pub amount: Option<f64>,
    pub limit: Option<f64>,
    pub unit: String,
    pub used: Option<f64>,
    pub display: String,
    pub reset: String,
}

/// One widget of a panel (schema v1: text, markdown, badge, kv, list,
/// code; plus group, actions and form).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct PanelWidget {
    pub quota: Option<QuotaCard>,
    /// Percentage displayed by a meter widget (0–100).
    pub percent: u8,
    pub detail: String,
    pub kind: String,
    pub key: String,
    pub title: String,
    #[serde(alias="markdown",alias="code",alias="label")]
    pub text: String,
    /// default | info | success | warning | danger
    pub tone: String,
    pub language: String,
    pub entries: Vec<PanelKv>,
    pub items: Vec<String>,
    pub children: Vec<PanelWidget>,
    pub actions: Vec<PanelAction>,
    pub fields: Vec<PanelField>,
}

/// A plugin's native panel.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct PanelView {
    pub version: i64,
    pub title: String,
    /// The status badge beside the title.
    pub badge: String,
    pub tone: String,
    pub widgets: Vec<PanelWidget>,
    pub actions: Vec<PanelAction>,
    /// An `https://` page to open in the browser on this computer once
    /// (a sign-in page), sent only in an action's answer.
    #[serde(rename = "openUrl")]
    pub open_url: String,
    /// Reload the panel after this many milliseconds (0: don't), while it
    /// waits on something outside the app.
    #[serde(rename = "pollMs")]
    pub poll_ms: u64,
}

/// The most widgets a panel shows (as the Core's schema bounds it).
pub const MAX_PANEL_NODES: usize = 128;
const MAX_PANEL_DEPTH: usize = 6;

impl PanelView {
    /// A panel from a `plugins.panel` / `plugins.run` output: the JSON panel,
    /// or `None` when the output is not one (plain text).
    pub fn parse(output: &str) -> Option<PanelView> {
        let text = output.trim();
        if !text.starts_with('{') {
            return None;
        }
        let value: Value = serde_json::from_str(text).ok()?;
        // v0.1.42's plugin host returns the versioned vantryPanel envelope.
        let value = if let Some(panel) = value.get("vantryPanel") {
            if panel.get("version").and_then(Value::as_i64) != Some(1) {return None;}
            panel.clone()
        } else { match value.get("panel") {
            Some(panel) if panel.is_object() => panel.clone(),
            _ => value,
        }};
        if value.get("widgets").is_none() && value.get("actions").is_none() {
            return None;
        }
        let mut view: PanelView = serde_json::from_value(value).ok()?;
        let mut budget = MAX_PANEL_NODES;
        view.widgets = bound(std::mem::take(&mut view.widgets), 1, &mut budget);
        Some(view)
    }

    /// Every action of the panel (root first, then nested), for lookup.
    pub fn all_actions(&self) -> Vec<PanelAction> {
        fn walk(widgets: &[PanelWidget], out: &mut Vec<PanelAction>) {
            for w in widgets {
                out.extend(w.actions.iter().cloned());
                walk(&w.children, out);
            }
        }
        let mut out = self.actions.clone();
        walk(&self.widgets, &mut out);
        out
    }

    /// Every form field of the panel with its initial value.
    pub fn all_fields(&self) -> Vec<PanelField> {
        fn walk(widgets: &[PanelWidget], out: &mut Vec<PanelField>) {
            for w in widgets {
                out.extend(w.fields.iter().cloned());
                walk(&w.children, out);
            }
        }
        let mut out = Vec::new();
        walk(&self.widgets, &mut out);
        out
    }
}

/// Cuts a widget tree to the depth and node budget.
fn bound(widgets: Vec<PanelWidget>, depth: usize, budget: &mut usize) -> Vec<PanelWidget> {
    let mut out = Vec::new();
    for mut widget in widgets {
        if *budget == 0 {
            break;
        }
        *budget -= 1;
        widget.children = if depth < MAX_PANEL_DEPTH {
            bound(std::mem::take(&mut widget.children), depth + 1, budget)
        } else {
            Vec::new()
        };
        out.push(widget);
    }
    out
}

/// `plugins.panel` for `plugin`'s `page`.
pub fn plugin_panel(plugin: &str, page: &str) -> WorkbenchRequest {
    let mut r = request(names::PLUGINS_PANEL);
    r.plugin_id = plugin.to_owned();
    r.name = page.to_owned();
    r
}

/// `plugins.run` with `input` (the action's, with the form's values).
pub fn plugin_run(plugin: &str, page: &str, input: Value) -> WorkbenchRequest {
    let mut r = request(names::PLUGINS_RUN);
    r.plugin_id = plugin.to_owned();
    r.name = page.to_owned();
    r.input = Some(input);
    r
}

/// An action's input with the form's values merged in (an object input
/// gets them as fields; anything else is wrapped as `{action, values}`).
pub fn action_input(
    action: &PanelAction,
    values: &std::collections::BTreeMap<String, String>,
) -> Value {
    let mut input = match &action.input {
        Value::Null => json!({ "action": action.id }),
        Value::Object(map) => Value::Object(map.clone()),
        other => json!({ "action": action.id, "value": other }),
    };
    if !values.is_empty()
        && let Value::Object(map) = &mut input
    {
        map.insert(
            "values".to_owned(),
            Value::Object(
                values
                    .iter()
                    .map(|(k, v)| (k.clone(), Value::String(v.clone())))
                    .collect(),
            ),
        );
    }
    input
}

pub fn plugin_toggle(plugin: &str, enable: bool) -> WorkbenchRequest {
    let mut r = request(if enable {
        names::PLUGINS_ENABLE
    } else {
        names::PLUGINS_DISABLE
    });
    r.plugin_id = plugin.to_owned();
    r
}

// --------------------------------------------------------------------- MCP

/// A server the settings' form adds or replaces (`mcp.upsert`).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct McpServerSpec {
    pub name: String,
    /// stdio | http
    pub transport: String,
    /// stdio: the command line.
    pub command: String,
    /// http: the URL.
    pub url: String,
    pub enabled: bool,
    /// user | project
    pub scope: String,
}

impl McpServerSpec {
    /// Why it cannot be saved, if it cannot.
    pub fn problem(&self) -> Option<&'static str> {
        let name = self.name.trim();
        if name.is_empty() {
            return Some("请填写服务器名称");
        }
        if !name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
        {
            return Some("名称只能包含字母、数字、- _ .");
        }
        match self.transport.as_str() {
            "stdio" if self.command.trim().is_empty() => Some("请填写启动命令"),
            "http" if !(self.url.starts_with("http://") || self.url.starts_with("https://")) => {
                Some("请填写 http(s):// 开头的地址")
            }
            "stdio" | "http" => None,
            _ => Some("传输方式只能为 stdio 或 http"),
        }
    }
}

pub fn mcp_upsert(spec: &McpServerSpec) -> WorkbenchRequest {
    let mut r = request(names::MCP_UPSERT);
    r.name = spec.name.trim().to_owned();
    let mut input = json!({
        "name": spec.name.trim(),
        "transport": spec.transport,
        "enabled": spec.enabled,
        "scope": if spec.scope.is_empty() { "user" } else { spec.scope.as_str() },
    });
    if spec.transport == "stdio" {
        let mut parts = spec.command.split_whitespace().map(str::to_owned);
        input["command"] = json!(parts.next().unwrap_or_default());
        input["args"] = json!(parts.collect::<Vec<_>>());
    } else {
        input["url"] = json!(spec.url.trim());
    }
    r.input = Some(input);
    r
}

/// `mcp.upsert` that only switches server `name` on or off.
pub fn mcp_set_enabled(name: &str, enabled: bool) -> WorkbenchRequest {
    let mut r = request(names::MCP_UPSERT);
    r.name = name.to_owned();
    r.input = Some(json!({ "name": name, "enabled": enabled }));
    r
}

pub fn mcp_named(action: &str, name: &str) -> WorkbenchRequest {
    let mut r = request(action);
    r.name = name.to_owned();
    r
}

// ------------------------------------------------------------------- Pi setup

/// `pi.setup.save`: a provider's key and the default model. The key is a
/// secret: it goes to the Core once and is never kept by the desktop.
pub fn pi_setup_save(provider: &str, api_key: &str, model: &str) -> WorkbenchRequest {
    let mut r = request(names::PI_SETUP_SAVE);
    r.name = provider.trim().to_owned();
    r.model = model.trim().to_owned();
    let id = model
        .trim()
        .strip_prefix(&format!("{}/", provider.trim()))
        .unwrap_or(model.trim());
    let setup = vantry_protocol::PiSetupRequest {
        provider_id: provider.trim().into(),
        api_key: api_key.into(),
        models: (!id.is_empty())
            .then(|| id.to_owned())
            .into_iter()
            .collect(),
        set_default: !id.is_empty(),
        ..Default::default()
    };
    r.input = Some(serde_json::to_value(setup).expect("Pi setup request serializes"));
    r
}

// ------------------------------------------------------ environments, projects

/// A remote environment (`environments.list`).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct EnvironmentEntry {
    pub id: String,
    pub name: String,
    /// Saved local or paired environment.
    pub kind: String,
    pub host: String,
    pub peer_id: String,
}

impl EnvironmentEntry {
    /// `{"environments": […]}`.
    pub fn read_list(response: &WorkbenchResponse) -> Result<Vec<Self>, BridgeError> {
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct List {
            environments: Option<Vec<EnvironmentEntry>>,
        }
        Ok(read_output::<List>(response)?.environments.unwrap_or_default().into_iter()
            .filter(|entry| matches!(entry.kind.as_str(), "local" | "peer" | "remote")).collect())
    }

    /// Where it connects to, as one line.
    pub fn target(&self) -> String {
        match self.kind.as_str() {
            "peer" | "remote" => format!("配对主机 · {}", if self.peer_id.is_empty() { &self.id } else { &self.peer_id }),
            _ => "本机".to_owned(),
        }
    }
}

pub fn environment_upsert(entry: &EnvironmentEntry) -> WorkbenchRequest {
    let mut r = request(names::ENVIRONMENTS_UPSERT);
    r.name = entry.id.clone();
    r.input = serde_json::to_value(entry).ok();
    r
}

/// A project (`projects.list`).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct ProjectEntry {
    pub id: String,
    pub name: String,
    pub path: String,
}

impl ProjectEntry {
    /// `{"projects": […]}`.
    pub fn read_list(response: &WorkbenchResponse) -> Result<Vec<Self>, BridgeError> {
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct List {
            projects: Option<Vec<ProjectEntry>>,
        }
        Ok(read_output::<List>(response)?.projects.unwrap_or_default())
    }
}

pub fn named(action: &str, name: &str) -> WorkbenchRequest {
    let mut r = request(action);
    r.name = name.to_owned();
    r
}

pub fn project_add(path: &str) -> WorkbenchRequest {
    let mut r = request(names::PROJECTS_ADD);
    r.workspace = path.to_owned();
    r.name = std::path::Path::new(path)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    r
}

// ---------------------------------------------------------------- thinking

/// A model value's base model and thinking level (`provider/model:high`),
/// given the catalog's models: the level is what follows the longest
/// catalog model it starts with, else none.
pub fn split_thinking<'a>(value: &'a str, models: &[String]) -> (&'a str, &'a str) {
    if models.iter().any(|m| m == value) {
        return (value, "");
    }
    let mut best: Option<(&str, &str)> = None;
    for model in models {
        if let Some(level) = value
            .strip_prefix(model.as_str())
            .and_then(|rest| rest.strip_prefix(':'))
            && best.is_none_or(|(b, _)| model.len() > b.len())
        {
            best = Some((&value[..model.len()], level));
        }
    }
    best.unwrap_or_else(|| match value.rsplit_once(':') {
        // Without a catalog, a known level word still reads as a level.
        Some((base, level)) if THINKING_WORDS.contains(&level) && !base.is_empty() => (base, level),
        _ => (value, ""),
    })
}

const THINKING_WORDS: &[&str] = &["off", "minimal", "low", "medium", "high", "xhigh", "max"];

/// `base` with thinking `level` (none when empty).
pub fn with_thinking(base: &str, level: &str) -> String {
    if level.is_empty() {
        base.to_owned()
    } else {
        format!("{base}:{level}")
    }
}

/// A level as people say it.
pub fn thinking_label(level: &str) -> String {
    match level {
        "" => "默认思考".to_owned(),
        "off" => "不思考".to_owned(),
        "minimal" => "极简".to_owned(),
        "low" => "低".to_owned(),
        "medium" => "中".to_owned(),
        "high" => "高".to_owned(),
        "xhigh" => "超高".to_owned(),
        "max" => "最高".to_owned(),
        other => other.to_owned(),
    }
}

/// The level after `current` in `levels` (with "" = the model's default
/// first), wrapping.
pub fn next_thinking(current: &str, levels: &[String]) -> String {
    let mut all = vec![String::new()];
    all.extend(levels.iter().filter(|l| !l.is_empty()).cloned());
    let at = all.iter().position(|l| l == current).map_or(0, |i| (i + 1) % all.len());
    all[at].clone()
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use super::*;

    fn output(text: &str) -> WorkbenchResponse {
        WorkbenchResponse {
            output: text.to_owned(),
            ..WorkbenchResponse::default()
        }
    }

    #[test]
    fn new_names_have_baseline_aliases() {
        let alias = baseline_alias(&pair_redeem(" SESSION-1 ", "工作室")).unwrap();
        assert_eq!((alias.action.as_str(), alias.message.as_str(), alias.name.as_str()), ("access.pair", "SESSION-1", "工作室"));
        assert_eq!(baseline_alias(&request(names::REMOTE_STATUS)).unwrap().action, "access.status");
        assert_eq!(baseline_alias(&request(names::RD_SESSION_START)).unwrap().action, "rd.host_start");
        assert_eq!(baseline_alias(&plugin_panel("p", "main")).unwrap().action, "plugins.page");
        let disable = baseline_alias(&plugin_toggle("p", false)).unwrap();
        assert_eq!(disable.action, "plugins.setting");
        assert_eq!(disable.input.unwrap()["value"], "false");
        assert_eq!(baseline_alias(&request(names::MODELS)), None);
    }

    #[test]
    fn unsupported_errors_are_told_apart() {
        assert!(unsupported(&BridgeError::new(ErrorCode::NotReady, "workbench service is not mounted on this Core")));
        assert!(unsupported(&BridgeError::new(ErrorCode::NotFound, "unknown endpoint")));
        assert!(!unsupported(&BridgeError::new(ErrorCode::NotFound, "thread t1 not found")));
        assert!(unknown_action(&BridgeError::new(ErrorCode::InvalidCommand, "未知工作台操作")));
        assert!(!unsupported(&BridgeError::new(ErrorCode::InvalidCommand, "沙箱模式参数无效")));
        assert!(!unknown_action(&BridgeError::unavailable("refused")));
        assert!(!unsupported(&BridgeError::new(ErrorCode::Conflict, "x")));
    }

    #[test]
    fn outputs_read_tolerantly() {
        let sandbox: SandboxInfo = read_output(&output(r#"{"mode":"workspace-write","supported":true,"extra":1}"#)).unwrap();
        assert_eq!(sandbox.mode, "workspace-write");
        assert!(sandbox.supported && !sandbox.env_override);
        let empty: SandboxInfo = read_output(&output("")).unwrap();
        assert_eq!(empty, SandboxInfo::default());
        assert!(read_output::<SandboxInfo>(&output("not json")).is_err());

        let usage: UsageSummary = read_output(&output(
            r#"{"threads":3,"runs":9,"totalTokens":1500000,"totalCostUsd":1.25,
                "models":[{"model":"a","totalTokens":10},{"model":"b","totalTokens":900,"runs":2}]}"#,
        ))
        .unwrap();
        assert_eq!(usage.models_by_tokens()[0].model, "b");
        assert_eq!(compact_count(usage.total_tokens), "1.50M");
        assert_eq!(compact_count(12_345), "12.3k");
        assert_eq!(compact_count(999), "999");

        let remote = RemoteStatus::read(&output(
            r#"{"enabled":true,"connected":true,"relay":"https://r.example","pairingCode":"SESSION-ABCD",
                "pairingExpiresAt":"0001-01-01T00:00:00Z",
                "peers":[{"id":"p1","name":"工作室","reachable":true,"latencyMs":12}],
                "clients":[{"id":"c1","name":"iPhone","createdAt":"2026-11-01T00:00:00Z"}]}"#,
        ))
        .unwrap();
        assert_eq!(remote.peers()[0].reachable, Some(true));
        assert_eq!(remote.clients()[0].name, "iPhone");
        assert!(remote.pairing_expires_at.is_empty());
        assert!(remote.summary().contains("r.example"));
        assert_eq!(RemoteStatus::default().summary(), "远端访问未开启");

        let rd = RdStatus::read(&output(r#"{"hosting":true,"clients":1,"permissions":{"screen":true,"accessibility":false}}"#)).unwrap();
        assert_eq!(rd.missing_permissions(), ["辅助功能（键鼠控制）"]);

        let envs = EnvironmentEntry::read_list(&output(r#"{"environments":[{"id":"e1","name":"构建机","kind":"ssh","host":"build"}]}"#)).unwrap();
        assert!(envs.is_empty());
        assert!(EnvironmentEntry::read_list(&output("{}")).unwrap().is_empty());
        let projects = ProjectEntry::read_list(&output(r#"{"projects":[{"id":"a","name":"app","path":"/code/app"}]}"#)).unwrap();
        assert_eq!(projects[0].path, "/code/app");
    }

    #[test]
    fn panels_parse_bound_and_collect_actions() {
        assert_eq!(PanelView::parse("plain text output"), None);
        assert_eq!(PanelView::parse(r#"{"other":1}"#), None);
        let panel = PanelView::parse(
            r#"{"version":1,"title":"用量","badge":"在线","tone":"success",
                "widgets":[
                  {"kind":"kv","title":"今日","entries":[{"key":"令牌","value":"12k"}]},
                  {"kind":"group","title":"操作","children":[
                     {"kind":"form","fields":[{"key":"days","label":"天数","value":"7"}]},
                     {"kind":"actions","actions":[{"id":"refresh","label":"刷新","input":{"op":"refresh"}}]}
                  ]},
                  {"kind":"mystery","text":"未知部件按文本显示"}
                ],
                "actions":[{"id":"export","label":"导出"}]}"#,
        )
        .unwrap();
        assert_eq!(panel.title, "用量");
        assert_eq!(panel.widgets.len(), 3);
        let actions: Vec<String> = panel.all_actions().into_iter().map(|a| a.id).collect();
        assert_eq!(actions, ["export", "refresh"]);
        assert_eq!(panel.all_fields()[0].value, "7");
        let native=PanelView::parse(r#"{"vantryPanel":{"version":1,"widgets":[{"kind":"badge","label":"Ready"}]}}"#).unwrap();
        assert_eq!(native.widgets[0].text,"Ready");
        let wrapped = PanelView::parse(r#"{"panel":{"widgets":[]}}"#).unwrap();
        assert!(wrapped.widgets.is_empty());

        // Deep and huge trees are cut.
        let mut deep = serde_json::json!({"kind":"text","text":"leaf"});
        for _ in 0..10 {
            deep = serde_json::json!({"kind":"group","children":[deep]});
        }
        let deep = PanelView::parse(&serde_json::json!({"widgets":[deep]}).to_string()).unwrap();
        let mut depth = 0;
        let mut node = &deep.widgets[0];
        while let Some(child) = node.children.first() {
            node = child;
            depth += 1;
        }
        assert_eq!(depth, MAX_PANEL_DEPTH - 1);
        let many: Vec<Value> = (0..500).map(|i| serde_json::json!({"kind":"text","text":i.to_string()})).collect();
        let many = PanelView::parse(&serde_json::json!({"widgets":many}).to_string()).unwrap();
        assert_eq!(many.widgets.len(), MAX_PANEL_NODES);

        let mut values = BTreeMap::new();
        values.insert("days".to_owned(), "30".to_owned());
        let input = action_input(&panel.all_actions()[1], &values);
        assert_eq!(input["op"], "refresh");
        assert_eq!(input["values"]["days"], "30");
        let bare = action_input(&panel.all_actions()[0], &BTreeMap::new());
        assert_eq!(bare, serde_json::json!({"action":"export"}));
    }

    #[test]
    fn mcp_specs_validate_and_build() {
        let mut spec = McpServerSpec {
            name: "figma".into(),
            transport: "http".into(),
            url: "ftp://x".into(),
            enabled: true,
            ..McpServerSpec::default()
        };
        assert!(spec.problem().is_some());
        spec.url = "http://127.0.0.1:3845/mcp".into();
        assert_eq!(spec.problem(), None);
        let request = mcp_upsert(&spec);
        assert_eq!(request.input.as_ref().unwrap()["url"], "http://127.0.0.1:3845/mcp");
        let stdio = McpServerSpec {
            name: "magpie".into(),
            transport: "stdio".into(),
            command: "npx -y magpie-mcp".into(),
            ..McpServerSpec::default()
        };
        let input = mcp_upsert(&stdio).input.unwrap();
        assert_eq!(input["command"], "npx");
        assert_eq!(input["args"], serde_json::json!(["-y", "magpie-mcp"]));
        assert!(McpServerSpec { name: "bad name".into(), ..stdio.clone() }.problem().is_some());
        assert_eq!(mcp_set_enabled("figma", false).input.unwrap()["enabled"], false);
    }

    #[test]
    fn thinking_levels_split_and_cycle() {
        let models = vec!["anthropic/opus".to_owned(), "anthropic/opus-fast".to_owned()];
        assert_eq!(split_thinking("anthropic/opus", &models), ("anthropic/opus", ""));
        assert_eq!(split_thinking("anthropic/opus:high", &models), ("anthropic/opus", "high"));
        assert_eq!(split_thinking("anthropic/opus-fast:low", &models), ("anthropic/opus-fast", "low"));
        assert_eq!(split_thinking("other/x:medium", &[]), ("other/x", "medium"));
        assert_eq!(split_thinking("local:8080", &[]), ("local:8080", ""));
        assert_eq!(with_thinking("a/b", "high"), "a/b:high");
        assert_eq!(with_thinking("a/b", ""), "a/b");
        let levels = vec!["low".to_owned(), "high".to_owned()];
        assert_eq!(next_thinking("", &levels), "low");
        assert_eq!(next_thinking("low", &levels), "high");
        assert_eq!(next_thinking("high", &levels), "");
        assert_eq!(next_thinking("weird", &levels), "");
        assert_eq!(thinking_label("high"), "高");
    }

    #[test]
    fn secrets_go_only_into_the_request() {
        let request = pi_setup_save("openai", "sk-test-123", "openai/gpt");
        assert_eq!(request.input.as_ref().unwrap()["apiKey"], "sk-test-123");
        assert!(!request.name.contains("sk-"));
        assert!(!request.message.contains("sk-"));
    }
}
