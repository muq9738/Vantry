//! Extension UI requests (`api.PendingRequest`) as the approval modal shows
//! them, and the answer each decision sends (`request.respond`).
//!
//! The mapping follows the 0.1.42 desktop (`controls_view.go`): an approval
//! `select` with 允许本次 / 本会话允许 … / 拒绝 answers with the option text;
//! `confirm` answers `confirmed`; `select` / `input` / `editor` answer a
//! value or `cancelled`.

use vantry_protocol::{Command, CommandKind, PendingRequest};

/// How risky the request looks, from what it says it will do.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Risk {
    Low,
    Medium,
    High,
}

impl Risk {
    /// The hint under the request.
    pub fn hint(self) -> &'static str {
        match self {
            Risk::Low => "只读或低影响操作。",
            Risk::Medium => "将执行命令或修改文件，请确认参数。",
            Risk::High => "可能造成不可逆修改或访问网络，请仔细核对。",
        }
    }
}

/// What kind of answer a request wants.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RequestKind {
    /// A tool approval: allow once / for the session / deny.
    Approval,
    /// A choice among the request's options.
    Choice,
    /// Free text (or a longer text in an editor).
    Text,
    /// A secret (`vantry_request_secret`, passwords, keys): typed masked,
    /// never shown again or kept by the desktop.
    Secret,
    /// An interactive terminal's prompt (`vantry_terminal_run`).
    Terminal,
}

impl RequestKind {
    pub fn label(self) -> &'static str {
        match self {
            RequestKind::Approval => "工具审批",
            RequestKind::Choice => "需要选择",
            RequestKind::Text => "需要输入",
            RequestKind::Secret => "私密输入",
            RequestKind::Terminal => "交互终端",
        }
    }
}

/// The tool whose input requests are secrets.
pub const SECRET_TOOL: &str = "vantry_request_secret";

/// Words of a prompt that asks for a secret.
const SECRET_WORDS: &[&str] = &[
    "密码", "口令", "密钥", "秘钥", "令牌", "验证码", "password", "passphrase", "secret",
    "api key", "api_key", "apikey", "token", "otp",
];

/// Structured payload carried by `vantry_terminal_run` via Pi's editor UI request.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TerminalRunPayload {
    pub title: String,
    pub command: String,
    pub cwd: String,
}

/// Recognizes Vantry's structured interactive terminal request (`vantry_terminal_run`).
pub fn parse_terminal_run_payload(request: &PendingRequest) -> Option<TerminalRunPayload> {
    if request.method != "editor" || request.prefill.trim().is_empty() {
        return None;
    }
    let val: serde_json::Value = serde_json::from_str(&request.prefill).ok()?;
    if val["kind"].as_str() != Some("vantry_terminal_run") {
        return None;
    }
    let command = val["command"].as_str()?.trim().to_owned();
    if command.is_empty() {
        return None;
    }
    let mut title = val["title"].as_str().unwrap_or("").trim().to_owned();
    if title.is_empty() {
        title = request
            .title
            .trim()
            .trim_start_matches("交互终端 · ")
            .trim_start_matches("交互终端")
            .trim()
            .to_owned();
    }
    let cwd = val["cwd"].as_str().unwrap_or("").trim().to_owned();
    Some(TerminalRunPayload {
        title,
        command,
        cwd,
    })
}

/// What the modal shows for one request.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApprovalView {
    pub request_id: String,
    pub thread_id: String,
    pub run_id: String,
    /// confirm | select | input | editor
    pub method: String,
    /// The first line: what is asked ("允许执行 bash？").
    pub headline: String,
    /// The parameters: the command, the path (may be empty), as it will
    /// run: undrawn characters named, a long text cut in its middle.
    pub body: String,
    /// The tool's other arguments, one `key: value` per line (may be empty).
    pub details: String,
    pub options: Vec<String>,
    pub prefill: String,
    /// Parsed `vantry_terminal_run` command & directory when `kind == RequestKind::Terminal`.
    pub terminal_run: Option<TerminalRunPayload>,
    /// The approval select's options, when it is one.
    pub allow_once: Option<String>,
    pub allow_session: Option<String>,
    pub deny: Option<String>,
    pub risk: Risk,
    pub kind: RequestKind,
    /// pending | responding | failed …
    pub status: String,
    pub error: String,
}

impl ApprovalView {
    /// Whether it is answered with Allow / Deny (an approval select or a
    /// confirm), rather than a choice or text.
    pub fn is_approval(&self) -> bool {
        self.method == "confirm" || (self.allow_once.is_some() && self.deny.is_some())
    }

    /// Whether it can be answered now.
    pub fn answerable(&self) -> bool {
        self.status == "pending"
    }
}

/// The most parameter text a card shows, its beginning and its end together
/// (T3 Code's `APPROVAL_TEXT_LIMIT`, scaled to a native text view).
const MAX_BODY_CHARS: usize = 12_000;
/// How much of the end a cut text keeps: what runs last is often what matters.
const TAIL_CHARS: usize = 2_000;
/// The keys whose value is what a tool call is about, in order of preference.
const PRIMARY_ARGS: &[&str] = &["command", "cmd", "path", "file_path", "url"];

/// The modal's view of `request`.
pub fn approval_view(request: &PendingRequest) -> ApprovalView {
    approval_view_with(request, false)
}

/// The kind of `request`; `secret_tool` when its run is inside
/// [`SECRET_TOOL`].
pub fn request_kind(request: &PendingRequest, secret_tool: bool) -> RequestKind {
    let (allow_once, _, deny) = approval_options(request);
    if request.method == "confirm" || (allow_once.is_some() && deny.is_some()) {
        return RequestKind::Approval;
    }
    match request.method.as_str() {
        "select" => RequestKind::Choice,
        "editor"
            if parse_terminal_run_payload(request).is_some()
                || request.title.trim_start().starts_with("交互终端") =>
        {
            RequestKind::Terminal
        }
        "input" => {
            let text = format!("{}\n{}", request.title, request.message).to_lowercase();
            if secret_tool || SECRET_WORDS.iter().any(|w| text.contains(w)) {
                RequestKind::Secret
            } else {
                RequestKind::Text
            }
        }
        _ => RequestKind::Text,
    }
}

/// [`approval_view`], knowing whether the run is inside [`SECRET_TOOL`].
pub fn approval_view_with(request: &PendingRequest, secret_tool: bool) -> ApprovalView {
    let terminal_run = parse_terminal_run_payload(request);
    let (mut headline, mut body) = split_title(request);
    let mut details = String::new();
    if let Some((primary, rest)) = tool_arguments(&body) {
        body = primary;
        details = rest;
    }
    if let Some(ref tr) = terminal_run {
        if !tr.title.is_empty() {
            headline = tr.title.clone();
        }
        let mut lines = vec![format!("命令: {}", tr.command)];
        if !tr.cwd.is_empty() {
            lines.push(format!("目录: {}", tr.cwd));
        }
        body = lines.join("\n");
    }
    let risk = assess_risk(&request.title, &body, &request.message);
    let body = approval_text(&body, MAX_BODY_CHARS, TAIL_CHARS);
    let (allow_once, allow_session, deny) = approval_options(request);
    let kind = request_kind(request, secret_tool);
    ApprovalView {
        request_id: request.id.clone(),
        thread_id: request.thread_id.clone(),
        run_id: request.run_id.clone(),
        method: request.method.clone(),
        headline,
        body,
        details,
        options: request.options.clone(),
        // A secret is never prefilled.
        prefill: if kind == RequestKind::Secret {
            String::new()
        } else {
            request.prefill.clone()
        },
        terminal_run,
        allow_once,
        allow_session,
        deny,
        risk,
        kind,
        status: request.status.clone(),
        error: request.error.clone(),
    }
}

/// `splitApprovalTitle`: the message is the body when there is one, else
/// what follows the title's first line.
fn split_title(request: &PendingRequest) -> (String, String) {
    if !request.message.trim().is_empty() {
        return (
            request.title.trim().to_owned(),
            request.message.trim().to_owned(),
        );
    }
    match request.title.split_once('\n') {
        Some((first, rest)) => (first.trim().to_owned(), rest.trim().to_owned()),
        None => (request.title.trim().to_owned(), String::new()),
    }
}

/// `parseApprovalSelect`.
fn approval_options(request: &PendingRequest) -> (Option<String>, Option<String>, Option<String>) {
    if request.method != "select" || request.options.len() < 2 {
        return (None, None, None);
    }
    let mut allow_once = None;
    let mut allow_session = None;
    let mut deny = None;
    for option in &request.options {
        if option == "允许本次" {
            allow_once = Some(option.clone());
        } else if option.starts_with("本会话允许 ") {
            allow_session = Some(option.clone());
        } else if option == "拒绝" {
            deny = Some(option.clone());
        }
    }
    if allow_once.is_some() && deny.is_some() {
        (allow_once, allow_session, deny)
    } else {
        (None, None, None)
    }
}

/// A tool call's JSON arguments (as the approval extension summarizes them)
/// split into what it acts on (the command, the path) and the rest. A
/// summary cut short by the extension still yields its command, marked cut.
fn tool_arguments(body: &str) -> Option<(String, String)> {
    let body = body.trim();
    if !body.starts_with('{') {
        return None;
    }
    let Ok(serde_json::Value::Object(args)) = serde_json::from_str::<serde_json::Value>(body)
    else {
        return cut_tool_arguments(body);
    };
    let key = PRIMARY_ARGS
        .iter()
        .find(|key| args.get(**key).is_some_and(serde_json::Value::is_string))?;
    let primary = args[*key].as_str().unwrap_or_default().to_owned();
    let details = args
        .iter()
        .filter(|(k, _)| k.as_str() != *key)
        .map(|(k, v)| {
            let value = match v {
                serde_json::Value::String(s) => s.clone(),
                other => other.to_string(),
            };
            format!("{k}: {}", approval_text(&value, 400, 0))
        })
        .collect::<Vec<_>>()
        .join("\n");
    Some((primary, details))
}

/// `{"command":"…` cut before its end: decodes the string as far as it goes.
fn cut_tool_arguments(body: &str) -> Option<(String, String)> {
    let key = PRIMARY_ARGS.iter().find(|key| {
        body.strip_prefix('{')
            .and_then(|rest| rest.strip_prefix(&format!("\"{key}\":\"")))
            .is_some()
    })?;
    let start = format!("{{\"{key}\":\"").len();
    let mut out = String::new();
    let mut chars = body[start..].chars();
    while let Some(c) = chars.next() {
        match c {
            '"' => return None, // a complete string: the JSON was malformed elsewhere
            '\\' => match chars.next() {
                Some('n') => out.push('\n'),
                Some('t') => out.push('\t'),
                Some('r') => out.push('\r'),
                Some('b') => out.push('\u{8}'),
                Some('f') => out.push('\u{c}'),
                Some('u') => {
                    let hex: String = chars.by_ref().take(4).collect();
                    match u32::from_str_radix(&hex, 16).ok().and_then(char::from_u32) {
                        Some(c) if hex.len() == 4 => out.push(c),
                        _ => break,
                    }
                }
                Some(other) => out.push(other),
                None => break,
            },
            c => out.push(c),
        }
    }
    Some((out, "（参数过长，审批请求只带来了开头）".to_owned()))
}

/// Whether `c` draws nothing (or moves text around) on screen, so a reader
/// could not see it: control characters other than line breaks and tabs,
/// zero-width and bidirectional marks.
fn undrawn(c: char) -> bool {
    (c.is_control() && c != '\n' && c != '\t')
        || matches!(c,
            '\u{00AD}' | '\u{061C}' | '\u{180E}' | '\u{200B}'..='\u{200F}'
            | '\u{202A}'..='\u{202E}' | '\u{2060}'..='\u{2064}' | '\u{2066}'..='\u{2069}'
            | '\u{FEFF}')
}

/// What an approval asks to run, as it will run (T3 Code's `ApprovalText`):
/// characters a screen would not draw are shown by name, and text past
/// `max` characters keeps its beginning and its last `tail`, with a line
/// between them saying how much is left out.
pub fn approval_text(text: &str, max: usize, tail: usize) -> String {
    let named = |s: &str| {
        s.chars().fold(String::with_capacity(s.len()), |mut out, c| {
            if undrawn(c) {
                out.push_str(&format!("⟨U+{:04X}⟩", c as u32));
            } else {
                out.push(c);
            }
            out
        })
    };
    let total = text.chars().count();
    if total <= max {
        return named(text);
    }
    let tail = tail.min(max);
    let head_len = max - tail;
    let head: String = text.chars().take(head_len).collect();
    let end: String = text.chars().skip(total - tail).collect();
    let hidden = total - head_len - tail;
    if tail == 0 {
        return format!("{}…（另有 {hidden} 个字符未显示）", named(&head));
    }
    format!(
        "{}\n… 中间省略 {hidden} 个字符，结尾为 …\n{}",
        named(&head),
        named(&end)
    )
}

/// A coarse reading of what the request would do. Only a hint for the
/// user; the Core and the extension decide what is allowed.
pub fn assess_risk(title: &str, body: &str, message: &str) -> Risk {
    let text = format!("{title}\n{body}\n{message}").to_lowercase();
    const HIGH: &[&str] = &[
        "rm -rf",
        "rm -fr",
        "sudo ",
        "git push",
        "--force",
        "mkfs",
        "dd if=",
        "chmod -r",
        "chown -r",
        "| sh",
        "| bash",
        "curl ",
        "wget ",
        "drop table",
        "删除",
    ];
    const MEDIUM: &[&str] = &[
        "bash", "shell", "write", "edit", "exec", "命令", "写入", "修改", "执行",
    ];
    if HIGH.iter().any(|needle| text.contains(needle)) {
        Risk::High
    } else if MEDIUM.iter().any(|needle| text.contains(needle)) {
        Risk::Medium
    } else {
        Risk::Low
    }
}

/// The user's answer.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ApprovalDecision {
    /// 允许本次 / 允许执行.
    Allow,
    /// 本会话允许 … (approval selects that offer it).
    AllowSession,
    /// 拒绝.
    Deny,
    /// One of a select's options.
    Choose(String),
    /// An input's or editor's text.
    Submit(String),
    /// 取消请求.
    Cancel,
}

/// Why a decision cannot be sent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DecisionError {
    /// The request is no longer pending.
    NotPending,
    /// The decision does not fit the request (a choice it does not offer).
    Invalid(&'static str),
}

/// The `request.respond` command for `decision`, without its command id
/// (the caller names it once and reuses it on retry).
pub fn respond_command(
    request: &PendingRequest,
    decision: &ApprovalDecision,
) -> Result<Command, DecisionError> {
    if request.status != "pending" {
        return Err(DecisionError::NotPending);
    }
    let mut command = Command::new("", CommandKind::RespondRequest);
    command.thread_id = request.thread_id.clone();
    command.run_id = request.run_id.clone();
    command.request_id = request.id.clone();
    let (allow_once, allow_session, deny) = approval_options(request);
    match (request.method.as_str(), decision) {
        // Approval selects answer with the option's own text.
        ("select", ApprovalDecision::Allow) if allow_once.is_some() => {
            command.value = allow_once.unwrap_or_default();
        }
        ("select", ApprovalDecision::AllowSession) if allow_once.is_some() => {
            command.value = allow_session.ok_or(DecisionError::Invalid("本会话允许不可用"))?;
        }
        ("select", ApprovalDecision::Deny) if deny.is_some() => {
            command.value = deny.unwrap_or_default();
        }
        ("select", ApprovalDecision::Choose(value)) => {
            if !request.options.contains(value) {
                return Err(DecisionError::Invalid("请选择请求提供的选项"));
            }
            command.value = value.clone();
        }
        ("select", ApprovalDecision::Deny | ApprovalDecision::Cancel) => {
            command.cancelled = true;
        }
        ("confirm", ApprovalDecision::Allow) => {
            command.confirmed = true;
        }
        // A confirm's denial is `confirmed: false`, not a cancellation.
        ("confirm", ApprovalDecision::Deny | ApprovalDecision::Cancel) => {}
        ("input" | "editor", ApprovalDecision::Submit(value)) => {
            command.value = value.clone();
        }
        ("input" | "editor", ApprovalDecision::Deny | ApprovalDecision::Cancel) => {
            command.cancelled = true;
        }
        _ => return Err(DecisionError::Invalid("该请求不支持此操作")),
    }
    Ok(command)
}
