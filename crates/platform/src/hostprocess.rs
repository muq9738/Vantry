//! Child lifetime owns a Unix process group or a Windows Job Object.
use std::{
    io::{self, Read, Write},
    process::{Child, Command, ExitStatus, Stdio},
    sync::atomic::{AtomicBool, Ordering},
    thread,
    time::{Duration, Instant},
};
#[cfg(windows)]
#[path = "windows_job.rs"]
mod windows_job;
#[cfg(unix)]
use std::os::unix::process::CommandExt;

pub fn clean_environment(command: &mut Command) {
    for key in [
        "GIT_DIR",
        "GIT_WORK_TREE",
        "GIT_COMMON_DIR",
        "GIT_INDEX_FILE",
        "GIT_OBJECT_DIRECTORY",
        "GIT_ALTERNATE_OBJECT_DIRECTORIES",
        "GIT_NAMESPACE",
        "GIT_PREFIX",
        "GIT_IMPLICIT_WORK_TREE",
        "GIT_TERMINAL_PROMPT",
        "GIT_MERGE_AUTOEDIT",
    ] {
        command.env_remove(key);
    }
}
pub struct Process {
    pub(crate) child: Child,
    status: Option<ExitStatus>,
    #[cfg(unix)]
    owns_session: bool,
    #[cfg(windows)]
    job: windows_job::Job,
}
impl Process {
    pub fn spawn(command: &mut Command) -> io::Result<Self> {
        #[cfg(unix)]
        {
            command.process_group(0);
            Ok(Self {
                child: command.spawn()?,
                status: None,
                owns_session: false,
            })
        }
        #[cfg(windows)]
        {
            let (child, job) = windows_job::spawn(command)?;
            Ok(Self {
                child,
                job,
                status: None,
            })
        }
    }
    #[cfg(unix)]
    pub(crate) fn from_session(child: Child) -> Self {
        Self {
            child,
            status: None,
            owns_session: true,
        }
    }
    pub fn take_stdin(&mut self) -> Option<std::process::ChildStdin> {
        self.child.stdin.take()
    }
    pub fn take_stdout(&mut self) -> Option<std::process::ChildStdout> {
        self.child.stdout.take()
    }
    pub fn take_stderr(&mut self) -> Option<std::process::ChildStderr> {
        self.child.stderr.take()
    }
    pub fn id(&self) -> u32 {
        self.child.id()
    }
    #[cfg(unix)]
    pub fn signal(&self, signal: i32) -> io::Result<()> {
        if self.status.is_some() {
            return Err(io::Error::new(io::ErrorKind::NotFound, "process exited"));
        }
        signal_group(self.id() as i32, signal)
    }
    fn kill_tree(&self) -> io::Result<()> {
        #[cfg(unix)]
        {
            let jobs = if self.owns_session {
                crate::pty::terminate_jobs(self.id() as i32)
            } else {
                Ok(())
            };
            self.signal(libc::SIGKILL)?;
            jobs
        }
        #[cfg(windows)]
        {
            self.job.kill()
        }
    }
    pub fn try_wait(&mut self) -> io::Result<Option<ExitStatus>> {
        if self.status.is_some() {
            return Ok(self.status);
        }
        #[cfg(unix)]
        {
            // Keep the leader waitable while signaling descendants. Reaping first would
            // let the kernel reuse this process-group identifier for an unrelated job.
            let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
            let rc = unsafe {
                libc::waitid(
                    libc::P_PID,
                    self.id(),
                    &mut info,
                    libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
                )
            };
            if rc < 0 {
                let err = io::Error::last_os_error();
                if err.kind() == io::ErrorKind::Interrupted {
                    return Ok(None);
                }
                return Err(err);
            }
            if unsafe { info.si_pid() } != 0 {
                let killed = self.kill_tree();
                self.status = Some(self.child.wait()?);
                // Darwin can return EPERM when the only member was already a zombie.
                if let Err(e) = killed
                    && !(e.raw_os_error() == Some(libc::EPERM)
                        && unsafe { libc::kill(-(self.id() as i32), 0) } < 0
                        && io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH))
                {
                    return Err(e);
                }
            }
        }
        #[cfg(windows)]
        if let Some(status) = self.child.try_wait()? {
            self.kill_tree()?;
            self.status = Some(status);
        }
        Ok(self.status)
    }
    pub fn wait(&mut self, timeout: Duration) -> io::Result<ExitStatus> {
        let start = Instant::now();
        loop {
            if let Some(s) = self.try_wait()? {
                return Ok(s);
            }
            if start.elapsed() >= timeout {
                self.stop(Duration::from_millis(150))?;
                return Err(io::ErrorKind::TimedOut.into());
            }
            thread::sleep(Duration::from_millis(4));
        }
    }
    pub fn stop(&mut self, grace: Duration) -> io::Result<ExitStatus> {
        if let Some(s) = self.try_wait()? {
            return Ok(s);
        }
        #[cfg(unix)]
        if let Err(error) = self.signal(libc::SIGTERM) {
            if let Some(s) = self.try_wait()? {
                return Ok(s);
            }
            return Err(error);
        }
        #[cfg(windows)]
        self.job.interrupt(self.id());
        let start = Instant::now();
        while start.elapsed() < grace {
            if let Some(s) = self.try_wait()? {
                return Ok(s);
            }
            thread::sleep(Duration::from_millis(4));
        }
        self.kill_tree()?;
        let status = self.child.wait()?;
        self.status = Some(status);
        Ok(status)
    }
}
impl Drop for Process {
    fn drop(&mut self) {
        if self.status.is_none() {
            let _ = self.kill_tree();
            let _ = self.child.wait();
        }
    }
}
#[cfg(unix)]
pub(crate) fn signal_group(group: i32, signal: i32) -> io::Result<()> {
    if group <= 1 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid process group",
        ));
    }
    if unsafe { libc::kill(-group, signal) } < 0 {
        let e = io::Error::last_os_error();
        if e.raw_os_error() != Some(libc::ESRCH) {
            return Err(e);
        }
    }
    Ok(())
}

#[derive(Debug)]
pub struct Output {
    pub status: ExitStatus,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}
fn process_error(operation: &str, e: io::Error) -> io::Error {
    io::Error::new(e.kind(), format!("{operation}: {e}"))
}
/// Drain both output streams while delivering stdin. Bound memory and terminate on overflow.
pub fn capture(
    command: &mut Command,
    input: &[u8],
    timeout: Duration,
    limit: usize,
) -> io::Result<Output> {
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut process = Process::spawn(command).map_err(|e| process_error("start child", e))?;
    let mut stdin = process.take_stdin().unwrap();
    let stdout = process.take_stdout().unwrap();
    let stderr = process.take_stderr().unwrap();
    #[cfg(unix)]
    {
        use std::os::fd::AsRawFd;
        for fd in [stdin.as_raw_fd(), stdout.as_raw_fd(), stderr.as_raw_fd()] {
            let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
            if flags < 0 || unsafe { libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0
            {
                return Err(io::Error::last_os_error());
            }
        }
    }
    let overflow = AtomicBool::new(false);
    let finished = AtomicBool::new(false);
    fn drain(
        reader: impl Read,
        limit: usize,
        overflow: &AtomicBool,
        strict: bool,
        finished: &AtomicBool,
    ) -> io::Result<Vec<u8>> {
        let mut reader = reader;
        let mut data = Vec::new();
        let mut buf = [0; 8192];
        loop {
            let n = match reader.read(&mut buf) {
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    if finished.load(Ordering::Acquire) {
                        break;
                    }
                    thread::sleep(Duration::from_millis(2));
                    continue;
                }
                result => result?,
            };
            if n == 0 {
                break;
            }
            let take = n.min(limit - data.len());
            data.extend_from_slice(&buf[..take]);
            if strict && take < n {
                overflow.store(true, Ordering::Release);
            }
        }
        Ok(data)
    }
    thread::scope(|s| {
        let done = &finished;
        let writer = s.spawn(move || -> io::Result<()> {
            let mut remaining = input;
            while !remaining.is_empty() {
                match stdin.write(remaining) {
                    Ok(0) => return Err(io::ErrorKind::WriteZero.into()),
                    Ok(n) => remaining = &remaining[n..],
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                    Err(e)
                        if e.kind() == io::ErrorKind::WouldBlock
                            && !done.load(Ordering::Acquire) =>
                    {
                        thread::sleep(Duration::from_millis(2))
                    }
                    Err(e) => return Err(e),
                }
            }
            Ok(())
        });
        let output = s.spawn(|| drain(stdout, limit, &overflow, true, done));
        let diagnostic = s.spawn(|| drain(stderr, 8192, &overflow, false, done));
        let start = Instant::now();
        let waited = (|| -> io::Result<Option<ExitStatus>> {
            loop {
                if overflow.load(Ordering::Acquire) || start.elapsed() >= timeout {
                    process
                        .stop(Duration::from_millis(100))
                        .map_err(|e| process_error("stop child group", e))?;
                    return Ok(None);
                }
                if let Some(status) = process
                    .try_wait()
                    .map_err(|e| process_error("reap child", e))?
                {
                    return Ok(Some(status));
                }
                thread::sleep(Duration::from_millis(4));
            }
        })();
        if waited.is_err() {
            let _ = process.stop(Duration::ZERO);
        }
        finished.store(true, Ordering::Release);
        let status = waited?;
        let stdout = output.join().unwrap()?;
        let stderr = diagnostic.join().unwrap()?;
        let wrote = writer.join().unwrap();
        if overflow.load(Ordering::Acquire) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "stdout exceeds limit",
            ));
        }
        let status = status.ok_or(io::ErrorKind::TimedOut)?;
        if status.success() {
            wrote?;
        }
        Ok(Output {
            stdout,
            stderr,
            status,
        })
    })
}
