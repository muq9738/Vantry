use anyhow::Result;
use serde_json::{Value, json};
use std::{fs, io::Write, path::Path, time::Duration};
use tempfile::tempdir;
use vantry_pi_runtime::*;
use vantry_protocol::{Attachment, ControlResponse, PiSetupRequest};

fn save(path: &Path, v: Value) -> Result<()> {
    fs::write(path, serde_json::to_vec(&v)?)?;
    Ok(())
}
fn load(path: &Path) -> Result<Value> {
    Ok(serde_json::from_slice(&fs::read(path)?)?)
}
#[test]
fn branch_cut_keeps_parallel_results_and_detects_corruption() -> Result<()> {
    let tmp = tempdir()?;
    let d = tmp.path();
    clone_session_file(d, "a", "b", 0)?;
    let dir = d.join("sessions");
    assert!(!dir.join("b.jsonl").exists());
    let path = dir.join("a.jsonl");
    let header = json!({"type":"session","id":"s","version":3,"cwd":"/workspace"});
    let nodes = [
        header,
        json!({"type":"message","id":"u","message":{"role":"user"}}),
        json!({"type":"message","id":"side","parentId":"u","message":{"role":"assistant"}}),
        json!({"type":"message","id":"a","parentId":"u","message":{"role":"assistant","content":[{"type":"toolCall","id":"x"},{"type":"toolCall","id":"y"}]}}),
        json!({"type":"message","id":"r1","parentId":"a","message":{"role":"toolResult","toolCallId":"y"}}),
        json!({"type":"message","id":"r2","parentId":"r1","message":{"role":"toolResult","toolCallId":"x"}}),
        json!({"type":"label","id":"l","parentId":"r2","targetId":"r1"}),
        json!({"type":"message","id":"next","parentId":"l","message":{"role":"assistant"}}),
    ];
    fs::write(
        &path,
        nodes
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>()
            .join("\n"),
    )?;
    clone_session_file(d, "a", "b", 1)?;
    let copy = dir.join("b.jsonl");
    let history = fs::read_to_string(&copy)?;
    assert!(
        history.contains("parentSession")
            && history.contains("\"r2\"")
            && history.contains("\"label\"")
    );
    assert!(!history.contains("side") && !history.contains("next"));
    truncate_session_file(d, "a", 1)?;
    assert!(!fs::read_to_string(&path)?.contains("next"));
    for bad in [
        json!({"type":"custom","id":"e","parentId":"missing"}),
        json!({"type":"custom","id":"e","parentId":"e"}),
        json!({"type":"message","id":"e","message":{"role":"toolResult","toolCallId":"x"}}),
        json!({"type":"message","id":"e","message":{"role":"assistant","content":[{"type":"toolCall","id":"z"}]}}),
    ] {
        save(&path, bad)?;
        assert!(clone_session_file(d, "a", "b", 0).is_err());
        assert_eq!(fs::read_to_string(&copy)?, history);
    }
    assert!(truncate_session_file(d, "../outside", 0).is_err());
    truncate_session_file(d, "a", 0)?;
    assert!(!path.exists());
    Ok(())
}
#[test]
fn journal_resumes_at_last_committed_byte_even_mid_utf8() -> Result<()> {
    let tmp = tempdir()?;
    let path = tmp.path().join("events.jsonl");
    let mut file = fs::File::create(&path)?;
    writeln!(file, "{}", json!({"sequence":1,"delta":"a"}))?;
    let mut r = JobEvents::open(tmp.path(), 1)?;
    let ck = r.checkpoint();
    let second = json!({"sequence":2,"delta":"中"}).to_string();
    let split = second.find('中').unwrap() + 1;
    file.write_all(&second.as_bytes()[..split])?;
    assert!(r.next_event()?.is_none());
    assert_eq!(r.checkpoint(), ck);
    let mut resumed = IncrementalJournalReader::resume(tmp.path(), ck)?;
    file.write_all(&second.as_bytes()[split..])?;
    assert!(resumed.next_event()?.is_none());
    file.write_all(b"\n")?;
    assert_eq!(resumed.next_event()?.unwrap().delta, "中");
    assert_eq!(r.next_event()?.unwrap().sequence, 2);
    assert!(resumed.next_event()?.is_none());
    assert!(
        IncrementalJournalReader::resume(
            tmp.path(),
            JournalCursor {
                offset: ck.offset - 1,
                ..ck
            }
        )
        .is_err()
    );
    writeln!(file, "{}", json!({"sequence":4}))?;
    assert!(r.next_event().is_err());
    Ok(())
}
#[test]
fn live_reader_discards_bytes_from_a_recovered_torn_record() -> Result<()> {
    let tmp = tempdir()?;
    let dir = tmp.path();
    let path = dir.join("events.jsonl");
    let prefix = b"{\"sequence\":1,\"delta\":\"stale";
    fs::write(&path, prefix)?;
    let mut reader = JobEvents::open(dir, 0)?;
    assert!(reader.next_event()?.is_none());
    fs::write(dir.join("events.partial"), prefix)?;
    fs::write(&path, b"")?;
    fs::write(dir.join("closing.json"), b"{}")?;
    fs::write(&path, b"{\"sequence\":1,\"delta\":\"fresh result\"}\n")?;
    assert_eq!(reader.next_event()?.unwrap().delta, "fresh result");
    Ok(())
}
#[tokio::test]
async fn catalog_projects_authenticated_models_and_magpie_order() -> Result<()> {
    let tmp = tempdir()?;
    let d = tmp.path();
    save_setup(
        d,
        &PiSetupRequest {
            provider_id: "magpie".into(),
            api_key: "fixture-private-token".into(),
            base_url: "http://localhost:12345/secret".into(),
            models: vec!["model:high".into(), "family/id".into()],
            set_default: true,
            ..Default::default()
        },
    )?;
    let mag = d.join("magpie.json");
    save(
        &mag,
        json!({"pickedModels":{"pi":["family/id","missing","model:high","family/id"],"other":["unrelated"]}}),
    )?;
    let picked = MagpieSelection::load(Some(&mag))?;
    assert_eq!(
        picked.resolve("magpie/model:high:low")?,
        "magpie/model:high"
    );
    assert!(picked.resolve("wrong").is_err());
    let catalog = model_catalog(d, Some("magpie/family/id:low"), Some(&picked)).await?;
    assert_eq!(catalog.default, "magpie/model:high");
    assert_eq!(catalog.configured_default, catalog.default);
    assert!(!catalog.warnings.is_empty());
    assert_eq!(
        catalog
            .models
            .as_ref()
            .unwrap()
            .iter()
            .map(|m| &m.id)
            .collect::<Vec<_>>(),
        ["family/id", "model:high"]
    );
    let json = serde_json::to_string(&catalog)?;
    assert!(!json.contains("fixture-private-token") && !json.contains("localhost"));
    let mut settings = load(&d.join("settings.json"))?;
    settings["modelThinkingLevels"] = json!({"magpie/model:high":"medium"});
    settings["defaultThinkingLevel"] = json!("low");
    save(&d.join("settings.json"), settings)?;
    let mut model_defs = load(&d.join("models.json"))?;
    model_defs["providers"]["magpie"]["models"][0]["reasoning"] = json!(true);
    save(&d.join("models.json"), model_defs)?;
    let default = resolve_model_catalog(d, None, None).await?;
    assert_eq!(default.selected_model, "magpie/model:high");
    assert_eq!(default.thinking_level.unwrap().as_str(), "medium");
    let explicit = resolve_model_catalog(d, Some("magpie/model:high:high"), Some(&picked)).await?;
    assert_eq!(explicit.thinking_level.unwrap().as_str(), "high");
    // Unprefixed legacy/picker selections resolve cleanly to canonical provider/id.
    let unprefixed = resolve_model_catalog(d, Some("family/id:low"), None).await?;
    assert_eq!(unprefixed.selected_model, "magpie/family/id");
    let unprefixed_magpie = resolve_model_catalog(d, Some("family/id:low"), Some(&picked)).await?;
    assert_eq!(unprefixed_magpie.selected_model, "magpie/family/id");
    assert_eq!(unprefixed_magpie.catalog.models.as_ref().unwrap()[0].key(), "magpie/family/id");
    let general = model_catalog(d, None, None).await?;
    assert_eq!(general.models.unwrap()[0].id, "model:high");
    assert!(model_catalog(d, Some("other/missing"), None).await.is_err());
    Ok(())
}
#[test]
fn mcp_override_projects_safe_fields_and_tracks_trust() -> Result<()> {
    let t = tempdir()?;
    let agent = t.path().join("agent");
    fs::create_dir(&agent)?;
    let cwd = t
        .path()
        .join("project")
        .canonicalize()
        .unwrap_or_else(|_| t.path().join("project"));
    fs::create_dir_all(cwd.join(".pi"))?;
    save(
        &agent.join("mcp.json"),
        json!({"mcpServers":{"test":{"url":"https://user:secret@example.org/token","headers":{"Authorization":"mcp-fixture-credential"},"toolExposure":{"a":"direct"}}}}),
    )?;
    save(
        &cwd.join(".pi/mcp.json"),
        json!({"autoEnableCodemode":false,"mcpServers":{"test":{"enabled":false,"toolExposure":{"b":"hidden"}},"bad":{"command":"test","url":"http://localhost"},"local":{"command":"server","args":["mcp-fixture-credential"]}}}),
    )?;
    let c = mcp::mcp_catalog(&agent, Some(&cwd))?;
    assert!(!c.project_trusted && !c.codemode_default);
    let servers = c.servers.as_ref().unwrap();
    assert!(!servers[0].error.is_empty());
    assert_eq!(servers[1].exposure, "codemode");
    assert_eq!(servers[2].scope, "project-override");
    assert!(!servers[2].enabled);
    assert_eq!(servers[2].tool_exposure["b"], "hidden");
    assert_eq!(servers[2].endpoint, "https://example.org");
    assert!(!serde_json::to_string(&c)?.contains("mcp-fixture-credential"));
    pi_coding_agent::resources::trust::ProjectTrustStore::new(&agent).set(&cwd, Some(true))?;
    assert!(mcp::mcp_catalog(&agent, Some(&cwd))?.project_trusted);
    Ok(())
}
#[test]
fn attachment_identity_text_images_and_pdf() -> Result<()> {
    let t = tempdir()?;
    let mut attachments = vec![];
    for (i, (name, mime, content)) in [
        ("note.txt", "text/plain", "笔记"),
        ("pixel.png", "image/png", "abc"),
        (
            "spec.pdf",
            "application/x-pdf",
            "%PDF-1.4\n/Type /Page\nstream\nBT (Specification) Tj ET\nendstream\n%%EOF",
        ),
    ]
    .into_iter()
    .enumerate()
    {
        let item = Attachment {
            id: format!("file_{i}"),
            name: name.into(),
            mime: mime.into(),
            size: content.len() as i64,
        };
        let root = t.path().join("attachments");
        let dir = root.join(&item.id);
        let blob = dir.join("content");
        fs::create_dir_all(&blob)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            for path in [&root, &dir, &blob] {
                fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
            }
        }
        save(&dir.join("metadata.json"), json!(&item))?;
        fs::write(blob.join(name), content)?;
        attachments.push(item);
    }
    let (text, images) = attachment_payload(t.path(), "test", &attachments)?;
    assert!(text.contains("笔记") && text.contains("Specification") && text.contains("1 pages"));
    assert_eq!(serde_json::to_value(images)?[0]["data"], "YWJj");
    let mut fake = attachments[0].clone();
    fake.mime = "image/png".into();
    assert!(attachment_payload(t.path(), "", &[fake]).is_err());
    #[cfg(unix)]
    {
        let path = t.path().join("attachments/file_0/content/note.txt");
        fs::remove_file(&path)?;
        std::os::unix::fs::symlink(t.path().join("attachments/file_1/content/pixel.png"), path)?;
        assert!(attachment_payload(t.path(), "", &attachments).is_err());
    }
    Ok(())
}
fn job(d: &Path, attempt: &str) -> Result<JobRequest> {
    Ok(serde_json::from_value(
        json!({"dataDir":d,"request":{"ProviderID":"controlled","ThreadID":"t","RunID":"r","AttemptID":attempt,"Message":"hello"}}),
    )?)
}
#[tokio::test]
async fn scripted_worker_handles_ui_steer_and_recovery() -> Result<()> {
    use ControlledStep::*;
    let t = tempdir()?;
    let j = job(t.path(), "a")?;
    let p = ControlledTestProvider {
        steps: vec![
            Thinking {
                delta: "reason".into(),
            },
            Tool {
                name: "read".into(),
                id: "call_1".into(),
                arguments: json!({}),
                result: json!("ok"),
            },
            Approval {
                title: "Allow".into(),
                message: "Edit?".into(),
            },
            AwaitSteer,
            Text {
                delta: "done".into(),
            },
        ],
    };
    let dir = p.prepare(&j)?;
    p.prepare(&j)?;
    let observe = async {
        let mut reader = loop {
            if dir.join("events.jsonl").exists() {
                break JobEvents::open(&dir, 0)?;
            }
            tokio::task::yield_now().await;
        };
        loop {
            while let Some(e) = reader.next_event()? {
                if e.r#type == "extension_ui_request" {
                    let c = ControlResponse {
                        id: "answer".into(),
                        kind: "response".into(),
                        request_id: e.data.unwrap()["id"].as_str().unwrap().into(),
                        confirmed: true,
                        ..Default::default()
                    };
                    deliver_control(&dir, &c)?;
                    deliver_control(&dir, &c)?;
                    let steer = ControlResponse {
                        id: "steer".into(),
                        kind: "steer".into(),
                        message: "new message".into(),
                        ..Default::default()
                    };
                    deliver_control(&dir, &steer)?;
                    return anyhow::Ok(());
                }
            }
            tokio::time::sleep(Duration::from_millis(3)).await;
        }
    };
    tokio::time::timeout(Duration::from_secs(3), async {
        tokio::try_join!(run_worker(&dir), observe)
    })
    .await??;
    let log = fs::read_to_string(dir.join("events.jsonl"))?;
    for s in [
        "thinking_delta",
        "tool_execution_start",
        "tool_execution_end",
        "new message",
        "done",
    ] {
        assert!(log.contains(s));
    }
    assert_eq!(log.matches("control_result").count(), 2);
    run_worker(&dir).await?;
    assert_eq!(log, fs::read_to_string(dir.join("events.jsonl"))?);
    let crash = ControlledTestProvider {
        steps: vec![
            Text {
                delta: "once".into(),
            },
            Crash,
        ],
    }
    .prepare(&job(t.path(), "crash")?)?;
    assert!(run_worker(&crash).await.is_err());
    assert!(!crash.join("result.json").exists());
    fs::OpenOptions::new()
        .append(true)
        .open(crash.join("events.jsonl"))?
        .write_all(b"partial")?;
    assert!(run_worker(&crash).await.is_err());
    assert_eq!(load(&crash.join("result.json"))?["status"], "interrupted");
    assert_eq!(fs::read(crash.join("events.partial"))?, b"partial");
    let input = job(t.path(), "abandoned-input")?;
    let abandoned = ControlledTestProvider { steps: vec![Crash] }.prepare(&input)?;
    assert!(run_worker(&abandoned).await.is_err());
    writeln!(
        fs::OpenOptions::new()
            .append(true)
            .open(abandoned.join("events.jsonl"))?,
        "{}",
        json!({"sequence":1,"type":"extension_ui_request","data":{"type":"extension_ui_request","method":"input","id":"private"}})
    )?;
    let reply = ControlResponse {
        id: "late".into(),
        kind: "response".into(),
        request_id: "private".into(),
        value: "late-fixture-secret".into(),
        ..Default::default()
    };
    deliver_control(&abandoned, &reply)?;
    assert!(run_worker(&abandoned).await.is_err());
    deliver_control(&abandoned, &reply)?;
    for entry in fs::read_dir(abandoned.join("controls"))? {
        assert!(!fs::read_to_string(entry?.path())?.contains("late-fixture-secret"));
    }
    let failed = ControlledTestProvider {
        steps: vec![Error {
            message: "injected".into(),
        }],
    }
    .prepare(&job(t.path(), "fail")?)?;
    assert!(run_worker(&failed).await.is_err());
    assert_eq!(load(&failed.join("result.json"))?["error"], "injected");
    let stopped = ControlledTestProvider {
        steps: vec![AwaitSteer],
    }
    .prepare(&job(t.path(), "stop")?)?;
    tokio::try_join!(run_worker(&stopped), async {
        tokio::time::sleep(Duration::from_millis(20)).await;
        request_stop(&stopped)
    })?;
    assert_eq!(load(&stopped.join("result.json"))?["status"], "cancelled");
    Ok(())
}
