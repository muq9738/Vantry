use crate::{
    relay::Relay,
    transport::{Stream, Tasks, header_token},
};
use anyhow::{Result, anyhow, ensure};
use bytes::Bytes;
use futures::TryStreamExt;
use http::{HeaderMap, Request, Response};
use http_body_util::{BodyExt, Full, Limited, combinators::UnsyncBoxBody};
use hyper::{body::Incoming, service::service_fn};
use hyper_util::rt::{TokioExecutor, TokioIo, TokioTimer};
use hyper_util::server::conn::auto::Builder;
use serde::{Serialize, de::DeserializeOwned};
use std::{
    convert::Infallible, io::BufReader, net::SocketAddr, path::PathBuf, sync::Arc, time::Duration,
};
use tokio::net::TcpListener;
use tokio_rustls::TlsAcceptor;
use tokio_util::sync::CancellationToken;

pub(crate) type Body = UnsyncBoxBody<Bytes, Box<dyn std::error::Error + Send + Sync>>;
pub(crate) type Reply = Response<Body>;
pub(crate) fn full(data: Bytes) -> Body {
    Full::new(data)
        .map_err(|e: Infallible| match e {})
        .boxed_unsync()
}
pub(crate) fn response(status: u16, data: Bytes) -> Reply {
    Response::builder().status(status).body(full(data)).unwrap()
}
pub(crate) fn json(status: u16, value: impl Serialize) -> Reply {
    let mut r = response(
        status,
        serde_json::to_vec(&value)
            .expect("serializable wire response")
            .into(),
    );
    r.headers_mut()
        .insert("content-type", "application/json".parse().unwrap());
    r.headers_mut()
        .insert("cache-control", "no-store".parse().unwrap());
    r
}
pub(crate) fn error(status: u16) -> Reply {
    response(
        status,
        Bytes::from_static(
            http::StatusCode::from_u16(status)
                .ok()
                .and_then(|s| s.canonical_reason())
                .unwrap_or("Request failed")
                .as_bytes(),
        ),
    )
}
pub(crate) async fn decode<T: DeserializeOwned>(req: Request<Incoming>) -> Result<T, u16> {
    let body = tokio::time::timeout(
        Duration::from_secs(10),
        Limited::new(req.into_body(), 32 << 10).collect(),
    )
    .await
    .map_err(|_| 408u16)?
    .map_err(|_| 400u16)?;
    serde_json::from_slice(&body.to_bytes()).map_err(|_| 400)
}
pub(crate) fn bearer(headers: &HeaderMap) -> &str {
    headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("")
}
pub(crate) fn query(req: &Request<Incoming>, key: &str) -> String {
    url::form_urlencoded::parse(req.uri().query().unwrap_or("").as_bytes())
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.into_owned())
        .unwrap_or_default()
}
#[derive(Clone)]
pub struct TlsFiles {
    pub certificate: PathBuf,
    pub private_key: PathBuf,
}
impl TlsFiles {
    pub fn load(&self) -> Result<Arc<rustls::ServerConfig>> {
        let chain =
            rustls_pemfile::certs(&mut BufReader::new(std::fs::File::open(&self.certificate)?))
                .collect::<std::io::Result<Vec<_>>>()?;
        let key = rustls_pemfile::private_key(&mut BufReader::new(std::fs::File::open(
            &self.private_key,
        )?))?
        .ok_or_else(|| anyhow!("missing TLS key"))?;
        let mut cfg = rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()?
        .with_no_client_auth()
        .with_single_cert(chain, key)?;
        cfg.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
        Ok(Arc::new(cfg))
    }
}

pub async fn serve_acme(
    listener: TcpListener,
    relay: Arc<Relay>,
    manager: Arc<crate::acme::Acme>,
    stop: CancellationToken,
) -> Result<()> {
    serve_with(listener, relay, None, Some(manager), stop).await
}
pub async fn serve(
    listener: TcpListener,
    relay: Arc<Relay>,
    tls: Option<TlsFiles>,
    stop: CancellationToken,
) -> Result<()> {
    serve_with(listener, relay, tls, None, stop).await
}
pub(crate) fn http_server() -> Builder<TokioExecutor> {
    let mut server = Builder::new(TokioExecutor::new());
    server
        .http1()
        .timer(TokioTimer::new())
        .max_buf_size(32768)
        .header_read_timeout(Duration::from_secs(10));
    server
        .http2()
        .timer(TokioTimer::new())
        .max_header_list_size(32768)
        .keep_alive_interval(Duration::from_secs(20))
        .keep_alive_timeout(Duration::from_secs(10));
    server
}
async fn serve_with(
    listener: TcpListener,
    relay: Arc<Relay>,
    tls: Option<TlsFiles>,
    acme: Option<Arc<crate::acme::Acme>>,
    stop: CancellationToken,
) -> Result<()> {
    if let Some(tls) = &tls {
        tls.load()?;
    }
    let tasks = Tasks::new(stop.clone());
    let result = loop {
        let accepted =
            tokio::select! {biased;_=stop.cancelled()=>break Ok(()),a=listener.accept()=>a};
        let (socket, addr) = match accepted {
            Ok(a) => a,
            Err(e) => break Err(e.into()),
        };
        let relay = relay.clone();
        let tls = tls.clone();
        let group = tasks.clone();
        let manager = acme.clone();
        tasks.spawn(async move {
            let stream: Stream = if let Some(m) = manager {
                let Ok(io) = m.accept(socket, group.clone()).await else {
                    return;
                };
                io
            } else if let Some(files) = tls {
                let Ok(Ok(cfg)) = tokio::task::spawn_blocking(move || files.load()).await else {
                    return;
                };
                let Ok(Ok(tls)) = tokio::time::timeout(
                    Duration::from_secs(10),
                    TlsAcceptor::from(cfg).accept(socket),
                )
                .await
                else {
                    return;
                };
                Box::new(tls)
            } else {
                Box::new(socket)
            };
            let handler = service_fn(move |req| {
                let relay = relay.clone();
                let tasks = group.clone();
                async move {
                    Ok::<_, Infallible>(relay.handle(req, addr, tasks).await.unwrap_or_else(error))
                }
            });
            let _ = http_server()
                .serve_connection_with_upgrades(TokioIo::new(stream), handler)
                .await;
        });
    };
    tasks.close().await;
    result
}
pub(crate) fn strip_hop(headers: &mut HeaderMap, upgrade: bool) {
    let names: Vec<String> = headers
        .get_all("connection")
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|v| v.split(','))
        .map(|v| v.trim().to_owned())
        .collect();
    for name in names {
        if !upgrade || !name.eq_ignore_ascii_case("upgrade") {
            headers.remove(name);
        }
    }
    for k in [
        "connection",
        "proxy-connection",
        "proxy-authenticate",
        "proxy-authorization",
        "keep-alive",
        "transfer-encoding",
        "te",
        "trailer",
    ] {
        headers.remove(k);
    }
    if upgrade {
        headers.insert("connection", "Upgrade".parse().unwrap());
    } else {
        headers.remove("upgrade");
    }
}
pub(crate) type Sender = hyper::client::conn::http1::SendRequest<Body>;
pub(crate) async fn sender(io: Stream, tasks: &Tasks) -> Result<Sender> {
    let (tx, conn) = hyper::client::conn::http1::handshake(TokioIo::new(io)).await?;
    tasks.spawn(async move {
        let _ = conn.with_upgrades().await;
    });
    Ok(tx)
}
pub(crate) async fn proxy(req: Request<Body>, io: Stream, tasks: &Tasks) -> Result<Reply> {
    let mut tx = sender(io, tasks).await?;
    forward(req, &mut tx, tasks).await
}
pub(crate) async fn forward(
    mut req: Request<Body>,
    tx: &mut Sender,
    tasks: &Tasks,
) -> Result<Reply> {
    let tunnel = header_token(req.headers(), "connection", "upgrade")
        && req.headers().contains_key("upgrade");
    let downstream = tunnel.then(|| hyper::upgrade::on(&mut req));
    strip_hop(req.headers_mut(), tunnel);
    if !req.headers().contains_key("host")
        && let Some(host) = req.uri().authority().map(|a| a.as_str().to_string())
    {
        req.headers_mut().insert("host", host.parse()?);
    }
    *req.version_mut() = http::Version::HTTP_11;
    *req.uri_mut() = req
        .uri()
        .path_and_query()
        .map(|v| v.as_str())
        .unwrap_or("/")
        .parse()?;
    let mut res = tx.send_request(req).await?;
    let switching = res.status() == 101;
    ensure!(!switching || tunnel, "unexpected upgrade");
    strip_hop(res.headers_mut(), switching);
    if let Some(down) = downstream.filter(|_| switching) {
        let up = hyper::upgrade::on(&mut res);
        tasks.spawn(async move {
            if let Ok((a, b)) = tokio::try_join!(down, up) {
                let _ =
                    tokio::io::copy_bidirectional(&mut TokioIo::new(a), &mut TokioIo::new(b)).await;
            }
        });
    }
    Ok(res.map(|b| b.map_err(Into::into).boxed_unsync()))
}
pub(crate) async fn proxy_target(
    mut req: Request<Incoming>,
    target: &url::Url,
    client: &reqwest::Client,
    tasks: &Tasks,
    addr: SocketAddr,
) -> Result<Reply> {
    let upgrade = header_token(req.headers(), "connection", "upgrade")
        && req.headers().contains_key("upgrade");
    let down = upgrade.then(|| hyper::upgrade::on(&mut req));
    strip_hop(req.headers_mut(), upgrade);
    let mut url = target.clone();
    url.set_path(&format!(
        "{}{}",
        target.path().trim_end_matches('/'),
        req.uri().path()
    ));
    let query = [target.query(), req.uri().query()]
        .into_iter()
        .flatten()
        .collect::<Vec<_>>()
        .join("&");
    url.set_query((!query.is_empty()).then_some(&query));
    let (parts, body) = req.into_parts();
    let mut headers = parts.headers;
    headers.append("x-forwarded-for", addr.ip().to_string().parse()?);
    let res = client
        .request(parts.method, url)
        .headers(headers)
        .body(reqwest::Body::wrap_stream(body.into_data_stream()))
        .send()
        .await
        .map_err(|_| anyhow!("upstream unavailable"))?;
    let status = res.status();
    let mut headers = res.headers().clone();
    strip_hop(&mut headers, status == 101);
    let body = if status == 101 {
        let down = down.ok_or_else(|| anyhow!("unexpected upgrade"))?;
        let mut up = res.upgrade().await?;
        tasks.spawn(async move {
            if let Ok(d) = down.await {
                let _ = tokio::io::copy_bidirectional(&mut TokioIo::new(d), &mut up).await;
            }
        });
        full(Bytes::new())
    } else {
        http_body_util::StreamBody::new(
            res.bytes_stream()
                .map_ok(hyper::body::Frame::data)
                .map_err(Into::into),
        )
        .boxed_unsync()
    };
    let mut result = Response::new(body);
    *result.status_mut() = status;
    *result.headers_mut() = headers;
    Ok(result)
}
