//! `vantry_todo_write`: the agent's visible task list. The desktop pins the
//! latest list of a thread above its composer (as Antigravity's task list);
//! the tool itself only validates it and echoes progress back to the model.
use anyhow::{Result, ensure};
use async_trait::async_trait;
use pi_agent::{AgentTool, AgentToolResult, ToolUpdateCallback};
use serde::Deserialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

pub(crate) const NAME: &str = "vantry_todo_write";
const MAX_ITEMS: usize = 40;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct TodoItem {
    #[serde(default)]
    pub content: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub active_form: String,
}

#[derive(Debug, Deserialize)]
pub(crate) struct TodoList {
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub todos: Vec<TodoItem>,
}

/// Checks a list and says where it stands, for the model.
pub(crate) fn summarize(args: &Value) -> Result<String> {
    let list: TodoList = serde_json::from_value(args.clone())
        .map_err(|e| anyhow::anyhow!("invalid todo list: {e}"))?;
    ensure!(!list.todos.is_empty(), "todos must list at least one item");
    ensure!(list.todos.len() <= MAX_ITEMS, "keep the list to {MAX_ITEMS} items or fewer");
    let mut done = 0;
    let mut active = Vec::new();
    for (i, item) in list.todos.iter().enumerate() {
        ensure!(!item.content.trim().is_empty(), "item {} has no content", i + 1);
        match item.status.as_str() {
            "completed" => done += 1,
            "in_progress" => active.push(item.content.trim()),
            "pending" | "" => {}
            other => anyhow::bail!(
                "item {} has status {other:?}; use pending, in_progress or completed",
                i + 1
            ),
        }
    }
    ensure!(
        active.len() <= 1,
        "only one item may be in_progress at a time (found {})",
        active.len()
    );
    let total = list.todos.len();
    let mut out = format!("Task list updated: {done}/{total} completed.");
    if let Some(now) = active.first() {
        out.push_str(&format!(" In progress: {now}."));
    } else if done < total {
        out.push_str(" Mark the next item in_progress before starting it.");
    } else {
        out.push_str(" All items are completed.");
    }
    Ok(out)
}

pub(crate) struct TodoWrite;

#[async_trait]
impl AgentTool for TodoWrite {
    fn name(&self) -> &str {
        NAME
    }
    fn label(&self) -> &str {
        "Vantry Task List"
    }
    fn description(&self) -> &str {
        "Create or replace the visible task list for this conversation. Vantry pins it above the composer so the user can follow progress. Send the complete list every time (it replaces the previous one)."
    }
    fn prompt_snippet(&self) -> Option<&str> {
        Some("Keep a visible task list for multi-step work")
    }
    fn prompt_guidelines(&self) -> Vec<String> {
        vec![
            "For work with three or more distinct steps, call vantry_todo_write first with the full plan, keep exactly one item in_progress while you work on it, mark items completed as soon as they are done, and when the last step is finished call vantry_todo_write once more with every item completed (or removed if no longer relevant) before writing your final reply. Skip it for single-step or conversational requests.".into(),
        ]
    }
    fn parameters(&self) -> Value {
        json!({"type":"object","properties":{
            "title":{"type":"string","description":"Optional short title for the plan"},
            "todos":{"type":"array","maxItems":MAX_ITEMS,"items":{"type":"object","properties":{
                "content":{"type":"string","description":"Imperative description, e.g. 'Run the test suite'"},
                "activeForm":{"type":"string","description":"Present-continuous form shown while in progress, e.g. 'Running the test suite'"},
                "status":{"type":"string","enum":["pending","in_progress","completed"]}
            },"required":["content","status"]}}
        },"required":["todos"]})
    }
    async fn execute(
        &self,
        _: &str,
        args: Value,
        _: CancellationToken,
        _: ToolUpdateCallback,
    ) -> Result<AgentToolResult> {
        let text = summarize(&args)?;
        Ok(AgentToolResult::text(text))
    }
}

#[cfg(test)]
mod tests {
    use super::summarize;
    use serde_json::json;

    #[test]
    fn validates_and_summarizes_progress() {
        let ok = summarize(&json!({"todos":[
            {"content":"读代码","status":"completed"},
            {"content":"改图表","status":"in_progress"},
            {"content":"发版本","status":"pending"}
        ]}))
        .unwrap();
        assert!(ok.contains("1/3") && ok.contains("改图表"), "{ok}");
        let done = summarize(&json!({"todos":[{"content":"a","status":"completed"}]})).unwrap();
        assert!(done.contains("All items are completed"));
        assert!(summarize(&json!({"todos":[]})).is_err());
        assert!(summarize(&json!({"todos":[{"content":"a","status":"done"}]})).is_err());
        assert!(
            summarize(&json!({"todos":[
                {"content":"a","status":"in_progress"},
                {"content":"b","status":"in_progress"}
            ]}))
            .is_err()
        );
        assert!(summarize(&json!({"todos":[{"content":"  ","status":"pending"}]})).is_err());
    }
}
