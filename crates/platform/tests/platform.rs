use anyhow::Result;
use serde_json::json;
use std::{
    fs,
    io::{Read, Write},
    path::{Path, PathBuf},
    time::Duration,
};
use vantry_platform::{
    environments::{Catalog, Entry, Kind},
    git::Repository,
    hostfs,
    hostplatform::{Capabilities, Paths},
    plugins::Host,
    projects::{Settings, Store},
    workspace::{self, WorkspaceSpec},
};

fn temp() -> tempfile::TempDir {
    tempfile::Builder::new()
        .prefix("vantry-p4-")
        .tempdir()
        .unwrap()
}
fn git(root: &Path, args: &[&str]) -> String {
    let mut cmd = std::process::Command::new("git");
    vantry_platform::hostprocess::clean_environment(&mut cmd);
    let out = cmd.current_dir(root).args(args).output().unwrap();
    assert!(
        out.status.success(),
        "{}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8(out.stdout).unwrap().trim_end().into()
}
fn repository(root: &Path) -> Result<Repository> {
    fs::create_dir(root)?;
    git(root, &["init", "-b", "main"]);
    git(root, &["config", "core.autocrlf", "false"]);
    git(root, &["config", "user.name", "P4 Fixture"]);
    git(root, &["config", "user.email", "test@localhost"]);
    git(root, &["config", "commit.gpgsign", "false"]);
    fs::write(root.join("tracked"), "base\n")?;
    git(root, &["add", "tracked"]);
    git(root, &["commit", "-m", "base"]);
    Repository::discover(root)
}
#[test]
fn paths_use_go_default_and_overrides() -> Result<()> {
    let t = temp();
    let p = Paths::from_env(|k| match k {
        "HOME" | "USERPROFILE" => Some(t.path().into()),
        _ => None,
    })?;
    assert!(p.data.ends_with("Vantry"));
    assert_eq!(p.socket, p.data.join("core.sock"));
    let p = Paths::from_env(|k| Some(t.path().join(k).into()))?;
    assert_eq!(p.data, t.path().join("VANTRY_DATA_DIR"));
    assert_eq!(p.config, t.path().join("VANTRY_CONFIG_DIR"));
    assert_eq!(p.cache, t.path().join("VANTRY_CACHE_DIR"));
    assert_eq!(p.runtime, t.path().join("VANTRY_RUNTIME_DIR"));
    assert_eq!(p.socket, t.path().join("VANTRY_SOCKET_PATH"));
    let c = Capabilities::probe();
    assert!(c.git);
    assert_eq!(c.pty, cfg!(unix));
    Ok(())
}
#[test]
fn private_atomic_files_are_consistent_and_bounded() -> Result<()> {
    let t = temp();
    let root = t.path().join("nested/data");
    hostfs::ensure_private_dir(&root)?;
    let path = root.join("state");
    hostfs::atomic_write(&path, &[0; 1024])?;
    std::thread::scope(|s| {
        for i in 1..=4 {
            let path = &path;
            s.spawn(move || {
                for _ in 0..8 {
                    hostfs::atomic_write(path, &[i; 1024]).unwrap();
                }
            });
        }
        for _ in 0..32 {
            let b = hostfs::read_file(&root, "state".as_ref(), 1024).unwrap();
            assert_eq!(b.len(), 1024);
            assert!(b.iter().all(|v| *v == b[0]));
        }
    });
    assert_eq!(fs::read_dir(&root)?.count(), 1);
    assert!(hostfs::read_file(&root, "state".as_ref(), 100).is_err());
    assert!(hostfs::read_file(&root, "../state".as_ref(), 1024).is_err());
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(fs::metadata(&root)?.permissions().mode() & 0o777, 0o700);
        assert_eq!(fs::metadata(&path)?.permissions().mode() & 0o777, 0o600);
    }
    Ok(())
}
#[cfg(unix)]
#[test]
fn private_io_refuses_links_shared_roots_and_special_files() -> Result<()> {
    use std::os::unix::fs::{PermissionsExt, symlink};
    let t = temp();
    let root = t.path().join("data");
    hostfs::ensure_private_dir(&root)?;
    let other = t.path().join("external");
    fs::create_dir(&other)?;
    fs::write(other.join("file"), "original")?;
    symlink(&other, root.join("link"))?;
    symlink(other.join("file"), root.join("leaf"))?;
    assert!(hostfs::atomic_write(&root.join("leaf"), b"change").is_err());
    assert!(hostfs::read_file(&root, "link/file".as_ref(), 10).is_err());
    assert!(hostfs::atomic_write(&root.join("link/nested/file"), b"x").is_err());
    fs::set_permissions(&root, fs::Permissions::from_mode(0o755))?;
    assert!(hostfs::check_private_dir(&root).is_err());
    assert_eq!(fs::read_to_string(other.join("file"))?, "original");
    Ok(())
}
#[test]
fn git_staging_diff_discard_commit_and_branches() -> Result<()> {
    let t = temp();
    let dir = t.path().join("repo");
    let repo = repository(&dir)?;
    fs::write(dir.join("tracked"), "staged\n")?;
    repo.stage(&["tracked"])?;
    fs::write(dir.join("tracked"), "unstaged\n")?;
    fs::write(dir.join("star[1]"), "new")?;
    let status = repo.status()?;
    assert!(
        status
            .iter()
            .any(|f| f.path == "tracked" && f.index == "M" && f.worktree == "M")
    );
    assert!(status.iter().any(|f| f.path == "star[1]" && f.index == "?"));
    assert!(repo.diff(true, &["tracked"])?.contains("+staged"));
    assert!(repo.diff(false, &[])?.contains("+unstaged"));
    repo.unstage(&["tracked"])?;
    assert!(repo.diff(true, &[])?.is_empty());
    repo.stage(&["star[1]"])?;
    assert!(repo.discard(&["tracked", "../oops"]).is_err());
    assert_eq!(fs::read_to_string(dir.join("tracked"))?, "unstaged\n");
    repo.discard(&["tracked", "star[1]"])?;
    assert!(repo.status()?.is_empty());
    repo.switch("feature", true)?;
    fs::write(dir.join("tracked"), "commit\n")?;
    repo.stage(&["tracked"])?;
    assert!(!repo.commit("feature change")?.is_empty());
    repo.switch("main", false)?;
    assert_eq!(fs::read_to_string(dir.join("tracked"))?, "base\n");
    assert!(repo.branches()?.contains(&"feature".into()));
    Ok(())
}
#[test]
fn worktree_lifecycle_honors_disk_intent_ownership_locks_and_ignored_files() -> Result<()> {
    let t = temp();
    let data = t.path().join("data");
    let repo = repository(&t.path().join("repo"))?;
    let request = WorkspaceSpec {
        repository: repo.root().to_str().unwrap().into(),
        ..Default::default()
    };
    assert!(workspace::existing(&data, "thread", request.clone()).is_err());
    assert!(!data.exists());
    let spec = workspace::prepare(&data, "thread", request.clone())?;
    let path = PathBuf::from(&spec.path);
    assert_eq!(repo.worktrees()?.len(), 2);
    fs::write(path.join("pending"), "keep")?;
    assert_eq!(workspace::prepare(&data, "thread", request.clone())?, spec);
    assert!(workspace::cleanup(&data, "thread", false).is_err());
    let linked = Repository::discover(&path)?;
    linked.switch("drift", true)?;
    assert!(workspace::cleanup(&data, "thread", true).is_err());
    assert!(workspace::existing(&data, "thread", request.clone()).is_err());
    linked.switch(&spec.branch, false)?;
    {
        let _lock = hostfs::lock_dir(&path, true)?;
        assert!(workspace::cleanup(&data, "thread", true).is_err());
    }
    fs::remove_file(path.join("pending"))?;
    fs::write(path.join(".gitignore"), "setup.env\n")?;
    fs::write(path.join("setup.env"), "ignored")?;
    linked.stage(&[".gitignore"])?;
    linked.commit("ignore setup")?;
    assert!(workspace::cleanup(&data, "thread", false).is_err());
    workspace::cleanup(&data, "thread", true)?;
    assert!(!path.exists());
    assert_eq!(repo.worktrees()?.len(), 1);
    assert!(repo.branches()?.contains(&spec.branch));
    assert!(workspace::prepare(&data, "../escape", request).is_err());
    Ok(())
}
#[test]
fn workspace_tree_read_bounds() -> Result<()> {
    let t = temp();
    fs::create_dir(t.path().join("d"))?;
    fs::create_dir(t.path().join(".git"))?;
    fs::write(t.path().join("d/file"), "text")?;
    let tree = workspace::list(t.path(), Path::new("."), 1)?;
    assert_eq!(tree.len(), 1);
    assert!(tree[0].directory);
    assert_eq!(workspace::read(t.path(), Path::new("d/file"), 4)?, b"text");
    assert!(workspace::read(t.path(), Path::new("d/file"), 3).is_err());
    assert!(workspace::list(t.path(), Path::new(".."), 5).is_err());
    Ok(())
}
#[test]
fn project_registry_adoption_reopen_and_recent() -> Result<()> {
    let t = temp();
    let data = t.path().join("data");
    let store = Store::open(&data)?;
    let root = t.path().join("project");
    let p = store.register(&root, Some("p1"), Default::default(), true)?;
    assert_eq!(p.default_base_branch, "main");
    assert_eq!(store.register(&root, None, Default::default(), false)?, p);
    let invalid = Settings {
        env: Some([("bad key".into(), "x".into())].into()),
        ..Default::default()
    };
    assert!(store.update("p1", invalid).is_err());
    store.update(
        "p1",
        Settings {
            dev_command: Some("test".into()),
            ..Default::default()
        },
    )?;
    let loaded = Store::open(&data)?;
    assert_eq!(loaded.list()?[0].dev_command, "test");
    assert_eq!(loaded.find_by_path(&root)?.id, "p1");
    store.remember(&root)?;
    store.remember(&root)?;
    assert_eq!(store.recent()?, vec![root.canonicalize()?]);
    let disk: serde_json::Value = serde_json::from_slice(&fs::read(data.join("projects.json"))?)?;
    assert_eq!(disk[0]["id"], "p1");
    assert!(disk[0]["createdAt"].is_string());
    store.delete("p1")?;
    assert!(root.join(".git").is_dir());
    Ok(())
}
#[test]
fn environment_v1_roundtrip_and_validation() -> Result<()> {
    let t = temp();
    let data = t.path().join("data");
    let catalog = Catalog::open(&data)?;
    hostfs::atomic_write(&data.join("environments.json"),br#"{"version":1,"environments":[{"id":"dev","name":"Dev","host":"user@dev","createdAt":"2020-01-01T01:02:03Z"}]}"#)?;
    let mut ssh = catalog.load()?.remove(0);
    assert_eq!(ssh.kind, Kind::Ssh);
    assert_eq!(
        ssh.resolve_socket("/home/dev")?,
        "/home/dev/.vantry/core.sock"
    );
    let created = ssh.created_at.take();
    ssh.name = "Dev 2".into();
    assert_eq!(catalog.upsert(ssh.clone())?.created_at, created);
    for host in ["-bad", "x y"] {
        ssh.host = host.into();
        assert!(catalog.upsert(ssh.clone()).is_err());
    }
    for kind in [Kind::Local, Kind::Remote] {
        catalog.upsert(Entry {
            kind,
            name: "Machine".into(),
            host: "test".into(),
            ..Default::default()
        })?;
    }
    assert_eq!(catalog.load()?.len(), 3);
    assert_eq!(catalog.load()?[2].id, "machine-2");
    catalog.remove("machine")?;
    assert!(catalog.save(vec![ssh.clone(), ssh]).is_err());
    hostfs::atomic_write(
        &data.join("environments.json"),
        br#"{"version":9,"environments":[]}"#,
    )?;
    assert!(catalog.load().is_err());
    Ok(())
}
fn zip(files: &[(&str, &[u8])]) -> Result<Vec<u8>> {
    let mut writer = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
    for (name, content) in files {
        writer.start_file(*name, zip::write::SimpleFileOptions::default())?;
        writer.write_all(content)?;
    }
    Ok(writer.finish()?.into_inner())
}
const PANEL:&[u8]=br#"{"id":"panel","name":"Example","runtime":"native","version":"1.0","navigation":[{"title":"Hello","page":"home"}],"pages":{"home":{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":"Hello"}]}}},"permissions":["workspace:read","process:tool","network:*.example.com"]}"#;
#[test]
fn plugin_install_enable_permissions_and_uninstall() -> Result<()> {
    let t = temp();
    let mut host = Host::open(&t.path().join("plugins"), t.path(), "0.1.42")?;
    assert!(host.list()?.is_empty());
    assert_eq!(
        host.install(&zip(&[("bundle/plugin.json", PANEL)])?)?.id,
        "panel"
    );
    assert!(
        host.invoke("panel", t.path(), "", "", json!({}))?
            .contains("Hello")
    );
    let permissions = host.manifest("panel")?.grants()?;
    assert!(permissions.allows_host("api.example.com"));
    assert!(!permissions.allows_host("badexample.com"));
    assert!(host.authorize_process("panel", "tool").is_err());
    host.set_process_grant("panel", true)?;
    host.authorize_process("panel", "tool")?;
    assert!(host.authorize_process("panel", "other").is_err());
    fs::write(t.path().join("file"), "ok")?;
    assert_eq!(
        host.workspace_read("panel", t.path(), "file".as_ref())?,
        b"ok"
    );
    host.set_enabled("panel", false)?;
    assert!(host.invoke("panel", t.path(), "", "", json!({})).is_err());
    host.set_enabled("panel", true)?;
    host.uninstall("panel")?;
    assert!(host.list()?.is_empty());
    assert!(!t.path().join(".plugin-data/panel").exists());
    Ok(())
}
#[test]
fn archives_reject_traversal_links_and_invalid_replacements() -> Result<()> {
    let t = temp();
    let plugins = t.path().join("plugins");
    let mut host = Host::open(&plugins, t.path(), "0.1.42")?;
    let mut tar = tar::Builder::new(flate2::write::GzEncoder::new(
        Vec::new(),
        flate2::Compression::default(),
    ));
    let mut hdr = tar::Header::new_gnu();
    hdr.set_size(PANEL.len() as u64);
    hdr.set_mode(0o600);
    hdr.set_cksum();
    tar.append_data(&mut hdr, "plugin.json", PANEL)?;
    let gz = tar.into_inner()?.finish()?;
    host.install(&gz)?;
    for name in [
        "../outside",
        "nested/../../outside",
        "C:/escape",
        "a\\b",
        "NUL.txt",
    ] {
        assert!(
            host.install(&zip(&[("plugin.json", PANEL), (name, b"x")])?)
                .is_err(),
            "{name}"
        );
    }
    let mut tar = tar::Builder::new(flate2::write::GzEncoder::new(
        Vec::new(),
        flate2::Compression::fast(),
    ));
    let mut hdr = tar::Header::new_gnu();
    hdr.set_size(0);
    hdr.set_mode(0o777);
    hdr.set_entry_type(tar::EntryType::Symlink);
    tar.append_link(&mut hdr, "link", "../outside")?;
    assert!(host.install(&tar.into_inner()?.finish()?).is_err());
    assert!(host.install(&zip(&[("plugin.json", b"invalid")])?).is_err());
    assert_eq!(fs::read_dir(&plugins)?.count(), 1);
    assert_eq!(host.manifest("panel")?.version, "1.0");
    Ok(())
}
#[cfg(unix)]
fn gone(pid: u32) {
    for _ in 0..200 {
        if unsafe { libc::kill(pid as i32, 0) } < 0
            && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH)
        {
            return;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
    panic!("child {pid} still exists");
}
#[cfg(unix)]
#[test]
fn real_process_plugin_v1_and_rpc_lifecycle() -> Result<()> {
    let t = temp();
    let mut host = Host::open(&t.path().join("plugins"), t.path(), "0.1.42")?;
    host.install(&zip(&[
        (
            "plugin.json",
            br#"{"id":"echo","name":"Echo","runtime":"process","command":["sh","echo.sh"]}"#,
        ),
        ("echo.sh", b"cat"),
    ])?)?;
    let v: serde_json::Value =
        serde_json::from_str(&host.invoke("echo", t.path(), "run", "", json!({"n":7}))?)?;
    assert_eq!(
        v,
        json!({"version":1,"pluginId":"echo","workspace":t.path(),"input":{"action":"run","n":7}})
    );
    let manifest=br#"{"id":"rpc","name":"RPC","runtime":"process","command":["sh","rpc.sh"],"timeoutMs":500}"#;
    let script=b"read -r req\nprintf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":7}' '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":42}'\nwhile read -r req; do :; done\n";
    host.install(&zip(&[("plugin.json", manifest), ("rpc.sh", script)])?)?;
    let pid = host.start_rpc("rpc")?;
    let mut events = Vec::new();
    assert_eq!(
        host.request("rpc", "compute", json!({}), |name, v| events
            .push((name.to_owned(), v.clone())))?,
        42
    );
    assert_eq!(events, vec![("progress".into(), json!(7))]);
    host.set_enabled("rpc", false)?;
    gone(pid);
    assert!(host.start_rpc("rpc").is_err());
    host.set_enabled("rpc", true)?;
    let pid = host.start_rpc("rpc")?;
    host.request("rpc", "compute", json!({}), |_, _| {})?;
    assert!(host.request("rpc", "hang", json!({}), |_, _| {}).is_err());
    gone(pid);
    Ok(())
}
#[cfg(unix)]
#[test]
fn timeout_and_group_cleanup_reap_resistant_grandchild() -> Result<()> {
    use vantry_platform::hostprocess::{Process, capture};
    let t = temp();
    let mut cmd = std::process::Command::new("sh");
    cmd.current_dir(t.path())
        .args(["-c", "trap '' TERM; sleep 60 & echo $! > child; wait"]);
    let mut proc = Process::spawn(&mut cmd)?;
    let pid = proc.id();
    let mut child = None;
    for _ in 0..200 {
        if let Ok(v) = fs::read_to_string(t.path().join("child"))
            && let Ok(p) = v.trim().parse::<u32>()
        {
            child = Some(p);
            break;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
    let child = child.expect("grandchild started");
    assert_eq!(
        proc.wait(Duration::from_millis(30)).unwrap_err().kind(),
        std::io::ErrorKind::TimedOut
    );
    gone(pid);
    gone(child);
    let mut cmd = std::process::Command::new("sh");
    cmd.args(["-c", "while :; do printf 1234567890; done"]);
    let error = capture(&mut cmd, &[], Duration::from_secs(2), 80).unwrap_err();
    assert_eq!(error.kind(), std::io::ErrorKind::InvalidData, "{error:?}");
    Ok(())
}
#[cfg(unix)]
#[test]
fn pty_controlling_terminal_resize_ctrl_c_io_and_reap() -> Result<()> {
    use std::os::fd::AsRawFd;
    use vantry_platform::pty::Manager;
    let t = temp();
    let mut manager = Manager::default();
    let mut command = std::process::Command::new("sh");
    command.current_dir(t.path()).args(["-c","trap 'echo INTERRUPTED; exit 7' INT; test -t 0 || exit 10; echo READY; read -r line; stty size; echo OUT:$line; while :; do read -r x; done"]);
    let id = manager.create(command, 20, 80)?;
    let session = manager.session(id)?;
    let pid = session.pid();
    let mut reader = session.reader()?;
    fn until(reader: &mut fs::File, needle: &str) -> Result<String> {
        let start = std::time::Instant::now();
        let mut out = String::new();
        while !out.contains(needle) {
            anyhow::ensure!(
                start.elapsed() < Duration::from_secs(3),
                "missing {needle}: {out}"
            );
            let mut fd = libc::pollfd {
                fd: reader.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            };
            if unsafe { libc::poll(&mut fd, 1, 50) } <= 0 {
                continue;
            }
            let mut bytes = [0; 2048];
            let n = reader.read(&mut bytes)?;
            anyhow::ensure!(n > 0, "unexpected EOF: {out}");
            out.push_str(&String::from_utf8_lossy(&bytes[..n]));
        }
        Ok(out)
    }
    until(&mut reader, "READY")?;
    session.resize(35, 104)?;
    session.write_all(b"hello\n")?;
    let out = until(&mut reader, "OUT:hello")?;
    assert!(out.contains("35 104"), "{out}");
    session.ctrl_c()?;
    until(&mut reader, "INTERRUPTED")?;
    assert_eq!(session.wait(Duration::from_secs(2))?.code(), Some(7));
    assert_eq!(manager.reap()?.len(), 1);
    gone(pid);
    Ok(())
}
