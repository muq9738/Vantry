//! Interactive Unix terminals. Each session owns the master and reaps its shell.
use crate::hostprocess::{Process, signal_group};
use std::{
    collections::HashMap,
    fs::File,
    io::{self, Read, Write},
    os::fd::{AsRawFd, FromRawFd},
    os::unix::process::CommandExt,
    process::{Command, ExitStatus, Stdio},
    time::Duration,
};
use uuid::Uuid;

// Interactive shells move background pipelines into separate process groups. The session
// leader is still unreaped here, so its session ID cannot have been reused.
pub(crate) fn terminate_jobs(leader: i32) -> io::Result<()> {
    #[cfg(target_os = "macos")]
    let pids = {
        let count = unsafe { libc::proc_listallpids(std::ptr::null_mut(), 0) };
        if count < 0 {
            return Err(io::Error::last_os_error());
        }
        let mut pids = vec![0i32; count as usize + 64];
        let n =
            unsafe { libc::proc_listallpids(pids.as_mut_ptr().cast(), (pids.len() * 4) as i32) };
        if n < 0 {
            return Err(io::Error::last_os_error());
        }
        pids.truncate(n as usize);
        pids
    };
    #[cfg(target_os = "linux")]
    let pids = std::fs::read_dir("/proc")?
        .filter_map(|entry| entry.ok()?.file_name().to_str()?.parse::<i32>().ok())
        .collect::<Vec<_>>();
    for pid in pids {
        if pid <= 1 || pid == leader || unsafe { libc::getsid(pid) } != leader {
            continue;
        }
        let group = unsafe { libc::getpgid(pid) };
        if group > 1 && group != leader && unsafe { libc::getsid(pid) } == leader {
            signal_group(group, libc::SIGKILL)?;
        }
    }
    Ok(())
}
fn size(rows: u16, cols: u16) -> io::Result<libc::winsize> {
    if rows == 0 || cols == 0 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "zero terminal dimension",
        ));
    }
    Ok(libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    })
}
pub struct Session {
    master: Option<File>,
    process: Process,
}
impl Session {
    pub fn spawn(mut command: Command, rows: u16, cols: u16) -> io::Result<Self> {
        let mut master = -1;
        let mut slave = -1;
        let mut ws = size(rows, cols)?;
        if unsafe {
            libc::openpty(
                &mut master,
                &mut slave,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &mut ws,
            )
        } < 0
        {
            return Err(io::Error::last_os_error());
        }
        let (master, slave) = unsafe { (File::from_raw_fd(master), File::from_raw_fd(slave)) };
        for file in [&master, &slave] {
            if unsafe { libc::fcntl(file.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
                return Err(io::Error::last_os_error());
            }
        }
        command
            .stdin(Stdio::from(slave.try_clone()?))
            .stdout(Stdio::from(slave.try_clone()?))
            .stderr(Stdio::from(slave));
        if std::env::var_os("TERM").is_none() {
            command.env("TERM", "xterm-256color");
        }
        crate::hostprocess::clean_environment(&mut command);
        // STDIN is already the slave by the time Command runs pre_exec.
        unsafe {
            command.pre_exec(|| {
                // A background Core can inherit ignored SIGINT from its launcher.
                if libc::signal(libc::SIGINT, libc::SIG_DFL) == libc::SIG_ERR {
                    return Err(io::Error::last_os_error());
                }
                if libc::setsid() < 0 || libc::ioctl(0, libc::TIOCSCTTY as _, 0) < 0 {
                    return Err(io::Error::last_os_error());
                }
                Ok(())
            });
        }
        Ok(Self {
            master: Some(master),
            process: Process::from_session(command.spawn()?),
        })
    }
    pub fn pid(&self) -> u32 {
        self.process.id()
    }
    fn master(&self) -> io::Result<&File> {
        self.master
            .as_ref()
            .ok_or_else(|| io::ErrorKind::BrokenPipe.into())
    }
    pub fn reader(&self) -> io::Result<File> {
        self.master()?.try_clone()
    }
    pub fn resize(&self, rows: u16, cols: u16) -> io::Result<()> {
        let ws = size(rows, cols)?;
        if unsafe { libc::ioctl(self.master()?.as_raw_fd(), libc::TIOCSWINSZ as _, &ws) } < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
    pub fn signal(&self, signal: i32) -> io::Result<()> {
        let group = unsafe { libc::tcgetpgrp(self.master()?.as_raw_fd()) };
        if group > 1 {
            signal_group(group, signal)
        } else {
            self.process.signal(signal)
        }
    }
    pub fn ctrl_c(&mut self) -> io::Result<()> {
        self.write_all(b"\x03")
    }
    pub fn try_wait(&mut self) -> io::Result<Option<ExitStatus>> {
        self.process.try_wait()
    }
    pub fn wait(&mut self, timeout: Duration) -> io::Result<ExitStatus> {
        let start = std::time::Instant::now();
        loop {
            if let Some(status) = self.try_wait()? {
                return Ok(status);
            }
            if start.elapsed() >= timeout {
                self.close()?;
                return Err(io::ErrorKind::TimedOut.into());
            }
            std::thread::sleep(Duration::from_millis(4));
        }
    }
    pub fn close(&mut self) -> io::Result<ExitStatus> {
        if let Some(master) = self.master.take() {
            unsafe { libc::tcflush(master.as_raw_fd(), libc::TCIOFLUSH) };
            drop(master);
        }
        self.process.stop(Duration::from_millis(150))
    }
}
impl Drop for Session {
    fn drop(&mut self) {
        let _ = self.close();
    }
}
impl Read for Session {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match self.master()?.read(buf) {
            Err(e) if e.raw_os_error() == Some(libc::EIO) => Ok(0),
            r => r,
        }
    }
}
impl Write for Session {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.master()?.write(bytes)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.master()?.flush()
    }
}
#[derive(Default)]
pub struct Manager {
    sessions: HashMap<Uuid, Session>,
}
impl Manager {
    pub fn create(&mut self, command: Command, rows: u16, cols: u16) -> io::Result<Uuid> {
        let session = Session::spawn(command, rows, cols)?;
        let id = Uuid::new_v4();
        self.sessions.insert(id, session);
        Ok(id)
    }
    pub fn session(&mut self, id: Uuid) -> io::Result<&mut Session> {
        self.sessions
            .get_mut(&id)
            .ok_or_else(|| io::ErrorKind::NotFound.into())
    }
    pub fn close(&mut self, id: Uuid) -> io::Result<ExitStatus> {
        self.sessions
            .remove(&id)
            .ok_or(io::ErrorKind::NotFound)?
            .close()
    }
    pub fn reap(&mut self) -> io::Result<Vec<(Uuid, ExitStatus)>> {
        let mut done = Vec::new();
        for (id, session) in &mut self.sessions {
            if let Some(status) = session.try_wait()? {
                done.push((*id, status));
            }
        }
        for (id, _) in &done {
            self.sessions.remove(id);
        }
        Ok(done)
    }
}
