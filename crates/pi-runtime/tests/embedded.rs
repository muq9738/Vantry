//! Drive the actual embedded worker with a loopback SSE model, private HOME and Unix Core socket.
#![cfg(unix)]
use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::Path,
    process::Command,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncRead, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, UnixListener},
};
use vantry_pi_runtime::*;
use vantry_protocol::{ControlResponse, PiSetupRequest};

async fn request(stream: &mut (impl AsyncRead + Unpin)) -> Result<(String, Value)> {
    let mut r = BufReader::new(stream);
    let mut line = String::new();
    r.read_line(&mut line).await?;
    let url = line
        .split_whitespace()
        .nth(1)
        .context("missing path")?
        .to_owned();
    let mut len = 0;
    loop {
        line.clear();
        ensure!(r.read_line(&mut line).await? > 0, "short HTTP headers");
        if line == "\r\n" {
            break;
        }
        if let Some((k, v)) = line.split_once(':')
            && k.eq_ignore_ascii_case("content-length")
        {
            len = v.trim().parse()?;
        }
    }
    let mut body = vec![0; len];
    r.read_exact(&mut body).await?;
    Ok((
        url,
        if len == 0 {
            Value::Null
        } else {
            serde_json::from_slice(&body)?
        },
    ))
}
async fn serve_mcp(listener: TcpListener) -> Result<()> {
    loop {
        let (mut stream, _) = listener.accept().await?;
        let (_, body) = request(&mut stream).await?;
        let (status, result) = match body["method"].as_str() {
            Some("initialize") => (
                "200 OK",
                json!({"protocolVersion":body["params"]["protocolVersion"],"capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}),
            ),
            Some("tools/list") => (
                "200 OK",
                json!({"tools":[{"name":"ping","description":"Fixture ping","inputSchema":{"type":"object","properties":{}}}]}),
            ),
            Some("tools/call") => (
                "200 OK",
                json!({"content":[{"type":"text","text":"mcp-fixture-pong"}]}),
            ),
            Some("notifications/initialized") => ("202 Accepted", Value::Null),
            _ => ("405 Method Not Allowed", Value::Null),
        };
        let text = if result.is_null() {
            String::new()
        } else {
            json!({"jsonrpc":"2.0","id":body["id"],"result":result}).to_string()
        };
        stream.write_all(format!("HTTP/1.1 {status}\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{text}",text.len()).as_bytes()).await?;
    }
}
struct Call(&'static str, Value);
async fn fixture(root: &Path) -> Result<()> {
    const PRIVATE: &str = "fixture-dialog-value-7";
    let cwd = root.join("project");
    let data = root.join("data");
    fs::create_dir(&cwd)?;
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let agent = agent_dir();
    save_setup(
        &agent,
        &PiSetupRequest {
            provider_id: "local".into(),
            api_key: "test".into(),
            base_url: format!("http://{addr}/v1"),
            models: vec!["fixture".into(), "second".into()],
            set_default: true,
            ..Default::default()
        },
    )?;
    fs::write(
        agent.join("settings.json"),
        serde_json::to_vec(
            &json!({"defaultProvider":"local","defaultModel":"fixture","retry":{"enabled":false}}),
        )?,
    )?;
    let mcp = TcpListener::bind("127.0.0.1:0").await?;
    fs::write(
        agent.join("mcp.json"),
        serde_json::to_vec(
            &json!({"mcpServers":{"fixture":{"url":format!("http://{}/mcp", mcp.local_addr()?),"exposure":"direct"}}}),
        )?,
    )?;
    let mcp = tokio::spawn(serve_mcp(mcp));
    let socket = root.join("core.sock");
    let core = UnixListener::bind(&socket)?;
    let callback = tokio::spawn(async move {
        let (mut stream, _) = core.accept().await?;
        let (path, body) = request(&mut stream).await?;
        ensure!(
            path == "/v1/orchestration/delegate",
            "callback endpoint {path}"
        );
        ensure!(
            body["parentRunId"] == "run-a"
                && body["parentAttemptId"] == "a"
                && body["task"] == "fixture child",
            "incorrect callback body {body}"
        );
        let result = r#"{"taskId":"fixture-child","status":"running"}"#;
        stream.write_all(format!("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{result}",result.len()).as_bytes()).await?;
        let (mut stream, _) = core.accept().await?;
        let (path, body) = request(&mut stream).await?;
        ensure!(
            path == "/v1/orchestration/threads/send"
                && body["targetThreadId"] == "peer"
                && body["callerThreadId"] == "thread"
                && body["message"] == "fixture note",
            "incorrect peer callback"
        );
        let result = r#"{"deliveryMessageId":"peer-receipt"}"#;
        stream.write_all(format!("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{result}",result.len()).as_bytes()).await?;
        anyhow::Ok(())
    });
    let calls = vec![
        Some(Call(
            "write",
            json!({"path":"allowed.txt","content":"approved"}),
        )),
        Some(Call(
            "vantry_request_secret",
            json!({"prompt":"Fixture private input"}),
        )),
        Some(Call(
            "vantry_terminal_run",
            json!({"command":"pwd","title":"Fixture terminal"}),
        )),
        Some(Call(
            "codemode",
            json!({"code":"text(await tools.vantry_delegate_task({task:'fixture child'}))"}),
        )),
        None,
        Some(Call(
            "write",
            json!({"path":"forbidden.txt","content":"blocked"}),
        )),
        None,
        Some(Call(
            "write",
            json!({"path":"auto.txt","content":"automatic"}),
        )),
        None,
        Some(Call("write", json!({"path":"full.txt","content":"full"}))),
        Some(Call("mcp__fixture__ping", json!({}))),
        Some(Call(
            "codemode",
            json!({"code":"text(await tools.vantry_thread_send({threadId:'peer',message:'fixture note'}))"}),
        )),
        None,
        Some(Call(
            "write",
            json!({"path":"granted.txt","content":"persisted"}),
        )),
        None,
    ];
    let requests = Arc::new(Mutex::new(Vec::new()));
    let observed = requests.clone();
    let model = tokio::spawn(async move {
        for (n, call) in calls.into_iter().enumerate() {
            let (mut stream, _) = listener.accept().await?;
            let (_, body) = request(&mut stream).await?;
            let model = body["model"].clone();
            observed.lock().unwrap().push(body);
            let (delta, reason) = match call {
                Some(Call(name, args)) => (
                    json!({"role":"assistant","tool_calls":[{"index":0,"id":format!("call_{n}"),"type":"function","function":{"name":name,"arguments":args.to_string()}}]}),
                    "tool_calls",
                ),
                None => (json!({"role":"assistant","content":"finished"}), "stop"),
            };
            let mut sse = String::new();
            for (delta, stop) in [(delta, Value::Null), (json!({}), json!(reason))] {
                let chunk = json!({"id":"completion","object":"chat.completion.chunk","created":1,"model":model,"choices":[{"index":0,"delta":delta,"finish_reason":stop}]});
                sse.push_str(&format!("data: {chunk}\n\n"));
            }
            sse.push_str("data: [DONE]\n\n");
            stream.write_all(format!("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\nContent-Length: {}\r\n\r\n{sse}",sse.len()).as_bytes()).await?;
        }
        anyhow::Ok(())
    });
    let mut dialogs = vec![];
    for (id, policy, mode, model) in [
        ("a", "ask", "default", "fixture"),
        ("b", "full-access", "plan", "second"),
        ("c", "auto-accept-edits", "default", "second"),
        ("d", "full-access", "default", "second"),
        ("e", "ask", "default", "second"),
    ] {
        let j: JobRequest = serde_json::from_value(
            json!({"dataDir":data,"socketPath":socket,"enableUserMCP":id=="d","sandboxMode":if cfg!(target_os="macos") && id=="e" {"workspace-write"}else{"off"},"request":{"ThreadID":"thread","RunID":format!("run-{id}"),"AttemptID":id,"Model":format!("local/{model}"),"ApprovalPolicy":policy,"InteractionMode":mode,"Message":format!("message {id}"),"Workspace":{"kind":"directory","path":cwd}}}),
        )?;
        let dir = prepare_job(&j)?;
        let monitor = async {
            while !dir.join("events.jsonl").exists() {
                tokio::task::yield_now().await;
            }
            let mut r = IncrementalJournalReader::open(&dir, 0)?;
            loop {
                while let Some(event) = r.next_event()? {
                    if event.r#type != "extension_ui_request" {
                        continue;
                    }
                    let v = event.data.unwrap();
                    let method = v["method"].as_str().unwrap_or("");
                    if method == "notify" {
                        continue;
                    }
                    let value = match method {
                        "select"
                            if v["options"]
                                .as_array()
                                .is_some_and(|a| a.iter().any(|s| s == "本会话允许 write")) =>
                        {
                            "本会话允许 write"
                        }
                        "select" => v["options"][0].as_str().context("missing options")?,
                        "input" => PRIVATE,
                        "editor" => {
                            ensure!(
                                v["prefill"]
                                    .as_str()
                                    .unwrap_or("")
                                    .contains("vantry_terminal_run"),
                                "missing terminal request"
                            );
                            r#"{"status":"exited","exitCode":0}"#
                        }
                        "confirm" => "",
                        _ => anyhow::bail!("unexpected dialog {method}"),
                    };
                    dialogs.push(method.to_owned());
                    let request_id = v["id"].as_str().unwrap().to_string();
                    deliver_control(
                        &dir,
                        &ControlResponse {
                            id: format!("reply-{request_id}"),
                            request_id,
                            kind: "response".into(),
                            value: value.into(),
                            confirmed: true,
                            ..Default::default()
                        },
                    )?;
                }
                if dir.join("result.json").exists() {
                    return anyhow::Ok(());
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        };
        tokio::try_join!(run_worker(&dir), monitor)?;
        let log = fs::read_to_string(dir.join("events.jsonl"))?;
        ensure!(!log.contains(PRIVATE), "private value reached journal");
        if id == "a" {
            for line in log.lines() {
                let e: Value = serde_json::from_str(line)?;
                if e["type"] == "tool_execution_end" {
                    let d = &e["data"];
                    ensure!(
                        d["isError"] != true,
                        "tool {} failed: {}",
                        d["toolName"],
                        d["result"]
                    );
                }
            }
            let secrets =
                fs::read_dir(dir.join("secrets"))?.collect::<std::io::Result<Vec<_>>>()?;
            ensure!(secrets.len() == 1, "expected one private file");
            let p = secrets[0].path();
            ensure!(
                fs::metadata(&p)?.permissions().mode() & 0o777 == 0o600
                    && fs::read_to_string(&p)? == PRIVATE,
                "incorrect private handoff"
            );
            fs::remove_file(p)?;
            for file in fs::read_dir(dir.join("controls"))? {
                ensure!(
                    !fs::read_to_string(file?.path())?.contains(PRIVATE),
                    "control retained private input"
                );
            }
        }
    }
    model.await??;
    callback.await??;
    mcp.abort();
    if let Ok(result) = mcp.await {
        result?;
    }
    let history = fs::read_to_string(data.join("sessions/thread.jsonl"))?;
    ensure!(history.contains("peer-receipt"), "missing peer delivery");
    ensure!(
        history.contains("mcp-fixture-pong"),
        "MCP did not return fixture tool result"
    );
    ensure!(
        dialogs == ["select", "input", "select", "editor"],
        "unexpected dialogs {dialogs:?}"
    );
    ensure!(
        cwd.join("allowed.txt").exists()
            && !cwd.join("forbidden.txt").exists()
            && cwd.join("auto.txt").exists()
            && cwd.join("granted.txt").exists()
            && cwd.join("full.txt").exists(),
        "approval effects mismatch"
    );
    let observed = requests.lock().unwrap();
    let initial_system = observed[0]["messages"].to_string();
    ensure!(
        initial_system.contains("Run sandboxed JavaScript to filter large payloads, call codemode MCP/Vantry tools")
            && initial_system.contains("Call native read, grep, find, and ls tools directly in parallel")
            && !initial_system.contains("instead of many separate calls"),
        "Vantry codemode snippet/guidelines did not override Pi defaults"
    );
    ensure!(
        !observed[0]["tools"]
            .to_string()
            .contains("vantry_delegate_task")
            && !observed[0]["tools"]
                .to_string()
                .contains("vantry_thread_send"),
        "orchestration vantry_* tools must stay deferred/codemode instead of upfront direct tools"
    );
    ensure!(
        !observed[0]["tools"]
            .to_string()
            .contains("mcp__fixture__ping")
            && observed[9]["tools"]
                .to_string()
                .contains("mcp__fixture__ping"),
        "MCP switch did not control tool exposure"
    );
    ensure!(
        observed[5]["model"] == "second"
            && observed[5]["messages"].to_string().contains("message a"),
        "lost history on model switch"
    );
    ensure!(
        observed[4]["messages"]
            .to_string()
            .contains("fixture-child"),
        "missing Core result"
    );
    ensure!(
        !fs::read_to_string(data.join("sessions/thread.jsonl"))?.contains(PRIVATE),
        "private value reached session"
    );
    Ok(())
}
#[test]
fn embedded_worker_tools_and_session() -> Result<()> {
    const KEY: &str = "VANTRY_PI_FIXTURE_ROOT";
    if let Some(root) = std::env::var_os(KEY) {
        return tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()?
            .block_on(async {
                tokio::time::timeout(Duration::from_secs(45), fixture(Path::new(&root))).await?
            });
    }
    let t = tempfile::tempdir()?;
    let path = t.path().canonicalize()?;
    let status = Command::new(std::env::current_exe()?)
        .args([
            "--exact",
            "embedded_worker_tools_and_session",
            "--nocapture",
        ])
        .env_clear()
        .env(KEY, &path)
        .env("HOME", &path)
        .env("TMPDIR", &path)
        .env("PATH", "/usr/bin:/bin")
        .env("PI_CODING_AGENT_DIR", path.join("pi"))
        .env("PI_OFFLINE", "1")
        .env("NO_PROXY", "127.0.0.1,localhost")
        .status()?;
    ensure!(status.success(), "isolated Pi fixture failed");
    Ok(())
}
