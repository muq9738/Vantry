#[test]
fn embedded_worker_round_trips_real_tools_and_controls() {
    let python = if cfg!(windows) { "python" } else { "python3" };
    let result = std::process::Command::new(python)
        .arg(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/worker_smoke.py"
        ))
        .arg(env!("CARGO_BIN_EXE_vantry-service"))
        .status()
        .expect("start local model fixture");
    assert!(result.success());
}
