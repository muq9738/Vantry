//! Block mappings and sequences used by relay deployment YAML files.
use anyhow::{Result, anyhow, bail, ensure};
use serde_json::{Map, Value};
#[derive(Clone, Copy)]
struct Line<'a> {
    indent: usize,
    text: &'a str,
}
fn split(s: &str, sep: char) -> Option<(&str, &str)> {
    let mut quote = None;
    let mut escape = false;
    let mut depth = 0i32;
    for (i, c) in s.char_indices() {
        if escape {
            escape = false;
            continue;
        }
        if let Some(q) = quote {
            if c == '\\' && q == '"' {
                escape = true
            } else if c == q {
                quote = None
            }
            continue;
        }
        match c {
            '\'' | '"' => quote = Some(c),
            '[' | '{' => depth += 1,
            ']' | '}' => depth -= 1,
            _ if c == sep && depth == 0 => return Some((&s[..i], &s[i + c.len_utf8()..])),
            _ => {}
        }
    }
    None
}
fn scalar(s: &str) -> Result<Value> {
    scalar_at(s, 0)
}
fn scalar_at(s: &str, depth: u8) -> Result<Value> {
    ensure!(depth < 32, "YAML nesting limit");
    let s = s.trim();
    if s.starts_with('"') {
        return Ok(Value::String(serde_json::from_str(s)?));
    }
    if s.starts_with('\'') {
        ensure!(s.len() >= 2 && s.ends_with('\''), "invalid YAML quote");
        return Ok(Value::String(s[1..s.len() - 1].replace("''", "'")));
    }
    if s.starts_with('[') || s.starts_with('{') {
        let map = s.starts_with('{');
        ensure!(
            s.ends_with(if map { '}' } else { ']' }),
            "invalid flow collection"
        );
        let mut rest = &s[1..s.len() - 1];
        let mut list = vec![];
        let mut object = Map::new();
        while !rest.trim().is_empty() {
            let (item, next) = split(rest, ',').unwrap_or((rest, ""));
            if map {
                let (k, v) = split(item, ':').ok_or_else(|| anyhow!("invalid field"))?;
                insert(&mut object, k, scalar_at(v, depth + 1)?)?
            } else {
                list.push(scalar_at(item, depth + 1)?)
            }
            rest = next
        }
        return Ok(if map {
            object.into()
        } else {
            Value::Array(list)
        });
    }
    if s.starts_with(['&', '*', '!', '|', '>']) {
        bail!("unsupported YAML scalar")
    }
    Ok(match s {
        "null" | "~" => Value::Null,
        "true" => true.into(),
        "false" => false.into(),
        _ => s
            .parse::<i64>()
            .map(Value::from)
            .unwrap_or_else(|_| Value::String(s.into())),
    })
}
fn insert(map: &mut Map<String, Value>, k: &str, v: Value) -> Result<()> {
    let k = k.trim();
    let key = if k.starts_with(['"', '\'']) {
        scalar(k)?
            .as_str()
            .ok_or_else(|| anyhow!("invalid key"))?
            .to_owned()
    } else {
        k.into()
    };
    ensure!(
        !key.is_empty() && map.insert(key, v).is_none(),
        "invalid or duplicate YAML field"
    );
    Ok(())
}
fn mapping(s: &str) -> Option<(&str, &str)> {
    let mut offset = 0;
    loop {
        let (a, b) = split(&s[offset..], ':')?;
        offset += a.len() + 1;
        if b.is_empty() || b.starts_with(char::is_whitespace) {
            return Some((&s[..offset - 1], b.trim()));
        }
    }
}
fn block(lines: &[Line], index: &mut usize, indent: usize, depth: u8) -> Result<Value> {
    ensure!(depth < 32, "YAML nesting limit");
    let seq = lines[*index].text.starts_with("- ") || lines[*index].text == "-";
    let mut list = vec![];
    let mut map = Map::new();
    while let Some(line) = lines.get(*index).filter(|l| l.indent == indent) {
        let text = line.text;
        if seq && text != "-" && !text.starts_with("- ") {
            break;
        }
        *index += 1;
        let child = |i: &mut usize| -> Result<Value> {
            if let Some(next) = lines.get(*i).filter(|l| l.indent > indent) {
                block(lines, i, next.indent, depth + 1)
            } else {
                Ok(Value::Null)
            }
        };
        if seq {
            let v = text
                .strip_prefix("- ")
                .or_else(|| (text == "-").then_some(""))
                .ok_or_else(|| anyhow!("expected YAML sequence"))?;
            if v.is_empty() {
                list.push(child(index)?)
            } else if let Some((k, v)) = mapping(v) {
                let mut m = Map::new();
                let xi = indent + 2;
                let value = if v.is_empty() && lines.get(*index).is_some_and(|l| l.indent > xi) {
                    let n = lines[*index].indent;
                    block(lines, index, n, depth + 1)?
                } else {
                    scalar(v)?
                };
                insert(&mut m, k, value)?;
                if lines.get(*index).is_some_and(|l| l.indent == xi) {
                    let tail = block(lines, index, xi, depth + 1)?;
                    for (k, v) in tail
                        .as_object()
                        .ok_or_else(|| anyhow!("expected YAML mapping"))?
                    {
                        insert(&mut m, k, v.clone())?
                    }
                }
                list.push(m.into())
            } else {
                list.push(scalar(v)?);
            }
        } else {
            let (k, v) = mapping(text).ok_or_else(|| anyhow!("expected YAML field"))?;
            let value = if v.is_empty() {
                if lines
                    .get(*index)
                    .is_some_and(|l| l.indent == indent && l.text.starts_with("- "))
                {
                    block(lines, index, indent, depth + 1)?
                } else {
                    child(index)?
                }
            } else {
                scalar(v)?
            };
            insert(&mut map, k, value)?;
        }
    }
    Ok(if seq { Value::Array(list) } else { map.into() })
}
pub fn parse(s: &str) -> Result<Value> {
    let mut lines = vec![];
    for line in s.lines() {
        let trimmed = line.trim_start_matches(' ');
        let indent = line.len() - trimmed.len();
        ensure!(!trimmed.starts_with('\t'), "invalid indentation");
        let mut text = trimmed.trim_end();
        if let Some((before, _)) = split(text, '#') {
            if before.is_empty() || before.ends_with(char::is_whitespace) {
                text = before.trim_end();
            }
        }
        if !text.is_empty() && text != "---" && text != "..." {
            lines.push(Line { indent, text })
        }
    }
    let mut i = 0;
    let indent = lines.first().ok_or_else(|| anyhow!("empty YAML"))?.indent;
    let v = block(&lines, &mut i, indent, 0)?;
    ensure!(i == lines.len(), "unexpected indentation");
    Ok(v)
}
