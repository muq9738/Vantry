//! TURN/TCP restricted to WebSocket allocations belonging to the same desktop room.
//! ICE candidates use loopback addresses as in the Go relay. Packets never leave the room.
use crate::pairing::token;
use anyhow::{Result, ensure};
use rand::{RngCore, rngs::OsRng};
use ring::hmac;
use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt, DuplexStream},
    sync::mpsc,
    time::Instant,
};
const COOKIE: u32 = 0x2112a442;
const REALM: &[u8] = b"vantry-desktop";
const USER: u16 = 6;
const MI: u16 = 8;
const PEER: u16 = 0x12;
const DATA: u16 = 0x13;
const REALM_ATTR: u16 = 0x14;
const NONCE: u16 = 0x15;
type Datagram = (u16, Vec<u8>);
#[derive(Default)]
pub(crate) struct Switch(Mutex<HashMap<u16, mpsc::Sender<Datagram>>>);
impl Switch {
    fn allocate(&self, tx: mpsc::Sender<Datagram>) -> Result<u16> {
        let mut ports = self.0.lock().unwrap();
        for _ in 0..256 {
            let port = 49152 + (OsRng.next_u32() % 16384) as u16;
            if let std::collections::hash_map::Entry::Vacant(e) = ports.entry(port) {
                e.insert(tx);
                return Ok(port);
            }
        }
        anyhow::bail!("allocation capacity reached")
    }
    fn send(&self, from: u16, to: u16, data: &[u8]) {
        if data.len() > 65000 {
            return;
        }
        if let Some(tx) = self.0.lock().unwrap().get(&to) {
            let _ = tx.try_send((from, data.to_vec()));
        }
    }
}
struct Allocation {
    port: u16,
    room: Arc<Switch>,
}
impl Drop for Allocation {
    fn drop(&mut self) {
        self.room.0.lock().unwrap().remove(&self.port);
    }
}
// RFC 5389 section 15.4 specifies MD5(username:realm:password) for this wire key.
fn long_term_key(input: &[u8]) -> [u8; 16] {
    const S: [u32; 16] = [7, 12, 17, 22, 5, 9, 14, 20, 4, 11, 16, 23, 6, 10, 15, 21];
    const K: [u32; 64] = [
        0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee, 0xf57c0faf, 0x4787c62a, 0xa8304613,
        0xfd469501, 0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be, 0x6b901122, 0xfd987193,
        0xa679438e, 0x49b40821, 0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa, 0xd62f105d,
        0x02441453, 0xd8a1e681, 0xe7d3fbc8, 0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
        0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a, 0xfffa3942, 0x8771f681, 0x6d9d6122,
        0xfde5380c, 0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70, 0x289b7ec6, 0xeaa127fa,
        0xd4ef3085, 0x04881d05, 0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665, 0xf4292244,
        0x432aff97, 0xab9423a7, 0xfc93a039, 0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
        0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1, 0xf7537e82, 0xbd3af235, 0x2ad7d2bb,
        0xeb86d391,
    ];
    let mut data = input.to_vec();
    data.push(128);
    while data.len() % 64 != 56 {
        data.push(0)
    }
    data.extend_from_slice(&(input.len() as u64 * 8).to_le_bytes());
    let mut state = [0x67452301u32, 0xefcdab89, 0x98badcfe, 0x10325476];
    for block in data.chunks_exact(64) {
        let [mut a, mut b, mut c, mut d] = state;
        for i in 0..64 {
            let (f, g) = match i / 16 {
                0 => ((b & c) | (!b & d), i),
                1 => ((d & b) | (!d & c), (5 * i + 1) % 16),
                2 => (b ^ c ^ d, (3 * i + 5) % 16),
                _ => (c ^ (b | !d), (7 * i) % 16),
            };
            let w = u32::from_le_bytes(block[4 * g..4 * g + 4].try_into().unwrap());
            let next = b.wrapping_add(
                a.wrapping_add(f)
                    .wrapping_add(K[i])
                    .wrapping_add(w)
                    .rotate_left(S[i / 16 * 4 + i % 4]),
            );
            (a, b, c, d) = (d, next, b, c);
        }
        for (s, v) in state.iter_mut().zip([a, b, c, d]) {
            *s = s.wrapping_add(v)
        }
    }
    let mut out = [0; 16];
    for (chunk, n) in out.chunks_exact_mut(4).zip(state) {
        chunk.copy_from_slice(&n.to_le_bytes())
    }
    out
}
fn word(b: &[u8]) -> u16 {
    u16::from_be_bytes([b[0], b[1]])
}
fn address(port: u16) -> [u8; 8] {
    let mut a = [0, 1, 0, 0, 0, 0, 0, 0];
    a[2..4].copy_from_slice(&(port ^ (COOKIE >> 16) as u16).to_be_bytes());
    a[4..].copy_from_slice(&(0x7f000001 ^ COOKIE).to_be_bytes());
    a
}
fn port(a: &[u8]) -> Option<u16> {
    (a.len() == 8 && a[1] == 1 && a[4..] == address(0)[4..])
        .then(|| word(&a[2..]) ^ (COOKIE >> 16) as u16)
}
fn message(kind: u16, tx: &[u8]) -> Vec<u8> {
    let mut b = vec![];
    b.extend(kind.to_be_bytes());
    b.extend([0, 0]);
    b.extend(COOKIE.to_be_bytes());
    b.extend(tx);
    b
}
fn length(b: &mut [u8], len: usize) {
    b[2..4].copy_from_slice(&(len as u16).to_be_bytes())
}
fn attr(b: &mut Vec<u8>, kind: u16, v: &[u8]) {
    b.extend(kind.to_be_bytes());
    b.extend((v.len() as u16).to_be_bytes());
    b.extend(v);
    b.resize(b.len().next_multiple_of(4), 0);
    let n = b.len() - 20;
    length(b, n)
}
fn sign(b: &mut Vec<u8>, key: &hmac::Key) {
    let n = b.len() - 20 + 24;
    length(b, n);
    let tag = hmac::sign(key, b);
    attr(b, MI, tag.as_ref());
}
struct Attributes<'a> {
    items: Vec<(u16, &'a [u8])>,
    integrity: Option<(usize, &'a [u8])>,
}
impl<'a> Attributes<'a> {
    fn parse(b: &'a [u8]) -> Result<Self> {
        let mut a = Self {
            items: vec![],
            integrity: None,
        };
        let mut i = 20;
        while i < b.len() {
            ensure!(i + 4 <= b.len(), "invalid TURN attribute");
            let kind = word(&b[i..]);
            let n = word(&b[i + 2..]) as usize;
            ensure!(
                i + 4 + n.next_multiple_of(4) <= b.len(),
                "invalid TURN attribute"
            );
            let v = &b[i + 4..i + 4 + n];
            if kind == MI {
                a.integrity = Some((i, v));
                break;
            }
            a.items.push((kind, v));
            i += 4 + n.next_multiple_of(4)
        }
        Ok(a)
    }
    fn get(&self, k: u16) -> Option<&'a [u8]> {
        self.items.iter().find(|(n, _)| *n == k).map(|(_, v)| *v)
    }
    fn verify(&self, b: &[u8], key: &hmac::Key) -> bool {
        self.integrity.is_some_and(|(at, tag)| {
            let mut original = b[..at].to_vec();
            length(&mut original, at + 4);
            hmac::verify(key, &original, tag).is_ok()
        })
    }
}
fn failure(kind: u16, tx: &[u8], code: u16, nonce: &str) -> Vec<u8> {
    let mut b = message(kind | 0x110, tx);
    attr(&mut b, 9, &[0, 0, (code / 100) as u8, (code % 100) as u8]);
    if code == 401 {
        attr(&mut b, REALM_ATTR, REALM);
        attr(&mut b, NONCE, nonce.as_bytes())
    }
    b
}
async fn frame(r: &mut (impl tokio::io::AsyncRead + Unpin)) -> Result<Vec<u8>> {
    let mut hdr = [0; 4];
    r.read_exact(&mut hdr).await?;
    let k = word(&hdr);
    let size = word(&hdr[2..]) as usize;
    ensure!(k < 0x8000, "invalid TURN frame");
    let len = if k >= 0x4000 {
        size.next_multiple_of(4)
    } else {
        size + 16
    };
    let mut b = hdr.to_vec();
    b.resize(4 + len, 0);
    r.read_exact(&mut b[4..]).await?;
    Ok(b)
}
pub(crate) async fn run(
    socket: DuplexStream,
    room: Arc<Switch>,
    username: String,
    password: String,
) -> Result<()> {
    let (mut r, mut w) = tokio::io::split(socket);
    let key = hmac::Key::new(
        hmac::HMAC_SHA1_FOR_LEGACY_USE_ONLY,
        &long_term_key(format!("{username}:vantry-desktop:{password}").as_bytes()),
    );
    let nonce = token();
    let (sender, mut receiver) = mpsc::channel::<Datagram>(64);
    let mut allocation: Option<Allocation> = None;
    let mut expires = Instant::now() + Duration::from_secs(60);
    let mut permission = Instant::now();
    let mut channels = HashMap::<u16, (u16, Instant)>::new();
    let mut read = Box::pin(frame(&mut r));
    loop {
        let b = tokio::select! {
            _ = tokio::time::sleep_until(expires) => break,
            packet = receiver.recv() => {
                let Some((from, data)) = packet else { break };
                if permission > Instant::now() {
                    let channel = channels.iter().find(|(_, (port, expiry))| *port == from && *expiry > Instant::now());
                    let bytes = data_frame(channel.map(|(id, _)| *id), from, &data);
                    w.write_all(&bytes).await?;
                }
                continue;
            }
            input = &mut read => input?,
        };
        drop(read);
        read = Box::pin(frame(&mut r));
        let kind = word(&b);
        if kind >= 0x4000 {
            if let (Some(alloc), Some((port, t))) = (&allocation, channels.get(&kind)) {
                if *t > Instant::now() && permission > Instant::now() {
                    room.send(alloc.port, *port, &b[4..4 + word(&b[2..]) as usize]);
                }
            }
            continue;
        }
        ensure!(b[4..8] == COOKIE.to_be_bytes(), "invalid STUN cookie");
        let tx = &b[8..20];
        let a = Attributes::parse(&b)?;
        if kind == 0x16 {
            if let (Some(alloc), Some(p), Some(data)) =
                (&allocation, a.get(PEER).and_then(port), a.get(DATA))
            {
                if permission > Instant::now() && data.len() < 65000 {
                    room.send(alloc.port, p, data)
                }
            }
            continue;
        }
        let mut response = message(kind | 0x100, tx);
        if kind == 1 {
            attr(
                &mut response,
                0x20,
                &address(allocation.as_ref().map(|a| a.port).unwrap_or(3478)),
            );
            w.write_all(&response).await?;
            continue;
        }
        if a.get(USER) != Some(username.as_bytes())
            || a.get(REALM_ATTR) != Some(REALM)
            || a.get(NONCE) != Some(nonce.as_bytes())
            || !a.verify(&b, &key)
        {
            w.write_all(&failure(kind, tx, 401, &nonce)).await?;
            continue;
        }
        let mut close = false;
        match kind {
            3 if allocation.is_none() && a.get(0x19).is_some_and(|v| v == [17, 0, 0, 0]) => {
                let p = room.allocate(sender.clone())?;
                allocation = Some(Allocation {
                    port: p,
                    room: room.clone(),
                });
                attr(&mut response, 0x16, &address(p));
                attr(&mut response, 0x20, &address(p));
                attr(&mut response, 0xd, &600u32.to_be_bytes());
                expires = Instant::now() + Duration::from_secs(600);
            }
            _ if allocation.is_none() => response = failure(kind, tx, 437, &nonce),
            4 => {
                let life = a
                    .get(0xd)
                    .and_then(|v| v.try_into().ok())
                    .map(u32::from_be_bytes)
                    .unwrap_or(600)
                    .min(600);
                attr(&mut response, 0xd, &life.to_be_bytes());
                expires = Instant::now() + Duration::from_secs(life.into());
                close = life == 0
            }
            8 if a.get(PEER).and_then(port).is_some() => {
                permission = Instant::now() + Duration::from_secs(300)
            }
            9 => {
                let peer = a.get(PEER).and_then(port);
                let chan = a.get(0xc).filter(|v| v.len() == 4).map(word);
                if let (Some(p), Some(c @ 0x4000..=0x7fff)) = (peer, chan) {
                    channels.retain(|_, (_, t)| *t > Instant::now());
                    if channels.len() < 128
                        && !channels
                            .iter()
                            .any(|(ch, (old, _))| (*ch == c) != (*old == p))
                    {
                        channels.insert(c, (p, Instant::now() + Duration::from_secs(600)));
                        permission = Instant::now() + Duration::from_secs(300);
                    } else {
                        response = failure(kind, tx, 400, &nonce)
                    }
                } else {
                    response = failure(kind, tx, 400, &nonce)
                }
            }
            _ => response = failure(kind, tx, 400, &nonce),
        }
        sign(&mut response, &key);
        w.write_all(&response).await?;
        if close {
            break;
        }
    }
    Ok(())
}
fn data_frame(channel: Option<u16>, from: u16, data: &[u8]) -> Vec<u8> {
    if let Some(channel) = channel {
        let mut bytes = channel.to_be_bytes().to_vec();
        bytes.extend((data.len() as u16).to_be_bytes());
        bytes.extend(data);
        bytes.resize(bytes.len().next_multiple_of(4), 0);
        bytes
    } else {
        let mut id = [0; 12];
        OsRng.fill_bytes(&mut id);
        let mut b = message(0x17, &id);
        attr(&mut b, PEER, &address(from));
        attr(&mut b, DATA, data);
        b
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn rfc1321_vector() {
        assert_eq!(
            long_term_key(b"abc"),
            [
                0x90, 0x01, 0x50, 0x98, 0x3c, 0xd2, 0x4f, 0xb0, 0xd6, 0x96, 0x3f, 0x7d, 0x28, 0xe1,
                0x7f, 0x72
            ]
        );
    }
    struct Client {
        read: tokio::io::ReadHalf<crate::transport::Stream>,
        write: tokio::io::WriteHalf<crate::transport::Stream>,
        username: String,
        nonce: Vec<u8>,
        key: hmac::Key,
        port: u16,
    }
    impl Client {
        async fn request(
            &mut self,
            kind: u16,
            attrs: &[(u16, &[u8])],
            authenticate: bool,
        ) -> Result<Vec<u8>> {
            let mut m = message(kind, &[9; 12]);
            for (k, v) in attrs {
                attr(&mut m, *k, v)
            }
            if authenticate {
                attr(&mut m, USER, self.username.as_bytes());
                attr(&mut m, REALM_ATTR, REALM);
                attr(&mut m, NONCE, &self.nonce);
                sign(&mut m, &self.key);
            }
            self.write.write_all(&m).await?;
            frame(&mut self.read).await
        }
        async fn allocate(
            socket: impl crate::transport::Socket,
            username: &str,
            password: &str,
        ) -> Result<Self> {
            let (read, write) = tokio::io::split(Box::new(socket) as crate::transport::Stream);
            let key = hmac::Key::new(
                hmac::HMAC_SHA1_FOR_LEGACY_USE_ONLY,
                &long_term_key(format!("{username}:vantry-desktop:{password}").as_bytes()),
            );
            let mut c = Self {
                username: username.to_owned(),
                read,
                write,
                key,
                nonce: Vec::new(),
                port: 0,
            };
            let r = c.request(3, &[], false).await?;
            ensure!(word(&r) == 0x113);
            c.nonce = Attributes::parse(&r)?.get(NONCE).unwrap().to_vec();
            let r = c.request(3, &[(0x19, &[17, 0, 0, 0])], true).await?;
            ensure!(word(&r) == 0x103);
            let a = Attributes::parse(&r)?;
            ensure!(a.verify(&r, &c.key));
            c.port = port(a.get(0x16).unwrap()).unwrap();
            Ok(c)
        }
    }
    #[tokio::test]
    async fn desktop_turn_over_tls_websockets() -> Result<()> {
        use crate::{
            desktop_client::DesktopClient,
            relay::Relay,
            web::{TlsFiles, serve},
        };
        use tokio::net::{TcpListener, TcpStream};
        use tokio_util::sync::CancellationToken;
        tokio::time::timeout(Duration::from_secs(8), async {
            let tmp = tempfile::tempdir()?;
            let key = rcgen::KeyPair::generate()?;
            let p = rcgen::CertificateParams::new(vec!["127.0.0.1".into()])?;
            let cert = p.self_signed(&key)?;
            let tls = TlsFiles {
                certificate: tmp.path().join("cert.pem"),
                private_key: tmp.path().join("key.pem"),
            };
            std::fs::write(&tls.certificate, cert.pem())?;
            std::fs::write(&tls.private_key, key.serialize_pem())?;
            let listener = TcpListener::bind("127.0.0.1:0").await?;
            let url = format!("https://{}", listener.local_addr()?);
            let stop = CancellationToken::new();
            let key = token();
            let server = tokio::spawn(serve(
                listener,
                Arc::new(Relay::new(None, Some(key.clone()))?),
                Some(tls),
                stop.clone(),
            ));
            let client = DesktopClient::with_trust_anchor(
                &url,
                reqwest::Certificate::from_der(cert.der())?,
            )?;
            let mut info = client
                .create(&key, serde_json::json!({"width":640}))
                .await?;
            let id = info["sessionId"].as_str().unwrap().to_string();
            let ht = info["hostToken"].as_str().unwrap().to_string();
            let vt = info["viewerToken"].as_str().unwrap().to_string();
            let invitation = client.invite(&key, &id, &vt, "host").await?;
            let ticket = client.redeem(&invitation.code).await?;
            ensure!(ticket.session_id == id && ticket.token == vt);
            client.request_access(&id, &vt, "viewer").await?;
            client.approve(&id, &ht, true).await?;
            let mut viewer = client.status(&id, &vt).await?;
            let user = viewer["iceServers"][0]["username"]
                .as_str()
                .unwrap()
                .to_owned();
            let password = viewer["iceServers"][0]["credential"]
                .as_str()
                .unwrap()
                .to_owned();
            let host_proxy = client.turn_bridge(&mut info, &ht, stop.clone()).await?;
            let viewer_proxy = client.turn_bridge(&mut viewer, &vt, stop.clone()).await?;
            let mut host = Client::allocate(
                TcpStream::connect(host_proxy.address).await?,
                &user,
                &password,
            )
            .await?;
            let mut viewer = Client::allocate(
                TcpStream::connect(viewer_proxy.address).await?,
                &user,
                &password,
            )
            .await?;
            let (hp, vp) = (host.port, viewer.port);
            for (c, p) in [(&mut host, vp), (&mut viewer, hp)] {
                let reply = c
                    .request(9, &[(PEER, &address(p)), (0xc, &[0x40, 1, 0, 0])], true)
                    .await?;
                ensure!(word(&reply) == 0x109);
            }
            for (from, to, data) in [
                (true, false, &b"encoded video"[..]),
                (false, true, &b"input control"[..]),
            ] {
                let bytes = data_frame(Some(0x4001), 0, data);
                if from && !to {
                    host.write.write_all(&bytes).await?;
                    ensure!(frame(&mut viewer.read).await? == bytes);
                } else {
                    viewer.write.write_all(&bytes).await?;
                    ensure!(frame(&mut host.read).await? == bytes);
                }
            }
            client.approve(&id, &ht, false).await?;
            ensure!(frame(&mut viewer.read).await.is_err());
            client.delete(&id, &ht).await?;
            host_proxy.close().await;
            viewer_proxy.close().await;
            stop.cancel();
            server.await??;
            tmp.close()?;
            Ok::<_, anyhow::Error>(())
        })
        .await?
    }
    #[tokio::test]
    async fn allocations_exchange_channel_data_and_release() -> Result<()> {
        tokio::time::timeout(Duration::from_secs(3), async {
            let room = Arc::new(Switch::default());
            let (a, server_a) = tokio::io::duplex(8192);
            let (b, server_b) = tokio::io::duplex(8192);
            let first = tokio::spawn(run(
                server_a,
                room.clone(),
                "user".into(),
                "password".into(),
            ));
            let second = tokio::spawn(run(
                server_b,
                room.clone(),
                "user".into(),
                "password".into(),
            ));
            let mut a = Client::allocate(a, "user", "password").await?;
            let mut b = Client::allocate(b, "user", "password").await?;
            let a_port = a.port;
            let b_port = b.port;
            for (c, p) in [(&mut a, b_port), (&mut b, a_port)] {
                let r = c.request(8, &[(PEER, &address(p))], true).await?;
                ensure!(word(&r) == 0x108);
                let r = c
                    .request(9, &[(PEER, &address(p)), (0xc, &[0x40, 0, 0, 0])], true)
                    .await?;
                ensure!(word(&r) == 0x109);
            }
            let data = b"\x40\x00\x00\x05media\x00\x00\x00";
            a.write.write_all(data).await?;
            ensure!(frame(&mut b.read).await? == data);
            b.write.write_all(data).await?;
            ensure!(frame(&mut a.read).await? == data);
            for c in [&mut a, &mut b] {
                ensure!(word(&c.request(4, &[(0xd, &[0; 4])], true).await?) == 0x104);
            }
            first.await??;
            second.await??;
            ensure!(room.0.lock().unwrap().is_empty());
            Ok::<_, anyhow::Error>(())
        })
        .await?
    }
}
