//! Validation for the declarative v1 native widget schema.
use anyhow::{Context, Result, bail, ensure};
use serde_json::Value;
use std::collections::HashSet;
const TEXT: usize = 256 << 10;
fn fields(value: &Value, allowed: &[&str]) -> Result<()> {
    let map = value.as_object().context("expected an object")?;
    for k in map.keys() {
        ensure!(allowed.contains(&k.as_str()), "unknown panel field {k}");
    }
    Ok(())
}
fn string<'a>(v: &'a Value, key: &str, limit: usize) -> Result<Option<&'a str>> {
    v.get(key)
        .map(|value| {
            let s = value.as_str().context("expected string")?;
            ensure!(s.len() <= limit, "panel string too long");
            Ok(s)
        })
        .transpose()
}
fn either<'a>(v: &'a Value, a: &str, b: &str) -> Result<&'a Value> {
    match (v.get(a), v.get(b)) {
        (Some(val), None) | (None, Some(val)) => Ok(val),
        _ => bail!("provide exactly one of {a}, {b}"),
    }
}
#[derive(Default)]
struct Tree {
    nodes: usize,
    keys: HashSet<String>,
}
impl Tree {
    fn count(&mut self, n: usize) -> Result<()> {
        self.nodes += n;
        ensure!(self.nodes <= 128, "too many panel nodes");
        Ok(())
    }
    fn widget(&mut self, item: &Value, depth: u8, fallback: String) -> Result<()> {
        ensure!(depth <= 6, "widgets nested too deeply");
        self.count(1)?;
        let key = string(item, "key", 128)?.unwrap_or(&fallback);
        ensure!(
            !key.trim().is_empty() && self.keys.insert(key.into()),
            "blank or duplicate widget key"
        );
        let kind = item["kind"].as_str().context("missing widget kind")?;
        let allowed: &[&str] = match kind {
            "text" => &["kind", "key", "title", "text", "tone"],
            "markdown" => &["kind", "key", "title", "markdown", "text"],
            "badge" => &["kind", "key", "label", "text", "tone"],
            "code" => &["kind", "key", "title", "code", "text", "language"],
            "kv" => &["kind", "key", "title", "entries", "items"],
            "quotaCard" => &["kind", "key", "quota"],
            "meter" => &["kind", "key", "title", "text", "percent", "detail"],
            "modelCatalog" => &["kind", "key", "title", "text"],
            "list" => &["kind", "key", "title", "items", "children"],
            _ => bail!("unknown widget kind {kind}"),
        };
        fields(item, allowed)?;
        string(item, "title", 256)?;
        if let Some(tone) = string(item, "tone", 32)? {
            ensure!(
                matches!(
                    tone,
                    "" | "default"
                        | "neutral"
                        | "muted"
                        | "info"
                        | "accent"
                        | "success"
                        | "warning"
                        | "danger"
                ),
                "unknown badge tone"
            );
        }
        match kind {
            "quotaCard" => {
                let card = &item["quota"];
                fields(card, &["account", "plan", "expires", "updated", "message", "windows", "models"])?;
                ensure!(card["models"].is_null() || card["models"].is_u64(), "expected a model count");
                for key in ["account", "plan", "expires", "updated", "message"] { string(card, key, 1024)?; }
                let windows = card["windows"].as_array().context("expected quota windows")?;
                self.count(windows.len())?;
                for w in windows {
                    fields(w, &["name", "amount", "limit", "unit", "used", "display", "reset"])?;
                    for key in ["name", "unit", "display", "reset"] { string(w, key, 256)?; }
                    for key in ["amount", "limit", "used"] { ensure!(w[key].is_null() || w[key].is_number(), "expected numeric quota value"); }
                }
            }
            "meter" => {
                ensure!(item["percent"].as_u64().is_some_and(|v| v <= 100), "invalid meter percentage");
                string(item, "detail", 256)?;
                ensure!(string(item, "text", TEXT)?.is_some(), "missing meter value");
            }
            "text" | "modelCatalog" => ensure!(
                string(item, "text", TEXT)?.is_some_and(|s| !s.is_empty()),
                "empty text"
            ),
            "markdown" | "badge" | "code" => {
                let field = if kind == "badge" { "label" } else { kind };
                let text = either(item, field, "text")?
                    .as_str()
                    .context("invalid widget text")?;
                ensure!(
                    !text.is_empty() && text.len() <= TEXT,
                    "invalid content length"
                );
                if kind == "badge" {
                    ensure!(
                        text.len() <= 256 && !text.trim().is_empty(),
                        "invalid badge"
                    );
                }
                if let Some(lang) = string(item, "language", 64)? {
                    ensure!(
                        !lang.chars().any(|c| c.is_control() || c.is_whitespace()),
                        "invalid language tag"
                    );
                }
            }
            "kv" => {
                let entries = either(item, "entries", "items")?
                    .as_array()
                    .context("expected key/value rows")?;
                ensure!(!entries.is_empty(), "empty kv");
                self.count(entries.len())?;
                for e in entries {
                    fields(e, &["key", "value"])?;
                    ensure!(
                        string(e, "key", 256)?.is_some_and(|s| !s.trim().is_empty())
                            && string(e, "value", TEXT)?.is_some(),
                        "invalid kv row"
                    );
                }
            }
            "list" => {
                let items = either(item, "items", "children")?
                    .as_array()
                    .context("expected list items")?;
                if item.get("children").is_some() || items.first().is_some_and(Value::is_object) {
                    for (i, v) in items.iter().enumerate() {
                        self.widget(v, depth + 1, format!("{key}.{i}"))?;
                    }
                } else {
                    self.count(items.len())?;
                    ensure!(
                        items
                            .iter()
                            .all(|v| v.as_str().is_some_and(|s| s.len() <= TEXT)),
                        "invalid list items"
                    );
                }
            }
            _ => unreachable!(),
        }
        Ok(())
    }
}
pub fn validate_panel(value: &Value) -> Result<()> {
    fields(value, &["vantryPanel"])?;
    let panel = &value["vantryPanel"];
    fields(panel, &["version", "title", "widgets"])?;
    ensure!(panel["version"] == 1, "unsupported panel version");
    string(panel, "title", 256)?;
    let widgets = panel["widgets"].as_array().context("missing widgets")?;
    ensure!(!widgets.is_empty(), "empty panel");
    let mut tree = Tree::default();
    for (i, w) in widgets.iter().enumerate() {
        tree.widget(w, 1, format!("w-{i}"))?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn nested_list_keys_and_content() {
        let panel = |widgets| json!({"vantryPanel":{"version":1,"widgets":widgets}});
        assert!(validate_panel(&panel(json!([{"kind":"list","items":[{"kind":"markdown","markdown":"ok"},{"kind":"kv","entries":[{"key":"K","value":"v"}]}]}]))).is_ok());
        for widgets in [
            json!([]),
            json!([{"kind":"text","text":"x","html":true}]),
            json!([{"kind":"list","children":[{"kind":"text","key":"w-0","text":"duplicate"}]}]),
            json!([{"kind":"badge","label":"a","text":"duplicate"}]),
        ] {
            assert!(validate_panel(&panel(widgets)).is_err());
        }
    }
}
