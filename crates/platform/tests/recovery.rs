use anyhow::Result;
use std::{
    fs,
    io::{Read, Write},
    path::Path,
    process::Command,
    time::Duration,
};
use vantry_platform::{
    git::Repository,
    hostfs, plugins,
    projects::{Settings, Store},
    workspace::{self, WorkspaceSpec},
};
fn temp() -> tempfile::TempDir {
    tempfile::Builder::new()
        .prefix("vantry-p4-")
        .tempdir()
        .unwrap()
}

#[test]
fn recover_old_intent_after_checkout_creation() {
    let t = temp();
    let data = t.path().join("data");
    let root = t.path().join("repo");
    let store = Store::open(&data).unwrap();
    store
        .register(&root, Some("project"), Settings::default(), true)
        .unwrap();
    let request = WorkspaceSpec {
        repository: root.to_string_lossy().into(),
        ..Default::default()
    };
    let spec = workspace::prepare(&data, "owner", request.clone()).unwrap();
    let owner = data.join("workspaces/owner");
    fs::remove_file(owner.join("prepared.json")).unwrap();
    assert!(workspace::existing(&data, "owner", request.clone()).is_err());
    fs::write(Path::new(&spec.path).join("pending"), "existing work").unwrap();
    workspace::prepare(&data, "owner", request).unwrap();
    assert_eq!(
        fs::read_to_string(Path::new(&spec.path).join("pending")).unwrap(),
        "existing work"
    );
    fs::remove_file(owner.join("prepared.json")).unwrap();
    assert!(workspace::cleanup(&data, "owner", false).is_err());
    workspace::cleanup(&data, "owner", true).unwrap();
    assert!(!owner.exists());
    assert_eq!(
        Repository::discover(&root)
            .unwrap()
            .worktrees()
            .unwrap()
            .len(),
        1
    );
}
#[test]
fn conflicting_go_ownership_records_preserve_worktree() -> Result<()> {
    let t = temp();
    let data = t.path().join("data");
    let root = t.path().join("source");
    Store::open(&data)?.register(&root, None, Settings::default(), true)?;
    let spec = workspace::prepare(
        &data,
        "one",
        WorkspaceSpec {
            repository: root.to_string_lossy().into(),
            ..Default::default()
        },
    )?;
    let record = data.join("workspaces/one/prepared.json");
    let mut doc: serde_json::Value = serde_json::from_slice(&fs::read(&record)?)?;
    doc["path"] = root.to_string_lossy().into_owned().into();
    hostfs::atomic_write(&record, &serde_json::to_vec(&doc)?)?;
    assert!(workspace::cleanup(&data, "one", true).is_err());
    assert!(Path::new(&spec.path).is_dir());
    assert!(root.join(".git").is_dir());
    Ok(())
}
#[test]
fn unborn_repository_unstage_discard_and_untracked_diff() -> Result<()> {
    let t = temp();
    let root = t.path().join("repo");
    fs::create_dir(&root)?;
    assert!(
        Command::new("git")
            .current_dir(&root)
            .args(["init", "-q", "-b", "main"])
            .status()?
            .success()
    );
    let repo = Repository::discover(&root)?;
    fs::write(root.join("[literal]"), "line without newline")?;
    repo.stage(&["[literal]"])?;
    assert!(repo.diff(true, &[])?.contains("+line without newline"));
    repo.unstage(&["[literal]"])?;
    assert!(root.join("[literal]").exists());
    let diff = repo.diff(false, &["[literal]"])?;
    assert!(diff.contains("@@ -0,0 +1,1 @@\n+line without newline\n\\ No newline"));
    repo.stage(&["[literal]"])?;
    repo.discard(&["[literal]"])?;
    assert!(!root.join("[literal]").exists());
    assert!(repo.status()?.is_empty());
    Ok(())
}
#[test]
fn concurrent_project_adoption_has_one_registration() -> Result<()> {
    let t = temp();
    let data = t.path().join("data");
    let path = t.path().join("project");
    fs::create_dir(&path)?;
    std::thread::scope(|s| {
        let mut workers = Vec::new();
        for _ in 0..8 {
            workers.push(
                s.spawn(|| Store::open(&data)?.register(&path, None, Default::default(), false)),
            );
        }
        let ids: std::collections::HashSet<_> = workers
            .into_iter()
            .map(|w| w.join().unwrap().unwrap().id)
            .collect();
        assert_eq!(ids.len(), 1);
    });
    let store = Store::open(&data)?;
    assert_eq!(store.list()?.len(), 1);
    Ok(())
}
#[test]
fn chat_query_and_remote_spec_have_no_checkout_fallback() -> Result<()> {
    let t = temp();
    let data = t.path().join("data");
    assert!(workspace::chat_directory(&data, "chat", false).is_err());
    assert!(!data.exists());
    let dir = workspace::chat_directory(&data, "chat", true)?;
    assert_eq!(dir, workspace::chat_directory(&data, "chat", false)?);
    let spec = WorkspaceSpec {
        environment_id: "other".into(),
        path: dir.to_string_lossy().into(),
        kind: "directory".into(),
        ..Default::default()
    };
    assert!(workspace::existing(&data, "chat", spec).is_err());
    Ok(())
}
#[cfg(target_os = "macos")]
#[test]
fn packaged_helper_discovery_launch_and_reap() -> Result<()> {
    use std::os::unix::fs::{PermissionsExt, symlink};
    let t = temp();
    let dir = t.path().join("remote-desktop");
    fs::create_dir(&dir)?;
    let path = dir.join("vantry-remote-helper");
    symlink("/bin/sh", &path)?;
    assert!(plugins::native_helper(t.path()).is_err());
    fs::remove_file(&path)?;
    fs::write(
        &path,
        "#!/bin/sh\nread -r value\nprintf '%s %s' \"$1\" \"$value\"\n",
    )?;
    fs::set_permissions(&path, fs::Permissions::from_mode(0o700))?;
    let mut helper = plugins::launch_helper(t.path(), &["reply".into()])?;
    helper.take_stdin().unwrap().write_all(b"hello\n")?;
    let mut out = helper.take_stdout().unwrap();
    assert!(helper.wait(Duration::from_secs(1))?.success());
    let mut text = String::new();
    out.read_to_string(&mut text)?;
    assert_eq!(text, "reply hello");
    Ok(())
}
#[cfg(unix)]
#[test]
fn pty_close_cleans_background_job_control_groups() -> Result<()> {
    let t = temp();
    let mut cmd = Command::new("/bin/sh");
    cmd.arg("-i")
        .current_dir(t.path())
        .env("HOME", t.path())
        .env("ENV", "/dev/null")
        .env("PS1", "");
    let mut session = vantry_platform::pty::Session::spawn(cmd, 24, 80)?;
    session.write_all(b"set -m; trap '' HUP TERM; sleep 8 & echo $! > pid\n")?;
    let start = std::time::Instant::now();
    let pid = loop {
        if let Ok(data) = fs::read_to_string(t.path().join("pid"))
            && let Ok(pid) = data.trim().parse::<i32>()
            && unsafe { libc::getpgid(pid) } == pid
        {
            break pid;
        }
        anyhow::ensure!(
            start.elapsed() < Duration::from_secs(2),
            "shell did not establish a background job group"
        );
        std::thread::sleep(Duration::from_millis(5));
    };
    assert_ne!(unsafe { libc::getpgid(pid) }, session.pid() as i32);
    let retained_reader = session.reader()?;
    session.close()?;
    drop(retained_reader);
    for _ in 0..100 {
        if unsafe { libc::kill(pid, 0) } < 0 {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    panic!("terminal job {pid} survived close");
}
