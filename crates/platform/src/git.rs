//! Git CLI adapter. File arguments always use literal pathspecs in the selected checkout.
use crate::{hostfs, hostprocess};
use anyhow::{Context, Result, bail, ensure};
use std::{
    collections::HashSet,
    fs::{self, File},
    path::{Path, PathBuf},
    process::Command,
    time::Duration,
};
pub use vantry_protocol::GitFile;

pub(crate) fn execute(root: &Path, args: &[&str]) -> Result<hostprocess::Output> {
    let mut command = Command::new("git");
    hostprocess::clean_environment(&mut command);
    command
        .current_dir(root)
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GIT_MERGE_AUTOEDIT", "no")
        .arg("--literal-pathspecs")
        .args(args);
    Ok(hostprocess::capture(
        &mut command,
        &[],
        Duration::from_secs(30),
        16 << 20,
    )?)
}
pub(crate) fn run(root: &Path, args: &[&str]) -> Result<String> {
    let result = execute(root, args)?;
    ensure!(
        result.status.success(),
        "git {} failed ({}): {}",
        args.first().unwrap_or(&""),
        result.status,
        String::from_utf8_lossy(&result.stderr).trim()
    );
    Ok(String::from_utf8(result.stdout)?)
}
pub(crate) fn line(root: &Path, args: &[&str]) -> Result<String> {
    let mut text = run(root, args)?;
    if text.ends_with('\n') {
        text.pop();
        if cfg!(windows) && text.ends_with('\r') {
            text.pop();
        }
    }
    Ok(text)
}
pub(crate) fn has_ref(root: &Path, reference: &str) -> Result<bool> {
    let result = execute(
        root,
        &[
            "rev-parse",
            "--verify",
            "--quiet",
            "--end-of-options",
            reference,
        ],
    )?;
    match result.status.code() {
        Some(0) => Ok(true),
        Some(1) => Ok(false),
        _ => bail!(
            "cannot resolve reference: {}",
            String::from_utf8_lossy(&result.stderr)
        ),
    }
}
pub(crate) fn path_str(p: &Path) -> Result<&str> {
    p.to_str().context("non-UTF8 path")
}
#[derive(Debug, Clone)]
pub struct Repository {
    root: PathBuf,
}
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Worktree {
    pub path: PathBuf,
    pub branch: Option<String>,
    pub locked: bool,
    pub detached: bool,
}
impl Repository {
    pub fn discover(path: &Path) -> Result<Self> {
        let root = PathBuf::from(line(path, &["rev-parse", "--show-toplevel"])?).canonicalize()?;
        Ok(Self { root })
    }
    pub fn root(&self) -> &Path {
        &self.root
    }
    pub fn common_dir(&self) -> Result<PathBuf> {
        Ok(PathBuf::from(line(
            &self.root,
            &["rev-parse", "--path-format=absolute", "--git-common-dir"],
        )?)
        .canonicalize()?)
    }
    pub fn branch(&self) -> Result<String> {
        line(&self.root, &["symbolic-ref", "--quiet", "--short", "HEAD"])
    }
    pub fn branches(&self) -> Result<Vec<String>> {
        Ok(run(
            &self.root,
            &[
                "for-each-ref",
                "--format=%(refname:short)",
                "refs/heads",
                "refs/remotes",
            ],
        )?
        .lines()
        .map(str::to_owned)
        .collect())
    }
    fn lock(&self) -> Result<File> {
        let lock = hostfs::lock_dir(&self.root, false)?;
        let dir = PathBuf::from(line(&self.root, &["rev-parse", "--absolute-git-dir"])?);
        for marker in [
            "MERGE_HEAD",
            "CHERRY_PICK_HEAD",
            "REVERT_HEAD",
            "rebase-merge",
            "rebase-apply",
            "sequencer",
        ] {
            ensure!(!dir.join(marker).try_exists()?, "Git operation in progress");
        }
        Ok(lock)
    }
    pub fn switch(&self, branch: &str, create: bool) -> Result<()> {
        let _guard = self.lock()?;
        self.validate_branch(branch)?;
        let mut args = vec!["switch", "--no-overwrite-ignore", "--no-guess"];
        if create {
            args.extend(["-c", branch]);
        } else if !has_ref(&self.root, &format!("refs/heads/{branch}"))?
            && has_ref(&self.root, &format!("refs/remotes/{branch}"))?
        {
            let (_, local) = branch.split_once('/').context("invalid remote branch")?;
            self.validate_branch(local)?;
            if has_ref(&self.root, &format!("refs/heads/{local}"))? {
                args.extend(["--", local]);
            } else {
                args.extend(["--track", "-c", local, "--", branch]);
            }
        } else {
            args.extend(["--", branch]);
        }
        run(&self.root, &args)?;
        Ok(())
    }
    pub(crate) fn validate_branch(&self, branch: &str) -> Result<()> {
        ensure!(
            !branch.starts_with('-')
                && line(&self.root, &["check-ref-format", "--branch", branch])? == branch,
            "invalid branch"
        );
        Ok(())
    }
    pub fn worktrees(&self) -> Result<Vec<Worktree>> {
        let data = run(&self.root, &["worktree", "list", "--porcelain", "-z"])?;
        let mut items = Vec::<Worktree>::new();
        for field in data.split('\0') {
            if let Some(path) = field.strip_prefix("worktree ") {
                items.push(Worktree {
                    path: path.into(),
                    ..Default::default()
                });
            } else if let Some(item) = items.last_mut() {
                if let Some(branch) = field.strip_prefix("branch refs/heads/") {
                    item.branch = Some(branch.into());
                } else if field == "locked" || field.starts_with("locked ") {
                    item.locked = true;
                } else if field == "detached" {
                    item.detached = true;
                }
            }
        }
        Ok(items)
    }
    pub fn status(&self) -> Result<Vec<GitFile>> {
        let data = run(
            &self.root,
            &["status", "--porcelain=v1", "-z", "--untracked-files=all"],
        )?;
        let mut fields = data.split_terminator('\0');
        let mut items = Vec::new();
        while let Some(s) = fields.next() {
            ensure!(s.as_bytes().get(2) == Some(&b' '), "invalid git status");
            let original_path = if s[..2].contains(['R', 'C']) {
                fields.next().context("missing rename source")?.into()
            } else {
                String::new()
            };
            items.push(GitFile {
                path: s[3..].into(),
                original_path,
                index: s[..1].into(),
                worktree: s[1..2].into(),
            });
        }
        Ok(items)
    }
    fn paths(&self, paths: &[&str]) -> Result<()> {
        ensure!(!paths.is_empty(), "select at least one path");
        for s in paths {
            let p = hostfs::relative(Path::new(s))?;
            let mut current = self.root.clone();
            for part in p.components() {
                ensure!(
                    !part.as_os_str().eq_ignore_ascii_case(".git"),
                    "repository metadata is protected"
                );
                current.push(part);
                match fs::symlink_metadata(&current) {
                    Ok(meta) => ensure!(!meta.is_symlink(), "symlink selection is not permitted"),
                    Err(e) if e.kind() == std::io::ErrorKind::NotFound => break,
                    Err(e) => return Err(e.into()),
                }
            }
        }
        Ok(())
    }
    fn selected<'a>(&self, mut args: Vec<&'a str>, paths: &[&'a str]) -> Result<String> {
        args.push("--");
        args.extend_from_slice(paths);
        run(&self.root, &args)
    }
    pub fn diff(&self, staged: bool, paths: &[&str]) -> Result<String> {
        if !paths.is_empty() {
            self.paths(paths)?;
        }
        let mut args = vec![
            "diff",
            "--no-color",
            "--no-ext-diff",
            "--no-textconv",
            "--unified=3",
        ];
        if staged {
            args.push("--cached");
        }
        let mut diff = self.selected(args, paths)?;
        if !staged {
            let untracked = self.selected(
                vec!["ls-files", "--others", "--exclude-standard", "-z"],
                paths,
            )?;
            if !untracked.is_empty() {
                diff.push_str("\n[未跟踪 / untracked]\n");
            }
            for name in untracked.split_terminator('\0') {
                if diff.len() >= 1 << 20 {
                    break;
                }
                self.append_untracked(name, &mut diff)?;
            }
        }
        const TRUNCATED: &str = "[输出已截断：仅显示前 1 MiB / 2000 行；请在终端查看完整输出。]\n";
        let byte_end = diff.floor_char_boundary(diff.len().min(1 << 20));
        let line_end = diff[..byte_end]
            .match_indices('\n')
            .nth(1999)
            .map(|(i, _)| i + 1)
            .unwrap_or(byte_end);
        if line_end < diff.len() {
            diff.truncate(line_end);
            diff.push_str(TRUNCATED);
        }
        Ok(diff)
    }
    fn append_untracked(&self, name: &str, diff: &mut String) -> Result<()> {
        use std::{fmt::Write, io::Read};
        let file = match hostfs::open_file(&self.root, Path::new(name)) {
            Ok(f) => f,
            Err(_) => return Ok(()),
        };
        #[cfg(unix)]
        let executable = {
            use std::os::unix::fs::PermissionsExt;
            file.metadata()?.permissions().mode() & 0o111 != 0
        };
        #[cfg(windows)]
        let executable = false;
        let mut bytes = Vec::new();
        const LIMIT: usize = 64 << 10;
        file.take((LIMIT + 1) as u64).read_to_end(&mut bytes)?;
        let partial = bytes.len() > LIMIT;
        bytes.truncate(LIMIT);
        if bytes.contains(&0) {
            return Ok(());
        }
        let text = match std::str::from_utf8(&bytes) {
            Ok(s) => s,
            Err(e) if partial && e.error_len().is_none() => {
                std::str::from_utf8(&bytes[..e.valid_up_to()])?
            }
            Err(_) => return Ok(()),
        };
        let quote = |s: String| {
            if s.contains(['\t', '\r', '\n', '"', '\\']) {
                format!("{s:?}")
            } else {
                s
            }
        };
        let old = quote(format!("a/{name}"));
        let new = quote(format!("b/{name}"));
        let mode = if executable { "100755" } else { "100644" };
        writeln!(
            diff,
            "diff --git {old} {new}\nnew file mode {mode}\n--- /dev/null\n+++ {new}"
        )?;
        if !text.is_empty() {
            writeln!(
                diff,
                "@@ -0,0 +1,{} @@",
                text.split_terminator('\n').count()
            )?;
            for line in text.split_terminator('\n') {
                writeln!(diff, "+{line}")?;
            }
            if !partial && !text.ends_with('\n') {
                diff.push_str("\\ No newline at end of file\n");
            }
        }
        if partial {
            diff.push_str("[未跟踪文件预览已截断：每个文件最多显示前 64 KiB。]\n");
        }
        Ok(())
    }
    pub fn stage(&self, paths: &[&str]) -> Result<()> {
        let _guard = self.lock()?;
        self.paths(paths)?;
        self.selected(vec!["add", "-A"], paths)?;
        Ok(())
    }
    pub fn unstage(&self, paths: &[&str]) -> Result<()> {
        let _guard = self.lock()?;
        self.paths(paths)?;
        let args = if has_ref(&self.root, "HEAD")? {
            vec!["reset", "-q", "HEAD"]
        } else {
            vec!["rm", "--cached", "-rf", "--ignore-unmatch"]
        };
        self.selected(args, paths)?;
        Ok(())
    }
    pub fn discard(&self, paths: &[&str]) -> Result<()> {
        let _guard = self.lock()?;
        self.paths(paths)?;
        let indexed = self.selected(vec!["ls-files", "-z"], paths)?;
        let head = if has_ref(&self.root, "HEAD")? {
            self.selected(vec!["ls-tree", "-r", "--name-only", "-z", "HEAD"], paths)?
        } else {
            String::new()
        };
        let tracked: Vec<_> = head.split_terminator('\0').collect();
        let known: HashSet<_> = tracked.iter().copied().collect();
        let added: Vec<_> = indexed
            .split_terminator('\0')
            .filter(|p| !known.contains(p))
            .collect();
        for list in [&tracked, &added] {
            if !list.is_empty() {
                self.paths(list)?;
            }
        }
        if !tracked.is_empty() {
            self.selected(vec!["checkout", "HEAD"], &tracked)?;
        }
        if !added.is_empty() {
            self.selected(vec!["rm", "-f"], &added)?;
        }
        self.selected(vec!["clean", "-fd"], paths)?;
        Ok(())
    }
    pub fn commit(&self, message: &str) -> Result<String> {
        ensure!(!message.trim().is_empty(), "empty commit message");
        let _guard = self.lock()?;
        run(&self.root, &["commit", "-m", message])?;
        line(&self.root, &["rev-parse", "HEAD"])
    }
}
