//! The official Qoder plugin end to end, against a stand-in for the community
//! plugin (no Qoder account or network): install from the plugin list, browser
//! sign-in, account models and usage, Pi's model catalog, and a Pi run whose
//! tool call Pi executes locally before the model answers from its result.
//! Runs using the service binary alone.

use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use std::{
    path::Path,
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};
use vantry_client::CoreClient;
use vantry_protocol::*;

const FAKE_PLUGIN: &str = r#"
import { appendFileSync } from "node:fs"
const LOG = process.env.FAKE_QODER_LOG
const sse = (chunks) => new Response(chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n", { headers: { "Content-Type": "text/event-stream" } })
export const QoderAuthPlugin = async ({ client }) => ({
  auth: {
    provider: "qoder",
    refreshLead: 600000,
    async refresh() { return undefined },
    methods: [{
      type: "oauth",
      label: "Sign in with Qoder",
      authorize: async () => ({
        url: "https://qoder.com/device/selectAccounts?fake=1",
        instructions: "authorize",
        method: "auto",
        async callback() {
          await new Promise((r) => setTimeout(r, 300))
          return { type: "success", access: "jt-fake", refresh: "rt-fake", expires: Date.now() + 3600e3, uid: "u-1", email: "dev@example.com", accountId: "dev@example.com" }
        },
      }),
    }],
    async usage(getAuth) {
      const a = await getAuth()
      if (!a) return { error: "not signed in" }
      return { plan: "Pro", windows: [{ name: "Credits", used: 25, display: "500 / 2,000 credits" }], signIn: "kept" }
    },
    async loader(getAuth) {
      if (!(await getAuth())) return {}
      return {
        async fetch(url, init) {
          const chat = JSON.parse(init.body)
          if (LOG) appendFileSync(LOG, JSON.stringify({ model: chat.model, roles: chat.messages.map((m) => m.role), tools: (chat.tools ?? []).map((t) => t.function?.name) }) + "\n")
          const result = chat.messages.find((m) => m.role === "tool")
          if (!result) {
            const path = chat.messages.map((m) => (typeof m.content === "string" ? m.content : (m.content ?? []).map((p) => p.text ?? "").join(""))).join("\n").match(/READ:(\S+)/)?.[1]
            return sse([
              { id: "c1", object: "chat.completion.chunk", model: chat.model, choices: [{ index: 0, delta: { role: "assistant", tool_calls: [{ index: 0, id: "call_1", type: "function", function: { name: "read", arguments: JSON.stringify({ path }) } }] }, finish_reason: null }] },
              { id: "c1", object: "chat.completion.chunk", model: chat.model, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } },
            ])
          }
          const text = typeof result.content === "string" ? result.content : result.content.map((p) => p.text ?? "").join("")
          const secret = text.match(/SECRET-[0-9a-f]+/)?.[0] ?? "missing"
          return sse([
            { id: "c2", object: "chat.completion.chunk", model: chat.model, choices: [{ index: 0, delta: { role: "assistant", content: `The file says ${secret}.` }, finish_reason: null }] },
            { id: "c2", object: "chat.completion.chunk", model: chat.model, choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 20, completion_tokens: 6, total_tokens: 26 } },
          ])
        },
      }
    },
  },
  provider: {
    id: "qoder",
    async models(provider, { auth } = {}) {
      if (auth?.type !== "oauth") return provider.models
      return { "fake-coder": { id: "fake-coder", name: "Fake Coder", limit: { context: 200000 }, capabilities: { reasoning: false, input: { image: false } }, variants: {}, free: false, rate: 1 } }
    },
  },
})
"#;

struct ChildGuard(Option<Child>);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        if let Some(mut c) = self.0.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

async fn workbench(
    client: &CoreClient,
    action: &str,
    plugin: &str,
    input: Option<Value>,
) -> Result<WorkbenchResponse> {
    client
        .workbench(&WorkbenchRequest {
            action: action.into(),
            plugin_id: plugin.into(),
            name: "main".into(),
            input,
            ..Default::default()
        })
        .await
        .with_context(|| format!("workbench {action}"))
}

fn panel(output: &str) -> Value {
    serde_json::from_str(output).unwrap_or(Value::Null)
}

#[tokio::test(flavor = "multi_thread")]
async fn qoder_plugin_signs_in_and_serves_pi_with_tools() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let data = tmp.path().join("data");
    let agent = tmp.path().join("pi-agent");
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(&agent)?;
    std::fs::create_dir_all(&ws)?;
    let secret = format!("SECRET-{}", uuid::Uuid::new_v4().simple());
    std::fs::write(ws.join("note.txt"), format!("{secret}\n"))?;
    let root = vantry_pi_runtime::mcp::project_trust_path(&ws);
    std::fs::write(
        agent.join("trust.json"),
        serde_json::to_vec(&json!({(root.display().to_string()): true}))?,
    )?;
    // The stand-in plugin, as an already-downloaded version newer than npm's.
    let version = data.join("model-plugins/qoder/versions/99.0.0");
    std::fs::create_dir_all(&version)?;
    std::fs::write(version.join("opencode.mjs"), FAKE_PLUGIN)?;
    std::fs::write(version.join("index.mjs"), "")?;
    std::fs::write(
        data.join("model-plugins/qoder/state.json"),
        serde_json::to_vec(&json!({"current": "99.0.0"}))?,
    )?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        for dir in [
            &data,
            &data.join("model-plugins"),
            &data.join("model-plugins/qoder"),
            &data.join("model-plugins/qoder/versions"),
            &version,
        ] {
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
    }
    // Magpie owns one selection, the second model comes from the imported plugin.
    vantry_pi_runtime::save_setup(
        &agent,
        &PiSetupRequest {
            provider_id: "magpie".into(),
            api_key: "fixture-token".into(),
            base_url: "http://127.0.0.1:1/v1".into(),
            models: vec!["picked".into(), "unpicked".into()],
            set_default: true,
            ..Default::default()
        },
    )?;
    let magpie = tmp.path().join("magpie.json");
    std::fs::write(&magpie, br#"{"pickedModels":{"pi":["picked"]}}"#)?;
    let log = tmp.path().join("requests.jsonl");

    let _core = ChildGuard(Some(
        Command::new(env!("CARGO_BIN_EXE_vantry-service"))
            .env("PI_CODING_AGENT_DIR", &agent)
            .env("FAKE_QODER_LOG", &log)
            .arg("core")
            .arg("--magpie-settings")
            .arg(&magpie)
            .args(["--provider", "pi", "--data-dir"])
            .arg(&data)
            .arg("--workspace")
            .arg(&ws)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()?,
    ));
    let client = CoreClient::from_data_dir(&data);
    let deadline = Instant::now() + Duration::from_secs(20);
    while client.health().await.is_err() {
        ensure!(Instant::now() < deadline, "Core did not start");
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let result = run(&client, &ws, &secret, &log, &agent).await;
    // Stop the detached host the test started.
    if let Ok(host) = std::fs::read(data.join("model-plugins/qoder/host.json")) {
        let host: Value = serde_json::from_slice(&host)?;
        if let Some(pid) = host["pid"].as_u64() {
            let _ = Command::new("kill").arg(pid.to_string()).status();
        }
    }
    result
}

async fn run(client: &CoreClient, ws: &Path, secret: &str, log: &Path, agent: &Path) -> Result<()> {
    // Offered before it is installed.
    let list = workbench(client, "plugins.list", "", None).await?;
    let offered = list
        .plugins
        .iter()
        .find(|p| p.id == "qoder")
        .context("qoder not offered")?;
    ensure!(
        offered.installable && offered.version.is_empty(),
        "qoder should be installable: {offered:?}"
    );

    let installed = workbench(client, "plugins.install", "qoder", None).await?;
    let entry = installed
        .plugins
        .iter()
        .find(|p| p.id == "qoder")
        .context("qoder missing after install")?;
    ensure!(
        !entry.installable && entry.version == "99.0.0" && !entry.navigation.is_empty(),
        "installed entry: {entry:?}"
    );

    let view = panel(
        &workbench(client, "plugins.panel", "qoder", None)
            .await?
            .output,
    );
    ensure!(view["badge"] == "未登录", "panel before sign-in: {view}");

    let started = panel(
        &workbench(
            client,
            "plugins.run",
            "qoder",
            Some(json!({"action": "login"})),
        )
        .await?
        .output,
    );
    ensure!(
        started["openUrl"]
            .as_str()
            .is_some_and(|u| u.starts_with("https://qoder.com/device/"))
            && started["badge"] == "等待授权",
        "login answer: {started}"
    );
    let deadline = Instant::now() + Duration::from_secs(15);
    let signed = loop {
        let view = panel(
            &workbench(client, "plugins.panel", "qoder", None)
                .await?
                .output,
        );
        let card = &view["widgets"][0];
        if view["badge"] == "已登录"
            && card["quota"]["models"] == 1
            && card["actions"].as_array().is_some_and(|a| a.iter().any(|a| a["id"] == "logout"))
        {
            break view;
        }
        ensure!(Instant::now() < deadline, "sign-in did not finish: {view}");
        tokio::time::sleep(Duration::from_millis(300)).await;
    };
    let text = signed.to_string();
    ensure!(
        text.contains("dev@example.com")
            && text.contains("500 / 2,000 credits")
            && text.contains("Pro"),
        "account panel: {text}"
    );

    // Pi's provider entry points at the host and carries no Qoder credential.
    let models: Value = serde_json::from_slice(&std::fs::read(agent.join("models.json"))?)?;
    let provider = &models["providers"]["qoder"];
    ensure!(
        provider["baseUrl"]
            .as_str()
            .is_some_and(|u| u.starts_with("http://127.0.0.1:")),
        "provider: {provider}"
    );
    ensure!(
        !provider.to_string().contains("jt-fake"),
        "Qoder token leaked into models.json"
    );

    let catalog = client
        .workbench(&WorkbenchRequest {
            action: "models".into(),
            model: "qoder/fake-coder".into(),
            ..Default::default()
        })
        .await?
        .catalog
        .context("no catalog")?;
    let entries = catalog.models.as_deref().unwrap_or_default();
    ensure!(
        catalog.default == "magpie/picked"
            && entries.len() == 2
            && entries[0].key() == "magpie/picked"
            && entries[0].plugin_source.is_none(),
        "unexpected merged catalog"
    );
    ensure!(
        catalog
            .models
            .iter()
            .flatten()
            .any(|m| m.provider == "qoder"
                && m.id == "fake-coder"
                && m.plugin_source
                    .as_ref()
                    .is_some_and(|s| s.name == "Qoder 国际版" && s.credit_multiplier == Some(1.0))),
        "catalog: {catalog:?}"
    );

    let created = client
        .dispatch(&vantry_protocol::Command {
            version: 1,
            command_id: "qoder-create".into(),
            kind: CommandKind::CreateThread,
            title: "Qoder".into(),
            provider_id: "pi".into(),
            model: "qoder/fake-coder".into(),
            approval_policy: "auto".into(),
            workspace: Some(WorkspaceSpec {
                kind: "directory".into(),
                path: ws.display().to_string(),
                ..Default::default()
            }),
            ..Default::default()
        })
        .await?;
    let sent = client
        .dispatch(&vantry_protocol::Command {
            version: 1,
            command_id: "qoder-send".into(),
            kind: CommandKind::SendMessage,
            thread_id: created.thread_id.clone(),
            model: "qoder/fake-coder".into(),
            message: "What does the file say? READ:note.txt".into(),
            ..Default::default()
        })
        .await?;
    let deadline = Instant::now() + Duration::from_secs(90);
    let run = loop {
        let detail = client.thread_detail(&created.thread_id, 10).await?;
        if let Some(run) = detail.runs.iter().flatten().find(|r| r.id == sent.run_id)
            && matches!(
                run.status,
                RunStatus::Completed | RunStatus::Failed | RunStatus::Cancelled
            )
        {
            break (run.clone(), detail);
        }
        ensure!(Instant::now() < deadline, "run did not finish");
        tokio::time::sleep(Duration::from_millis(200)).await;
    };
    let (run, detail) = run;
    ensure!(
        run.status == RunStatus::Completed,
        "run failed: {}",
        run.error
    );
    ensure!(
        run.output.contains(secret),
        "answer should quote the file: {:?}",
        run.output
    );
    ensure!(
        detail.tools.iter().flatten().any(|t| t.name == "read"),
        "Pi should have run the read tool: {:?}",
        detail.tools
    );
    let calls: Vec<Value> = std::fs::read_to_string(log)?
        .lines()
        .filter_map(|l| serde_json::from_str(l).ok())
        .collect();
    ensure!(
        calls.iter().any(|c| c["roles"]
            .as_array()
            .is_some_and(|r| r.iter().any(|x| x == "tool")))
            && calls.iter().all(|c| c["model"] == "fake-coder")
            && calls[0]["tools"]
                .as_array()
                .is_some_and(|t| t.iter().any(|x| x == "read")),
        "plugin requests: {calls:?}"
    );

    // Sign-out removes Pi's provider and the stored credentials.
    let out = panel(
        &workbench(
            client,
            "plugins.run",
            "qoder",
            Some(json!({"action": "logout"})),
        )
        .await?
        .output,
    );
    ensure!(out["badge"] == "未登录", "after logout: {out}");
    let models: Value = serde_json::from_slice(&std::fs::read(agent.join("models.json"))?)?;
    ensure!(
        models["providers"].get("qoder").is_none(),
        "provider kept after logout"
    );
    let _ = ws;
    Ok(())
}

/// Upgrade drill against npm (network): an older installed version is
/// replaced by the registry's latest after integrity check and self-test.
/// Run with `cargo test -p vantry-service --test qoder_plugin -- --ignored`.
#[tokio::test(flavor = "multi_thread")]
#[ignore = "downloads the plugin from registry.npmjs.org"]
async fn qoder_plugin_updates_itself_from_npm() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let data = tmp.path().join("data");
    let agent = tmp.path().join("pi-agent");
    std::fs::create_dir_all(&agent)?;
    // Replace a stand-in older provider with the published upstream version.
    let old = data.join("model-plugins/qoder/versions/0.2.70");
    std::fs::create_dir_all(&old)?;
    std::fs::write(old.join("opencode.mjs"), FAKE_PLUGIN)?;
    std::fs::write(
        data.join("model-plugins/qoder/state.json"),
        serde_json::to_vec(&json!({"current": "0.2.70"}))?,
    )?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        for dir in [
            &data,
            &data.join("model-plugins"),
            &data.join("model-plugins/qoder"),
            &data.join("model-plugins/qoder/versions"),
            &old,
        ] {
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
    }
    let _core = ChildGuard(Some(
        Command::new(env!("CARGO_BIN_EXE_vantry-service"))
            .env("PI_CODING_AGENT_DIR", &agent)
            .args(["core", "--provider", "pi", "--data-dir"])
            .arg(&data)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()?,
    ));
    let client = CoreClient::from_data_dir(&data);
    let deadline = Instant::now() + Duration::from_secs(20);
    while client.health().await.is_err() {
        ensure!(Instant::now() < deadline, "Core did not start");
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let result = async {
        let installed = workbench(&client, "plugins.install", "qoder", None).await?;
        let entry = installed
            .plugins
            .iter()
            .find(|p| p.id == "qoder")
            .context("missing")?;
        ensure!(entry.version == "0.2.70", "installed {entry:?}");
        let checked = panel(
            &workbench(
                &client,
                "plugins.run",
                "qoder",
                Some(json!({"action": "check_update"})),
            )
            .await?
            .output,
        );
        let state: Value =
            serde_json::from_slice(&std::fs::read(data.join("model-plugins/qoder/state.json"))?)?;
        ensure!(
            state["current"] != "0.2.70"
                && state["previous"] == "0.2.70"
                && state["checkError"] == "",
            "state after update: {state}\npanel: {checked}"
        );
        let host: Value =
            serde_json::from_slice(&std::fs::read(data.join("model-plugins/qoder/host.json"))?)?;
        ensure!(
            host["pluginVersion"] == state["current"],
            "host runs {host}"
        );
        Ok(())
    }
    .await;
    if let Ok(host) = std::fs::read(data.join("model-plugins/qoder/host.json")) {
        let host: Value = serde_json::from_slice(&host)?;
        if let Some(pid) = host["pid"].as_u64() {
            let _ = Command::new("kill").arg(pid.to_string()).status();
        }
    }
    result
}

/// A plugin version whose host cannot start rolls back to the previous one
/// and is not tried again automatically.
#[tokio::test(flavor = "multi_thread")]
async fn qoder_plugin_rolls_back_a_version_that_fails_to_start() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let data = tmp.path().join("data");
    let agent = tmp.path().join("pi-agent");
    std::fs::create_dir_all(&agent)?;
    let broken = data.join("model-plugins/qoder/versions/99.0.1");
    std::fs::create_dir_all(&broken)?;
    std::fs::write(
        broken.join("opencode.mjs"),
        "throw new Error('broken build')\n",
    )?;
    let previous = broken.with_file_name("0.2.73");
    std::fs::create_dir_all(&previous)?;
    std::fs::write(previous.join("opencode.mjs"), FAKE_PLUGIN)?;
    std::fs::write(
        data.join("model-plugins/qoder/state.json"),
        serde_json::to_vec(&json!({"current": "99.0.1", "previous": "0.2.73"}))?,
    )?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        for dir in [
            &data,
            &data.join("model-plugins"),
            &data.join("model-plugins/qoder"),
            &data.join("model-plugins/qoder/versions"),
            &broken,
        ] {
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
    }
    let _core = ChildGuard(Some(
        Command::new(env!("CARGO_BIN_EXE_vantry-service"))
            .env("PI_CODING_AGENT_DIR", &agent)
            .args(["core", "--provider", "pi", "--data-dir"])
            .arg(&data)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()?,
    ));
    let client = CoreClient::from_data_dir(&data);
    let deadline = Instant::now() + Duration::from_secs(20);
    while client.health().await.is_err() {
        ensure!(Instant::now() < deadline, "Core did not start");
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let result = async {
        // Starting the broken revision restores the previously installed one.
        let installed = workbench(&client, "plugins.install", "qoder", None).await?;
        let entry = installed
            .plugins
            .iter()
            .find(|p| p.id == "qoder")
            .context("missing")?;
        ensure!(
            entry.version == "0.2.73",
            "should run the previous version: {entry:?}"
        );
        let state: Value =
            serde_json::from_slice(&std::fs::read(data.join("model-plugins/qoder/state.json"))?)?;
        ensure!(state["rejected"] == json!(["99.0.1"]), "state: {state}");
        let view = panel(
            &workbench(&client, "plugins.panel", "qoder", None)
                .await?
                .output,
        );
        ensure!(
            view.to_string().contains("已回退到 0.2.73"),
            "panel: {view}"
        );
        Ok(())
    }
    .await;
    if let Ok(host) = std::fs::read(data.join("model-plugins/qoder/host.json")) {
        let host: Value = serde_json::from_slice(&host)?;
        if let Some(pid) = host["pid"].as_u64() {
            let _ = Command::new("kill").arg(pid.to_string()).status();
        }
    }
    result
}
