//! End-to-end integration tests verifying parity gaps closed against Go v0.1.42:
//! 1. Workbench actions: orchestration.environments, skills.list, files.search,
//!    git.log, git.commit_detail, git.compare, git.checkpoints.*, git.worktree.status.
//! 2. Cross-machine federated subtask delegation (Core A <-> paired Core B),
//!    including remote execution, result delivery back to A, and Core A restart persistence.

use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use std::{
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};
use vantry_client::CoreClient;
use vantry_protocol::*;

struct ChildGuard(Option<Child>);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        if let Some(mut c) = self.0.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

fn bin() -> PathBuf {
    PathBuf::from(env!("CARGO_BIN_EXE_vantry-service"))
}

fn run_git(dir: &Path, args: &[&str]) -> Result<()> {
    let out = Command::new("git")
        .current_dir(dir)
        .env("GIT_AUTHOR_NAME", "Vantry Test")
        .env("GIT_AUTHOR_EMAIL", "test@vantry.local")
        .env("GIT_COMMITTER_NAME", "Vantry Test")
        .env("GIT_COMMITTER_EMAIL", "test@vantry.local")
        .args(args)
        .output()?;
    ensure!(
        out.status.success(),
        "git {:?} failed: {}",
        args,
        String::from_utf8_lossy(&out.stderr)
    );
    Ok(())
}

fn init_sample_repo(dir: &Path) -> Result<()> {
    std::fs::create_dir_all(dir)?;
    run_git(dir, &["init", "-b", "main"])?;
    // The service runs its own Git process when the test requests a checkpoint.
    run_git(dir, &["config", "user.name", "Vantry Test"])?;
    run_git(dir, &["config", "user.email", "test@vantry.local"])?;
    std::fs::write(dir.join("README.md"), "# Sample\n")?;
    std::fs::create_dir_all(dir.join("src"))?;
    std::fs::write(dir.join("src/app.rs"), "fn main() {}\n")?;
    run_git(dir, &["add", "-A"])?;
    run_git(dir, &["commit", "-m", "initial commit"])?;
    run_git(dir, &["checkout", "-b", "feature"])?;
    std::fs::write(dir.join("src/feature.rs"), "pub fn feat() -> i32 { 42 }\n")?;
    run_git(dir, &["add", "-A"])?;
    run_git(dir, &["commit", "-m", "add feature"])?;
    run_git(dir, &["checkout", "main"])?;

    // Add a project-local skill so skills.list discovers it.
    let skill_dir = dir.join(".pi/skills/sample-skill");
    std::fs::create_dir_all(&skill_dir)?;
    std::fs::write(
        skill_dir.join("SKILL.md"),
        "---\nname: sample-skill\ndescription: A sample skill for parity test\n---\nBody\n",
    )?;
    Ok(())
}

async fn wait_health(client: &CoreClient) -> Result<()> {
    let deadline = Instant::now() + Duration::from_secs(15);
    while Instant::now() < deadline {
        if client.health().await.is_ok() {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(80)).await;
    }
    anyhow::bail!("Core did not become healthy in time")
}

fn spawn_core(data: &Path, workspace: &Path, listen_remote: Option<&str>) -> Result<ChildGuard> {
    let agent = data.with_extension("pi-agent");
    std::fs::create_dir_all(&agent)?;
    let root = vantry_pi_runtime::mcp::project_trust_path(workspace);
    std::fs::write(agent.join("trust.json"), serde_json::to_vec(&json!({(root.display().to_string()): true}))?)?;
    let mut cmd = Command::new(bin());
    cmd.env("PI_CODING_AGENT_DIR", agent).arg("core")
        .arg("--data-dir")
        .arg(data)
        .arg("--provider")
        .arg("controlled")
        .arg("--controlled-delay")
        .arg("10ms")
        .arg("--workspace")
        .arg(workspace)
        .env("VANTRY_ALLOW_INSECURE_LAN_RELAY", "1")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    if let Some(addr) = listen_remote {
        cmd.arg("--listen-remote").arg(addr);
    }
    let child = cmd.spawn().context("spawn vantry-service core")?;
    Ok(ChildGuard(Some(child)))
}

#[tokio::test]
async fn workbench_parity_git_files_skills_and_environments() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let repo = tmp.path().join("repo");
    let data = tmp.path().join("data");
    init_sample_repo(&repo)?;

    let _core = spawn_core(&data, &repo, None)?;
    let client = CoreClient::from_data_dir(&data);
    wait_health(&client).await?;

    // 1. orchestration.environments
    let envs = client
        .workbench(&WorkbenchRequest {
            action: "orchestration.environments".into(),
            ..Default::default()
        })
        .await?;
    let envs_json: Value = serde_json::from_str(&envs.output)?;
    assert!(envs_json["environments"].is_array());

    // 2. skills.list
    let skills = client
        .workbench(&WorkbenchRequest {
            action: "skills.list".into(),
            workspace: repo.display().to_string(),
            ..Default::default()
        })
        .await?;
    let skills_json: Value = serde_json::from_str(&skills.output)?;
    assert!(
        skills_json["skills"]
            .as_array()
            .unwrap()
            .iter()
            .any(|s| s["name"] == "sample-skill"),
        "expected sample-skill in {}",
        skills.output
    );

    // 3. files.search
    let search = client
        .workbench(&WorkbenchRequest {
            action: "files.search".into(),
            workspace: repo.display().to_string(),
            message: "app".into(),
            ..Default::default()
        })
        .await?;
    let search_json: Value = serde_json::from_str(&search.output)?;
    assert!(
        search_json["results"]
            .as_array()
            .unwrap()
            .iter()
            .any(|r| r["path"] == "src/app.rs"),
        "expected src/app.rs in {}",
        search.output
    );

    // 4. git.log
    let log = client
        .workbench(&WorkbenchRequest {
            action: "git.log".into(),
            workspace: repo.display().to_string(),
            branch: "*".into(),
            ..Default::default()
        })
        .await?;
    let commits = log.git.context("missing git state")?.commits;
    assert!(commits.len() >= 2);
    let head_id = commits[0].id.clone();

    // 5. git.commit_detail
    let detail = client
        .workbench(&WorkbenchRequest {
            action: "git.commit_detail".into(),
            workspace: repo.display().to_string(),
            name: head_id,
            ..Default::default()
        })
        .await?;
    let detail_json: Value = serde_json::from_str(&detail.output)?;
    assert!(!detail_json["commit"]["id"].as_str().unwrap().is_empty());

    // 6. git.compare
    let cmp = client
        .workbench(&WorkbenchRequest {
            action: "git.compare".into(),
            workspace: repo.display().to_string(),
            branch: "main".into(),
            name: "feature".into(),
            ..Default::default()
        })
        .await?;
    let cmp_json: Value = serde_json::from_str(&cmp.output)?;
    assert!(
        cmp_json["files"]
            .as_array()
            .unwrap()
            .iter()
            .any(|f| f["path"] == "src/feature.rs")
    );

    // 7. git.checkpoints.capture + list + diff
    std::fs::write(repo.join("src/app.rs"), "fn main() { println!(\"hi\"); }\n")?;
    let cap = client
        .workbench(&WorkbenchRequest {
            action: "git.checkpoints.capture".into(),
            workspace: repo.display().to_string(),
            name: "cp-test".into(),
            message: "before edit".into(),
            ..Default::default()
        })
        .await?;
    let cap_json: Value = serde_json::from_str(&cap.output)?;
    assert_eq!(cap_json["checkpoint"]["id"], "cp-test");

    let list = client
        .workbench(&WorkbenchRequest {
            action: "git.checkpoints.list".into(),
            workspace: repo.display().to_string(),
            ..Default::default()
        })
        .await?;
    let list_json: Value = serde_json::from_str(&list.output)?;
    assert!(
        list_json["checkpoints"]
            .as_array()
            .unwrap()
            .iter()
            .any(|c| c["id"] == "cp-test")
    );

    // 8. git.worktree.status
    let wt = client
        .workbench(&WorkbenchRequest {
            action: "git.worktree.status".into(),
            workspace: repo.display().to_string(),
            ..Default::default()
        })
        .await?;
    let wt_json: Value = serde_json::from_str(&wt.output)?;
    assert_eq!(wt_json["exists"], true);
    assert_eq!(wt_json["isWorktree"], false);

    // Ensure child process exits before tmp drops.
    drop(_core);
    Ok(())
}

#[tokio::test]
async fn paired_cross_machine_subtask_delegation_and_restart() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let repo_a = tmp.path().join("repo-a");
    let repo_b = tmp.path().join("repo-b");
    let data_a = tmp.path().join("data-a");
    let data_b = tmp.path().join("data-b");
    init_sample_repo(&repo_a)?;
    init_sample_repo(&repo_b)?;

    let core_b = spawn_core(&data_b, &repo_b, Some("127.0.0.1:0"))?;
    let mut core_a = spawn_core(&data_a, &repo_a, None)?;
    let client_b = CoreClient::from_data_dir(&data_b);
    let client_a = CoreClient::from_data_dir(&data_a);
    wait_health(&client_b).await?;
    wait_health(&client_a).await?;

    // Pair A with B.
    let invite = client_b
        .workbench(&WorkbenchRequest {
            action: "remote.pair.create".into(),
            ..Default::default()
        })
        .await?;
    let link = serde_json::from_str::<Value>(&invite.output)?["link"]
        .as_str()
        .context("missing pairing link")?
        .to_owned();
    let redeemed = client_a
        .workbench(&WorkbenchRequest {
            action: "remote.pair.redeem".into(),
            name: "Peer B".into(),
            input: Some(json!({"link": link})),
            ..Default::default()
        })
        .await?;
    let peer_id = serde_json::from_str::<Value>(&redeemed.output)?["id"]
        .as_str()
        .context("missing peer ID")?
        .to_owned();

    // Read the target checkout and an image attachment through the desktop's paired proxy.
    let proxy_socket = data_a.join("proxy.sock");
    let proxy = ChildGuard(Some(Command::new(bin())
        .arg("peer-proxy").arg("--data-dir").arg(&data_a)
        .arg("--peer").arg(&peer_id).arg("--socket").arg(&proxy_socket).spawn()?));
    let remote = CoreClient::new(&proxy_socket);
    wait_health(&remote).await?;
    // The released desktop checks every identifier before it calls environment.info.
    let status: Value = remote.get_json("/v1/status").await?;
    let caps = status["capabilities"].as_array().context("missing capabilities")?;
    for required in ["status.v1", "threads.summaries.v1", "threads.detail.v1",
        "orchestration.inter-thread.v1", "workbench.v3", "plugins.native-pages",
        "remote-access.alma-relay", "remote-access.native.v1", "remote-access.direct-discovery",
        "environments.sessions", "orchestration.environments.v1", "workbench.skills.v1",
        "provider.pi.scripted-tools.v1", "remote-desktop.permissions.v1"] {
        ensure!(caps.iter().any(|cap| cap == required), "missing {required}");
    }
    let environment = remote.workbench(&WorkbenchRequest {
        action: "environment.info".into(), ..Default::default()
    }).await?;
    let environment_b = serde_json::from_str::<EnvironmentInfo>(&environment.output)?.id;
    assert_eq!(environment_b, std::fs::read_to_string(data_b.join("environment-id"))?.trim());
    assert_ne!(environment_b, peer_id);
    std::fs::write(repo_b.join("remote.md"), "# Only on B")?;
    let file = remote.workbench(&WorkbenchRequest {
        action: "files.read".into(), workspace: repo_b.display().to_string(),
        paths: vec!["remote.md".into()], ..Default::default()
    }).await?;
    assert_eq!(file.output, "# Only on B");
    #[cfg(unix)]
    {
        use base64::{Engine, engine::general_purpose::STANDARD};
        let opened = remote.workbench(&WorkbenchRequest {
            action: "terminal.session.open".into(), workspace: repo_b.display().to_string(),
            input: Some(json!({"command":"cat remote.md; exit 7"})), ..Default::default()
        }).await?;
        let opened: Value = serde_json::from_str(&opened.output)?;
        let mut bytes = Vec::new();
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            ensure!(Instant::now() < deadline, "paired terminal did not exit");
            let read = remote.workbench(&WorkbenchRequest {
                action: "terminal.session.read".into(),
                input: Some(json!({"sessionId": opened["sessionId"], "offset": bytes.len()})),
                ..Default::default()
            }).await?;
            let chunk: Value = serde_json::from_str(&read.output)?;
            bytes.extend(STANDARD.decode(chunk["data"].as_str().context("missing terminal data")?)?);
            if chunk["exited"] == true {
                assert_eq!(chunk["exitCode"], 7);
                break;
            }
        }
        assert_eq!(bytes, b"# Only on B");
    }
    let png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jOscAAAAASUVORK5CYII=";
    let image = client_b.workbench(&WorkbenchRequest {
        action: "attachment.import".into(), name: "dot.png".into(), data: png.into(),
        ..Default::default()
    }).await?.attachment.context("missing image attachment")?;
    assert_eq!(image.mime, "image/png");
    let preview = remote.workbench(&WorkbenchRequest {
        action: "attachment.read".into(), input: Some(serde_json::to_value(image)?),
        ..Default::default()
    }).await?;
    assert_eq!(preview.data, png);
    drop(proxy);

    // Verify orchestration.environments sees Peer B.
    let envs = client_a
        .workbench(&WorkbenchRequest {
            action: "orchestration.environments".into(),
            ..Default::default()
        })
        .await?;
    assert!(envs.output.contains(&peer_id));

    // Create parent thread and initial completed run on A.
    let created = client_a
        .dispatch(&vantry_protocol::Command {
            version: 1,
            command_id: "cmd-create-parent".into(),
            kind: CommandKind::CreateThread,
            title: "Parent on A".into(),
            provider_id: "controlled".into(),
            ..Default::default()
        })
        .await?;
    let sent = client_a
        .dispatch(&vantry_protocol::Command {
            version: 1,
            command_id: "cmd-send-parent".into(),
            kind: CommandKind::SendMessage,
            thread_id: created.thread_id.clone(),
            message: "Start parent".into(),
            ..Default::default()
        })
        .await?;

    // Wait for parent run to finish.
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        let detail = client_a.thread_detail(&created.thread_id, 10).await?;
        let done = detail
            .runs
            .iter()
            .flatten()
            .any(|r| r.id == sent.run_id && r.status == RunStatus::Completed);
        if done {
            break;
        }
        ensure!(Instant::now() < deadline, "parent run timed out");
        tokio::time::sleep(Duration::from_millis(50)).await;
    }

    // Delegate subtask from A's parent run to paired Core B!
    let delegated: Value = client_a
        .post_json(
            "/v1/orchestration/delegate",
            &json!({
                "callerThreadId": created.thread_id,
                "parentRunId": sent.run_id,
                "environmentId": peer_id,
                "title": "Remote child on B",
                "task": "Hello remote B",
                "mode": "wait",
                "timeoutMs": 15000,
                "clientRequestId": "req-remote-1",
            }),
        )
        .await?;
    assert_eq!(delegated["status"], "completed", "delegated={delegated}");
    assert_eq!(delegated["environmentId"], peer_id);
    let task_id = delegated["taskId"].as_str().unwrap().to_owned();
    let remote_thread_id = delegated["remoteThreadId"].as_str().unwrap().to_owned();
    assert!(!remote_thread_id.is_empty());

    let url = format!("{THREAD_SCHEME}{remote_thread_id}?environmentId={environment_b}");
    let copied = thread_markdown("Remote [B]", &url);
    for client in [&client_a, &client_b] {
        for input in [&url, &copied] {
            let read: Value = client.post_json("/v1/orchestration/threads/read", &json!({"threadId": input})).await?;
            assert_eq!(read["thread"]["threadId"], remote_thread_id);
            let returned = ThreadLink::parse(read["thread"]["link"].as_str().unwrap()).unwrap();
            assert_eq!(returned.url, url);
        }
    }

    // Confirm Core B actually executed the remote thread.
    let snap_b = client_b.snapshot().await?;
    let b_runs: Vec<_> = snap_b
        .runs
        .iter()
        .flatten()
        .filter(|r| r.thread_id == remote_thread_id)
        .collect();
    assert_eq!(b_runs.len(), 1);
    assert_eq!(b_runs[0].status, RunStatus::Completed);

    // Restart Core A and verify mapping persists without re-dispatching on B.
    drop(core_a);
    core_a = spawn_core(&data_a, &repo_a, None)?;
    wait_health(&client_a).await?;
    let status_after: Value = client_a
        .post_json(
            "/v1/orchestration/tasks/status",
            &json!({"taskId": task_id}),
        )
        .await?;
    assert_eq!(
        status_after["tasks"][0]["remoteThreadId"],
        remote_thread_id
    );
    let snap_b_after = client_b.snapshot().await?;
    assert_eq!(
        snap_b_after
            .runs
            .iter()
            .flatten()
            .filter(|r| r.thread_id == remote_thread_id)
            .count(),
        1,
        "Core A restart must not re-dispatch settled remote run"
    );

    drop(core_a);
    drop(core_b);
    Ok(())
}
