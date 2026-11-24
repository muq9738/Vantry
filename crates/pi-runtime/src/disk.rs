use anyhow::Result;
use serde::{Serialize, de::DeserializeOwned};
use std::{
    fs::{self, File, OpenOptions},
    io::Write,
    path::Path,
};
use uuid::Uuid;

pub(crate) fn directory(path: &Path) -> Result<()> {
    let mut builder = fs::DirBuilder::new();
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.recursive(true).create(path)?;
    Ok(())
}
pub(crate) fn new_file(path: &Path) -> Result<File> {
    let mut opts = OpenOptions::new();
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    Ok(opts.write(true).create_new(true).open(path)?)
}
pub(crate) fn read<T: DeserializeOwned>(path: &Path) -> Result<T> {
    Ok(serde_json::from_reader(File::open(path)?)?)
}
pub(crate) fn atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let dir = path.parent().expect("file in directory");
    let temp = dir.join(format!(".{}.tmp", Uuid::new_v4()));
    let mut file = new_file(&temp)?;
    let result = (|| {
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, path)?;
        #[cfg(unix)]
        File::open(dir)?.sync_all()?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(temp);
    }
    result
}
pub(crate) fn write_json(path: &Path, data: &impl Serialize) -> Result<()> {
    atomic(path, &serde_json::to_vec(data)?)
}
