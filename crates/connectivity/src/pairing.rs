use anyhow::{Result, bail, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Duration, Utc};
use rand::{RngCore, rngs::OsRng};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::HashMap;

pub fn token() -> String {
    let mut bytes = [0; 32];
    OsRng.fill_bytes(&mut bytes);
    URL_SAFE_NO_PAD.encode(bytes)
}
pub(crate) fn matches_secret(a: &str, b: &str) -> bool {
    #[allow(deprecated)]
    ring::constant_time::verify_slices_are_equal(a.as_bytes(), b.as_bytes()).is_ok()
}
pub fn normalize(code: &str) -> String {
    code.trim().replace(' ', "").to_ascii_uppercase()
}
fn code(scope: &str) -> String {
    const ALPHABET: &[u8] = b"23456789ABCDEFGHJKLMNPQRSTUVWXYZ";
    let mut bytes = [0; 12];
    OsRng.fill_bytes(&mut bytes);
    let mut result = if scope == "desktop" {
        "DESK"
    } else {
        "SESSION"
    }
    .to_owned();
    for chunk in bytes.chunks(4) {
        result.push('-');
        for b in chunk {
            result.push(ALPHABET[*b as usize % ALPHABET.len()] as char);
        }
    }
    result
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Pairing {
    pub id: String,
    pub code: String,
    pub scope: String,
    pub name: String,
    pub expires_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub payload: Option<Value>,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub paired_with: String,
    #[serde(skip)]
    owner: String,
}
#[derive(Default)]
pub struct Pairings(HashMap<String, Pairing>);
impl Pairings {
    fn prune(&mut self) {
        let cutoff = Utc::now() - Duration::minutes(5);
        self.0.retain(|_, e| e.expires_at > cutoff);
    }
    pub fn issue(
        &mut self,
        scope: &str,
        name: &str,
        payload: Value,
        expires: Option<DateTime<Utc>>,
        owner: &str,
    ) -> Result<Pairing> {
        ensure!(
            matches!(scope, "session" | "desktop"),
            "invalid pairing scope"
        );
        ensure!(
            serde_json::to_vec(&payload)?.len() <= 16 * 1024,
            "pairing payload too large"
        );
        self.prune();
        ensure!(self.0.len() < 1024, "pairing capacity reached");
        let now = Utc::now();
        let expiry = expires
            .filter(|e| *e > now && *e <= now + Duration::minutes(10))
            .unwrap_or(now + Duration::minutes(5));
        let mut entry = Pairing {
            id: token(),
            code: code(scope),
            scope: scope.into(),
            name: name.into(),
            expires_at: expiry,
            payload: Some(payload),
            status: "waiting".into(),
            paired_with: String::new(),
            owner: owner.into(),
        };
        self.0.insert(entry.code.clone(), entry.clone());
        entry.payload = None;
        Ok(entry)
    }
    pub fn redeem(&mut self, scope: &str, code: &str, name: &str) -> Result<Pairing> {
        self.prune();
        let Some(e) = self.0.get_mut(&normalize(code)) else {
            bail!("invalid or expired pairing code")
        };
        ensure!(
            e.status == "waiting" && e.expires_at > Utc::now() && e.scope == scope,
            "invalid or expired pairing code"
        );
        let result = e.clone();
        e.status = "paired".into();
        e.payload = None;
        e.paired_with = short_label(name);
        Ok(result)
    }
    pub fn info(&mut self, id: &str, owner: &str) -> Option<Pairing> {
        self.prune();
        let mut e = self
            .0
            .values()
            .find(|e| e.id == id && e.owner == owner)?
            .clone();
        e.payload = None;
        if e.status == "waiting" && e.expires_at <= Utc::now() {
            e.status = "expired".into();
        }
        Some(e)
    }
    pub fn revoke(&mut self, id: &str, owner: &str) -> bool {
        if let Some(e) = self.0.values_mut().find(|e| e.id == id && e.owner == owner) {
            e.payload = None;
            e.status = "revoked".into();
            true
        } else {
            false
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn expired_revoked_and_wrong_scope_entries_never_release_payloads() -> Result<()> {
        let mut codes = Pairings::default();
        let issue = |codes: &mut Pairings| {
            codes.issue("session", "host", serde_json::json!({"v":2}), None, "host")
        };
        let expired = issue(&mut codes)?;
        codes.0.get_mut(&expired.code).unwrap().expires_at = Utc::now() - Duration::seconds(1);
        assert_eq!(codes.info(&expired.id, "host").unwrap().status, "expired");
        assert!(codes.redeem("session", &expired.code, "").is_err());
        let revoked = issue(&mut codes)?;
        assert!(!codes.revoke(&revoked.id, "other"));
        assert!(codes.redeem("desktop", &revoked.code, "").is_err());
        assert!(codes.revoke(&revoked.id, "host"));
        assert!(codes.redeem("session", &revoked.code, "").is_err());
        let valid = issue(&mut codes)?;
        assert_eq!(
            codes
                .redeem("session", &valid.code.to_ascii_lowercase(), "guest")?
                .payload
                .unwrap()["v"],
            2
        );
        Ok(())
    }
}
pub(crate) fn short_label(s: &str) -> String {
    s[..s.floor_char_boundary(128.min(s.len()))].to_string()
}
