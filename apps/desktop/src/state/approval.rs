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
    /// The parameters: the command, the path (may be empty).
    pub body: String,
    pub options: Vec<String>,
    pub prefill: String,
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

/// The longest parameter text the modal shows.
const MAX_BODY_CHARS: usize = 1200;

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
        "editor" if request.title.trim_start().starts_with("交互终端") => RequestKind::Terminal,
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
    let (headline, body) = split_title(request);
    let body = truncate_chars(&body, MAX_BODY_CHARS);
    let (allow_once, allow_session, deny) = approval_options(request);
    let risk = assess_risk(&request.title, &body, &request.message);
    ApprovalView {
        request_id: request.id.clone(),
        thread_id: request.thread_id.clone(),
        run_id: request.run_id.clone(),
        method: request.method.clone(),
        headline,
        body,
        options: request.options.clone(),
        // A secret is never prefilled.
        prefill: if request_kind(request, secret_tool) == RequestKind::Secret {
            String::new()
        } else {
            request.prefill.clone()
        },
        allow_once,
        allow_session,
        deny,
        risk,
        kind: request_kind(request, secret_tool),
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

fn truncate_chars(text: &str, max: usize) -> String {
    match text.char_indices().nth(max) {
        Some((end, _)) => format!("{}…", &text[..end]),
        None => text.to_owned(),
    }
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
