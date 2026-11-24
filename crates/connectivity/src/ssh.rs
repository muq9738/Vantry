//! An owned OpenSSH StreamLocal forward for an existing Vantry Core Unix socket.
use anyhow::{Result, anyhow, bail, ensure};
use serde_json::Value;
use std::{
    path::{Path, PathBuf},
    process::Stdio,
    time::Duration,
};
#[cfg(unix)]
use tokio::net::UnixStream;
use tokio::{
    process::Command,
    sync::watch,
    task::JoinHandle,
    time::{sleep, timeout},
};
use tokio_util::sync::CancellationToken;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum State {
    Connecting,
    Online,
    Reconnecting,
    Stopped,
}
pub struct SshForward {
    socket: PathBuf,
    state: watch::Receiver<State>,
    stop: CancellationToken,
    worker: Option<JoinHandle<Result<()>>>,
}
impl SshForward {
    pub async fn connect(
        host: &str,
        remote_socket: &Path,
        parent: CancellationToken,
    ) -> Result<Self> {
        Self::start(Path::new("ssh"), host, remote_socket, parent).await
    }
    async fn start(
        executable: &Path,
        host: &str,
        remote_socket: &Path,
        parent: CancellationToken,
    ) -> Result<Self> {
        ensure!(
            !host.is_empty()
                && !host.starts_with('-')
                && !host.chars().any(|c| c.is_control() || c.is_whitespace()),
            "invalid SSH host"
        );
        let remote = remote_socket
            .to_str()
            .ok_or_else(|| anyhow!("invalid remote socket"))?;
        ensure!(
            remote_socket.is_absolute() && !remote.contains([':', '\n', '\r', '\0']),
            "invalid remote socket"
        );
        ensure!(!parent.is_cancelled(), "forward cancelled");
        let dir = tempfile::Builder::new().prefix("vantry-relay-").tempdir()?;
        let socket = dir.path().join("core.sock");
        #[cfg(unix)]
        let spec = format!("{}:{remote}", socket.display());
        #[cfg(not(unix))]
        let spec = {
            let probe = std::net::TcpListener::bind("127.0.0.1:0")?;
            let addr = probe.local_addr()?;
            drop(probe);
            let ep = vantry_protocol::LocalTcpEndpoint::new(
                addr.to_string(),
                format!("{}{}", uuid::Uuid::new_v4().simple(), uuid::Uuid::new_v4().simple()),
            );
            std::fs::write(&socket, serde_json::to_vec(&ep)?)?;
            format!("{addr}:{remote}")
        };
        let host = host.to_owned();
        let exe = executable.to_owned();
        let local = socket.clone();
        let stop = parent.child_token();
        let cancel = stop.clone();
        let (state, rx) = watch::channel(State::Connecting);
        let worker = tokio::spawn(async move {
            let result = async {
                loop {
                    if cancel.is_cancelled() { break }
                    let mut child = Command::new(&exe)
                        .args(["-N", "-T", "-S", "none", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
                            "-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes", "-o", "ForkAfterAuthentication=no",
                            "-o", "ConnectTimeout=10", "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=15",
                            "-o", "ServerAliveCountMax=3", "-L", &spec, "--", &host])
                        .stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null()).kill_on_drop(true).spawn()?;
                    let ready = async {
                        loop {
                            if let Ok(Ok(v)) = timeout(Duration::from_millis(300),probe(&local)).await
                                && v["version"] == 1 && v["state"] == "ready" { break }
                            sleep(Duration::from_millis(100)).await;
                        }
                    };
                    let exited = tokio::select! {
                        _ = cancel.cancelled() => false,
                        status = child.wait() => {status?; true},
                        _ = ready => {
                            state.send_replace(State::Online);
                            tokio::select! { _ = cancel.cancelled() => false, status = child.wait() => {status?; true} }
                        }
                    };
                    if !exited { let _ = child.start_kill(); child.wait().await?; break }
                    state.send_replace(State::Reconnecting);
                    if let Err(e) = std::fs::remove_file(&local) && e.kind()!=std::io::ErrorKind::NotFound {return Err(e.into())}
                    tokio::select! { _ = cancel.cancelled() => break, _ = sleep(Duration::from_secs(1)) => {} }
                }
                Ok::<_,anyhow::Error>(())
            }.await;
            dir.close()?;
            state.send_replace(State::Stopped);
            result
        });
        let mut result = Self {
            socket,
            state: rx,
            stop: stop.clone(),
            worker: Some(worker),
        };
        let online = tokio::select! {_=stop.cancelled()=>false,r=timeout(Duration::from_secs(12),result.state.wait_for(|state|matches!(state,State::Online|State::Stopped)))=>matches!(r,Ok(Ok(state)) if *state==State::Online)};
        if !online {
            result.close().await?;
            bail!("SSH forward failed to reach a ready Core")
        }
        Ok(result)
    }
    pub fn socket(&self) -> &Path {
        &self.socket
    }
    pub fn state(&self) -> watch::Receiver<State> {
        self.state.clone()
    }
    pub async fn close(mut self) -> Result<()> {
        self.stop.cancel();
        if let Some(worker) = self.worker.take() {
            worker.await??;
        }
        Ok(())
    }
}
impl Drop for SshForward {
    fn drop(&mut self) {
        self.stop.cancel()
    }
}
#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    #[tokio::test]
    async fn owned_forward_reaps_foreground_child_and_removes_socket() -> Result<()> {
        let dir = tempfile::tempdir()?;
        let exe = dir.path().join("ssh-fixture");
        std::fs::write(
            &exe,
            "#!/bin/sh\nwhile [ \"$1\" != -L ]; do shift; done\nshift\nprintf '%s\\n' \"$$\" \"$1\" > \"${0%/*}/arguments\"\nexec /bin/sleep 30\n",
        )?;
        std::fs::set_permissions(&exe, std::fs::Permissions::from_mode(0o700))?;
        let stop = CancellationToken::new();
        let owner = stop.clone();
        let connect = tokio::spawn(async move {
            SshForward::start(&exe, "local-fixture", Path::new("/remote/core.sock"), owner).await
        });
        let args = timeout(Duration::from_secs(3), async {
            loop {
                if let Ok(v) = std::fs::read_to_string(dir.path().join("arguments"))
                    && v.lines().count() == 2
                {
                    break v;
                }
                sleep(Duration::from_millis(10)).await;
            }
        })
        .await?;
        let mut lines = args.lines();
        let pid = lines.next().unwrap();
        let socket = PathBuf::from(
            lines
                .next()
                .unwrap()
                .strip_suffix(":/remote/core.sock")
                .unwrap(),
        );
        let listener = tokio::net::UnixListener::bind(&socket)?;
        let (mut io, _) = timeout(Duration::from_secs(2), listener.accept()).await??;
        let mut buffer = [0; 1024];
        io.read(&mut buffer).await?;
        let body = br#"{"version":1,"state":"ready"}"#;
        io.write_all(
            format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            )
            .as_bytes(),
        )
        .await?;
        io.write_all(body).await?;
        drop(io);
        drop(listener);
        let forward = connect.await??;
        ensure!(*forward.state().borrow() == State::Online);
        forward.close().await?;
        ensure!(!socket.parent().unwrap().exists());
        let alive = std::process::Command::new("kill")
            .args(["-0", pid])
            .stderr(Stdio::null())
            .status()?;
        ensure!(!alive.success());
        dir.close()?;
        Ok(())
    }
}
async fn probe(path: &Path) -> Result<Value> {
    use bytes::Bytes;
    use http_body_util::{BodyExt, Empty, Limited};
    let (io, token): (crate::transport::Stream, Option<String>) =
        if let Ok(bytes) = std::fs::read(path)
            && let Some(ep) = vantry_protocol::LocalTcpEndpoint::parse_bytes(&bytes)
        {
            (
                Box::new(tokio::net::TcpStream::connect(&ep.addr).await?),
                Some(ep.token),
            )
        } else {
            #[cfg(unix)]
            {
                (Box::new(UnixStream::connect(path).await?), None)
            }
            #[cfg(not(unix))]
            {
                bail!("local SSH forward endpoint unavailable")
            }
        };
    let (mut tx, conn) =
        hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(io)).await?;
    let request = async move {
        let mut req = http::Request::get("/v1/health")
            .header("Host", "vantry.local")
            .header("Connection", "close");
        if let Some(t) = token {
            req = req.header(vantry_protocol::LOCAL_TOKEN_HEADER, t);
        }
        let res = tx.send_request(req.body(Empty::<Bytes>::new())?).await?;
        ensure!(res.status().is_success(), "Core unavailable");
        let data = Limited::new(res.into_body(), 4096)
            .collect()
            .await
            .map_err(|_| anyhow!("invalid Core health response"))?;
        Ok::<_, anyhow::Error>(serde_json::from_slice(&data.to_bytes())?)
    };
    let (_, v) = tokio::try_join!(async { conn.await.map_err(anyhow::Error::from) }, request)?;
    Ok(v)
}
