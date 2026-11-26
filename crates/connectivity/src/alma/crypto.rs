//! Responder for the existing Alma E2E v1 wire protocol.
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use ring::{
    aead, agreement, digest, hkdf,
    rand::{SecureRandom, SystemRandom},
};
use serde_json::{Value, json};
use subtle::ConstantTimeEq;

const PROTOCOL: &[u8] = b"alma-relay-e2e/v1";

pub fn new_code() -> Result<String> {
    let mut bytes = [0; 32];
    SystemRandom::new().fill(&mut bytes)?;
    Ok(B64.encode(bytes))
}

fn derive<const N: usize>(secret: &[u8], salt: &[u8], info: &[&[u8]]) -> Result<[u8; N]> {
    struct Length(usize);
    impl hkdf::KeyType for Length {
        fn len(&self) -> usize {
            self.0
        }
    }
    let prk = hkdf::Salt::new(hkdf::HKDF_SHA256, salt).extract(secret);
    let mut bytes = [0; N];
    prk.expand(info, Length(N))?.fill(&mut bytes)?;
    Ok(bytes)
}

pub(super) fn routing(code: &str) -> Result<String> {
    Ok(B64.encode(derive::<16>(
        code.as_bytes(),
        b"alma-relay-e2e/v1/routing",
        &[],
    )?))
}

pub(super) fn registration(code: &str) -> Result<String> {
    let routing = routing(code)?;
    Ok(digest::digest(&digest::SHA256, routing.as_bytes())
        .as_ref()
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect())
}

pub(super) struct Pending {
    confirm: [u8; 32],
    keys: Keys,
}

impl Pending {
    pub fn accept(code: &str, hello: &Value) -> Result<(Self, Value)> {
        ensure!(hello["t"] == "hi", "expected Alma hello");
        let field = |name: &str| B64.decode(hello[name].as_str().unwrap_or(""));
        let peer = field("epk")?;
        let nonce = field("n")?;
        ensure!(peer.len() == 65 && nonce.len() == 16, "invalid Alma hello");
        let rng = SystemRandom::new();
        let secret = agreement::EphemeralPrivateKey::generate(&agreement::ECDH_P256, &rng)?;
        let public = secret.compute_public_key()?;
        let mut local_nonce = [0; 16];
        rng.fill(&mut local_nonce)?;
        let mut transcript = digest::Context::new(&digest::SHA256);
        for part in [PROTOCOL, &peer, public.as_ref(), &nonce, &local_nonce] {
            transcript.update(part);
        }
        let transcript = transcript.finish();
        let peer = agreement::UnparsedPublicKey::new(&agreement::ECDH_P256, peer);
        agreement::agree_ephemeral(secret, &peer, |shared| -> Result<_> {
            let key =
                |label: &[u8]| derive::<32>(shared, code.as_bytes(), &[label, transcript.as_ref()]);
            let keys = Keys {
                incoming: Stream::new(key(b"c2s|")?)?,
                outgoing: Stream::new(key(b"s2c|")?)?,
            };
            let hello = json!({"t":"hi","epk":B64.encode(public.as_ref()),"n":B64.encode(local_nonce),
                "cr":B64.encode(key(b"confirm-r|")?)});
            Ok((
                Self {
                    confirm: key(b"confirm-i|")?,
                    keys,
                },
                hello,
            ))
        })?
    }

    pub fn confirm(self, frame: &Value) -> Result<Keys> {
        let proof = B64.decode(frame["ci"].as_str().unwrap_or(""))?;
        ensure!(
            frame["t"] == "ok" && bool::from(self.confirm.as_slice().ct_eq(&proof)),
            "Alma pairing was not confirmed"
        );
        Ok(self.keys)
    }
}

struct Stream {
    key: aead::LessSafeKey,
    count: u64,
}

impl Stream {
    fn new(bytes: [u8; 32]) -> Result<Self> {
        Ok(Self {
            key: aead::LessSafeKey::new(aead::UnboundKey::new(&aead::AES_256_GCM, &bytes)?),
            count: 0,
        })
    }

    fn nonce(&mut self) -> Result<[u8; 12]> {
        let mut nonce = [0; 12];
        nonce[4..].copy_from_slice(&self.count.to_be_bytes());
        self.count = self
            .count
            .checked_add(1)
            .ok_or_else(|| anyhow!("Alma session exhausted"))?;
        Ok(nonce)
    }
}

pub(super) struct Keys {
    incoming: Stream,
    outgoing: Stream,
}

impl Keys {
    pub fn open(&mut self, bytes: &mut [u8]) -> Result<Value> {
        let nonce = self.incoming.nonce()?;
        ensure!(
            bytes.len() >= 28 && bytes[..12] == nonce,
            "Alma session out of sequence"
        );
        let plain = self.incoming.key.open_in_place(
            aead::Nonce::assume_unique_for_key(nonce),
            aead::Aad::empty(),
            &mut bytes[12..],
        )?;
        Ok(serde_json::from_slice(plain)?)
    }

    pub fn seal(&mut self, value: &Value) -> Result<Vec<u8>> {
        let nonce = self.outgoing.nonce()?;
        let mut bytes = nonce.to_vec();
        serde_json::to_writer(&mut bytes, value)?;
        let tag = self.outgoing.key.seal_in_place_separate_tag(
            aead::Nonce::assume_unique_for_key(nonce),
            aead::Aad::empty(),
            &mut bytes[12..],
        )?;
        bytes.extend_from_slice(tag.as_ref());
        Ok(bytes)
    }
}
