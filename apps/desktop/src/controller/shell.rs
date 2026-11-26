//! A shell belongs to its session until it exits or its tab is closed.
use super::*;
use std::collections::HashMap;
use tokio::sync::mpsc::{UnboundedSender, unbounded_channel};

pub(super) enum Input {
    Data(String),
    Size(u16, u16),
    Interrupt,
    Close,
}
thread_local! { static SHELLS: RefCell<HashMap<String,UnboundedSender<Input>>> = RefCell::new(HashMap::new()); }

pub(crate) fn native_shell() -> bool {
    !bridges(|b| b.mock) && (cfg!(unix) || current_machine().is_some() || remote_project())
}

fn terminal_peer<'a>(
    owner: Option<&'a str>,
    environment: Option<&'a str>,
    thread: Option<&str>,
) -> Option<(&'a str, String)> {
    let host = environment.filter(|id| !id.is_empty()).or(owner)?;
    let id = match thread {
        Some(id) if Some(host) != owner => vantry_protocol::federated_thread_id(id),
        _ => thread.unwrap_or_default().into(),
    };
    Some((host, id))
}

/// Resolve the checkout and machine from the originating thread.
fn terminal_workbench(
    thread: Option<&vantry_protocol::Thread>,
    cwd: &str,
) -> Result<(Arc<dyn WorkbenchBridge>, String), String> {
    let workspace = match thread {
        Some(t) => t.workspace.clone(),
        None => state().with_untracked(|s| s.new_thread.workspace.clone()),
    };
    let root = workspace
        .as_ref()
        .and_then(|w| [&w.path, &w.repository].into_iter().find(|p| !p.is_empty()))
        .or_else(|| thread.map(|t| &t.project_path).filter(|p| !p.is_empty()))
        .cloned()
        .unwrap_or_default();
    let current = current_machine();
    let peer = terminal_peer(
        current.as_deref(),
        workspace.as_ref().map(|w| w.environment_id.as_str()),
        thread.map(|t| t.id.as_str()),
    );
    let default = workbench().ok_or("工作台尚未就绪")?;
    if bridges(|b| b.mock) {
        return Ok((default, root));
    }
    if let Some((peer, remote_thread)) = peer {
        let same = current.as_deref() == Some(peer);
        let mut socket = None;
        if same {
            socket = bridges(|b| b.core_socket.clone());
        } else {
            machines::run_on(peer, || socket = bridges(|b| b.core_socket.clone()));
        }
        let socket = socket.ok_or_else(|| format!("配对主机 {peer} 的工作台不可用"))?;
        let scoped = CoreWorkbench::for_core_workspace(
            socket,
            root.clone(),
            LocalWorkbench::new(PathBuf::from(&root), None),
            None,
        )
        .terminal_scope(remote_thread, cwd.into());
        let dir = if cwd.is_empty() { root } else { cwd.into() };
        return Ok((Arc::new(scoped), dir));
    }
    let root = if root.is_empty() {
        default.root()
    } else {
        root
    };
    let dir = PathBuf::from(root).join(cwd);
    Ok((
        Arc::new(LocalWorkbench::new(dir.clone(), None)),
        dir.display().to_string(),
    ))
}

/// The registry key of session `id` on the shown machine: what writes,
/// resizes and interrupts look the shell up by.
fn shell_key(id: &str) -> String {
    format!("{}\x1f{id}", current_machine().unwrap_or_default())
}

pub(crate) fn shell_start(id: String) {
    let key = shell_key(&id);
    if id.starts_with("task-") || SHELLS.with(|all| all.borrow().contains_key(&key)) {
        return;
    }
    let thread = state().with_untracked(|s| s.selected_thread().cloned());
    let bridge = match terminal_workbench(thread.as_ref(), "") {
        Ok((bridge, _)) => bridge,
        Err(error) => {
            bench().update(|b| b.terminal.on_exit(&id, Err(error)));
            return;
        }
    };
    let (input, mut events) = unbounded_channel();
    SHELLS.with(|all| all.borrow_mut().insert(key.clone(), input));
    let (sink, mut output) = unbounded_channel();
    let thread = state().with_untracked(|s| s.selected.clone());
    bench().update(|b| {
        if let Some(s) = b.terminal.sessions.iter_mut().find(|s| s.id == id) {
            s.shell_scope = thread.map(|id| (id, b.root.clone()));
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
                        Input::Interrupt => call(bridge.terminal_interrupt(id.clone())).await,
                        Input::Close => call(bridge.terminal_close(id.clone())).await,
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
pub(super) fn send(id: &str, event: Input) -> bool {
    SHELLS.with(|all| {
        all.borrow()
            .get(&shell_key(id))
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
pub(crate) fn start_terminal_task(request_id: String, title: String, command: String, cwd: String) {
    let thread = state().with_untracked(|s| {
        let request = s.requests.iter().find(|r| r.id == request_id)?;
        s.thread(&request.thread_id).cloned()
    });
    let Some(thread) = thread else {
        warn("找不到发起终端请求的会话");
        return;
    };
    let (bridge, dir) = match terminal_workbench(Some(&thread), cwd.trim()) {
        Ok(target) => target,
        Err(error) => {
            warn(error);
            return;
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

    // Registered under the same key the terminal's keystrokes and resizes
    // look it up by, or they never reach the PTY.
    let key = shell_key(&session_id);
    if SHELLS.with(|all| all.borrow().contains_key(&key)) {
        return;
    }

    let (input, mut events) = unbounded_channel();
    SHELLS.with(|all| all.borrow_mut().insert(key.clone(), input));
    let (sink, mut output) = unbounded_channel();
    bench().update(|b| {
        if let Some(s) = b.terminal.sessions.iter_mut().find(|s| s.id == session_id) {
            s.running = true;
        }
        b.terminal
            .on_output(&session_id, &format!("$ {command}\r\n"));
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
                        Input::Interrupt => {
                            call(bridge.terminal_interrupt(session_id.clone())).await
                        }
                        Input::Close => call(bridge.terminal_close(session_id.clone())).await,
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
        SHELLS.with(|all| all.borrow_mut().remove(&key));
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

/// Submit to the selected thread's interactive shell. shell_start queues early
/// input until the first PTY output, including when the dock was just opened.
pub(crate) fn run_in_terminal(command: String) {
    let Some(thread) = state().with_untracked(|s| s.selected.clone()) else { return };
    if !native_shell() || bench().with_untracked(|b| b.root.is_empty()) {
        warn("此会话的工作区交互终端不可用");
        return;
    }
    let mut id = String::new();
    bench().update(|b| id = b.terminal.select_shell(&(thread, b.root.clone())));
    super::select_dock_tab(DockTab::Terminal);
    shell_start(id.clone());
    shell_write(&id, terminal_input(&command));
}

fn terminal_input(command: &str) -> String {
    if !command.contains(['\n', '\t']) {
        return format!("{command}\r");
    }
    // Submit one physical line so foreground commands cannot consume later
    // lines and the interactive editor cannot treat a literal tab as completion.
    // eval runs in the existing shell, keeping changes such as cd and exports.
    let quoted = command
        .replace('\\', "\\\\")
        .replace('\'', "'\\''")
        .replace('\n', "\\n")
        .replace('\t', "\\t");
    format!("eval \"$(printf '%b' '{quoted}')\"\r")
}

#[cfg(test)]
mod tests {
    use super::{terminal_input, terminal_peer};

    #[test]
    fn terminal_command_submits_multiline_as_one_shell_input() {
        assert_eq!(terminal_input("pwd"), "pwd\r");
        let script = "cat <<'END'\n\t$literal \\n\nEND\nprintf '%s' \"$((1+2))\"";
        let input = terminal_input(script);
        assert!(!input.contains(['\n', '\t']));
        #[cfg(unix)]
        {
            let output = std::process::Command::new("/bin/sh")
                .args(["-c", input.strip_suffix('\r').unwrap()])
                .output()
                .unwrap();
            assert!(output.status.success());
            assert_eq!(output.stdout, b"\t$literal \\n\n3");
        }
    }

    #[test]
    fn terminal_machine_and_thread_follow_the_request_owner() {
        assert_eq!(terminal_peer(None, Some(""), Some("chat")), None);
        assert_eq!(
            terminal_peer(Some("mac"), None, Some("chat")),
            Some(("mac", "chat".into()))
        );
        assert_eq!(
            terminal_peer(None, Some("linux"), Some("child")),
            Some(("linux", "remote_child".into()))
        );
        assert_eq!(
            terminal_peer(Some("mac"), Some("linux"), Some("child")),
            Some(("linux", "remote_child".into()))
        );
        assert_eq!(
            terminal_peer(None, Some("linux"), None),
            Some(("linux", String::new()))
        );
    }
}
