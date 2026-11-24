use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use std::cmp::Ordering;

pub fn normalize_version(s: &str) -> &str {
    let s = s.trim();
    let s = s.strip_prefix('v').unwrap_or(s);
    s.strip_prefix('V').unwrap_or(s)
}
fn number(s: &str) -> u32 {
    s.bytes().take_while(u8::is_ascii_digit).fold(0u32, |n, b| {
        n.saturating_mul(10)
            .saturating_add((b - b'0') as u32)
            .min(1 << 30)
    })
}
fn parts(s: &str) -> ([u32; 3], Option<&str>) {
    let s = normalize_version(s).split('+').next().unwrap();
    let (core, pre) = s.split_once('-').unwrap_or((s, ""));
    let mut nums = [0; 3];
    for (dst, p) in nums.iter_mut().zip(core.split('.')) {
        *dst = number(p);
    }
    (nums, (!pre.is_empty()).then_some(pre))
}
/// Go-compatible comparison, including shortened versions, v-prefixes and build metadata.
pub fn compare_versions(a: &str, b: &str) -> Ordering {
    let (x, pre) = parts(a);
    let (y, other) = parts(b);
    x.cmp(&y).then_with(|| match (pre, other) {
        (None, None) => Ordering::Equal,
        (None, _) => Ordering::Greater,
        (_, None) => Ordering::Less,
        (Some(a), Some(b)) => {
            let mut l = a.split('.');
            let mut r = b.split('.');
            loop {
                match (l.next(), r.next()) {
                    (Some(x), Some(y)) => {
                        let numeric =
                            |s: &str| !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit());
                        let cmp = match (numeric(x), numeric(y)) {
                            (true, true) => number(x).cmp(&number(y)),
                            (true, false) => Ordering::Less,
                            (false, true) => Ordering::Greater,
                            _ => x.cmp(y),
                        };
                        if cmp != Ordering::Equal {
                            break cmp;
                        }
                    }
                    (x, y) => break x.is_some().cmp(&y.is_some()),
                }
            }
        }
    })
}
#[derive(Clone, Copy, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Channel {
    #[default]
    Stable,
    Beta,
    Nightly,
}
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct Asset {
    pub name: String,
    pub size: u64,
    pub url: String,
    #[serde(alias = "download_url")]
    pub browser_download_url: String,
}
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct Release {
    #[serde(alias = "version")]
    pub tag_name: String,
    pub name: String,
    #[serde(alias = "notes")]
    pub body: String,
    pub html_url: String,
    pub published_at: Option<String>,
    pub draft: bool,
    pub prerelease: bool,
    pub channel: Option<Channel>,
    pub assets: Vec<Asset>,
}
impl Release {
    pub fn version(&self) -> &str {
        normalize_version(if self.tag_name.trim().is_empty() {
            &self.name
        } else {
            &self.tag_name
        })
    }
    pub fn assets_for(
        &self,
        slug: &str,
        platform: &str,
        from: &str,
    ) -> (Option<Asset>, Option<Asset>) {
        let slug = slug.to_ascii_lowercase();
        let version = self.version();
        let find = |name: String| {
            self.assets
                .iter()
                .find(|a| a.name.eq_ignore_ascii_case(&name))
                .cloned()
        };
        let delta = find(format!(
            "{slug}-{}-to-{version}-{platform}.delta",
            normalize_version(from)
        ));
        let mut full = ["tar.gz", "zip"]
            .into_iter()
            .find_map(|ext| find(format!("{slug}-{version}-{platform}.{ext}")));
        if full.is_none()
            && let Some(arch) = platform.strip_prefix("darwin-")
        {
            full = find(format!("{slug}-{version}-{arch}.dmg"))
                .or_else(|| find(format!("{slug}-{version}.dmg")));
        }
        if full.is_none() {
            full = [".tar.gz", ".zip", ".dmg"].into_iter().find_map(|ext| {
                self.assets
                    .iter()
                    .find(|a| {
                        let name = a.name.to_ascii_lowercase();
                        name.starts_with(&format!("{slug}-"))
                            && !name.contains(".json")
                            && !(slug == "vantry"
                                && (name.starts_with("vantry-service-")
                                    || name.starts_with("vantry-relay-")))
                            && name.ends_with(ext)
                            && (name.contains(platform)
                                || (ext == ".dmg"
                                    && platform.strip_prefix("darwin-").is_some_and(|arch| {
                                        name.ends_with(&format!("-{arch}.dmg"))
                                    })))
                    })
                    .cloned()
            });
        }
        (full, delta)
    }
}
/// Accept GitHub release objects/arrays and custom manifests with a `releases` array.
/// A custom release uses `version`, `channel`, `notes` and `assets[{name,size,url}]`.
pub fn parse_release(data: &[u8], channel: Channel) -> Result<Release> {
    ensure!(data.len() <= 1 << 20, "release metadata too large");
    let v: serde_json::Value = serde_json::from_slice(data)?;
    let releases: Vec<Release> = if let Some(rs) = v.get("releases") {
        serde_json::from_value(rs.clone())?
    } else if v.is_array() {
        serde_json::from_value(v)?
    } else {
        vec![serde_json::from_value(v)?]
    };
    releases
        .into_iter()
        .filter(|r| {
            if r.draft || r.version().is_empty() {
                return false;
            }
            let (_, pre) = parts(r.version());
            let actual = r.channel.unwrap_or_else(|| {
                if pre.is_some_and(|p| p.starts_with("nightly") || p.starts_with("alpha")) {
                    Channel::Nightly
                } else if r.prerelease || pre.is_some() {
                    Channel::Beta
                } else {
                    Channel::Stable
                }
            });
            match channel {
                Channel::Stable => actual == Channel::Stable && !r.prerelease && pre.is_none(),
                Channel::Beta => actual != Channel::Nightly,
                Channel::Nightly => true,
            }
        })
        .max_by(|a, b| compare_versions(a.version(), b.version()))
        .context("no release for update channel")
}
