use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use std::{
    cmp::{Ordering, Reverse},
    collections::{BTreeMap, BinaryHeap},
};

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
        let delta = delta.filter(|d| {
            full.as_ref()
                .is_none_or(|f| delta_worth_using(d.size, f.size, 1))
        });
        (full, delta)
    }
}

/// Minimum bandwidth-savings threshold for preferring incremental delta(s) over the full package.
///
/// A single-step delta must be at most 75% of the full archive size (saving >= 25%), and a
/// multi-step chain must be at most 65% (saving >= 35%) because each extra hop adds another
/// download round-trip plus a full on-disk bundle reconstruction and SHA-256 verification pass.
pub fn delta_worth_using(delta_bytes: u64, full_bytes: u64, steps: usize) -> bool {
    if delta_bytes == 0 || full_bytes == 0 {
        return true;
    }
    let max_pct: u128 = if steps > 1 { 65 } else { 75 };
    (delta_bytes as u128) * 100 <= (full_bytes as u128) * max_pct
}
/// One hop in a single- or multi-version incremental delta update chain.
#[derive(Debug, Clone)]
pub struct DeltaStep {
    pub from: String,
    pub to: String,
    pub asset: Asset,
}

/// Parse `{slug}-{from}-to-{to}-{platform}.delta` (case-insensitive) into normalized `(from, to)`.
pub fn parse_delta_asset_name(name: &str, slug: &str, platform: &str) -> Option<(String, String)> {
    let lower = name.to_ascii_lowercase();
    let prefix = format!("{}-", slug.to_ascii_lowercase());
    let suffix = format!("-{}.delta", platform.to_ascii_lowercase());
    let middle = lower.strip_prefix(&prefix)?.strip_suffix(&suffix)?;
    let (from, to) = middle.split_once("-to-")?;
    let from = normalize_version(from);
    let to = normalize_version(to);
    if from.is_empty() || to.is_empty() {
        return None;
    }
    Some((from.to_owned(), to.to_owned()))
}

const MAX_DELTA_HOPS: usize = 24;

/// Plan the smallest-byte incremental delta chain from `current` to `latest` across `releases`.
///
/// Supports both a direct `current -> latest` delta and multi-version chains such as
/// `0.2.13 -> 0.2.14 -> 0.2.15 -> 0.2.16`. Returns an empty list if no path exists or if the
/// combined delta size is not smaller than `full_size`.
pub fn plan_delta_chain(
    releases: &[Release],
    slug: &str,
    platform: &str,
    current: &str,
    latest: &str,
    full_size: Option<u64>,
) -> Vec<DeltaStep> {
    let cur = normalize_version(current).to_owned();
    let target = normalize_version(latest).to_owned();
    if cur.is_empty() || target.is_empty() || compare_versions(&cur, &target) != Ordering::Less {
        return Vec::new();
    }

    // Build adjacency list: from_version -> Vec<DeltaStep>
    let mut edges: BTreeMap<String, Vec<DeltaStep>> = BTreeMap::new();
    for release in releases {
        let rel_ver = release.version();
        if compare_versions(rel_ver, &cur) != Ordering::Greater
            || compare_versions(rel_ver, &target) == Ordering::Greater
        {
            continue;
        }
        for asset in &release.assets {
            let Some((from, to)) = parse_delta_asset_name(&asset.name, slug, platform) else {
                continue;
            };
            if compare_versions(&to, rel_ver) != Ordering::Equal {
                continue;
            }
            if compare_versions(&from, &cur) == Ordering::Less
                || compare_versions(&from, &to) != Ordering::Less
            {
                continue;
            }
            edges.entry(from.clone()).or_default().push(DeltaStep {
                from,
                to,
                asset: asset.clone(),
            });
        }
    }

    if edges.is_empty() {
        return Vec::new();
    }

    // Dijkstra minimizing (total_bytes, hop_count)
    let mut dist: BTreeMap<String, (u64, usize)> = BTreeMap::new();
    let mut prev: BTreeMap<String, DeltaStep> = BTreeMap::new();
    let mut heap: BinaryHeap<Reverse<(u64, usize, String)>> = BinaryHeap::new();

    dist.insert(cur.clone(), (0, 0));
    heap.push(Reverse((0, 0, cur.clone())));

    while let Some(Reverse((cost, hops, node))) = heap.pop() {
        if node == target {
            break;
        }
        if dist.get(&node).is_some_and(|&best| (cost, hops) > best) {
            continue;
        }
        if hops >= MAX_DELTA_HOPS {
            continue;
        }
        let Some(nexts) = edges.get(&node) else {
            continue;
        };
        for step in nexts {
            let next_cost = cost.saturating_add(step.asset.size);
            let next_hops = hops + 1;
            let better = dist
                .get(&step.to)
                .is_none_or(|&best| (next_cost, next_hops) < best);
            if better {
                dist.insert(step.to.clone(), (next_cost, next_hops));
                prev.insert(step.to.clone(), step.clone());
                heap.push(Reverse((next_cost, next_hops, step.to.clone())));
            }
        }
    }

    let Some(&(total_bytes, hops)) = dist.get(&target) else {
        return Vec::new();
    };
    if let Some(full) = full_size
        && !delta_worth_using(total_bytes, full, hops)
    {
        return Vec::new();
    }

    let mut chain = Vec::new();
    let mut cursor = target;
    while cursor != cur {
        let Some(step) = prev.remove(&cursor) else {
            return Vec::new();
        };
        cursor = step.from.clone();
        chain.push(step);
    }
    chain.reverse();
    chain
}

/// Combine release notes across all releases in `(current, latest]` (newest first).
pub fn combine_release_notes(releases: &[Release], current: &str, latest: &str) -> String {
    let mut relevant: Vec<&Release> = releases
        .iter()
        .filter(|r| {
            compare_versions(r.version(), current).is_gt()
                && !compare_versions(r.version(), latest).is_gt()
        })
        .collect();
    relevant.sort_by(|a, b| compare_versions(b.version(), a.version()));
    if relevant.is_empty() {
        return String::new();
    }
    let out = if relevant.len() == 1 {
        relevant[0].body.trim().to_owned()
    } else {
        let mut sections = Vec::new();
        for r in relevant {
            let body = r.body.trim();
            if body.is_empty() {
                continue;
            }
            let ver = r.version();
            let first = body.lines().next().unwrap_or("");
            if first.contains(ver) && first.starts_with('#') {
                sections.push(body.to_owned());
            } else {
                sections.push(format!("### v{ver}\n{body}"));
            }
        }
        sections.join("\n\n")
    };
    if out.len() > 64 << 10 {
        out[..out.floor_char_boundary(64 << 10)].to_owned()
    } else {
        out
    }
}

/// Accept GitHub release objects/arrays and custom manifests with a `releases` array.
/// Returns all eligible releases for `channel`, sorted ascending by version.
pub fn parse_releases(data: &[u8], channel: Channel) -> Result<Vec<Release>> {
    ensure!(data.len() <= 1 << 20, "release metadata too large");
    let v: serde_json::Value = serde_json::from_slice(data)?;
    let releases: Vec<Release> = if let Some(rs) = v.get("releases") {
        serde_json::from_value(rs.clone())?
    } else if v.is_array() {
        serde_json::from_value(v)?
    } else {
        vec![serde_json::from_value(v)?]
    };
    let mut filtered: Vec<Release> = releases
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
        .collect();
    filtered.sort_by(|a, b| compare_versions(a.version(), b.version()));
    let mut deduped: Vec<Release> = Vec::with_capacity(filtered.len());
    for r in filtered {
        if let Some(last) = deduped.last_mut()
            && compare_versions(last.version(), r.version()) == Ordering::Equal
        {
            if r.assets.len() >= last.assets.len() {
                *last = r;
            }
            continue;
        }
        deduped.push(r);
    }
    Ok(deduped)
}

/// Accept GitHub release objects/arrays and custom manifests with a `releases` array.
/// A custom release uses `version`, `channel`, `notes` and `assets[{name,size,url}]`.
pub fn parse_release(data: &[u8], channel: Channel) -> Result<Release> {
    parse_releases(data, channel)?
        .into_iter()
        .last()
        .context("no release for update channel")
}
