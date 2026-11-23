//! Bounded text extraction for Core PDF uploads (uncompressed and Flate streams).
use anyhow::{Result, ensure};
use std::collections::BTreeMap;

pub(super) fn has_header(bytes: &[u8]) -> bool {
    bytes[..bytes.len().min(1024)]
        .windows(5)
        .any(|w| w == b"%PDF-")
}
fn find(bytes: &[u8], needle: &[u8]) -> Option<usize> {
    bytes.windows(needle.len()).position(|w| w == needle)
}
#[cfg(unix)]
fn inflate(bytes: &[u8], limit: usize) -> Result<Vec<u8>> {
    use std::ffi::{c_int, c_ulong};
    #[link(name = "z")]
    unsafe extern "C" {
        fn uncompress(dst: *mut u8, len: *mut c_ulong, src: *const u8, n: c_ulong) -> c_int;
    }
    let mut size = bytes.len().max(1024).min(limit);
    loop {
        let mut out = vec![0; size];
        let mut len = size as c_ulong;
        let status = unsafe {
            uncompress(
                out.as_mut_ptr(),
                &mut len,
                bytes.as_ptr(),
                bytes.len() as c_ulong,
            )
        };
        if status == 0 {
            out.truncate(len as usize);
            return Ok(out);
        }
        ensure!(
            status == -5 && size < limit,
            "unsupported or oversized PDF stream"
        );
        size = (size * 2).min(limit);
    }
}
#[cfg(windows)]
fn inflate(bytes: &[u8], limit: usize) -> Result<Vec<u8>> {
    use std::{
        io::Write,
        os::windows::process::CommandExt,
        path::PathBuf,
        process::{Command, Stdio},
    };
    // Windows PowerShell ships with .NET DeflateStream. The program is fixed and the
    // binary input travels on stdin. Strip the RFC 1950 envelope for the raw inflater.
    ensure!(
        bytes.len() >= 6 && bytes[0] & 15 == 8 && bytes[1] & 32 == 0,
        "invalid PDF Flate stream"
    );
    let system = std::env::var_os("SystemRoot")
        .ok_or_else(|| anyhow::anyhow!("missing Windows system directory"))?;
    let script = format!(
        r#"$ErrorActionPreference = 'Stop'
$inputStream = [Console]::OpenStandardInput()
$decoder = New-Object IO.Compression.DeflateStream($inputStream, [IO.Compression.CompressionMode]::Decompress)
$outputStream = [Console]::OpenStandardOutput()
$buffer = New-Object byte[] 8192
$total = 0
while (($n = $decoder.Read($buffer, 0, $buffer.Length)) -gt 0) {{
    $total += $n
    if ($total -gt {limit}) {{ exit 1 }}
    $outputStream.Write($buffer, 0, $n)
}}
$decoder.Dispose()"#
    );
    let mut child =
        Command::new(PathBuf::from(system).join("System32/WindowsPowerShell/v1.0/powershell.exe"))
            .args([
                "-NoLogo",
                "-NoProfile",
                "-NonInteractive",
                "-Command",
                &script,
            ])
            .creation_flags(0x08000000)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()?;
    let mut stdin = child.stdin.take().unwrap();
    let output = std::thread::scope(|scope| -> Result<_> {
        let write = scope.spawn(move || stdin.write_all(&bytes[2..bytes.len() - 4]));
        let output = child.wait_with_output()?;
        write.join().unwrap()?;
        Ok(output)
    })?;
    ensure!(
        output.status.success() && output.stdout.len() <= limit,
        "invalid or oversized PDF stream"
    );
    Ok(output.stdout)
}

fn hex(bytes: &[u8]) -> Vec<u8> {
    let mut out = vec![];
    let mut high = None;
    for c in bytes.iter().filter(|b| !b.is_ascii_whitespace()) {
        let Some(d) = (*c as char).to_digit(16) else {
            return vec![];
        };
        if let Some(h) = high.take() {
            out.push(h | d as u8)
        } else {
            high = Some((d as u8) << 4)
        }
    }
    if let Some(h) = high {
        out.push(h);
    }
    out
}
fn unicode(bytes: &[u8]) -> String {
    String::from_utf16_lossy(
        &bytes
            .chunks_exact(2)
            .map(|b| u16::from_be_bytes([b[0], b[1]]))
            .collect::<Vec<_>>(),
    )
}
fn mappings(stream: &[u8], map: &mut BTreeMap<Vec<u8>, String>) {
    let mut mode = 0;
    for line in stream.split(|b| *b == b'\n') {
        if find(line, b"beginbfchar").is_some() {
            mode = 1;
            continue;
        }
        if find(line, b"beginbfrange").is_some() {
            mode = 2;
            continue;
        }
        if find(line, b"endbf").is_some() {
            mode = 0;
            continue;
        }
        if mode == 0 {
            continue;
        }
        let mut fields = vec![];
        let mut rest = line;
        while let Some(start) = find(rest, b"<") {
            rest = &rest[start + 1..];
            let Some(end) = find(rest, b">") else { break };
            fields.push(hex(&rest[..end]));
            rest = &rest[end + 1..];
        }
        if mode == 1 && fields.len() >= 2 && !fields[0].is_empty() {
            map.insert(fields[0].clone(), unicode(&fields[1]));
        }
        if mode == 2
            && fields.len() >= 3
            && (1..=4).contains(&fields[0].len())
            && fields[0].len() == fields[1].len()
        {
            let number = |b: &[u8]| b.iter().fold(0u32, |v, c| (v << 8) | (*c as u32));
            let start = number(&fields[0]);
            let stop = number(&fields[1]);
            if stop < start || stop - start > 65535 {
                continue;
            }
            for n in start..=stop {
                if map.len() >= 65536 {
                    return;
                }
                let i = (n - start) as usize;
                let value = if line.contains(&b'[') {
                    let Some(v) = fields.get(2 + i) else { break };
                    unicode(v)
                } else {
                    let Some(code) = number(&fields[2]).checked_add(i as u32) else {
                        break;
                    };
                    let encoded = code.to_be_bytes();
                    unicode(&encoded[4 - fields[2].len().min(4)..])
                };
                map.insert(n.to_be_bytes()[4 - fields[0].len()..].to_vec(), value);
            }
        }
        if map.len() >= 65536 {
            break;
        }
    }
}
fn decode(bytes: &[u8], map: &BTreeMap<Vec<u8>, String>) -> String {
    if bytes.starts_with(&[0xfe, 0xff]) {
        return unicode(&bytes[2..]);
    }
    if map.is_empty() {
        return String::from_utf8_lossy(bytes).into_owned();
    }
    let mut out = String::new();
    let mut rest = bytes;
    while !rest.is_empty() && out.len() <= 256 << 10 {
        if let Some((n, v)) = (1..=rest.len().min(4))
            .rev()
            .find_map(|n| map.get(&rest[..n]).map(|v| (n, v)))
        {
            out.push_str(v);
            rest = &rest[n..];
        } else {
            out.push(char::from(rest[0]));
            rest = &rest[1..];
        }
    }
    out
}
fn literal(data: &[u8]) -> (Vec<u8>, usize) {
    let mut out = vec![];
    let mut depth = 1;
    let mut i = 0;
    while i < data.len() {
        let c = data[i];
        i += 1;
        match c {
            b'(' => {
                depth += 1;
                out.push(c);
            }
            b')' => {
                depth -= 1;
                if depth == 0 {
                    break;
                }
                out.push(c);
            }
            b'\\' if i < data.len() => {
                let c = data[i];
                i += 1;
                match c {
                    b'0'..=b'7' => {
                        let mut n = c - b'0';
                        for _ in 0..2 {
                            if i == data.len() || !(b'0'..=b'7').contains(&data[i]) {
                                break;
                            }
                            n = n.wrapping_mul(8) + (data[i] - b'0');
                            i += 1;
                        }
                        out.push(n);
                    }
                    b'\r' => {
                        if data.get(i) == Some(&b'\n') {
                            i += 1;
                        }
                    }
                    b'\n' => {}
                    c => out.push(match c {
                        b'n' => b'\n',
                        b'r' => b'\r',
                        b't' => b'\t',
                        b'b' => 8,
                        b'f' => 12,
                        _ => c,
                    }),
                }
            }
            _ => out.push(c),
        }
    }
    (out, i)
}
pub(super) fn extract(data: &[u8]) -> Result<(String, usize)> {
    ensure!(has_header(data), "missing PDF header");
    let pages = data
        .split(|c| *c == b'/')
        .filter(|s| s.starts_with(b"Page") && s.get(4).is_none_or(|b| !b.is_ascii_alphanumeric()))
        .count();
    let mut streams = vec![];
    let mut rest = data;
    let mut total = 0;
    let mut cmap = BTreeMap::new();
    while let Some(start) = find(rest, b"stream") {
        let dict = &rest[start.saturating_sub(512)..start];
        let payload = &rest[start + 6..];
        let payload = payload.strip_prefix(b"\r").unwrap_or(payload);
        let payload = payload.strip_prefix(b"\n").unwrap_or(payload);
        let Some(end) = find(payload, b"endstream") else {
            break;
        };
        rest = &payload[end + 9..];
        let raw = &payload[..end];
        let stream = if find(dict, b"/FlateDecode").is_some() {
            inflate(raw, 16 << 20)?
        } else {
            raw.to_vec()
        };
        total += stream.len();
        ensure!(total <= 32 << 20, "PDF expanded streams too large");
        if find(&stream, b"beginbf").is_some() {
            mappings(&stream, &mut cmap);
        } else {
            streams.push(stream);
        }
    }
    let mut out = String::new();
    for stream in streams {
        let mut rest = &stream[..];
        while let Some(bt) = find(rest, b"BT") {
            rest = &rest[bt + 2..];
            let Some(et) = find(rest, b"ET") else { break };
            let mut block = &rest[..et];
            rest = &rest[et + 2..];
            while let Some(i) = block.iter().position(|c| matches!(c, b'(' | b'<')) {
                let c = block[i];
                block = &block[i + 1..];
                let (text, n) = if c == b'(' {
                    literal(block)
                } else {
                    let Some(end) = find(block, b">") else { break };
                    (hex(&block[..end]), end + 1)
                };
                block = &block[n..];
                out.push_str(&decode(&text, &cmap));
                if out.len() > 256 << 10 {
                    out.truncate(out.floor_char_boundary(256 << 10));
                    out.push_str("\n…[PDF text truncated]");
                    return Ok((out, pages));
                }
            }
            out.push('\n');
        }
    }
    Ok((out.trim().into(), pages))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn flate_stream_uses_the_document_unicode_map() -> Result<()> {
        let mut pdf=b"%PDF-1.5\n/Type /Page\nstream\n1 beginbfchar\n<01> <4E2D>\nendbfchar\n1 beginbfrange\n<02> <03> <0041>\nendbfrange\nendstream\n<< /Filter /FlateDecode >>\nstream\n".to_vec();
        pdf.extend([
            120, 156, 115, 10, 81, 176, 49, 48, 52, 48, 50, 48, 182, 83, 8, 201, 82, 112, 13, 1, 0,
            33, 223, 3, 238,
        ]);
        pdf.extend(b"\nendstream\n%%EOF");
        assert_eq!(extract(&pdf)?, ("中AB".into(), 1));
        assert!(extract(b"not a PDF").is_err());
        assert!(extract(b"%PDF-1.5\n/FlateDecode\nstream\nbad\nendstream").is_err());
        Ok(())
    }
}
