use serde::{Deserialize, Serialize};
use vantry_protocol::Attachment;

use super::{AppState, InteractionMode, NEW_THREAD};

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct DraftMention {
    pub kind: u8,
    pub display: String,
    pub expand: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub attachment_id: String,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct Draft {
    pub text: String,
    pub mode: InteractionMode,
    pub attachments: Vec<Attachment>,
    pub mentions: Vec<DraftMention>,
    /// Source paths for local previews when using a workbench without attachment.read.
    pub image_paths: std::collections::BTreeMap<String, std::path::PathBuf>,
}

impl Draft {
    pub(super) fn is_blank(&self) -> bool {
        self.text.trim().is_empty() && self.attachments.is_empty()
    }

    /// The original Go draft stores what the editor displays and what each mention sends.
    pub(super) fn outgoing(&self) -> std::borrow::Cow<'_, str> {
        self.replace_mentions(|mention| &mention.expand)
    }

    pub(super) fn remove_attachment_mention(&mut self, id: &str) {
        if self.mentions.iter().any(|m| m.attachment_id == id) {
            self.text = self
                .replace_mentions(|m| {
                    if m.attachment_id == id {
                        ""
                    } else {
                        &m.display
                    }
                })
                .into_owned();
            self.mentions.retain(|m| m.attachment_id != id);
        }
    }

    fn replace_mentions(
        &self,
        replacement: impl Fn(&DraftMention) -> &str,
    ) -> std::borrow::Cow<'_, str> {
        let mut mentions: Vec<_> = self
            .mentions
            .iter()
            .filter(|m| !m.display.is_empty())
            .collect();
        if mentions.is_empty() {
            return (&self.text[..]).into();
        }
        mentions.sort_by_key(|m| std::cmp::Reverse(m.display.len()));
        let mut result = String::new();
        let mut rest = self.text.as_str();
        while let Some((at, mention)) = mentions
            .iter()
            .filter_map(|m| rest.find(&m.display).map(|at| (at, m)))
            .min_by_key(|p| p.0)
        {
            result.push_str(&rest[..at]);
            result.push_str(replacement(mention));
            rest = &rest[at + mention.display.len()..];
        }
        result.push_str(rest);
        result.into()
    }
}

impl AppState {
    pub fn set_skill_mention(&mut self, text: &str, name: &str) {
        self.set_draft_text(text);
        let key = self.draft_key();
        let draft = self.drafts.entry(key).or_default();
        let display = format!("/{name}");
        draft.mentions.retain(|m| m.display != display);
        draft.mentions.push(DraftMention {
            kind: 2,
            display,
            expand: format!("/skill:{name}"),
            ..Default::default()
        });
    }

    pub(super) fn restore_local_draft(&mut self, id: &str) -> bool {
        let Some(serial) = id
            .strip_prefix("local.")
            .and_then(|v| v.parse::<u64>().ok())
        else {
            return false;
        };
        if !self.local_order.iter().any(|k| k == id) {
            self.local_order.push(id.to_owned());
        }
        self.local_serial = self.local_serial.max(serial);
        true
    }

    /// A local id becomes a real thread only when its first send is accepted.
    pub fn start_local_draft(&mut self) {
        if self.selected.is_none()
            && self.local_selected.is_some()
            && self.draft().is_blank()
            && !self.attachment_imports.contains(&self.draft_key())
            && self
                .pending_by_key(&format!("submit:{}", self.draft_key()))
                .is_none()
        {
            return;
        }
        // Give an earlier Rust preview's single unnamed draft a sidebar entry.
        if self
            .drafts
            .get(NEW_THREAD)
            .is_some_and(|draft| !draft.is_blank())
            && self.pending_by_key("submit:").is_none()
        {
            let previous = self.allocate_local_draft();
            let draft = self.drafts.remove(NEW_THREAD).unwrap();
            self.drafts.insert(previous, draft);
        }
        let id = self.allocate_local_draft();
        self.select_thread(Some(id));
    }

    pub fn discard_local_draft(&mut self, id: &str) {
        if !self.local_order.iter().any(|item| item == id)
            || self.attachment_imports.contains(id)
            || self.pending_by_key(&format!("submit:{id}")).is_some()
        {
            return;
        }
        self.local_order.retain(|item| item != id);
        self.drafts.remove(id);
        if self.local_selected.as_deref() == Some(id) {
            let next = self
                .local_order
                .last()
                .cloned()
                .or_else(|| self.visible_threads().first().map(|t| t.id.clone()));
            self.select_thread(next);
        }
    }

    pub(super) fn allocate_local_draft(&mut self) -> String {
        loop {
            self.local_serial = self.local_serial.wrapping_add(1);
            let id = format!("local.{}", self.local_serial);
            if !self.local_order.contains(&id) && !self.drafts.contains_key(&id) {
                self.local_order.push(id.clone());
                return id;
            }
        }
    }
}
