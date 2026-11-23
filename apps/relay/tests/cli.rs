use anyhow::{Result, ensure};
use serde_json::Value;
use std::{
    fs,
    io::{BufRead, BufReader, Read, Write},
    net::TcpStream,
    process::{Child, Command, Stdio},
    time::Duration,
};
const BIN: &str = env!("CARGO_BIN_EXE_vantry-relay");
struct Process(Child);
impl Drop for Process {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
#[test]
fn config_initialization_validation_and_version() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let path = tmp.path().join("relay.json");
    let run = || {
        Command::new(BIN)
            .args(["init", "--desktop", "--config"])
            .arg(&path)
            .output()
    };
    let init = run()?;
    ensure!(init.status.success());
    let cfg: Value = serde_json::from_slice(&fs::read(&path)?)?;
    let session = cfg["sessions"]["registrationKey"].as_str().unwrap();
    let desktop = cfg["desktop"]["registrationKey"].as_str().unwrap();
    ensure!(
        session != desktop
            && session.len() >= 32
            && !String::from_utf8_lossy(&init.stdout).contains(session)
    );
    ensure!(!run()?.status.success());
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        ensure!(path.metadata()?.permissions().mode() & 0o777 == 0o600);
    }
    ensure!(Command::new(BIN).arg("version").output()?.status.success());
    let yaml = tmp.path().join("relay.yaml");
    fs::write(
        &yaml,
        format!(
            "listen: 127.0.0.1:0\nsessions:\n  registrationKey: '{session}'\n  routes: {{}}\n  hostCredentials:\n    - key: '{desktop}'\n      routes: [route_0123456789abcdef]\n"
        ),
    )?;
    let check = || {
        Command::new(BIN)
            .arg("check-config")
            .env("VANTRY_RELAY_CONFIG", &yaml)
            .output()
    };
    ensure!(check()?.status.success());
    fs::write(
        &yaml,
        format!("listen: 0.0.0.0:9000\nsessions:\n  registrationKey: {session}\n"),
    )?;
    let bad = check()?;
    ensure!(!bad.status.success() && !String::from_utf8_lossy(&bad.stderr).contains(session));
    tmp.close()?;
    Ok(())
}
#[cfg(unix)]
#[test]
fn systemd_entry_exits_on_signals() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let config = tmp.path().join("relay.json");
    fs::write(
        &config,
        serde_json::to_vec(
            &serde_json::json!({"listen":"127.0.0.1:0","sessions":{"registrationKey":"x".repeat(32)}}),
        )?,
    )?;
    for signal in ["-TERM", "-INT"] {
        let mut proc = Process(
            Command::new(BIN)
                .args(["serve", "--config"])
                .arg(&config)
                .stderr(Stdio::piped())
                .spawn()?,
        );
        let stderr = proc.0.stderr.take().unwrap();
        let (tx, rx) = std::sync::mpsc::channel();
        let reader = std::thread::spawn(move || {
            let mut line = String::new();
            let _ = BufReader::new(stderr).read_line(&mut line);
            let _ = tx.send(line);
        });
        let ready = rx.recv_timeout(Duration::from_secs(4))?;
        reader.join().unwrap();
        let addr = ready
            .split_whitespace()
            .nth(4)
            .ok_or_else(|| anyhow::anyhow!("missing listen address"))?;
        let mut s = TcpStream::connect(addr)?;
        s.set_read_timeout(Some(Duration::from_secs(2)))?;
        s.write_all(b"GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")?;
        let mut response = String::new();
        s.read_to_string(&mut response)?;
        ensure!(response.contains("200 OK") && response.contains("vantry-relay"));
        ensure!(
            Command::new("kill")
                .args([signal, &proc.0.id().to_string()])
                .status()?
                .success()
        );
        let start = std::time::Instant::now();
        loop {
            if let Some(status) = proc.0.try_wait()? {
                ensure!(status.success());
                break;
            }
            ensure!(
                start.elapsed() < Duration::from_secs(3),
                "shutdown timed out"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    tmp.close()?;
    Ok(())
}
