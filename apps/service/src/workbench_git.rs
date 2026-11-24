//! Full Git workbench operations matching Go v0.1.42:
//! log, commit detail, branch compare, checkpoints, worktree status/cleanup,
//! branch management, sync (fetch/pull/push), rebase, stash, hunk stage/unstage/revert,
//! and file search.
use anyhow::{Context, Result, bail, ensure};
use chrono::Utc;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    path::{Path, PathBuf},
    process::Command,
    time::Duration,
};
use vantry_platform::{git, hostprocess, workspace};
use vantry_protocol::*;

const CHECKPOINT_REF_PREFIX: &str = "refs/vantry/checkpoints/";
const COMMIT_LOG_FORMAT: &str = "%H%x1f%h%x1f%s%x1f%an%x1f%aI%x1f%D%x1f%P%x1e";

fn run_git(root: &Path, args: &[&str], stdin: &[u8]) -> Result<(bool, String, String)> {
    let mut cmd = Command::new("git");
    hostprocess::clean_environment(&mut cmd);
    cmd.current_dir(root)
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GIT_MERGE_AUTOEDIT", "no")
        .arg("--literal-pathspecs")
        .args(args);
    let out = hostprocess::capture(&mut cmd, stdin, Duration::from_secs(60), 16 << 20)?;
    Ok((
        out.status.success(),
        String::from_utf8_lossy(&out.stdout).into_owned(),
        String::from_utf8_lossy(&out.stderr).into_owned(),
    ))
}

fn git_ok(root: &Path, args: &[&str]) -> Result<String> {
    let (ok, stdout, stderr) = run_git(root, args, &[])?;
    ensure!(
        ok,
        "git {} failed: {}",
        args.first().copied().unwrap_or(""),
        stderr.trim()
    );
    Ok(stdout)
}

fn validate_ref_arg(rev: &str) -> Result<&str> {
    let trimmed = rev.trim();
    ensure!(
        !trimmed.is_empty() && !trimmed.starts_with('-') && !trimmed.contains('\0'),
        "请选择有效的提交或分支引用"
    );
    Ok(trimmed)
}

fn parse_commits(raw: &str) -> Vec<GitCommit> {
    let mut commits = Vec::new();
    for record in raw.split('\x1e') {
        let record = record.strip_prefix('\n').unwrap_or(record);
        if record.is_empty() {
            continue;
        }
        let fields: Vec<&str> = record.split('\x1f').collect();
        if fields.len() != 7 || fields[0].is_empty() {
            continue;
        }
        let refs = if fields[5].is_empty() {
            Vec::new()
        } else {
            fields[5].split(", ").map(str::to_owned).collect()
        };
        let parents = fields[6].split_whitespace().map(str::to_owned).collect();
        commits.push(GitCommit {
            id: fields[0].to_owned(),
            short_id: fields[1].to_owned(),
            subject: fields[2].to_owned(),
            author: fields[3].to_owned(),
            author_time: fields[4].to_owned(),
            refs,
            parents,
        });
    }
    commits
}

#[derive(Debug, Default, Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct LogFilter {
    query: String,
    branch: String,
    author: String,
    since: String,
    until: String,
    path: String,
    limit: usize,
}

pub fn git_log(root: &Path, req: &WorkbenchRequest) -> Result<Vec<GitCommit>> {
    let mut filter: LogFilter = req
        .input
        .as_ref()
        .and_then(|v| serde_json::from_value(v.clone()).ok())
        .unwrap_or_default();
    if filter.query.is_empty() {
        filter.query = req.message.clone();
    }
    if filter.branch.is_empty() {
        filter.branch = req.branch.clone();
    }
    if filter.limit == 0 {
        filter.limit = 200;
    }
    let limit = filter.limit.clamp(1, 500).to_string();
    let format_arg = format!("--format={COMMIT_LOG_FORMAT}");
    let mut args: Vec<String> = vec![
        "log".into(),
        "-n".into(),
        limit,
        "--date-order".into(),
        "--no-color".into(),
        "--no-show-signature".into(),
        "--decorate=short".into(),
        format_arg,
    ];
    if !filter.author.trim().is_empty() {
        args.push(format!("--author={}", filter.author.trim()));
        args.push("--regexp-ignore-case".into());
        args.push("--fixed-strings".into());
    }
    if !filter.since.trim().is_empty() {
        args.push(format!("--since={}", filter.since.trim()));
    }
    if !filter.until.trim().is_empty() {
        args.push(format!("--until={}", filter.until.trim()));
    }
    if !filter.query.trim().is_empty() {
        args.push(format!("--grep={}", filter.query.trim()));
        args.push("--regexp-ignore-case".into());
        args.push("--fixed-strings".into());
    }
    match filter.branch.trim() {
        "" => {
            args.push("HEAD".into());
        }
        "*" => {
            args.push("--all".into());
        }
        branch => {
            let rev = validate_ref_arg(branch)?;
            args.push(rev.to_owned());
        }
    }
    args.push("--".into());
    if !filter.path.trim().is_empty() {
        args.push(filter.path.trim().to_owned());
    }
    let arg_refs: Vec<&str> = args.iter().map(String::as_str).collect();
    let (ok, stdout, stderr) = run_git(root, &arg_refs, &[])?;
    if !ok {
        if stderr.contains("does not have any commits yet")
            || stderr.contains("bad default revision 'HEAD'")
        {
            return Ok(Vec::new());
        }
        bail!("git log failed: {}", stderr.trim());
    }
    Ok(parse_commits(&stdout))
}

pub fn git_commit_detail(root: &Path, revision: &str) -> Result<Value> {
    let rev = validate_ref_arg(revision)?;
    let full_id = git_ok(root, &["rev-parse", "--verify", "--end-of-options", &format!("{rev}^{{commit}}")])?
        .trim()
        .to_owned();
    let format_arg = format!("--format={COMMIT_LOG_FORMAT}");
    let meta = git_ok(
        root,
        &[
            "show",
            "-s",
            "--no-color",
            "--no-show-signature",
            "--decorate=short",
            &format_arg,
            "--end-of-options",
            &full_id,
        ],
    )?;
    let commit = parse_commits(&meta)
        .into_iter()
        .next()
        .context("commit not found")?;
    let body = git_ok(
        root,
        &["show", "-s", "--no-color", "--format=%b", "--end-of-options", &full_id],
    )
    .unwrap_or_default();
    let numstat = git_ok(
        root,
        &[
            "show",
            "--format=",
            "--name-status",
            "--no-color",
            "--end-of-options",
            &full_id,
        ],
    )
    .unwrap_or_default();
    let files = parse_name_status(&numstat);
    let diff = git_ok(
        root,
        &[
            "show",
            "--format=",
            "--patch",
            "--no-color",
            "--no-ext-diff",
            "--no-textconv",
            "--end-of-options",
            &full_id,
        ],
    )
    .unwrap_or_default();
    let (diff, truncated) = truncate_diff(diff, 256 << 10);
    Ok(json!({
        "commit": commit,
        "body": body.trim(),
        "files": files,
        "diff": diff,
        "truncated": truncated,
    }))
}

pub fn git_compare(root: &Path, base_branch: &str, head_branch: &str) -> Result<Value> {
    let base = if base_branch.trim().is_empty() {
        "HEAD"
    } else {
        validate_ref_arg(base_branch)?
    };
    let head = validate_ref_arg(head_branch)?;
    let range = format!("{base}..{head}");
    let format_arg = format!("--format={COMMIT_LOG_FORMAT}");
    let raw_commits = git_ok(
        root,
        &[
            "log",
            "-n",
            "200",
            "--date-order",
            "--no-color",
            "--no-show-signature",
            "--decorate=short",
            &format_arg,
            "--end-of-options",
            &range,
            "--",
        ],
    )
    .unwrap_or_default();
    let commits = parse_commits(&raw_commits);
    let status_out = git_ok(
        root,
        &[
            "diff",
            "--name-status",
            "--no-color",
            "--end-of-options",
            base,
            head,
            "--",
        ],
    )
    .unwrap_or_default();
    let files = parse_name_status(&status_out);
    let diff = git_ok(
        root,
        &[
            "diff",
            "--no-color",
            "--no-ext-diff",
            "--no-textconv",
            "--end-of-options",
            base,
            head,
            "--",
        ],
    )
    .unwrap_or_default();
    let (diff, truncated) = truncate_diff(diff, 256 << 10);
    Ok(json!({
        "base": base,
        "head": head,
        "commits": commits,
        "files": files,
        "diff": diff,
        "truncated": truncated,
    }))
}

fn parse_name_status(raw: &str) -> Vec<GitFile> {
    let mut files = Vec::new();
    for line in raw.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let mut parts = trimmed.split('\t');
        let Some(code) = parts.next() else { continue };
        let Some(first) = parts.next() else { continue };
        let ch = code.chars().next().unwrap_or('M').to_string();
        if let Some(second) = parts.next() {
            files.push(GitFile {
                path: second.to_owned(),
                original_path: first.to_owned(),
                index: ch,
                worktree: " ".into(),
            });
        } else {
            files.push(GitFile {
                path: first.to_owned(),
                original_path: String::new(),
                index: ch,
                worktree: " ".into(),
            });
        }
    }
    files
}

fn truncate_diff(mut diff: String, limit: usize) -> (String, bool) {
    if diff.len() <= limit {
        return (diff, false);
    }
    let mut end = limit;
    while end > 0 && !diff.is_char_boundary(end) {
        end -= 1;
    }
    diff.truncate(end);
    (diff, true)
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Checkpoint {
    pub id: String,
    pub r#ref: String,
    pub label: String,
    pub created_at: String,
}

fn valid_checkpoint_id(id: &str) -> Result<String> {
    let trimmed = id.trim();
    ensure!(
        !trimmed.is_empty()
            && trimmed.len() <= 128
            && !trimmed.starts_with('.')
            && !trimmed.contains("..")
            && trimmed
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b)),
        "invalid checkpoint ID"
    );
    Ok(trimmed.to_owned())
}

pub fn list_checkpoints(root: &Path) -> Result<Vec<Checkpoint>> {
    let (ok, stdout, _) = run_git(
        root,
        &[
            "for-each-ref",
            "--sort=-creatordate",
            "--format=%(refname)%00%(creatordate:iso-strict)%00%(contents:subject)",
            CHECKPOINT_REF_PREFIX,
        ],
        &[],
    )?;
    if !ok {
        return Ok(Vec::new());
    }
    let mut out = Vec::new();
    for line in stdout.lines() {
        if line.is_empty() {
            continue;
        }
        let parts: Vec<&str> = line.splitn(3, '\0').collect();
        if parts.len() != 3 {
            continue;
        }
        let id = parts[0]
            .strip_prefix(CHECKPOINT_REF_PREFIX)
            .unwrap_or(parts[0])
            .to_owned();
        out.push(Checkpoint {
            id,
            r#ref: parts[0].to_owned(),
            label: parts[2].to_owned(),
            created_at: parts[1].to_owned(),
        });
    }
    Ok(out)
}

pub fn capture_checkpoint(root: &Path, id: &str, label: &str) -> Result<Checkpoint> {
    let id = if id.trim().is_empty() {
        format!("cp-{}", Utc::now().format("%Y%m%d%H%M%S"))
    } else {
        valid_checkpoint_id(id)?
    };
    let ref_name = format!("{CHECKPOINT_REF_PREFIX}{id}");
    let msg = if label.trim().is_empty() {
        format!("Vantry checkpoint {id}")
    } else {
        label.trim().to_owned()
    };
    let tmp_index = std::env::temp_dir().join(format!(
        "vantry-cp-index-{}-{}.idx",
        std::process::id(),
        uuid::Uuid::new_v4().simple()
    ));
    let _cleanup = TempFileGuard(tmp_index.clone());

    let run_with_index = |args: &[&str]| -> Result<String> {
        let mut cmd = Command::new("git");
        hostprocess::clean_environment(&mut cmd);
        cmd.current_dir(root)
            .env("GIT_INDEX_FILE", &tmp_index)
            .env("GIT_TERMINAL_PROMPT", "0")
            .arg("--literal-pathspecs")
            .args(args);
        let out = hostprocess::capture(&mut cmd, &[], Duration::from_secs(30), 4 << 20)?;
        ensure!(
            out.status.success(),
            "git {} failed: {}",
            args.first().copied().unwrap_or(""),
            String::from_utf8_lossy(&out.stderr).trim()
        );
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    };

    let head = git_ok(root, &["rev-parse", "--verify", "HEAD"])
        .ok()
        .map(|s| s.trim().to_owned());
    if let Some(ref h) = head {
        let _ = run_with_index(&["read-tree", h]);
    }
    let _ = run_with_index(&["add", "-A"]);
    let tree = run_with_index(&["write-tree"])?.trim().to_owned();
    let commit_id = if let Some(ref h) = head {
        git_ok(root, &["commit-tree", &tree, "-p", h, "-m", &msg])?
            .trim()
            .to_owned()
    } else {
        git_ok(root, &["commit-tree", &tree, "-m", &msg])?
            .trim()
            .to_owned()
    };
    git_ok(root, &["update-ref", &ref_name, &commit_id])?;
    Ok(Checkpoint {
        id,
        r#ref: ref_name,
        label: msg,
        created_at: Utc::now().to_rfc3339(),
    })
}

pub fn diff_checkpoint(root: &Path, id: &str) -> Result<String> {
    let id = valid_checkpoint_id(id)?;
    let ref_name = format!("{CHECKPOINT_REF_PREFIX}{id}");
    git_ok(root, &["diff", "--no-color", &ref_name, "--"])
}

pub fn restore_checkpoint(root: &Path, id: &str) -> Result<Value> {
    let id = valid_checkpoint_id(id)?;
    let ref_name = format!("{CHECKPOINT_REF_PREFIX}{id}");
    let backup = capture_checkpoint(
        root,
        &format!("backup-{}", Utc::now().format("%Y%m%d%H%M%S")),
        &format!("Before restore {id}"),
    )?;
    git_ok(root, &["checkout", &ref_name, "--", "."])?;
    Ok(json!({"backup": backup}))
}

struct TempFileGuard(PathBuf);
impl Drop for TempFileGuard {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

pub fn worktree_status(
    data_dir: &Path,
    thread_id: &str,
    checkout_path: Option<&Path>,
    spec: Option<&WorkspaceSpec>,
) -> Result<Value> {
    let mut is_worktree = spec.is_some_and(|s| s.kind == "worktree");
    let mut managed = false;
    let mut exists = false;
    let mut path_str = checkout_path
        .map(|p| p.display().to_string())
        .or_else(|| spec.map(|s| s.path.clone()))
        .unwrap_or_default();
    let mut repository = spec.map(|s| s.repository.clone()).unwrap_or_default();
    let mut branch = spec.map(|s| s.branch.clone()).unwrap_or_default();
    let mut base_branch = spec.map(|s| s.base_ref.clone()).unwrap_or_default();
    let mut dirty = false;
    let mut unmerged_commits: i64 = 0;

    if !thread_id.is_empty() {
        let record_path = data_dir
            .join("workspaces")
            .join(thread_id)
            .join("prepared.json");
        if record_path.is_file() {
            managed = true;
            if let Ok(bytes) = std::fs::read(&record_path)
                && let Ok(val) = serde_json::from_slice::<Value>(&bytes)
            {
                if path_str.is_empty() {
                    path_str = val["path"].as_str().unwrap_or_default().to_owned();
                }
                if repository.is_empty() {
                    repository = val["spec"]["repository"]
                        .as_str()
                        .unwrap_or_default()
                        .to_owned();
                }
                if branch.is_empty() {
                    branch = val["spec"]["branch"]
                        .as_str()
                        .unwrap_or_default()
                        .to_owned();
                }
                if base_branch.is_empty() {
                    base_branch = val["spec"]["baseRef"]
                        .as_str()
                        .unwrap_or_default()
                        .to_owned();
                }
            }
        }
    }

    if !path_str.is_empty() && Path::new(&path_str).exists() {
        exists = true;
        if let Ok(repo) = git::Repository::discover(Path::new(&path_str)) {
            let root = repo.root().to_path_buf();
            path_str = root.display().to_string();
            let git_dir = git_ok(&root, &["rev-parse", "--path-format=absolute", "--git-dir"])
                .ok()
                .and_then(|s| PathBuf::from(s.trim()).canonicalize().ok());
            let common_dir = repo.common_dir().ok();
            if let (Some(gd), Some(cd)) = (&git_dir, &common_dir)
                && gd != cd
            {
                is_worktree = true;
                if repository.is_empty()
                    && let Some(parent) = cd.parent()
                {
                    repository = parent.display().to_string();
                }
            }
            if branch.is_empty() {
                branch = repo.branch().unwrap_or_default();
            }
            dirty = !repo.status().unwrap_or_default().is_empty();
            if is_worktree && !branch.is_empty() {
                let target = if base_branch.is_empty() {
                    "HEAD".to_owned()
                } else {
                    base_branch.clone()
                };
                if let Ok(count) =
                    git_ok(&root, &["rev-list", "--count", &format!("{target}..{branch}")])
                {
                    unmerged_commits = count.trim().parse().unwrap_or(0);
                }
            }
        }
    }

    let can_merge = is_worktree && exists && !dirty && !repository.is_empty() && !branch.is_empty();
    let can_cleanup = is_worktree && managed && exists;
    Ok(json!({
        "isWorktree": is_worktree,
        "managed": managed,
        "exists": exists,
        "path": path_str,
        "repository": repository,
        "branch": branch,
        "baseBranch": base_branch,
        "unmergedCommits": unmerged_commits,
        "dirty": dirty,
        "canMerge": can_merge,
        "canCleanup": can_cleanup,
    }))
}

pub fn worktree_cleanup(data_dir: &Path, thread_id: &str, merge_first: bool) -> Result<Value> {
    let record_path = data_dir
        .join("workspaces")
        .join(thread_id)
        .join("prepared.json");
    let rec: Value = serde_json::from_slice(
        &std::fs::read(&record_path).context("no managed worktree record for thread")?,
    )?;
    let repo_path = rec["spec"]["repository"]
        .as_str()
        .filter(|s| !s.is_empty())
        .context("missing worktree repository")?;
    let wt_path = rec["path"]
        .as_str()
        .filter(|s| !s.is_empty())
        .context("missing worktree path")?;
    let branch = rec["spec"]["branch"].as_str().unwrap_or_default();
    let repo = git::Repository::discover(Path::new(repo_path))?;
    let target_branch = repo.branch().unwrap_or_else(|_| "HEAD".into());
    let mut output = String::new();
    if merge_first && !branch.is_empty() {
        output = git_ok(repo.root(), &["merge", "--no-edit", "--", branch])?;
    }
    let _ = git_ok(repo.root(), &["worktree", "remove", "--force", wt_path]);
    let branch_deleted = if !branch.is_empty() {
        run_git(repo.root(), &["branch", "-d", branch], &[])
            .map(|(ok, _, _)| ok)
            .unwrap_or(false)
    } else {
        false
    };
    let _ = std::fs::remove_file(&record_path);
    Ok(json!({
        "repository": repo.root().display().to_string(),
        "targetBranch": target_branch,
        "mergedBranch": branch,
        "cleanedUp": true,
        "branchDeleted": branch_deleted,
        "output": output,
    }))
}

pub fn handle_extra_git_action(
    repo: &git::Repository,
    req: &WorkbenchRequest,
    input: &Value,
) -> Result<Option<String>> {
    let root = repo.root();
    match req.action.as_str() {
        "git.rebase" => {
            let target = if !req.name.trim().is_empty() {
                req.name.trim()
            } else {
                req.branch.trim()
            };
            let target = validate_ref_arg(target)?;
            Ok(Some(git_ok(root, &["rebase", "--", target])?))
        }
        "git.branch.create" => {
            let name = validate_ref_arg(&req.name)?;
            let mut args = vec!["checkout", "-b", name];
            if !req.branch.trim().is_empty() {
                args.push(validate_ref_arg(&req.branch)?);
            }
            Ok(Some(git_ok(root, &args)?))
        }
        "git.branch.update" => {
            let target = if !req.name.trim().is_empty() {
                req.name.trim()
            } else {
                req.branch.trim()
            };
            let target = validate_ref_arg(target)?;
            Ok(Some(git_ok(root, &["fetch", "origin", target])?))
        }
        "git.branch.rename" => {
            let old = validate_ref_arg(&req.branch)?;
            let new = validate_ref_arg(&req.name)?;
            Ok(Some(git_ok(root, &["branch", "-m", old, new])?))
        }
        "git.branch.delete" => {
            let target = if !req.branch.trim().is_empty() {
                req.branch.trim()
            } else {
                req.name.trim()
            };
            let target = validate_ref_arg(target)?;
            let flag = if req.amend { "-D" } else { "-d" };
            Ok(Some(git_ok(root, &["branch", flag, target])?))
        }
        "git.fetch" => Ok(Some(git_ok(root, &["fetch", "--all", "--prune"])?)),
        "git.pull" => {
            let mut args = vec!["pull", "--ff-only"];
            if !req.branch.trim().is_empty() {
                args.push("origin");
                args.push(validate_ref_arg(&req.branch)?);
            }
            Ok(Some(git_ok(root, &args)?))
        }
        "git.push" => {
            let mut args = vec!["push"];
            if !req.branch.trim().is_empty() {
                args.push("-u");
                args.push("origin");
                args.push(validate_ref_arg(&req.branch)?);
            }
            Ok(Some(git_ok(root, &args)?))
        }
        "git.stash" => {
            let mut args = vec!["stash", "push", "--include-untracked"];
            if !req.message.trim().is_empty() {
                args.push("-m");
                args.push(req.message.trim());
            }
            Ok(Some(git_ok(root, &args)?))
        }
        "git.stash.pop" | "git.stash_pop" => {
            let mut args = vec!["stash", "pop"];
            if !req.name.trim().is_empty() {
                args.push(validate_ref_arg(&req.name)?);
            }
            Ok(Some(git_ok(root, &args)?))
        }
        "git.stash.drop" | "git.stash_drop" => {
            let mut args = vec!["stash", "drop"];
            if !req.name.trim().is_empty() {
                args.push(validate_ref_arg(&req.name)?);
            }
            Ok(Some(git_ok(root, &args)?))
        }
        "git.hunk.stage" | "git.hunk.unstage" | "git.hunk.revert" => {
            let patch = input["patch"]
                .as_str()
                .filter(|s| !s.is_empty())
                .context("missing hunk patch")?;
            let mut args = vec!["apply", "--whitespace=nowarn"];
            match req.action.as_str() {
                "git.hunk.stage" => args.push("--cached"),
                "git.hunk.unstage" => {
                    args.push("--cached");
                    args.push("--reverse");
                }
                "git.hunk.revert" => args.push("--reverse"),
                _ => {}
            }
            args.push("-");
            let (ok, stdout, stderr) = run_git(root, &args, patch.as_bytes())?;
            ensure!(ok, "git apply failed: {}", stderr.trim());
            Ok(Some(stdout))
        }
        "git.commit_op" => {
            let rev = validate_ref_arg(&req.name)?;
            match req.message.trim() {
                "cherry-pick" | "cherry_pick" => Ok(Some(git_ok(root, &["cherry-pick", rev])?)),
                "revert" => Ok(Some(git_ok(root, &["revert", "--no-edit", rev])?)),
                "reset-soft" => Ok(Some(git_ok(root, &["reset", "--soft", rev])?)),
                "reset-mixed" => Ok(Some(git_ok(root, &["reset", "--mixed", rev])?)),
                "patch" => Ok(Some(git_ok(root, &["format-patch", "-1", "--stdout", rev])?)),
                other => bail!("unknown commit operation: {other}"),
            }
        }
        _ => Ok(None),
    }
}

pub fn enrich_git_state(repo: &git::Repository, state: &mut GitState) {
    let root = repo.root();
    if let Ok(up) = git_ok(root, &["rev-parse", "--abbrev-ref", "@{upstream}"]) {
        state.remote_branch = up.trim().to_owned();
        if let Ok(counts) = git_ok(
            root,
            &["rev-list", "--left-right", "--count", "HEAD...@{upstream}"],
        ) {
            let mut parts = counts.split_whitespace();
            state.ahead = parts.next().and_then(|s| s.parse().ok()).unwrap_or(0);
            state.behind = parts.next().and_then(|s| s.parse().ok()).unwrap_or(0);
        }
    }
    if let Ok(remotes) = git_ok(
        root,
        &["for-each-ref", "--format=%(refname:short)", "refs/remotes"],
    ) {
        state.remote_branches = remotes
            .lines()
            .filter(|l| !l.ends_with("/HEAD"))
            .map(str::to_owned)
            .collect();
    }
    if let Ok(stash_out) = git_ok(root, &["stash", "list", "--format=%gd%x1f%gs"]) {
        state.stashes = stash_out
            .lines()
            .filter_map(|line| {
                let (r, msg) = line.split_once('\x1f')?;
                Some(GitStash {
                    r#ref: r.to_owned(),
                    branch: String::new(),
                    message: msg.to_owned(),
                })
            })
            .collect();
    }
    state.conflicts = state.files.as_deref().unwrap_or(&[]).iter().any(|f| {
        f.index == "U"
            || f.worktree == "U"
            || (f.index == "A" && f.worktree == "A")
            || (f.index == "D" && f.worktree == "D")
    });
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct FileMatch {
    pub path: String,
    pub name: String,
    pub score: i32,
    pub match_kind: &'static str,
}

pub fn search_files(root: &Path, query: &str, limit: usize) -> Result<Vec<FileMatch>> {
    let limit = limit.clamp(1, 200);
    let candidates = git_candidate_files(root).unwrap_or_else(|| walk_candidate_files(root, 10_000));
    let needle = query.trim().to_ascii_lowercase();
    let mut matches: Vec<FileMatch> = candidates
        .into_iter()
        .filter_map(|rel| {
            let name = Path::new(&rel)
                .file_name()
                .map(|s| s.to_string_lossy().into_owned())
                .unwrap_or_else(|| rel.clone());
            if needle.is_empty() {
                return Some(FileMatch {
                    path: rel,
                    name,
                    score: 1,
                    match_kind: "all",
                });
            }
            let name_lc = name.to_ascii_lowercase();
            let path_lc = rel.to_ascii_lowercase();
            let (score, kind) = if name_lc == needle {
                (1000, "exact")
            } else if name_lc.starts_with(&needle) {
                (800, "prefix")
            } else if name_lc.contains(&needle) {
                (600, "name")
            } else if path_lc.contains(&needle) {
                (400, "path")
            } else if is_subsequence(&needle, &path_lc) {
                (200, "fuzzy")
            } else {
                return None;
            };
            Some(FileMatch {
                path: rel,
                name,
                score,
                match_kind: kind,
            })
        })
        .collect();
    matches.sort_by(|a, b| {
        b.score
            .cmp(&a.score)
            .then_with(|| a.path.matches('/').count().cmp(&b.path.matches('/').count()))
            .then_with(|| a.path.len().cmp(&b.path.len()))
            .then_with(|| a.path.cmp(&b.path))
    });
    matches.truncate(limit);
    Ok(matches)
}

fn is_subsequence(needle: &str, haystack: &str) -> bool {
    let mut chars = needle.chars();
    let Some(mut cur) = chars.next() else {
        return true;
    };
    for h in haystack.chars() {
        if h == cur {
            match chars.next() {
                Some(next) => cur = next,
                None => return true,
            }
        }
    }
    false
}

fn git_candidate_files(root: &Path) -> Option<Vec<String>> {
    let (ok, stdout, _) = run_git(
        root,
        &["ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        &[],
    )
    .ok()?;
    if !ok {
        return None;
    }
    Some(
        stdout
            .split('\0')
            .filter(|s| !s.is_empty())
            .map(str::to_owned)
            .collect(),
    )
}

fn walk_candidate_files(root: &Path, max_files: usize) -> Vec<String> {
    let mut out = Vec::new();
    let mut stack = vec![PathBuf::new()];
    while let Some(rel) = stack.pop() {
        if out.len() >= max_files {
            break;
        }
        let Ok(entries) = workspace::list(root, &rel, 1000) else {
            continue;
        };
        for entry in entries {
            let name = entry.name.to_string_lossy();
            if matches!(name.as_ref(), ".git" | "node_modules" | "target" | ".venv") {
                continue;
            }
            let child = if rel.as_os_str().is_empty() {
                PathBuf::from(&entry.name)
            } else {
                rel.join(&entry.name)
            };
            if entry.directory {
                stack.push(child);
            } else {
                out.push(child.to_string_lossy().replace('\\', "/"));
                if out.len() >= max_files {
                    break;
                }
            }
        }
    }
    out
}
