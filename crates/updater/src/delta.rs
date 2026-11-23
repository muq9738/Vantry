use crate::inflate::Inflater as DeflateDecoder;
use crate::{MAX_ASSET, MAX_FILE, MAX_TREE, normalize_version, patch, tree};
use anyhow::{Context, Result, ensure};
use flate2::{Compression, write::DeflateEncoder};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet},
    fs::{self, File},
    io::{self, Read, Seek, SeekFrom, Write},
    path::Path,
};
const MAGIC: &[u8] = b"vantry-delta-1\n";
const MAX_INDEX: u64 = 16 << 20;
const TICKET: &str = "Contents/CodeResources";
#[derive(Serialize, Deserialize)]
struct Index {
    from: String,
    version: String,
    entries: Option<Vec<Entry>>,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
struct Entry {
    path: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    dir: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    link: String,
    #[serde(skip_serializing_if = "zero")]
    mode: u32,
    #[serde(skip_serializing_if = "zero")]
    size: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    sha256: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    from: String,
    #[serde(skip_serializing_if = "zero")]
    data: u64,
}
fn zero<T: Default + PartialEq>(n: &T) -> bool {
    *n == T::default()
}
/// Required by the original v1 per-file reconstruction contract.
fn file_sum(file: &mut File) -> Result<String> {
    let mut h = Sha256::new();
    io::copy(file, &mut h)?;
    Ok(format!("{:x}", h.finalize()))
}
/// Write a Go-compatible v1 delta. Returns the uncompressed target file bytes.
pub fn write_delta(
    mut out: impl Write,
    from: &str,
    to: &str,
    old: &Path,
    new: &Path,
) -> Result<u64> {
    let (from, to) = (normalize_version(from), normalize_version(to));
    ensure!(!from.is_empty() && !to.is_empty(), "missing version");
    let mut old_paths = HashMap::new();
    let mut by_sum = HashMap::new();
    tree::walk(old, |rel, meta| {
        let name = tree::wire_path(rel)?;
        if meta.is_file() && name != TICKET {
            let sum = file_sum(&mut File::open(old.join(rel))?)?;
            by_sum.entry(sum.clone()).or_insert_with(|| name.clone());
            old_paths.insert(name, sum);
        }
        Ok(())
    })?;
    let mut blobs = tempfile::tempfile()?;
    let mut entries = Vec::new();
    let mut total = 0;
    tree::walk(new, |rel, meta| {
        let mut e = Entry {
            path: tree::wire_path(rel)?,
            ..Entry::default()
        };
        tree::relative(&e.path)?;
        let path = new.join(rel);
        if meta.is_dir() {
            e.dir = true;
        } else if meta.is_symlink() {
            e.link = tree::wire_path(&fs::read_link(path)?)?;
            tree::validate_link(new, rel, &e.link)?;
        } else {
            ensure!(
                meta.is_file() && meta.len() <= MAX_FILE,
                "unsupported entry {}",
                e.path
            );
            e.mode = tree::mode(meta);
            e.size = meta.len();
            total += e.size;
            ensure!(total <= MAX_TREE, "target too large");
            let mut input = File::open(path)?;
            e.sha256 = file_sum(&mut input)?;
            if e.path != TICKET && old_paths.get(&e.path) == Some(&e.sha256) {
                e.from = e.path.clone();
            } else if let Some(name) = by_sum.get(&e.sha256).filter(|_| e.path != TICKET) {
                e.from = name.clone();
            } else {
                let start = blobs.stream_position()?;
                input.rewind()?;
                let mut enc = DeflateEncoder::new(&mut blobs, Compression::best());
                ensure!(io::copy(&mut input, &mut enc)? == e.size, "input changed");
                enc.finish()?;
                e.data = blobs.stream_position()? - start;
                if old_paths.contains_key(&e.path) && e.size <= 256 << 20 {
                    let mut base = File::open(old.join(rel))?;
                    if (1..=256 << 20).contains(&base.metadata()?.len()) {
                        let mut candidate = tempfile::tempfile()?;
                        patch::encode(&mut base, &mut input, &mut candidate)?;
                        if candidate.stream_position()? < e.data {
                            candidate.rewind()?;
                            blobs.seek(SeekFrom::Start(start))?;
                            e.data = io::copy(&mut candidate, &mut blobs)?;
                            blobs.set_len(start + e.data)?;
                            e.from = e.path.clone();
                        }
                    }
                }
                ensure!(start + e.data <= MAX_ASSET, "delta exceeds release limit");
            }
        }
        entries.push(e);
        Ok(())
    })?;
    let index = serde_json::to_vec(&Index {
        from: from.into(),
        version: to.into(),
        entries: (!entries.is_empty()).then_some(entries),
    })?;
    ensure!(index.len() as u64 <= MAX_INDEX, "index too large");
    let index = patch::compress(&index)?;
    let mut header = MAGIC.to_vec();
    patch::put(&mut header, index.len() as u64)?;
    ensure!(
        blobs.stream_position()? + (header.len() + index.len()) as u64 <= MAX_ASSET,
        "delta too large"
    );
    out.write_all(&header)?;
    out.write_all(&index)?;
    blobs.rewind()?;
    io::copy(&mut blobs, &mut out)?;
    Ok(total)
}
/// Reconstruct into a fresh directory. A failed apply removes only its own output.
pub fn apply_delta(
    mut input: impl Read + Seek,
    from: &str,
    to: &str,
    old: &Path,
    new: &Path,
) -> Result<()> {
    let len = input.seek(SeekFrom::End(0))?;
    ensure!(len <= MAX_ASSET, "delta too large");
    input.rewind()?;
    let mut magic = [0; MAGIC.len()];
    input.read_exact(&mut magic)?;
    ensure!(magic == MAGIC, "invalid delta magic");
    let compressed = patch::uint(&mut input)?;
    let start = input.stream_position()?;
    ensure!(compressed <= len - start, "invalid index length");
    let mut raw = Vec::new();
    DeflateDecoder::new((&mut input).take(compressed))
        .take(MAX_INDEX + 1)
        .read_to_end(&mut raw)?;
    ensure!(raw.len() as u64 <= MAX_INDEX, "invalid index size");
    let index: Index = serde_json::from_slice(&raw)?;
    ensure!(
        normalize_version(&index.from) == normalize_version(from)
            && normalize_version(&index.version) == normalize_version(to),
        "delta version mismatch"
    );
    let entries = index.entries.unwrap_or_default();
    let mut total = 0;
    let mut end = start + compressed;
    let mut seen = HashSet::new();
    for e in &entries {
        tree::relative(&e.path)?;
        ensure!(
            seen.insert(&e.path) && e.size <= MAX_FILE && e.data <= len - end,
            "invalid entry {}",
            e.path
        );
        if e.dir || !e.link.is_empty() {
            ensure!(
                !(e.dir && !e.link.is_empty()) && e.from.is_empty() && e.size == 0 && e.data == 0,
                "invalid directory/link"
            );
        } else {
            if !e.from.is_empty() {
                tree::relative(&e.from)?;
            }
            ensure!(!e.from.is_empty() || e.data > 0, "missing file data");
            total += e.size;
            ensure!(total <= MAX_TREE, "delta too large");
        }
        end += e.data;
    }
    ensure!(end == len, "trailing delta data");
    let old = old.canonicalize()?;
    ensure!(old.is_dir(), "missing base directory");
    let mut builder = fs::DirBuilder::new();
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.create(new)?;
    let result = (|| {
        let mut offset = start + compressed;
        let mut links = Vec::new();
        for e in entries {
            let rel = Path::new(&e.path);
            let dest = tree::destination(new, rel)?;
            if e.dir {
                fs::create_dir_all(dest)?;
                continue;
            }
            if !e.link.is_empty() {
                links.push((rel.to_owned(), e.link));
                continue;
            }
            input.seek(SeekFrom::Start(offset))?;
            let mut out = File::create_new(&dest)?;
            if e.from.is_empty() {
                tree::copy_exact(
                    DeflateDecoder::new((&mut input).take(e.data)),
                    &mut out,
                    e.size,
                )?;
            } else {
                let src = old
                    .join(&e.from)
                    .canonicalize()
                    .context("missing base file")?;
                ensure!(src.starts_with(&old) && src.is_file(), "invalid base file");
                let mut base = File::open(src)?;
                if e.data == 0 {
                    tree::copy_exact(base, &mut out, e.size)?;
                } else {
                    patch::decode(&mut input, e.data, &mut base, e.size, &mut out)?;
                }
            }
            drop(out);
            ensure!(
                file_sum(&mut File::open(&dest)?)? == e.sha256,
                "delta reconstruction failed for {}",
                e.path
            );
            tree::set_mode(&dest, e.mode)?;
            offset += e.data;
        }
        tree::links(new, links)?;
        tree::set_mode(new, 0o755)
    })();
    if result.is_err() {
        fs::remove_dir_all(new)?;
    }
    result
}
