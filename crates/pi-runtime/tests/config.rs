use serde_json::json;
use std::{fs, path::Path, sync::Arc, thread};
use vantry_pi_runtime::{read_config, save_setup};
use vantry_protocol::PiSetupRequest;

#[test]
fn lazy_read_and_concurrent_config_merges() -> anyhow::Result<()> {
    let root = std::env::temp_dir().join(format!("vantry-config-{}", uuid::Uuid::new_v4()));
    let result = exercise(&root);
    if root.exists() {
        fs::remove_dir_all(root)?;
    }
    result
}
fn exercise(dir: &Path) -> anyhow::Result<()> {
    let config = read_config(dir)?;
    assert!(config.auth.is_empty() && config.models.is_empty() && config.settings.is_empty());
    assert!(!dir.exists());
    let barrier = Arc::new(std::sync::Barrier::new(2));
    thread::scope(|scope| {
        let mut workers = Vec::new();
        for id in ["alpha", "beta"] {
            let barrier = Arc::clone(&barrier);
            workers.push(scope.spawn(move || {
                barrier.wait();
                save_setup(
                    dir,
                    &PiSetupRequest {
                        provider_id: id.into(),
                        api_key: "fixture".into(),
                        ..Default::default()
                    },
                )
            }));
        }
        for worker in workers {
            worker.join().unwrap()?;
        }
        anyhow::Ok(())
    })?;
    let config = read_config(dir)?;
    assert!(config.auth.contains_key("alpha") && config.auth.contains_key("beta"));
    assert!(matches!(
        config.settings["defaultProvider"].as_str(),
        Some("alpha" | "beta")
    ));
    assert!(!dir.join("models.json").exists());
    let settings = config.settings;
    fs::write(dir.join("models.json"), "{")?;
    let setup = PiSetupRequest {
        provider_id: "custom".into(),
        api_key: "fixture".into(),
        models: vec!["tiny".into()],
        set_default: true,
        ..Default::default()
    };
    assert!(save_setup(dir, &setup).is_err());
    assert_eq!(fs::read_to_string(dir.join("models.json"))?, "{");
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&fs::read(dir.join("settings.json"))?)?,
        json!(settings)
    );
    fs::write(dir.join("models.json"), "{}")?;
    save_setup(dir, &setup)?;
    assert_eq!(read_config(dir)?.settings["defaultModel"], "tiny");
    Ok(())
}
