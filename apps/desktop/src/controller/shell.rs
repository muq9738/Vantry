//! A shell belongs to its session until it exits or its tab is closed.
use super::*;
use std::collections::HashMap;
use tokio::sync::mpsc::{UnboundedSender, unbounded_channel};

enum Input {
    Data(String),
    Size(u16, u16),
    Close,
}
thread_local! { static SHELLS: RefCell<HashMap<String,UnboundedSender<Input>>> = RefCell::new(HashMap::new()); }

pub(crate) fn native_shell() -> bool {
    cfg!(unix) && !bridges(|b| b.mock) && !remote_project()
}

pub(crate) fn shell_start(id: String) {
    let key = format!("{}\x1f{id}", current_machine().unwrap_or_default());
    if id.starts_with("task-") || SHELLS.with(|all| all.borrow().contains_key(&key)) {
        return;
    }
    let Some(bridge) = project_workbench()
        .map(|(_, bridge)| bridge)
        .or_else(workbench)
    else {
        return;
    };
    let (input, mut events) = unbounded_channel();
    SHELLS.with(|all| all.borrow_mut().insert(key.clone(), input));
    let (sink, mut output) = unbounded_channel();
    bench().update(|b| {
        if let Some(s) = b.terminal.sessions.iter_mut().find(|s| s.id == id) {
            s.running = true;
        }
    });
    spawn_for_window(async move {
        let job = background(bridge.terminal_run(id.clone(), String::new(), sink));
        let mut ready = false;
        let mut pending = Vec::new();
        loop {
            let event = tokio::select! {
                event=events.recv()=>event,
                chunk=output.recv()=>{
                    let Some(chunk)=chunk else{break};
                    bench().update(|b|b.terminal.on_output(&id,&chunk)); ready=true;
                    None
                }
            };
            if let Some(event) = event {
                pending.push(event);
            }
            if ready {
                for event in pending.drain(..) {
                    let result = match event {
                        Input::Data(s) => call(bridge.terminal_write(id.clone(), s)).await,
                        Input::Size(rows, cols) => {
                            call(bridge.terminal_resize(id.clone(), rows, cols)).await
                        }
                        Input::Close => call(bridge.terminal_interrupt(id.clone())).await,
                    };
                    if let Err(e) = result {
                        bench().update(|b| {
                            b.terminal.on_output(&id, &format!("\r\n{}\r\n", e.message))
                        });
                    }
                }
            }
        }
        let result = job
            .await
            .map_err(|e| e.to_string())
            .and_then(|r| r.map_err(|e| e.message));
        SHELLS.with(|all| all.borrow_mut().remove(&key));
        bench().update(|b| b.terminal.on_exit(&id, result));
    });
}
fn send(id: &str, event: Input) -> bool {
    SHELLS.with(|all| {
        all.borrow()
            .get(&format!("{}\x1f{id}", current_machine().unwrap_or_default()))
            .is_some_and(|tx| tx.send(event).is_ok())
    })
}
pub(crate) fn shell_write(id: &str, data: String) {
    send(id, Input::Data(data));
}
pub(crate) fn shell_resize(id: &str, rows: u16, cols: u16) {
    send(id, Input::Size(rows, cols));
}
pub(crate) fn shell_close(id: &str) -> bool {
    send(id, Input::Close)
}

/// Launch a real PTY terminal tab for a `vantry_terminal_run` request, stream
/// output and accept keyboard input, and automatically submit the process exit
/// code back to Core when the command exits.
pub(crate) fn start_terminal_task(
    request_id: String,
    title: String,
    command: String,
    cwd: String,
) {
    let dir = if !cwd.trim().is_empty() {
        cwd.trim().to_owned()
    } else {
        let root = bench().with_untracked(|b| b.root.clone());
        if !root.is_empty() {
            root
        } else {
            std::env::var("HOME").unwrap_or_else(|_| "/".into())
        }
    };
    let tab_title = if title.trim().is_empty() {
        command.clone()
    } else {
        title
    };
    let mut session_id = String::new();
    bench().update(|b| session_id = b.terminal.open_task(&request_id, &tab_title, &dir));
    // Open the Terminal dock tab and defer the approval modal so the user
    // interacts directly with the live PTY terminal.
    ui().update(|u| {
        u.dock_open = true;
        u.child_panel = None;
    });
    super::select_dock_tab(DockTab::Terminal);
    super::defer_request(request_id.clone());

    if SHELLS.with(|all| all.borrow().contains_key(&session_id)) {
        return;
    }

    let bridge: Arc<dyn WorkbenchBridge> = if bridges(|b| b.mock) {
        let Some(wb) = workbench() else { return };
        wb
    } else {
        Arc::new(LocalWorkbench::new(PathBuf::from(&dir), None))
    };

    let (input, mut events) = unbounded_channel();
    SHELLS.with(|all| all.borrow_mut().insert(session_id.clone(), input));
    let (sink, mut output) = unbounded_channel();
    bench().update(|b| {
        if let Some(s) = b.terminal.sessions.iter_mut().find(|s| s.id == session_id) {
            s.running = true;
        }
        b.terminal.on_output(&session_id, &format!("$ {command}\r\n"));
    });

    spawn_for_window(async move {
        let job = background(bridge.terminal_run(session_id.clone(), command, sink));
        let mut ready = false;
        let mut pending = Vec::new();
        loop {
            let event = tokio::select! {
                event = events.recv() => event,
                chunk = output.recv() => {
                    let Some(chunk) = chunk else { break };
                    bench().update(|b| b.terminal.on_output(&session_id, &chunk));
                    ready = true;
                    None
                }
            };
            if let Some(event) = event {
                pending.push(event);
            }
            if ready {
                for event in pending.drain(..) {
                    let result = match event {
                        Input::Data(s) => call(bridge.terminal_write(session_id.clone(), s)).await,
                        Input::Size(rows, cols) => {
                            call(bridge.terminal_resize(session_id.clone(), rows, cols)).await
                        }
                        Input::Close => call(bridge.terminal_interrupt(session_id.clone())).await,
                    };
                    if let Err(e) = result {
                        bench().update(|b| {
                            b.terminal
                                .on_output(&session_id, &format!("\r\n{}\r\n", e.message))
                        });
                    }
                }
            }
        }
        let result = job
            .await
            .map_err(|e| e.to_string())
            .and_then(|r| r.map_err(|e| e.message));
        SHELLS.with(|all| all.borrow_mut().remove(&session_id));
        match &result {
            Ok(code) => {
                bench().update(|b| {
                    b.terminal.on_output(
                        &session_id,
                        &format!("\r\n[交互命令已退出，退出码 {code}，正在继续任务…]\r\n"),
                    );
                    b.terminal.on_exit(&session_id, Ok(*code));
                });
                let still_pending = state().with_untracked(|s| {
                    s.requests
                        .iter()
                        .any(|r| r.id == request_id && r.status == "pending")
                });
                if still_pending {
                    let payload = serde_json::json!({
                        "status": "exited",
                        "exitCode": code,
                        "cwd": dir,
                    })
                    .to_string();
                    super::respond(request_id, ApprovalDecision::Submit(payload));
                }
            }
            Err(err) => {
                bench().update(|b| {
                    b.terminal
                        .on_output(&session_id, &format!("\r\n[启动终端任务失败：{err}]\r\n"));
                    b.terminal.on_exit(&session_id, Err(err.clone()));
                });
            }
        }
    });
}

pub(crate) fn complete_terminal_task(request_id: String, cwd: String) {
    let payload = serde_json::json!({
        "status": "completed",
        "cwd": cwd,
        "note": "User marked interactive terminal step completed.",
    })
    .to_string();
    super::respond(request_id, ApprovalDecision::Submit(payload));
}

pub(crate) fn cancel_terminal_task(request_id: String) {
    let session_id = format!("task-{request_id}");
    shell_close(&session_id);
    super::respond(request_id, ApprovalDecision::Cancel);
}
