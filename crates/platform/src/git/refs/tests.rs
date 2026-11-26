use super::*;
use std::{fs, path::PathBuf};
use tempfile::TempDir;

struct Fixture {
    _dir: TempDir,
    root: PathBuf,
    repo: Repository,
}
impl Fixture {
    fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("checkout");
        fs::create_dir(&root).unwrap();
        run(&root, &["init", "-b", "main"]).unwrap();
        for (key, value) in [
            ("user.email", "test@example.invalid"),
            ("user.name", "Tester"),
            ("commit.gpgSign", "false"),
        ] {
            run(&root, &["config", key, value]).unwrap();
        }
        run(&root, &["commit", "--allow-empty", "-m", "base"]).unwrap();
        let repo = Repository::discover(&root).unwrap();
        Self {
            _dir: dir,
            root,
            repo,
        }
    }
    fn call(&self, op: &str, branch: &str, name: &str) -> Result<String> {
        action(
            &self.repo,
            &WorkbenchRequest {
                action: format!("git.refs.{op}"),
                branch: branch.into(),
                name: name.into(),
                ..Default::default()
            },
        )
    }
    fn git(&self, args: &[&str]) -> String {
        line(&self.root, args).unwrap()
    }
    fn commit(&self, message: &str) {
        self.git(&["commit", "--allow-empty", "-m", message]);
    }
    fn list(&self) -> Catalog {
        serde_json::from_str(&self.call("list", "", "").unwrap()).unwrap()
    }
}

#[test]
fn branches_tags_recent_diff_and_two_sided_compare() {
    let f = Fixture::new();
    fs::write(f.root.join("file"), "base\n").unwrap();
    f.git(&["add", "file"]);
    f.commit("file");
    f.git(&["tag", "v1"]);
    f.call("create", "refs/heads/main", "topic/ui").unwrap();
    f.commit("topic only");
    f.call("checkout", "refs/heads/main", "").unwrap();
    f.commit("main only");
    let list = f.list();
    assert_eq!(list.recent, ["main", "topic/ui"]);
    assert_eq!(list.tags, ["v1"]);
    let log = f.call("compare", "refs/heads/topic/ui", "").unwrap();
    let (left, right) = log.split_once("HEAD 独有").unwrap();
    assert!(left.contains("topic only") && !left.contains("main only"));
    assert!(right.contains("main only") && !right.contains("topic only"));
    fs::write(f.root.join("file"), "changed\n").unwrap();
    assert!(
        f.call("diff", "refs/tags/v1", "")
            .unwrap()
            .contains("+changed")
    );
    assert!(
        f.call("history", "refs/heads/topic/ui", "")
            .unwrap()
            .contains("topic only")
    );
    assert!(f.call("checkout", "v1~1", "").is_err());
    assert_eq!(
        fs::read_to_string(f.root.join("file")).unwrap(),
        "changed\n"
    );
    f.git(&["restore", "--", "file"]);
    for rev in ["refs/tags/v1", "HEAD~1"] {
        f.call("checkout", rev, "").unwrap();
        assert!(f.list().current.is_empty());
    }
    f.call("checkout", "refs/heads/main", "").unwrap();
    f.call("favorite", "refs/heads/main", "").unwrap();
    assert!(
        f.list()
            .branches
            .iter()
            .any(|b| b.name == "main" && b.favorite)
    );
    f.call("unfavorite", "refs/heads/main", "").unwrap();
    assert!(f.list().branches.iter().all(|b| !b.favorite));
}
#[test]
fn worktree_merge_rebase_rename_and_delete() {
    let f = Fixture::new();
    let main = "refs/heads/main";
    let worktree = f.call("worktree", main, "sandbox").unwrap();
    assert_eq!(
        line(Path::new(&worktree), &["branch", "--show-current"]).unwrap(),
        "sandbox"
    );
    assert!(f.call("delete", "refs/heads/sandbox", "").is_err());
    f.call("create", main, "feature").unwrap();
    f.commit("feature");
    f.call("checkout", main, "").unwrap();
    f.commit("on main");
    let branch = "refs/heads/feature";
    assert!(
        f.call("delete", branch, "")
            .unwrap_err()
            .to_string()
            .contains("UNMERGED")
    );
    f.call("checkout-rebase", branch, "").unwrap();
    assert_eq!(f.list().current, "feature");
    f.call("rebase", main, "").unwrap();
    f.call("checkout", main, "").unwrap();
    f.call("merge", branch, "").unwrap();
    f.call("rename", branch, "landed").unwrap();
    f.call("delete", "refs/heads/landed", "").unwrap();
    f.call("create", main, "discard-me").unwrap();
    f.commit("unmerged");
    f.call("checkout", main, "").unwrap();
    f.call("force-delete", "refs/heads/discard-me", "").unwrap();
}
#[test]
fn push_track_and_update_respect_configured_remote_and_refspec() {
    let f = Fixture::new();
    let main = "refs/heads/main";
    let bare = f._dir.path().join("bare");
    f.git(&["init", "--bare", bare.to_str().unwrap()]);
    f.git(&["remote", "add", "team", bare.to_str().unwrap()]);
    f.call("push", main, "team").unwrap();
    assert_eq!(
        f.list()
            .branches
            .iter()
            .find(|b| !b.remote)
            .unwrap()
            .upstream,
        "team/main"
    );
    f.call("create", main, "topic").unwrap();
    f.call("track", "refs/heads/topic", "refs/remotes/team/main")
        .unwrap();
    f.commit("new remote commit");
    assert_eq!(
        f.list()
            .branches
            .iter()
            .find(|b| b.name == "topic")
            .unwrap()
            .ahead,
        1
    );
    f.call("push", "refs/heads/topic", "").unwrap();
    assert_eq!(
        f.list()
            .branches
            .iter()
            .find(|b| b.name == "main")
            .unwrap()
            .behind,
        1
    );
    f.call("update", main, "").unwrap();
    assert_eq!(
        f.git(&["rev-parse", "main"]),
        f.git(&["rev-parse", "topic"])
    );
    f.call("checkout", main, "").unwrap();
    f.call("update", main, "").unwrap();
    f.git(&["push", "team", "main:remoteonly"]);
    f.call("checkout", "refs/remotes/team/remoteonly", "")
        .unwrap();
    assert_eq!(f.list().current, "remoteonly");
}
#[test]
fn rejects_options_paths_and_stale_context() {
    let f = Fixture::new();
    for op in [
        "checkout", "rebase", "merge", "delete", "compare", "diff", "history", "worktree",
        "create", "push", "track",
    ] {
        assert!(f.call(op, "--help", "ok").is_err(), "{op}");
    }
    for (op, name) in [
        ("create", "-x"),
        ("rename", "--help"),
        ("worktree", "../outside"),
        ("push", "file:///outside"),
        ("track", "--help"),
    ] {
        assert!(f.call(op, "refs/heads/main", name).is_err(), "{op}");
    }
    assert!(
        action(
            &f.repo,
            &WorkbenchRequest {
                action: "git.refs.rename".into(),
                branch: "refs/heads/main".into(),
                name: "other".into(),
                input: Some(serde_json::json!({"current":"old"})),
                ..Default::default()
            }
        )
        .is_err()
    );
    assert_eq!(f.list().current, "main");
}
