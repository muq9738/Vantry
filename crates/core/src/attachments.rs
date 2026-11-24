use crate::{Result, invalid};
use std::{fs, io::Read, path::Path};
use vantry_protocol::Attachment;
pub(crate) const MAX_ATTACHMENTS: usize = 32;

/// Match an existing imported attachment using the persisted v1 metadata and content size.
pub(crate) fn validate(data: &Path, items: &[Attachment]) -> Result<()> {
    if items.len() > MAX_ATTACHMENTS {
        return Err(invalid("at most 32 attachments are supported"));
    }
    for item in items {
        let Some(suffix) = item.id.strip_prefix("file_") else {
            return Err(invalid("invalid attachment id"));
        };
        if suffix.is_empty()
            || item.id.len() > 128
            || !suffix
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
            || item.name.is_empty()
            || item.name.len() > 255
            || matches!(item.name.as_str(), "." | "..")
            || item.name.contains(['/', '\\'])
            || item.name.chars().any(char::is_control)
            || !(1..=(8 << 20)).contains(&item.size)
        {
            return Err(invalid("invalid attachment metadata"));
        }
        let root = data.join("attachments");
        let dir = root.join(&item.id);
        let content = dir.join("content");
        for path in [&root, &dir, &content] {
            let meta = fs::symlink_metadata(path)
                .map_err(|_| invalid("attachment directory unavailable"))?;
            if !meta.is_dir() {
                return Err(invalid("invalid attachment directory"));
            }
        }
        let metadata = dir.join("metadata.json");
        let m =
            fs::symlink_metadata(&metadata).map_err(|_| invalid("missing attachment metadata"))?;
        if !m.is_file() || m.len() > 4096 {
            return Err(invalid("invalid attachment metadata file"));
        }
        let mut raw = Vec::new();
        fs::File::open(metadata)?.take(4097).read_to_end(&mut raw)?;
        let stored: Attachment = serde_json::from_slice(&raw)
            .map_err(|_| invalid("invalid attachment metadata JSON"))?;
        let m = fs::symlink_metadata(content.join(&item.name))
            .map_err(|_| invalid("missing attachment data"))?;
        if stored != *item || !m.is_file() || m.len() != item.size as u64 {
            return Err(invalid("attachment differs from its uploaded record"));
        }
    }
    Ok(())
}
