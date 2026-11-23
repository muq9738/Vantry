use anyhow::{Context, Result, bail, ensure};
use std::path::Path;

/// Match the image formats accepted by the Go preview and bound decoded pixel count.
pub fn validate(path: &Path, data: &[u8]) -> Result<()> {
    if path
        .extension()
        .is_some_and(|v| v.eq_ignore_ascii_case("svg"))
    {
        return Ok(());
    }
    let (width, height) =
        if data.starts_with(b"\x89PNG\r\n\x1a\n") && data.get(12..16) == Some(b"IHDR") {
            let dimensions = data.get(16..24).context("invalid PNG header")?;
            (
                u32::from_be_bytes(dimensions[..4].try_into()?),
                u32::from_be_bytes(dimensions[4..].try_into()?),
            )
        } else if data.starts_with(b"GIF87a") || data.starts_with(b"GIF89a") {
            let size = data.get(6..10).context("invalid GIF header")?;
            (
                u16::from_le_bytes(size[..2].try_into()?).into(),
                u16::from_le_bytes(size[2..].try_into()?).into(),
            )
        } else if data.starts_with(&[255, 216]) {
            jpeg_size(&data[2..])?
        } else {
            bail!("unsupported image format")
        };
    ensure!(
        width > 0 && height > 0 && u64::from(width) * u64::from(height) <= 16_000_000,
        "invalid or oversized image dimensions"
    );
    Ok(())
}
fn jpeg_size(mut data: &[u8]) -> Result<(u32, u32)> {
    while data.starts_with(&[255]) {
        while data.first() == Some(&255) {
            data = &data[1..];
        }
        let (&marker, rest) = data.split_first().context("invalid JPEG marker")?;
        ensure!(
            !matches!(marker, 0 | 0xd9 | 0xda),
            "missing JPEG frame header"
        );
        if matches!(marker, 0x01 | 0xd0..=0xd8) {
            data = rest;
            continue;
        }
        let size =
            u16::from_be_bytes(rest.get(..2).context("invalid JPEG length")?.try_into()?) as usize;
        ensure!(size >= 2 && size <= rest.len(), "invalid JPEG segment");
        if matches!(marker,0xc0..=0xc3|0xc5..=0xc7|0xc9..=0xcb|0xcd..=0xcf) {
            let frame = rest[..size].get(3..7).context("invalid JPEG dimensions")?;
            return Ok((
                u16::from_be_bytes(frame[2..].try_into()?).into(),
                u16::from_be_bytes(frame[..2].try_into()?).into(),
            ));
        }
        data = &rest[size..];
    }
    bail!("missing JPEG frame header")
}
