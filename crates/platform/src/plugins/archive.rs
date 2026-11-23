use super::Manifest;
use crate::hostfs;
use anyhow::{Context, Result, bail, ensure};
use flate2::read::GzDecoder;
use std::{
    fs::{self, OpenOptions},
    io::{self, Cursor, Read},
    path::{Path, PathBuf},
};
const FILE_LIMIT: u64 = 16 << 20;
#[derive(Default)]
struct Limits {
    entries: usize,
    total: u64,
}
impl Limits {
    fn extract(
        &mut self,
        root: &Path,
        name: &Path,
        directory: bool,
        mode: u32,
        mut reader: impl Read,
    ) -> Result<()> {
        self.entries += 1;
        ensure!(self.entries <= 512, "archive contains too many entries");
        let name = name.to_str().context("non-UTF8 archive name")?;
        ensure!(!name.contains(['\\', ':', '\0']), "invalid archive name");
        // Allow conventional tar ./ prefixes, and reject special device names and streams on all hosts.
        if directory && (name == "." || name == "./") {
            return Ok(());
        }
        let relative = hostfs::relative(Path::new(name))?;
        for part in relative.components().filter_map(|c| {
            if let std::path::Component::Normal(p) = c {
                p.to_str()
            } else {
                None
            }
        }) {
            let stem = part
                .split('.')
                .next()
                .unwrap_or_default()
                .to_ascii_uppercase();
            ensure!(
                !part.ends_with(['.', ' '])
                    && !part.chars().any(char::is_control)
                    && !["CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"].contains(&stem.as_str())
                    && !(stem.len() == 4
                        && (stem.starts_with("COM") || stem.starts_with("LPT"))
                        && stem.as_bytes()[3].is_ascii_digit()),
                "invalid archive component"
            );
        }
        let path = root.join(relative);
        if directory {
            hostfs::ensure_private_dir(&path)?;
            return Ok(());
        }
        hostfs::ensure_private_dir(path.parent().context("invalid archive path")?)?;
        let mut options = OpenOptions::new();
        options.create_new(true).write(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(if mode & 0o111 != 0 { 0o700 } else { 0o600 });
        }
        #[cfg(windows)]
        let _ = mode;
        let mut file = options.open(path)?;
        let bytes = io::copy(&mut reader.by_ref().take(FILE_LIMIT + 1), &mut file)?;
        self.total += bytes;
        ensure!(
            bytes <= FILE_LIMIT && self.total <= 64 << 20,
            "archive exceeds extraction limit"
        );
        file.sync_all()?;
        Ok(())
    }
}
pub(super) fn unpack(data: &[u8], root: &Path, version: &str) -> Result<(Manifest, PathBuf)> {
    ensure!(data.len() <= 32 << 20, "archive exceeds download limit");
    let mut limits = Limits::default();
    if data.starts_with(b"\x1f\x8b") {
        let mut archive = tar::Archive::new(GzDecoder::new(data).take(70 << 20));
        for entry in archive.entries()? {
            let mut entry = entry?;
            let kind = entry.header().entry_type();
            ensure!(
                kind.is_dir() || kind.is_file(),
                "links and special archive entries are prohibited"
            );
            let name = entry.path()?.into_owned();
            let mode = entry.header().mode()?;
            limits.extract(root, &name, kind.is_dir(), mode, &mut entry)?;
        }
    } else {
        let mut archive = zip::ZipArchive::new(Cursor::new(data))?;
        ensure!(archive.len() <= 512, "too many entries");
        for i in 0..archive.len() {
            let mut entry = archive.by_index(i)?;
            let mode = entry.unix_mode().unwrap_or_default();
            ensure!(
                matches!(mode & 0o170000, 0 | 0o040000 | 0o100000),
                "links and special archive entries are prohibited"
            );
            let path = PathBuf::from(entry.name());
            limits.extract(root, &path, entry.is_dir(), mode, &mut entry)?;
        }
    }
    let mut candidates = Vec::new();
    if root.join("plugin.json").is_file() {
        candidates.push(root.to_owned());
    } else {
        for e in fs::read_dir(root)? {
            let e = e?;
            if e.file_type()?.is_dir() && e.path().join("plugin.json").is_file() {
                candidates.push(e.path());
            }
        }
    }
    if candidates.len() != 1 {
        bail!("archive must contain one plugin root");
    }
    let path = candidates.pop().unwrap();
    let m = Manifest::load(&path, version, None)?;
    Ok((m, path))
}
