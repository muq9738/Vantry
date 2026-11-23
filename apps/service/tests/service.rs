#[test]
fn service_socket_cli_and_worker_recovery() {
    let status = std::process::Command::new("python3")
        .args([
            "-B",
            concat!(env!("CARGO_MANIFEST_DIR"), "/tests/service.py"),
            env!("CARGO_BIN_EXE_vantry-service"),
        ])
        .status()
        .expect("start isolated service fixture");
    assert!(status.success());
}
