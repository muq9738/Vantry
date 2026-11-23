use crate::{JobRequest, disk};
use anyhow::{Context, Result, ensure};
use serde::Deserialize;
use std::{
    fs,
    path::{Path, PathBuf},
};
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
}
