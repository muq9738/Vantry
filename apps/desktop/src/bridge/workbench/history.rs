//! Read-only history queries and the boundaries of a single-file unified patch.
use super::local::run_git;
use crate::bridge::BridgeError;
use std::path::Path;
use tokio::{io::AsyncWriteExt, process::Command};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Revision {
    Commit(String),
    Compare(String),
    CompareReverse(String),
}

impl Revision {
    fn range(&self) -> String {
        match self {
            Self::Commit(id) => id.clone(),
            Self::Compare(branch) => format!("HEAD..refs/heads/{branch}"),
            Self::CompareReverse(branch) => format!("refs/heads/{branch}..HEAD"),
        }
    }
    fn args(&self, files: bool) -> Vec<String> {
        let mut args: Vec<String> = match self {
            Self::Commit(_) => vec!["show".into(), "--format=".into()],
            Self::Compare(_) | Self::CompareReverse(_) => vec!["diff".into()],
        };
        if files {
            args.extend(["--name-only".into(), "-z".into()]);
        }
        args.extend([
            "--no-color".into(),
            "--no-ext-diff".into(),
            "--no-textconv".into(),
            "--end-of-options".into(),
            self.range(),
            "--".into(),
        ]);
        args
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CommitRow {
    pub id: String,
    pub graph: String,
    pub subject: String,
    pub author: String,
    pub date: String,
    pub refs: String,
}

pub(super) async fn log(
    root: &Path,
    branch: &str,
    path: &str,
    compare: bool,
) -> Result<Vec<CommitRow>, BridgeError> {
    let mut args = vec![
        "log".into(),
        "-200".into(),
        "--graph".into(),
        "--date=short".into(),
        "--pretty=format:%x00%H%x00%s%x00%an%x00%ad%x00%D".into(),
    ];
    if compare {
        args.push("--left-right".into());
    }
    let range = if compare {
        format!("HEAD...refs/heads/{branch}")
    } else if branch.is_empty() {
        "HEAD".into()
    } else if branch.contains('/') {
        branch.to_owned()
    } else {
        format!("refs/heads/{branch}")
    };
    args.extend(["--end-of-options".into(), range, "--".into()]);
    if !compare && !path.is_empty() {
        args.push(path.to_owned());
    }
    let text = run_git(root, &args).await?;
    Ok(text
        .lines()
        .filter_map(|line| {
            let mut f = line.split('\0');
            Some(CommitRow {
                graph: f.next()?.to_owned(),
                id: f.next()?.to_owned(),
                subject: f.next()?.to_owned(),
                author: f.next()?.to_owned(),
                date: f.next()?.to_owned(),
                refs: f.next()?.to_owned(),
            })
        })
        .collect())
}

pub(super) async fn files(root: &Path, revision: &Revision) -> Result<Vec<String>, BridgeError> {
    Ok(run_git(root, &revision.args(true))
        .await?
        .split('\0')
        .filter(|s| !s.is_empty())
        .map(str::to_owned)
        .collect())
}
pub(super) async fn diff(
    root: &Path,
    revision: &Revision,
    path: String,
) -> Result<String, BridgeError> {
    let mut args = revision.args(false);
    args.push(path);
    run_git(root, &args).await
}

/// Keep the file header and exactly one complete hunk, including newline markers.
pub fn hunks(diff: &str) -> (&str, Vec<&str>) {
    let mut offsets: Vec<_> = diff
        .match_indices("\n@@ ")
        .map(|(offset, _)| offset + 1)
        .collect();
    let head = &diff[..offsets.first().copied().unwrap_or(diff.len())];
    offsets.push(diff.len());
    (
        head,
        offsets.windows(2).map(|p| &diff[p[0]..p[1]]).collect(),
    )
}

pub(super) async fn apply(root: &Path, patch: String, reverse: bool) -> Result<(), BridgeError> {
    use std::process::Stdio;
    let mut command = Command::new("git");
    command
        .arg("-C")
        .arg(root)
        .args(["apply", "--cached", "--whitespace=nowarn"]);
    if reverse {
        command.arg("--reverse");
    }
    let mut child = command
        .arg("-")
        .stdin(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(|e| BridgeError::unavailable(e.to_string()))?;
    let mut input = child.stdin.take().expect("piped stdin");
    let (write, result) = tokio::join!(
        async move { input.write_all(patch.as_bytes()).await },
        child.wait_with_output()
    );
    let output = result.map_err(|e| BridgeError::unavailable(e.to_string()))?;
    if !output.status.success() {
        return Err(BridgeError::unavailable(
            String::from_utf8_lossy(&output.stderr).trim().to_owned(),
        ));
    }
    write.map_err(|e| BridgeError::unavailable(e.to_string()))
}
