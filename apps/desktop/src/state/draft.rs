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

    /// Short display label (without leading `@`) for an attachment chip.
    pub fn attachment_short_label(&self, item: &Attachment) -> String {
        if let Some(m) = self.mentions.iter().find(|m| m.attachment_id == item.id) {
            return m.display.trim_start_matches('@').to_owned();
        }
        middle_truncate_name(&item.name, 18)
    }

    pub(super) fn build_attachment_mention(&self, item: &Attachment) -> DraftMention {
        let is_pasted_image = item.mime.starts_with("image/")
            && is_generic_image_name(&item.name);
        let display = if is_pasted_image {
            let mut n = 1usize;
            loop {
                let candidate = format!("@图{n}");
                if !self.mentions.iter().any(|m| m.display == candidate && m.attachment_id != item.id)
                    && !self.text.contains(&candidate)
                {
                    break candidate;
                }
                n += 1;
            }
        } else {
            let base = format!("@{}", middle_truncate_name(&item.name, 22));
            let mut candidate = base.clone();
            let mut seq = 2usize;
            while self.mentions.iter().any(|m| m.display == candidate && m.attachment_id != item.id) {
                candidate = format!("{base}#{seq}");
                seq += 1;
            }
            candidate
        };
        DraftMention {
            kind: 3,
            display,
            expand: format!("@{}", item.name),
            attachment_id: item.id.clone(),
        }
    }

    pub(super) fn insert_attachment_mention(&mut self, item: &Attachment) {
        if self.mentions.iter().any(|m| m.attachment_id == item.id) {
            return;
        }
        let mention = self.build_attachment_mention(item);
        let token = mention.display.clone();
        self.mentions.push(mention);
        if !self.text.is_empty() && !self.text.ends_with(char::is_whitespace) {
            self.text.push(' ');
        }
        self.text.push_str(&token);
        self.text.push(' ');
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

pub(crate) fn is_generic_image_name(name: &str) -> bool {
    let lower = name.trim().to_ascii_lowercase();
    if lower.is_empty() {
        return true;
    }
    let stem = lower.rsplit_once('.').map_or(lower.as_str(), |(s, _)| s);
    stem == "image"
        || stem == "clipboard"
        || stem.starts_with("pasted-image")
        || stem.starts_with("image-")
        || stem.starts_with("screenshot")
        || stem.starts_with("截屏")
}

/// Shortens a file name from the middle, keeping the extension readable (aligned with T3 Code).
pub(crate) fn middle_truncate_name(name: &str, max_chars: usize) -> String {
    let chars: Vec<char> = name.chars().collect();
    if chars.len() <= max_chars || max_chars <= 1 {
        return name.to_owned();
    }
    let available = max_chars - 1;
    let suffix_len = (available.div_ceil(2)).min(10).min(available.saturating_sub(1));
    let prefix_len = available - suffix_len;
    let prefix: String = chars[..prefix_len].iter().collect();
    let suffix: String = chars[chars.len() - suffix_len..].iter().collect();
    format!("{prefix}…{suffix}")
}

impl AppState {
    pub fn begin_attachment_import(&mut self, key: &str) {
        *self.attachment_imports.entry(key.to_owned()).or_default() += 1;
    }

    pub fn finish_attachment_import(&mut self, key: &str) {
        if let Some(count) = self.attachment_imports.get_mut(key) {
            *count -= 1;
            if *count == 0 { self.attachment_imports.remove(key); }
        }
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
            && !self.attachment_imports.contains_key(&self.draft_key())
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
            || self.attachment_imports.contains_key(id)
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
