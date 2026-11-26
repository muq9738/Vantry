use vantry_protocol::ThreadLink;

use super::markdown::Inline;

/// Keep the complete Markdown in draft storage and on the wire. The native text field edits
/// the suffix after the reference chips, so IME and selection offsets are local to that field.
pub(crate) fn skill_prefix(source: &str) -> (Option<&str>, &str) {
    if let Some((command, body)) = source.strip_prefix("/skill:").and_then(|s| s.split_once(' '))
        && !command.is_empty() && !command.contains(char::is_whitespace)
    {
        return (Some(command), body);
    }
    (None, source)
}

pub(crate) fn draft_parts(source: &str) -> (Option<&str>, Vec<ThreadLink<'_>>, &str) {
    let (skill, mut rest) = skill_prefix(source);
    let mut links = Vec::new();
    while let Some(link) = ThreadLink::parse(rest) {
        rest = &rest[link.source.len()..];
        rest = rest.strip_prefix(' ').unwrap_or(rest);
        links.push(link);
    }
    (skill, links, rest)
}

pub(crate) fn with_body(source: &str, new_body: &str) -> String {
    let (skill, links, _) = draft_parts(source);
    compose(skill, links.iter().map(|link| link.source), new_body)
}

pub(crate) fn compose<'a>(skill: Option<&str>, links: impl Iterator<Item = &'a str>, body: &str) -> String {
    let mut result = skill.map(|s| format!("/skill:{s} ")).unwrap_or_default();
    for link in links {
        result.push_str(link);
        result.push(' ');
    }
    result.push_str(body);
    result
}

/// User messages keep their literal text, including Markdown punctuation outside references.
pub(crate) fn message_inlines(source: &str) -> Vec<Inline> {
    let mut parts = vec![];
    let mut rest = source;
    let mut brackets = Vec::new();
    let mut chars = rest.char_indices();
    while let Some((at, ch)) = chars.next() {
        match ch {
            '\\' => {
                chars.next();
            }
            '\n' => brackets.clear(),
            '[' => brackets.push(at),
            ']' => {
                let Some(start) = brackets.pop() else {
                    continue;
                };
                if !rest[at..].starts_with("](vantry-thread://") {
                    continue;
                }
                let Some(link) = ThreadLink::parse(&rest[start..]) else {
                    continue;
                };
                if start > 0 {
                    parts.push(Inline::Text(rest[..start].into()));
                }
                rest = &rest[start + link.source.len()..];
                parts.push(Inline::Link {
                    text: link.title,
                    url: link.url.into(),
                });
                chars = rest.char_indices();
                brackets.clear();
            }
            _ => {}
        }
    }
    if !rest.is_empty() {
        parts.push(Inline::Text(rest.into()));
    }
    parts
}

pub(crate) fn escape_html(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

pub(crate) fn clipboard_html(source: &str) -> String {
    let mut html = String::new();
    for part in message_inlines(source) {
        match part {
            Inline::Text(text) => html.push_str(&escape_html(&text).replace('\n', "<br>")),
            Inline::Link { text, url } => html.push_str(&format!(
                "<a href=\"{}\">{}</a>",
                escape_html(&url),
                escape_html(&text)
            )),
            _ => unreachable!("message_inlines returns only literal text and thread links"),
        }
    }
    html
}

/// Convert HTML clipboard anchors to draft Markdown before a plain-text editor drops their hrefs.
pub(crate) fn pasted_thread_html(html: &str) -> Option<String> {
    use html5ever::tendril::TendrilSink;
    use markup5ever_rcdom::{Handle, NodeData, RcDom};
    fn visit(node: &Handle, out: &mut String, found: &mut bool) {
        let start = out.len();
        let mut block = false;
        let mut href = None;
        match &node.data {
            NodeData::Text { contents } => out.push_str(&contents.borrow()),
            NodeData::Element { name, attrs, .. } => match name.local.as_ref() {
                "head" | "script" | "style" => return,
                "br" => out.push('\n'),
                "p" | "div" | "li" => block = true,
                "a" => {
                    href = attrs
                        .borrow()
                        .iter()
                        .find(|a| a.name.local.as_ref() == "href")
                        .map(|a| a.value.to_string())
                }
                _ => {}
            },
            _ => {}
        }
        for child in node.children.borrow().iter() {
            visit(child, out, found);
        }
        if let Some(url) = href.filter(|u| vantry_protocol::ThreadTarget::parse_url(u).is_some()) {
            let label = out.split_off(start);
            out.push_str(&vantry_protocol::thread_markdown(&label, &url));
            *found = true;
        } else if block && !out.ends_with('\n') {
            out.push('\n');
        }
    }
    if !html.contains(vantry_protocol::THREAD_SCHEME) {
        return None;
    }
    let dom = html5ever::parse_document(RcDom::default(), Default::default()).one(html);
    let mut out = String::new();
    let mut found = false;
    visit(&dom.document, &mut out, &mut found);
    found.then(|| out.trim_end_matches('\n').to_owned())
}

pub(crate) fn clipboard_text<'a>(text: &'a str, html: &str) -> std::borrow::Cow<'a, str> {
    if !html.contains(vantry_protocol::THREAD_SCHEME)
        || (text.contains(vantry_protocol::THREAD_SCHEME)
            && message_inlines(text).iter().any(|i| matches!(i, Inline::Link { .. })))
    {
        return text.into();
    }
    pasted_thread_html(html).map_or_else(|| text.into(), Into::into)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn thread_link_clipboard_flavors_round_trip() {
        let url = "vantry-thread://th_B?environmentId=env_B";
        let md = vantry_protocol::thread_markdown("问 [B] & <C>", url);
        assert_eq!(clipboard_html(&md), format!("<a href=\"{url}\">问 [B] &amp; &lt;C&gt;</a>"));
        let html = format!("<html><body><p>读 <A href='{url}'><b>问 [B]</b> &amp; &lt;C&gt;</A> 后继续</p></body></html>");
        assert_eq!(pasted_thread_html(&html).unwrap(), format!("读 {md} 后继续"));
        assert_eq!(clipboard_text("问 [B] & <C>", &clipboard_html(&md)), md);
        assert_eq!(clipboard_text(&md, &clipboard_html(&md)), md);
        assert_eq!(clipboard_text("", &clipboard_html(&md)), md);
        assert!(pasted_thread_html("<a href='https://example.org'>标题</a>").is_none());
    }

    #[test]
    fn selected_skill_is_a_name_chip_and_reaches_pi_as_a_leading_command() {
        let src = "/skill:pdf [前次](vantry-thread://one) 请提取文字";
        let (skill, links, body) = draft_parts(src);
        assert_eq!(skill, Some("pdf"));
        assert_eq!(body, "请提取文字");
        assert_eq!(compose(skill, links.iter().map(|l| l.source), body), src);
        assert_eq!(with_body(src, "修改"), "/skill:pdf [前次](vantry-thread://one) 修改");
    }

    #[test]
    fn editing_and_sending_references_with_identical_titles() {
        let a = "[接手](vantry-thread://one)";
        let b = "[接手](vantry-thread://two)";
        let source = format!("{a} {b} **对比**");
        let (_, links, body) = draft_parts(&source);
        assert_eq!(body, "**对比**");
        assert_ne!(links[0].url, links[1].url);
        assert_eq!(with_body(&source, "再查"), format!("{a} {b} 再查"));
        assert_eq!(
            message_inlines(&source).last(),
            Some(&Inline::Text(" **对比**".into()))
        );
    }
}
