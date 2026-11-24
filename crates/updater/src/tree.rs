use anyhow::{Context, Result, bail, ensure};
use std::{
    collections::VecDeque,
    fs, io,
    path::{Component, Path, PathBuf},
};

pub(crate) fn walk(
    root: &Path,
    mut visit: impl FnMut(&Path, &fs::Metadata) -> Result<()>,
) -> Result<()> {
    fn descend(
        root: &Path,
        rel: &Path,
        visit: &mut impl FnMut(&Path, &fs::Metadata) -> Result<()>,
    ) -> Result<()> {
        let mut children = fs::read_dir(root.join(rel))?.collect::<io::Result<Vec<_>>>()?;
        children.sort_by_key(|e| e.file_name());
        for child in children {
            let path = rel.join(child.file_name());
            let meta = fs::symlink_metadata(child.path())?;
            visit(&path, &meta)?;
            if meta.is_dir() {
                descend(root, &path, visit)?;
            }
        }
        Ok(())
    }
    ensure!(fs::metadata(root)?.is_dir(), "expected directory");
    descend(root, Path::new(""), &mut visit)
}
pub(crate) fn wire_path(path: &Path) -> Result<String> {
    Ok(path
        .to_str()
        .context("non-UTF-8 path")?
        .replace(std::path::MAIN_SEPARATOR, "/"))
}
fn portable(name: &str) -> Result<()> {
    ensure!(
        !name.is_empty() && !name.contains(['\\', ':', '\0']) && !name.starts_with('/'),
        "unsafe path {name:?}"
    );
    #[cfg(windows)]
    for part in name.split('/').filter(|p| !matches!(*p, "." | "..")) {
        let base = part
            .split('.')
            .next()
            .unwrap()
            .trim_end()
            .to_ascii_uppercase();
        ensure!(
            !part.ends_with(['.', ' '])
                && !matches!(
                    base.as_str(),
                    "CON" | "AUX" | "PRN" | "NUL" | "CONIN$" | "CONOUT$"
                )
                && !base
                    .strip_prefix("COM")
                    .or_else(|| base.strip_prefix("LPT"))
                    .is_some_and(|s| matches!(
                        s,
                        "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9" | "¹" | "²" | "³"
                    )),
            "unsafe Windows path"
        );
    }
    Ok(())
}
pub(crate) fn relative(name: &str) -> Result<&Path> {
    portable(name)?;
    ensure!(
        name.split('/').all(|s| !matches!(s, "" | "." | "..")),
        "unsafe path {name:?}"
    );
    Ok(Path::new(name))
}
/// Extraction roots are newly created and private. Never follow links while creating children.
pub(crate) fn destination(root: &Path, rel: &Path) -> Result<PathBuf> {
    let mut parent = root.to_owned();
    for part in rel.parent().context("missing parent")?.components() {
        ensure!(matches!(part, Component::Normal(_)), "invalid path");
        parent.push(part);
        match fs::create_dir(&parent) {
            Ok(()) => (),
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => ensure!(
                fs::symlink_metadata(&parent)?.is_dir(),
                "non-directory ancestor"
            ),
            Err(e) => return Err(e.into()),
        }
    }
    Ok(root.join(rel))
}
/// Resolve each component in filesystem order, including links followed by `..`.
/// Dangling in-tree links are permitted by the release format.
pub(crate) fn validate_link(root: &Path, rel: &Path, link: &str) -> Result<()> {
    portable(link)?;
    let mut path = rel.parent().context("link parent")?.to_owned();
    let mut pending: VecDeque<String> = link.split('/').map(str::to_owned).collect();
    let mut hops = 0;
    while let Some(part) = pending.pop_front() {
        match part.as_str() {
            "" | "." => continue,
            ".." => {
                ensure!(path.pop(), "symlink escapes root");
                continue;
            }
            _ => path.push(part),
        }
        match fs::symlink_metadata(root.join(&path)) {
            Ok(m) if m.is_symlink() => {
                hops += 1;
                ensure!(hops <= 40, "symlink cycle");
                let target = wire_path(&fs::read_link(root.join(&path))?)?;
                portable(&target)?;
                path.pop();
                for item in target.split('/').rev() {
                    pending.push_front(item.to_owned());
                }
            }
            Ok(_) => (),
            Err(e) if e.kind() == io::ErrorKind::NotFound => (),
            Err(e) => return Err(e.into()),
        }
    }
    Ok(())
}
pub(crate) fn links(root: &Path, entries: Vec<(PathBuf, String)>) -> Result<()> {
    for (rel, target) in &entries {
        portable(target)?;
        let path = destination(root, rel)?;
        #[cfg(unix)]
        std::os::unix::fs::symlink(target, path)?;
        #[cfg(windows)]
        {
            let absolute = path.parent().unwrap().join(target);
            if absolute.is_dir() {
                std::os::windows::fs::symlink_dir(target, path)?
            } else {
                std::os::windows::fs::symlink_file(target, path)?
            }
        }
    }
    for (rel, target) in entries {
        validate_link(root, &rel, &target)?;
    }
    Ok(())
}
pub(crate) fn mode(meta: &fs::Metadata) -> u32 {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        meta.permissions().mode() & 0o777
    }
    #[cfg(not(unix))]
    {
        let _ = meta;
        0o644
    }
}
pub(crate) fn set_mode(path: &Path, bits: u32) -> Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(
            path,
            fs::Permissions::from_mode(if bits & 0o777 == 0 {
                0o644
            } else {
                bits & 0o777
            }),
        )?;
    }
    #[cfg(not(unix))]
    let _ = (path, bits);
    Ok(())
}
pub(crate) fn copy_exact(
    mut reader: impl io::Read,
    writer: &mut impl io::Write,
    size: u64,
) -> Result<()> {
    use io::Read;
    let n = io::copy(&mut reader.by_ref().take(size), writer)?;
    if n != size || reader.read(&mut [0])? != 0 {
        bail!("entry size mismatch");
    }
    Ok(())
}
