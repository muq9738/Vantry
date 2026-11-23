//! Fork and rewind the active Pi JSONL branch under the same lock used by the worker.
use crate::{
    disk,
    worker::{lock_file, valid_id},
};
use anyhow::{Context, Result, ensure};
use chrono::Utc;
use serde_json::{Value, json};
use std::{
    collections::{HashMap, HashSet},
    fs::{self, File},
    io::ErrorKind,
    path::{Path, PathBuf},
};
use uuid::Uuid;

fn sessions(data: &Path) -> Result<PathBuf> {
    ensure!(
        !data.as_os_str().is_empty(),
        "missing session data directory"
    );
    let dir = std::path::absolute(data)?.join("sessions");
    disk::directory(&dir)?;
    Ok(dir)
}
pub(crate) fn lock_thread(dir: &Path, thread: &str) -> Result<File> {
    valid_id(thread)?;
    let lock = lock_file(&dir.join(format!("{thread}.lock")))?;
    lock.try_lock().context("thread session in use")?;
    Ok(lock)
}

fn active_branch(entries: &[Value]) -> Result<Vec<&Value>> {
    let mut by_id = HashMap::new();
    for entry in entries {
        if let Some(id) = entry["id"].as_str().filter(|s| !s.is_empty()) {
            ensure!(by_id.insert(id, entry).is_none(), "duplicate Pi entry ID");
        }
    }
    // Pre-tree sessions are linear and can lack IDs.
    if by_id.len() != entries.len() {
        ensure!(
            entries.iter().all(|e| e["parentId"].is_null()),
            "mixed linear and branch history"
        );
        return Ok(entries.iter().collect());
    }
    let mut branch = vec![];
    let mut visited = HashSet::new();
    let mut cursor = entries.last();
    while let Some(entry) = cursor {
        ensure!(visited.insert(entry["id"].as_str()), "cycle in Pi history");
        branch.push(entry);
        cursor = match &entry["parentId"] {
            Value::Null => None,
            parent => Some(
                *by_id
                    .get(parent.as_str().context("invalid parentId")?)
                    .context("missing Pi parent entry")?,
            ),
        };
    }
    branch.reverse();
    Ok(branch)
}

fn seed(source: &Path, target: &Path, fork: Option<&str>, turns: usize) -> Result<()> {
    let text = match fs::read_to_string(source) {
        Ok(text) => text,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e.into()),
    };
    let mut header = None;
    let mut entries = vec![];
    for (i, line) in text
        .lines()
        .enumerate()
        .filter(|(_, l)| !l.trim().is_empty())
    {
        let value: Value = serde_json::from_str(line)
            .with_context(|| format!("invalid session line {}", i + 1))?;
        let kind = value["type"].as_str().context("missing Pi entry type")?;
        if kind == "session" {
            ensure!(
                header.is_none() && entries.is_empty(),
                "misplaced Pi session header"
            );
            header = Some(value);
        } else {
            entries.push(value);
        }
    }
    if entries.is_empty() && header.is_none() {
        return Ok(());
    }
    let mut header = header.unwrap_or_else(|| json!({"type":"session"}));
    header
        .as_object_mut()
        .unwrap()
        .entry("version")
        .or_insert(json!(3));
    if let Some(thread) = fork {
        header["id"] = json!(format!("vantry-{thread}-{}", Uuid::new_v4()));
        header["timestamp"] = json!(Utc::now());
        header["parentSession"] = json!(source);
    }
    let mut bytes = serde_json::to_vec(&header)?;
    bytes.push(b'\n');
    let mut assistants = 0;
    let mut kept = HashSet::new();
    let mut pending_calls = HashSet::new();
    for entry in active_branch(&entries)? {
        let kind = entry["type"].as_str().unwrap();
        let msg = &entry["message"];
        let role = msg["role"].as_str().unwrap_or_default();
        let result = kind == "message" && matches!(role, "toolResult" | "tool_result");
        let trailing = result
            || matches!(kind, "usage" | "custom" | "session_info")
            || (matches!(kind, "label" | "context_edit")
                && entry["targetId"]
                    .as_str()
                    .is_none_or(|s| s.is_empty() || kept.contains(s)));
        if turns > 0 && assistants >= turns && !trailing {
            break;
        }
        if result {
            ensure!(
                pending_calls.remove(
                    msg["toolCallId"]
                        .as_str()
                        .context("tool result missing call ID")?
                ),
                "orphan Pi tool result"
            );
        } else if kind == "message" {
            ensure!(pending_calls.is_empty(), "incomplete Pi tool calls");
            if role == "assistant" {
                assistants += 1;
                for block in msg["content"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .filter(|b| b["type"] == "toolCall")
                {
                    ensure!(
                        pending_calls.insert(block["id"].as_str().context("tool call missing ID")?),
                        "duplicate Pi tool call"
                    );
                }
            }
        }
        if let Some(id) = entry["id"].as_str() {
            kept.insert(id);
        }
        serde_json::to_writer(&mut bytes, entry)?;
        bytes.push(b'\n');
    }
    ensure!(pending_calls.is_empty(), "session ends within tool calls");
    disk::atomic(target, &bytes)
}

/// Zero turns clones the complete active branch. A missing or empty source is a no-op.
pub fn clone_session_file(data: &Path, source: &str, target: &str, turns: usize) -> Result<()> {
    valid_id(source)?;
    valid_id(target)?;
    ensure!(source != target, "cannot fork a thread onto itself");
    let dir = sessions(data)?;
    let _src = lock_thread(&dir, source)?;
    let _dst = lock_thread(&dir, target)?;
    seed(
        &dir.join(format!("{source}.jsonl")),
        &dir.join(format!("{target}.jsonl")),
        Some(target),
        turns,
    )
}
/// Zero turns removes the session so the next attempt starts fresh.
pub fn truncate_session_file(data: &Path, thread: &str, turns: usize) -> Result<()> {
    valid_id(thread)?;
    let dir = sessions(data)?;
    let _guard = lock_thread(&dir, thread)?;
    let path = dir.join(format!("{thread}.jsonl"));
    if turns == 0 {
        match fs::remove_file(path) {
            Ok(()) => {}
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
        #[cfg(unix)]
        File::open(dir)?.sync_all()?;
        return Ok(());
    }
    seed(&path, &path, None, turns)
}
