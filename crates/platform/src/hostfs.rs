//! Root-relative, no-follow I/O and durable private state files.
use anyhow::{Result, ensure};
use serde::{Serialize, de::DeserializeOwned};
use std::{
    fs::{self, File},
    io::{self, Read, Write},
    path::{Component, Path, PathBuf},
};

pub(crate) fn invalid(reason: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, reason)
}

pub fn relative(path: &Path) -> io::Result<&Path> {
    if path.as_os_str().is_empty()
        || !path.components().any(|p| matches!(p, Component::Normal(_)))
        || path
            .components()
            .any(|p| !matches!(p, Component::Normal(_) | Component::CurDir))
    {
        return Err(invalid("expected a nonempty path inside the root"));
    }
    #[cfg(windows)]
    if path.to_string_lossy().contains(':') {
        return Err(invalid("alternate streams are not allowed"));
    }
    Ok(path)
}
pub(crate) fn component(value: &str) -> Result<()> {
    ensure!(
        !value.is_empty()
            && value != "."
            && value != ".."
            && !value.contains(['/', '\\', ':'])
            && !value.chars().any(char::is_control),
        "invalid path component"
    );
    relative(Path::new(value))?;
    Ok(())
}

#[cfg(unix)]
mod imp {
    use super::*;
    use std::{
        ffi::{CString, OsStr},
        os::unix::{
            ffi::OsStrExt,
            fs::MetadataExt,
            io::{AsRawFd, FromRawFd},
        },
    };
    fn name(part: &OsStr) -> io::Result<CString> {
        CString::new(part.as_bytes()).map_err(|_| invalid("NUL in path"))
    }
    fn openat(dir: &File, part: &OsStr, flags: i32) -> io::Result<File> {
        let n = name(part)?;
        let fd = unsafe {
            libc::openat(
                dir.as_raw_fd(),
                n.as_ptr(),
                flags | libc::O_CLOEXEC | libc::O_NOFOLLOW | libc::O_NONBLOCK,
                0o600 as libc::c_int,
            )
        };
        if fd < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(unsafe { File::from_raw_fd(fd) })
        }
    }
    pub(super) fn directory(path: &Path, create: bool) -> io::Result<File> {
        if path.as_os_str().is_empty() {
            return Err(invalid("empty directory"));
        }
        let absolute = std::path::absolute(path)?;
        // Darwin's system-owned aliases are part of the OS layout.
        #[cfg(target_os = "macos")]
        let absolute = ["/tmp", "/var", "/etc"]
            .into_iter()
            .find_map(|alias| {
                absolute
                    .strip_prefix(alias)
                    .ok()
                    .map(|tail| Path::new("/private").join(&alias[1..]).join(tail))
            })
            .unwrap_or(absolute);
        let mut dir = File::open("/")?;
        for p in absolute.components() {
            match p {
                Component::RootDir | Component::CurDir => continue,
                Component::Normal(part) => {
                    let flags = libc::O_DIRECTORY | libc::O_RDONLY;
                    dir = match openat(&dir, part, flags) {
                        Err(e) if create && e.kind() == io::ErrorKind::NotFound => {
                            if unsafe {
                                libc::mkdirat(dir.as_raw_fd(), name(part)?.as_ptr(), 0o700)
                            } < 0
                            {
                                let e = io::Error::last_os_error();
                                if e.kind() != io::ErrorKind::AlreadyExists {
                                    return Err(e);
                                }
                            }
                            openat(&dir, part, flags)?
                        }
                        result => result?,
                    };
                }
                _ => return Err(invalid("invalid directory component")),
            }
        }
        Ok(dir)
    }
    pub(super) fn private(path: &Path, create: bool) -> io::Result<File> {
        let dir = directory(path, create)?;
        let m = dir.metadata()?;
        if m.mode() & 0o077 != 0 || m.uid() != unsafe { libc::geteuid() } {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "directory must be owner-private",
            ));
        }
        Ok(dir)
    }
    pub(super) fn open(path: &Path, lock: bool) -> io::Result<File> {
        let (parent, leaf) = split(path)?;
        let dir = directory(parent, false)?;
        let flags = if lock {
            libc::O_RDWR | libc::O_CREAT
        } else {
            libc::O_RDONLY
        };
        let file = openat(&dir, leaf, flags)?;
        if !file.metadata()?.is_file() {
            return Err(invalid("expected regular file"));
        }
        Ok(file)
    }
    pub(super) fn atomic(path: &Path, bytes: &[u8]) -> io::Result<()> {
        let (parent, leaf) = split(path)?;
        let dir = private(parent, true)?;
        let dest = name(leaf)?;
        match openat(&dir, leaf, libc::O_RDONLY) {
            Ok(f) if !f.metadata()?.is_file() => {
                return Err(invalid("destination must be regular"));
            }
            Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
            _ => (),
        }
        let tmp = format!(".tmp-{}", uuid::Uuid::new_v4());
        let temp_name = name(tmp.as_ref())?;
        let fd = dir.as_raw_fd();
        let mut f = openat(
            &dir,
            tmp.as_ref(),
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
        )?;
        let result = (|| {
            f.write_all(bytes)?;
            f.sync_all()?;
            drop(f);
            if unsafe { libc::renameat(fd, temp_name.as_ptr(), fd, dest.as_ptr()) } < 0 {
                return Err(io::Error::last_os_error());
            }
            dir.sync_all()
        })();
        if result.is_err() {
            unsafe { libc::unlinkat(fd, temp_name.as_ptr(), 0) };
        }
        result
    }
}
#[cfg(windows)]
#[path = "windows_fs.rs"]
mod imp;

fn split(path: &Path) -> io::Result<(&Path, &std::ffi::OsStr)> {
    let parent = path.parent().ok_or_else(|| invalid("missing parent"))?;
    Ok((
        if parent.as_os_str().is_empty() {
            Path::new(".")
        } else {
            parent
        },
        path.file_name()
            .ok_or_else(|| invalid("missing file name"))?,
    ))
}
pub fn ensure_private_dir(path: &Path) -> io::Result<()> {
    imp::private(path, true).map(drop)
}
pub fn check_private_dir(path: &Path) -> io::Result<()> {
    imp::private(path, false).map(drop)
}
pub fn atomic_write(path: &Path, bytes: &[u8]) -> io::Result<()> {
    imp::atomic(path, bytes)
}
pub fn directory_handle(path: &Path) -> io::Result<File> {
    imp::directory(path, false)
}
pub fn open_file(root: &Path, path: &Path) -> io::Result<File> {
    imp::open(&root.join(relative(path)?), false)
}
pub fn read_file(root: &Path, path: &Path, limit: usize) -> io::Result<Vec<u8>> {
    let f = open_file(root, path)?;
    let mut data = Vec::new();
    f.take((limit as u64).saturating_add(1))
        .read_to_end(&mut data)?;
    if data.len() > limit {
        return Err(invalid("file exceeds size limit"));
    }
    Ok(data)
}
pub fn lock_dir(path: &Path, wait: bool) -> io::Result<File> {
    #[cfg(unix)]
    let handle = directory_handle(path)?;
    #[cfg(windows)]
    let handle = imp::directory_lock(path)?;
    lock(handle, wait)
}
pub fn lock_file(path: &Path, wait: bool) -> io::Result<File> {
    lock(imp::open(path, true)?, wait)
}
fn lock(file: File, wait: bool) -> io::Result<File> {
    if wait {
        file.lock()?;
    } else {
        file.try_lock().map_err(io::Error::from)?;
    }
    Ok(file)
}
pub(crate) fn read_json<T: DeserializeOwned>(path: &Path) -> Result<Option<T>> {
    let file = match imp::open(path, false) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        r => r?,
    };
    Ok(Some(serde_json::from_reader(file)?))
}
pub(crate) fn write_json(path: &Path, value: &impl Serialize) -> Result<()> {
    let mut data = serde_json::to_vec_pretty(value)?;
    data.push(b'\n');
    Ok(atomic_write(path, &data)?)
}
pub(crate) fn private_root(path: &Path) -> Result<PathBuf> {
    ensure_private_dir(path)?;
    Ok(fs::canonicalize(path)?)
}
