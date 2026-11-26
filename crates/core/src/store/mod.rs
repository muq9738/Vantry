pub(crate) mod blocks;
pub(crate) mod projection;
mod schema;
#[cfg(test)]
mod tests;
use crate::{Result, lock::InstanceLock};
use rusqlite::{Connection, OpenFlags};
use std::{
    path::{Path, PathBuf},
    sync::Mutex,
    time::Duration,
};

pub struct Store {
    pub(crate) db: Mutex<Connection>,
    pub(crate) directory: PathBuf,
    _lock: InstanceLock,
}
impl Store {
    pub fn open(directory: impl AsRef<Path>) -> Result<Self> {
        let directory = directory.as_ref();
        let lock = InstanceLock::acquire(directory)?;
        let directory = directory.canonicalize()?;
        let mut db = Connection::open(directory.join("state.sqlite"))?;
        db.busy_timeout(Duration::from_secs(5))?;
        schema::migrate(&mut db)?;
        crate::session::flush(&db, &directory)?;
        crate::telemetry::import(&mut db, &directory)?;
        Ok(Self {
            db: Mutex::new(db),
            directory,
            _lock: lock,
        })
    }
    /// Read a WAL snapshot through a separate handle so command commits can continue.
    pub fn backup_to(&self, destination: impl AsRef<Path>) -> Result<()> {
        let path = self.directory.join("state.sqlite");
        let target = destination.as_ref();
        if target.canonicalize().ok().as_ref() == Some(&path) {
            return Err(crate::invalid("backup destination is the source database"));
        }
        #[cfg(unix)]
        if let Ok(target_meta) = target.metadata() {
            use std::os::unix::fs::MetadataExt;
            let src = path.metadata()?;
            if (src.dev(), src.ino()) == (target_meta.dev(), target_meta.ino()) {
                return Err(crate::invalid("backup destination is the source database"));
            }
        }
        let src = Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY)?;
        let mut dst = Connection::open(destination)?;
        rusqlite::backup::Backup::new(&src, &mut dst)?.run_to_completion(
            128,
            Duration::from_millis(2),
            None,
        )?;
        Ok(())
    }
}
