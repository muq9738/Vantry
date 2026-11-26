//! Session references as exchanged between the clipboard, messages and Core tools.

pub const THREAD_SCHEME: &str = "vantry-thread://";

/// A thread URL, optionally qualified with the owning Core's environment.
#[derive(Debug, PartialEq, Eq)]
pub struct ThreadTarget<'a> {
    pub id: &'a str,
    pub environment: Option<&'a str>,
}

impl<'a> ThreadTarget<'a> {
    pub fn parse_url(url: &'a str) -> Option<Self> {
        let target = url.strip_prefix(THREAD_SCHEME)?.split('#').next()?;
        let (id, query) = target.split_once('?').unwrap_or((target, ""));
        if id.is_empty()
            || !id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
            || url.contains(|c: char| c.is_whitespace() || matches!(c, '(' | ')'))
        {
            return None;
        }
        let environment = query
            .split('&')
            .find_map(|p| p.strip_prefix("environmentId="))
            .filter(|s| !s.is_empty());
        Some(Self { id, environment })
    }
}

pub fn thread_markdown(title: &str, url: &str) -> String {
    let label = title
        .replace('\\', "\\\\")
        .replace('[', "\\[")
        .replace(']', "\\]")
        .replace(['\r', '\n'], " ");
    format!("[{label}]({url})")
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThreadLink<'a> {
    pub source: &'a str,
    pub title: String,
    pub url: &'a str,
}

/// Recognize a complete reference at the start of a string, including old labels with nested brackets.
impl<'a> ThreadLink<'a> {
    pub fn parse(text: &'a str) -> Option<Self> {
        let mut chars = text.strip_prefix('[')?.char_indices().peekable();
        let mut depth = 0;
        let mut title = String::new();
        while let Some((i, ch)) = chars.next() {
            match ch {
                '\n' | '\r' => return None,
                '\\' if chars
                    .peek()
                    .is_some_and(|(_, c)| matches!(c, '\\' | '[' | ']')) =>
                {
                    title.push(chars.next()?.1);
                    continue;
                }
                '[' => depth += 1,
                ']' if depth == 0 => {
                    let rest = text[i + 2..].strip_prefix('(')?;
                    let (url, _) = rest.split_once(')')?;
                    ThreadTarget::parse_url(url)?;
                    let source = &text[..i + 4 + url.len()];
                    return Some(Self { source, title, url });
                }
                ']' => depth -= 1,
                _ => {}
            }
            title.push(ch);
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reference_round_trip() {
        let title = "接手 [Rust] \\ 名称 ] 🦀";
        let url = "vantry-thread://th_old?environmentId=remote";
        let md = thread_markdown(title, url);
        let prompt = format!("{md} 继续");
        let link = ThreadLink::parse(&prompt).unwrap();
        assert_eq!(
            (link.title.as_str(), link.url, link.source),
            (title, url, md.as_str())
        );
        assert!(ThreadLink::parse("[接手(接手 Rust…").is_none());
        assert_eq!(
            ThreadTarget::parse_url(link.url),
            Some(ThreadTarget {
                id: "th_old",
                environment: Some("remote")
            })
        );
        assert_eq!(
            ThreadTarget::parse_url("vantry-thread://one#fragment")
                .unwrap()
                .environment,
            None
        );
        assert!(ThreadTarget::parse_url("vantry-thread://?environmentId=remote").is_none());
        assert!(ThreadLink::parse("[目录](/some/path)").is_none());
    }
}
