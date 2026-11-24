//! An unnamed kill-on-close job. Start suspended so descendants cannot escape assignment.
use std::ptr::{null, null_mut};
use std::{
    ffi::c_void,
    io, mem,
    os::windows::{
        io::{AsRawHandle, FromRawHandle, OwnedHandle},
        process::CommandExt,
    },
    process::{Child, Command},
};
type Handle = *mut c_void;
#[repr(C)]
#[derive(Default)]
struct BasicLimits {
    process_time: i64,
    job_time: i64,
    flags: u32,
    min_ws: usize,
    max_ws: usize,
    active: u32,
    affinity: usize,
    priority: u32,
    scheduling: u32,
}
#[repr(C)]
#[derive(Default)]
struct Limits {
    basic: BasicLimits,
    io: [u64; 6],
    process_memory: usize,
    job_memory: usize,
    peak_process: usize,
    peak_job: usize,
}
#[link(name = "kernel32")]
unsafe extern "system" {
    fn CreateJobObjectW(attributes: *const c_void, name: *const u16) -> Handle;
    fn SetInformationJobObject(job: Handle, class: i32, info: *const c_void, size: u32) -> i32;
    fn AssignProcessToJobObject(job: Handle, process: Handle) -> i32;
    fn TerminateJobObject(job: Handle, code: u32) -> i32;
    fn GenerateConsoleCtrlEvent(event: u32, group: u32) -> i32;
}
#[link(name = "ntdll")]
unsafe extern "system" {
    fn NtResumeProcess(process: Handle) -> i32;
    fn RtlNtStatusToDosError(status: i32) -> u32;
}
pub(super) struct Job(OwnedHandle);
impl Job {
    pub(super) fn kill(&self) -> io::Result<()> {
        if unsafe { TerminateJobObject(self.0.as_raw_handle(), 1) } == 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }
    pub(super) fn interrupt(&self, pid: u32) {
        unsafe { GenerateConsoleCtrlEvent(1, pid) };
    }
}
pub(super) fn spawn(command: &mut Command) -> io::Result<(Child, Job)> {
    let handle = unsafe { CreateJobObjectW(null(), null()) };
    if handle == null_mut() {
        return Err(io::Error::last_os_error());
    }
    let job = Job(unsafe { OwnedHandle::from_raw_handle(handle) });
    let mut limits = Limits::default();
    limits.basic.flags = 0x2000;
    if unsafe {
        SetInformationJobObject(
            handle,
            9,
            (&limits as *const Limits).cast(),
            mem::size_of::<Limits>() as u32,
        )
    } == 0
    {
        return Err(io::Error::last_os_error());
    }
    command.creation_flags(0x4 | 0x200);
    let mut child = command.spawn()?;
    let result = if unsafe { AssignProcessToJobObject(handle, child.as_raw_handle()) } == 0 {
        Err(io::Error::last_os_error())
    } else {
        let status = unsafe { NtResumeProcess(child.as_raw_handle()) };
        if status < 0 {
            Err(io::Error::from_raw_os_error(
                unsafe { RtlNtStatusToDosError(status) } as i32,
            ))
        } else {
            Ok(())
        }
    };
    if let Err(e) = result {
        let _ = child.kill();
        let _ = child.wait();
        return Err(e);
    }
    Ok((child, job))
}
