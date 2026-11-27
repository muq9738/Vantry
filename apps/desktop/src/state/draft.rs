use serde::{Deserialize, Serialize};
use vantry_protocol::{Attachment, ThreadLink};

use super::{AppState, InteractionMode, NEW_THREAD, thread_links};

/// What a [`DraftMention`] refers to (v0.1.42's `mentionKind`).
pub const MENTION_THREAD: u8 = 1;
pub const MENTION_ATTACHMENT: u8 = 3;

/// The byte offset of UTF-16 offset `units` in `text` (the native input's
/// offsets), on a char boundary; the end when past it.
pub(crate) fn utf16_to_byte(text: &str, units: u32) -> usize {
    let mut seen = 0u32;
    for (byte, ch) in text.char_indices() {
        if seen >= units {
            return byte;
        }
        seen += ch.len_utf16() as u32;
    }
    text.len()
}

/// A thread link's title as its mention shows it (v0.1.42's `mentionTitle`).
fn mention_title(title: &str) -> String {
    let title = title.split_whitespace().collect::<Vec<_>>().join(" ");
    if title.chars().count() > 48 {
        let mut short: String = title.chars().take(47).collect();
        short.push('…');
        short
    } else {
        title
    }
}

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
    pub(crate) fn sidebar_title(&self) -> String {
        let topics = self.replace_mentions(|m| {
            if m.kind == MENTION_ATTACHMENT { "\n" } else { &m.display }
        });
        let mut parts = Vec::new();
        for clause in topics.split(['\n', '；', ';', '。']).map(str::trim) {
            if clause.is_empty() || clause.starts_with("https://") || clause.starts_with("http://") {
                continue;
            }
            let title = super::title_from_message(clause);
            if !parts.contains(&title) {
                parts.push(title);
            }
            if parts.len() == 3 {
                break;
            }
        }
        if parts.is_empty() {
            if !self.attachments.is_empty() && self.attachments.iter().all(|a| a.mime.starts_with("image/")) {
                return format!("{} 张图片", self.attachments.len());
            }
            return super::title_from_message(&self.text);
        }
        if parts.len() > 1 {
            for part in &mut parts {
                *part = part.split(['，', ',']).next().unwrap_or(part).chars().take(12).collect();
            }
        }
        super::title_from_message(&parts.join(" · "))
    }
    pub(super) fn is_blank(&self) -> bool {
        self.text.trim().is_empty() && self.attachments.is_empty()
    }

    /// The original Go draft stores what the editor displays and what each mention sends.
    pub(super) fn outgoing(&self) -> std::borrow::Cow<'_, str> {
        self.replace_mentions(|mention| &mention.expand)
    }

    pub(super) fn message_for(&self, provider: &str, model: &str) -> String {
        if vantry_protocol::uses_gemini_web(provider, model) {
            return self.text.clone();
        }
        self.outgoing().trim().to_owned()
    }

    /// Short display label (without leading `@`) for an attachment: its
    /// mention's text when the draft has one, so the thumbnail and the text
    /// name it alike; else the image's number in the UI's language.
    pub fn attachment_short_label(&self, item: &Attachment, lang: super::language::Language) -> String {
        if let Some(m) = self.mentions.iter().find(|m| m.attachment_id == item.id) {
            return m.display.trim_start_matches('@').to_owned();
        }
        if item.mime.starts_with("image/") {
            let mut img_seq = 0usize;
            for att in &self.attachments {
                if att.mime.starts_with("image/") {
                    img_seq += 1;
                    if att.id == item.id {
                        return lang.image_label(img_seq);
                    }
                }
            }
        }
        if let Some(m) = self.mentions.iter().find(|m| m.attachment_id == item.id) {
            return m.display.trim_start_matches('@').to_owned();
        }
        middle_truncate_name(&item.name, 18)
    }

    pub(crate) fn build_attachment_mention(
        &self,
        item: &Attachment,
        lang: super::language::Language,
    ) -> DraftMention {
        let is_pasted_image = item.mime.starts_with("image/")
            && is_generic_image_name(&item.name);
        let display = if is_pasted_image {
            let mut n = 1usize;
            loop {
                let candidate = format!("@{}", lang.image_label(n));
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
            kind: MENTION_ATTACHMENT,
            display,
            expand: format!("@{}", item.name),
            attachment_id: item.id.clone(),
        }
    }

    /// Where the editable body starts in `text` (after a `/skill:` prefix and
    /// any leading thread references kept by older drafts).
    fn body_start(&self) -> usize {
        self.text.len() - thread_links::draft_parts(&self.text).2.len()
    }

    /// Inserts `token` at `caret` (UTF-16 in the body, as the native input
    /// counts; `None`: the end), spaced from the text around it. Returns the
    /// caret after it.
    pub(crate) fn insert_token(&mut self, token: &str, caret: Option<u32>) -> u32 {
        let start = self.body_start();
        let body = &self.text[start..];
        let at = caret.map_or(body.len(), |c| utf16_to_byte(body, c));
        let (before, after) = body.split_at(at);
        let mut insert = String::new();
        if !before.is_empty() && !before.ends_with(char::is_whitespace) {
            insert.push(' ');
        }
        insert.push_str(token);
        if !after.starts_with(char::is_whitespace) {
            insert.push(' ');
        }
        let end = at + insert.len();
        self.text.insert_str(start + at, &insert);
        self.text[start..start + end].encode_utf16().count() as u32
    }

    /// Adds `item`'s `@` reference at `caret` (see [`Self::insert_token`]);
    /// `None` when the draft already refers to it.
    pub(super) fn insert_attachment_mention(
        &mut self,
        item: &Attachment,
        lang: super::language::Language,
        caret: Option<u32>,
    ) -> Option<u32> {
        if self.mentions.iter().any(|m| m.attachment_id == item.id) {
            return None;
        }
        let mention = self.build_attachment_mention(item, lang);
        let token = mention.display.clone();
        self.mentions.push(mention);
        Some(self.insert_token(&token, caret))
    }

    /// Records a mention, replacing an older one with the same text.
    fn add_mention(&mut self, mention: DraftMention) {
        match self.mentions.iter_mut().find(|m| m.display == mention.display) {
            Some(old) => *old = mention,
            None => self.mentions.push(mention),
        }
    }

    /// Turns every Markdown thread link after the skill prefix into its
    /// `@title` mention, where it stands (v0.1.42's `collapseThreadLinks`):
    /// the field shows the title and sending delivers the link in its place.
    /// `caret` (UTF-16 in the body) keeps its place in the text around it.
    pub(crate) fn collapse_thread_links(&mut self, caret: Option<u32>) -> (bool, Option<u32>) {
        let (skill, rest) = thread_links::skill_prefix(&self.text);
        let prefix_len = self.text.len() - rest.len();
        let body_start = self.body_start();
        // The caret as a byte offset of `rest` (the body starts within it).
        let caret_byte = caret.map(|c| {
            body_start - prefix_len + utf16_to_byte(&self.text[body_start..], c)
        });
        let mut out = String::with_capacity(rest.len());
        let mut mentions = Vec::new();
        let mut new_caret = caret_byte;
        let mut last = 0;
        let mut at = 0;
        while let Some(found) = rest[at..].find('[') {
            let start = at + found;
            let Some(link) = ThreadLink::parse(&rest[start..]) else {
                at = start + 1;
                continue;
            };
            let end = start + link.source.len();
            let display = format!("@{}", mention_title(&link.title));
            out.push_str(&rest[last..start]);
            let shift_from = out.len();
            out.push_str(&display);
            if let (Some(c), Some(old)) = (new_caret.as_mut(), caret_byte) {
                if old >= end {
                    *c = c.saturating_sub((end - start).saturating_sub(display.len()));
                } else if old > start {
                    *c = shift_from + display.len();
                }
            }
            mentions.push(DraftMention {
                kind: MENTION_THREAD,
                display,
                expand: link.source.to_owned(),
                attachment_id: String::new(),
            });
            last = end;
            at = end;
        }
        if mentions.is_empty() {
            return (false, caret);
        }
        out.push_str(&rest[last..]);
        let skill = skill.map(str::to_owned);
        for mention in mentions {
            self.add_mention(mention);
        }
        self.text = thread_links::compose(skill.as_deref(), std::iter::empty(), &out);
        // The body now starts right after the skill prefix.
        let caret = new_caret.map(|c| {
            let c = c.min(out.len());
            let c = super::timeline::floor_char_boundary(&out, c);
            out[..c].encode_utf16().count() as u32
        });
        (true, caret)
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
            .filter_map(|m| mention_at(rest, &m.display).map(|at| (at, m)))
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

/// The next whole numbered reference (e.g. 图1 must not match the start of 图12).
pub(crate) fn mention_at(text: &str, label: &str) -> Option<usize> {
    if label.is_empty() {
        return None;
    }
    text.match_indices(label)
        .find(|(at, _)| {
            !label.ends_with(|c: char| c.is_ascii_digit())
                || !text[at + label.len()..].starts_with(|c: char| c.is_ascii_digit())
        })
        .map(|(at, _)| at)
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::language::Language;

    fn image(id: &str) -> Attachment {
        Attachment {
            id: id.into(),
            name: format!("pasted-image-{id}.png"),
            mime: "image/png".into(),
            size: 10,
        }
    }

    #[test]
    fn a_pasted_image_reference_goes_in_the_text_at_the_caret() {
        let mut draft = Draft {
            text: "看这里说明".into(),
            ..Draft::default()
        };
        let caret = "看这里".encode_utf16().count() as u32;
        let next = draft.insert_attachment_mention(&image("a"), Language::Zh, Some(caret));
        assert_eq!(draft.text, "看这里 @图1 说明");
        assert_eq!(next, Some("看这里 @图1 ".encode_utf16().count() as u32));
        // The second image follows the first one where the caret moved.
        draft.insert_attachment_mention(&image("b"), Language::Zh, next);
        assert_eq!(draft.text, "看这里 @图1 @图2 说明");
        assert_eq!(draft.attachment_short_label(&image("b"), Language::En), "图2");
        // Sending names the files in place.
        assert_eq!(draft.outgoing(), "看这里 @pasted-image-a.png @pasted-image-b.png 说明");
    }

    #[test]
    fn a_pasted_thread_link_reads_as_its_title_where_it_was_pasted() {
        let link = "[修复 [登录] 流程](vantry-thread://th_one)";
        let mut draft = Draft {
            text: format!("先看 {link} 再改"),
            ..Draft::default()
        };
        let caret = format!("先看 {link}").encode_utf16().count() as u32;
        let (changed, caret) = draft.collapse_thread_links(Some(caret));
        assert!(changed);
        assert_eq!(draft.text, "先看 @修复 [登录] 流程 再改");
        assert_eq!(caret, Some("先看 @修复 [登录] 流程".encode_utf16().count() as u32));
        assert_eq!(draft.outgoing(), format!("先看 {link} 再改"));
        for model in ["", "google/gemini-4-neon", "magpie/dmit-ai/group/gemini-4-neon:high",
            "magpie/dmit-ai/group/gemini-web-neon-high", "other/gemini-web/gemini-4-neon"] {
            assert_eq!(draft.message_for("pi", model), draft.outgoing(), "{model}");
        }
        for model in ["gemini-web/gemini-4-neon", "magpie/gemini-web-bridge/gemini-web-neon-high"] {
            assert_eq!(draft.message_for("pi", model), draft.text, "{model}");
        }
        assert_eq!(draft.message_for(vantry_protocol::GEMINI_WRITING_PROVIDER, vantry_protocol::GEMINI_WRITING_MODEL), draft.text);
    }

    #[test]
    fn older_leading_references_move_into_the_text_after_the_skill() {
        let mut draft = Draft {
            text: "/skill:pdf [前次](vantry-thread://one) 请提取".into(),
            ..Draft::default()
        };
        draft.collapse_thread_links(None);
        assert_eq!(draft.text, "/skill:pdf @前次 请提取");
        assert_eq!(
            draft.outgoing(),
            "/skill:pdf [前次](vantry-thread://one) 请提取"
        );
        let (_, links, body) = thread_links::draft_parts(&draft.text);
        assert!(links.is_empty());
        assert_eq!(body, "@前次 请提取");
    }

    #[test]
    fn draft_title_collects_topics_and_omits_numbered_images() {
        let draft = Draft {
            text: "@图1 优化增量更新，参考发布包； @图2 复制图片引用； @图3 归纳会话标题".into(),
            mentions: (1..=3).map(|i| DraftMention {
                kind: MENTION_ATTACHMENT, display: format!("@图{i}"), ..Default::default()
            }).collect(),
            ..Default::default()
        };
        assert_eq!(draft.sidebar_title(), "优化增量更新 · 复制图片引用 · 归纳会话标题");
    }

    #[test]
    fn utf16_offsets_land_on_characters() {
        assert_eq!(utf16_to_byte("🌱看", 2), "🌱".len());
        assert_eq!(utf16_to_byte("🌱看", 9), "🌱看".len());
        assert_eq!(utf16_to_byte("", 3), 0);
    }
}
