//! Run the imported provider in Pi's embedded engine. Rust owns the listener,
//! request authentication, streaming and lifetime of this child process.
use super::{account::Account, plugin::Plugin};
use anyhow::{Context, Result, bail, ensure};
use bytes::Bytes;
use clap::Args;
use futures::{StreamExt, stream};
use http_body_util::{BodyExt, Full, Limited, StreamBody, combinators::UnsyncBoxBody};
use hyper::{
    Request, Response,
    body::{Frame, Incoming},
    server::conn::http1,
    service::service_fn,
};
use hyper_util::rt::TokioIo;
use pi_js::{HostValue, JsRef, JsRuntime, JsRuntimeOptions};
use serde_json::{Value, json};
use std::{
    convert::Infallible,
    io,
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use vantry_platform::hostfs;

#[derive(Args)]
pub struct HostArgs {
    #[arg(long)]
    pub plugin: PathBuf,
    #[arg(long)]
    pub dir: PathBuf,
    #[arg(long, default_value = "")]
    pub plugin_version: String,
    #[arg(long, default_value_t = 0)]
    pub port: u16,
    #[arg(long)]
    pub selftest: bool,
}

type Body = UnsyncBoxBody<Bytes, io::Error>;
fn answer(status: u16, value: Value) -> Response<Body> {
    Response::builder()
        .status(status)
        .header("content-type", "application/json; charset=utf-8")
        .body(
            Full::new(Bytes::from(value.to_string()))
                .map_err(|never| match never {})
                .boxed_unsync(),
        )
        .unwrap()
}

#[derive(Default)]
struct Activity {
    streams: usize,
    last: Option<Instant>,
}
struct Host {
    account: Account,
    bridge: Plugin,
    version: String,
    control: String,
    inference: String,
    activity: Arc<Mutex<Activity>>,
    stop: CancellationToken,
}
struct StreamGuard {
    activity: Arc<Mutex<Activity>>,
    controller: JsRef,
    reader: Option<JsRef>,
}
impl Drop for StreamGuard {
    fn drop(&mut self) {
        let mut a = self.activity.lock().unwrap();
        a.streams -= 1;
        a.last = Some(Instant::now());
        let controller = self.controller.clone();
        let reader = self.reader.take();
        tokio::spawn(async move {
            let _ = controller.call_method("abort", vec![]).await;
            if let Some(reader) = reader {
                let _ = reader.call_method("cancel", vec![]).await;
            }
        });
    }
}
impl Host {
    async fn request(&self, req: Request<Incoming>) -> Result<Response<Body>> {
        let path = req.uri().path().to_owned();
        let control = path.starts_with("/control/");
        let expected = if control {
            &self.control
        } else {
            &self.inference
        };
        let token = req
            .headers()
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .and_then(|s| s.strip_prefix("Bearer "));
        if token != Some(expected.as_str()) {
            return Ok(answer(401, json!({"error":{"message":"unauthorized"}})));
        }
        let method = req.method().clone();
        let session = ["x-magpie-qoder-session", "x-session-id", "session_id"]
            .iter()
            .find_map(|key| req.headers().get(*key)?.to_str().ok())
            .unwrap_or_default()
            .chars()
            .take(128)
            .collect::<String>();
        let body = match Limited::new(req.into_body(), 64 << 20).collect().await {
            Ok(collected) => collected.to_bytes(),
            Err(_) => {
                return Ok(answer(
                    413,
                    json!({"error":{"message":"request too large"}}),
                ));
            }
        };
        let value = match (method.as_str(), path.as_str()) {
            ("GET", "/control/health") => {
                let pending = self.account.pending();
                let a = self.activity.lock().unwrap();
                json!({"ok":true,"protocol":super::PROTOCOL,"pluginVersion":self.version,"pid":std::process::id(),"loginPending":pending,"idleMs":a.last.map(|i|i.elapsed().as_millis()),"activeStreams":a.streams,"draining":self.stop.is_cancelled()})
            }
            ("GET", "/control/account") => self.account.snapshot(),
            ("POST", "/control/refresh") => {
                self.account.refresh(&self.bridge).await;
                self.account.snapshot()
            }
            ("POST", "/control/login") => {
                let input = if body.is_empty() {
                    json!({})
                } else {
                    serde_json::from_slice(&body)?
                };
                self.account.login(&self.bridge, input).await?
            }
            ("POST", "/control/login/cancel") => {
                self.account.cancel();
                self.account.snapshot()
            }
            ("POST", "/control/logout") => {
                self.account.logout().await?;
                self.account.snapshot()
            }
            ("POST", "/control/shutdown") => {
                self.stop.cancel();
                json!({"ok":true})
            }
            ("GET", "/v1/models") => {
                let account = self.account.snapshot();
                let models: Vec<_> = account["models"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .map(|m| json!({"id":m["id"],"object":"model","owned_by":"qoder"}))
                    .collect();
                json!({"object":"list","data":models})
            }
            ("POST", "/v1/chat/completions") => {
                return self.chat(std::str::from_utf8(&body)?, session).await;
            }
            _ => return Ok(answer(404, json!({"error":{"message":"not found"}}))),
        };
        Ok(answer(200, value))
    }
    async fn chat(&self, body: &str, session: String) -> Result<Response<Body>> {
        if !self.account.signed_in() {
            return Ok(answer(
                401,
                json!({"error":{"message":"Qoder: not signed in"}}),
            ));
        }
        let epoch = self.account.epoch();
        let controller = self.bridge.abort_controller().await?;
        {
            let mut a = self.activity.lock().unwrap();
            a.streams += 1;
            a.last = Some(Instant::now());
        }
        let mut guard = StreamGuard {
            activity: self.activity.clone(),
            controller,
            reader: None,
        };
        let upstream = self.bridge.fetch(body, session, &guard.controller).await?;
        let status = upstream
            .get("status")
            .await?
            .into_json()
            .as_u64()
            .context("missing response status")? as u16;
        let headers = upstream.get_ref("headers").await?;
        let header = |name: &'static str| headers.call_method("get", vec![name.into()]);
        let mut response = Response::builder().status(status).header(
            "content-type",
            header("content-type")
                .await?
                .as_str()
                .unwrap_or("application/json"),
        );
        if let Some(retry) = header("retry-after").await?.as_str() {
            response = response.header("retry-after", retry);
        }
        if status == 401 && header("x-magpie-sign-in").await?.as_str() == Some("expired") {
            self.account.expired(epoch);
        }
        let body = upstream.get_ref("body").await?;
        let inspect = body.clone();
        if body
            .handle()
            .with(move |scope| Ok(scope.value(&inspect)?.is_object()))
            .await?
        {
            guard.reader = Some(body.call_method_ref("getReader", vec![]).await?);
        }
        let chunks = stream::try_unfold(guard, |guard| async move {
            let Some(reader) = guard.reader.as_ref() else {
                return Ok(None);
            };
            let chunk = reader
                .call_method_ref("read", vec![])
                .await
                .map_err(io::Error::other)?;
            match chunk.get("value").await.map_err(io::Error::other)? {
                HostValue::Bytes(bytes) => Ok(Some((Bytes::from(bytes), guard))),
                HostValue::Undefined => Ok(None),
                _ => Err(io::Error::other("invalid stream chunk")),
            }
        });
        Ok(response.body(StreamBody::new(chunks.map(|r| r.map(Frame::data))).boxed_unsync())?)
    }
}

pub async fn run(args: HostArgs) -> Result<()> {
    let mut opts = JsRuntimeOptions::new(&args.dir);
    opts.tokio = Some(tokio::runtime::Handle::current());
    // The plugin sees only the environment it needs; service control tokens stay in Rust.
    opts.env = Some(
        std::env::vars()
            .filter(|(k, _)| k != "VANTRY_QODER_INFERENCE_TOKEN")
            .collect(),
    );
    let account = Account::open(args.dir.join("credentials.json"), args.selftest)?;
    let runtime = JsRuntime::new(opts)?;
    let plugin = runtime
        .import(args.plugin.canonicalize()?.to_string_lossy().to_string())
        .await?;
    let bridge = Plugin::new(plugin.get_ref("QoderAuthPlugin").await?, account.clone());
    bridge.check(args.selftest).await?;
    if args.selftest {
        println!("{}", json!({"ok":true,"protocol":super::PROTOCOL}));
        return Ok(());
    }
    let inference = std::env::var("VANTRY_QODER_INFERENCE_TOKEN")?;
    ensure!(inference.len() >= 32, "missing inference token");
    let listener = match TcpListener::bind(("127.0.0.1", args.port)).await {
        Ok(l) => l,
        Err(_) if args.port != 0 => TcpListener::bind(("127.0.0.1", 0)).await?,
        Err(e) => bail!(e),
    };
    let control = uuid::Uuid::new_v4().simple().to_string();
    hostfs::atomic_write(
        &args.dir.join("host.json"),
        &serde_json::to_vec(
            &json!({"protocol":super::PROTOCOL,"port":listener.local_addr()?.port(),"pid":std::process::id(),"pluginVersion":args.plugin_version,"controlToken":control}),
        )?,
    )?;
    let host = Arc::new(Host {
        account,
        bridge,
        version: args.plugin_version,
        control,
        inference,
        activity: Default::default(),
        stop: CancellationToken::new(),
    });
    let service = host.clone();
    let maintenance = tokio::spawn(async move {
        let mut models = tokio::time::interval(Duration::from_secs(1800));
        let mut renew = tokio::time::interval(Duration::from_secs(60));
        loop {
            tokio::select! {
                _ = service.stop.cancelled() => break,
                _ = models.tick() => service.account.refresh(&service.bridge).await,
                _ = renew.tick() => service.account.renew(&service.bridge).await,
            }
        }
    });
    let mut connections = tokio::task::JoinSet::new();
    loop {
        tokio::select! {
            _ = host.stop.cancelled() => break,
            Some(_) = connections.join_next(), if !connections.is_empty() => {},
            socket = listener.accept() => {
                let (socket, _) = socket?;
                let host = host.clone();
                connections.spawn(async move {
                    let stop = host.stop.clone();
                    let route = service_fn(move |request| {
                        let host = host.clone();
                        async move {
                            let response = host.request(request).await.unwrap_or_else(|error| {
                                answer(502, json!({"error": {"message": error.to_string()}}))
                            });
                            Ok::<_, Infallible>(response)
                        }
                    });
                    let connection = http1::Builder::new().serve_connection(TokioIo::new(socket), route);
                    tokio::pin!(connection);
                    tokio::select! {
                        _ = &mut connection => {},
                        _ = stop.cancelled() => {
                            connection.as_mut().graceful_shutdown();
                            let _ = connection.await;
                        }
                    }
                });
            }
        }
    }
    while connections.join_next().await.is_some() {}
    maintenance.abort();
    Ok(())
}
