use crate::{apply_delta, archive};
use anyhow::{Context, Result, ensure};
use std::{
    fs::{self, File},
    path::{Path, PathBuf},
    process::Command,
};
use tempfile::{Builder, TempDir};

/// Advisory lock shared by installers targeting the same directory. The OS releases
/// the lock on process exit. Keep the inode in place to serialize concurrent opens.
pub struct UpdateLock {
    _file: File,
}
impl UpdateLock {
    pub fn acquire(target: &Path) -> Result<Self> {
        let mut name = target.file_name().context("invalid target")?.to_owned();
        name.push(".update-lock");
        let file = File::options()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(target.with_file_name(name))?;
        file.try_lock()
            .context("another installer holds the update lock")?;
        Ok(Self { _file: file })
    }
}
fn is_app(path: &Path) -> bool {
    path.extension()
        .is_some_and(|e| e.eq_ignore_ascii_case("app"))
}
pub(crate) fn verify(staged: &Path, target: &Path) -> Result<()> {
    ensure!(
        fs::symlink_metadata(staged)?.is_dir(),
        "staged payload must be a directory"
    );
    crate::tree::walk(staged, |rel, meta| {
        if meta.is_symlink() {
            crate::tree::validate_link(
                staged,
                rel,
                &crate::tree::wire_path(&fs::read_link(staged.join(rel))?)?,
            )?;
        }
        Ok(())
    })?;
    if is_app(target) || is_app(staged) {
        ensure!(is_app(staged), "expected an app bundle");
        ensure!(cfg!(target_os = "macos"), "macOS required to verify app");
        ensure!(
            Command::new("/usr/bin/codesign")
                .args(["--verify", "--deep", "--strict"])
                .arg(staged)
                .status()?
                .success(),
            "staged app signature invalid"
        );
    }
    Ok(())
}
/// Owns a verified staging directory until installation, cancellation (drop), or
/// explicit handoff to the offline worker. Preparing never modifies the target.
pub struct InstallPlan {
    root: TempDir,
    staged: PathBuf,
    target: PathBuf,
    pub version: String,
    pub used_delta: bool,
}
impl InstallPlan {
    pub fn prepare(package: &Path, target: &Path, from: &str, to: &str) -> Result<Self> {
        let asset = package
            .file_name()
            .and_then(|n| n.to_str())
            .context("missing package filename")?;
        Self::from_asset(package, asset, Some(target), is_app(target), from, to)
    }
    pub(crate) fn from_asset(
        package: &Path,
        asset: &str,
        target: Option<&Path>,
        app: bool,
        from: &str,
        to: &str,
    ) -> Result<Self> {
        let mut builder = Builder::new();
        builder.prefix(".vantry-update-");
        let (root, target) = if let Some(target) = target {
            let parent = target
                .parent()
                .filter(|p| !p.as_os_str().is_empty())
                .unwrap_or(Path::new("."))
                .canonicalize()?;
            let root = builder.tempdir_in(&parent).or_else(|_| builder.tempdir())?;
            (
                root,
                parent.join(target.file_name().context("invalid install target")?),
            )
        } else {
            (builder.tempdir()?, PathBuf::new())
        };
        let payload = root.path().join("payload");
        fs::create_dir(&payload)?;
        let used_delta = asset.to_ascii_lowercase().ends_with(".delta");
        let staged = if used_delta {
            let dest = payload.join(
                target
                    .file_name()
                    .context("delta requires a base installation")?,
            );
            apply_delta(File::open(package)?, from, to, &target, &dest)?;
            dest
        } else {
            if asset.to_ascii_lowercase().ends_with(".dmg") {
                extract_dmg(package, &payload, root.path())?;
            } else {
                archive::extract(package, asset, &payload)?;
            }
            archive::payload(&payload, app)?
        };
        verify(&staged, &target)?;
        // A full archive can wrap the bundle or include readmes alongside it.
        // Only retain the verified bundle for the detached/manual installer.
        let staged = if app {
            let ready = root
                .path()
                .join(staged.file_name().context("missing app bundle name")?);
            fs::rename(&staged, &ready)?;
            fs::remove_dir_all(&payload)?;
            ready
        } else {
            staged
        };
        Ok(Self {
            root,
            staged,
            target,
            version: to.to_owned(),
            used_delta,
        })
    }
    pub fn staged(&self) -> &Path {
        &self.staged
    }
    pub fn target(&self) -> &Path {
        &self.target
    }
    pub fn install(&self) -> Result<()> {
        crate::apply_staged(&self.staged, &self.target, &Default::default())
    }
    pub fn persist(self) -> PathBuf {
        let _ = self.root.keep();
        self.staged
    }
    pub(crate) fn retain_for_worker(&mut self) {
        self.root.disable_cleanup(true);
    }
    pub fn installer(&self, data_dir: &Path) -> Result<PathBuf> {
        crate::offline::installer(self.root.path(), &self.staged, &self.target, data_dir)
    }
}
fn extract_dmg(package: &Path, dest: &Path, root: &Path) -> Result<()> {
    ensure!(cfg!(target_os = "macos"), "DMG requires macOS");
    ensure!(
        package.metadata()?.len() <= crate::MAX_ASSET,
        "disk image too large"
    );
    // Keep mount outside the staging tree so cleanup can never traverse a live mount.
    let mount = Builder::new()
        .prefix(".vantry-mount-")
        .tempdir_in(root.parent().unwrap())?
        .keep();
    let attach = Command::new("/usr/bin/hdiutil")
        .args(["attach", "-nobrowse", "-readonly", "-mountpoint"])
        .arg(&mount)
        .arg(package)
        .status();
    if !attach.is_ok_and(|s| s.success()) {
        fs::remove_dir(&mount)?;
        anyhow::bail!("could not mount disk image");
    }
    let result = (|| {
        let app = archive::payload(&mount, true)?;
        let dst = dest.join(app.file_name().unwrap());
        fs::create_dir(&dst)?;
        let mut links = vec![];
        let mut total = 0;
        crate::tree::walk(&app, |rel, m| {
            let src = app.join(rel);
            let out = dst.join(rel);
            if m.is_dir() {
                fs::create_dir(out)?;
            } else if m.is_symlink() {
                links.push((
                    rel.to_owned(),
                    crate::tree::wire_path(&fs::read_link(src)?)?,
                ));
            } else {
                ensure!(
                    m.is_file() && m.len() <= crate::MAX_FILE,
                    "unsupported DMG entry"
                );
                total += m.len();
                ensure!(total <= crate::MAX_TREE, "disk image too large");
                fs::copy(src, out)?;
            }
            Ok(())
        })?;
        crate::tree::links(&dst, links)
    })();
    ensure!(
        Command::new("/usr/bin/hdiutil")
            .arg("detach")
            .arg(&mount)
            .status()?
            .success(),
        "could not detach {}",
        mount.display()
    );
    fs::remove_dir(mount)?;
    result
}
#[test]
fn rename_failure_restores_previous_installation() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let target = tmp.path().join("target");
    fs::create_dir(&target)?;
    fs::write(target.join("version"), "previous")?;
    assert!(swap(&tmp.path().join("missing"), &target).is_err());
    assert_eq!(fs::read_to_string(target.join("version"))?, "previous");
    assert_eq!(fs::read_dir(tmp.path())?.count(), 1);
    Ok(())
}
pub(crate) fn swap(staged: &Path, target: &Path) -> Result<()> {
    let backup = Builder::new()
        .prefix(".vantry-backup-")
        .tempdir_in(target.parent().context("missing target parent")?)?;
    let saved = backup.path().join("previous");
    let exists = match fs::symlink_metadata(target) {
        Ok(meta) => {
            ensure!(meta.is_dir(), "target is not a directory");
            true
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => false,
        Err(e) => return Err(e.into()),
    };
    if exists {
        fs::rename(target, &saved)?;
    }
    if let Err(e) = fs::rename(staged, target) {
        if exists && let Err(restore) = fs::rename(&saved, target) {
            let kept = backup.keep();
            anyhow::bail!(
                "swap failed ({e}), restore failed ({restore}), previous version at {}",
                kept.display()
            );
        }
        return Err(e).context("could not install staged version");
    }
    Ok(())
}
