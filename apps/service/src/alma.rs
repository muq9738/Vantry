mod sharing;
pub use sharing::Sharing;

use anyhow::{Result, ensure};
use qrcodegen::{QrCode, QrCodeEcc};
use serde::{Deserialize, Serialize};
use std::{fmt::Write, io::ErrorKind, path::Path};
use vantry_client::CoreClient;
use vantry_connectivity::{
    alma::{Host, new_code},
    transport::relay_url,
};
use vantry_platform::hostfs;

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Pairing {
    relay: String,
    thread_id: String,
    id: String,
    code: String,
}

pub fn prepare(data: &Path, relay: &str, thread: &str, reset: bool) -> Result<Host> {
    let relay = relay_url(relay, false)?;
    ensure!(
        !thread.is_empty()
            && thread.len() <= 255
            && thread
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-._~".contains(&b)),
        "expected a Core thread ID"
    );
    let key = match hostfs::read_file(data, Path::new("alma-registration-key"), 16 * 1024) {
        Ok(bytes) => Some(String::from_utf8(bytes)?.trim().to_owned()).filter(|s| !s.is_empty()),
        Err(e) if e.kind() == ErrorKind::NotFound => None,
        Err(e) => return Err(e.into()),
    };
    let state = Path::new("alma-pairing.json");
    let saved: Option<Pairing> = match hostfs::read_file(data, state, 16 * 1024) {
        Ok(bytes) if !reset => Some(serde_json::from_slice(&bytes)?),
        Ok(_) => None,
        Err(e) if e.kind() == ErrorKind::NotFound => None,
        Err(e) => return Err(e.into()),
    };
    let (pairing, changed) = match saved {
        Some(p) if p.relay == relay.as_str() && p.thread_id == thread => {
            use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
            ensure!(
                URL_SAFE_NO_PAD.decode(&p.code)?.len() == 32
                    && uuid::Uuid::parse_str(&p.id).is_ok(),
                "Invalid saved Alma pairing"
            );
            (p, false)
        }
        _ => {
            let p = Pairing {
                relay: relay.to_string(),
                thread_id: thread.into(),
                id: uuid::Uuid::new_v4().to_string(),
                code: new_code()?,
            };
            (p, true)
        }
    };
    let link = pairing_link(&relay, &pairing.code);
    let qr = QrCode::encode_text(link.as_str(), QrCodeEcc::Medium)?;
    let size = qr.size() + 8;
    let mut svg = format!(
        r#"<svg xmlns="http://www.w3.org/2000/svg" width="420" height="420" viewBox="0 0 {size} {size}" shape-rendering="crispEdges"><rect width="100%" height="100%" fill="white"/><path d=""#
    );
    for y in 0..qr.size() {
        for x in 0..qr.size() {
            if qr.get_module(x, y) {
                write!(svg, "M{} {}h1v1h-1z", x + 4, y + 4)?;
            }
        }
    }
    svg.push_str(r#""/></svg>"#);
    if changed {
        hostfs::atomic_write(&data.join(state), &serde_json::to_vec(&pairing)?)?;
    }
    hostfs::atomic_write(&data.join("alma-pairing.svg"), svg.as_bytes())?;
    Host::new(
        relay,
        pairing.code,
        key,
        pairing.id,
        thread.into(),
        CoreClient::from_data_dir(data),
    )
}

fn pairing_link(endpoint: &url::Url, code: &str) -> url::Url {
    let mut link = url::Url::parse("alma://connect").expect("static Alma URL");
    link.query_pairs_mut()
        .append_pair("u", endpoint.as_str().trim_end_matches('/'))
        .append_pair("k", code)
        .append_pair("e2e", "1");
    link
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pairing_survives_restart_and_rotates_when_scope_changes() -> Result<()> {
        let temp = tempfile::tempdir()?;
        let data = temp.path().join("core");
        hostfs::ensure_private_dir(&data)?;
        let read = || -> Result<Pairing> {
            Ok(serde_json::from_slice(&hostfs::read_file(
                &data,
                Path::new("alma-pairing.json"),
                4096,
            )?)?)
        };
        let relay = "https://mobile.example.test";
        prepare(&data, relay, "thread_one", false)?;
        let first = read()?;
        prepare(&data, relay, "thread_one", false)?;
        assert!(read()?.code == first.code);
        prepare(&data, relay, "thread_two", false)?;
        let second = read()?;
        assert!(second.code != first.code && second.id != first.id);
        prepare(&data, relay, "thread_two", true)?;
        assert!(read()?.code != second.code);
        let reset = read()?;
        prepare(&data, "https://other.example.test", "thread_two", false)?;
        assert!(read()?.code != reset.code);
        assert!(prepare(&data, "http://mobile.example.test", "thread_two", false).is_err());
        Ok(())
    }
}
