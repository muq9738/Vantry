//! Version-one environment catalog. An absent kind denotes a legacy SSH entry.
use crate::hostfs;
use anyhow::{Context, Result, ensure};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashSet,
    path::{Path, PathBuf},
};
pub const DEFAULT_SOCKET: &str = "~/.vantry/core.sock";
#[derive(Default, Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    Local,
    #[default]
    Ssh,
    Remote,
}
fn legacy(kind: &Kind) -> bool {
    *kind == Kind::Ssh
}
#[derive(Default, Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Entry {
    pub id: String,
    pub name: String,
    pub host: String,
    #[serde(skip_serializing_if = "legacy")]
    pub kind: Kind,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub socket_path: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub default_workspace: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub created_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_connected_at: Option<DateTime<Utc>>,
}
impl Entry {
    pub fn socket(&self) -> &str {
        if self.socket_path.is_empty() {
            DEFAULT_SOCKET
        } else {
            &self.socket_path
        }
    }
    pub fn resolve_socket(&self, home: &str) -> Result<String> {
        if let Some(rest) = self.socket().strip_prefix("~/") {
            ensure!(
                home.starts_with('/') && !rest.is_empty(),
                "absolute remote home required"
            );
            Ok(format!("{}/{rest}", home.trim_end_matches('/')))
        } else {
            Ok(self.socket().into())
        }
    }
    fn normalize(&mut self) {
        for s in [
            &mut self.id,
            &mut self.name,
            &mut self.host,
            &mut self.socket_path,
            &mut self.default_workspace,
        ] {
            *s = s.trim().into();
        }
        if self.socket_path == DEFAULT_SOCKET {
            self.socket_path.clear();
        }
    }
    pub fn validate(&self) -> Result<()> {
        ensure!(
            !self.id.is_empty()
                && self.id.len() <= 64
                && self
                    .id
                    .bytes()
                    .all(|c| c.is_ascii_alphanumeric() || b"._-".contains(&c)),
            "invalid environment id"
        );
        ensure!(
            !self.name.is_empty()
                && self.name.chars().count() <= 80
                && !self.name.chars().any(char::is_control),
            "invalid environment name"
        );
        if self.kind != Kind::Local {
            ensure!(
                !self.host.is_empty()
                    && !self.host.starts_with('-')
                    && !self
                        .host
                        .chars()
                        .any(|c| c.is_whitespace() || c.is_control()),
                "invalid host"
            );
        }
        for path in [&self.socket_path, &self.default_workspace] {
            if path.is_empty() {
                continue;
            }
            let valid = if self.kind == Kind::Local {
                Path::new(path).is_absolute()
            } else {
                (path.starts_with('/') || (path.starts_with("~/") && path.len() > 2))
                    && !path.contains(':')
            };
            ensure!(
                valid && !path.chars().any(char::is_control),
                "expected absolute or home-relative path"
            );
        }
        ensure!(
            self.default_workspace.chars().count() <= 1024,
            "workspace path too long"
        );
        Ok(())
    }
}
#[derive(Serialize, Deserialize, Default)]
struct Document {
    #[serde(default)]
    version: u32,
    #[serde(default)]
    environments: Option<Vec<Entry>>,
}
pub struct Catalog {
    dir: PathBuf,
}
impl Catalog {
    pub fn open(data: &Path) -> Result<Self> {
        Ok(Self {
            dir: hostfs::private_root(data)?,
        })
    }
    fn validate(entries: &mut [Entry]) -> Result<()> {
        let mut seen = HashSet::new();
        for e in entries {
            e.normalize();
            e.validate()?;
            ensure!(seen.insert(e.id.clone()), "duplicate environment id");
        }
        Ok(())
    }
    pub fn load(&self) -> Result<Vec<Entry>> {
        let doc: Document =
            hostfs::read_json(&self.dir.join("environments.json"))?.unwrap_or_default();
        ensure!(doc.version <= 1, "unsupported environments catalog version");
        let mut entries = doc.environments.unwrap_or_default();
        Self::validate(&mut entries)?;
        Ok(entries)
    }
    fn write(&self, mut entries: Vec<Entry>) -> Result<()> {
        Self::validate(&mut entries)?;
        hostfs::write_json(
            &self.dir.join("environments.json"),
            &Document {
                version: 1,
                environments: Some(entries),
            },
        )
    }
    pub fn save(&self, entries: Vec<Entry>) -> Result<()> {
        let _lock = hostfs::lock_dir(&self.dir, true)?;
        self.load()?;
        self.write(entries)
    }
    pub fn upsert(&self, mut entry: Entry) -> Result<Entry> {
        let _lock = hostfs::lock_dir(&self.dir, true)?;
        let mut entries = self.load()?;
        entry.normalize();
        if entry.id.is_empty() {
            let mut slug = String::new();
            for c in entry.name.to_ascii_lowercase().chars().take(48) {
                if c.is_ascii_alphanumeric() {
                    slug.push(c);
                } else if !slug.ends_with('-') {
                    slug.push('-');
                }
            }
            let base = slug.trim_matches('-');
            let base = if base.is_empty() { "environment" } else { base };
            entry.id = base.into();
            let mut n = 2;
            while entries.iter().any(|e| e.id == entry.id) {
                entry.id = format!("{base}-{n}");
                n += 1;
            }
        }
        if let Some(existing) = entries.iter_mut().find(|e| e.id == entry.id) {
            entry.created_at = entry.created_at.or(existing.created_at);
            entry.last_connected_at = entry.last_connected_at.or(existing.last_connected_at);
            *existing = entry.clone();
        } else {
            entry.created_at.get_or_insert_with(Utc::now);
            entries.push(entry.clone());
        }
        self.write(entries)?;
        Ok(entry)
    }
    pub fn remove(&self, id: &str) -> Result<()> {
        let _lock = hostfs::lock_dir(&self.dir, true)?;
        let mut entries = self.load()?;
        let i = entries
            .iter()
            .position(|e| e.id == id)
            .context("environment not found")?;
        entries.remove(i);
        self.write(entries)
    }
    pub fn touch_connected(&self, id: &str, at: DateTime<Utc>) -> Result<()> {
        let _lock = hostfs::lock_dir(&self.dir, true)?;
        let mut entries = self.load()?;
        entries
            .iter_mut()
            .find(|e| e.id == id)
            .context("environment not found")?
            .last_connected_at = Some(at);
        self.write(entries)
    }
}
