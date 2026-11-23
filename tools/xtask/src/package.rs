use anyhow::{Context, Result, ensure};
use clap::Args;
use serde::Serialize;
use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    process::Command,
    time::{SystemTime, UNIX_EPOCH},
};

#[derive(Debug, Args)]
pub struct Options {
    #[arg(long, default_value = "aarch64-apple-darwin")]
    target: String,
    /// Defaults to the vantry desktop binary in the release target directory.
    #[arg(long)]
    desktop: Option<PathBuf>,
    /// Defaults to vantry-service in the release target directory.
    #[arg(long)]
    service: Option<PathBuf>,
    /// Additional arm64 executable to place in Contents/Helpers (repeatable).
    #[arg(long)]
    helper: Vec<PathBuf>,
    /// Destination for Vantry.app, its ZIP, and the JSON size report.
    #[arg(long)]
    out: Option<PathBuf>,
}
#[derive(Serialize)]
struct Entry {
    path: PathBuf,
    component: &'static str,
    bytes: u64,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Report {
    target: String,
    files: Vec<Entry>,
    component_bytes: BTreeMap<&'static str, u64>,
    installed_bytes: u64,
    zip_bytes: u64,
}
const PLIST: &str = r#"<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>io.github.muq9738.vantry</string>
<key>CFBundleExecutable</key><string>Vantry</string>
<key>CFBundleName</key><string>Vantry</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>@VERSION@</string>
<key>CFBundleVersion</key><string>@VERSION@</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
</dict></plist>
"#;
fn copy(
    src: &Path,
    dest: &Path,
    label: &'static str,
    app: &Path,
    list: &mut Vec<Entry>,
) -> Result<()> {
    let meta = fs::metadata(src).with_context(|| format!("open {}", src.display()))?;
    if meta.is_dir() {
        for entry in fs::read_dir(src)? {
            let entry = entry?;
            copy(
                &entry.path(),
                &dest.join(entry.file_name()),
                label,
                app,
                list,
            )?;
        }
        return Ok(());
    }
    ensure!(meta.is_file(), "expected file: {}", src.display());
    if matches!(label, "desktop" | "service" | "helpers") {
        let status = Command::new("/usr/bin/lipo")
            .arg(src)
            .args(["-verify_arch", "arm64"])
            .status()?;
        ensure!(status.success(), "{} lacks arm64 slice", src.display());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            ensure!(
                meta.permissions().mode() & 0o111 != 0,
                "{} lacks executable permission",
                src.display()
            );
        }
    }
    let file = app.join(dest);
    ensure!(
        !file.try_exists()?,
        "duplicate destination {}",
        dest.display()
    );
    fs::create_dir_all(file.parent().unwrap())?;
    let bytes = fs::copy(src, file)?;
    list.push(Entry {
        path: dest.to_owned(),
        component: label,
        bytes,
    });
    Ok(())
}
fn assemble(stage: &Path, root: &Path, target_dir: &Path, options: Options) -> Result<Report> {
    let app = stage.join("Vantry.app");
    let mut files = vec![];
    let release = |name: &str| {
        let explicit = target_dir.join(&options.target).join("release").join(name);
        if explicit.is_file() {
            explicit
        } else {
            target_dir.join("release").join(name)
        }
    };
    copy(
        &options.desktop.unwrap_or_else(|| release("vantry")),
        Path::new("Contents/MacOS/Vantry"),
        "desktop",
        &app,
        &mut files,
    )?;
    copy(
        &options.service.unwrap_or_else(|| release("vantry-service")),
        Path::new("Contents/MacOS/vantry-service"),
        "service",
        &app,
        &mut files,
    )?;
    for path in options.helper {
        copy(
            &path,
            &Path::new("Contents/Helpers").join(path.file_name().context("helper filename")?),
            "helpers",
            &app,
            &mut files,
        )?;
    }
    let resources = Path::new("Contents/Resources");
    for path in ["LICENSE", "THIRD_PARTY_NOTICES.md", "third_party/licenses"] {
        copy(
            &root.join(path),
            &resources.join(path),
            "licenses",
            &app,
            &mut files,
        )?;
    }
    copy(
        &root.join("resources/icon.png"),
        &resources.join("icon.png"),
        "resources",
        &app,
        &mut files,
    )?;
    let plist = PLIST.replace("@VERSION@", env!("CARGO_PKG_VERSION"));
    fs::write(app.join("Contents/Info.plist"), &plist)?;
    files.push(Entry {
        path: "Contents/Info.plist".into(),
        component: "resources",
        bytes: plist.len() as u64,
    });
    files.sort_by(|a, b| a.path.cmp(&b.path));
    let archive = stage.join("Vantry-macos-arm64.zip");
    let status = Command::new("/usr/bin/ditto")
        .args(["-c", "-k", "--norsrc", "--keepParent"])
        .arg(&app)
        .arg(&archive)
        .status()?;
    ensure!(status.success(), "ditto failed: {status}");
    let mut component_bytes = BTreeMap::new();
    for entry in &files {
        *component_bytes.entry(entry.component).or_default() += entry.bytes;
    }
    let report = Report {
        target: options.target,
        installed_bytes: component_bytes.values().sum(),
        zip_bytes: archive.metadata()?.len(),
        files,
        component_bytes,
    };
    fs::write(
        stage.join("package-report.json"),
        serde_json::to_vec_pretty(&report)?,
    )?;
    Ok(report)
}
pub fn run(options: Options) -> Result<()> {
    ensure!(
        cfg!(target_os = "macos") && options.target == "aarch64-apple-darwin",
        "package requires macOS and target aarch64-apple-darwin"
    );
    let root = Path::new(env!("CARGO_MANIFEST_DIR"))
        .ancestors()
        .nth(2)
        .unwrap();
    let target = std::env::var_os("CARGO_TARGET_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| root.join("target"));
    let out = options
        .out
        .clone()
        .unwrap_or_else(|| target.join("package-macos-arm64"));
    let parent = out
        .parent()
        .filter(|p| !p.as_os_str().is_empty())
        .unwrap_or(Path::new("."));
    fs::create_dir_all(parent)?;
    let stage = parent.join(format!(
        ".vantry-package-{}-{}",
        std::process::id(),
        SystemTime::now().duration_since(UNIX_EPOCH)?.as_nanos()
    ));
    fs::create_dir(&stage)?;
    let result = assemble(&stage, root, &target, options).and_then(|report| {
        fs::rename(&stage, &out).with_context(|| {
            format!("publish to {} (choose an empty destination)", out.display())
        })?;
        Ok(report)
    });
    if result.is_err() {
        fs::remove_dir_all(&stage)?;
    }
    let report = result?;
    println!("{:<16} {:>14}", "Component", "Bytes");
    for (name, bytes) in report.component_bytes {
        println!("{name:<16} {bytes:>14}");
    }
    println!(
        "{:<16} {:>14}\n{:<16} {:>14}\n{}",
        "Installed",
        report.installed_bytes,
        "ZIP",
        report.zip_bytes,
        out.display()
    );
    Ok(())
}

#[test]
#[cfg(target_os = "macos")]
fn package_includes_arm64_executables_licenses_icon_and_size_report() -> Result<()> {
    let path = std::env::temp_dir().join(format!(
        "vantry-xtask-test-{}-{}",
        std::process::id(),
        SystemTime::now().duration_since(UNIX_EPOCH)?.as_nanos()
    ));
    let out = path.join("package");
    let fixture = std::env::current_exe()?;
    let helper_name = fixture.file_name().unwrap().to_owned();
    let result = (|| -> Result<()> {
        run(Options {
            target: "aarch64-apple-darwin".into(),
            desktop: Some(fixture.clone()),
            service: Some(fixture.clone()),
            helper: vec![fixture],
            out: Some(out.clone()),
        })?;
        let doc: serde_json::Value =
            serde_json::from_reader(fs::File::open(out.join("package-report.json"))?)?;
        let extracted = path.join("unpacked");
        ensure!(
            Command::new("/usr/bin/ditto")
                .args(["-x", "-k"])
                .arg(out.join("Vantry-macos-arm64.zip"))
                .arg(&extracted)
                .status()?
                .success()
        );
        let app = extracted.join("Vantry.app");
        let mut total = 0;
        for file in doc["files"].as_array().unwrap() {
            let bytes = app.join(file["path"].as_str().unwrap()).metadata()?.len();
            assert_eq!(file["bytes"], bytes);
            total += bytes;
        }
        assert_eq!(doc["installedBytes"], total);
        assert_eq!(
            doc["zipBytes"],
            out.join("Vantry-macos-arm64.zip").metadata()?.len()
        );
        assert!(app.join("Contents/Helpers").join(helper_name).is_file());
        for p in [
            "MacOS/Vantry",
            "MacOS/vantry-service",
            "Resources/icon.png",
            "Resources/LICENSE",
            "Resources/THIRD_PARTY_NOTICES.md",
            "Resources/third_party/licenses",
            "Info.plist",
        ] {
            assert!(app.join("Contents").join(p).exists(), "{p}");
        }
        Ok(())
    })();
    fs::remove_dir_all(path)?;
    result
}
