//! A small Markdown reader for replies: blocks (headings, paragraphs,
//! ordered / unordered / task lists, fenced code with its language, diff
//! blocks, quotes, rules, pipe tables) and inlines (strong, emphasis,
//! strike, code, links).
//!
//! It is line based and every block ends at a line, so a document grows
//! incrementally: [`MarkdownDoc::update`] keeps every block before the last
//! one and parses again only from where the last block starts. A long
//! streamed reply costs the size of its last block per delta, not its
//! whole length, and the blocks it keeps compare equal, so their views do
//! not change.

use std::sync::Arc;

/// A run of inline text.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Inline {
    Text(String),
    Strong(String),
    Emphasis(String),
    Strike(String),
    Code(String),
    Link { text: String, url: String },
}

/// One item of a list (nested lists are flattened, with their depth).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ListItem {
    pub depth: u8,
    /// The marker shown: `1.`, `•`.
    pub marker: String,
    /// A task item's box.
    pub checked: Option<bool>,
    pub inlines: Vec<Inline>,
}

/// A line of a diff block.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DiffKind {
    Added,
    Removed,
    Hunk,
    /// `diff --git`, `---`, `+++`, `index …`.
    Meta,
    Context,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DiffLine {
    pub kind: DiffKind,
    pub text: String,
}

/// A block of a document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Block {
    Heading { level: u8, inlines: Vec<Inline> },
    Paragraph(Vec<Inline>),
    List { ordered: bool, items: Vec<ListItem> },
    /// A fenced code block; `closed` is false while its fence is still open
    /// (a streaming reply).
    Code { lang: String, code: String, closed: bool },
    /// A ```` ```diff ```` (or `patch`) block.
    Diff { lines: Vec<DiffLine>, closed: bool },
    /// A quote's lines.
    Quote(Vec<Vec<Inline>>),
    Rule,
    Table { header: Vec<Vec<Inline>>, rows: Vec<Vec<Vec<Inline>>> },
}

/// A block and the byte offset of the line it starts on.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Spanned {
    pub start: usize,
    pub block: Arc<Block>,
}

/// A parsed document. Equal documents compare by their blocks, which are
/// shared (`Arc`), so a kept block compares by pointer first.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct MarkdownDoc {
    /// The length of the source it was parsed from.
    pub len: usize,
    /// Where that source's last, unfinished line starts (`len` when it
    /// ended with a newline).
    pub open_line: usize,
    pub blocks: Vec<Spanned>,
}

/// Where `source`'s last unfinished line starts.
fn open_line_of(source: &str) -> usize {
    source.rfind('\n').map_or(0, |at| at + 1)
}

impl MarkdownDoc {
    pub fn parse(source: &str) -> Self {
        MarkdownDoc {
            len: source.len(),
            open_line: open_line_of(source),
            blocks: parse_blocks(source, 0),
        }
    }

    /// How many leading blocks are final: a block ends where the next one
    /// starts, and that is settled only when the next block's first line
    /// was complete (an unfinished `2` may still become `2. item` and
    /// continue a list).
    pub fn final_blocks(&self) -> usize {
        self.blocks
            .windows(2)
            .take_while(|pair| pair[1].start < self.open_line)
            .count()
    }

    /// Follows `source`, which usually extends what was parsed: keeps the
    /// final blocks and parses from where the first unsettled one starts.
    /// Anything else (a shorter source) parses again.
    pub fn update(&mut self, source: &str) {
        if source.len() < self.len {
            *self = Self::parse(source);
            return;
        }
        let keep = self.final_blocks();
        let from = self.blocks.get(keep).map_or(0, |b| b.start);
        if !source.is_char_boundary(from) {
            *self = Self::parse(source);
            return;
        }
        self.blocks.truncate(keep);
        let tail = parse_blocks(&source[from..], from);
        self.blocks.extend(tail);
        self.len = source.len();
        self.open_line = open_line_of(source);
    }

    pub fn blocks(&self) -> impl Iterator<Item = &Block> {
        self.blocks.iter().map(|b| b.block.as_ref())
    }
}

/// The plain text of `inlines` (copying, tests).
pub fn plain(inlines: &[Inline]) -> String {
    inlines
        .iter()
        .map(|i| match i {
            Inline::Text(t)
            | Inline::Strong(t)
            | Inline::Emphasis(t)
            | Inline::Strike(t)
            | Inline::Code(t) => t.as_str(),
            Inline::Link { text, .. } => text.as_str(),
        })
        .collect()
}

// ------------------------------------------------------------------- blocks

/// The lines of `source` with their byte offsets (`base` added).
fn lines_of(source: &str, base: usize) -> Vec<(usize, &str)> {
    let mut out = Vec::new();
    let mut at = 0;
    for line in source.split_inclusive('\n') {
        let text = line.strip_suffix('\n').unwrap_or(line);
        let text = text.strip_suffix('\r').unwrap_or(text);
        out.push((base + at, text));
        at += line.len();
    }
    out
}

fn is_blank(line: &str) -> bool {
    line.trim().is_empty()
}

fn heading(line: &str) -> Option<(u8, &str)> {
    let trimmed = line.trim_start();
    if line.len() - trimmed.len() > 3 {
        return None;
    }
    let level = trimmed.bytes().take_while(|b| *b == b'#').count();
    if !(1..=6).contains(&level) {
        return None;
    }
    let rest = &trimmed[level..];
    if !rest.is_empty() && !rest.starts_with(' ') {
        return None;
    }
    Some((level as u8, rest.trim().trim_end_matches('#').trim_end()))
}

fn is_rule(line: &str) -> bool {
    let compact: String = line.chars().filter(|c| !c.is_whitespace()).collect();
    compact.len() >= 3
        && ['-', '*', '_']
            .iter()
            .any(|m| compact.chars().all(|c| c == *m))
}

/// A fence opening: its marker character, length and info string.
fn fence(line: &str) -> Option<(char, usize, &str)> {
    let trimmed = line.trim_start();
    if line.len() - trimmed.len() > 3 {
        return None;
    }
    let marker = trimmed.chars().next()?;
    if marker != '`' && marker != '~' {
        return None;
    }
    let count = trimmed.chars().take_while(|c| *c == marker).count();
    if count < 3 {
        return None;
    }
    let info = trimmed[count..].trim();
    if marker == '`' && info.contains('`') {
        return None;
    }
    Some((marker, count, info))
}

fn closes(line: &str, marker: char, count: usize) -> bool {
    let trimmed = line.trim();
    trimmed.chars().take_while(|c| *c == marker).count() >= count
        && trimmed.chars().all(|c| c == marker)
}

/// A list item's marker: (indent, ordered, number text, content).
fn list_marker(line: &str) -> Option<(usize, bool, String, &str)> {
    let content = line.trim_start();
    let indent = line.len() - content.len();
    for bullet in ["- ", "* ", "+ "] {
        if let Some(rest) = content.strip_prefix(bullet) {
            return Some((indent, false, "•".into(), rest));
        }
        if content == bullet.trim_end() {
            return Some((indent, false, "•".into(), ""));
        }
    }
    let digits = content.bytes().take_while(u8::is_ascii_digit).count();
    if (1..=9).contains(&digits) {
        let after = &content[digits..];
        for delimiter in [". ", ") "] {
            if let Some(rest) = after.strip_prefix(delimiter) {
                return Some((indent, true, format!("{}.", &content[..digits]), rest));
            }
        }
    }
    None
}

fn is_table_row(line: &str) -> bool {
    let t = line.trim();
    t.starts_with('|') && t.len() > 1
}

fn table_cells(line: &str) -> Vec<String> {
    let t = line.trim().trim_start_matches('|');
    let t = t.strip_suffix('|').unwrap_or(t);
    t.split('|').map(|c| c.trim().to_owned()).collect()
}

fn is_table_separator(line: &str) -> bool {
    is_table_row(line)
        && table_cells(line)
            .iter()
            .all(|c| !c.is_empty() && c.chars().all(|ch| matches!(ch, '-' | ':' | ' ')))
}

/// Whether `line` starts a block other than a paragraph (ending one).
fn starts_block(line: &str) -> bool {
    heading(line).is_some()
        || fence(line).is_some()
        || is_rule(line)
        || line.trim_start().starts_with('>')
        || list_marker(line).is_some()
}

fn parse_blocks(source: &str, base: usize) -> Vec<Spanned> {
    let lines = lines_of(source, base);
    let mut blocks = Vec::new();
    let mut i = 0;
    let push = |blocks: &mut Vec<Spanned>, start: usize, block: Block| {
        blocks.push(Spanned {
            start,
            block: Arc::new(block),
        });
    };
    while i < lines.len() {
        let (start, line) = lines[i];
        if is_blank(line) {
            i += 1;
            continue;
        }
        if let Some((marker, count, info)) = fence(line) {
            let first = info.split_whitespace().next().unwrap_or("");
            let lower = first.to_ascii_lowercase();
            let preserve_args = matches!(
                lower.as_str(),
                "ask"
                    | "dispatch"
                    | "callout"
                    | "kv"
                    | "limits"
                    | "timeline"
                    | "tree"
                    | "api"
                    | "chart"
                    | "vantry-ui"
                    | "mermaid"
                    | "flow"
                    | "state"
                    | "sequence"
                    | "er"
                    | "class"
                    | "math"
                    | "latex"
                    | "tex"
                    | "svg"
                    | "html"
                    | "artifact"
            );
            let lang = if preserve_args {
                info.trim().to_owned()
            } else {
                first.to_owned()
            };
            let mut body = Vec::new();
            let mut closed = false;
            i += 1;
            while i < lines.len() {
                if closes(lines[i].1, marker, count) {
                    closed = true;
                    i += 1;
                    break;
                }
                body.push(lines[i].1);
                i += 1;
            }
            let code = body.join("\n");
            let block = if lower == "diff" || lower == "patch" {
                Block::Diff {
                    lines: diff_lines(&code),
                    closed,
                }
            } else {
                Block::Code { lang, code, closed }
            };
            push(&mut blocks, start, block);
            continue;
        }
        if let Some((level, text)) = heading(line) {
            push(
                &mut blocks,
                start,
                Block::Heading {
                    level,
                    inlines: parse_inlines(text),
                },
            );
            i += 1;
            continue;
        }
        // A thematic break wins over a list item (`- - -`).
        if is_rule(line) {
            push(&mut blocks, start, Block::Rule);
            i += 1;
            continue;
        }
        if line.trim_start().starts_with('>') {
            let mut quote = Vec::new();
            while i < lines.len() && lines[i].1.trim_start().starts_with('>') {
                let text = lines[i].1.trim_start().trim_start_matches('>');
                quote.push(parse_inlines(text.strip_prefix(' ').unwrap_or(text)));
                i += 1;
            }
            push(&mut blocks, start, Block::Quote(quote));
            continue;
        }
        if let Some((_, ordered, _, _)) = list_marker(line) {
            let mut items: Vec<ListItem> = Vec::new();
            let base_indent = line.len() - line.trim_start().len();
            while i < lines.len() {
                let current = lines[i].1;
                // An item of the other kind at the list's level starts
                // another list.
                let same_kind = |l: &str| {
                    list_marker(l).is_some_and(|(indent, o, _, _)| o == ordered || indent > base_indent)
                };
                if is_blank(current) {
                    // A blank line ends the list unless an item follows.
                    let next = lines[i + 1..].iter().find(|(_, l)| !is_blank(l));
                    match next {
                        Some((_, l)) if same_kind(l) => {
                            i += 1;
                            continue;
                        }
                        _ => break,
                    }
                }
                if list_marker(current).is_some() && !same_kind(current) {
                    break;
                }
                if let Some((indent, _, marker, rest)) = list_marker(current) {
                    let (checked, rest) = task_box(rest);
                    items.push(ListItem {
                        depth: (indent.saturating_sub(base_indent) / 2).min(6) as u8,
                        marker,
                        checked,
                        inlines: parse_inlines(rest),
                    });
                    i += 1;
                } else if current.starts_with(' ') || current.starts_with('\t') {
                    // An indented line continues the item.
                    if let Some(last) = items.last_mut() {
                        last.inlines.push(Inline::Text(" ".into()));
                        last.inlines.extend(parse_inlines(current.trim()));
                    }
                    i += 1;
                } else {
                    break;
                }
            }
            push(&mut blocks, start, Block::List { ordered, items });
            continue;
        }
        if is_table_row(line) && lines.get(i + 1).is_some_and(|(_, l)| is_table_separator(l)) {
            let header = table_cells(line).iter().map(|c| parse_inlines(c)).collect();
            i += 2;
            let mut rows = Vec::new();
            while i < lines.len() && is_table_row(lines[i].1) {
                rows.push(table_cells(lines[i].1).iter().map(|c| parse_inlines(c)).collect());
                i += 1;
            }
            push(&mut blocks, start, Block::Table { header, rows });
            continue;
        }
        // A paragraph: until a blank line or another block.
        let mut text = String::new();
        while i < lines.len() && !is_blank(lines[i].1) {
            if !text.is_empty() {
                if starts_block(lines[i].1) {
                    break;
                }
                text.push('\n');
            }
            text.push_str(lines[i].1.trim_end());
            i += 1;
        }
        push(&mut blocks, start, Block::Paragraph(parse_inlines(&text)));
    }
    blocks
}

fn task_box(rest: &str) -> (Option<bool>, &str) {
    if let Some(r) = rest.strip_prefix("[ ] ") {
        (Some(false), r)
    } else if let Some(r) = rest.strip_prefix("[x] ").or_else(|| rest.strip_prefix("[X] ")) {
        (Some(true), r)
    } else {
        (None, rest)
    }
}

/// The lines of a unified diff, classified.
pub fn diff_lines(code: &str) -> Vec<DiffLine> {
    code.lines()
        .map(|line| {
            let kind = if line.starts_with("+++")
                || line.starts_with("---")
                || line.starts_with("diff ")
                || line.starts_with("index ")
                || line.starts_with("new file")
                || line.starts_with("deleted file")
            {
                DiffKind::Meta
            } else if line.starts_with("@@") {
                DiffKind::Hunk
            } else if line.starts_with('+') {
                DiffKind::Added
            } else if line.starts_with('-') {
                DiffKind::Removed
            } else {
                DiffKind::Context
            };
            DiffLine {
                kind,
                text: line.to_owned(),
            }
        })
        .collect()
}

// ------------------------------------------------------------------ inlines

/// The inline runs of `text`.
pub fn parse_inlines(text: &str) -> Vec<Inline> {
    let chars: Vec<char> = text.chars().collect();
    let mut out: Vec<Inline> = Vec::new();
    let mut plain = String::new();
    let mut i = 0;
    let flush = |plain: &mut String, out: &mut Vec<Inline>| {
        if !plain.is_empty() {
            out.push(Inline::Text(std::mem::take(plain)));
        }
    };
    while i < chars.len() {
        let c = chars[i];
        if c == '\\' && i + 1 < chars.len() && chars[i + 1].is_ascii_punctuation() {
            plain.push(chars[i + 1]);
            i += 2;
            continue;
        }
        if c == '`' {
            let ticks = chars[i..].iter().take_while(|c| **c == '`').count();
            if let Some(end) = find_run(&chars, i + ticks, '`', ticks) {
                let code: String = chars[i + ticks..end].iter().collect();
                flush(&mut plain, &mut out);
                let code = if code.starts_with(' ') && code.ends_with(' ') && code.trim() != "" {
                    code[1..code.len() - 1].to_owned()
                } else {
                    code
                };
                out.push(Inline::Code(code));
                i = end + ticks;
                continue;
            }
            plain.extend(&chars[i..i + ticks]);
            i += ticks;
            continue;
        }
        if c == '[' {
            if let Some((text_end, url_end)) = link_at(&chars, i) {
                let text: String = chars[i + 1..text_end].iter().collect();
                let url: String = chars[text_end + 2..url_end].iter().collect();
                flush(&mut plain, &mut out);
                out.push(Inline::Link {
                    text,
                    url: url.trim().to_owned(),
                });
                i = url_end + 1;
                continue;
            }
        }
        let mut consumed = false;
        for (marker, len, make) in [
            ('*', 2usize, Inline::Strong as fn(String) -> Inline),
            ('_', 2, Inline::Strong),
            ('~', 2, Inline::Strike),
            ('*', 1, Inline::Emphasis),
            ('_', 1, Inline::Emphasis),
        ] {
            if c != marker || !starts_run(&chars, i, marker, len) {
                continue;
            }
            // `_` inside a word is a character (snake_case).
            if marker == '_' && i > 0 && chars[i - 1].is_alphanumeric() {
                continue;
            }
            let open_end = i + len;
            if open_end >= chars.len() || chars[open_end].is_whitespace() {
                continue;
            }
            if let Some(end) = find_closing(&chars, open_end, marker, len)
                && !(marker == '_'
                    && chars.get(end + len).is_some_and(|c| c.is_alphanumeric()))
            {
                let inner: String = chars[open_end..end].iter().collect();
                flush(&mut plain, &mut out);
                out.push(make(inner));
                i = end + len;
                consumed = true;
                break;
            }
        }
        if !consumed {
            // A marker that opens nothing is a character; a run of them
            // stays together (`**` that never closes).
            let run = if matches!(c, '*' | '_' | '~') {
                chars[i..].iter().take_while(|x| **x == c).count()
            } else {
                1
            };
            plain.extend(&chars[i..i + run]);
            i += run;
        }
    }
    flush(&mut plain, &mut out);
    out
}

fn starts_run(chars: &[char], at: usize, marker: char, len: usize) -> bool {
    let run = chars[at..].iter().take_while(|c| **c == marker).count();
    if len == 1 { run == 1 || run == 3 } else { run >= 2 }
}

/// The start of the next run of exactly `len` `marker`s after `from`,
/// not preceded by whitespace.
fn find_closing(chars: &[char], from: usize, marker: char, len: usize) -> Option<usize> {
    let mut j = from;
    while j < chars.len() {
        if chars[j] == '`' {
            // Skip code spans.
            let ticks = chars[j..].iter().take_while(|c| **c == '`').count();
            match find_run(chars, j + ticks, '`', ticks) {
                Some(end) => {
                    j = end + ticks;
                    continue;
                }
                None => return None,
            }
        }
        if chars[j] == marker {
            let run = chars[j..].iter().take_while(|c| **c == marker).count();
            if run >= len && j > from && !chars[j - 1].is_whitespace() {
                return Some(j);
            }
            j += run;
            continue;
        }
        j += 1;
    }
    None
}

/// The start of a run of exactly `len` `marker`s at or after `from`.
fn find_run(chars: &[char], from: usize, marker: char, len: usize) -> Option<usize> {
    let mut j = from;
    while j < chars.len() {
        if chars[j] == marker {
            let run = chars[j..].iter().take_while(|c| **c == marker).count();
            if run == len {
                return Some(j);
            }
            j += run;
        } else {
            j += 1;
        }
    }
    None
}

/// `[text](url)` at `at`: the `]` and `)` positions.
fn link_at(chars: &[char], at: usize) -> Option<(usize, usize)> {
    let mut depth = 0;
    let mut j = at;
    let text_end = loop {
        match chars.get(j)? {
            '[' => depth += 1,
            ']' => {
                depth -= 1;
                if depth == 0 {
                    break j;
                }
            }
            '\n' => return None,
            _ => {}
        }
        j += 1;
    };
    if chars.get(text_end + 1) != Some(&'(') {
        return None;
    }
    let url_end = chars[text_end + 2..].iter().position(|c| *c == ')')? + text_end + 2;
    if chars[text_end + 2..url_end].iter().any(|c| *c == ' ' || *c == '\n') {
        return None;
    }
    Some((text_end, url_end))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn blocks(source: &str) -> Vec<Block> {
        MarkdownDoc::parse(source).blocks().cloned().collect()
    }

    fn t(s: &str) -> Inline {
        Inline::Text(s.into())
    }

    #[test]
    fn headings_paragraphs_and_rules() {
        let doc = blocks("# 标题\n\n第一段\n继续\n\n---\n### 小节 ###\n");
        assert_eq!(
            doc,
            vec![
                Block::Heading { level: 1, inlines: vec![t("标题")] },
                Block::Paragraph(vec![t("第一段\n继续")]),
                Block::Rule,
                Block::Heading { level: 3, inlines: vec![t("小节")] },
            ]
        );
        assert_eq!(blocks("#没有空格"), vec![Block::Paragraph(vec![t("#没有空格")])]);
    }

    #[test]
    fn lists_ordered_unordered_nested_and_tasks() {
        let doc = blocks("- 一\n- 二\n  - 二点一\n- [x] 完成\n- [ ] 待办\n\n1. 甲\n2) 乙\n");
        let Block::List { ordered: false, items } = &doc[0] else {
            panic!("{doc:?}")
        };
        assert_eq!(items.len(), 5);
        assert_eq!(items[2].depth, 1);
        assert_eq!(items[3].checked, Some(true));
        assert_eq!(items[4].checked, Some(false));
        assert_eq!(plain(&items[4].inlines), "待办");
        let Block::List { ordered: true, items } = &doc[1] else {
            panic!("{doc:?}")
        };
        assert_eq!(items[1].marker, "2.");
    }

    #[test]
    fn loose_lists_stay_one_list() {
        let doc = blocks("- a\n\n- b\n\n段落");
        assert_eq!(doc.len(), 2);
        assert!(matches!(&doc[0], Block::List { items, .. } if items.len() == 2));
    }

    #[test]
    fn fenced_code_keeps_language_and_open_state() {
        let doc = blocks("```rust\nfn main() {}\n```\n\n~~~\nraw\n");
        assert_eq!(
            doc[0],
            Block::Code { lang: "rust".into(), code: "fn main() {}".into(), closed: true }
        );
        assert_eq!(
            doc[1],
            Block::Code { lang: String::new(), code: "raw".into(), closed: false }
        );
        // Markdown inside a fence is code.
        assert!(matches!(&blocks("```\n# not a heading\n```")[0], Block::Code { code, .. } if code == "# not a heading"));
    }

    #[test]
    fn diff_blocks_classify_lines() {
        let doc = blocks("```diff\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n same\n```");
        let Block::Diff { lines, closed: true } = &doc[0] else {
            panic!("{doc:?}")
        };
        let kinds: Vec<DiffKind> = lines.iter().map(|l| l.kind).collect();
        assert_eq!(
            kinds,
            [
                DiffKind::Meta,
                DiffKind::Meta,
                DiffKind::Hunk,
                DiffKind::Removed,
                DiffKind::Added,
                DiffKind::Context
            ]
        );
    }

    #[test]
    fn quotes_and_tables() {
        let doc = blocks("> 引用一\n> **二**\n\n| a | b |\n|---|:-:|\n| 1 | `2` |\n");
        assert_eq!(
            doc[0],
            Block::Quote(vec![vec![t("引用一")], vec![Inline::Strong("二".into())]])
        );
        let Block::Table { header, rows } = &doc[1] else {
            panic!("{doc:?}")
        };
        assert_eq!(header.len(), 2);
        assert_eq!(rows[0][1], vec![Inline::Code("2".into())]);
    }

    #[test]
    fn inlines_parse_spans_and_leave_plain_text() {
        assert_eq!(
            parse_inlines("用 **粗体**、*斜体*、~~删除~~、`代码` 和 [链接](https://x.dev)。"),
            vec![
                t("用 "),
                Inline::Strong("粗体".into()),
                t("、"),
                Inline::Emphasis("斜体".into()),
                t("、"),
                Inline::Strike("删除".into()),
                t("、"),
                Inline::Code("代码".into()),
                t(" 和 "),
                Inline::Link { text: "链接".into(), url: "https://x.dev".into() },
                t("。"),
            ]
        );
        assert_eq!(parse_inlines("snake_case_name"), vec![t("snake_case_name")]);
        assert_eq!(parse_inlines("2 * 3 * 4"), vec![t("2 * 3 * 4")]);
        assert_eq!(parse_inlines("未闭合 **粗"), vec![t("未闭合 **粗")]);
        assert_eq!(parse_inlines(r"\*字面\*"), vec![t("*字面*")]);
        assert_eq!(parse_inlines("`a*b*c`"), vec![Inline::Code("a*b*c".into())]);
        assert_eq!(parse_inlines("[不是链接] (x)"), vec![t("[不是链接] (x)")]);
    }

    /// Streaming a document in any pieces ends where parsing it whole does,
    /// and blocks before the last are kept, not rebuilt.
    #[test]
    fn incremental_updates_match_a_full_parse() {
        let source = "# 计划\n\n先看 `src`：\n\n1. 读取\n2. 修改\n\n```rust\nfn main() {\n    println!(\"hi\");\n}\n```\n\n> 注意\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```diff\n-a\n+b\n```\n结束 **粗体** 文本。\n";
        let chars: Vec<char> = source.chars().collect();
        for step in [1, 2, 3, 5, 7, 11, 64] {
            let mut doc = MarkdownDoc::default();
            let mut text = String::new();
            for piece in chars.chunks(step) {
                text.extend(piece);
                let before: Vec<Arc<Block>> = doc
                    .blocks
                    .iter()
                    .take(doc.final_blocks())
                    .map(|b| b.block.clone())
                    .collect();
                doc.update(&text);
                for (kept, now) in before.iter().zip(&doc.blocks) {
                    assert!(Arc::ptr_eq(kept, &now.block), "a finished block was rebuilt");
                }
            }
            assert_eq!(doc, MarkdownDoc::parse(source), "step {step}");
        }
    }

    #[test]
    fn a_shorter_source_parses_again() {
        let mut doc = MarkdownDoc::parse("# a\n\nb");
        doc.update("x");
        assert_eq!(doc, MarkdownDoc::parse("x"));
    }
}
