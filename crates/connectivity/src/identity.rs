use crate::{
    pairing::{Pairing, Pairings, matches_secret, short_label, token},
    transport::relay_url,
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, NaiveDateTime, TimeZone, Utc};
use rcgen::{
    CertificateParams, DistinguishedName, DnType, ExtendedKeyUsagePurpose, KeyPair,
    PKCS_ECDSA_P256_SHA256, PKCS_ED25519,
};
use rustls::{
    ClientConfig, DigitallySignedStruct, RootCertStore, ServerConfig, SignatureScheme,
    client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier},
    pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime},
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    fs::{self, File, OpenOptions},
    io::{ErrorKind, Write},
    path::{Path, PathBuf},
    sync::{Arc, LazyLock, Mutex, Weak},
    time::SystemTime,
};
use tokio_util::sync::CancellationToken;

#[derive(Default, Clone, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Pair {
    pub v: u8,
    pub name: String,
    pub relay: String,
    pub route: String,
    pub secret: String,
    pub certificate: String,
    pub device_id: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub relays: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub direct: Vec<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub next_certificate: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub t3_url: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub t3_token: String,
}
impl Pair {
    pub fn link(&self) -> String {
        format!(
            "vantry://connect#{}",
            URL_SAFE_NO_PAD.encode(serde_json::to_vec(self).unwrap())
        )
    }
    pub fn parse(link: &str, private: bool) -> Result<Self> {
        let raw = link
            .trim()
            .strip_prefix("vantry://connect#")
            .ok_or_else(|| anyhow!("invalid pairing link"))?;
        ensure!(raw.len() <= 16 << 10, "pairing link too long");
        let p: Self = serde_json::from_slice(
            &URL_SAFE_NO_PAD
                .decode(raw)
                .map_err(|_| anyhow!("invalid pairing link"))?,
        )
        .map_err(|_| anyhow!("invalid pairing link"))?;
        p.validate(private)?;
        Ok(p)
    }
    pub fn validate(&self, private: bool) -> Result<()> {
        if self.v == 3 {
            ensure!(
                self.t3_token.len() >= 16
                    && !self.device_id.is_empty()
                    && relay_url(&self.t3_url, private)?.path() == "/",
                "invalid T3 pairing"
            );
            return Ok(());
        }
        ensure!(
            self.route.len() >= 32
                && match self.v {
                    1 => self.secret.len() >= 32,
                    2 => !self.code.is_empty() && self.expires_at.is_some_and(|t| t > Utc::now()),
                    _ => false,
                },
            "invalid or expired pairing"
        );
        ensure!(
            !self.relay.is_empty() || !self.relays.is_empty() || !self.direct.is_empty(),
            "missing peer address"
        );
        for s in self
            .relays
            .iter()
            .chain(std::iter::once(&self.relay))
            .filter(|s| !s.is_empty())
        {
            relay_url(s, private)?;
        }
        for s in &self.direct {
            let a = s
                .parse::<http::uri::Authority>()
                .map_err(|_| anyhow!("invalid direct address"))?;
            ensure!(
                a.port_u16().is_some() && !a.host().is_empty() && !s.contains('@'),
                "invalid direct address"
            );
        }
        validate_cert(
            &cert(&self.certificate)?,
            &ServerName::try_from("vantry.remote")?,
            UnixTime::now(),
        )?;
        Ok(())
    }
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Peer {
    pub id: String,
    pub pair: Pair,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AccessClient {
    pub id: String,
    pub name: String,
    pub created_at: DateTime<Utc>,
    #[serde(default)]
    pub secret: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub certificate: String,
}
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RelayEndpoint {
    pub url: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub registration_key: String,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RevokedClient {
    pub id: String,
    pub name: String,
    pub revoked_at: DateTime<Utc>,
}
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Configuration {
    pub enabled: bool,
    pub host: Pair,
    pub private_key: String,
    pub mobile_certificate: String,
    pub mobile_private_key: String,
    pub next_certificate: String,
    pub next_private_key: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub registration_key: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub relays: Vec<RelayEndpoint>,
    pub peers: Vec<Peer>,
    pub clients: Vec<AccessClient>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub revocations: Vec<RevokedClient>,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, Value>,
}
fn cert(pem: &str) -> Result<CertificateDer<'static>> {
    rustls_pemfile::certs(&mut pem.as_bytes())
        .next()
        .transpose()?
        .ok_or_else(|| anyhow!("missing peer certificate"))
}
fn private(pem: &str) -> Result<PrivateKeyDer<'static>> {
    rustls_pemfile::private_key(&mut pem.as_bytes())?.ok_or_else(|| anyhow!("missing private key"))
}
fn generate(mobile: bool) -> Result<(String, String)> {
    let key = KeyPair::generate_for(if mobile {
        &PKCS_ECDSA_P256_SHA256
    } else {
        &PKCS_ED25519
    })?;
    let mut p = CertificateParams::new(vec![
        if mobile {
            "vantry.mobile"
        } else {
            "vantry.remote"
        }
        .into(),
    ])?;
    let mut serial = [0; 15];
    rand::RngCore::fill_bytes(&mut rand::rngs::OsRng, &mut serial);
    p.serial_number = Some(rcgen::SerialNumber::from_slice(&serial));
    let now = SystemTime::now();
    p.not_before = (now - std::time::Duration::from_secs(60)).into();
    p.not_after = (now + std::time::Duration::from_secs(365 * 86400)).into();
    p.distinguished_name = DistinguishedName::new();
    p.distinguished_name.push(
        DnType::CommonName,
        if mobile {
            "Vantry mobile access"
        } else {
            "Vantry remote access"
        },
    );
    p.extended_key_usages = vec![
        ExtendedKeyUsagePurpose::ServerAuth,
        ExtendedKeyUsagePurpose::ClientAuth,
    ];
    Ok((p.self_signed(&key)?.pem(), key.serialize_pem()))
}
// The Go identity is a self-signed CA used directly as the TLS end entity.
// Verify the exact paired certificate, hostname, validity and handshake signature.
fn tlv<'a>(data: &mut &'a [u8]) -> Option<(u8, &'a [u8])> {
    let (&tag, r) = data.split_first()?;
    let (&len, mut r) = r.split_first()?;
    let mut n = len as usize;
    if len >= 128 {
        let size = (len & 127) as usize;
        if size == 0 || size > 4 {
            return None;
        }
        let (v, next) = r.split_at_checked(size)?;
        n = v.iter().fold(0, |a, b| a * 256 + *b as usize);
        r = next
    }
    let (v, rest) = r.split_at_checked(n)?;
    *data = rest;
    Some((tag, v))
}
pub(crate) fn validity(der: &CertificateDer<'_>) -> Option<(i64, i64)> {
    let mut outer = der.as_ref();
    let mut seq = tlv(&mut outer)?.1;
    let mut tbs = tlv(&mut seq)?.1;
    if tbs.first() == Some(&0xa0) {
        tlv(&mut tbs)?;
    }
    for _ in 0..3 {
        tlv(&mut tbs)?;
    }
    let mut dates = tlv(&mut tbs)?.1;
    let mut next = || {
        let (tag, b) = tlv(&mut dates)?;
        let s = std::str::from_utf8(b).ok()?;
        let full = match tag {
            0x17 if s.len() == 13 => format!(
                "{}{}",
                if s.get(..2)?.parse::<u8>().ok()? >= 50 {
                    "19"
                } else {
                    "20"
                },
                s
            ),
            0x18 => s.into(),
            _ => return None,
        };
        Some(
            NaiveDateTime::parse_from_str(&full, "%Y%m%d%H%M%SZ")
                .ok()?
                .and_utc()
                .timestamp(),
        )
    };
    Some((next()?, next()?))
}
fn validate_cert(
    c: &CertificateDer<'_>,
    name: &ServerName<'_>,
    now: UnixTime,
) -> Result<(), rustls::Error> {
    use rustls::CertificateError;
    let parsed = rustls::server::ParsedCertificate::try_from(c)?;
    rustls::client::verify_server_name(&parsed, name)?;
    let (start, end) = validity(c).ok_or(CertificateError::BadEncoding)?;
    let now = now.as_secs() as i64;
    if now < start {
        return Err(CertificateError::NotValidYet.into());
    }
    if now > end {
        return Err(CertificateError::Expired.into());
    }
    Ok(())
}
#[derive(Debug)]
struct Pinned(Vec<CertificateDer<'static>>);
impl ServerCertVerifier for Pinned {
    fn verify_server_cert(
        &self,
        c: &CertificateDer<'_>,
        _: &[CertificateDer<'_>],
        name: &ServerName<'_>,
        _: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        if !self.0.iter().any(|p| p.as_ref() == c.as_ref()) {
            return Err(rustls::CertificateError::UnknownIssuer.into());
        }
        validate_cert(c, name, now)?;
        Ok(ServerCertVerified::assertion())
    }
    fn verify_tls12_signature(
        &self,
        m: &[u8],
        c: &CertificateDer<'_>,
        d: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            m,
            c,
            d,
            &provider().signature_verification_algorithms,
        )
    }
    fn verify_tls13_signature(
        &self,
        m: &[u8],
        c: &CertificateDer<'_>,
        d: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            m,
            c,
            d,
            &provider().signature_verification_algorithms,
        )
    }
    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}
fn provider() -> Arc<rustls::crypto::CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}
pub fn client_tls(host: &Pair, identity: Option<&Configuration>) -> Result<Arc<ClientConfig>> {
    let mut pins = vec![cert(&host.certificate)?];
    if let Ok(next) = cert(&host.next_certificate) {
        pins.push(next)
    }
    let b = ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(Pinned(pins)));
    Ok(Arc::new(if let Some(id) = identity {
        b.with_client_auth_cert(vec![cert(&id.host.certificate)?], private(&id.private_key)?)?
    } else {
        b.with_no_client_auth()
    }))
}
#[derive(Debug)]
struct Certificates {
    native: Arc<rustls::sign::CertifiedKey>,
    mobile: Option<Arc<rustls::sign::CertifiedKey>>,
}
impl rustls::server::ResolvesServerCert for Certificates {
    fn resolve(
        &self,
        hello: rustls::server::ClientHello<'_>,
    ) -> Option<Arc<rustls::sign::CertifiedKey>> {
        if hello.server_name() != Some("vantry.remote") {
            if let Some(c) = &self.mobile {
                return Some(c.clone());
            }
        }
        Some(self.native.clone())
    }
}
impl Configuration {
    pub fn relay_endpoints(&self) -> Vec<RelayEndpoint> {
        let mut endpoints = vec![];
        if !self.host.relay.is_empty() {
            endpoints.push(RelayEndpoint {
                url: self.host.relay.clone(),
                registration_key: self.registration_key.clone(),
            })
        }
        for ep in &self.relays {
            if !ep.url.is_empty() && !endpoints.iter().any(|old| old.url == ep.url) {
                let mut ep = ep.clone();
                if ep.registration_key.is_empty() {
                    ep.registration_key = self.registration_key.clone()
                }
                endpoints.push(ep);
            }
        }
        endpoints
    }
    pub fn server_tls(&self, mtls: bool) -> Result<Arc<ServerConfig>> {
        let b = ServerConfig::builder_with_provider(provider())
            .with_protocol_versions(&[&rustls::version::TLS13])?;
        let b = if mtls {
            let mut roots = RootCertStore::empty();
            for c in self.clients.iter().filter(|c| !c.certificate.is_empty()) {
                roots.add(cert(&c.certificate)?)?
            }
            let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
                Arc::new(roots),
                provider(),
            )
            .build()?;
            b.with_client_cert_verifier(verifier)
        } else {
            b.with_no_client_auth()
        };
        let make = |pem: &str, key: &str| -> Result<_> {
            Ok(Arc::new(rustls::sign::CertifiedKey::from_der(
                vec![cert(pem)?],
                private(key)?,
                &provider(),
            )?))
        };
        let native = make(&self.host.certificate, &self.private_key)?;
        let mobile = if self.mobile_certificate.is_empty() {
            None
        } else {
            Some(make(&self.mobile_certificate, &self.mobile_private_key)?)
        };
        let mut c = b.with_cert_resolver(Arc::new(Certificates { native, mobile }));
        c.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
        c.send_tls13_tickets = 0;
        Ok(Arc::new(c))
    }
}
struct State {
    cfg: Configuration,
    codes: Pairings,
    current: Option<Pairing>,
    sessions: HashMap<String, CancellationToken>,
}
static STORES: LazyLock<Mutex<HashMap<PathBuf, Weak<Mutex<State>>>>> =
    LazyLock::new(Mutex::default);
pub struct IdentityStore {
    path: PathBuf,
    state: Arc<Mutex<State>>,
}
fn persist(path: &Path, cfg: &Configuration) -> Result<()> {
    let temp = path.with_file_name(format!(".remote-access-{}.tmp", token()));
    let mut opts = OpenOptions::new();
    opts.create_new(true).write(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let mut file = opts.open(&temp)?;
    let result = (|| -> Result<()> {
        serde_json::to_writer_pretty(&mut file, cfg)?;
        file.flush()?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, path)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(temp);
    }
    result
}
impl IdentityStore {
    pub fn open(dir: &Path, name: &str) -> Result<Self> {
        let mut dirs = fs::DirBuilder::new();
        dirs.recursive(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::DirBuilderExt;
            dirs.mode(0o700);
        }
        dirs.create(dir)?;
        let path = dir.canonicalize()?.join("remote-access.json");
        let mut stores = STORES.lock().unwrap();
        stores.retain(|_, s| s.strong_count() > 0);
        if let Some(state) = stores.get(&path).and_then(Weak::upgrade) {
            return Ok(Self { path, state });
        }
        let mut cfg: Configuration = match File::open(&path) {
            Ok(file) => {
                serde_json::from_reader(file).map_err(|_| anyhow!("invalid remote-access.json"))?
            }
            Err(e) if e.kind() == ErrorKind::NotFound => Default::default(),
            Err(e) => return Err(e.into()),
        };
        let mut changed = false;
        if cfg.host.v == 0 && cfg.private_key.is_empty() {
            let (c, k) = generate(false)?;
            cfg.host = Pair {
                v: 1,
                name: name.into(),
                device_id: token(),
                route: token(),
                secret: token(),
                certificate: c,
                ..Default::default()
            };
            cfg.private_key = k;
            changed = true
        }
        if cfg.host.device_id.is_empty() {
            cfg.host.device_id = token();
            changed = true;
        }
        let now = Utc::now().timestamp();
        let (_, expiry) = validity(&cert(&cfg.host.certificate)?)
            .ok_or_else(|| anyhow!("invalid host certificate validity"))?;
        if expiry < now && !cfg.next_certificate.is_empty() && !cfg.next_private_key.is_empty() {
            validate_cert(
                &cert(&cfg.next_certificate)?,
                &ServerName::try_from("vantry.remote")?,
                UnixTime::now(),
            )?;
            cfg.host.certificate = std::mem::take(&mut cfg.next_certificate);
            cfg.private_key = std::mem::take(&mut cfg.next_private_key);
            changed = true;
        } else if (now..=now + 30 * 86400).contains(&expiry) && cfg.next_certificate.is_empty() {
            (cfg.next_certificate, cfg.next_private_key) = generate(false)?;
            changed = true;
        }
        cfg.server_tls(false)?;
        if cfg.mobile_certificate.is_empty() && cfg.mobile_private_key.is_empty() {
            (cfg.mobile_certificate, cfg.mobile_private_key) = generate(true)?;
            changed = true
        }
        if changed {
            persist(&path, &cfg)?;
        }
        let state = Arc::new(Mutex::new(State {
            cfg,
            codes: Pairings::default(),
            current: None,
            sessions: HashMap::new(),
        }));
        stores.insert(path.clone(), Arc::downgrade(&state));
        Ok(Self { path, state })
    }
    pub fn snapshot(&self) -> Configuration {
        self.state.lock().unwrap().cfg.clone()
    }
    pub fn public_identity(&self) -> Value {
        let cfg = self.snapshot();
        let end = cert(&cfg.host.certificate)
            .ok()
            .and_then(|c| validity(&c))
            .and_then(|(_, end)| Utc.timestamp_opt(end, 0).single());
        let relays: Vec<_> = cfg.relay_endpoints().into_iter().map(|ep| ep.url).collect();
        json!({"deviceId":cfg.host.device_id,"certificate":cfg.host.certificate,"nextCertificate":cfg.next_certificate,"expiresAt":end,"direct":cfg.host.direct,"relays":relays})
    }
    pub fn set_routes(&self, direct: Vec<String>, relays: Vec<String>) -> Result<()> {
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        cfg.host.direct = direct;
        cfg.host.relay = relays.first().cloned().unwrap_or_default();
        cfg.host.relays = relays;
        cfg.relays = cfg
            .host
            .relays
            .iter()
            .map(|url| {
                cfg.relays
                    .iter()
                    .find(|ep| ep.url == *url)
                    .cloned()
                    .unwrap_or(RelayEndpoint {
                        url: url.clone(),
                        registration_key: String::new(),
                    })
            })
            .collect();
        cfg.host
            .validate(std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1"))?;
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        Ok(())
    }
    pub fn issue(&self) -> Result<Pair> {
        let mut s = self.state.lock().unwrap();
        ensure!(s.cfg.clients.len() < 128, "paired device limit reached");
        let mut public = Pair {
            v: 2,
            name: s.cfg.host.name.clone(),
            relay: s.cfg.host.relay.clone(),
            route: s.cfg.host.route.clone(),
            device_id: s.cfg.host.device_id.clone(),
            direct: s.cfg.host.direct.clone(),
            certificate: s.cfg.host.certificate.clone(),
            next_certificate: s.cfg.next_certificate.clone(),
            ..Default::default()
        };
        public.relays = s
            .cfg
            .relay_endpoints()
            .into_iter()
            .map(|ep| ep.url)
            .collect();
        let entry = s.codes.issue(
            "session",
            &public.name,
            serde_json::to_value(&public)?,
            None,
            "host",
        )?;
        if let Some(old) = s.current.replace(entry.clone()) {
            s.codes.revoke(&old.id, "host");
        }
        public.code = entry.code;
        public.expires_at = Some(entry.expires_at);
        Ok(public)
    }
    pub(crate) fn use_relay_code(&self, local: &Pair, entry: Pairing) -> Result<Pair> {
        let mut s = self.state.lock().unwrap();
        ensure!(
            s.current.as_ref().is_some_and(|e| e.code == local.code)
                && entry.scope == "session"
                && entry.code.starts_with("SESSION-")
                && entry.status == "waiting"
                && entry.expires_at > Utc::now()
                && entry.expires_at <= Utc::now() + chrono::Duration::minutes(10),
            "invalid or superseded relay invitation"
        );
        let mut public = local.clone();
        public.code = entry.code.clone();
        public.expires_at = Some(entry.expires_at);
        s.current = Some(entry);
        Ok(public)
    }
    pub fn update_peer_routes(
        &self,
        id: &str,
        name: &str,
        direct: Vec<String>,
        relays: Vec<String>,
    ) -> Result<()> {
        ensure!(!name.trim().is_empty(), "missing peer name");
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        let p = &mut cfg
            .peers
            .iter_mut()
            .find(|p| p.id == id)
            .ok_or_else(|| anyhow!("unknown peer"))?
            .pair;
        p.name = name.trim().into();
        p.direct = direct;
        p.relay = relays.first().cloned().unwrap_or_default();
        p.relays = relays;
        p.validate(std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1"))?;
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        Ok(())
    }
    pub(crate) fn sync_identity(&self, id: &str, snapshot: Value) -> Result<()> {
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        let p = &mut cfg
            .peers
            .iter_mut()
            .find(|p| p.id == id)
            .ok_or_else(|| anyhow!("unknown peer"))?
            .pair;
        let active = snapshot["certificate"]
            .as_str()
            .ok_or_else(|| anyhow!("missing certificate"))?;
        ensure!(
            (p.device_id.is_empty() || snapshot["deviceId"] == p.device_id)
                && (active == p.certificate || active == p.next_certificate),
            "peer identity changed"
        );
        let next = snapshot["nextCertificate"].as_str().unwrap_or("");
        if !next.is_empty() {
            validate_cert(
                &cert(next)?,
                &ServerName::try_from("vantry.remote")?,
                UnixTime::now(),
            )?;
        }
        let before = serde_json::to_value(&*p)?;
        p.certificate = active.into();
        p.next_certificate = next.into();
        if let Some(d) = snapshot.get("direct").filter(|v| v.is_array()) {
            p.direct = serde_json::from_value(d.clone())?;
        }
        if let Some(r) = snapshot.get("relays").filter(|v| v.is_array()) {
            p.relays = serde_json::from_value(r.clone())?;
        }
        p.validate(std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1"))?;
        if before != serde_json::to_value(&*p)? {
            persist(&self.path, &cfg)?;
            s.cfg = cfg;
        }
        Ok(())
    }
    pub fn remove_peer(&self, id: &str) -> Result<bool> {
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        let before = cfg.peers.len();
        cfg.peers.retain(|p| p.id != id);
        if before == cfg.peers.len() {
            return Ok(false);
        }
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        Ok(true)
    }
    pub fn renew_certificate(&self) -> Result<String> {
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        (cfg.next_certificate, cfg.next_private_key) = generate(false)?;
        persist(&self.path, &cfg)?;
        let next = cfg.next_certificate.clone();
        s.cfg = cfg;
        Ok(next)
    }
    pub fn activate_certificate(&self) -> Result<()> {
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        ensure!(!cfg.next_certificate.is_empty(), "no pending certificate");
        cfg.host.certificate = std::mem::take(&mut cfg.next_certificate);
        cfg.private_key = std::mem::take(&mut cfg.next_private_key);
        cfg.server_tls(false)?;
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        Ok(())
    }
    pub fn revoke_code(&self) {
        let mut s = self.state.lock().unwrap();
        if let Some(e) = s.current.take() {
            s.codes.revoke(&e.id, "host");
        }
    }
    pub fn redeem(&self, code: &str, name: &str, client_certificate: &str) -> Result<Value> {
        let mut s = self.state.lock().unwrap();
        let e = s
            .current
            .as_ref()
            .ok_or_else(|| anyhow!("invalid pairing code"))?
            .clone();
        ensure!(
            matches_secret(&e.code, &crate::pairing::normalize(code))
                && e.expires_at > Utc::now()
                && s.cfg.clients.len() < 128,
            "invalid or expired pairing code"
        );
        if !client_certificate.is_empty() {
            validate_cert(
                &cert(client_certificate)?,
                &ServerName::try_from("vantry.remote")?,
                UnixTime::now(),
            )?;
        }
        let mut cfg = s.cfg.clone();
        let name = short_label(if name.trim().is_empty() {
            "配对设备"
        } else {
            name.trim()
        });
        let mut issued = cfg.host.clone();
        issued.relays = cfg.relay_endpoints().into_iter().map(|ep| ep.url).collect();
        issued.secret = token();
        issued.next_certificate = cfg.next_certificate.clone();
        cfg.clients.push(AccessClient {
            id: token(),
            name: name.clone(),
            created_at: Utc::now(),
            secret: issued.secret.clone(),
            certificate: client_certificate.into(),
        });
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        s.current = None;
        s.codes.revoke(&e.id, "host");
        Ok(
            json!({"id":e.id,"code":e.code,"scope":"session","name":issued.name,"expiresAt":e.expires_at,"payload":issued,"status":"paired","pairedWith":name}),
        )
    }
    pub fn save_peer(&self, pair: Pair) -> Result<String> {
        pair.validate(std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1"))?;
        ensure!(pair.v != 2, "pairing redemption required");
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        let id = if let Some(p) = cfg.peers.iter_mut().find(|p| {
            (!pair.device_id.is_empty() && pair.device_id == p.pair.device_id)
                || (pair.route == p.pair.route && pair.relay == p.pair.relay)
        }) {
            p.pair = pair;
            p.id.clone()
        } else {
            let id = token();
            cfg.peers.push(Peer {
                id: id.clone(),
                pair,
            });
            id
        };
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        Ok(id)
    }
    pub fn revoke_client(&self, id: &str) -> Result<bool> {
        let mut s = self.state.lock().unwrap();
        let mut cfg = s.cfg.clone();
        let Some(index) = cfg.clients.iter().position(|client| client.id == id) else {
            return Ok(false);
        };
        let client = cfg.clients.remove(index);
        cfg.revocations.push(RevokedClient {
            id: client.id,
            name: client.name,
            revoked_at: Utc::now(),
        });
        persist(&self.path, &cfg)?;
        s.cfg = cfg;
        if let Some(cancel) = s.sessions.remove(id) {
            cancel.cancel()
        }
        Ok(true)
    }
    pub fn authorize(&self, bearer: &str) -> Option<CancellationToken> {
        if bearer.len() < 32 {
            return None;
        }
        let mut s = self.state.lock().unwrap();
        let id = if matches_secret(bearer, &s.cfg.host.secret) {
            "host".to_string()
        } else {
            s.cfg
                .clients
                .iter()
                .find(|c| matches_secret(bearer, &c.secret))?
                .id
                .clone()
        };
        Some(s.sessions.entry(id).or_default().clone())
    }
}
