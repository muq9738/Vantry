//! UI projections of Pi's authenticated model registry. Credentials, URLs and headers stay local.
use anyhow::{Context, Result, ensure};
use pi_coding_agent::{
    agent_session::{ModelRuntime, ModelRuntimeOptions},
    model_resolver::{InitialModelOptions, find_initial_model},
};
use serde::Deserialize;
use serde_json::Value;
use std::{
    collections::{BTreeMap, HashSet},
    fs::File,
    path::Path,
};
use vantry_protocol::{ModelCatalog, ModelInfo};

#[derive(Debug, Default)]
pub struct MagpieSelection(Vec<String>);
impl MagpieSelection {
    pub fn load(path: Option<&Path>) -> Result<Self> {
        let default = pi_coding_agent::config::home_dir().join(".config/magpie/settings.json");
        #[derive(Deserialize)]
        struct Selection {
            #[serde(default, rename = "pickedModels")]
            picked: BTreeMap<String, Vec<String>>,
        }
        let mut settings: Selection =
            serde_json::from_reader(File::open(path.unwrap_or(&default))?)
                .map_err(|_| anyhow::anyhow!("invalid Magpie settings"))?;
        let mut seen = HashSet::new();
        Ok(Self(
            settings
                .picked
                .remove("pi")
                .unwrap_or_default()
                .iter()
                .map(|id| id.trim())
                .filter(|id| !id.is_empty() && seen.insert(*id))
                .map(|id| format!("magpie/{id}"))
                .collect(),
        ))
    }
    pub fn resolve(&self, requested: &str) -> Result<&str> {
        self.0
            .iter()
            .find(|s| *s == requested)
            .or_else(|| {
                self.0
                    .iter()
                    .filter(|s| {
                        requested
                            .strip_prefix(s.as_str())
                            .and_then(|v| v.strip_prefix(':'))
                            .is_some_and(|v| !v.is_empty())
                    })
                    .max_by_key(|s| s.len())
            })
            .map(String::as_str)
            .context("model outside Magpie selection")
    }
}
fn key(provider: &str, id: &str) -> String {
    format!(
        "{provider}/{}",
        id.strip_prefix(&format!("{provider}/")).unwrap_or(id)
    )
}

/// Split an optional `:thinking_level` suffix (e.g. `openai/o3:high`) from a model spec.
pub fn split_model_thinking(spec: &str) -> (&str, Option<pi_ai::ThinkingLevel>) {
    if let Some((base, suffix)) = spec.rsplit_once(':')
        && !base.is_empty()
        && let Ok(level) = suffix.parse::<pi_ai::ThinkingLevel>()
    {
        return (base, Some(level));
    }
    (spec, None)
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SkillInfo {
    pub name: String,
    pub description: String,
    pub file_path: String,
    pub source: String,
    pub command: String,
}

/// Discover skills actually visible to embedded Pi for `(agent_dir, cwd)`.
pub fn discover_skills(agent_dir: &Path, cwd: &Path) -> Vec<SkillInfo> {
    use pi_coding_agent::resources::skills::{LoadSkillsOptions, load_skills};
    let loaded = load_skills(&LoadSkillsOptions {
        cwd: cwd.to_path_buf(),
        agent_dir: agent_dir.to_path_buf(),
        skill_paths: Vec::new(),
        include_defaults: true,
    });
    let mut skills: Vec<SkillInfo> = loaded
        .skills
        .into_iter()
        .filter(|s| !s.disable_model_invocation)
        .map(|s| {
            let source = format!("{:?}", s.source_info.scope).to_ascii_lowercase();
            let command = format!("skill:{}", s.name);
            SkillInfo {
                name: s.name,
                description: s.description,
                file_path: s.file_path.display().to_string(),
                source,
                command,
            }
        })
        .collect();
    skills.sort_by(|a, b| a.name.cmp(&b.name));
    skills
}

#[derive(Debug, Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ResolvedModelCatalog {
    pub catalog: ModelCatalog,
    pub selected_model: String,
    pub thinking_level: Option<pi_ai::ThinkingLevel>,
}

pub async fn model_catalog(
    dir: &Path,
    selected: Option<&str>,
    magpie: Option<&MagpieSelection>,
) -> Result<ModelCatalog> {
    Ok(resolve_model_catalog(dir, selected, magpie).await?.catalog)
}

pub async fn resolve_model_catalog(
    dir: &Path,
    selected: Option<&str>,
    magpie: Option<&MagpieSelection>,
) -> Result<ResolvedModelCatalog> {
    let settings = crate::config::object(&dir.join("settings.json"))
        .map_err(|_| anyhow::anyhow!("invalid Pi settings"))?;
    let provider = settings
        .get("defaultProvider")
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty());
    let model = settings
        .get("defaultModel")
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty());
    let configured_default = provider
        .zip(model)
        .map(|(p, id)| key(p, id))
        .unwrap_or_default();
    let selected = selected.filter(|s| !s.is_empty());
    if let Some(selection) = magpie {
        if let Some(s) = selected {
            selection.resolve(s)?;
        }
        if selection.0.is_empty() {
            return Ok(ResolvedModelCatalog {
                selected_model: String::new(),
                thinking_level: None,
                catalog: ModelCatalog {
                    models: Some(vec![]),
                    configured_default,
                    warnings: vec!["尚未在 Magpie 中为 Pi 选择模型".into()],
                    ..Default::default()
                },
            });
        }
    }
    let runtime = ModelRuntime::create(ModelRuntimeOptions::for_agent_dir(dir)).await;
    ensure!(
        runtime.load_error().is_none(),
        "invalid Pi model configuration"
    );
    let initial = find_initial_model(
        &InitialModelOptions {
            default_provider: provider,
            default_model_id: configured_default.split_once('/').map(|(_, id)| id),
            ..Default::default()
        },
        &runtime,
    )
    .map_err(|_| anyhow::anyhow!("invalid default model"))?;
    let mut default = initial
        .model
        .map(|m| format!("{}/{}", m.provider, m.id))
        .unwrap_or_default();
    let mut available: BTreeMap<_, _> = runtime
        .available_models()
        .into_iter()
        .filter(|m| m.model_type.as_deref().is_none_or(|t| t == "chat"))
        .map(|m| (format!("{}/{}", m.provider, m.id), m))
        .collect();
    let mut order = vec![];
    let mut warnings = vec![];
    if let Some(selection) = magpie {
        order.extend(
            selection
                .0
                .iter()
                .filter(|s| available.contains_key(*s))
                .cloned(),
        );
        if order.len() != selection.0.len() || order.is_empty() {
            warnings.push("Magpie 已选模型尚未就绪".into());
        }
        if !order.contains(&default) {
            default.clear();
        }
    } else {
        let models = crate::config::object(&dir.join("models.json"))
            .map_err(|_| anyhow::anyhow!("invalid Pi model configuration"))?;
        let providers: BTreeMap<_, _> = models
            .get("providers")
            .and_then(Value::as_object)
            .into_iter()
            .flatten()
            .collect();
        let mut seen = HashSet::new();
        for (p, value) in providers {
            for m in value["models"].as_array().into_iter().flatten() {
                if let Some(id) = m["id"].as_str() {
                    let k = key(p, id);
                    if available.contains_key(&k) && seen.insert(k.clone()) {
                        order.push(k);
                    }
                }
            }
        }
        if order.is_empty() {
            order.extend(available.keys().cloned());
            order.sort_by_key(|s| s.to_lowercase());
        }
    }
    if !configured_default.is_empty() && !order.contains(&configured_default) {
        warnings.push("Pi 默认模型当前不可用".into());
    }
    let chosen = match selected {
        Some(s) if order.iter().any(|id| id == s) => s,
        Some(s) => {
            if let Some(selection) = magpie {
                selection.resolve(s)?
            } else {
                s.rsplit_once(':')
                    .filter(|(_, level)| {
                        matches!(
                            *level,
                            "off" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max"
                        )
                    })
                    .map_or(s, |(id, _)| id)
            }
        }
        None => default.as_str(),
    };
    ensure!(
        selected.is_none() || order.iter().any(|id| id == chosen),
        "selected model unavailable"
    );
    let thinking_levels = available
        .get(chosen)
        .map(|m| {
            pi_ai::simple_options::supported_thinking_levels(m)
                .iter()
                .map(|level| level.as_str().to_owned())
                .collect()
        })
        .unwrap_or_default();
    let thinking_level = available.get(chosen).map(|m| {
        let suffix = selected
            .and_then(|s| s.strip_prefix(chosen))
            .and_then(|s| s.strip_prefix(':'));
        let model_default = settings
            .get("modelThinkingLevels")
            .and_then(|v| v[chosen].as_str());
        let global_default = settings.get("defaultThinkingLevel").and_then(Value::as_str);
        let level = [suffix, model_default, global_default]
            .into_iter()
            .flatten()
            .find_map(|v| v.parse().ok())
            .unwrap_or(pi_coding_agent::settings::DEFAULT_THINKING_LEVEL);
        pi_ai::simple_options::clamp_thinking_level(m, level)
    });
    let selected_model = chosen.to_owned();
    let models = order
        .iter()
        .filter_map(|k| available.remove(k))
        .map(|m| ModelInfo {
            id: m.id,
            provider: m.provider,
            name: m.name,
            reasoning: m.reasoning,
            input: Some(m.input),
            context_window: m.context_window as i64,
            max_tokens: m.max_tokens as i64,
        })
        .collect();
    Ok(ResolvedModelCatalog {
        selected_model,
        thinking_level,
        catalog: ModelCatalog {
            models: Some(models),
            default,
            configured_default,
            thinking_levels,
            warnings,
        },
    })
}
