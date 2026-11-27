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
        } else if data.len() >= 12 && &data[0..4] == b"RIFF" && &data[8..12] == b"WEBP" {
            webp_size(data)?
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
fn webp_size(data: &[u8]) -> Result<(u32, u32)> {
    let chunk = data.get(12..16).context("invalid WebP chunk")?;
    match chunk {
        b"VP8X" => {
            let b = data.get(24..30).context("invalid WebP VP8X header")?;
            let w = 1 + u32::from_le_bytes([b[0], b[1], b[2], 0]);
            let h = 1 + u32::from_le_bytes([b[3], b[4], b[5], 0]);
            Ok((w, h))
        }
        b"VP8L" => {
            let b = data.get(21..25).context("invalid WebP VP8L header")?;
            let bits = u32::from_le_bytes([b[0], b[1], b[2], b[3]]);
            Ok((1 + (bits & 0x3FFF), 1 + ((bits >> 14) & 0x3FFF)))
        }
        b"VP8 " => {
            let b = data.get(26..30).context("invalid WebP VP8 header")?;
            let w = u32::from(u16::from_le_bytes([b[0], b[1]]) & 0x3FFF);
            let h = u32::from(u16::from_le_bytes([b[2], b[3]]) & 0x3FFF);
            Ok((w, h))
        }
        _ => bail!("unsupported WebP chunk"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validates_png_gif_webp_and_svg() {
        let png = [
            0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48,
            0x44, 0x52, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x10,
        ];
        assert!(validate(Path::new("a.png"), &png).is_ok());

        let gif = b"GIF89a\x02\x00\x03\x00";
        assert!(validate(Path::new("a.gif"), gif).is_ok());

        // VP8X WebP with width = 200 (199 = 0xC7), height = 100 (99 = 0x63)
        let mut webp = vec![0u8; 30];
        webp[0..4].copy_from_slice(b"RIFF");
        webp[8..12].copy_from_slice(b"WEBP");
        webp[12..16].copy_from_slice(b"VP8X");
        webp[24..27].copy_from_slice(&[199, 0, 0]);
        webp[27..30].copy_from_slice(&[99, 0, 0]);
        assert!(validate(Path::new("a.webp"), &webp).is_ok());

        assert!(validate(Path::new("a.svg"), b"<svg/>").is_ok());
        assert!(validate(Path::new("a.txt"), b"hello").is_err());
    }
}
