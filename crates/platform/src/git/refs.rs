//! Branch picker operations shared by Core and the desktop local workbench.
use super::{Repository, has_ref, line, run};
use anyhow::{Context, Result, bail, ensure};
use serde::{Deserialize, Serialize};
use std::{collections::HashSet, path::Path};
use vantry_protocol::WorkbenchRequest;

#[cfg(test)]
mod tests;

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Branch {
    pub name: String,
    pub upstream: String,
    pub ahead: usize,
    pub behind: usize,
    pub remote: bool,
    pub favorite: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Catalog {
    pub current: String,
    pub branches: Vec<Branch>,
    pub tags: Vec<String>,
    pub recent: Vec<String>,
    pub remotes: Vec<String>,
}

fn argument(value: &str) -> Result<&str> {
    ensure!(
        !value.is_empty() && !value.starts_with('-') && !value.chars().any(char::is_control),
        "无效的 Git 引用：{value:?}"
    );
    Ok(value)
}
fn oid(root: &Path, value: &str) -> Result<String> {
    argument(value)?;
    line(
        root,
        &[
            "rev-parse",
            "--verify",
            "--end-of-options",
            &format!("{value}^{{commit}}"),
        ],
    )
}
fn recent(log: &str, branches: &[Branch]) -> Vec<String> {
    let local: HashSet<_> = branches
        .iter()
        .filter(|b| !b.remote)
        .map(|b| b.name.as_str())
        .collect();
    let mut seen = HashSet::new();
    log.lines()
        .filter_map(|l| l.strip_prefix("checkout: moving from ")?.split_once(" to "))
        .flat_map(|(from, to)| [to, from])
        .filter(|b| local.contains(b) && seen.insert(*b))
        .map(str::to_owned)
        .collect()
}

fn favorite_key(remote: bool, name: &str) -> String {
    // Git moves/removes the branch subsection when a local branch is renamed/deleted.
    let section = if remote { "vantry-remote" } else { "branch" };
    format!("{section}.{name}.vantry-favorite")
}

fn catalog(repo: &Repository) -> Result<Catalog> {
    let root = repo.root();
    let mut result = Catalog {
        current: repo.branch().unwrap_or_default(),
        remotes: run(root, &["remote"])?.lines().map(str::to_owned).collect(),
        ..Default::default()
    };
    let favorites = run(
        root,
        &[
            "config",
            "--local",
            "--name-only",
            "--get-regexp",
            r"^(branch|vantry-remote)\..*\.vantry-favorite$",
            "^true$",
        ],
    )
    .unwrap_or_default();
    let favorites: HashSet<_> = favorites.lines().collect();
    let rows = run(
        root,
        &[
            "for-each-ref",
            "--format=%(refname)%00%(symref)%00%(upstream:short)%00%(upstream:track,nobracket)",
            "refs/heads",
            "refs/remotes",
            "refs/tags",
        ],
    )?;
    for row in rows.lines() {
        let mut fields = row.split('\0');
        let full = fields.next().unwrap_or_default();
        if fields.next() != Some("") {
            continue;
        }
        if let Some(tag) = full.strip_prefix("refs/tags/") {
            result.tags.push(tag.into());
            continue;
        }
        let remote = full.starts_with("refs/remotes/");
        let Some(name) = full
            .strip_prefix("refs/heads/")
            .or_else(|| full.strip_prefix("refs/remotes/"))
        else {
            continue;
        };
        let mut branch = Branch {
            name: name.into(),
            upstream: fields.next().unwrap_or_default().into(),
            remote,
            favorite: favorites.contains(favorite_key(remote, name).as_str()),
            ..Default::default()
        };
        for part in fields.next().unwrap_or_default().split(", ") {
            if let Some(n) = part.strip_prefix("ahead ") {
                branch.ahead = n.parse().unwrap_or(0);
            }
            if let Some(n) = part.strip_prefix("behind ") {
                branch.behind = n.parse().unwrap_or(0);
            }
        }
        result.branches.push(branch);
    }
    if has_ref(root, "HEAD")? {
        result.recent = recent(
            &run(root, &["reflog", "show", "--format=%gs", "HEAD", "--"])?,
            &result.branches,
        );
    }
    Ok(result)
}

pub fn read_only(action: &str) -> bool {
    matches!(
        action,
        "git.refs.list" | "git.refs.compare" | "git.refs.diff" | "git.refs.history"
    )
}

pub fn action(repo: &Repository, req: &WorkbenchRequest) -> Result<String> {
    let root = repo.root();
    let op = req
        .action
        .strip_prefix("git.refs.")
        .context("unknown Git operation")?;
    if op == "list" {
        return Ok(serde_json::to_string(&catalog(repo)?)?);
    }
    let _guard = if read_only(&req.action) {
        None
    } else {
        Some(repo.lock()?)
    };
    let current = repo.branch().unwrap_or_default();
    if let Some(expected) = req.input.as_ref().and_then(|v| v["current"].as_str()) {
        ensure!(expected == current, "当前分支已更改，请重新打开分支菜单");
    }
    let target = argument(&req.branch)?;
    let name = req.name.as_str();
    let local = target.strip_prefix("refs/heads/");
    let local_name = || -> Result<&str> {
        let branch = argument(local.context("请选择本地分支")?)?;
        repo.validate_branch(branch)?;
        Ok(branch)
    };
    match op {
        "fetch" => run(root, &["fetch", "--all", "--prune"]),
        "checkout" => {
            if let Some(branch) = local {
                repo.validate_branch(branch)?;
                return run(
                    root,
                    &[
                        "switch",
                        "--no-overwrite-ignore",
                        "--no-guess",
                        "--",
                        branch,
                    ],
                );
            }
            if let Some(remote) = target.strip_prefix("refs/remotes/") {
                let (_, branch) = remote.split_once('/').context("invalid remote branch")?;
                repo.validate_branch(branch)?;
                let head = format!("refs/heads/{branch}");
                if has_ref(root, &head)? {
                    ensure!(
                        line(root, &["for-each-ref", "--format=%(upstream)", "--", &head])?
                            == target,
                        "已有同名本地分支，请选择「新建分支」"
                    );
                    return run(
                        root,
                        &[
                            "switch",
                            "--no-overwrite-ignore",
                            "--no-guess",
                            "--",
                            branch,
                        ],
                    );
                }
                return run(
                    root,
                    &[
                        "switch",
                        "--no-overwrite-ignore",
                        "--track",
                        "-c",
                        branch,
                        "--",
                        target,
                    ],
                );
            }
            run(
                root,
                &[
                    "switch",
                    "--no-overwrite-ignore",
                    "--detach",
                    "--",
                    &oid(root, target)?,
                ],
            )
        }
        "create" => {
            repo.validate_branch(argument(name)?)?;
            run(
                root,
                &[
                    "switch",
                    "--no-overwrite-ignore",
                    "-c",
                    name,
                    "--",
                    &oid(root, target)?,
                ],
            )
        }
        "worktree" => {
            repo.validate_branch(argument(name)?)?;
            ensure!(
                !name.contains(['/', '\\', ':']) && !name.starts_with('.'),
                "请输入单层目录名作为新分支名"
            );
            let parent = root.parent().context("工作区没有父目录")?;
            let path = parent.join(name);
            ensure!(!path.exists(), "工作树目录已存在");
            run(
                root,
                &[
                    "worktree",
                    "add",
                    "-b",
                    name,
                    "--",
                    super::path_str(&path)?,
                    &oid(root, target)?,
                ],
            )?;
            Ok(path.display().to_string())
        }
        "merge" => run(root, &["merge", "--no-edit", "--", &oid(root, target)?]),
        "rebase" => run(root, &["rebase", "--", &oid(root, target)?]),
        "checkout-rebase" => {
            ensure!(!current.is_empty(), "请先检出本地分支");
            let branch = local_name()?;
            run(root, &["rebase", "--", &oid(root, "HEAD")?, branch])
        }
        "rename" => {
            repo.validate_branch(argument(name)?)?;
            run(root, &["branch", "-m", "--", local_name()?, name])
        }
        "delete" | "force-delete" => {
            if op == "delete"
                && super::execute(
                    root,
                    &["merge-base", "--is-ancestor", &oid(root, target)?, "HEAD"],
                )?
                .status
                .code()
                    == Some(1)
            {
                bail!("UNMERGED: 分支含有尚未合并到 HEAD 的提交");
            }
            let result = run(
                root,
                &[
                    "branch",
                    if op == "force-delete" { "-D" } else { "-d" },
                    "--",
                    local_name()?,
                ],
            );
            if let Err(e) = &result
                && op == "delete"
                && e.to_string().contains("not fully merged")
            {
                bail!("UNMERGED: {e}");
            }
            result
        }
        "track" => {
            ensure!(
                name.starts_with("refs/remotes/") && has_ref(root, argument(name)?)?,
                "请选择远端跟踪分支"
            );
            run(
                root,
                &[
                    "branch",
                    &format!("--set-upstream-to={name}"),
                    "--",
                    local_name()?,
                ],
            )
        }
        "update" | "push" => {
            let branch = local_name()?;
            let tracking = line(
                root,
                &[
                    "for-each-ref",
                    "--format=%(upstream:remotename)%00%(upstream:remoteref)",
                    "--",
                    target,
                ],
            )?;
            let (remote, dest) = tracking.split_once('\0').unwrap_or_default();
            if remote.is_empty() {
                ensure!(
                    op == "push" && run(root, &["remote"])?.lines().any(|r| r == name),
                    "请选择已配置的远端"
                );
                return run(
                    root,
                    &[
                        "push",
                        "--set-upstream",
                        "--",
                        argument(name)?,
                        &format!("{target}:{target}"),
                    ],
                );
            }
            argument(remote)?;
            ensure!(dest.starts_with("refs/heads/"), "上游不是分支");
            if op == "push" {
                run(root, &["push", "--", remote, &format!("{target}:{dest}")])
            } else if current == branch {
                run(root, &["pull", "--ff-only", "--", remote, dest])
            } else {
                run(root, &["fetch", "--", remote, &format!("{dest}:{target}")])
            }
        }
        "compare" => {
            let a = oid(root, if name.is_empty() { "HEAD" } else { name })?;
            let b = oid(root, target)?;
            let log = |a: &str, b: &str| {
                run(
                    root,
                    &["log", "-200", "--oneline", &format!("{a}..{b}"), "--"],
                )
            };
            Ok(format!(
                "{target} 独有\n{}\n{} 独有\n{}",
                log(&a, &b)?,
                if name.is_empty() { "HEAD" } else { name },
                log(&b, &a)?
            ))
        }
        "diff" => run(
            root,
            &[
                "diff",
                "--no-ext-diff",
                "--no-textconv",
                &oid(root, target)?,
                "--",
            ],
        ),
        "history" => run(
            root,
            &[
                "log",
                "-200",
                "--oneline",
                "--decorate",
                &oid(root, target)?,
                "--",
            ],
        ),
        "favorite" | "unfavorite" => {
            ensure!(has_ref(root, target)?, "分支已不存在");
            let remote = target.strip_prefix("refs/remotes/");
            let key = favorite_key(remote.is_some(), local.or(remote).context("请选择分支")?);
            if op == "favorite" {
                run(root, &["config", "--local", "--replace-all", &key, "true"])
            } else {
                run(root, &["config", "--local", "--unset-all", &key])
            }
        }
        _ => bail!("未知分支操作 {op}"),
    }
}
