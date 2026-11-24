use crate::{JobRequest, disk};
use anyhow::{Context, Result, bail, ensure};
use serde::Deserialize;
use std::{
    fs::{self, File, TryLockError},
    path::{Path, PathBuf},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
use vantry_protocol::WorkspaceSpec;

fn git_dir(root: &Path) -> Result<PathBuf> {
    let dot = root.join(".git");
    if dot.is_dir() {
        return Ok(dot.canonicalize()?);
    }
    let pointer = fs::read_to_string(dot)?;
    Ok(root
        .join(
            pointer
                .trim()
                .strip_prefix("gitdir: ")
                .context("invalid worktree gitdir")?,
        )
        .canonicalize()?)
}
fn common_dir(git: &Path) -> Result<PathBuf> {
    let path = git.join("commondir");
    if path.try_exists()? {
        Ok(git.join(fs::read_to_string(path)?.trim()).canonicalize()?)
    } else {
        Ok(git.to_owned())
    }
}
/// Core prepares managed worktrees. Recheck its ownership record and Git's active branch before opening Pi.
pub(crate) fn cwd(job: &JobRequest) -> Result<PathBuf> {
    let requested = job
        .request
        .workspace
        .as_ref()
        .context("missing workspace")?;
    ensure!(
        requested.environment_id.is_empty(),
        "workspace belongs to a different execution environment"
    );
    if requested.kind == "directory" {
        let path = Path::new(&requested.path);
        ensure!(
            path.is_absolute() && path.is_dir(),
            "expected prepared local directory"
        );
        return Ok(path.canonicalize()?);
    }
    ensure!(requested.kind == "worktree", "unsupported workspace kind");
    #[derive(Deserialize)]
    struct Prepared {
        owner: String,
        spec: WorkspaceSpec,
        path: PathBuf,
    }
    let record: Prepared = disk::read(
        &job.data_dir
            .join("workspaces")
            .join(&job.request.thread_id)
            .join("prepared.json"),
    )
    .context("read prepared worktree")?;
    let spec = record.spec;
    ensure!(
        record.owner == job.request.thread_id
            && spec.kind == "worktree"
            && spec.environment_id.is_empty()
            && record.path.is_absolute()
            && spec.path == record.path.to_str().unwrap_or("")
            && !spec.branch.is_empty(),
        "invalid prepared worktree ownership"
    );
    let cwd = record.path.canonicalize()?;
    let repository = Path::new(&spec.repository).canonicalize()?;
    ensure!(
        cwd != repository
            && (requested.path.is_empty() || Path::new(&requested.path).canonicalize()? == cwd)
            && (requested.repository.is_empty()
                || Path::new(&requested.repository).canonicalize()? == repository)
            && (requested.branch.is_empty() || requested.branch == spec.branch),
        "requested and prepared worktree differ"
    );
    let git = git_dir(&cwd)?;
    let common = common_dir(&git)?;
    ensure!(
        git != common
            && common == common_dir(&git_dir(&repository)?)?
            && git
                .join(fs::read_to_string(git.join("gitdir"))?.trim())
                .canonicalize()?
                == cwd.join(".git")
            && fs::read_to_string(git.join("HEAD"))?.trim()
                == format!("ref: refs/heads/{}", spec.branch),
        "worktree checkout or branch changed"
    );
    Ok(cwd)
}

#[cfg(unix)]
fn open_workspace_lock_handle(cwd: &Path, _data_dir: &Path) -> Result<File> {
    File::open(cwd).with_context(|| format!("open workspace for writing lock: {}", cwd.display()))
}

#[cfg(not(unix))]
fn open_workspace_lock_handle(cwd: &Path, data_dir: &Path) -> Result<File> {
    let locks_dir = data_dir.join("workspace-locks");
    disk::directory(&locks_dir)?;
    let key: String = cwd
        .to_string_lossy()
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.') { c } else { '_' })
        .collect();
    crate::worker::lock_file(&locks_dir.join(format!("{key}.lock")))
}

/// Serialize concurrent writers sharing the same checkout directory without creating
/// lock files inside the user's checkout (Unix locks the directory inode directly).
/// Independent worktrees have distinct canonical paths and run in parallel.
pub(crate) async fn lock_workspace_dir(
    cwd: &Path,
    data_dir: &Path,
    stop_file: &Path,
    failed: &CancellationToken,
    mut on_busy: impl FnMut(),
) -> Result<(File, bool)> {
    let file = open_workspace_lock_handle(cwd, data_dir)?;
    let mut waited = false;
    loop {
        if stop_file.try_exists()? {
            bail!("attempt stopped while waiting for workspace lock");
        }
        if failed.is_cancelled() {
            bail!("journal write failed while waiting for workspace lock");
        }
        match file.try_lock() {
            Ok(()) => return Ok((file, waited)),
            Err(TryLockError::WouldBlock) => {
                if !waited {
                    waited = true;
                    on_busy();
                }
                tokio::select! {
                    _ = failed.cancelled() => bail!("journal write failed while waiting for workspace lock"),
                    _ = tokio::time::sleep(Duration::from_millis(50)) => {}
                }
            }
            Err(TryLockError::Error(err)) => {
                return Err(err).with_context(|| format!("lock workspace {}", cwd.display()));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn verifies_linked_worktree_owner_repository_and_branch() -> Result<()> {
        let tmp = tempfile::tempdir()?;
        let data = tmp.path().canonicalize()?;
        let owner = data.join("workspaces/thread");
        let ws = owner.join("worktree");
        let repo = data.join("main");
        let git = repo.join(".git/worktrees/thread");
        fs::create_dir_all(&git)?;
        fs::create_dir_all(&ws)?;
        fs::write(ws.join(".git"), format!("gitdir: {}\n", git.display()))?;
        fs::write(git.join("commondir"), "../..\n")?;
        fs::write(git.join("gitdir"), ws.join(".git").to_str().unwrap())?;
        fs::write(git.join("HEAD"), "ref: refs/heads/feature\n")?;
        let spec = json!({"kind":"worktree","path":ws,"repository":repo,"branch":"feature"});
        let record = owner.join("prepared.json");
        disk::write_json(&record, &json!({"owner":"thread","spec":spec,"path":ws}))?;
        let job: JobRequest = serde_json::from_value(
            json!({"dataDir":data,"request":{"ThreadID":"thread","Workspace":spec}}),
        )?;
        assert_eq!(cwd(&job)?, ws);
        fs::write(git.join("HEAD"), "ref: refs/heads/other\n")?;
        assert!(cwd(&job).is_err());
        fs::write(git.join("HEAD"), "ref: refs/heads/feature\n")?;
        disk::write_json(&record, &json!({"owner":"other","spec":spec,"path":ws}))?;
        assert!(cwd(&job).is_err());
        Ok(())
    }

    #[tokio::test]
    async fn shared_workspace_lock_serializes_and_honors_stop() -> Result<()> {
        let tmp = tempfile::tempdir()?;
        let data = tmp.path().join("data");
        let ws = tmp.path().join("repo");
        fs::create_dir_all(&data)?;
        fs::create_dir_all(&ws)?;
        let stop1 = data.join("stop1.json");
        let stop2 = data.join("stop2.json");
        let token = CancellationToken::new();

        let (lock1, waited1) = lock_workspace_dir(&ws, &data, &stop1, &token, || {}).await?;
        assert!(!waited1);

        // Second waiter observes busy, then aborts cleanly when stop2 appears.
        let busy = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        let busy_flag = busy.clone();
        let ws2 = ws.clone();
        let data2 = data.clone();
        let stop2_bg = stop2.clone();
        let tok2 = token.clone();
        let waiter = tokio::spawn(async move {
            lock_workspace_dir(&ws2, &data2, &stop2_bg, &tok2, || {
                busy_flag.store(true, std::sync::atomic::Ordering::SeqCst);
            })
            .await
        });
        tokio::time::sleep(Duration::from_millis(90)).await;
        assert!(busy.load(std::sync::atomic::Ordering::SeqCst));
        fs::write(&stop2, b"{}")?;
        assert!(waiter.await?.is_err());

        // Once lock1 drops, a third caller acquires immediately.
        drop(lock1);
        let stop3 = data.join("stop3.json");
        let (_lock3, waited3) = lock_workspace_dir(&ws, &data, &stop3, &token, || {}).await?;
        assert!(!waited3);
        Ok(())
    }
}
