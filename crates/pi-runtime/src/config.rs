use crate::disk;
use anyhow::{Context, Result, ensure};
use pi_ai::auth::store_lock::FileLock;
use serde_json::{Map, Value, json};
use std::{
    fs,
    io::ErrorKind,
    path::{Path, PathBuf},
};
use vantry_protocol::PiSetupRequest;

pub fn agent_dir() -> PathBuf {
    pi_coding_agent::config::agent_dir()
}

pub struct PiConfig {
    pub auth: Map<String, Value>,
    pub settings: Map<String, Value>,
    pub models: Map<String, Value>,
}
pub(crate) fn object(path: &Path) -> Result<Map<String, Value>> {
    match fs::File::open(path) {
        Ok(file) => Ok(
            serde_json::from_reader(file).with_context(|| format!("read {}", path.display()))?
        ),
        Err(e) if e.kind() == ErrorKind::NotFound => Ok(Map::new()),
        Err(e) => Err(e.into()),
    }
}
pub fn read_config(dir: &Path) -> Result<PiConfig> {
    Ok(PiConfig {
        auth: object(&dir.join("auth.json"))?,
        settings: object(&dir.join("settings.json"))?,
        models: object(&dir.join("models.json"))?,
    })
}
fn update(
    dir: &Path,
    name: &str,
    edit: impl FnOnce(&mut Map<String, Value>) -> Result<()>,
) -> Result<()> {
    let path = dir.join(name);
    let _lock = FileLock::acquire_blocking(&path)?;
    let mut doc = object(&path)?;
    edit(&mut doc)?;
    disk::write_json(&path, &doc)
}
fn child<'a>(parent: &'a mut Map<String, Value>, key: &str) -> Result<&'a mut Map<String, Value>> {
    parent
        .entry(key)
        .or_insert_with(|| json!({}))
        .as_object_mut()
        .with_context(|| format!("expected object for {key}"))
}
/// Update only this provider's entry. A plugin contributes models independently
/// of the user's Pi default and Magpie selection.
pub fn set_managed_provider(dir: &Path, id: &str, provider: Option<Value>) -> Result<bool> {
    ensure!(
        !id.is_empty()
            && id
                .bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-'),
        "invalid provider ID"
    );
    disk::directory(dir)?;
    let path = dir.join("models.json");
    let _lock = FileLock::acquire_blocking(&path)?;
    let mut doc = object(&path)?;
    let providers = child(&mut doc, "providers")?;
    if providers.get(id) == provider.as_ref() {
        return Ok(false);
    }
    match provider {
        Some(value) => { providers.insert(id.into(), value); }
        None => { providers.remove(id); }
    }
    disk::write_json(&path, &doc)?;
    Ok(true)
}
pub fn save_setup(dir: &Path, req: &PiSetupRequest) -> Result<()> {
    let provider = req.provider_id.trim();
    ensure!(
        !provider.is_empty()
            && provider.len() <= 64
            && provider.as_bytes()[0].is_ascii_alphanumeric()
            && provider
                .bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || b"._-".contains(&c)),
        "invalid provider ID"
    );
    ensure!(!req.api_key.trim().is_empty(), "missing API key");
    let models: Vec<_> = req
        .models
        .iter()
        .flat_map(|s| s.split(','))
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .collect();
    let base = req.base_url.trim_end_matches('/');
    if !base.is_empty() {
        let url = url::Url::parse(base)?;
        ensure!(
            matches!(url.scheme(), "http" | "https") && url.host().is_some(),
            "invalid endpoint"
        );
    }
    ensure!(
        req.preset != "custom" || (!base.is_empty() && !models.is_empty()),
        "custom endpoint and model required"
    );
    disk::directory(dir)?;
    // Keep setup calls ordered across the three files. Native Pi readers share the auth lock.
    let auth_path = dir.join("auth.json");
    let _auth_lock = FileLock::acquire_blocking(&auth_path)?;
    let mut auth = object(&auth_path)?;
    let entry = child(&mut auth, provider)?;
    if entry.get("type").and_then(Value::as_str) == Some("oauth") {
        for field in ["refresh", "access", "expires"] {
            entry.remove(field);
        }
    }
    entry.insert("type".into(), json!("api_key"));
    entry.insert("key".into(), json!(req.api_key.trim()));
    disk::write_json(&auth_path, &auth)?;
    if !base.is_empty() || !models.is_empty() {
        update(dir, "models.json", |doc| {
            let entry = child(child(doc, "providers")?, provider)?;
            if !base.is_empty() {
                entry.insert("baseUrl".into(), json!(base));
            }
            if !req.api.is_empty() {
                entry.insert("api".into(), json!(req.api));
            } else {
                entry.entry("api").or_insert(json!("openai-completions"));
            }
            if !models.is_empty() {
                let list = entry
                    .entry("models")
                    .or_insert_with(|| json!([]))
                    .as_array_mut()
                    .context("expected models array")?;
                for id in &models {
                    if !list.iter().any(|entry| entry["id"].as_str() == Some(id)) {
                        list.push(json!({"id":id,"name":id}));
                    }
                }
            }
            Ok(())
        })?;
    }
    let settings_path = dir.join("settings.json");
    let _settings_lock = FileLock::acquire_blocking(&settings_path)?;
    let mut settings = object(&settings_path)?;
    if req.set_default
        || settings
            .get("defaultProvider")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .trim()
            .is_empty()
    {
        settings.insert("defaultProvider".into(), json!(provider));
        if let Some(model) = models.first() {
            settings.insert("defaultModel".into(), json!(model));
        } else {
            settings.remove("defaultModel");
        }
        disk::write_json(&settings_path, &settings)?;
    }
    Ok(())
}
