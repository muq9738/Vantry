//! The composer's attributed editing surface for drafts containing real references.
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};

use crate::state::thread_links::{draft_parts, with_body};
use crate::{
    controller,
    state::{Draft, DraftMention, draft::MENTION_ATTACHMENT},
    theme::metrics,
};
use nsh_base::rich_editor::{
    commands::base_keymap,
    extensions::{HistoryOptions, history, keymap},
    model::{Node, SchemaSpec, Slice},
    state::{
        ClickEvent, Decoration, DecorationSet, EditorState, EditorStateConfig, PluginSpec,
        Transaction,
    },
    view::{
        EditorSession, NodeViews, RichEditor, RichEditorApi, RichEditorChange,
        RichEditorPastedFiles, RichEditorSelection, rich_editor,
    },
};
use nsh_core::prelude::*;
use serde_json::{Value, json};

#[derive(Copy, Clone)]
pub(super) struct InputRef {
    pub plain: ElementRef,
    pub rich: ComponentRef<RichEditor>,
}
impl InputRef {
    pub fn new() -> Self {
        Self {
            plain: element_ref(),
            rich: component_ref(),
        }
    }
    pub fn focus(self) {
        if let Some(api) = self.rich.api::<RichEditorApi>() {
            api.focus();
        } else {
            self.plain.focus();
        }
    }
    pub fn set_selection(self, start: u32, end: u32) {
        if let Some(api) = self.rich.api::<RichEditorApi>() {
            let draft = controller::state().with_untracked(|s| s.draft());
            let body = draft_parts(&draft.text).2;
            api.set_selection(doc_position(body, start), doc_position(body, end));
        } else {
            self.plain.set_selection(start, end);
        }
    }
}

// Each paragraph has an opening and closing position. Plain text has one newline between them.
fn doc_position(body: &str, offset: u32) -> u32 {
    1 + offset
        + body
            .encode_utf16()
            .take(offset as usize)
            .filter(|c| *c == 10)
            .count() as u32
}
fn text_offset(doc: &Node, pos: u32) -> u32 {
    let mut before = 0;
    let mut start = 1;
    for paragraph in doc.content().iter() {
        let len = paragraph.content().size() as u32;
        if pos <= start + len {
            return before + pos.saturating_sub(start);
        }
        start += len + 2;
        before += len + 1;
    }
    before.saturating_sub(1)
}
fn schema() -> nsh_base::rich_editor::model::Schema {
    SchemaSpec::from_json(&json!({"nodes":{"doc":{"content":"paragraph+"},"paragraph":{"content":"text*","group":"block"},"text":{"group":"inline"}}})).expect("composer schema")
}

fn document(body: &str) -> Value {
    let blocks: Vec<_> = body
        .split('\n')
        .map(|line| {
            if line.is_empty() {
                json!({"type":"paragraph"})
            } else {
                json!({"type":"paragraph", "content":[{"type":"text","text":line}]})
            }
        })
        .collect();
    json!({"type":"doc","content":blocks})
}
fn plain(doc: &Node) -> String {
    doc.content()
        .iter()
        .map(Node::text_content)
        .collect::<Vec<_>>()
        .join("\n")
}

fn draft_references(draft: &Draft) -> Vec<DraftMention> {
    let mut mentions = draft.mentions.clone();
    for file in &draft.attachments {
        let display = format!("@{}", file.name);
        if !file.id.is_empty()
            && !file.name.is_empty()
            && !mentions.iter().any(|m| m.display == display)
        {
            mentions.push(DraftMention {
                kind: MENTION_ATTACHMENT,
                display: display.clone(),
                expand: display,
                attachment_id: file.id.clone(),
            });
        }
    }
    mentions
}

#[derive(Clone)]
struct Submit(std::rc::Rc<dyn Fn()>);

#[derive(Default)]
struct References(Mutex<Vec<DraftMention>>);
impl References {
    fn spans(&self, doc: &Node) -> Vec<(usize, usize, String)> {
        let names = self.0.lock().unwrap();
        let mut result = Vec::new();
        let mut pos = 1;
        for block in doc.content().iter() {
            let line = block.text_content();
            let mut rest = line.as_str();
            let mut cursor = pos;
            while let Some((at, mention)) = names
                .iter()
                .filter(|m| !m.display.is_empty())
                .filter_map(|m| crate::state::draft::mention_at(rest, &m.display).map(|at| (at, m)))
                .min_by_key(|(i, m)| (*i, std::cmp::Reverse(m.display.len())))
            {
                let from = cursor + rest[..at].encode_utf16().count();
                let to = from + mention.display.encode_utf16().count();
                let url = if mention.kind == MENTION_ATTACHMENT {
                    Some(format!("vantry-attachment://{}", mention.attachment_id))
                } else {
                    vantry_protocol::ThreadLink::parse(&mention.expand).map(|l| l.url.to_owned())
                };
                if let Some(url) = url {
                    result.push((from, to, url));
                }
                cursor = to;
                rest = &rest[at + mention.display.len()..];
            }
            pos += block.node_size();
        }
        result
    }
}
impl PluginSpec for References {
    fn decorations(&self, state: &EditorState) -> Option<DecorationSet> {
        let attrs = BTreeMap::from([("class".into(), json!("text-blue-500 underline"))]);
        let marks = self
            .spans(state.doc())
            .into_iter()
            .map(|(from, to, _)| Decoration::inline(from, to, attrs.clone()))
            .collect();
        Some(DecorationSet::create(state.doc(), marks))
    }
    fn handle_key_down(
        &self,
        state: &EditorState,
        event: &nsh_base::rich_editor::state::KeyEvent,
        dispatch: &mut dyn FnMut(Transaction),
    ) -> bool {
        if event.key != "Enter" {
            return false;
        }
        let plain_enter = !event.mod_key && !event.ctrl && !event.alt;
        let send = controller::ui().with_untracked(|u| {
            if u.send_with_enter {
                plain_enter
            } else {
                event.mod_key || event.ctrl
            }
        });
        if !event.shift && send {
            if let Some(handler) = use_context::<Submit>() {
                (handler.0)();
            }
            true
        } else {
            nsh_base::rich_editor::commands::split_block(state, Some(dispatch))
        }
    }
    fn handle_click(
        &self,
        state: &EditorState,
        pos: usize,
        event: &ClickEvent,
        _: &mut dyn FnMut(Transaction),
    ) -> bool {
        if event.button != 0 || event.shift {
            return false;
        }
        let Some((_, _, url)) = self
            .spans(state.doc())
            .into_iter()
            .find(|(from, to, _)| (*from..*to).contains(&pos))
        else {
            return false;
        };
        controller::open_link(url);
        true
    }
}

pub(super) fn reference_input(
    input: InputRef,
    draft: Memo<Draft>,
    focused: Signal<bool>,
    composing: Signal<bool>,
    selection: Signal<Option<(u32, u32)>>,
    changed: impl Fn() + Copy + 'static,
    submit: impl Fn() + 'static,
) -> impl IntoElement {
    provide_context(Submit(std::rc::Rc::new(submit)));
    let references = Arc::new(References::default());
    let initial = draft.get_untracked();
    *references.0.lock().unwrap() = draft_references(&initial);
    let schema = schema();
    let doc = schema
        .node_from_json(&document(draft_parts(&initial.text).2))
        .expect("plain text document");
    let config = EditorStateConfig::new(schema).doc(doc).plugins(vec![
        references.clone(),
        keymap(nsh_base::rich_editor::extensions::kit::history_keys()),
        keymap(base_keymap()),
        history(HistoryOptions::default()),
    ]);
    let session = stored(EditorSession::new(
        EditorState::create(config).expect("composer state"),
        NodeViews::new(),
    ));
    effect(move || {
        let current = draft.get();
        let session = session.get();
        let state = session.state();
        let mut names = references.0.lock().unwrap();
        let next = draft_references(&current);
        let names_changed = *names != next;
        if names_changed {
            *names = next;
        }
        drop(names);
        let body = draft_parts(&current.text).2;
        let mut tr = state.tr();
        if body != plain(state.doc()) {
            let doc = state
                .schema()
                .node_from_json(&document(body))
                .expect("plain text document");
            if tr
                .replace(
                    0,
                    state.doc().content().size(),
                    Slice::new(doc.content().clone(), 0, 0),
                )
                .is_err()
            {
                return;
            }
        } else if !names_changed {
            return;
        }
        tr.set_meta("addToHistory", json!(false));
        let _ = session.dispatch(tr);
    });
    let height = signal(44f32);
    rich_editor()
        .component_ref(&input.rich)
        .editor(session.get())
        .accessibility_label("消息输入框")
        .auto_focus(true)
        .placeholder("输入消息…")
        .w_full()
        .h(move || px(height.get()))
        .text_size(px(metrics::BODY + 0.5))
        .on_content_size_change(
            move |event: &nsh_base::rich_editor::view::RichEditorEvent| {
                if let nsh_base::rich_editor::view::RichEditorEvent::ContentSizeChange {
                    content_size,
                } = event
                {
                    height.set((content_size.height.0 + 8.0).clamp(44., 220.));
                }
            },
        )
        .on_focus(move || focused.set(true))
        .on_blur(move || focused.set(false))
        .on_composition_change(move |active: &bool| composing.set(*active))
        .on_selection_change(move |s: &RichEditorSelection| {
            let state = session.get().state();
            let end = text_offset(state.doc(), s.head);
            selection.set(Some((text_offset(state.doc(), s.anchor), end)));
            controller::note_composer_caret(end);
        })
        .on_change(move |_: &RichEditorChange| {
            let state = session.get().state();
            let text = plain(state.doc());
            let source = controller::state().with_untracked(|s| with_body(&s.draft().text, &text));
            if source != controller::state().with_untracked(|s| s.draft().text) {
                changed();
                let at = text_offset(state.doc(), state.selection().head() as u32);
                let next = controller::paste_draft_text(&source, at);
                if next != at {
                    input.set_selection(next, next);
                }
            }
        })
        .on_paste_files(move |p: &RichEditorPastedFiles| {
            if !p.uris.is_empty() {
                controller::paste_file_uris(&p.uris);
            }
            if !p.files.is_empty() {
                let mut paste = PasteEvent::default();
                paste.files = p
                    .files
                    .iter()
                    .map(|f| nsh_core::PastedFile::new(&f.name, &f.mime, f.data.clone()))
                    .collect();
                controller::paste_images(&paste);
            }
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn reference_offsets_follow_utf16_across_paragraphs_and_numbered_images() {
        let body = "😀看图\n@图12 @图1\n\n收尾";
        let doc = schema().node_from_json(&document(body)).unwrap();
        for i in 0..=body.encode_utf16().count() as u32 {
            assert_eq!(text_offset(&doc, doc_position(body, i)), i);
        }
        let refs = References(Mutex::new(vec![DraftMention {
            display: "@图1".into(),
            attachment_id: "one".into(),
            kind: MENTION_ATTACHMENT,
            ..Default::default()
        }]));
        let start = doc_position(body, "😀看图\n@图12 ".encode_utf16().count() as u32) as usize;
        assert_eq!(
            refs.spans(&doc),
            [(start, start + 3, "vantry-attachment://one".into())]
        );
    }
}
