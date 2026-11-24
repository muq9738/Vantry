use crate::{Result, conflict};
#[cfg(unix)]
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::{
    fs::{self, File, OpenOptions},
    path::Path,
};

/// Uses the same advisory lock and persistent inode as the Go service.
pub struct InstanceLock(File);
impl InstanceLock {
    pub fn acquire(dir: &Path) -> Result<Self> {
        let mut builder = fs::DirBuilder::new();
        builder.recursive(true);
        #[cfg(unix)]
        builder.mode(0o700);
        builder.create(dir)?;
        let mut options = OpenOptions::new();
        options.create(true).read(true).write(true);
        #[cfg(unix)]
        options.mode(0o600);
        let file = options.open(dir.join("core.lock"))?;
        match file.try_lock() {
            Ok(()) => Ok(Self(file)),
            Err(std::fs::TryLockError::WouldBlock) => {
                Err(conflict("another Core owns this directory"))
            }
            Err(std::fs::TryLockError::Error(e)) => Err(e.into()),
        }
    }
}
impl Drop for InstanceLock {
    fn drop(&mut self) {
        let _ = self.0.unlock();
    }
}
