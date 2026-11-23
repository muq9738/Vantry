use anyhow::{Result, ensure};
use rcgen::{
    BasicConstraints, CertificateParams, ExtendedKeyUsagePurpose, IsCa, KeyPair, KeyUsagePurpose,
    PKCS_ED25519,
};
use serde_json::json;
use std::{sync::Arc, time::Duration};
use tokio::net::{TcpListener, TcpStream};
use tokio_rustls::{TlsAcceptor, TlsConnector};
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    acme::Acme,
    identity::{Configuration, IdentityStore, Pair, client_tls},
    pairing::token,
    relay::{Relay, SessionConfig},
    serve_acme,
};
#[tokio::test]
async fn go_ca_leaf_compatibility_and_expiry() -> Result<()> {
    let key = KeyPair::generate_for(&PKCS_ED25519)?;
    let mut p = CertificateParams::new(vec!["vantry.remote".into()])?;
    p.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    p.key_usages = vec![
        KeyUsagePurpose::DigitalSignature,
        KeyUsagePurpose::KeyCertSign,
    ];
    let certificate = p.clone().self_signed(&key)?.pem();
    let cfg = Configuration {
        host: Pair {
            certificate: certificate.clone(),
            ..Default::default()
        },
        private_key: key.serialize_pem(),
        ..Default::default()
    };
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let tls = TlsAcceptor::from(cfg.server_tls(false)?);
    let server = tokio::spawn(async move {
        tls.accept(listener.accept().await?.0).await?;
        Ok::<_, anyhow::Error>(())
    });
    let mut peer = Pair {
        v: 1,
        name: "Go host".into(),
        certificate,
        route: token(),
        secret: token(),
        direct: vec![addr.to_string()],
        ..Default::default()
    };
    Pair::parse(&peer.link(), false)?;
    let connector = TlsConnector::from(client_tls(&peer, None)?);
    let _socket = connector
        .connect("vantry.remote".try_into()?, TcpStream::connect(addr).await?)
        .await?;
    server.await??;
    p.not_before = rcgen::date_time_ymd(2010, 1, 1);
    p.not_after = rcgen::date_time_ymd(2011, 1, 1);
    peer.certificate = p.self_signed(&key)?.pem();
    ensure!(Pair::parse(&peer.link(), false).is_err());
    Ok(())
}
#[test]
fn identity_updates_preserve_unknown_config_and_private_files() -> Result<()> {
    let tmp = tempfile::tempdir()?;
    let path = tmp.path().join("remote-access.json");
    std::fs::write(
        &path,
        serde_json::to_vec(
            &json!({"enabled":true,"directPort":5432,"registrationKey":token(),"futureOption":{"enabled":true},"peers":[]}),
        )?,
    )?;
    let store = IdentityStore::open(tmp.path(), "test")?;
    let initial = store.snapshot();
    let next = store.renew_certificate()?;
    ensure!(next != initial.host.certificate);
    store.activate_certificate()?;
    drop(store);
    let cfg = IdentityStore::open(tmp.path(), "different")?.snapshot();
    ensure!(
        cfg.host.name == "test"
            && cfg.host.certificate == next
            && cfg.host.device_id == initial.host.device_id
            && cfg.host.route == initial.host.route
            && cfg.host.secret == initial.host.secret
    );
    ensure!(cfg.extra["futureOption"]["enabled"] == true && cfg.extra["directPort"] == 5432);
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        ensure!(path.metadata()?.permissions().mode() & 0o777 == 0o600);
    }
    ensure!(std::fs::read_dir(tmp.path())?.count() == 1);
    tmp.close()?;
    Ok(())
}
#[tokio::test]
async fn accepts_go_autocert_cache_on_tls_listener() -> Result<()> {
    let dir = tempfile::tempdir()?;
    let key = KeyPair::generate()?;
    let p = CertificateParams::new(vec!["localhost".into()])?;
    let cert = p.self_signed(&key)?;
    std::fs::write(
        dir.path().join("localhost"),
        key.serialize_pem() + &cert.pem(),
    )?;
    let manager = Acme::new(dir.path().into(), &["localhost".into()], String::new())?;
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let stop = CancellationToken::new();
    let relay = Relay::new(
        Some(SessionConfig {
            registration_key: token(),
            ..Default::default()
        }),
        None,
    )?;
    let driver = tokio::spawn(serve_acme(
        listener,
        Arc::new(relay),
        Arc::new(manager),
        stop.clone(),
    ));
    let client = reqwest::Client::builder()
        .no_proxy()
        .add_root_certificate(reqwest::Certificate::from_der(cert.der())?)
        .resolve("localhost", addr)
        .timeout(Duration::from_secs(3))
        .build()?;
    let res = client
        .get(format!("https://localhost:{}/healthz", addr.port()))
        .send()
        .await?;
    ensure!(res.status() == 200);
    stop.cancel();
    driver.await??;
    ensure!(std::fs::read_dir(dir.path())?.count() == 1);
    dir.close()?;
    Ok(())
}
