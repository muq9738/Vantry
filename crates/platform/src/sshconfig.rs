//! Passive OpenSSH configuration discovery and wildcard resolution.
use anyhow::{Result, ensure};
use std::{
    collections::HashSet,
    fs,
    io::{self, BufRead},
    path::{Component, Path, PathBuf},
};
pub use vantry_protocol::SSHHostInfo;
#[derive(Default, Debug)]
pub struct Host {
    pub host_name: String,
    pub user: String,
    pub port: u16,
    pub identity_files: Vec<PathBuf>,
    pub proxy_jump: String,
    pub forward_agent: bool,
}
fn words(s: &str) -> Vec<String> {
    let mut fields = Vec::new();
    let mut token = String::new();
    let mut quote = None;
    let mut escape = false;
    for c in s.chars() {
        if escape {
            if !c.is_whitespace() && !matches!(c, '\\' | '\'' | '"' | '#') {
                token.push('\\');
            }
            token.push(c);
            escape = false;
        } else if c == '\\' {
            escape = true;
        } else if Some(c) == quote {
            quote = None;
        } else if quote.is_some() {
            token.push(c);
        } else if matches!(c, '\'' | '"') {
            quote = Some(c);
        } else if c == '#' && token.is_empty() {
            break;
        } else if c.is_whitespace() {
            if !token.is_empty() {
                fields.push(std::mem::take(&mut token));
            }
        } else {
            token.push(c);
        }
    }
    if escape {
        token.push('\\');
    }
    if !token.is_empty() {
        fields.push(token);
    }
    fields
}
fn wildcard(pattern: &str, value: &str) -> bool {
    #[cfg(unix)]
    {
        use std::ffi::CString;
        let (Ok(p), Ok(v)) = (CString::new(pattern), CString::new(value)) else {
            return false;
        };
        unsafe { libc::fnmatch(p.as_ptr(), v.as_ptr(), 0) == 0 }
    }
    #[cfg(windows)]
    {
        let value: Vec<_> = value.chars().collect();
        let mut prev = vec![false; value.len() + 1];
        prev[0] = true;
        for c in pattern.chars() {
            let mut next = vec![false; prev.len()];
            if c == '*' {
                next[0] = prev[0];
            }
            for (i, &v) in value.iter().enumerate() {
                next[i + 1] = if c == '*' {
                    prev[i + 1] || next[i]
                } else {
                    prev[i] && (c == '?' || c == v)
                };
            }
            prev = next;
        }
        prev[value.len()]
    }
}
fn matches(patterns: &[String], alias: &str) -> bool {
    let alias = alias.to_ascii_lowercase();
    let mut positive = false;
    for pattern in patterns {
        let p = pattern.to_ascii_lowercase();
        if let Some(no) = p.strip_prefix('!') {
            if wildcard(no, &alias) {
                return false;
            }
        } else if wildcard(&p, &alias) {
            positive = true;
        }
    }
    positive
}
fn expand(home: &Path, pattern: &str) -> Vec<PathBuf> {
    let path = if let Some(tail) = pattern.strip_prefix("~/") {
        home.join(tail)
    } else {
        home.join(".ssh").join(pattern)
    };
    let mut paths = vec![PathBuf::new()];
    for c in path.components() {
        let text = c.as_os_str().to_string_lossy();
        if matches!(c, Component::Normal(_)) && text.contains(['*', '?', '[']) {
            let mut next = Vec::new();
            for p in paths {
                if let Ok(entries) = fs::read_dir(p) {
                    for e in entries.flatten() {
                        if wildcard(&text, &e.file_name().to_string_lossy()) {
                            next.push(e.path());
                        }
                    }
                }
            }
            paths = next;
        } else {
            for p in &mut paths {
                p.push(c);
            }
        }
    }
    paths.sort();
    paths
}
struct Parser<'a> {
    home: &'a Path,
    alias: Option<&'a str>,
    include_forges: bool,
    active: bool,
    in_match: bool,
    host: Host,
    assigned: HashSet<String>,
    block: Option<SSHHostInfo>,
    found: Vec<SSHHostInfo>,
    stack: HashSet<PathBuf>,
}
impl Parser<'_> {
    fn flush(&mut self) {
        if let Some(block) = self.block.take()
            && !self.found.iter().any(|b| b.alias == block.alias)
        {
            self.found.push(block);
        }
    }
    fn file(&mut self, path: &Path, depth: u8) -> Result<()> {
        if depth > 6 {
            return Ok(());
        }
        let file = match fs::File::open(path) {
            Err(e) if depth > 0 || e.kind() == io::ErrorKind::NotFound => return Ok(()),
            r => r?,
        };
        let canonical = path.canonicalize()?;
        if !self.stack.insert(canonical.clone()) {
            return Ok(());
        }
        for line in io::BufReader::new(file).lines() {
            let line = line?;
            ensure!(line.len() <= 1 << 20, "SSH config line too long");
            let line = line.trim();
            let Some(idx) = line.find(|c: char| c == '=' || c.is_whitespace()) else {
                continue;
            };
            let key = line[..idx].to_ascii_lowercase();
            let args = words(line[idx..].trim_start().trim_start_matches('='));
            let Some(value) = args.first() else { continue };
            match key.as_str() {
                "host" => {
                    self.flush();
                    self.in_match = false;
                    self.active = self.alias.is_none_or(|a| matches(&args, a));
                    if self.alias.is_none() {
                        let names: Vec<String> = args
                            .into_iter()
                            .filter(|s| {
                                !s.contains(['*', '?', '!', '['])
                                    && (self.include_forges
                                        || ![
                                            "github.com",
                                            "ssh.github.com",
                                            "gitlab.com",
                                            "bitbucket.org",
                                            "gitee.com",
                                            "codeberg.org",
                                        ]
                                        .contains(&s.to_ascii_lowercase().as_str()))
                            })
                            .collect();
                        if let Some(primary) = names.first() {
                            self.block = Some(SSHHostInfo {
                                alias: primary.clone(),
                                aliases: if names.len() > 1 { names } else { Vec::new() },
                                source_file: path
                                    .strip_prefix(self.home)
                                    .map(|p| Path::new("~").join(p))
                                    .unwrap_or_else(|_| path.into())
                                    .to_string_lossy()
                                    .into(),
                                ..Default::default()
                            });
                        }
                    }
                }
                "match" => {
                    self.flush();
                    self.active = false;
                    self.in_match = true;
                }
                "include" if self.active && !self.in_match => {
                    for pattern in args {
                        for p in expand(self.home, &pattern) {
                            self.file(&p, depth + 1)?;
                        }
                    }
                }
                _ if self.active => {
                    let none = value.eq_ignore_ascii_case("none");
                    let port = value.parse::<u16>().ok().filter(|n| *n > 0);
                    let yes =
                        ["yes", "true", "on", "1"].contains(&value.to_ascii_lowercase().as_str());
                    if let Some(b) = self.block.as_mut() {
                        match key.as_str() {
                            "user" => b.user = value.clone(),
                            "hostname" if !none => b.host_name = value.clone(),
                            "port" => b.port = port.filter(|n| *n != 22).unwrap_or_default() as i64,
                            "proxyjump" if !none => b.proxy_jump = value.clone(),
                            "identityfile" if !none => b.has_identity_file = true,
                            "forwardagent" => b.forward_agent = yes,
                            _ => (),
                        }
                    }
                    if self.alias.is_some() {
                        if key == "identityfile" && !none {
                            self.host.identity_files.push(
                                value
                                    .strip_prefix("~/")
                                    .map(|s| self.home.join(s))
                                    .unwrap_or_else(|| PathBuf::from(value)),
                            );
                        } else if self.assigned.insert(key.clone()) {
                            match key.as_str() {
                                "user" => self.host.user = value.clone(),
                                "hostname" if !none => {
                                    self.host.host_name =
                                        value.replace("%h", self.alias.unwrap_or_default())
                                }
                                "port" => self.host.port = port.unwrap_or(22),
                                "proxyjump" if !none => self.host.proxy_jump = value.clone(),
                                "forwardagent" => self.host.forward_agent = yes,
                                _ => (),
                            }
                        }
                    }
                }
                _ => (),
            }
        }
        self.stack.remove(&canonical);
        Ok(())
    }
}
fn parse<'a>(home: &'a Path, alias: Option<&'a str>, include_forges: bool) -> Result<Parser<'a>> {
    let mut p = Parser {
        home,
        alias,
        include_forges,
        active: true,
        in_match: false,
        host: Host {
            port: 22,
            host_name: alias.unwrap_or_default().into(),
            ..Default::default()
        },
        assigned: Default::default(),
        block: None,
        found: Vec::new(),
        stack: Default::default(),
    };
    p.file(&home.join(".ssh/config"), 0)?;
    p.flush();
    Ok(p)
}
pub fn discover(home: &Path, include_forges: bool) -> Result<Vec<SSHHostInfo>> {
    Ok(parse(home, None, include_forges)?.found)
}
pub fn resolve(home: &Path, alias: &str) -> Result<Host> {
    ensure!(
        !alias.is_empty()
            && !alias.starts_with('-')
            && !alias.chars().any(|c| c.is_control() || c.is_whitespace()),
        "invalid alias"
    );
    Ok(parse(home, Some(alias), true)?.host)
}
