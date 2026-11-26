//! A relay that drops the model stream with a bare `error` event ("Error Code
//! null: Unknown error") must not end the attempt: the worker asks the model to
//! continue, at most twice.
#![cfg(unix)]
use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use std::{
    path::Path,
    process::Command,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
};
use vantry_pi_runtime::*;
use vantry_protocol::PiSetupRequest;

async fn read_body(stream: &mut TcpStream) -> Result<Value> {
    let mut r = BufReader::new(stream);
    let mut line = String::new();
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
    Ok(serde_json::from_slice(&body)?)
}

fn frame(data: Value) -> String {
    format!("event: {}\ndata: {data}\n\n", data["type"].as_str().unwrap())
}

fn blank_error() -> String {
    frame(json!({"type": "error", "code": null}))
}

fn answer() -> String {
    let item = json!({"type": "message", "id": "msg_1", "role": "assistant", "content": []});
    let done = json!({"type": "message", "id": "msg_1", "content": [{"type": "output_text", "text": "继续完成"}]});
    [
        frame(json!({"type": "response.created", "response": {"id": "resp_1"}})),
        frame(json!({"type": "response.output_item.added", "output_index": 0, "item": item})),
        frame(json!({"type": "response.output_text.delta", "output_index": 0, "delta": "继续完成"})),
        frame(json!({"type": "response.output_item.done", "output_index": 0, "item": done})),
        frame(json!({"type": "response.completed", "response": {"id": "resp_1", "status": "completed",
            "usage": {"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}, "output": []}})),
    ]
    .concat()
}

async fn scenario(root: &Path, replies: Vec<String>, attempt: &str) -> Result<(Value, Vec<Value>)> {
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let agent = agent_dir();
    save_setup(
        &agent,
        &PiSetupRequest {
            provider_id: "relay".into(),
            api_key: "test".into(),
            base_url: format!("http://{addr}/v1"),
            api: "openai-responses".into(),
            models: vec!["fixture".into()],
            set_default: true,
            ..Default::default()
        },
    )?;
    std::fs::write(
        agent.join("settings.json"),
        serde_json::to_vec(&json!({"defaultProvider":"relay","defaultModel":"fixture"}))?,
    )?;
    let bodies = Arc::new(Mutex::new(Vec::new()));
    let seen = bodies.clone();
    let server = tokio::spawn(async move {
        for sse in replies {
            let (mut stream, _) = listener.accept().await?;
            let body = read_body(&mut stream).await?;
            seen.lock().unwrap().push(body);
            stream.write_all(format!("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\nContent-Length: {}\r\n\r\n{sse}", sse.len()).as_bytes()).await?;
        }
        anyhow::Ok(())
    });
    let cwd = root.join("project");
    std::fs::create_dir_all(&cwd)?;
    let job: JobRequest = serde_json::from_value(json!({
        "dataDir": root.join("data"), "socketPath": root.join("core.sock"),
        "request": {"ThreadID": format!("thread-{attempt}"), "RunID": format!("run-{attempt}"),
            "AttemptID": attempt, "Model": "relay/fixture", "ApprovalPolicy": "full-access",
            "InteractionMode": "default", "Message": "原始任务",
            "Workspace": {"kind": "directory", "path": cwd}}
    }))?;
    let dir = prepare_job(&job)?;
    let _ = run_worker(&dir).await;
    server.abort();
    let result: Value = serde_json::from_slice(&std::fs::read(dir.join("result.json"))?)?;
    let bodies = bodies.lock().unwrap().clone();
    Ok((result, bodies))
}

async fn fixture(root: &Path) -> Result<()> {
    // One dropped stream, then the model answers.
    let (result, bodies) = scenario(root, vec![blank_error(), answer()], "a").await?;
    ensure!(result["status"] == "completed", "attempt did not recover: {result}");
    ensure!(bodies.len() == 2, "expected one retry, saw {} requests", bodies.len());
    let input = bodies[1]["input"].to_string();
    ensure!(
        input.contains("原始任务") && input.contains("请从中断处继续"),
        "the retry lost the task or the continue prompt: {input}"
    );

    // The relay keeps failing: two retries, then the attempt fails with the cause.
    let replies = vec![blank_error(), blank_error(), blank_error(), answer()];
    let (result, bodies) = scenario(root, replies, "b").await?;
    ensure!(result["status"] == "failed", "attempt should fail: {result}");
    ensure!(bodies.len() == 3, "expected two retries, saw {} requests", bodies.len());
    ensure!(
        result["error"].as_str().is_some_and(|e| e.contains("Unknown error")),
        "missing upstream cause: {result}"
    );
    Ok(())
}

#[test]
fn blank_upstream_failures_continue_the_attempt() -> Result<()> {
    const KEY: &str = "VANTRY_PI_BLANK_FAILURE_ROOT";
    if let Some(root) = std::env::var_os(KEY) {
        return tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()?
            .block_on(async {
                tokio::time::timeout(Duration::from_secs(60), fixture(Path::new(&root))).await?
            });
    }
    let t = tempfile::tempdir()?;
    let path = t.path().canonicalize()?;
    let status = Command::new(std::env::current_exe()?)
        .args(["--exact", "blank_upstream_failures_continue_the_attempt", "--nocapture"])
        .env_clear()
        .env(KEY, &path)
        .env("HOME", &path)
        .env("TMPDIR", &path)
        .env("PATH", "/usr/bin:/bin")
        .env("PI_CODING_AGENT_DIR", path.join("pi"))
        .env("PI_OFFLINE", "1")
        .env("NO_PROXY", "127.0.0.1,localhost")
        .status()
        .context("isolated run")?;
    ensure!(status.success(), "isolated Pi fixture failed");
    Ok(())
}
