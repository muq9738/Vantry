use anyhow::{Context, Result, bail, ensure};
use clap::{Args, ValueEnum};
use flate2::{Compression, write::GzEncoder};
use serde::Serialize;
use std::{
    collections::BTreeMap,
    fs::{self, File},
    io,
    path::{Path, PathBuf},
    process::Command,
};
use zip::{ZipWriter, write::SimpleFileOptions};

#[derive(Clone, Copy, Debug, Default, ValueEnum, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
enum Kind {
    #[default]
    Desktop,
    Service,
    Relay,
}
#[derive(Debug, Args)]
pub struct Options {
    /// Product to package (default desktop).
    #[arg(value_enum, conflicts_with = "kind")]
    product: Option<Kind>,
    #[arg(long, value_enum, alias = "package")]
    kind: Option<Kind>,
    /// Cargo target triple (defaults to the host).
    #[arg(long)]
    target: Option<String>,
    #[arg(long, default_value = "release")]
    profile: String,
    /// Use artifacts already present in CARGO_TARGET_DIR or explicit binary paths.
    #[arg(long)]
    skip_build: bool,
    #[arg(long)]
    desktop: Option<PathBuf>,
    #[arg(long)]
    service: Option<PathBuf>,
    #[arg(long)]
    relay: Option<PathBuf>,
    /// Additional platform executable (repeatable).
    #[arg(long)]
    helper: Vec<PathBuf>,
    /// Directory of prebuilt platform helpers.
    #[arg(long)]
    helper_dir: Option<PathBuf>,
    #[arg(long = "out-dir", visible_alias = "out")]
    out: Option<PathBuf>,
}
fn host() -> String {
    let arch = std::env::consts::ARCH;
    let suffix = if cfg!(target_os = "macos") {
        "apple-darwin"
    } else if cfg!(windows) {
        if cfg!(target_env = "gnu") {
            "pc-windows-gnu"
        } else {
            "pc-windows-msvc"
        }
    } else if cfg!(target_env = "musl") {
        "unknown-linux-musl"
    } else {
        "unknown-linux-gnu"
    };
    format!("{arch}-{suffix}")
}
fn normalize_target(raw: &str) -> String {
    match raw {
        "darwin-arm64" | "macos-arm64" | "darwin-aarch64" => "aarch64-apple-darwin".into(),
        "darwin-amd64" | "darwin-x86_64" | "macos-x86_64" => "x86_64-apple-darwin".into(),
        "linux-amd64" | "linux-x86_64" => "x86_64-unknown-linux-gnu".into(),
        "linux-arm64" | "linux-aarch64" => "aarch64-unknown-linux-gnu".into(),
        "windows-amd64" | "windows-x86_64" => "x86_64-pc-windows-msvc".into(),
        "windows-arm64" | "windows-aarch64" => "aarch64-pc-windows-msvc".into(),
        other => other.into(),
    }
}
struct Platform<'a> {
    os: &'a str,
    arch: &'a str,
    cargo_arch: &'a str,
}
impl<'a> Platform<'a> {
    fn parse(target: &'a str) -> Result<Self> {
        let arch = target.split('-').next().unwrap();
        ensure!(
            target
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || b"_-".contains(&c)),
            "invalid Cargo target"
        );
        let os = if target.ends_with("-apple-darwin") {
            "darwin"
        } else if target.contains("-windows-") {
            "windows"
        } else if target.contains("-linux-") {
            "linux"
        } else {
            bail!("unsupported target {target}")
        };
        Ok(Self {
            os,
            cargo_arch: arch,
            arch: match arch {
                "aarch64" => "arm64",
                "x86_64" => "amd64",
                "i686" => "386",
                a => a,
            },
        })
    }
}
#[derive(Serialize)]
struct Entry {
    path: PathBuf,
    component: &'static str,
    bytes: u64,
    source: String,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Report<'a> {
    target: &'a str,
    product: Kind,
    profile: &'a str,
    files: &'a [Entry],
    component_bytes: BTreeMap<&'static str, u64>,
    installed_bytes: u64,
    archive: String,
    archive_bytes: u64,
    zip_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    disk_image: Option<DiskImage>,
    budget_bytes: u64,
    within_budget: bool,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct DiskImage {
    name: String,
    bytes: u64,
    budget_bytes: u64,
}

#[cfg(target_os = "macos")]
fn disk_image(stage: &Path, app: &Path, name: String) -> Result<DiskImage> {
    const LIMIT: u64 = 50_000_000;
    let payload = tempfile::tempdir_in(stage)?;
    let staged_app = payload.path().join("Vantry.app");
    fs::rename(app, &staged_app)?;
    std::os::unix::fs::symlink("/Applications", payload.path().join("Applications"))?;
    let image = stage.join(&name);
    let status = Command::new("/usr/bin/hdiutil")
        .args([
            "create",
            "-quiet",
            "-fs",
            "HFS+",
            "-format",
            "ULFO",
            "-volname",
            "Vantry",
            "-srcfolder",
        ])
        .arg(payload.path())
        .arg(&image)
        .status()
        .context("create macOS disk image")?;
    ensure!(status.success(), "hdiutil create failed ({status})");
    let bytes = image.metadata()?.len();
    ensure!(
        bytes < LIMIT,
        "DMG is {bytes} bytes, limit is below {LIMIT} bytes"
    );
    fs::rename(staged_app, app)?;
    Ok(DiskImage {
        name,
        bytes,
        budget_bytes: LIMIT,
    })
}

struct Bundle {
    root: PathBuf,
    files: Vec<Entry>,
}
impl Bundle {
    fn copy(
        &mut self,
        src: &Path,
        rel: &Path,
        component: &'static str,
        source: &str,
    ) -> Result<()> {
        let meta = fs::symlink_metadata(src).with_context(|| format!("open {}", src.display()))?;
        if meta.is_dir() {
            let mut children = fs::read_dir(src)?.collect::<io::Result<Vec<_>>>()?;
            children.sort_by_key(|e| e.file_name());
            for child in children {
                let name = child.file_name();
                self.copy(
                    &child.path(),
                    &rel.join(&name),
                    component,
                    &format!("{source}/{}", name.to_string_lossy()),
                )?;
            }
            return Ok(());
        }
        ensure!(
            meta.is_file(),
            "expected a regular file at {}",
            src.display()
        );
        let out = self.root.join(rel);
        ensure!(
            !out.try_exists()?,
            "duplicate destination {}",
            rel.display()
        );
        fs::create_dir_all(crate::parent(&out))?;
        let bytes = fs::copy(src, out)?;
        self.files.push(Entry {
            path: rel.to_owned(),
            component,
            bytes,
            source: source.into(),
        });
        Ok(())
    }
    fn generated(&mut self, rel: &str, source: &str) -> Result<()> {
        self.files.push(Entry {
            path: rel.into(),
            component: "resources",
            bytes: self.root.join(rel).metadata()?.len(),
            source: source.into(),
        });
        Ok(())
    }
    fn archive(&self, stage: &Path, name: &str) -> Result<u64> {
        let path = stage.join(name);
        let dir = self.root.file_name().unwrap();
        let file = File::create(&path)?;
        if name.ends_with(".zip") {
            let mut zip = ZipWriter::new(file);
            for entry in &self.files {
                let src = self.root.join(&entry.path);
                let mut input = File::open(src)?;
                #[cfg(unix)]
                let mode = {
                    use std::os::unix::fs::PermissionsExt;
                    input.metadata()?.permissions().mode()
                };
                #[cfg(not(unix))]
                let mode = 0o644;
                let rel = Path::new(dir).join(&entry.path);
                zip.start_file(
                    rel.to_str()
                        .context("non-UTF-8 release path")?
                        .replace('\\', "/"),
                    SimpleFileOptions::default()
                        .unix_permissions(mode)
                        .compression_method(zip::CompressionMethod::Deflated),
                )?;
                io::copy(&mut input, &mut zip)?;
            }
            zip.finish()?.sync_all()?;
        } else {
            let mut tar = tar::Builder::new(GzEncoder::new(file, Compression::best()));
            for entry in &self.files {
                tar.append_path_with_name(
                    self.root.join(&entry.path),
                    Path::new(dir).join(&entry.path),
                )?;
            }
            tar.into_inner()?.finish()?.sync_all()?;
        }
        Ok(path.metadata()?.len())
    }
}
const PLIST: &str = r#"<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>io.github.muq9738.vantry</string>
<key>CFBundleExecutable</key><string>Vantry</string>
<key>CFBundleName</key><string>Vantry</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleIconFile</key><string>Vantry.icns</string>
<key>CFBundleShortVersionString</key><string>@VERSION@</string>
<key>CFBundleVersion</key><string>@VERSION@</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
<key>CFBundleDevelopmentRegion</key><string>zh-Hans</string>
<key>CFBundleLocalizations</key><array><string>zh-Hans</string><string>en</string><string>ja</string></array>
<key>NSLocalNetworkUsageDescription</key><string>Vantry 需要发现并直连局域网中的电脑，以访问你的项目和会话。</string>
<key>NSScreenCaptureUsageDescription</key><string>Vantry 仅在你开启本机桌面共享且经你确认授权时采集屏幕，并通过局域网直连或你配置的自建中继加密传输。</string>
<key>NSBonjourServices</key><array><string>_vantry._tcp</string></array>
</dict></plist>
"#;
fn verify_executable(src: &Path, p: &Platform) -> Result<()> {
    #[cfg(unix)]
    if p.os != "windows" {
        use std::os::unix::fs::PermissionsExt;
        ensure!(
            src.metadata()?.permissions().mode() & 0o111 != 0,
            "missing executable permission on {}",
            src.display()
        );
    }
    if cfg!(target_os = "macos") && p.os == "darwin" {
        let arch = if p.cargo_arch == "aarch64" {
            "arm64"
        } else {
            p.cargo_arch
        };
        ensure!(
            Command::new("/usr/bin/lipo")
                .arg(src)
                .args(["-verify_arch", arch])
                .status()?
                .success(),
            "{} lacks {arch} slice",
            src.display()
        );
    }
    Ok(())
}
pub fn run(o: Options) -> Result<()> {
    let kind = o.product.or(o.kind).unwrap_or_default();
    let root = Path::new(env!("CARGO_MANIFEST_DIR"))
        .ancestors()
        .nth(2)
        .unwrap();
    let target = o
        .target
        .as_deref()
        .map(normalize_target)
        .unwrap_or_else(host);
    let p = Platform::parse(&target)?;
    ensure!(
        !o.profile.is_empty()
            && o.profile
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b)),
        "invalid Cargo profile"
    );
    let target_dir = std::env::var_os("CARGO_TARGET_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| root.join("target"));
    let target_dir = std::path::absolute(target_dir)?;
    let platform = format!("{}-{}", p.os, p.arch);
    let mac = kind == Kind::Desktop && p.os == "darwin";
    let exe = if p.os == "windows" { ".exe" } else { "" };
    let slug = match kind {
        Kind::Desktop => "vantry",
        Kind::Service => "vantry-service",
        Kind::Relay => "vantry-relay",
    };
    let out = o.out.clone().unwrap_or_else(|| {
        target_dir.join(if mac {
            format!("package-macos-{}", p.arch)
        } else {
            format!("package-{slug}-{platform}")
        })
    });
    let parent = crate::parent(&out);
    fs::create_dir_all(parent)?;
    let stage = tempfile::Builder::new()
        .prefix(".vantry-package-")
        .tempdir_in(parent)?;
    let mut bundle = Bundle {
        root: stage.path().join(if mac {
            "Vantry.app"
        } else if kind == Kind::Desktop {
            "Vantry"
        } else {
            slug
        }),
        files: vec![],
    };
    let profile_dir = match o.profile.as_str() {
        "dev" | "test" => "debug",
        "bench" => "release",
        name => name,
    };
    let mut binaries = Vec::new();
    if kind == Kind::Desktop {
        binaries.push(("vantry-desktop", "vantry", "desktop", o.desktop.as_ref()));
    }
    if kind != Kind::Relay {
        binaries.push((
            "vantry-service",
            "vantry-service",
            "service",
            o.service.as_ref(),
        ));
    } else {
        binaries.push(("vantry-relay", "vantry-relay", "relay", o.relay.as_ref()));
    }
    if !o.skip_build && binaries.iter().any(|(_, _, _, src)| src.is_none()) {
        let mut cmd = Command::new(std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into()));
        cmd.current_dir(root)
            .env("CARGO_TARGET_DIR", &target_dir)
            .args([
                "build",
                "--locked",
                "--profile",
                &o.profile,
                "--target",
                &target,
            ]);
        for (pkg, _, _, src) in &binaries {
            if src.is_none() {
                cmd.args(["-p", pkg]);
            }
        }
        ensure!(cmd.status()?.success(), "Cargo build failed");
    }
    for (pkg, bin, component, explicit) in binaries {
        let name = format!("{bin}{exe}");
        let mut src = target_dir.join(&target).join(profile_dir).join(&name);
        if !src.is_file() && target == host() {
            src = target_dir.join(profile_dir).join(&name);
        }
        let src = explicit.unwrap_or(&src);
        let dest = if mac {
            Path::new("Contents/MacOS").join(if component == "desktop" {
                "Vantry"
            } else {
                bin
            })
        } else if component == "service" && kind == Kind::Desktop {
            Path::new("libexec").join(&name)
        } else if component == "desktop" && p.os == "windows" {
            "Vantry.exe".into()
        } else {
            name.into()
        };
        verify_executable(src, &p)?;
        let origin = if explicit.is_some() {
            format!("supplied {pkg}/{bin}: {}", src.display())
        } else {
            format!(
                "cargo {pkg}/{bin} ({target}, {}): {}",
                o.profile,
                src.display()
            )
        };
        bundle.copy(src, &dest, component, &origin)?;
    }
    let resources = if mac {
        Path::new("Contents/Resources")
    } else {
        Path::new("")
    };
    for asset in ["LICENSE", "THIRD_PARTY_NOTICES.md", "third_party/licenses"] {
        bundle.copy(&root.join(asset), &resources.join(asset), "licenses", asset)?;
    }
    if kind == Kind::Relay && p.os == "linux" {
        let unit = "deploy/relay/vantry-relay.service";
        bundle.copy(
            &root.join(unit),
            Path::new("vantry-relay.service"),
            "resources",
            unit,
        )?;
    }
    if kind == Kind::Desktop {
        let icon = "resources/icon.png";
        bundle.copy(
            &root.join(icon),
            &resources.join("icon.png"),
            "resources",
            icon,
        )?;
        if root.join("resources/update-token").is_file() {
            bundle.copy(
                &root.join("resources/update-token"),
                &resources.join("update-token"),
                "resources",
                "resources/update-token",
            )?;
        }
    }
    if kind == Kind::Desktop || !o.helper.is_empty() || o.helper_dir.is_some() {
        let mut helpers = o.helper;
        if let Some(dir) = o.helper_dir {
            for e in fs::read_dir(dir)? {
                helpers.push(e?.path());
            }
        }
        let native = format!("vantry-remote-helper{exe}");
        let prebuilt = root
            .join("resources")
            .join(&platform)
            .join("remote-desktop")
            .join(&native);
        if kind == Kind::Desktop
            && prebuilt.is_file()
            && !helpers
                .iter()
                .any(|h| h.file_name() == prebuilt.file_name())
        {
            helpers.push(prebuilt);
        }
        for path in helpers {
            verify_executable(&path, &p)?;
            let name = path.file_name().context("missing helper name")?;
            let dir = if name == native.as_str() {
                resources.join("remote-desktop")
            } else if mac {
                "Contents/Helpers".into()
            } else {
                "helpers".into()
            };
            bundle.copy(
                &path,
                &dir.join(name),
                "helpers",
                &format!("platform helper ({platform}): {}", path.display()),
            )?;
        }
    }
    if mac {
        fs::write(
            bundle.root.join("Contents/Info.plist"),
            PLIST.replace("@VERSION@", env!("CARGO_PKG_VERSION")),
        )?;
        bundle.generated("Contents/Info.plist", "xtask bundle metadata")?;
        let rel = "Contents/Resources/Vantry.icns";
        crate::icon::icns(
            &image::open(root.join("resources/icon.png"))?.to_rgba8(),
            &bundle.root.join(rel),
        )?;
        bundle.generated(rel, "xtask from resources/icon.png")?;
        // The existing macOS release profile uses an ad-hoc identity. Sign the
        // copied bundle before archiving so the offline codesign check succeeds.
        ensure!(
            cfg!(target_os = "macos"),
            "assembling a signed .app requires macOS"
        );
        let status = Command::new("/usr/bin/codesign")
            .args(["--force", "--sign", "-"])
            .arg(&bundle.root)
            .status()?;
        ensure!(status.success(), "could not sign macOS bundle");
        bundle.generated(
            "Contents/_CodeSignature/CodeResources",
            "macOS codesign bundle resources",
        )?;
        for entry in &mut bundle.files {
            entry.bytes = bundle.root.join(&entry.path).metadata()?.len();
        }
    }
    bundle.files.sort_by(|a, b| a.path.cmp(&b.path));
    let ext = if p.os == "linux" { "tar.gz" } else { "zip" };
    let archive = format!("{slug}-{}-{platform}.{ext}", env!("CARGO_PKG_VERSION"));
    let bytes = bundle.archive(stage.path(), &archive)?;
    if mac && p.arch == "arm64" {
        fs::hard_link(
            stage.path().join(&archive),
            stage.path().join("Vantry-macos-arm64.zip"),
        )?;
    }
    #[cfg(target_os = "macos")]
    let disk_image = if mac && o.profile == "release" {
        let name = format!("Vantry-{}-macos-{}.dmg", env!("CARGO_PKG_VERSION"), p.arch);
        Some(disk_image(stage.path(), &bundle.root, name)?)
    } else {
        None
    };
    #[cfg(not(target_os = "macos"))]
    let disk_image = None;
    let mut components = BTreeMap::from([
        ("desktop", 0),
        ("service", 0),
        ("relay", 0),
        ("helpers", 0),
        ("resources", 0),
        ("licenses", 0),
    ]);
    for e in &bundle.files {
        *components.get_mut(e.component).unwrap() += e.bytes;
    }
    let report = Report {
        target: &target,
        profile: &o.profile,
        product: kind,
        files: &bundle.files,
        installed_bytes: components.values().sum(),
        component_bytes: components,
        archive,
        archive_bytes: bytes,
        zip_bytes: (ext == "zip").then_some(bytes),
        disk_image,
        budget_bytes: 25_000_000,
        within_budget: bytes <= 25_000_000,
    };
    let data = serde_json::to_vec_pretty(&report)?;
    for name in ["package-report.json", "size-report.json"] {
        fs::write(stage.path().join(name), &data)?;
    }
    fs::write(
        stage.path().join("provenance.json"),
        serde_json::to_vec_pretty(
            &serde_json::json!({"target":target,"product":kind,"profile":o.profile,"files":bundle.files}),
        )?,
    )?;
    fs::rename(stage.path(), &out)
        .with_context(|| format!("publish to {} (choose an empty destination)", out.display()))?;
    if let Some(image) = &report.disk_image {
        println!(
            "{}  {} bytes (limit < {} bytes)",
            image.name, image.bytes, image.budget_bytes
        );
    }
    println!("{:<16} {:>14}", "Component", "Bytes");
    for (name, bytes) in report.component_bytes {
        println!("{name:<16} {bytes:>14}");
    }
    println!(
        "{:<16} {:>14}\n{:<16} {:>14}\n{:<16} {:>14} ({})\n{}",
        "Installed",
        report.installed_bytes,
        ext,
        bytes,
        "Budget",
        report.budget_bytes,
        if report.within_budget {
            "within budget"
        } else {
            "over budget"
        },
        out.display()
    );
    Ok(())
}
