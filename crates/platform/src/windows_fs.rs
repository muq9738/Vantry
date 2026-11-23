//! Reparse-point-aware Win32 handles and owner-private inheritable ACLs.
use super::*;
use std::{
    ffi::c_void,
    fs::OpenOptions,
    mem,
    os::windows::{
        ffi::{OsStrExt, OsStringExt},
        fs::{MetadataExt, OpenOptionsExt},
        io::AsRawHandle,
    },
    ptr::{null, null_mut},
};
type Ptr = *mut c_void;
#[repr(C)]
struct SecurityAttributes {
    len: u32,
    descriptor: Ptr,
    inherit: i32,
}
#[repr(C)]
struct Acl {
    revision: u8,
    reserved: u8,
    size: u16,
    count: u16,
    reserved2: u16,
}
#[repr(C)]
struct FileInfo {
    attributes: u32,
    times: [u32; 6],
    volume: u32,
    size_high: u32,
    size_low: u32,
    links: u32,
    index_high: u32,
    index_low: u32,
}
#[link(name = "kernel32")]
unsafe extern "system" {
    fn LocalFree(p: Ptr) -> Ptr;
    fn CreateDirectoryW(name: *const u16, attributes: *const SecurityAttributes) -> i32;
    fn MoveFileExW(src: *const u16, dst: *const u16, flags: u32) -> i32;
    fn GetFileInformationByHandle(file: Ptr, info: *mut FileInfo) -> i32;
}
#[link(name = "advapi32")]
unsafe extern "system" {
    fn GetTokenInformation(token: Ptr, kind: u32, buf: Ptr, len: u32, needed: *mut u32) -> i32;
    fn ConvertSidToStringSidW(sid: Ptr, text: *mut *mut u16) -> i32;
    fn ConvertStringSecurityDescriptorToSecurityDescriptorW(
        text: *const u16,
        revision: u32,
        sd: *mut Ptr,
        size: *mut u32,
    ) -> i32;
    fn GetSecurityInfo(
        handle: Ptr,
        kind: u32,
        flags: u32,
        owner: *mut Ptr,
        group: *mut Ptr,
        dacl: *mut *mut Acl,
        sacl: *mut Ptr,
        sd: *mut Ptr,
    ) -> u32;
    fn EqualSid(a: Ptr, b: Ptr) -> i32;
    fn IsWellKnownSid(sid: Ptr, kind: u32) -> i32;
    fn GetAce(acl: *mut Acl, index: u32, ace: *mut Ptr) -> i32;
}
struct Local(Ptr);
impl Drop for Local {
    fn drop(&mut self) {
        unsafe { LocalFree(self.0) };
    }
}
fn wide(path: &Path) -> io::Result<Vec<u16>> {
    let mut units: Vec<_> = path.as_os_str().encode_wide().collect();
    if units.contains(&0) {
        return Err(invalid("NUL in path"));
    }
    units.push(0);
    Ok(units)
}
fn check(ok: i32) -> io::Result<()> {
    if ok == 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}
fn user() -> io::Result<Vec<usize>> {
    let mut needed = 0;
    let token = (-4isize) as Ptr;
    unsafe { GetTokenInformation(token, 1, null_mut(), 0, &mut needed) };
    if needed == 0 {
        return Err(io::Error::last_os_error());
    }
    let mut buf = vec![0usize; (needed as usize).div_ceil(mem::size_of::<usize>())];
    check(unsafe { GetTokenInformation(token, 1, buf.as_mut_ptr().cast(), needed, &mut needed) })?;
    Ok(buf)
}
fn create(path: &Path) -> io::Result<()> {
    let u = user()?;
    let mut text = null_mut();
    check(unsafe { ConvertSidToStringSidW(u[0] as Ptr, &mut text) })?;
    let _sid = Local(text.cast());
    let mut n = 0;
    unsafe {
        while *text.add(n) != 0 {
            n += 1;
        }
    }
    let sid = String::from_utf16_lossy(unsafe { std::slice::from_raw_parts(text, n) });
    let sddl: Vec<_> = format!("O:{sid}D:P(A;OICI;FA;;;{sid})(A;OICI;FA;;;SY)\0")
        .encode_utf16()
        .collect();
    let mut sd = null_mut();
    check(unsafe {
        ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl.as_ptr(), 1, &mut sd, null_mut())
    })?;
    let _sd = Local(sd);
    let attr = SecurityAttributes {
        len: mem::size_of::<SecurityAttributes>() as u32,
        descriptor: sd,
        inherit: 0,
    };
    check(unsafe { CreateDirectoryW(wide(path)?.as_ptr(), &attr) })
}
fn check_acl(dir: &File) -> io::Result<()> {
    let mut owner = null_mut();
    let mut acl = null_mut();
    let mut sd = null_mut();
    let result = unsafe {
        GetSecurityInfo(
            dir.as_raw_handle(),
            1,
            5,
            &mut owner,
            null_mut(),
            &mut acl,
            null_mut(),
            &mut sd,
        )
    };
    if result != 0 {
        return Err(io::Error::from_raw_os_error(result as i32));
    }
    let _sd = Local(sd);
    let u = user()?;
    let sid = u[0] as Ptr;
    let deny = || {
        io::Error::new(
            io::ErrorKind::PermissionDenied,
            "directory ACL is not owner-private",
        )
    };
    if owner.is_null() || acl.is_null() || unsafe { EqualSid(owner, sid) } == 0 {
        return Err(deny());
    }
    for i in 0..unsafe { (*acl).count } {
        let mut ace = null_mut();
        check(unsafe { GetAce(acl, i.into(), &mut ace) })?;
        let p = ace.cast::<u8>();
        let kind = unsafe { *p };
        if kind == 1 {
            continue;
        }
        if kind != 0 {
            return Err(deny());
        }
        let principal = unsafe { p.add(8) }.cast();
        if unsafe {
            *p.add(4).cast::<u32>() != 0
                && EqualSid(principal, sid) == 0
                && IsWellKnownSid(principal, 22) == 0
                && IsWellKnownSid(principal, 26) == 0
        } {
            return Err(deny());
        }
    }
    Ok(())
}
fn checked(file: File, dir: bool) -> io::Result<File> {
    let m = file.metadata()?;
    if m.file_attributes() & 0x400 != 0 || (dir && !m.is_dir()) || (!dir && !m.is_file()) {
        return Err(invalid("unexpected file type or reparse point"));
    }
    Ok(file)
}
fn chain(path: &Path, create_dirs: bool) -> io::Result<Vec<File>> {
    if path.as_os_str().is_empty() {
        return Err(invalid("empty path"));
    }
    let absolute = std::path::absolute(path)?;
    let mut current = PathBuf::new();
    let mut handles = Vec::new();
    for part in absolute.components() {
        match part {
            Component::ParentDir => return Err(invalid("parent traversal")),
            Component::Prefix(_) => {
                current.push(part);
                continue;
            }
            Component::CurDir => continue,
            _ => current.push(part),
        }
        let open = || {
            OpenOptions::new()
                .access_mode(0x20080)
                .share_mode(3)
                .custom_flags(0x02200000)
                .open(&current)
        };
        let f = match open() {
            Err(e) if create_dirs && e.kind() == io::ErrorKind::NotFound => {
                if let Err(e) = create(&current)
                    && e.kind() != io::ErrorKind::AlreadyExists
                {
                    return Err(e);
                }
                open()?
            }
            r => r?,
        };
        handles.push(checked(f, true)?);
    }
    Ok(handles)
}
pub(super) fn directory(path: &Path, create: bool) -> io::Result<File> {
    chain(path, create)?
        .pop()
        .ok_or_else(|| invalid("missing directory"))
}
pub(super) fn private(path: &Path, create: bool) -> io::Result<File> {
    let dir = directory(path, create)?;
    check_acl(&dir)?;
    Ok(dir)
}
pub(super) fn open(path: &Path, lock: bool) -> io::Result<File> {
    let (dir, _) = split(path)?;
    let _ancestors = chain(dir, false)?;
    checked(
        OpenOptions::new()
            .read(true)
            .write(lock)
            .create(lock)
            .custom_flags(0x00200000)
            .open(path)?,
        false,
    )
}
pub(super) fn atomic(path: &Path, bytes: &[u8]) -> io::Result<()> {
    let (parent, _) = split(path)?;
    let ancestors = chain(parent, true)?;
    check_acl(
        ancestors
            .last()
            .ok_or_else(|| invalid("missing directory"))?,
    )?;
    match fs::symlink_metadata(path) {
        Ok(m) if m.file_attributes() & 0x400 != 0 || !m.is_file() => {
            return Err(invalid("invalid destination"));
        }
        Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
        _ => (),
    };
    let mut tmp = tempfile::NamedTempFile::new_in(parent)?;
    tmp.write_all(bytes)?;
    tmp.as_file().sync_all()?;
    let name = tmp.into_temp_path();
    check(unsafe { MoveFileExW(wide(&name)?.as_ptr(), wide(path)?.as_ptr(), 9) })
}
#[link(name = "shell32")]
unsafe extern "system" {
    fn SHGetKnownFolderPath(id: *const u128, flags: u32, token: Ptr, path: *mut *mut u16) -> i32;
}
#[link(name = "ole32")]
unsafe extern "system" {
    fn CoTaskMemFree(p: Ptr);
}
pub(super) fn directory_lock(path: &Path) -> io::Result<File> {
    let dir = directory(path, false)?;
    let mut info = mem::MaybeUninit::uninit();
    check(unsafe { GetFileInformationByHandle(dir.as_raw_handle(), info.as_mut_ptr()) })?;
    let info = unsafe { info.assume_init() };
    let id = u128::from_le_bytes([
        0x85, 0x27, 0xb3, 0xf1, 0xba, 0x6f, 0xcf, 0x4f, 0x9d, 0x55, 0x7b, 0x8e, 0x7f, 0x15, 0x70,
        0x91,
    ]);
    let mut raw = null_mut();
    let hr = unsafe { SHGetKnownFolderPath(&id, 0, null_mut(), &mut raw) };
    if hr < 0 {
        return Err(io::Error::from_raw_os_error(hr));
    }
    let mut n = 0;
    unsafe {
        while *raw.add(n) != 0 {
            n += 1;
        }
    }
    let path = std::ffi::OsString::from_wide(unsafe { std::slice::from_raw_parts(raw, n) });
    unsafe { CoTaskMemFree(raw.cast()) };
    let locks = PathBuf::from(path).join("Vantry/workspace-locks");
    private(&locks, true)?;
    open(
        &locks.join(format!(
            "{:08x}-{:08x}{:08x}.lock",
            info.volume, info.index_high, info.index_low
        )),
        true,
    )
}
