#![cfg(windows)]
use anyhow::{Context, Result};
use std::{
    fs, io,
    os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle, RawHandle},
    path::PathBuf,
    process::Command,
    thread,
    time::{Duration, Instant},
};
use vantry_platform::{hostfs, hostprocess::Process};
#[link(name = "kernel32")]
unsafe extern "system" {
    fn OpenProcess(access: u32, inherit: i32, pid: u32) -> RawHandle;
    fn WaitForSingleObject(handle: RawHandle, millis: u32) -> u32;
}

#[test]
fn job_wait_fixture() {
    if std::env::var_os("VANTRY_P4_JOB_FIXTURE").is_some() {
        thread::sleep(Duration::from_secs(8));
    }
}
#[test]
fn job_parent_fixture() -> Result<()> {
    let Some(path) = std::env::var_os("VANTRY_P4_JOB_FIXTURE") else {
        return Ok(());
    };
    let mut child = Command::new(std::env::current_exe()?)
        .args(["--exact", "job_wait_fixture"])
        .spawn()?;
    fs::write(path, child.id().to_string())?;
    child.wait()?;
    Ok(())
}
#[test]
fn suspended_assignment_contains_first_grandchild() -> Result<()> {
    let temp = tempfile::Builder::new().prefix("vantry-p4-").tempdir()?;
    let marker = temp.path().join("child");
    let mut cmd = Command::new(std::env::current_exe()?);
    cmd.args(["--exact", "job_parent_fixture"])
        .env("VANTRY_P4_JOB_FIXTURE", &marker);
    let mut process = Process::spawn(&mut cmd)?;
    let start = Instant::now();
    let id = loop {
        if let Ok(text) = fs::read_to_string(&marker)
            && let Ok(pid) = text.parse::<u32>()
        {
            break pid;
        }
        anyhow::ensure!(
            start.elapsed() < Duration::from_secs(4),
            "missing child PID"
        );
        thread::sleep(Duration::from_millis(10));
    };
    let handle = unsafe { OpenProcess(0x00100000, 0, id) };
    if handle.is_null() {
        return Err(io::Error::last_os_error().into());
    }
    let handle = unsafe { OwnedHandle::from_raw_handle(handle) };
    process.stop(Duration::from_millis(50))?;
    assert_eq!(
        unsafe { WaitForSingleObject(handle.as_raw_handle(), 2000) },
        0
    );
    Ok(())
}
#[test]
fn private_acl_refuses_everyone_and_directory_lock_uses_same_identity() -> Result<()> {
    let tmp = tempfile::Builder::new().prefix("vantry-p4-").tempdir()?;
    let root = tmp.path().join("private");
    hostfs::ensure_private_dir(&root)?;
    hostfs::check_private_dir(&root)?;
    let lock = hostfs::lock_dir(&root, true)?;
    assert!(hostfs::lock_dir(&root.canonicalize()?, false).is_err());
    drop(lock);
    let _lock = hostfs::lock_dir(&root, false)?;
    let icacls = PathBuf::from(std::env::var_os("SystemRoot").context("missing SystemRoot")?)
        .join("System32/icacls.exe");
    let status = Command::new(icacls)
        .arg(&root)
        .args(["/grant", "*S-1-1-0:(RX)"])
        .status()?;
    assert!(status.success());
    assert!(hostfs::check_private_dir(&root).is_err());
    assert!(hostfs::atomic_write(&root.join("value"), b"private").is_err());
    Ok(())
}
