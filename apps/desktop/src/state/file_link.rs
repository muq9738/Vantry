//! Parses file links from Markdown (`file://...`, absolute paths, `~/...`,
//! and workspace-relative paths) including optional line/column anchors such as
//! `:42`, `:42:10`, `:42-55`, `#L42`, `#L42-L55`, `#L42C5-L55C10`.

use std::path::{Path, PathBuf};

/// A parsed local/workspace file reference from a Markdown link or image.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedFileLink {
    /// Clean path without `file://` prefix or line/fragment suffixes.
    pub path: String,
    /// Optional 1-based inclusive line range `(start, end)`.
    pub lines: Option<(usize, usize)>,
    /// Whether `path` is already an absolute filesystem path.
    pub is_absolute: bool,
    /// Whether `path` started with `~/` (caller resolves against `$HOME`).
    pub is_home_relative: bool,
}

/// Parses a Markdown link URL into a file reference if it refers to a local or
/// workspace file. Returns `None` for web URLs, custom app schemes, or pure
/// fragment links (`#heading`).
pub fn parse_file_link(raw: &str) -> Option<ParsedFileLink> {
    let trimmed = raw.trim().trim_start_matches('<').trim_end_matches('>').trim();
    if trimmed.is_empty() || trimmed.starts_with('#') {
        return None;
    }

    if let Some(rest) = strip_file_scheme(trimmed) {
        let _ = rest;
        // Split fragment before URL-parsing so we preserve `#L10-L20`.
        let (base_url, fragment) = match trimmed.split_once('#') {
            Some((b, f)) => (b, Some(f)),
            None => (trimmed, None),
        };
        let (base_no_query, _) = base_url.split_once('?').unwrap_or((base_url, ""));
        let parsed_url = url::Url::parse(base_no_query).ok()?;
        let file_path = parsed_url.to_file_path().ok()?;
        let path_str = file_path.to_string_lossy();
        let (clean, colon_lines) = split_colon_lines(&path_str);
        let lines = fragment.and_then(parse_fragment_lines).or(colon_lines);
        if clean.is_empty() {
            return None;
        }
        return Some(ParsedFileLink {
            path: clean.to_owned(),
            lines,
            is_absolute: true,
            is_home_relative: false,
        });
    }

    // Reject non-file URI schemes (`https://`, `mailto:`, `vantry-thread://`, `data:`, ...),
    // while allowing Windows drive letters like `C:\foo` or `C:/foo` and `path:42`.
    if has_uri_scheme(trimmed) {
        return None;
    }

    let (without_fragment, fragment) = match trimmed.split_once('#') {
        Some((b, f)) => (b, Some(f)),
        None => (trimmed, None),
    };
    let (without_query, _) = without_fragment
        .split_once('?')
        .unwrap_or((without_fragment, ""));
    let (clean_raw, colon_lines) = split_colon_lines(without_query);
    let lines = fragment.and_then(parse_fragment_lines).or(colon_lines);
    let clean_raw = clean_raw.trim();
    if clean_raw.is_empty() {
        return None;
    }

    let is_home = clean_raw == "~" || clean_raw.starts_with("~/");
    let is_abs = !is_home && is_absolute_path(clean_raw);
    let normalized = if !is_abs && !is_home {
        strip_leading_dot_slash(clean_raw).to_owned()
    } else {
        clean_raw.to_owned()
    };
    if normalized.is_empty() {
        return None;
    }

    Some(ParsedFileLink {
        path: normalized,
        lines,
        is_absolute: is_abs,
        is_home_relative: is_home,
    })
}

/// Resolves a parsed link against an optional workspace `root` and optional `home` directory.
/// Returns `(workspace_relative, local_absolute)`:
/// - `workspace_relative` is `Some` when the target is inside the workspace (used for Dock / remote Core).
/// - `local_absolute` is `Some` when the target resolves to a local filesystem path.
pub fn resolve_target(
    link: &ParsedFileLink,
    root: &str,
    home: Option<&Path>,
    remote: bool,
) -> (Option<String>, Option<PathBuf>) {
    let root_trimmed = root.trim();
    let root_path = (!root_trimmed.is_empty()).then(|| Path::new(root_trimmed));

    if link.is_home_relative {
        if remote {
            return (None, None);
        }
        let expanded = home.map(|h| {
            let rest = link.path.strip_prefix("~/").unwrap_or("");
            if rest.is_empty() {
                h.to_path_buf()
            } else {
                h.join(rest)
            }
        });
        let rel = expanded
            .as_deref()
            .and_then(|abs| relative_inside(abs, root_path));
        return (rel, expanded);
    }

    if link.is_absolute {
        let abs = PathBuf::from(&link.path);
        let rel = relative_inside(&abs, root_path);
        let local = (!remote).then_some(abs);
        return (rel, local);
    }

    // Workspace-relative path (`src/sse.ts`, `screenshots/list.png`, `../sibling/a.rs`).
    if let Some(root_p) = root_path {
        let candidate = root_p.join(&link.path);
        let rel = if !link.path.split('/').any(|s| s == "..") {
            Some(link.path.replace('\\', "/"))
        } else {
            relative_inside(&candidate, Some(root_p))
        };
        let local = (!remote).then_some(candidate);
        (rel, local)
    } else {
        (None, None)
    }
}

/// Percent-decodes a path string when a literal file wasn't found (e.g. `my%20file.png`).
pub fn percent_decode_path(input: &str) -> Option<String> {
    if !input.contains('%') {
        return None;
    }
    let bytes = input.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    let mut changed = false;
    while i < bytes.len() {
        if bytes[i] == b'%'
            && i + 2 < bytes.len()
            && let (Some(hi), Some(lo)) = (hex_val(bytes[i + 1]), hex_val(bytes[i + 2]))
        {
            out.push((hi << 4) | lo);
            i += 3;
            changed = true;
            continue;
        }
        out.push(bytes[i]);
        i += 1;
    }
    changed.then(|| String::from_utf8_lossy(&out).into_owned())
}

fn hex_val(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

fn relative_inside(abs: &Path, root: Option<&Path>) -> Option<String> {
    let root = root?;
    if let Ok(rel) = abs.strip_prefix(root)
        && !rel.as_os_str().is_empty()
    {
        return Some(rel.to_string_lossy().replace('\\', "/"));
    }
    // Try canonical paths when symlinks (e.g. /var -> /private/var on macOS) differ.
    if let (Ok(c_abs), Ok(c_root)) = (std::fs::canonicalize(abs), std::fs::canonicalize(root))
        && let Ok(rel) = c_abs.strip_prefix(&c_root)
        && !rel.as_os_str().is_empty()
    {
        return Some(rel.to_string_lossy().replace('\\', "/"));
    }
    None
}

fn strip_file_scheme(s: &str) -> Option<&str> {
    let prefix = s.get(..7)?;
    prefix.eq_ignore_ascii_case("file://").then(|| &s[7..])
}

fn has_uri_scheme(s: &str) -> bool {
    let Some((scheme, rest)) = s.split_once(':') else {
        return false;
    };
    if scheme.is_empty() {
        return false;
    }
    // Windows drive letter: `C:\...` or `C:/...`.
    if scheme.len() == 1
        && scheme.as_bytes()[0].is_ascii_alphabetic()
        && (rest.starts_with('\\') || rest.starts_with('/'))
    {
        return false;
    }
    // Relative path with line suffix like `Makefile:42` or `README:10-20`.
    if split_colon_lines(s).1.is_some() && !rest.starts_with("//") {
        return false;
    }
    let bytes = scheme.as_bytes();
    bytes[0].is_ascii_alphabetic()
        && bytes[1..]
            .iter()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'+' | b'-' | b'.'))
        && (rest.starts_with("//")
            || matches!(
                scheme.to_ascii_lowercase().as_str(),
                "mailto" | "tel" | "data" | "javascript" | "about" | "blob"
            ))
}

fn is_absolute_path(s: &str) -> bool {
    if s.starts_with('/') {
        return true;
    }
    let b = s.as_bytes();
    b.len() >= 3
        && b[0].is_ascii_alphabetic()
        && b[1] == b':'
        && (b[2] == b'\\' || b[2] == b'/')
}

fn strip_leading_dot_slash(mut s: &str) -> &str {
    while let Some(rest) = s.strip_prefix("./") {
        s = rest;
    }
    s
}

/// Parses `#L42`, `#L42-L55`, `#L42-55`, `#L42C5-L55C10`, `#42`, `#42-55`.
fn parse_fragment_lines(fragment: &str) -> Option<(usize, usize)> {
    let frag = fragment.trim();
    if frag.is_empty() {
        return None;
    }
    let parse_one = |part: &str| -> Option<usize> {
        let p = part
            .trim()
            .strip_prefix('L')
            .or_else(|| part.trim().strip_prefix('l'))
            .unwrap_or(part.trim());
        let digits: String = p.chars().take_while(|c| c.is_ascii_digit()).collect();
        let rest = &p[digits.len()..];
        // Allow optional column suffix `C12` or `:12`, reject random headings like `L1-overview`.
        if !rest.is_empty() {
            let col = rest
                .strip_prefix('C')
                .or_else(|| rest.strip_prefix('c'))
                .or_else(|| rest.strip_prefix(':'))?;
            if col.is_empty() || !col.chars().all(|c| c.is_ascii_digit()) {
                return None;
            }
        }
        let n = digits.parse::<usize>().ok()?;
        (n > 0).then_some(n)
    };
    if let Some((a, b)) = frag.split_once('-') {
        let start = parse_one(a)?;
        let end = parse_one(b)?;
        Some((start.min(end), start.max(end)))
    } else {
        let n = parse_one(frag)?;
        Some((n, n))
    }
}

/// Strips trailing `:line`, `:line:col`, `:line-end`, or `:line:col-end:col`
/// from a file path while preserving Windows drive prefixes (`C:\...`).
fn split_colon_lines(path: &str) -> (&str, Option<(usize, usize)>) {
    // Handle `path:start:col` (e.g. `src/main.rs:42:10`).
    if let Some((before_col, col)) = path.rsplit_once(':')
        && !col.is_empty()
        && col.chars().all(|c| c.is_ascii_digit())
        && let Some((before_line, line)) = before_col.rsplit_once(':')
        && !before_line.is_empty()
        && !is_drive_prefix(before_line)
        && let Some(n) = parse_positive(line)
    {
        return (before_line, Some((n, n)));
    }
    // Handle `path:start-end` or `path:start`.
    if let Some((before, suffix)) = path.rsplit_once(':')
        && !before.is_empty()
        && !is_drive_prefix(before)
    {
        if let Some((a, b)) = suffix.split_once('-')
            && let (Some(start), Some(end)) = (parse_positive(a), parse_positive(b))
        {
            return (before, Some((start.min(end), start.max(end))));
        }
        if let Some(n) = parse_positive(suffix) {
            return (before, Some((n, n)));
        }
    }
    (path, None)
}

fn is_drive_prefix(s: &str) -> bool {
    s.len() == 1 && s.as_bytes()[0].is_ascii_alphabetic()
}

fn parse_positive(s: &str) -> Option<usize> {
    if s.is_empty() || !s.chars().all(|c| c.is_ascii_digit()) {
        return None;
    }
    let n = s.parse::<usize>().ok()?;
    (n > 0).then_some(n)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_file_urls_with_fragments_and_spaces() {
        let link = parse_file_link("file:///Users/demo/my%20shots/list.png").unwrap();
        assert_eq!(link.path, "/Users/demo/my shots/list.png");
        assert_eq!(link.lines, None);
        assert!(link.is_absolute);

        let code = parse_file_link("file:///Users/demo/src/sse.ts#L12-L34").unwrap();
        assert_eq!(code.path, "/Users/demo/src/sse.ts");
        assert_eq!(code.lines, Some((12, 34)));

        let col = parse_file_link("file:///Users/demo/src/sse.ts#L12C4-L18C9").unwrap();
        assert_eq!(col.path, "/Users/demo/src/sse.ts");
        assert_eq!(col.lines, Some((12, 18)));
    }

    #[test]
    fn parses_absolute_relative_and_home_paths_with_line_ranges() {
        let rel = parse_file_link("src/utils/sse.ts:42:10").unwrap();
        assert_eq!(rel.path, "src/utils/sse.ts");
        assert_eq!(rel.lines, Some((42, 42)));
        assert!(!rel.is_absolute);

        let dot = parse_file_link("./screenshots/design-list.png").unwrap();
        assert_eq!(dot.path, "screenshots/design-list.png");
        assert_eq!(dot.lines, None);

        let range = parse_file_link("apps/desktop/src/lib.rs:100-120").unwrap();
        assert_eq!(range.path, "apps/desktop/src/lib.rs");
        assert_eq!(range.lines, Some((100, 120)));

        let home = parse_file_link("~/Desktop/shot.png").unwrap();
        assert_eq!(home.path, "~/Desktop/shot.png");
        assert!(home.is_home_relative);

        let hash = parse_file_link("/tmp/report/sse.ts#L8").unwrap();
        assert_eq!(hash.path, "/tmp/report/sse.ts");
        assert_eq!(hash.lines, Some((8, 8)));
        assert!(hash.is_absolute);
    }

    #[test]
    fn rejects_web_and_special_uris() {
        for url in [
            "",
            "#section",
            "https://example.com/a.png",
            "http://localhost:8888/3dply",
            "mailto:a@b.com",
            "vantry-thread://th_123",
            "vantry-attachment://file_123",
            "data:image/png;base64,AAAA",
        ] {
            assert_eq!(parse_file_link(url), None, "should reject {url}");
        }
    }

    #[test]
    fn resolves_workspace_relative_and_external_paths() {
        let rel = parse_file_link("screenshots/list.png").unwrap();
        let (ws, local) = resolve_target(&rel, "/repo", None, false);
        assert_eq!(ws.as_deref(), Some("screenshots/list.png"));
        assert_eq!(local, Some(PathBuf::from("/repo/screenshots/list.png")));

        let ext = parse_file_link("/tmp/shot.png").unwrap();
        let (ws, local) = resolve_target(&ext, "/repo", None, false);
        assert_eq!(ws, None);
        assert_eq!(local, Some(PathBuf::from("/tmp/shot.png")));

        let inside = parse_file_link("/repo/src/sse.ts:15").unwrap();
        let (ws, local) = resolve_target(&inside, "/repo", None, false);
        assert_eq!(ws.as_deref(), Some("src/sse.ts"));
        assert_eq!(local, Some(PathBuf::from("/repo/src/sse.ts")));
    }

    #[test]
    fn percent_decodes_spaces_and_unicode() {
        assert_eq!(
            percent_decode_path("docs/%E6%88%AA%E5%9B%BE%201.png").as_deref(),
            Some("docs/截图 1.png")
        );
        assert_eq!(percent_decode_path("plain.png"), None);
    }
}
