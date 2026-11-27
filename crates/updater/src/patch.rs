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
const MAX_BSDIFF_SIZE: u64 = 128 << 20;

/// Encode a v1-compatible bsdiff patch. Uses full suffix-array bsdiff with
/// fuzzy forward/backward extension for files up to 128 MiB so moved functions
/// and shifted PC-relative offsets compress tightly while remaining 100%
/// decodable by existing `vantry-delta-1` clients.
pub(crate) fn encode(old: &mut File, new: &mut File, out: &mut File) -> Result<()> {
    let (a, b) = (old.metadata()?.len(), new.metadata()?.len());
    if a.max(b) <= MAX_BSDIFF_SIZE {
        return encode_bsdiff(old, new, out);
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
fn qsufsort_split(sa: &mut [i32], rank: &mut [i32], start: usize, len: usize, h: usize) {
    let mut work = vec![(start, len)];
    while let Some((start, len)) = work.pop() {
        if len < 16 {
            let mut k = start;
            while k < start + len {
                let mut j = 1usize;
                let mut x = rank[sa[k] as usize + h];
                let mut i = 1usize;
                while k + i < start + len {
                    let v = rank[sa[k + i] as usize + h];
                    if v < x {
                        x = v;
                        j = 0;
                    }
                    if v == x {
                        sa.swap(k + j, k + i);
                        j += 1;
                    }
                    i += 1;
                }
                for m in 0..j {
                    rank[sa[k + m] as usize] = (k + j - 1) as i32;
                }
                if j == 1 {
                    sa[k] = -1;
                }
                k += j;
            }
            continue;
        }

        let x = rank[sa[start + len / 2] as usize + h];
        let mut jj = 0usize;
        let mut kk = 0usize;
        for i in start..start + len {
            let v = rank[sa[i] as usize + h];
            if v < x {
                jj += 1;
            } else if v == x {
                kk += 1;
            }
        }
        jj += start;
        kk += jj;

        let mut i = start;
        let mut j = 0usize;
        let mut k = 0usize;
        while i < jj {
            let v = rank[sa[i] as usize + h];
            if v < x {
                i += 1;
            } else if v == x {
                sa.swap(i, jj + j);
                j += 1;
            } else {
                sa.swap(i, kk + k);
                k += 1;
            }
        }
        while jj + j < kk {
            if rank[sa[jj + j] as usize + h] == x {
                j += 1;
            } else {
                sa.swap(jj + j, kk + k);
                k += 1;
            }
        }

        if start + len > kk {
            work.push((kk, start + len - kk));
        }
        for m in 0..(kk - jj) {
            rank[sa[jj + m] as usize] = (kk - 1) as i32;
        }
        if kk == jj + 1 {
            sa[jj] = -1;
        }
        if jj > start {
            work.push((start, jj - start));
        }
    }
}

/// Larsson-Sadakane suffix sort (`qsufsort`) producing the suffix array `sa[0..=old.len()]`.
fn qsufsort(old: &[u8]) -> Vec<i32> {
    let n = old.len();
    let mut sa = vec![0i32; n + 1];
    let mut rank = vec![0i32; n + 1];
    let mut buckets = [0i32; 256];

    for &b in old {
        buckets[b as usize] += 1;
    }
    for c in 1..256 {
        buckets[c] += buckets[c - 1];
    }
    for c in (1..256).rev() {
        buckets[c] = buckets[c - 1];
    }
    buckets[0] = 0;

    for (idx, &b) in old.iter().enumerate() {
        let c = b as usize;
        buckets[c] += 1;
        sa[buckets[c] as usize] = idx as i32;
    }
    sa[0] = n as i32;

    for (idx, &b) in old.iter().enumerate() {
        rank[idx] = buckets[b as usize];
    }
    rank[n] = 0;

    for c in 1..256 {
        if buckets[c] == buckets[c - 1] + 1 {
            sa[buckets[c] as usize] = -1;
        }
    }
    sa[0] = -1;

    let mut h = 1usize;
    while sa[0] != -(n as i32 + 1) {
        let mut len: i32 = 0;
        let mut i = 0usize;
        while i < n + 1 {
            if sa[i] < 0 {
                len -= sa[i];
                i = (i as i32 - sa[i]) as usize;
            } else {
                if len != 0 {
                    sa[i - len as usize] = -len;
                }
                let group_len = rank[sa[i] as usize] + 1 - i as i32;
                qsufsort_split(&mut sa, &mut rank, i, group_len as usize, h);
                i += group_len as usize;
                len = 0;
            }
        }
        if len != 0 {
            sa[i - len as usize] = -len;
        }
        h += h;
    }

    for i in 0..=n {
        sa[rank[i] as usize] = i as i32;
    }
    sa
}

#[inline]
fn matchlen(old: &[u8], new: &[u8]) -> usize {
    let limit = old.len().min(new.len());
    let mut i = 0usize;
    while i + 8 <= limit && old[i..i + 8] == new[i..i + 8] {
        i += 8;
    }
    while i < limit && old[i] == new[i] {
        i += 1;
    }
    i
}

#[inline]
fn search_suffix(sa: &[i32], old: &[u8], new: &[u8]) -> (usize, usize) {
    let mut st = 0usize;
    let mut en = old.len();
    while en - st >= 2 {
        let mid = st + (en - st) / 2;
        let pos = sa[mid] as usize;
        let cmp_len = (old.len() - pos).min(new.len());
        if old[pos..pos + cmp_len] < new[..cmp_len] {
            st = mid;
        } else {
            en = mid;
        }
    }
    let pos_st = sa[st] as usize;
    let pos_en = sa[en] as usize;
    let x = matchlen(&old[pos_st..], new);
    let y = matchlen(&old[pos_en..], new);
    if x > y { (x, pos_st) } else { (y, pos_en) }
}

fn encode_bsdiff(old: &mut File, new: &mut File, out: &mut File) -> Result<()> {
    let (mut base, mut target) = (Vec::new(), Vec::new());
    old.rewind()?;
    new.rewind()?;
    old.read_to_end(&mut base)?;
    new.read_to_end(&mut target)?;

    let oldsize = base.len();
    let newsize = target.len();
    let sa = qsufsort(&base);

    let mut control = Vec::new();
    let mut diffs = DeflateEncoder::new(Vec::new(), Compression::best());
    let mut literals = DeflateEncoder::new(Vec::new(), Compression::best());
    let mut diff_buf = [0u8; CHUNK];
    let mut blocks = 0u64;

    let mut scan = 0usize;
    let mut len = 0usize;
    let mut pos = 0usize;
    let mut lastscan = 0usize;
    let mut lastpos = 0usize;
    let mut lastoffset: isize = 0;

    while scan < newsize {
        let mut oldscore = 0usize;
        scan += len;
        let mut scsc = scan;
        while scan < newsize {
            let (m_len, m_pos) = search_suffix(&sa, &base, &target[scan..]);
            len = m_len;
            pos = m_pos;
            while scsc < scan + len {
                let old_idx = scsc as isize + lastoffset;
                if old_idx >= 0
                    && (old_idx as usize) < oldsize
                    && base[old_idx as usize] == target[scsc]
                {
                    oldscore += 1;
                }
                scsc += 1;
            }
            if (len == oldscore && len != 0) || len > oldscore + 8 {
                break;
            }
            let old_idx = scan as isize + lastoffset;
            if old_idx >= 0
                && (old_idx as usize) < oldsize
                && base[old_idx as usize] == target[scan]
            {
                oldscore -= 1;
            }
            scan += 1;
        }

        if len != oldscore || scan == newsize {
            let mut s: isize = 0;
            let mut sf: isize = 0;
            let mut lenf = 0usize;
            let mut i = 0usize;
            while lastscan + i < scan && lastpos + i < oldsize {
                if base[lastpos + i] == target[lastscan + i] {
                    s += 1;
                }
                i += 1;
                if s * 2 - i as isize > sf * 2 - lenf as isize {
                    sf = s;
                    lenf = i;
                }
            }

            let mut lenb = 0usize;
            if scan < newsize {
                let mut s: isize = 0;
                let mut sb: isize = 0;
                let mut i = 1usize;
                while scan >= lastscan + i && pos >= i {
                    if base[pos - i] == target[scan - i] {
                        s += 1;
                    }
                    if s * 2 - i as isize > sb * 2 - lenb as isize {
                        sb = s;
                        lenb = i;
                    }
                    i += 1;
                }
            }

            if lastscan + lenf > scan - lenb {
                let overlap = (lastscan + lenf) - (scan - lenb);
                let mut s: isize = 0;
                let mut ss: isize = 0;
                let mut lens = 0usize;
                for i in 0..overlap {
                    if target[lastscan + lenf - overlap + i]
                        == base[lastpos + lenf - overlap + i]
                    {
                        s += 1;
                    }
                    if target[scan - lenb + i] == base[pos - lenb + i] {
                        s -= 1;
                    }
                    if s > ss {
                        ss = s;
                        lens = i + 1;
                    }
                }
                lenf += lens;
                lenf -= overlap;
                lenb -= lens;
            }

            let extra_start = lastscan + lenf;
            let extra_end = scan - lenb;
            let extra_len = extra_end - extra_start;
            let seek = (pos - lenb) as i64 - (lastpos + lenf) as i64;

            put(&mut control, lenf as u64)?;
            put(&mut control, extra_len as u64)?;
            put(&mut control, ((seek << 1) ^ (seek >> 63)) as u64)?;

            let mut written = 0usize;
            while written < lenf {
                let n = (lenf - written).min(CHUNK);
                for j in 0..n {
                    diff_buf[j] =
                        target[lastscan + written + j].wrapping_sub(base[lastpos + written + j]);
                }
                diffs.write_all(&diff_buf[..n])?;
                written += n;
            }
            if extra_len > 0 {
                literals.write_all(&target[extra_start..extra_end])?;
            }
            blocks += 1;

            lastscan = scan - lenb;
            lastpos = pos - lenb;
            lastoffset = pos as isize - scan as isize;
        }
    }

    let ctrl = compress(&control)?;
    let diff = diffs.finish()?;
    let extra = literals.finish()?;
    put(&mut *out, blocks)?;
    put(&mut *out, ctrl.len() as u64)?;
    put(&mut *out, diff.len() as u64)?;
    out.write_all(&ctrl)?;
    out.write_all(&diff)?;
    out.write_all(&extra)?;
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
    // Cache base file in RAM for files <= 128 MiB to avoid tens of thousands of seek+read syscalls
    // when applying fine-grained bsdiff patches.
    let old_mem = if old_size >= 0 && (old_size as u64) <= MAX_BSDIFF_SIZE {
        old.rewind()?;
        let mut data = Vec::with_capacity(old_size as usize);
        old.read_to_end(&mut data)?;
        Some(data)
    } else {
        None
    };
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
                let src_slice: &[u8] = if let Some(mem) = old_mem.as_deref() {
                    &mem[lo as usize..hi as usize]
                } else {
                    old.seek(SeekFrom::Start(lo as u64))?;
                    old.read_exact(&mut prev[..count])?;
                    &prev[..count]
                };
                for (dst, src) in buf[(lo - pos) as usize..].iter_mut().zip(src_slice) {
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
