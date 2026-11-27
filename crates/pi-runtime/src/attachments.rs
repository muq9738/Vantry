//! Resolve only canonical Core upload records for initial prompts and steering.
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use pi_ai::ContentBlock;
use std::{
    fs::{self, File},
    io::Read,
    path::{Path, PathBuf},
};
use vantry_protocol::Attachment;

// Anchor each Unix lookup to an open directory. NOFOLLOW applies to each individual
// component; NONBLOCK keeps a substituted FIFO from wedging the worker before fstat.
#[cfg(any(target_os = "macos", target_os = "linux"))]
fn child(parent: &File, _: &Path, name: &str) -> Result<File> {
    use std::{
        ffi::CString,
        os::fd::{AsRawFd, FromRawFd},
    };
    unsafe extern "C" {
        fn openat(dir: i32, name: *const std::ffi::c_char, flags: i32, ...) -> i32;
    }
    #[cfg(target_os = "macos")]
    const FLAGS: i32 = 0x100 | 4 | 0x1000000; // O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC
    #[cfg(target_os = "linux")]
    const FLAGS: i32 = 0o400000 | 0o4000 | 0o2000000;
    let name = CString::new(name)?;
    let fd = unsafe { openat(parent.as_raw_fd(), name.as_ptr(), FLAGS) };
    if fd < 0 {
        return Err(std::io::Error::last_os_error().into());
    }
    Ok(unsafe { File::from_raw_fd(fd) })
}
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
fn child(_: &File, path: &Path, name: &str) -> Result<File> {
    open_path(&path.join(name))
}
fn open_path(path: &Path) -> Result<File> {
    let metadata = fs::symlink_metadata(path)?;
    ensure!(!metadata.is_symlink(), "attachment symlink");
    let mut opts = File::options();
    opts.read(true);
    #[cfg(windows)]
    {
        use std::os::windows::fs::{MetadataExt, OpenOptionsExt};
        ensure!(
            metadata.file_attributes() & 0x400 == 0,
            "attachment reparse point"
        );
        opts.custom_flags(0x02000000 | 0x00200000);
    }
    Ok(opts.open(path)?)
}
fn directory(file: &File) -> Result<()> {
    let m = file.metadata()?;
    ensure!(m.is_dir(), "invalid attachment directory");
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        ensure!(
            m.permissions().mode() & 0o077 == 0,
            "attachment directory is not private"
        );
    }
    Ok(())
}
fn bytes(file: File, limit: u64) -> Result<Vec<u8>> {
    let m = file.metadata()?;
    ensure!(
        m.is_file() && m.len() > 0 && m.len() <= limit,
        "invalid attachment file"
    );
    let mut bytes = vec![];
    file.take(limit + 1).read_to_end(&mut bytes)?;
    ensure!(bytes.len() as u64 <= limit, "attachment too large");
    Ok(bytes)
}
fn resolve(data: &Path, item: &Attachment) -> Result<(PathBuf, Vec<u8>)> {
    let id = item
        .id
        .strip_prefix("file_")
        .context("invalid attachment ID")?;
    ensure!(
        !id.is_empty()
            && item.id.len() <= 128
            && id
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || b"_-".contains(&c)),
        "invalid attachment ID"
    );
    ensure!(
        !matches!(item.name.as_str(), "" | "." | "..")
            && item.name.len() <= 255
            && !item.name.contains(['/', '\\'])
            && !item.name.chars().any(char::is_control),
        "invalid attachment name"
    );
    #[cfg(windows)]
    ensure!(!item.name.contains(':'), "invalid attachment filename");
    ensure!(
        (1..=8 << 20).contains(&item.size),
        "invalid attachment size"
    );
    let mut dir = open_path(data)?;
    let mut path = data.to_owned();
    for name in ["attachments", &item.id] {
        dir = child(&dir, &path, name)?;
        directory(&dir)?;
        path.push(name);
    }
    let metadata: Attachment =
        serde_json::from_slice(&bytes(child(&dir, &path, "metadata.json")?, 4096)?)?;
    ensure!(metadata == *item, "attachment metadata mismatch");
    dir = child(&dir, &path, "content")?;
    directory(&dir)?;
    path.push("content");
    let content = bytes(child(&dir, &path, &item.name)?, item.size as u64)?;
    ensure!(
        content.len() as i64 == item.size,
        "attachment size mismatch"
    );
    Ok((path.join(&item.name), content))
}
enum AttachmentContent {
    Image(ContentBlock),
    Text(String),
    Pdf {
        text: String,
        pages: usize,
        path: PathBuf,
    },
}

fn content(data: &Path, item: &Attachment) -> Result<AttachmentContent> {
    let (path, bytes) = resolve(data, item)?;
    let mime = item
        .mime
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .to_ascii_lowercase();
    if matches!(
        mime.as_str(),
        "image/png" | "image/jpeg" | "image/webp" | "image/gif"
    ) {
        return Ok(AttachmentContent::Image(ContentBlock::Image {
            mime_type: mime,
            data: STANDARD.encode(bytes),
        }));
    }
    if matches!(mime.as_str(), "application/pdf" | "application/x-pdf")
        || (item.name.to_ascii_lowercase().ends_with(".pdf") && crate::pdf::has_header(&bytes))
    {
        let (text, pages) = crate::pdf::extract(&bytes)?;
        return Ok(AttachmentContent::Pdf { text, pages, path });
    }
    ensure!(!bytes.contains(&0), "unsupported binary attachment");
    Ok(AttachmentContent::Text(
        String::from_utf8(bytes).context("unsupported binary attachment")?,
    ))
}

pub fn attachment_payload(
    data: &Path,
    text: &str,
    items: &[Attachment],
) -> Result<(String, Vec<ContentBlock>)> {
    let mut message = text.to_owned();
    let mut images = vec![];
    for item in items {
        match content(data, item)? {
            AttachmentContent::Image(image) => images.push(image),
            AttachmentContent::Pdf { text, pages, path } => {
                message.push_str(&format!(
                    "\n\nAttached PDF {:?} ({} B, {pages} pages, path: {}):\n",
                    item.name,
                    item.size,
                    path.display()
                ));
                message.push_str(if text.is_empty() {
                    "[PDF contains no embedded text layer.]"
                } else {
                    &text
                });
            }
            AttachmentContent::Text(text) => {
                message.push_str(&format!("\n\nAttached file {:?}:\n{text}", item.name))
            }
        }
    }
    Ok((message, images))
}

pub(crate) fn user_message(
    data: &Path,
    text: &str,
    items: &[Attachment],
) -> Result<pi_ai::Message> {
    if items.is_empty() {
        return Ok(pi_ai::Message::user(text));
    }
    let mut blocks = Vec::new();
    if !text.is_empty() {
        blocks.push(ContentBlock::text(text));
    }
    for item in items {
        blocks.push(match content(data, item)? {
            AttachmentContent::Image(image) => image,
            AttachmentContent::Text(text) | AttachmentContent::Pdf { text, .. } => {
                ContentBlock::text(text)
            }
        });
    }
    Ok(pi_ai::Message::User(pi_ai::UserMessage {
        content: pi_ai::UserContent::Blocks(blocks),
        timestamp: chrono::Utc::now().timestamp_millis(),
    }))
}
