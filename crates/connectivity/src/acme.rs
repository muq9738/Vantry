//! ACME v2 account registration, certificate orders and TLS-ALPN-01 (RFC 8555/8737).
use crate::{
    identity::validity,
    pairing::token,
    transport::{Stream, Tasks},
};
use anyhow::{Result, anyhow, bail, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use chrono::Utc;
use rcgen::{CertificateParams, CustomExtension, KeyPair};
use ring::digest;
use rustls::{
    ServerConfig, SignatureScheme,
    crypto::ring::default_provider,
    pki_types::{PrivateKeyDer, ServerName},
};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    fs,
    io::{ErrorKind, Write},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    net::TcpStream,
    time::{Instant, timeout},
};
use tokio_rustls::LazyConfigAcceptor;
const DIRECTORY: &str = "https://acme-v02.api.letsencrypt.org/directory";
const ALPN: &[u8] = b"acme-tls/1";
struct Domain {
    challenge: Mutex<Option<Arc<ServerConfig>>>,
    order: Arc<tokio::sync::Mutex<()>>,
    last: Mutex<Option<Instant>>,
}
pub struct Acme {
    directory: PathBuf,
    domains: HashMap<String, Arc<Domain>>,
    email: String,
    account: Mutex<Option<Arc<Account>>>,
}
impl Acme {
    pub fn new(directory: PathBuf, hosts: &[String], email: String) -> Result<Self> {
        ensure!(
            directory.is_absolute() && !hosts.is_empty(),
            "ACME needs an absolute certificateDirectory and hostnames"
        );
        let mut domains = HashMap::new();
        for name in hosts {
            let name = name.to_ascii_lowercase();
            ensure!(
                matches!(
                    ServerName::try_from(name.as_str()),
                    Ok(ServerName::DnsName(_))
                ) && name
                    .split('.')
                    .all(|s| !s.is_empty()
                        && s.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')),
                "invalid ACME hostname"
            );
            domains.insert(
                name,
                Arc::new(Domain {
                    challenge: Mutex::default(),
                    order: Arc::default(),
                    last: Mutex::default(),
                }),
            );
        }
        Ok(Self {
            directory,
            domains,
            email,
            account: Mutex::default(),
        })
    }
    pub(crate) async fn accept(self: Arc<Self>, socket: TcpStream, tasks: Tasks) -> Result<Stream> {
        let hello = timeout(
            Duration::from_secs(10),
            LazyConfigAcceptor::new(Default::default(), socket),
        )
        .await??;
        let h = hello.client_hello();
        let name = h
            .server_name()
            .ok_or_else(|| anyhow!("ACME requires SNI"))?
            .to_ascii_lowercase();
        let domain = self
            .domains
            .get(&name)
            .ok_or_else(|| anyhow!("unknown relay hostname"))?
            .clone();
        let validation = h.alpn().is_some_and(|mut a| a.any(|v| v == ALPN));
        let cfg = if validation {
            domain
                .challenge
                .lock()
                .unwrap()
                .clone()
                .ok_or_else(|| anyhow!("no pending TLS-ALPN challenge"))?
        } else {
            let cached = self.cached(&name);
            if cached
                .as_ref()
                .is_none_or(|(_, end)| *end < Utc::now().timestamp() + 30 * 86400)
            {
                if let Ok(guard) = domain.order.clone().try_lock_owned() {
                    let manager = self.clone();
                    let domain = domain.clone();
                    let hostname = name.clone();
                    let due = domain
                        .last
                        .lock()
                        .unwrap()
                        .is_none_or(|t| t.elapsed() > Duration::from_secs(600));
                    if due {
                        *domain.last.lock().unwrap() = Some(Instant::now());
                        tasks.spawn(async move {
                            let _guard = guard;
                            if !matches!(
                                timeout(
                                    Duration::from_secs(180),
                                    manager.order(&hostname, &domain)
                                )
                                .await,
                                Ok(Ok(()))
                            ) {
                                tracing::warn!("relay ACME order failed")
                            }
                            *domain.challenge.lock().unwrap() = None;
                        });
                    }
                }
            }
            match cached {
                Some((cfg, _)) => cfg,
                None => {
                    let _guard = domain.order.lock().await;
                    self.cached(&name)
                        .ok_or_else(|| anyhow!("relay certificate unavailable"))?
                        .0
                }
            }
        };
        Ok(Box::new(
            timeout(Duration::from_secs(10), hello.into_stream(cfg)).await??,
        ))
    }
    fn cached(&self, name: &str) -> Option<(Arc<ServerConfig>, i64)> {
        for file in [name.to_string(), format!("{name}+rsa")] {
            if let Ok(data) = fs::read(self.directory.join(file)) {
                if let Ok((cfg, end)) = tls(&data, name, false) {
                    return Some((cfg, end));
                }
            }
        }
        None
    }
    fn account(&self) -> Result<Arc<Account>> {
        let mut saved = self.account.lock().unwrap();
        if let Some(a) = saved.as_ref() {
            return Ok(a.clone());
        }
        let mut dir = fs::DirBuilder::new();
        dir.recursive(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::DirBuilderExt;
            dir.mode(0o700);
        }
        dir.create(&self.directory)?;
        let path = self.directory.join("acme_account+key");
        let data = match fs::read(&path) {
            Ok(bytes) => bytes,
            Err(e) if e.kind() == ErrorKind::NotFound => {
                match fs::read(self.directory.join("acme_account.key")) {
                    Ok(bytes) => bytes,
                    Err(e) if e.kind() == ErrorKind::NotFound => {
                        let key = KeyPair::generate()?.serialize_pem();
                        let tmp = private_file(&self.directory, key.as_bytes())?;
                        let result = fs::hard_link(&tmp, &path);
                        let _ = fs::remove_file(tmp);
                        if let Err(e) = result {
                            ensure!(
                                e.kind() == ErrorKind::AlreadyExists,
                                "cannot create ACME account"
                            );
                        }
                        fs::read(path)?
                    }
                    Err(e) => return Err(e.into()),
                }
            }
            Err(e) => return Err(e.into()),
        };
        let key = rustls_pemfile::private_key(&mut data.as_slice())?
            .ok_or_else(|| anyhow!("missing ACME account key"))?;
        let a = Arc::new(Account::new(key)?);
        *saved = Some(a.clone());
        Ok(a)
    }
    async fn order(&self, name: &str, domain: &Domain) -> Result<()> {
        let account = self.account()?;
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(20))
            .redirect(reqwest::redirect::Policy::none())
            .build()?;
        let directory: Value = client
            .get(DIRECTORY)
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        let mut acme = Client {
            client,
            account,
            nonce_url: field(&directory, "newNonce")?.into(),
            kid: None,
            nonce: None,
        };
        let contact: Vec<String> = (!self.email.is_empty())
            .then(|| format!("mailto:{}", self.email))
            .into_iter()
            .collect();
        let reg = acme
            .post(
                field(&directory, "newAccount")?,
                Some(json!({"termsOfServiceAgreed":true,"contact":contact})),
            )
            .await?;
        acme.kid = Some(location(&reg)?);
        let order = acme
            .post(
                field(&directory, "newOrder")?,
                Some(json!({"identifiers":[{"type":"dns","value":name}]})),
            )
            .await?;
        let order_url = location(&order)?;
        let order: Value = order.json().await?;
        let finalize = field(&order, "finalize")?;
        for auth in order["authorizations"]
            .as_array()
            .ok_or_else(|| anyhow!("invalid ACME order"))?
        {
            let url = auth
                .as_str()
                .ok_or_else(|| anyhow!("invalid ACME authorization"))?;
            let a: Value = acme.post(url, None).await?.json().await?;
            if a["status"] == "valid" {
                continue;
            }
            ensure!(a["identifier"]["value"] == name, "ACME identifier mismatch");
            let challenge = a["challenges"]
                .as_array()
                .and_then(|v| v.iter().find(|c| c["type"] == "tls-alpn-01"))
                .ok_or_else(|| anyhow!("no TLS-ALPN challenge"))?;
            let token = field(challenge, "token")?;
            let authorization = format!("{token}.{}", acme.account.thumbprint);
            let mut p = CertificateParams::new(vec![name.into()])?;
            p.custom_extensions
                .push(CustomExtension::new_acme_identifier(
                    digest::digest(&digest::SHA256, authorization.as_bytes()).as_ref(),
                ));
            let key = KeyPair::generate()?;
            let pem = key.serialize_pem() + &p.self_signed(&key)?.pem();
            *domain.challenge.lock().unwrap() = Some(tls(pem.as_bytes(), name, true)?.0);
            acme.post(field(challenge, "url")?, Some(json!({}))).await?;
            acme.wait(url, "valid").await?;
            *domain.challenge.lock().unwrap() = None;
        }
        acme.wait(&order_url, "ready").await?;
        let key = KeyPair::generate()?;
        let params = CertificateParams::new(vec![name.into()])?;
        let csr = params.serialize_request(&key)?;
        acme.post(finalize, Some(json!({"csr":B64.encode(csr.der())})))
            .await?;
        let issued = acme.wait(&order_url, "valid").await?;
        let chain = acme
            .post(field(&issued, "certificate")?, None)
            .await?
            .text()
            .await?;
        let bundle = key.serialize_pem() + &chain;
        tls(bundle.as_bytes(), name, false)?;
        let temp = private_file(&self.directory, bundle.as_bytes())?;
        let result = fs::rename(&temp, self.directory.join(name));
        if result.is_err() {
            let _ = fs::remove_file(temp);
        }
        Ok(result?)
    }
}
fn tls(pem: &[u8], name: &str, challenge: bool) -> Result<(Arc<ServerConfig>, i64)> {
    let chain = rustls_pemfile::certs(&mut &pem[..]).collect::<std::io::Result<Vec<_>>>()?;
    let leaf = chain
        .first()
        .ok_or_else(|| anyhow!("empty certificate chain"))?;
    let parsed = rustls::server::ParsedCertificate::try_from(leaf)?;
    rustls::client::verify_server_name(&parsed, &ServerName::try_from(name)?)?;
    let (start, end) = validity(leaf).ok_or_else(|| anyhow!("invalid certificate validity"))?;
    ensure!(
        (start..=end).contains(&Utc::now().timestamp()),
        "invalid certificate validity"
    );
    let key = rustls_pemfile::private_key(&mut &pem[..])?
        .ok_or_else(|| anyhow!("missing certificate key"))?;
    let mut c = ServerConfig::builder_with_provider(Arc::new(default_provider()))
        .with_safe_default_protocol_versions()?
        .with_no_client_auth()
        .with_single_cert(chain, key)?;
    if challenge {
        c.alpn_protocols = vec![ALPN.to_vec()];
    } else {
        c.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
    }
    Ok((Arc::new(c), end))
}
fn private_file(dir: &Path, data: &[u8]) -> Result<PathBuf> {
    let path = dir.join(format!(".acme-{}.tmp", token()));
    let mut opt = fs::OpenOptions::new();
    opt.create_new(true).write(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opt.mode(0o600);
    }
    let mut f = opt.open(&path)?;
    if let Err(e) = f.write_all(data).and_then(|_| f.sync_all()) {
        drop(f);
        let _ = fs::remove_file(path);
        return Err(e.into());
    }
    Ok(path)
}
fn field<'a>(v: &'a Value, k: &str) -> Result<&'a str> {
    v[k].as_str()
        .ok_or_else(|| anyhow!("invalid ACME response"))
}
fn location(r: &reqwest::Response) -> Result<String> {
    Ok(r.headers()
        .get("location")
        .ok_or_else(|| anyhow!("missing ACME location"))?
        .to_str()?
        .to_string())
}
struct Account {
    signer: Box<dyn rustls::sign::Signer>,
    jwk: Value,
    thumbprint: String,
}
impl Account {
    fn new(key: PrivateKeyDer<'static>) -> Result<Self> {
        let key = rustls::crypto::ring::sign::any_supported_type(&key)?;
        let signer = key
            .choose_scheme(&[SignatureScheme::ECDSA_NISTP256_SHA256])
            .ok_or_else(|| anyhow!("unsupported ACME account key"))?;
        let spki = key
            .public_key()
            .ok_or_else(|| anyhow!("missing account public key"))?;
        let pubkey = spki
            .as_ref()
            .last_chunk::<65>()
            .filter(|v| v[0] == 4)
            .ok_or_else(|| anyhow!("invalid account public key"))?;
        let jwk = json!({"crv":"P-256","kty":"EC","x":B64.encode(&pubkey[1..33]),"y":B64.encode(&pubkey[33..])});
        let thumbprint = B64.encode(digest::digest(&digest::SHA256, jwk.to_string().as_bytes()));
        Ok(Self {
            signer,
            jwk,
            thumbprint,
        })
    }
    fn signature(&self, input: &str) -> Result<String> {
        let der = self.signer.sign(input.as_bytes())?;
        let mut bytes = der
            .get(2..)
            .ok_or_else(|| anyhow!("invalid ECDSA signature"))?;
        let mut signature = [0; 64];
        for dest in signature.chunks_exact_mut(32) {
            ensure!(bytes.len() >= 2 && bytes[0] == 2, "invalid ECDSA integer");
            let n = bytes[1] as usize;
            let (num, rest) = bytes[2..]
                .split_at_checked(n)
                .ok_or_else(|| anyhow!("invalid signature"))?;
            let num = num.strip_prefix(&[0]).unwrap_or(num);
            ensure!(num.len() <= 32, "invalid signature");
            dest[32 - num.len()..].copy_from_slice(num);
            bytes = rest
        }
        Ok(B64.encode(signature))
    }
}
struct Client {
    client: reqwest::Client,
    account: Arc<Account>,
    nonce_url: String,
    kid: Option<String>,
    nonce: Option<String>,
}
impl Client {
    async fn post(&mut self, url: &str, data: Option<Value>) -> Result<reqwest::Response> {
        ensure!(url.starts_with("https://"), "invalid ACME endpoint");
        let payload = data.map(|v| B64.encode(v.to_string())).unwrap_or_default();
        for _ in 0..2 {
            if self.nonce.is_none() {
                let r = self
                    .client
                    .head(&self.nonce_url)
                    .send()
                    .await?
                    .error_for_status()?;
                self.nonce = r
                    .headers()
                    .get("replay-nonce")
                    .and_then(|v| v.to_str().ok())
                    .map(str::to_string)
            }
            let mut protected = json!({"alg":"ES256","nonce":self.nonce.take().ok_or_else(||anyhow!("missing ACME nonce"))?,"url":url});
            if let Some(kid) = &self.kid {
                protected["kid"] = json!(kid)
            } else {
                protected["jwk"] = self.account.jwk.clone()
            }
            let p = B64.encode(protected.to_string());
            let signature = self.account.signature(&format!("{p}.{payload}"))?;
            let r = self
                .client
                .post(url)
                .header("content-type", "application/jose+json")
                .body(json!({"protected":p,"payload":payload,"signature":signature}).to_string())
                .send()
                .await?;
            self.nonce = r
                .headers()
                .get("replay-nonce")
                .and_then(|v| v.to_str().ok())
                .map(str::to_string);
            if r.status().is_success() {
                return Ok(r);
            }
            let status = r.status();
            let v: Value = r.json().await.unwrap_or_default();
            if v["type"] != "urn:ietf:params:acme:error:badNonce" {
                bail!("ACME request failed (HTTP {status})")
            }
        }
        bail!("ACME nonce rejected")
    }
    async fn wait(&mut self, url: &str, expected: &str) -> Result<Value> {
        loop {
            let r = self.post(url, None).await?;
            let delay = r
                .headers()
                .get("retry-after")
                .and_then(|v| v.to_str().ok()?.parse::<u64>().ok())
                .unwrap_or(2)
                .clamp(1, 30);
            let v: Value = r.json().await?;
            let status = field(&v, "status")?;
            if status == expected {
                return Ok(v);
            }
            ensure!(
                matches!(status, "pending" | "processing" | "ready"),
                "ACME order failed"
            );
            tokio::time::sleep(Duration::from_secs(delay)).await;
        }
    }
}
