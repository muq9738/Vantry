//! Preparation and recovery use the same owner/spec records as Go 0.1.42.
use crate::{
    git::{self, Repository, path_str},
    hostfs,
};
use anyhow::{Context, Result, bail, ensure};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    path::{Path, PathBuf},
};
pub use vantry_protocol::WorkspaceSpec;
#[derive(Serialize, Deserialize, PartialEq)]
struct Intent {
    owner: String,
    spec: WorkspaceSpec,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Prepared {
    owner: String,
    spec: WorkspaceSpec,
    path: String,
    created_at: String,
}
fn owner_dir(data: &Path, owner: &str) -> Result<PathBuf> {
    hostfs::component(owner)?;
    ensure!(
        owner
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-_.".contains(&c)),
        "invalid workspace owner"
    );
    ensure!(!data.as_os_str().is_empty(), "missing data directory");
    Ok(std::path::absolute(data)?.join("workspaces").join(owner))
}
fn normalize(dir: &Path, mut spec: WorkspaceSpec, owner: &str) -> Result<WorkspaceSpec> {
    ensure!(
        spec.environment_id.is_empty(),
        "workspace belongs to a remote environment"
    );
    if spec.kind.is_empty() {
        spec.kind = if spec.repository.is_empty() {
            "directory"
        } else {
            "worktree"
        }
        .into();
    }
    match spec.kind.as_str() {
        "directory" => {
            ensure!(
                !spec.path.is_empty()
                    && spec.repository.is_empty()
                    && spec.branch.is_empty()
                    && spec.base_ref.is_empty(),
                "invalid directory spec"
            );
        }
        "worktree" => {
            ensure!(!spec.repository.is_empty(), "missing repository");
            spec.repository = path_str(&std::path::absolute(&spec.repository)?)?.into();
            if spec.path.is_empty() {
                spec.path = path_str(&dir.join("worktree"))?.into();
            }
            if spec.branch.is_empty() {
                spec.branch = format!("vantry/{owner}");
            }
            if spec.base_ref.is_empty() {
                spec.base_ref = "HEAD".into();
            }
        }
        _ => bail!("unknown workspace kind"),
    };
    spec.path = path_str(&std::path::absolute(&spec.path)?)?.into();
    ensure!(
        spec.kind != "worktree" || !same_path(Path::new(&spec.repository), Path::new(&spec.path)),
        "source checkout is protected"
    );
    Ok(spec)
}
fn same_path(a: &Path, b: &Path) -> bool {
    a == b
        || a.canonicalize()
            .is_ok_and(|p| b.canonicalize().ok() == Some(p))
}
/// Conversation directories are only created when execution requests preparation.
pub fn chat_directory(data: &Path, owner: &str, create: bool) -> Result<PathBuf> {
    owner_dir(data, owner)?;
    let dir = std::path::absolute(data)?.join("conversations").join(owner);
    if create {
        hostfs::ensure_private_dir(data)?;
        hostfs::ensure_private_dir(&dir)?;
    } else {
        hostfs::check_private_dir(&dir)?;
    }
    Ok(dir)
}
fn source(spec: &WorkspaceSpec) -> Result<Repository> {
    let p = Path::new(&spec.repository);
    let repo = Repository::discover(p)?;
    ensure!(
        p.canonicalize()? == repo.root(),
        "source must be checkout root"
    );
    Ok(repo)
}
fn verify(spec: &WorkspaceSpec) -> Result<()> {
    let path = Path::new(&spec.path);
    if spec.kind == "directory" {
        ensure!(path.is_dir(), "directory unavailable");
        return Ok(());
    }
    hostfs::directory_handle(path)?;
    let actual = Repository::discover(path)?;
    let repo = source(spec)?;
    let path = path.canonicalize()?;
    ensure!(
        actual.root() == path
            && repo.root() != path
            && actual.common_dir()? == repo.common_dir()?,
        "worktree repository mismatch"
    );
    ensure!(
        PathBuf::from(git::line(&path, &["rev-parse", "--absolute-git-dir"])?).canonicalize()?
            != actual.common_dir()?,
        "primary checkout is protected"
    );
    ensure!(
        actual.branch()? == spec.branch
            && repo
                .worktrees()?
                .iter()
                .any(|w| w.path.canonicalize().ok().as_ref() == Some(&path)
                    && w.branch.as_ref() == Some(&spec.branch)),
        "worktree registration or branch mismatch"
    );
    Ok(())
}
fn check_record(record: &Prepared, owner: &str, spec: &WorkspaceSpec) -> Result<()> {
    ensure!(
        record.owner == owner && record.spec == *spec && record.path == spec.path,
        "workspace ownership conflict"
    );
    Ok(())
}
pub fn existing(data: &Path, owner: &str, spec: WorkspaceSpec) -> Result<PathBuf> {
    let dir = owner_dir(data, owner)?;
    let spec = normalize(&dir, spec, owner)?;
    if let Some(record) = hostfs::read_json(&dir.join("prepared.json"))? {
        check_record(&record, owner, &spec)?;
    } else {
        ensure!(spec.kind == "directory", "worktree not yet prepared");
    }
    verify(&spec)?;
    Ok(spec.path.into())
}
pub fn prepare(data: &Path, owner: &str, spec: WorkspaceSpec) -> Result<WorkspaceSpec> {
    let dir = owner_dir(data, owner)?;
    hostfs::ensure_private_dir(data)?;
    hostfs::ensure_private_dir(&data.join("workspaces"))?;
    hostfs::ensure_private_dir(&dir)?;
    let _lock = hostfs::lock_file(&dir.join("prepare.lock"), true)?;
    let spec = normalize(&dir, spec, owner)?;
    let intent = Intent {
        owner: owner.into(),
        spec: spec.clone(),
    };
    let path = dir.join("intent.json");
    if let Some(saved) = hostfs::read_json::<Intent>(&path)? {
        ensure!(intent == saved, "workspace intent conflict");
    } else {
        hostfs::write_json(&path, &intent)?;
    }
    if hostfs::read_json::<Prepared>(&dir.join("prepared.json"))?.is_some() {
        existing(data, owner, spec.clone())?;
        return Ok(spec);
    }
    if spec.kind == "worktree" {
        let repo = source(&spec)?;
        repo.validate_branch(&spec.branch)?;
        let path = Path::new(&spec.path);
        match fs::symlink_metadata(path) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                let parent = path.parent().context("missing parent")?;
                if !parent.try_exists()? {
                    hostfs::ensure_private_dir(parent)?;
                }
                hostfs::directory_handle(parent)?;
                if git::has_ref(repo.root(), &format!("refs/heads/{}", spec.branch))? {
                    git::run(
                        repo.root(),
                        &["worktree", "add", "--", &spec.path, &spec.branch],
                    )?;
                } else {
                    let base = git::line(
                        repo.root(),
                        &[
                            "rev-parse",
                            "--verify",
                            "--end-of-options",
                            &format!("{}^{{commit}}", spec.base_ref),
                        ],
                    )?;
                    git::run(
                        repo.root(),
                        &[
                            "worktree",
                            "add",
                            "-b",
                            &spec.branch,
                            "--",
                            &spec.path,
                            &base,
                        ],
                    )?;
                }
            }
            Err(e) => return Err(e.into()),
            Ok(_) => (),
        }
    }
    verify(&spec)?;
    let record = Prepared {
        owner: owner.into(),
        spec: spec.clone(),
        path: spec.path.clone(),
        created_at: chrono::Utc::now().to_rfc3339(),
    };
    hostfs::write_json(&dir.join("prepared.json"), &record)?;
    Ok(spec)
}
fn managed_record(dir: &Path, owner: &str) -> Result<Prepared> {
    let intent: Option<Intent> = hostfs::read_json(&dir.join("intent.json"))?;
    let record: Option<Prepared> = hostfs::read_json(&dir.join("prepared.json"))?;
    let mut record = match (record, intent) {
        (Some(r), Some(i)) => {
            ensure!(i.owner == owner, "intent owner mismatch");
            check_record(&r, owner, &i.spec)?;
            r
        }
        (Some(r), None) => r,
        (None, Some(i)) => Prepared {
            owner: i.owner,
            path: i.spec.path.clone(),
            spec: i.spec,
            created_at: String::new(),
        },
        (None, None) => bail!("missing ownership records"),
    };
    let path = Path::new(&record.path);
    ensure!(
        record.owner == owner
            && record.path == record.spec.path
            && record.spec.kind == "worktree"
            && record.spec.environment_id.is_empty()
            && !record.spec.branch.is_empty()
            && !record.spec.base_ref.is_empty()
            && !record.spec.repository.is_empty()
            && path.file_name().is_some_and(|n| n == "worktree")
            && path.parent().is_some_and(|p| same_path(p, dir)),
        "cleanup limited to owned managed worktrees"
    );
    record.path = path_str(&dir.join("worktree"))?.into();
    record.spec.path = record.path.clone();
    Ok(record)
}
pub fn cleanup(data: &Path, owner: &str, force_dirty: bool) -> Result<()> {
    let dir = owner_dir(data, owner)?;
    match hostfs::directory_handle(&dir) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        r => drop(r?),
    }
    let _work = hostfs::lock_file(&dir.join("workspace.lock"), false)?;
    let _prepare = hostfs::lock_file(&dir.join("prepare.lock"), false)?;
    let record = managed_record(&dir, owner)?;
    let path = dir.join("worktree");
    ensure!(
        force_dirty || !dir.join("handoff-source.json").try_exists()?,
        "handoff recovery data retained"
    );
    let repo = source(&record.spec)?;
    let entry = repo.worktrees()?.into_iter().find(|w| {
        w.path.file_name() == path.file_name()
            && w.path.parent().is_some_and(|p| same_path(p, &dir))
    });
    let _dir_lock = match fs::symlink_metadata(&path) {
        Ok(_) => {
            let lock = hostfs::lock_dir(&path, false)?;
            verify(&record.spec)?;
            ensure!(
                force_dirty
                    || git::run(
                        &path,
                        &[
                            "status",
                            "--porcelain=v1",
                            "--untracked-files=all",
                            "--ignored"
                        ]
                    )?
                    .is_empty(),
                "dirty worktree preserved"
            );
            ensure!(entry.is_some(), "missing registration");
            Some(lock)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
        Err(e) => return Err(e.into()),
    };
    if let Some(entry) = entry {
        ensure!(
            !entry.locked && entry.branch.as_ref() == Some(&record.spec.branch),
            "registration changed or locked"
        );
        let mut args = vec!["worktree", "remove"];
        if force_dirty {
            args.push("--force");
        }
        args.extend(["--", path_str(&path)?]);
        git::run(repo.root(), &args)?;
    }
    fs::remove_dir_all(dir)?;
    Ok(())
}

#[derive(Debug)]
pub struct FileEntry {
    pub name: std::ffi::OsString,
    pub directory: bool,
    pub symlink: bool,
}
/// List a single tree level, without following links. Callers expand directories on demand.
pub fn list(root: &Path, relative: &Path, limit: usize) -> Result<Vec<FileEntry>> {
    protect_metadata(relative)?;
    let path = if relative == Path::new("") || relative == Path::new(".") {
        root.to_owned()
    } else {
        root.join(hostfs::relative(relative)?)
    };
    let directory = hostfs::directory_handle(&path)?;
    let mut entries = Vec::new();
    #[cfg(unix)]
    {
        use std::ffi::{CStr, OsStr};
        use std::os::unix::{ffi::OsStrExt, io::IntoRawFd};
        struct Stream(*mut libc::DIR);
        impl Drop for Stream {
            fn drop(&mut self) {
                unsafe { libc::closedir(self.0) };
            }
        }
        let fd = directory.into_raw_fd();
        let ptr = unsafe { libc::fdopendir(fd) };
        if ptr.is_null() {
            let e = std::io::Error::last_os_error();
            unsafe { libc::close(fd) };
            return Err(e.into());
        }
        let stream = Stream(ptr);
        loop {
            let item = unsafe { libc::readdir(stream.0) };
            if item.is_null() {
                break;
            }
            let item = unsafe { &*item };
            let name =
                OsStr::from_bytes(unsafe { CStr::from_ptr(item.d_name.as_ptr()) }.to_bytes());
            if name == "." || name == ".." || name == ".git" {
                continue;
            }
            ensure!(entries.len() < limit, "directory exceeds entry limit");
            entries.push(FileEntry {
                name: name.into(),
                directory: item.d_type == libc::DT_DIR,
                symlink: item.d_type == libc::DT_LNK,
            });
        }
    }
    #[cfg(windows)]
    {
        let _directory = directory;
        for item in fs::read_dir(path)? {
            let item = item?;
            if item.file_name() == ".git" {
                continue;
            }
            ensure!(entries.len() < limit, "directory exceeds entry limit");
            let kind = item.file_type()?;
            entries.push(FileEntry {
                name: item.file_name(),
                directory: kind.is_dir(),
                symlink: kind.is_symlink(),
            });
        }
    }
    entries.sort_by(|a, b| {
        b.directory
            .cmp(&a.directory)
            .then_with(|| a.name.cmp(&b.name))
    });
    Ok(entries)
}
fn protect_metadata(relative: &Path) -> Result<()> {
    ensure!(
        !relative
            .components()
            .any(|p| p.as_os_str().eq_ignore_ascii_case(".git")),
        "repository metadata is protected"
    );
    Ok(())
}
pub fn read(root: &Path, relative: &Path, limit: usize) -> Result<Vec<u8>> {
    protect_metadata(relative)?;
    Ok(hostfs::read_file(root, relative, limit)?)
}
