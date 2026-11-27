use super::bootstrap::{Choice, selection};
use anyhow::{Result, bail, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde_json::{Value, json};
use std::collections::{BTreeMap, BTreeSet};
use vantry_client::{ClientError, CoreClient, EventStream};
use vantry_protocol::{Command, CommandKind, CoreEvent, ErrorCode, Run, ThreadDetail};

#[derive(Clone)]
pub(super) struct Scope {
    pub core: CoreClient,
    pub thread: String,
    pub pairing: String,
    pub lan: Option<std::net::SocketAddr>,
}

fn error(status: u16, message: &str) -> (u16, Value) {
    (status, json!({"error":message}))
}

fn body(request: &Value) -> Result<Value, (u16, Value)> {
    B64.decode(request["b"].as_str().unwrap_or(""))
        .ok()
        .and_then(|v| serde_json::from_slice(&v).ok())
        .ok_or_else(|| error(400, "Invalid request body"))
}

fn core_error(e: ClientError) -> (u16, Value) {
    match e {
        ClientError::Api {
            code: ErrorCode::NotFound,
            ..
        } => error(404, "Thread not found"),
        ClientError::Api {
            code: ErrorCode::Conflict,
            ..
        } => error(409, "Command conflicts with current state"),
        ClientError::Api {
            code: ErrorCode::InvalidCommand,
            ..
        } => error(400, "Invalid command"),
        _ => error(503, "Core unavailable"),
    }
}

fn message(run: &Run, assistant: bool) -> Value {
    let (id, role, text) = if assistant {
        (run.id.clone(), "assistant", &run.output)
    } else {
        (format!("{}:user", run.id), "user", &run.input)
    };
    let mut part = json!({"type":"text","text":text});
    if assistant {
        part["state"] = if run.status.terminal() {
            "done"
        } else {
            "streaming"
        }
        .into();
    }
    json!({"id":id,"threadId":run.thread_id,"slotId":id,"parentToolCallId":null,
        "message":{"id":id,"role":role,"parts":[part]},
        "timestamp":run.created_at,"createdAt":run.created_at,"updatedAt":run.updated_at,
        "metadata":{"slotId":id,"versionIndex":0,"versionCount":1}})
}

fn history(detail: &ThreadDetail, operation_prefix: &str) -> Vec<Value> {
    let mut messages = Vec::new();
    let mut parent = Value::Null;
    for run in detail.runs.iter().flatten() {
        for assistant in [false, true] {
            let mut row = message(run, assistant);
            row["parentId"] = parent;
            row["depth"] = messages.len().into();
            row["metadata"]["depth"] = messages.len().into();
            if !assistant
                && let Some(id) = detail
                    .send_command_ids
                    .get(&run.id)
                    .and_then(|cmd| cmd.strip_prefix(operation_prefix))
            {
                row["metadata"]["optimisticId"] = id.into();
            }
            parent = row["id"].clone();
            messages.push(row);
        }
    }
    messages
}

fn thread(detail: &ThreadDetail, workspace: &str) -> Value {
    let t = &detail.thread;
    let runs = detail.runs.as_deref().unwrap_or_default();
    let updated = runs
        .iter()
        .map(|r| &r.updated_at)
        .max()
        .unwrap_or(&t.created_at);
    thread_row(
        workspace,
        &t.id,
        &t.title,
        &t.model,
        &t.created_at,
        updated,
        t.pinned,
        runs.iter().any(|r| !r.status.terminal()),
    )
}

fn thread_row(
    workspace: &str,
    id: &str,
    title: &str,
    model: &str,
    created: &str,
    updated: &str,
    pinned: bool,
    generating: bool,
) -> Value {
    json!({"id":id,"title":title,"model":selection(model),"workspaceId":workspace,
        "createdAt":created,"updatedAt":updated,"isGenerating":generating,"isFavorited":pinned,
        "isFavoritePinned":false,"favoritePinnedOrder":0,"isIncognito":false,"enableArtifacts":false,
        "tools":[],"metadata":{}})
}

impl Scope {
    fn operation_prefix(&self) -> String {
        format!("alma/{}/op/", self.pairing)
    }

    async fn detail(&self) -> Result<ThreadDetail, ClientError> {
        self.core
            .get_json(&format!(
                "/v1/threads/detail?threadId={}&commandPrefix={}",
                self.thread,
                self.operation_prefix()
            ))
            .await
    }

    async fn send(
        &self,
        text: &str,
        operation: Option<&str>,
        model: Option<&Value>,
    ) -> Result<vantry_protocol::Receipt, (u16, Value)> {
        if text.trim().is_empty() || text.len() > 256 * 1024 {
            return Err(error(400, "Invalid message text"));
        }
        let command_id = match operation {
            Some(id) if !id.is_empty() && id.len() <= 200 => {
                format!("{}{id}", self.operation_prefix())
            }
            Some(_) => return Err(error(400, "Invalid operation ID")),
            None => {
                let thread = self
                    .core
                    .thread_detail(&self.thread, 1)
                    .await
                    .map_err(core_error)?
                    .thread;
                if thread.provider_id == vantry_protocol::GEMINI_WRITING_PROVIDER {
                    return Err(error(400, "写作消息缺少可用于重连的操作 ID"));
                }
                format!("alma/{}/new/{}", self.pairing, uuid::Uuid::new_v4())
            }
        };
        let mut cmd = Command::new(command_id, CommandKind::SendMessage);
        cmd.thread_id = self.thread.clone();
        cmd.message = text.into();
        if let Some(model) = model.filter(|m| !m.is_null()) {
            let choice = self.resolve_model(model).await?;
            cmd.model = choice.model;
            cmd.provider_id = choice.provider;
        }
        self.core.dispatch(&cmd).await.map_err(core_error)
    }

    async fn resolve_model(&self, value: &Value) -> Result<Choice, (u16, Value)> {
        let selected = value.as_str().ok_or_else(|| error(400, "Invalid model"))?;
        let thread = self
            .core
            .thread_detail(&self.thread, 1)
            .await
            .map_err(core_error)?
            .thread;
        self.choices(&thread)
            .await
            .into_iter()
            .find(|c| selection(&c.model) == selected)
            .ok_or_else(|| error(400, "Model unavailable on this thread"))
    }

    async fn http(&self, request: &Value) -> Result<(u16, Value), (u16, Value)> {
        let uri: http::Uri = request["path"]
            .as_str()
            .unwrap_or("")
            .parse()
            .map_err(|_| error(400, "Invalid path"))?;
        let mut segments = uri.path().split('/');
        if uri.scheme().is_some() || uri.authority().is_some() {
            return Err(error(404, "Unknown endpoint"));
        }
        if request["method"] == "GET" {
            if uri.path() == "/__alma_lan__"
                && let Some(bound) = self.lan
            {
                let addresses = crate::lan::listener_addresses(bound)
                    .map_err(|_| error(503, "Local network unavailable"))?;
                let hosts: Vec<_> = addresses.iter().map(|addr| addr.ip()).collect();
                return Ok((200, json!({"hosts": hosts, "port": bound.port()})));
            }
            if let Some(body) = self.bootstrap(uri.path()).await.map_err(core_error)? {
                return Ok((200, body));
            }
        }
        if segments.next() != Some("")
            || segments.next() != Some("api")
            || segments.next() != Some("threads")
        {
            return Err(error(404, "Unknown endpoint"));
        }
        let target = segments.next();
        if target.is_some_and(|s| s != self.thread) {
            return Err(error(404, "Thread not found"));
        }
        let action = segments.next();
        if segments.next().is_some() {
            return Err(error(404, "Unknown endpoint"));
        }
        let params: BTreeMap<_, _> =
            url::form_urlencoded::parse(uri.query().unwrap_or("").as_bytes()).collect();
        let param = |key: &str| params.get(key).map(|v| v.as_ref());
        match (request["method"].as_str(), target, action) {
            (Some("GET"), None, None) => {
                if param("ids").is_some_and(|list| !list.split(',').any(|id| id == self.thread)) {
                    return Ok((200, json!([])));
                }
                let summaries = self.core.thread_summaries().await.map_err(core_error)?;
                let shared = summaries
                    .threads
                    .into_iter()
                    .flatten()
                    .find(|t| t.id == self.thread);
                let rows: Vec<_> = shared
                    .iter()
                    .map(|t| {
                        thread_row(
                            &self.pairing,
                            &t.id,
                            &t.title,
                            &t.model,
                            &t.created_at,
                            &t.updated_at,
                            t.pinned,
                            t.active_runs > 0,
                        )
                    })
                    .collect();
                Ok((200, rows.into()))
            }
            (Some("GET"), Some(_), None | Some("messages")) => {
                let detail = self.detail().await.map_err(core_error)?;
                let rows = history(&detail, &self.operation_prefix());
                let total = rows.len();
                if action.is_none() {
                    let mut view = thread(&detail, &self.pairing);
                    let end = match param("before") {
                        Some(id) => rows.iter().position(|row| row["id"] == id)
                            .ok_or_else(|| (409,json!({"error":"Message is no longer on the thread path","code":"window_anchor_gone"})))?,
                        None => total,
                    };
                    let window = if param("before").is_some() {
                        param("limit").or(Some("200"))
                    } else {
                        param("tail")
                    };
                    let start = window
                        .and_then(|v| v.parse::<usize>().ok())
                        .filter(|n| *n > 0)
                        .map_or(0, |n| end.saturating_sub(n.min(2000)));
                    view["metadata"]["activePath"] = rows.iter().map(|v| v["id"].clone()).collect();
                    view["messages"] = json!(&rows[start..end]);
                    view["messagesOffset"] = start.into();
                    view["messagesTotal"] = total.into();
                    return Ok((200, view));
                }
                if param("page") != Some("1") {
                    return Ok((200, rows.into()));
                }
                let limit = param("limit")
                    .unwrap_or("20")
                    .parse::<usize>()
                    .ok()
                    .filter(|v| (1..=100).contains(v))
                    .ok_or_else(|| error(400, "Invalid limit"))?;
                let offset = param("offset")
                    .unwrap_or("0")
                    .parse::<usize>()
                    .map_err(|_| error(400, "Invalid offset"))?;
                let end = total.min(offset.saturating_add(limit));
                Ok((
                    200,
                    json!({"messages":&rows[offset.min(total)..end],"scope":"active-path","missingMessageIds":[],
                    "total":total,"limit":limit,"offset":offset,"hasMore":end<total,"nextOffset":(end<total).then_some(end)}),
                ))
            }
            (Some("GET"), Some(_), Some("file-writes")) => Ok((200, json!({"fileWrites":[]}))),
            (Some("GET"), Some(_), Some("diff-stats")) => {
                Ok((200, json!({"additions":0,"deletions":0,"filesChanged":0})))
            }
            (Some("PUT"), Some(_), None) => {
                let body = body(request)?;
                if !body.as_object().is_some_and(|o| {
                    o.keys()
                        .all(|k| matches!(k.as_str(), "model" | "skipUpdatedAt"))
                }) {
                    return Err(error(400, "Only the shared thread model can be changed"));
                }
                let choice = self.resolve_model(&body["model"]).await?;
                let mut cmd = Command::new(
                    format!("alma/{}/model/{}", self.pairing, uuid::Uuid::new_v4()),
                    CommandKind::SetThreadModel,
                );
                cmd.thread_id = self.thread.clone();
                cmd.model = choice.model;
                cmd.provider_id = choice.provider;
                self.core.dispatch(&cmd).await.map_err(core_error)?;
                let detail = self
                    .core
                    .thread_detail(&self.thread, 1)
                    .await
                    .map_err(core_error)?;
                Ok((200, thread(&detail, &self.pairing)))
            }
            (Some("POST"), Some(_), Some("send")) => {
                let body = body(request)?;
                if body.get("fromThreadId").is_some() {
                    return Err(error(400, "This pairing uses the shared thread's settings"));
                }
                let receipt = self
                    .send(
                        body["text"].as_str().unwrap_or(""),
                        body["optimisticId"].as_str(),
                        body.get("model"),
                    )
                    .await?;
                Ok((
                    202,
                    json!({"ok":true,"started":receipt.accepted,"threadId":receipt.thread_id}),
                ))
            }
            _ => Err(error(404, "Unknown endpoint")),
        }
    }
}

fn user_text(payload: &Value, thread: &str) -> Result<String> {
    let data = &payload["data"];
    ensure!(data["threadId"] == thread, "Thread not shared");
    ensure!(
        payload["type"] == "generate_response"
            && data["userMessage"]["role"] == "user"
            && data["retryOfMessageId"].is_null()
            && data["replaceMessageId"].is_null()
            && data["regenerate"] != true,
        "Unsupported chat operation"
    );
    let parts = data["userMessage"]["parts"]
        .as_array()
        .ok_or_else(|| anyhow::anyhow!("Text message required"))?;
    let mut text = String::new();
    for part in parts {
        ensure!(part["type"] == "text", "Only text messages are supported");
        text.push_str(
            part["text"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("Text message required"))?,
        );
    }
    Ok(text)
}

fn ws(id: &str, kind: &str, data: Value) -> Value {
    json!({"t":"wmsg","id":id,"s":json!({"type":kind,"data":data,"timestamp":chrono::Utc::now()}).to_string()})
}

struct Subscription {
    cursor: i64,
    active: BTreeSet<String>,
    optimistic: BTreeMap<String, String>,
}

pub(super) struct Chat {
    scope: Scope,
    sockets: BTreeMap<String, Subscription>,
    notifications: BTreeSet<String>,
    events: Option<EventStream>,
}

impl Chat {
    pub fn new(scope: Scope) -> Self {
        Self {
            scope,
            sockets: BTreeMap::new(),
            notifications: BTreeSet::new(),
            events: None,
        }
    }

    pub async fn next_event(&mut self) -> Result<Option<CoreEvent>, ClientError> {
        let Some(events) = self.events.as_mut() else {
            return std::future::pending().await;
        };
        events.next_event().await
    }

    pub async fn resync(&mut self) -> Result<Vec<Value>> {
        let detail = self.scope.detail().await?;
        self.events = Some(self.scope.core.subscribe(detail.sequence).await?);
        let mut replies = Vec::new();
        for id in std::mem::take(&mut self.sockets).into_keys() {
            replies.extend(self.snapshot(&id, &detail));
        }
        Ok(replies)
    }

    fn snapshot(&mut self, id: &str, detail: &ThreadDetail) -> Vec<Value> {
        let active: BTreeSet<_> = detail
            .runs
            .iter()
            .flatten()
            .filter(|r| !r.status.terminal())
            .map(|r| r.id.clone())
            .collect();
        let ids: Vec<_> = if active.is_empty() {
            vec![]
        } else {
            vec![&self.scope.thread]
        };
        self.sockets.insert(
            id.into(),
            Subscription {
                cursor: detail.sequence,
                active,
                optimistic: BTreeMap::new(),
            },
        );
        let mut replies = vec![ws(
            id,
            "generating_snapshot",
            json!({"ids":ids,"compacting":[]}),
        )];
        let rows = history(detail, &self.scope.operation_prefix());
        let sync = ws(
            id,
            "thread_messages_sync",
            json!({"threadId":self.scope.thread,"messages":rows}),
        );
        // Older clients read history over HTTP. Keep the live socket usable when the
        // full history would not fit into a single negotiated WebSocket message.
        if serde_json::to_vec(&sync)
            .is_ok_and(|bytes| bytes.len() < super::MAX_FRAME * 3 / 4 - 2048)
        {
            replies.push(sync);
        } else {
            replies.push(ws(
                id,
                "thread_updated",
                thread(detail, &self.scope.pairing),
            ));
        }
        replies
    }

    pub async fn request(&mut self, frame: Value) -> Result<Vec<Value>> {
        let id = frame["id"].as_str().unwrap_or("");
        ensure!(!id.is_empty() && id.len() <= 255, "Invalid request ID");
        match frame["t"].as_str() {
            Some("hreq") => {
                let (status, body) = self.scope.http(&frame).await.unwrap_or_else(|e| e);
                let bytes = serde_json::to_vec(&body)?;
                let mut replies = vec![
                    json!({"t":"hres","id":id,"status":status,"headers":{"content-type":"application/json","cache-control":"no-store"}}),
                ];
                replies.extend(
                    bytes
                        .chunks(32 * 1024)
                        .map(|chunk| json!({"t":"hchunk","id":id,"b":B64.encode(chunk)})),
                );
                replies.push(json!({"t":"hend","id":id}));
                Ok(replies)
            }
            Some("wopen") => {
                let path = frame["path"].as_str().and_then(|s| s.split('?').next());
                if !matches!(
                    path,
                    Some("/ws/threads" | "/ws/handoff/user-questions" | "/ws/herdr")
                ) || self.sockets.len() + self.notifications.len() >= 8
                    || self.sockets.contains_key(id)
                    || self.notifications.contains(id)
                {
                    return Ok(vec![
                        json!({"t":"werr","id":id,"message":"Unavailable socket"}),
                    ]);
                }
                if path != Some("/ws/threads") {
                    self.notifications.insert(id.to_owned());
                    let mut frames = vec![json!({"t":"wopened","id":id})];
                    if path == Some("/ws/herdr") {
                        // iOS holds the host presence socket open even on a
                        // chat-only computer with no Herdr agent runtime.
                        for event in [
                            json!({"type":"herdr","online":false,"reason":"Vantry chat host"}),
                            json!({"type":"pins","tabs":[]}),
                            json!({"type":"archived","count":0}),
                        ] {
                            frames.push(json!({"t":"wmsg","id":id,"s":event.to_string()}));
                        }
                    }
                    return Ok(frames);
                }
                let detail = self.scope.detail().await?;
                if self.events.is_none() {
                    self.events = Some(self.scope.core.subscribe(detail.sequence).await?);
                }
                let mut replies = vec![json!({"t":"wopened","id":id})];
                replies.extend(self.snapshot(id, &detail));
                Ok(replies)
            }
            Some("wmsg") if self.sockets.contains_key(id) => {
                let payload: Value = serde_json::from_str(frame["s"].as_str().unwrap_or(""))?;
                self.generate(id, &payload).await
            }
            Some("wclose") => {
                self.sockets.remove(id);
                self.notifications.remove(id);
                if self.sockets.is_empty() {
                    self.events = None;
                }
                Ok(vec![])
            }
            Some("hcancel") => Ok(vec![]),
            Some("wmsg") => Ok(vec![
                json!({"t":"werr","id":id,"message":"Unavailable socket"}),
            ]),
            _ => bail!("Unsupported Alma frame"),
        }
    }

    async fn generate(&mut self, socket: &str, payload: &Value) -> Result<Vec<Value>> {
        let data = &payload["data"];
        let optimistic = data
            .pointer("/userMessageMetadata/optimisticId")
            .and_then(Value::as_str)
            .or_else(|| data.pointer("/userMessage/id").and_then(Value::as_str));
        let result = match user_text(payload, &self.scope.thread) {
            Ok(text) => self.scope.send(&text, optimistic, data.get("model")).await,
            Err(e) => Err(error(400, &e.to_string())),
        };
        let receipt = match result {
            Ok(receipt) => receipt,
            Err((_, body)) => {
                return Ok(vec![ws(
                    socket,
                    "generation_error",
                    json!({"threadId": self.scope.thread, "error": body["error"]}),
                )]);
            }
        };
        if receipt.run_id.is_empty() {
            let detail = self.scope.detail().await?;
            return Ok(self.snapshot(socket, &detail));
        }
        let Some(op) = optimistic else {
            return Ok(vec![]);
        };
        let subscription = self.sockets.get_mut(socket).expect("open chat socket");
        if receipt.sequence > subscription.cursor {
            subscription
                .optimistic
                .insert(receipt.run_id, op.to_owned());
            return Ok(vec![]);
        }

        // Core already consumed this operation. Echo the same persisted user row for the phone's pending bubble.
        let detail = self.scope.core.thread_detail(&self.scope.thread, 0).await?;
        let existing = detail
            .runs
            .iter()
            .flatten()
            .find(|r| r.id == receipt.run_id);
        Ok(existing
            .map(|run| {
                let mut row = message(run, false);
                row["metadata"]["optimisticId"] = op.into();
                ws(socket, "message_added", row)
            })
            .into_iter()
            .collect())
    }

    pub fn event(&mut self, event: CoreEvent) -> Result<Vec<Value>> {
        if event.thread_id != self.scope.thread {
            return Ok(vec![]);
        }
        let mut frames = Vec::new();
        for (id, sub) in &mut self.sockets {
            if event.sequence <= sub.cursor {
                continue;
            }
            sub.cursor = event.sequence;
            if event.r#type == "run.progress" {
                frames.push(ws(
                    id,
                    "thread_generating",
                    json!({"id":event.thread_id,"isGenerating":true,"status":event.data["status"]}),
                ));
            } else if event.r#type == "thread.updated" {
                let t: vantry_protocol::Thread =
                    serde_json::from_value(event.data["thread"].clone())?;
                frames.push(ws(
                    id,
                    "thread_updated",
                    thread_row(
                        &self.scope.pairing,
                        &t.id,
                        &t.title,
                        &t.model,
                        &t.created_at,
                        &event.timestamp,
                        t.pinned,
                        !sub.active.is_empty(),
                    ),
                ));
            } else if event.r#type == "run.output" {
                let delta = json!({"type":"text_append","threadId":event.thread_id,"messageId":event.run_id,
                    "seq":event.sequence,"partIndex":0,"partType":"text","text":event.data["delta"]});
                frames.push(ws(
                    id,
                    "message_delta",
                    json!({"threadId":event.thread_id,"messageId":event.run_id,"deltas":[delta]}),
                ));
            } else if let Some(run) = event.data.get("run") {
                let run: Run = serde_json::from_value(run.clone())?;
                let was_active = !sub.active.is_empty();
                if run.status.terminal() {
                    sub.active.remove(&run.id);
                    frames.push(ws(id, "message_updated", message(&run, true)));
                    if !run.error.is_empty() {
                        frames.push(ws(
                            id,
                            "generation_error",
                            json!({"threadId":run.thread_id,"error":run.error}),
                        ));
                    }
                } else if sub.active.insert(run.id.clone()) {
                    let mut user = message(&run, false);
                    if let Some(op) = sub.optimistic.remove(&run.id) {
                        user["metadata"]["optimisticId"] = op.into();
                    }
                    frames.push(ws(id, "message_added", user));
                    frames.push(ws(id, "message_added", message(&run, true)));
                }
                let generating = !sub.active.is_empty();
                if was_active != generating {
                    frames.push(ws(
                        id,
                        "thread_generating",
                        json!({"id":run.thread_id,"isGenerating":generating}),
                    ));
                }
            }
        }
        Ok(frames)
    }
}
