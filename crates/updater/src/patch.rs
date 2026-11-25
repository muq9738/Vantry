//! The v1 bsdiff variant uses unsigned LEB128 lengths, zig-zag seeks, and
//! three independent raw DEFLATE streams (control, byte differences, literals).
use crate::inflate::Inflater as DeflateDecoder;
use anyhow::{Result, ensure};
use flate2::{Compression, write::DeflateEncoder};
use std::{
    cell::RefCell,
    fs::File,
    io::{self, BufReader, Read, Seek, SeekFrom, Write},
};

pub(crate) fn uint(mut r: impl Read) -> Result<u64> {
    let mut n = 0;
    for shift in (0..64).step_by(7) {
        let mut b = [0];
        r.read_exact(&mut b)?;
        ensure!(shift != 63 || b[0] <= 1, "varint overflow");
        n |= ((b[0] & 127) as u64) << shift;
        if b[0] < 128 {
            return Ok(n);
        }
    }
    unreachable!()
}
pub(crate) fn put(mut w: impl Write, mut n: u64) -> io::Result<()> {
    while n >= 128 {
        w.write_all(&[(n as u8) | 128])?;
        n >>= 7;
    }
    w.write_all(&[n as u8])
}
pub(crate) fn compress(data: &[u8]) -> Result<Vec<u8>> {
    let mut enc = DeflateEncoder::new(Vec::new(), Compression::best());
    enc.write_all(data)?;
    Ok(enc.finish()?)
}
const CHUNK: usize = 32 * 1024;
/// Match a shifted suffix and encode the aligned prefix as byte differences.
/// This produces ordinary bsdiff control triples without a whole-file suffix array.
pub(crate) fn encode(old: &mut File, new: &mut File, out: &mut File) -> Result<()> {
    let (a, b) = (old.metadata()?.len(), new.metadata()?.len());
    // Release binaries move functions and sections between builds. Re-align at
    // matching byte runs before encoding differences in the existing v1 format.
    if a.max(b) <= 64 << 20 {
        return encode_indexed(old, new, out);
    }
    let mut suffix = 0;
    let (mut left, mut right) = ([0; CHUNK], [0; CHUNK]);
    if a != b {
        while suffix < a.min(b) {
            let n = (a.min(b) - suffix).min(CHUNK as u64);
            old.seek(SeekFrom::Start(a - suffix - n))?;
            new.seek(SeekFrom::Start(b - suffix - n))?;
            let (x, y) = (&mut left[..n as usize], &mut right[..n as usize]);
            old.read_exact(x)?;
            new.read_exact(y)?;
            let matched = x
                .iter()
                .rev()
                .zip(y.iter().rev())
                .take_while(|(l, r)| l == r)
                .count() as u64;
            suffix += matched;
            if matched != n {
                break;
            }
        }
    }
    let prefix = a.min(b) - suffix;
    let extra = b - suffix - prefix;
    let mut ctl = Vec::new();
    put(&mut ctl, prefix)?;
    put(&mut ctl, extra)?;
    put(&mut ctl, (a - suffix - prefix) << 1)?;
    if suffix > 0 {
        put(&mut ctl, suffix)?;
        put(&mut ctl, 0)?;
        put(&mut ctl, 0)?;
    }
    let ctl = compress(&ctl)?;
    let mut diff = DeflateEncoder::new(tempfile::tempfile()?, Compression::best());
    old.rewind()?;
    new.rewind()?;
    let mut remaining = prefix;
    while remaining > 0 {
        let n = remaining.min(CHUNK as u64) as usize;
        old.read_exact(&mut left[..n])?;
        new.read_exact(&mut right[..n])?;
        for i in 0..n {
            right[i] = right[i].wrapping_sub(left[i]);
        }
        diff.write_all(&right[..n])?;
        remaining -= n as u64;
    }
    left.fill(0);
    remaining = suffix;
    while remaining > 0 {
        let n = remaining.min(CHUNK as u64) as usize;
        diff.write_all(&left[..n])?;
        remaining -= n as u64;
    }
    let mut diff = diff.finish()?;
    put(&mut *out, if suffix > 0 { 2 } else { 1 })?;
    put(&mut *out, ctl.len() as u64)?;
    put(&mut *out, diff.stream_position()?)?;
    out.write_all(&ctl)?;
    diff.rewind()?;
    io::copy(&mut diff, out)?;
    let mut literals = DeflateEncoder::new(out, Compression::best());
    ensure!(
        io::copy(&mut new.take(extra), &mut literals)? == extra,
        "file changed while generating patch"
    );
    literals.finish()?;
    Ok(())
}
fn encode_indexed(old: &mut File, new: &mut File, out: &mut File) -> Result<()> {
    use std::collections::HashMap;
    let (mut base, mut target) = (Vec::new(), Vec::new());
    old.rewind()?;
    new.rewind()?;
    old.read_to_end(&mut base)?;
    new.read_to_end(&mut target)?;
    const ANCHOR: usize = 32;
    let key = |s: &[u8]| u64::from_le_bytes(s[..8].try_into().unwrap());
    let mut index = HashMap::new();
    for (i, chunk) in base.chunks_exact(64).enumerate() {
        index.entry(key(chunk)).or_insert(i * 64);
    }
    let mut control = Vec::new();
    let mut diffs = DeflateEncoder::new(Vec::new(), Compression::best());
    let mut literals = DeflateEncoder::new(Vec::new(), Compression::best());
    let (mut start, mut pos, mut cursor, mut blocks) = (0, 0, 0, 0);
    loop {
        let span = cursor - start;
        let aligned = pos + span;
        let remaining = &target[cursor..];
        if remaining.len() >= ANCHOR
            && base.get(aligned..aligned + ANCHOR) == Some(&remaining[..ANCHOR])
        {
            cursor += ANCHOR;
            continue;
        }
        let next = if remaining.is_empty() {
            pos
        } else {
            let found = remaining.get(..ANCHOR).and_then(|anchor| {
                let &at = index.get(&key(anchor))?;
                (base[at..at + ANCHOR] == *anchor).then_some(at)
            });
            let Some(found) = found else {
                cursor += 1;
                continue;
            };
            found
        };
        let n = span.min(base.len() - pos);
        put(&mut control, n as u64)?;
        put(&mut control, (span - n) as u64)?;
        let seek = next as i64 - (pos + n) as i64;
        put(&mut control, ((seek << 1) ^ (seek >> 63)) as u64)?;
        for (i, chunk) in target[start..start + n].chunks(CHUNK).enumerate() {
            let mut buf = [0u8; CHUNK];
            for (j, &byte) in chunk.iter().enumerate() {
                buf[j] = byte.wrapping_sub(base[pos + i * CHUNK + j]);
            }
            diffs.write_all(&buf[..chunk.len()])?;
        }
        literals.write_all(&target[start + n..cursor])?;
        blocks += 1;
        if remaining.is_empty() {
            break;
        }
        start = cursor;
        pos = next;
        cursor += ANCHOR;
    }
    let ctrl = compress(&control)?;
    let diff = diffs.finish()?;
    put(&mut *out, blocks)?;
    put(&mut *out, ctrl.len() as u64)?;
    put(&mut *out, diff.len() as u64)?;
    out.write_all(&ctrl)?;
    out.write_all(&diff)?;
    out.write_all(&literals.finish()?)?;
    Ok(())
}

struct Section<'a, R> {
    reader: &'a RefCell<R>,
    offset: u64,
    left: u64,
}
impl<R: Read + Seek> Read for Section<'_, R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let n = self.left.min(buf.len() as u64) as usize;
        if n == 0 {
            return Ok(0);
        }
        let mut r = self.reader.borrow_mut();
        r.seek(SeekFrom::Start(self.offset))?;
        let got = r.read(&mut buf[..n])?;
        self.left -= got as u64;
        self.offset += got as u64;
        Ok(got)
    }
}
pub(crate) fn decode(
    mut r: impl Read + Seek,
    len: u64,
    old: &mut File,
    size: u64,
    out: &mut impl Write,
) -> Result<()> {
    let start = r.stream_position()?;
    let (blocks, ctrl, diff) = (uint(&mut r)?, uint(&mut r)?, uint(&mut r)?);
    let base = r.stream_position()?;
    let end = start + len;
    ensure!(
        base <= end && blocks <= size + 1 && ctrl <= end - base && diff <= end - base - ctrl,
        "invalid patch header"
    );
    let source = RefCell::new(r);
    let stream = |offset, left| {
        DeflateDecoder::new(Section {
            reader: &source,
            offset,
            left,
        })
    };
    let mut control = BufReader::new(stream(base, ctrl));
    let mut diffs = stream(base + ctrl, diff);
    let mut extras = stream(base + ctrl + diff, end - base - ctrl - diff);
    let mut pos = 0i64;
    let mut remaining = size;
    let old_size = i64::try_from(old.metadata()?.len())?;
    let (mut buf, mut prev) = ([0u8; CHUNK], [0; CHUNK]);
    for _ in 0..blocks {
        let (mut d, e, z) = (
            uint(&mut control)?,
            uint(&mut control)?,
            uint(&mut control)?,
        );
        let seek = ((z >> 1) as i64) ^ -((z & 1) as i64);
        ensure!(
            d <= remaining && e <= remaining - d,
            "invalid patch lengths"
        );
        remaining -= d + e;
        while d > 0 {
            let n = d.min(CHUNK as u64) as usize;
            diffs.read_exact(&mut buf[..n])?;
            let lo = pos.max(0);
            let hi = (pos + n as i64).min(old_size);
            if lo < hi {
                let count = (hi - lo) as usize;
                old.seek(SeekFrom::Start(lo as u64))?;
                old.read_exact(&mut prev[..count])?;
                for (dst, src) in buf[(lo - pos) as usize..].iter_mut().zip(&prev[..count]) {
                    *dst = dst.wrapping_add(*src);
                }
            }
            out.write_all(&buf[..n])?;
            pos += n as i64;
            d -= n as u64;
        }
        ensure!(
            io::copy(&mut (&mut extras).take(e), out)? == e,
            "truncated patch literals"
        );
        pos = pos
            .checked_add(seek)
            .filter(|p| p.unsigned_abs() <= 1 << 40)
            .ok_or_else(|| anyhow::anyhow!("invalid patch seek"))?;
    }
    ensure!(
        remaining == 0
            && control.read(&mut [0])? == 0
            && diffs.read(&mut [0])? == 0
            && extras.read(&mut [0])? == 0,
        "invalid patch stream length"
    );
    Ok(())
}
