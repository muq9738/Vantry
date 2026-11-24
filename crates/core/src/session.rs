//! Materialize the Pi session selected by a fork or rewind before another attempt can start.
//! The pending JSONL is committed with the command receipt and cleared after atomic publication.
use crate::{Result, id, invalid, now, timestamp};
use rusqlite::{Connection, params};
use serde_json::{Value, json};
use std::{
    collections::{HashMap, HashSet},
    fs,
    io::{ErrorKind, Write},
    path::{Path, PathBuf},
};
fn path(dir: &Path, thread: &str) -> Result<PathBuf> {
    if thread.is_empty()
        || matches!(thread, "." | "..")
        || !thread
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-._".contains(&b))
    {
        return Err(invalid("invalid Pi session thread ID"));
    }
    Ok(dir.join(format!("{thread}.jsonl")))
}
fn directory(data: &Path) -> Result<PathBuf> {
    let dir = data.join("sessions");
    match fs::symlink_metadata(&dir) {
        Ok(m) if !m.is_dir() => Err(invalid("invalid sessions directory")),
        Err(e) if e.kind() != ErrorKind::NotFound => Err(e.into()),
        _ => Ok(dir),
    }
}
fn active_branch(entries: &[Value]) -> Vec<&Value> {
    let by_id: HashMap<_, _> = entries
        .iter()
        .filter_map(|e| e["id"].as_str().filter(|s| !s.is_empty()).map(|id| (id, e)))
        .collect();
    if by_id.len() != entries.len() {
        return entries.iter().collect();
    }
    let mut rev = vec![];
    let mut seen = HashSet::new();
    let mut current = entries.last();
    while let Some(entry) = current {
        if !seen.insert(entry["id"].as_str()) {
            break;
        }
        rev.push(entry);
        current = entry["parentId"]
            .as_str()
            .and_then(|p| by_id.get(p).copied());
    }
    rev.reverse();
    rev
}
pub(super) fn stage(
    db: &Connection,
    data: &Path,
    source: &str,
    target: &str,
    keep: i64,
) -> Result<()> {
    let dir = directory(data)?;
    if !dir.exists() {
        return Ok(());
    }
    let source_path = path(&dir, source)?;
    path(&dir, target)?;
    let metadata = match fs::symlink_metadata(&source_path) {
        Ok(m) => m,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e.into()),
    };
    if !metadata.is_file() {
        return Err(invalid("session must be a regular file"));
    }
    let mut output = String::new();
    if keep > 0 {
        let raw = fs::read_to_string(&source_path)?;
        if raw.trim().is_empty() {
            return Ok(());
        }
        let mut header = None;
        let mut entries = Vec::new();
        for line in raw.lines().filter(|s| !s.trim().is_empty()) {
            let v: Value = serde_json::from_str(line)?;
            if v["type"].as_str().is_none() {
                return Err(invalid("session entry missing type"));
            }
            if v["type"] == "session" && header.is_none() {
                header = Some(v);
            } else {
                entries.push(v);
            }
        }
        let mut header = header.unwrap_or_else(|| json!({"type":"session","version":3}));
        if source != target {
            header["id"] = json!(id("vantry"));
            header["timestamp"] = json!(timestamp(now()));
            header["parentSession"] = json!(source_path);
        }
        output.push_str(&header.to_string());
        output.push('\n');
        let mut turns = 0;
        let mut ids = HashSet::new();
        for e in active_branch(&entries) {
            let kind = e["type"].as_str().unwrap_or_default();
            let role = e.pointer("/message/role").and_then(Value::as_str);
            if turns >= keep {
                let trailing = match kind {
                    "message" => role == Some("toolResult"),
                    "usage" | "custom" | "session_info" => true,
                    "label" | "context_edit" => e["targetId"]
                        .as_str()
                        .filter(|s| !s.is_empty())
                        .is_none_or(|id| ids.contains(id)),
                    _ => false,
                };
                if !trailing {
                    break;
                }
            }
            if let Some(id) = e["id"].as_str() {
                ids.insert(id);
            }
            if kind == "message" && role == Some("assistant") {
                turns += 1;
            }
            output.push_str(&e.to_string());
            output.push('\n');
        }
    }
    if source != target && keep == 0 {
        return Ok(());
    }
    db.execute(
        "INSERT OR REPLACE INTO core_metadata(key,text_value) VALUES(?,?)",
        params![format!("session.publish.{target}"), output],
    )?;
    Ok(())
}
pub(crate) fn flush(db: &Connection, data: &Path) -> Result<()> {
    let entries = db
        .prepare("SELECT key,text_value FROM core_metadata WHERE key GLOB 'session.publish.*'")?
        .query_map([], |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for (key, contents) in entries {
        let dir = directory(data)?;
        let dest = path(&dir, &key["session.publish.".len()..])?;
        if contents.is_empty() {
            match fs::remove_file(&dest) {
                Ok(()) => {}
                Err(e) if e.kind() == ErrorKind::NotFound => {}
                Err(e) => return Err(e.into()),
            }
        } else {
            if !dir.exists() {
                let mut builder = fs::DirBuilder::new();
                #[cfg(unix)]
                {
                    use std::os::unix::fs::DirBuilderExt;
                    builder.mode(0o700);
                }
                builder.create(&dir)?;
            }
            let temp = dir.join(format!(".tmp-session-{}", uuid::Uuid::new_v4()));
            let result = (|| -> Result<()> {
                let mut options = fs::File::options();
                options.create_new(true).write(true);
                #[cfg(unix)]
                {
                    use std::os::unix::fs::OpenOptionsExt;
                    options.mode(0o600);
                }
                let mut f = options.open(&temp)?;
                f.write_all(contents.as_bytes())?;
                f.sync_all()?;
                drop(f);
                fs::rename(&temp, &dest)?;
                Ok(())
            })();
            if result.is_err() {
                let _ = fs::remove_file(&temp);
            }
            result?;
        }
        #[cfg(unix)]
        if dir.exists() {
            fs::File::open(dir)?.sync_all()?;
        }
        db.execute("DELETE FROM core_metadata WHERE key=?", [key])?;
    }
    Ok(())
}
