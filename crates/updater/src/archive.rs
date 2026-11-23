use crate::{MAX_FILE, MAX_TREE, tree};
use anyhow::{Context, Result, bail, ensure};
use flate2::read::GzDecoder;
use std::{
    fs::{self, File},
    io::{self, Read},
    path::{Path, PathBuf},
};

struct Extraction<'a> {
    root: &'a Path,
    total: u64,
    links: Vec<(PathBuf, String)>,
}
impl Extraction<'_> {
    fn file(&mut self, path: &Path, size: u64, mode: u32, source: impl Read) -> Result<()> {
        ensure!(
            size <= MAX_FILE && size <= MAX_TREE - self.total,
            "archive exceeds extraction limit"
        );
        self.total += size;
        let mut dest = File::create_new(path)?;
        tree::copy_exact(source, &mut dest, size)?;
        tree::set_mode(path, mode)
    }
}
fn name(raw: &str) -> Result<Option<&Path>> {
    let raw = raw.trim_end_matches('/');
    let raw = raw.trim_start_matches("./");
    if raw == "." {
        return Ok(None);
    }
    Ok(Some(tree::relative(raw)?))
}
pub(crate) fn extract(path: &Path, asset: &str, root: &Path) -> Result<()> {
    let source = File::open(path)?;
    ensure!(
        source.metadata()?.len() <= crate::MAX_ASSET,
        "archive too large"
    );
    let mut unpack = Extraction {
        root,
        total: 0,
        links: vec![],
    };
    let lower = asset.to_ascii_lowercase();
    if lower.ends_with(".zip") {
        let mut zip = zip::ZipArchive::new(source)?;
        for i in 0..zip.len() {
            let mut e = zip.by_index(i)?;
            let Some(rel) = name(e.name())?.map(Path::to_owned) else {
                ensure!(e.is_dir(), "invalid root entry");
                continue;
            };
            let dest = tree::destination(unpack.root, &rel)?;
            if e.is_symlink() {
                ensure!(e.size() <= 4096, "link too long");
                let mut target = String::new();
                (&mut e).take(4097).read_to_string(&mut target)?;
                ensure!(target.len() <= 4096, "link too long");
                unpack.links.push((rel, target));
            } else if e.is_dir() {
                fs::create_dir_all(dest)?;
            } else {
                unpack.file(&dest, e.size(), e.unix_mode().unwrap_or(0o644), e)?;
            }
        }
    } else if lower.ends_with(".tar.gz") || lower.ends_with(".tgz") {
        let mut tar = tar::Archive::new(GzDecoder::new(source));
        for e in tar.entries()? {
            let e = e?;
            let path = e.path()?;
            let Some(rel) = name(path.to_str().context("non-UTF-8 archive entry")?)? else {
                ensure!(e.header().entry_type().is_dir(), "invalid root entry");
                continue;
            };
            let dest = tree::destination(root, rel)?;
            let kind = e.header().entry_type();
            if kind.is_dir() {
                fs::create_dir_all(dest)?;
            } else if kind.is_symlink() || kind.is_hard_link() {
                let link = e.link_name()?.context("missing link")?;
                let target = link.to_str().context("non-UTF-8 link")?;
                if kind.is_symlink() {
                    unpack.links.push((rel.to_owned(), target.to_owned()));
                } else {
                    let src = root.join(name(target)?.context("invalid hard link")?);
                    ensure!(fs::symlink_metadata(&src)?.is_file(), "invalid hard link");
                    fs::hard_link(src, dest)?;
                }
            } else if kind.is_file() {
                unpack.file(&dest, e.size(), e.header().mode()?, e)?;
            } else {
                bail!("unsupported tar entry type");
            }
        }
        // Consume the gzip trailer (CRC and uncompressed length).
        io::copy(&mut tar.into_inner().take(MAX_TREE + 1), &mut io::sink()).and_then(|n| {
            if n > MAX_TREE {
                Err(io::Error::other("excess tar padding"))
            } else {
                Ok(n)
            }
        })?;
    } else {
        bail!("unsupported archive {asset}");
    }
    tree::links(root, unpack.links)
}
pub(crate) fn payload(root: &Path, app: bool) -> Result<PathBuf> {
    if app {
        let mut bundles = Vec::new();
        tree::walk(root, |p, m| {
            if m.is_dir()
                && p.extension()
                    .is_some_and(|ext| ext.eq_ignore_ascii_case("app"))
            {
                bundles.push(p.to_owned());
            }
            Ok(())
        })?;
        bundles.sort_by_key(|p| p.components().count());
        return Ok(root.join(bundles.first().context("archive lacks .app")?));
    }
    let entries = fs::read_dir(root)?.collect::<io::Result<Vec<_>>>()?;
    ensure!(!entries.is_empty(), "empty archive");
    if entries.len() == 1 && entries[0].file_type()?.is_dir() {
        Ok(entries[0].path())
    } else {
        Ok(root.to_owned())
    }
}
