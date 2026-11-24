//! Intents beyond sending: thread management (rename, pin, snooze,
//! archive, delete, fork, rewind, merge back, model, permissions), the
//! queue (edit, reorder, promote to a steer, cancel) and attachments.
//!
//! Field changes are optimistic: the thread shows the change at once, the
//! previous value comes back when the Core refuses it, and an unknown
//! outcome keeps it with a retry of the same command.

use vantry_protocol::{Attachment, Command, CommandKind, Input, RunStatus};

use super::{AppState, Draft, Notice, NoticeLevel, Purpose, QueueEdit};
use crate::bridge::new_command_id;

/// What a queued row is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum QueueItemKind {
    /// Waits for the run to end (`message.queue`).
    Queued,
    /// A steer not yet confirmed delivered.
    Steer,
}

/// A row of the composer's queue.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct QueueItem {
    pub id: String,
    pub kind: QueueItemKind,
    pub text: String,
    /// Its place among the queued (0 first).
    pub index: usize,
    pub count: usize,
    pub attachments: usize,
    /// What it is waiting for ("排队中", "追加中", "交付不明").
    pub status: String,
    /// A command about it is on its way.
    pub busy: bool,
    pub editing: bool,
}

fn thread_command(kind: CommandKind, prefix: &str, thread_id: &str) -> Command {
    let mut command = Command::new(new_command_id(prefix), kind);
    command.thread_id = thread_id.to_owned();
    command
}

impl AppState {
    // ---------------------------------------------------------------- threads

    /// Applies `change` to thread `id` now and sends `command`; the thread
    /// comes back as it was if the Core refuses.
    fn update_thread(
        &mut self,
        id: &str,
        field: &'static str,
        command: Command,
        change: impl FnOnce(&mut vantry_protocol::Thread),
    ) -> Option<Command> {
        let thread = self.threads.iter_mut().find(|t| t.id == id)?;
        let previous = Box::new(thread.clone());
        change(thread);
        self.sidebar_revision += 1;
        Some(self.track(
            format!("thread:{id}:{field}"),
            command,
            Purpose::ThreadUpdate { previous, field },
        ))
    }

    pub fn rename_thread(&mut self, id: &str, title: &str) -> Option<Command> {
        let title = title.trim();
        if title.is_empty() || self.thread(id)?.title == title {
            return None;
        }
        let mut command = thread_command(CommandKind::UpdateThread, "rename", id);
        command.title = title.to_owned();
        let title = title.to_owned();
        self.update_thread(id, "title", command, move |t| t.title = title)
    }

    pub fn set_pinned(&mut self, id: &str, pinned: bool) -> Option<Command> {
        let mut command = thread_command(CommandKind::UpdateThread, "pin", id);
        command.pinned = Some(pinned);
        self.update_thread(id, "pin", command, move |t| t.pinned = pinned)
    }

    pub fn set_archived(&mut self, id: &str, archived: bool) -> Option<Command> {
        let mut command = thread_command(CommandKind::UpdateThread, "archive", id);
        command.archived = Some(archived);
        let command = self.update_thread(id, "archive", command, move |t| t.archived = archived);
        if archived && self.selected.as_deref() == Some(id) {
            self.selected = self.visible_threads().first().map(|t| t.id.clone());
        }
        command
    }

    /// Snoozes thread `id` until `until` (RFC 3339), or wakes it (`None`).
    pub fn snooze(&mut self, id: &str, until: Option<String>) -> Option<Command> {
        let mut command = thread_command(CommandKind::UpdateThread, "snooze", id);
        command.snoozed_until = Some(until.clone().unwrap_or_default());
        self.update_thread(id, "snooze", command, move |t| t.snoozed_until = until)
    }

    /// The approval policy of the shown thread, or of new threads.
    pub fn set_policy(&mut self, policy: &str) -> Option<Command> {
        let Some(id) = self.selected.clone() else {
            self.new_thread.approval_policy = policy.to_owned();
            return None;
        };
        if self.thread(&id)?.approval_policy == policy {
            return None;
        }
        let mut command = thread_command(CommandKind::UpdateThread, "policy", &id);
        command.approval_policy = policy.to_owned();
        let policy = policy.to_owned();
        self.update_thread(&id, "policy", command, move |t| t.approval_policy = policy)
    }

    /// The model of the shown thread's next messages (`thread.model`), or of
    /// new threads. Runs already started keep theirs.
    pub fn set_model(&mut self, model: &str) -> Option<Command> {
        let model = model.trim();
        if model.is_empty() {
            return None;
        }
        let Some(id) = self.selected.clone() else {
            self.new_thread.model = model.to_owned();
            return None;
        };
        if self.thread(&id)?.model == model {
            return None;
        }
        let mut command = thread_command(CommandKind::SetThreadModel, "model", &id);
        command.model = model.to_owned();
        command.provider_id = self.thread(&id)?.provider_id.clone();
        let model = model.to_owned();
        self.update_thread(&id, "model", command, move |t| t.model = model)
    }

    /// Deletes thread `id` (once the Core confirms, its event removes it).
    pub fn delete_thread(&mut self, id: &str) -> Option<Command> {
        self.thread(id)?;
        if self.active_run(id).is_some() {
            self.notice = Some(Notice::new(
                NoticeLevel::Warn,
                "会话仍在运行，请先停止再删除。",
            ));
            return None;
        }
        let key = format!("thread:{id}:delete");
        if self.in_flight(&key) {
            return None;
        }
        let command = thread_command(CommandKind::DeleteThread, "delete", id);
        Some(self.track(key, command, Purpose::Delete))
    }

    /// Forks thread `id` after run `run_id` (its latest finished run when
    /// `None`) into a new thread, shown once the Core makes it.
    pub fn fork(&mut self, id: &str, run_id: Option<&str>) -> Option<Command> {
        let thread = self.thread(id)?.clone();
        let run = match run_id {
            Some(run_id) => self.run(run_id)?.clone(),
            None => self
                .thread_runs(id)
                .into_iter()
                .rev()
                .find(|r| r.status.terminal())?
                .clone(),
        };
        let key = format!("thread:{id}:fork");
        if self.in_flight(&key) {
            return None;
        }
        let mut command = thread_command(CommandKind::ForkThread, "fork", id);
        command.run_id = run.id;
        command.title = format!("{}（分叉）", thread.title);
        command.provider_id = thread.provider_id.clone();
        command.model = thread.model.clone();
        Some(self.track(key, command, Purpose::OpenResult))
    }

    /// Rewinds thread `id`: run `run_id` and every later run go.
    pub fn rewind(&mut self, id: &str, run_id: &str) -> Option<Command> {
        let run = self.run(run_id)?;
        if run.thread_id != id {
            return None;
        }
        if self.active_run(id).is_some() {
            self.notice = Some(Notice::new(
                NoticeLevel::Warn,
                "会话仍在运行，请先停止再回退。",
            ));
            return None;
        }
        let key = format!("thread:{id}:rewind");
        if self.in_flight(&key) {
            return None;
        }
        let mut command = thread_command(CommandKind::RewindThread, "rewind", id);
        command.run_id = run_id.to_owned();
        Some(self.track(key, command, Purpose::Rewind))
    }

    /// Whether thread `id` can merge back into the thread it was forked from.
    pub fn can_merge_back(&self, id: &str) -> bool {
        self.thread(id).is_some_and(|t| {
            !t.forked_from_thread_id.is_empty() && self.thread(&t.forked_from_thread_id).is_some()
        })
    }

    /// Merges fork `id` back into its source; the source is shown.
    pub fn merge_back(&mut self, id: &str) -> Option<Command> {
        if !self.can_merge_back(id) {
            return None;
        }
        let key = format!("thread:{id}:merge");
        if self.in_flight(&key) {
            return None;
        }
        let command = thread_command(CommandKind::MergeBackThread, "merge", id);
        Some(self.track(key, command, Purpose::OpenResult))
    }

    // ------------------------------------------------------------------ queue

    /// The shown thread's queued messages and undelivered steers, in order.
    pub fn queue_items(&self) -> Vec<QueueItem> {
        let Some(thread_id) = self.selected.as_deref() else {
            return Vec::new();
        };
        let mut queued: Vec<&Input> = self
            .inputs
            .iter()
            .filter(|i| i.thread_id == thread_id && i.kind == "queue" && i.status == "pending")
            .collect();
        queued.sort_by_key(|i| (i.position, i.created_at.clone()));
        let count = queued.len();
        let editing = self.queue_edit.as_ref().map(|e| e.input_id.as_str());
        let mut items: Vec<QueueItem> = queued
            .iter()
            .enumerate()
            .map(|(index, input)| QueueItem {
                id: input.id.clone(),
                kind: QueueItemKind::Queued,
                text: input.message.clone(),
                index,
                count,
                attachments: input.attachments.len(),
                status: "排队中".to_owned(),
                busy: self.busy(&format!("queue:{}:", input.id)),
                editing: editing == Some(input.id.as_str()),
            })
            .collect();
        items.extend(
            self.inputs
                .iter()
                .filter(|i| {
                    i.thread_id == thread_id
                        && i.kind == "steer"
                        && matches!(i.status.as_str(), "pending" | "sending" | "unknown")
                })
                .map(|input| QueueItem {
                    id: input.id.clone(),
                    kind: QueueItemKind::Steer,
                    text: input.message.clone(),
                    index: 0,
                    count: 0,
                    attachments: input.attachments.len(),
                    status: if input.status == "unknown" {
                        "交付不明（不会自动重发）".to_owned()
                    } else {
                        "追加中".to_owned()
                    },
                    busy: false,
                    editing: false,
                }),
        );
        items
    }

    fn queued_input(&self, input_id: &str) -> Option<Input> {
        self.inputs
            .iter()
            .find(|i| i.id == input_id && i.kind == "queue" && i.status == "pending")
            .cloned()
    }

    /// Opens queued message `input_id` in the composer; the draft there
    /// comes back when the edit ends.
    pub fn begin_queue_edit(&mut self, input_id: &str) {
        let Some(input) = self.queued_input(input_id) else {
            return;
        };
        if self.queue_edit.is_some() {
            self.end_queue_edit();
        }
        self.select_thread(Some(input.thread_id.clone()));
        let key = self.draft_key();
        let saved = self.drafts.remove(&key).unwrap_or_default();
        self.drafts.insert(
            key,
            Draft {
                text: input.message.clone(),
                mode: saved.mode,
                attachments: input.attachments.clone(),
                ..Default::default()
            },
        );
        self.queue_edit = Some(QueueEdit {
            input_id: input.id,
            thread_id: input.thread_id,
            saved,
        });
    }

    /// Ends the edit without saving: the earlier draft comes back.
    pub fn end_queue_edit(&mut self) {
        if let Some(edit) = self.queue_edit.take() {
            self.drafts.insert(edit.thread_id, edit.saved);
        }
    }

    /// Saves the edited queued message (`input.edit`).
    pub(super) fn save_queue_edit(&mut self) -> Option<Command> {
        let edit = self.queue_edit.clone()?;
        let draft = self.drafts.get(&edit.thread_id).cloned().unwrap_or_default();
        let text = draft.outgoing().trim().to_owned();
        let Some(input) = self.queued_input(&edit.input_id) else {
            self.end_queue_edit();
            self.notice = Some(Notice::new(
                NoticeLevel::Warn,
                "这条排队消息已开始或已取消。",
            ));
            return None;
        };
        self.end_queue_edit();
        if text.is_empty() || (text == input.message && draft.attachments == input.attachments) {
            return None;
        }
        let mut command = thread_command(CommandKind::EditInput, "edit", &input.thread_id);
        command.input_id = input.id.clone();
        command.message = text.clone();
        command.attachments = draft.attachments.clone();
        if let Some(i) = self.inputs.iter_mut().find(|i| i.id == input.id) {
            i.message = text;
            i.attachments = draft.attachments;
        }
        Some(self.track(
            format!("queue:{}:edit", input.id),
            command,
            Purpose::Queue {
                previous: vec![input],
            },
        ))
    }

    /// Moves queued message `input_id` by `delta` places (`input.reorder`
    /// with the zero-based destination).
    pub fn move_queued(&mut self, input_id: &str, delta: i64) -> Option<Command> {
        let input = self.queued_input(input_id)?;
        let mut queue: Vec<Input> = self
            .inputs
            .iter()
            .filter(|i| i.thread_id == input.thread_id && i.kind == "queue" && i.status == "pending")
            .cloned()
            .collect();
        queue.sort_by_key(|i| (i.position, i.created_at.clone()));
        let from = queue.iter().position(|i| i.id == input_id)?;
        let to = (from as i64 + delta).clamp(0, queue.len() as i64 - 1) as usize;
        if to == from {
            return None;
        }
        let previous = queue.clone();
        let moved = queue.remove(from);
        queue.insert(to, moved);
        for (position, item) in queue.iter().enumerate() {
            if let Some(i) = self.inputs.iter_mut().find(|i| i.id == item.id) {
                i.position = position as i64;
            }
        }
        let mut command = thread_command(CommandKind::ReorderInput, "reorder", &input.thread_id);
        command.input_id = input_id.to_owned();
        command.position = to as i64;
        Some(self.track(
            format!("queue:{input_id}:reorder"),
            command,
            Purpose::Queue { previous },
        ))
    }

    /// Hands queued message `input_id` to the running attempt now.
    pub fn promote_queued(&mut self, input_id: &str) -> Option<Command> {
        let input = self.queued_input(input_id)?;
        let running = self
            .active_run(&input.thread_id)
            .is_some_and(|r| r.status == RunStatus::Running);
        if !running {
            self.notice = Some(Notice::new(
                NoticeLevel::Warn,
                "只有运行中的任务可以接收追加。",
            ));
            return None;
        }
        let mut command =
            thread_command(CommandKind::PromoteInputToSteer, "promote", &input.thread_id);
        command.input_id = input_id.to_owned();
        Some(self.track(
            format!("queue:{input_id}:promote"),
            command,
            Purpose::Queue {
                previous: Vec::new(),
            },
        ))
    }

    /// Cancels queued message `input_id`.
    pub fn cancel_queued(&mut self, input_id: &str) -> Option<Command> {
        let input = self.queued_input(input_id)?;
        if self.queue_edit.as_ref().is_some_and(|e| e.input_id == input_id) {
            self.end_queue_edit();
        }
        let mut command = thread_command(CommandKind::CancelInput, "cancel", &input.thread_id);
        command.input_id = input_id.to_owned();
        Some(self.track(
            format!("queue:{input_id}:cancel"),
            command,
            Purpose::Queue {
                previous: Vec::new(),
            },
        ))
    }

    // ------------------------------------------------------------ attachments

    /// Adds `attachment` to the shown draft (once).
    pub fn add_attachment(&mut self, attachment: Attachment) {
        self.add_attachment_to(self.draft_key(), attachment, None);
    }

    pub fn add_attachment_to(
        &mut self,
        key: String,
        attachment: Attachment,
        path: Option<std::path::PathBuf>,
    ) {
        let draft = self.drafts.entry(key).or_default();
        if !draft.attachments.iter().any(|a| a.id == attachment.id) {
            if let Some(path) = path
                && attachment.mime.starts_with("image/")
            {
                draft.image_paths.insert(attachment.id.clone(), path);
            }
            draft.attachments.push(attachment);
        }
    }

    pub fn remove_attachment(&mut self, id: &str) {
        let key = self.draft_key();
        if let Some(draft) = self.drafts.get_mut(&key) {
            draft.attachments.retain(|a| a.id != id);
            draft.image_paths.remove(id);
            draft.remove_attachment_mention(id);
        }
    }
}
