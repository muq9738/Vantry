//! The agent's task list (`vantry_todo_write`), pinned above the composer as
//! Antigravity shows its task list. The latest call of a thread wins.

use vantry_protocol::ToolCall;

/// The tool that writes the list (also accepted: common agent spellings).
pub const TODO_TOOLS: [&str; 4] = ["vantry_todo_write", "todo_write", "todowrite", "update_plan"];

pub fn is_todo_tool(name: &str) -> bool {
    let name = name.to_ascii_lowercase();
    TODO_TOOLS.contains(&name.as_str())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TodoStatus {
    Pending,
    InProgress,
    Completed,
}

#[derive(Debug, Clone, PartialEq)]
pub struct TodoEntry {
    pub content: String,
    /// What the panel shows while the item runs ("Running tests").
    pub active_form: String,
    pub status: TodoStatus,
}

impl TodoEntry {
    pub fn label(&self) -> &str {
        if self.status == TodoStatus::InProgress && !self.active_form.trim().is_empty() {
            self.active_form.trim()
        } else {
            self.content.trim()
        }
    }
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct TodoList {
    /// The call it came from (the panel resets its folding on a new one).
    pub call_id: String,
    pub run_id: String,
    pub title: String,
    pub items: Vec<TodoEntry>,
}

impl TodoList {
    pub fn done(&self) -> usize {
        self.items.iter().filter(|i| i.status == TodoStatus::Completed).count()
    }

    pub fn all_done(&self) -> bool {
        !self.items.is_empty() && self.done() == self.items.len()
    }

    pub fn current(&self) -> Option<&TodoEntry> {
        self.items.iter().find(|i| i.status == TodoStatus::InProgress)
    }

    /// `2/5`.
    pub fn progress(&self) -> String {
        format!("{}/{}", self.done(), self.items.len())
    }
}

/// Reads one call's arguments: `{todos:[{content,status,activeForm}]}`, or
/// Codex's `update_plan` `{plan:[{step,status}]}`.
pub fn parse(call: &ToolCall) -> Option<TodoList> {
    let args = call.arguments.as_ref()?;
    let items = args.get("todos").or_else(|| args.get("plan"))?.as_array()?;
    let items: Vec<TodoEntry> = items
        .iter()
        .filter_map(|item| {
            let content = item
                .get("content")
                .or_else(|| item.get("step"))
                .and_then(|v| v.as_str())?
                .trim()
                .to_owned();
            if content.is_empty() {
                return None;
            }
            let status = match item.get("status").and_then(|v| v.as_str()).unwrap_or("") {
                "completed" | "done" | "complete" => TodoStatus::Completed,
                "in_progress" | "in-progress" | "active" | "running" => TodoStatus::InProgress,
                _ => TodoStatus::Pending,
            };
            Some(TodoEntry {
                content,
                active_form: item
                    .get("activeForm")
                    .or_else(|| item.get("active_form"))
                    .and_then(|v| v.as_str())
                    .unwrap_or("")
                    .to_owned(),
                status,
            })
        })
        .collect();
    if items.is_empty() {
        return None;
    }
    Some(TodoList {
        call_id: call.id.clone(),
        run_id: call.run_id.clone(),
        title: args
            .get("title")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_owned(),
        items,
    })
}

/// The newest list `thread` has, from a failed call never.
pub fn latest<'a>(tools: impl DoubleEndedIterator<Item = &'a ToolCall>, thread: &str) -> Option<TodoList> {
    tools
        .rev()
        .filter(|t| t.thread_id == thread && is_todo_tool(&t.name) && t.status != "failed")
        .find_map(parse)
}

/// The timeline's one-line summary of a todo call.
pub fn summary(arguments: Option<&serde_json::Value>) -> Option<String> {
    let call = ToolCall {
        arguments: arguments.cloned(),
        ..ToolCall::default()
    };
    let list = parse(&call)?;
    Some(match list.current() {
        Some(now) => format!("{} · {}", list.progress(), now.label()),
        None => list.progress(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn call(id: &str, thread: &str, status: &str, args: serde_json::Value) -> ToolCall {
        ToolCall {
            id: id.into(),
            thread_id: thread.into(),
            run_id: "r".into(),
            name: "vantry_todo_write".into(),
            arguments: Some(args),
            status: status.into(),
            ..ToolCall::default()
        }
    }

    #[test]
    fn latest_list_of_the_thread_wins() {
        let tools = vec![
            call("1", "t", "completed", json!({"todos":[{"content":"读代码","status":"in_progress"}]})),
            call("2", "other", "completed", json!({"todos":[{"content":"别的","status":"pending"}]})),
            call(
                "3",
                "t",
                "completed",
                json!({"title":"发布","todos":[
                    {"content":"读代码","status":"completed"},
                    {"content":"改图表","activeForm":"正在改图表","status":"in_progress"},
                    {"content":"发版本","status":"pending"}
                ]}),
            ),
            call("4", "t", "failed", json!({"todos":[{"content":"坏的","status":"pending"}]})),
        ];
        let list = latest(tools.iter(), "t").unwrap();
        assert_eq!(list.call_id, "3");
        assert_eq!(list.title, "发布");
        assert_eq!(list.progress(), "1/3");
        assert_eq!(list.current().unwrap().label(), "正在改图表");
        assert!(!list.all_done());
        assert!(latest(tools.iter(), "none").is_none());
    }

    #[test]
    fn reads_codex_plans_and_summaries() {
        let plan = call(
            "p",
            "t",
            "completed",
            json!({"plan":[{"step":"a","status":"completed"},{"step":"b","status":"completed"}]}),
        );
        let list = parse(&plan).unwrap();
        assert!(list.all_done());
        assert_eq!(summary(plan.arguments.as_ref()).unwrap(), "2/2");
        assert!(summary(Some(&json!({"todos":[]}))).is_none());
        assert!(is_todo_tool("TodoWrite") && is_todo_tool("update_plan") && !is_todo_tool("bash"));
    }
}
