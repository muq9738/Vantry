use anyhow::{Result, ensure};
use clap::Args;
use image::{
    ImageEncoder, ImageFormat, Rgba, RgbaImage,
    codecs::png::{CompressionType, FilterType as PngFilterType, PngEncoder},
    imageops::{self, FilterType},
};
use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
};
#[derive(Debug, Args)]
pub struct Options {
    #[arg(long, default_value = "assets/branding/icon-original.png")]
    source: PathBuf,
    #[arg(long, default_value = "resources/icon.png")]
    out: PathBuf,
    /// Write the standard macOS iconset directory.
    #[arg(long)]
    iconset: Option<PathBuf>,
    /// Write a macOS icon container using PNG representations.
    #[arg(long)]
    icns: Option<PathBuf>,
}
/// Write a lossless PNG with the strongest encoder settings supported by the
/// bundled `image` version.  Packaging re-encodes the checked-in icon through
/// this path so the bundle does not inherit a fast, unnecessarily large PNG.
/// The source is already RGBA8, so this changes only the PNG representation,
/// never dimensions or pixel values.
fn encode_png(image: &RgbaImage) -> Result<Vec<u8>> {
    let mut data = Vec::new();
    PngEncoder::new_with_quality(&mut data, CompressionType::Best, PngFilterType::Adaptive)
        .write_image(
            image.as_raw(),
            image.width(),
            image.height(),
            image::ExtendedColorType::Rgba8,
        )?;
    Ok(data)
}

pub(crate) fn png(image: &RgbaImage, path: &Path) -> Result<()> {
    fs::create_dir_all(crate::parent(path))?;
    let mut tmp = tempfile::NamedTempFile::new_in(crate::parent(path))?;
    tmp.write_all(&encode_png(image)?)?;
    tmp.persist(path)?;
    Ok(())
}
fn remove_edges(img: &mut RgbaImage) -> usize {
    let n = img.width();
    let mut seen = vec![false; img.as_raw().len() / 4];
    let mut queue = Vec::new();
    let visit = |x: u32, y: u32, queue: &mut Vec<(u32, u32)>, seen: &mut [bool]| {
        let i = y as usize * n as usize + x as usize;
        let [r, g, b, _] = img.get_pixel(x, y).0;
        if !seen[i] && r <= 32 && g <= 32 && b <= 32 {
            seen[i] = true;
            queue.push((x, y));
        }
    };
    for i in 0..n {
        for (x, y) in [(0, i), (n - 1, i), (i, 0), (i, n - 1)] {
            visit(x, y, &mut queue, &mut seen);
        }
    }
    let mut head = 0;
    while head < queue.len() {
        let (x, y) = queue[head];
        head += 1;
        if x > 0 {
            visit(x - 1, y, &mut queue, &mut seen)
        }
        if x + 1 < n {
            visit(x + 1, y, &mut queue, &mut seen)
        }
        if y > 0 {
            visit(x, y - 1, &mut queue, &mut seen)
        }
        if y + 1 < n {
            visit(x, y + 1, &mut queue, &mut seen)
        }
    }
    for (x, y) in queue {
        img.put_pixel(x, y, Rgba([0; 4]));
    }
    head
}
// Catmull-Rom interpolation operates on premultiplied components (Go image/draw).
fn resize(img: &RgbaImage, n: u32) -> RgbaImage {
    if img.width() == n {
        return img.clone();
    }
    let linear = image::ImageBuffer::from_fn(img.width(), img.height(), |x, y| {
        let [r, g, b, a] = img.get_pixel(x, y).0;
        let alpha = f32::from(a) / 255.;
        Rgba([
            r as f32 / 255. * alpha,
            g as f32 / 255. * alpha,
            b as f32 / 255. * alpha,
            alpha,
        ])
    });
    let scaled = imageops::resize(&linear, n, n, FilterType::CatmullRom);
    RgbaImage::from_fn(n, n, |x, y| {
        let [r, g, b, a] = scaled.get_pixel(x, y).0;
        let byte = |v: f32| (255. * v.clamp(0., 1.)).round() as u8;
        if a <= 0. {
            Rgba([0; 4])
        } else {
            Rgba([byte(r / a), byte(g / a), byte(b / a), byte(a)])
        }
    })
}
pub(crate) fn icns(icon: &RgbaImage, path: &Path) -> Result<()> {
    let mut body = Vec::new();
    for (code, n) in [
        (b"icp4", 16),
        (b"icp5", 32),
        (b"icp6", 64),
        (b"ic07", 128),
        (b"ic08", 256),
        (b"ic09", 512),
        (b"ic10", 1024),
    ] {
        let data = encode_png(&resize(icon, n))?;
        body.extend(code);
        body.extend(((8 + data.len()) as u32).to_be_bytes());
        body.extend(data);
    }
    let dir = crate::parent(path);
    fs::create_dir_all(dir)?;
    let mut out = tempfile::NamedTempFile::new_in(dir)?;
    out.write_all(b"icns")?;
    out.write_all(&((body.len() + 8) as u32).to_be_bytes())?;
    out.write_all(&body)?;
    out.persist(path)?;
    Ok(())
}
pub fn run(opts: Options) -> Result<()> {
    let mut reader = image::ImageReader::open(opts.source)?;
    reader.set_format(ImageFormat::Png);
    let mut img = reader.decode()?.to_rgba8();
    let n = img.width();
    ensure!(
        n > 0 && n == img.height(),
        "icon artwork must be square: {n}x{}",
        img.height()
    );
    let count = remove_edges(&mut img);
    let icon = resize(&img, 1024);
    png(&icon, &opts.out)?;
    if let Some(dir) = opts.iconset {
        for size in [16, 32, 128, 256, 512] {
            for scale in [1, 2] {
                let suffix = if scale == 2 { "@2x" } else { "" };
                png(
                    &resize(&icon, size * scale),
                    &dir.join(format!("icon_{size}x{size}{suffix}.png")),
                )?;
            }
        }
    }
    if let Some(path) = opts.icns {
        icns(&icon, &path)?;
    }
    println!(
        "{}: {n}x{n} → 1024x1024; {count} edge background pixels made transparent",
        opts.out.display()
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compact_png_preserves_dimensions_and_pixels() -> Result<()> {
        let tmp = tempfile::tempdir()?;
        let path = tmp.path().join("icon.png");
        let image = RgbaImage::from_fn(7, 5, |x, y| {
            Rgba([
                (x * 31) as u8,
                (y * 47) as u8,
                (x * 11 + y * 13) as u8,
                if (x + y) % 3 == 0 { 0 } else { 255 },
            ])
        });

        png(&image, &path)?;
        let decoded = image::open(&path)?.to_rgba8();
        assert_eq!(decoded.dimensions(), image.dimensions());
        assert_eq!(decoded.as_raw(), image.as_raw());
        Ok(())
    }
}
