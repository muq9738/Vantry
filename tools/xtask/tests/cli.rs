use anyhow::{Context, Result, ensure};
use std::{
    fs::{self, File},
    io::{self, Read},
    path::Path,
    process::Command,
};
use tempfile::tempdir;
fn cli(args: &[&str]) -> Result<String> {
    let res = Command::new(env!("CARGO_BIN_EXE_xtask"))
        .args(args)
        .output()?;
    ensure!(
        res.status.success(),
        "xtask: {}",
        String::from_utf8_lossy(&res.stderr)
    );
    Ok(String::from_utf8(res.stdout)?)
}
fn s(p: &Path) -> &str {
    p.to_str().unwrap()
}
#[test]
fn prepare_icon_preserves_enclosed_artwork() -> Result<()> {
    let tmp = tempdir()?;
    let source = tmp.path().join("in.png");
    let out = tmp.path().join("assets/app.png");
    let img = image::RgbaImage::from_fn(16, 16, |x, y| {
        if x == 0 || x == 15 || y == 0 || y == 15 {
            image::Rgba([32, 25, 8, 255])
        } else if (6..10).contains(&x) && (6..10).contains(&y) {
            image::Rgba([0, 0, 0, 255])
        } else {
            image::Rgba([255; 4])
        }
    });
    img.save(&source)?;
    let set = tmp.path().join("Vantry.iconset");
    let icns = tmp.path().join("Vantry.icns");
    let info = cli(&[
        "prepare-icon",
        "-source",
        s(&source),
        "--out",
        s(&out),
        "--iconset",
        s(&set),
        "--icns",
        s(&icns),
    ])?;
    assert_eq!(
        image::image_dimensions(set.join("icon_256x256@2x.png"))?,
        (512, 512)
    );
    let container = fs::read(icns)?;
    assert_eq!(&container[..4], b"icns");
    assert_eq!(
        u32::from_be_bytes(container[4..8].try_into()?) as usize,
        container.len()
    );
    assert!(info.contains("60 edge"));
    let img = image::open(&out)?.to_rgba8();
    assert_eq!(img.dimensions(), (1024, 1024));
    assert_eq!(img.get_pixel(0, 512).0[3], 0);
    assert_eq!(img.get_pixel(512, 512).0, [0, 0, 0, 255]);
    assert_eq!(img.get_pixel(200, 200).0, [255; 4]);
    image::RgbaImage::new(3, 4).save(&source)?;
    assert!(cli(&["prepare-icon", "--source", s(&source), "--out", s(&out)]).is_err());
    Ok(())
}
#[test]
fn make_delta_is_atomic_and_applies() -> Result<()> {
    let tmp = tempdir()?;
    let old = tmp.path().join("old");
    let new = tmp.path().join("new");
    let patch = tmp.path().join("release.delta");
    let out = tmp.path().join("out");
    fs::create_dir(&old)?;
    fs::create_dir(&new)?;
    fs::write(new.join("release"), b"current")?;
    let args = [
        "make-delta",
        "-from",
        "1",
        "-to",
        "2",
        "-old",
        s(&old),
        "-new",
        s(&new),
        "-out",
        s(&patch),
    ];
    assert!(cli(&args)?.contains("7 bytes uncompressed"));
    let good = fs::read(&patch)?;
    vantry_updater::apply_delta(File::open(&patch)?, "1", "2", &old, &out)?;
    assert_eq!(fs::read(out.join("release"))?, b"current");
    fs::remove_dir_all(&new)?;
    assert!(cli(&args).is_err());
    assert_eq!(fs::read(patch)?, good);
    Ok(())
}
#[test]
fn make_deltas_builds_cross_version_patches_in_parallel() -> Result<()> {
    let tmp = tempdir()?;
    let v1 = tmp.path().join("v1");
    let v2 = tmp.path().join("v2");
    let v3 = tmp.path().join("v3");
    let out_dir = tmp.path().join("deltas");
    for (dir, body) in [(&v1, b"alpha-001"), (&v2, b"alpha-002"), (&v3, b"alpha-003")] {
        fs::create_dir(dir)?;
        fs::write(dir.join("bin"), body)?;
    }
    let output = cli(&[
        "make-deltas",
        "--to",
        "0.2.23",
        "--platform",
        "darwin-arm64",
        "--new",
        s(&v3),
        "--out-dir",
        s(&out_dir),
        "--base",
        &format!("0.2.21={}", s(&v1)),
        "--base",
        &format!("0.2.22={}", s(&v2)),
    ])?;
    assert!(output.contains("vantry-0.2.21-to-0.2.23-darwin-arm64.delta"));
    assert!(output.contains("vantry-0.2.22-to-0.2.23-darwin-arm64.delta"));
    for (from, base) in [("0.2.21", &v1), ("0.2.22", &v2)] {
        let p = out_dir.join(format!("vantry-{from}-to-0.2.23-darwin-arm64.delta"));
        let rebuilt = tmp.path().join(format!("rebuilt-{from}"));
        vantry_updater::apply_delta(File::open(&p)?, from, "0.2.23", base, &rebuilt)?;
        assert_eq!(fs::read(rebuilt.join("bin"))?, b"alpha-003");
    }
    Ok(())
}
fn package_case(root: &Path, kind: &str, target: &str, helper: &Path, bin: &Path) -> Result<()> {
    let out = root.join(format!("{kind}-{target}"));
    let mut args = vec![
        "package",
        kind,
        "--skip-build",
        "--profile",
        "dev",
        "--target",
        target,
        "--out",
        s(&out),
    ];
    match kind {
        "desktop" => args.extend([
            "--desktop",
            s(bin),
            "--service",
            s(bin),
            "--helper",
            s(helper),
        ]),
        "service" => args.extend(["--service", s(bin)]),
        "relay" => args.extend(["--relay", s(bin)]),
        _ => unreachable!(),
    };
    let log = cli(&args)?;
    assert!(log.contains("25000000"));
    let json = |name| -> Result<serde_json::Value> {
        Ok(serde_json::from_reader(File::open(out.join(name))?)?)
    };
    let report = json("size-report.json")?;
    let prov = json("provenance.json")?;
    assert_eq!(prov["files"], report["files"]);
    assert_eq!(report["product"], kind);
    assert_eq!(report["target"], target);
    let archive = out.join(report["archive"].as_str().unwrap());
    assert_eq!(archive.metadata()?.len(), report["archiveBytes"]);
    let bundle = if target.contains("darwin") {
        "Vantry.app".into()
    } else if kind == "desktop" {
        "Vantry".into()
    } else {
        format!("vantry-{kind}")
    };
    let mut expected = std::collections::BTreeMap::new();
    for e in prov["files"].as_array().context("files")? {
        let rel = e["path"].as_str().unwrap().replace('\\', "/");
        let size = out.join(&bundle).join(&rel).metadata()?.len();
        assert_eq!(e["bytes"], size);
        assert!(!e["source"].as_str().unwrap().is_empty());
        expected.insert(format!("{bundle}/{rel}"), size);
    }
    assert_eq!(expected.values().sum::<u64>(), report["installedBytes"]);
    assert!(
        expected
            .keys()
            .any(|k| k.ends_with("/THIRD_PARTY_NOTICES.md"))
    );
    let mut got = std::collections::BTreeMap::new();
    if target.contains("linux") {
        let mut tar = tar::Archive::new(flate2::read::GzDecoder::new(File::open(&archive)?));
        for e in tar.entries()? {
            let mut e = e?;
            let path = e.path()?.to_str().unwrap().to_owned();
            got.insert(path, io::copy(&mut e, &mut io::sink())?);
        }
        if kind == "relay" {
            assert!(expected.contains_key("vantry-relay/vantry-relay.service"));
        }
    } else {
        let mut zip = zip::ZipArchive::new(File::open(&archive)?)?;
        for i in 0..zip.len() {
            let mut e = zip.by_index(i)?;
            got.insert(e.name().to_owned(), io::copy(&mut e, &mut io::sink())?);
        }
        assert_eq!(report["zipBytes"], archive.metadata()?.len());
    }
    assert_eq!(got, expected);
    if kind == "desktop" {
        if target.contains("darwin") {
            let installed = root.join("Installed.app");
            let plan = vantry_updater::InstallPlan::prepare(&archive, &installed, "0", "1")?;
            let command = plan.installer(&root.join("user's data"))?;
            assert!(fs::read_to_string(command)?.contains("'\\''"));
            plan.install()?;
            assert!(installed.join("Contents/MacOS/Vantry").is_file());
        }
        let (head, svc) = if target.contains("darwin") {
            (
                "Vantry.app/Contents/MacOS/Vantry",
                "Vantry.app/Contents/MacOS/vantry-service",
            )
        } else if target.contains("linux") {
            ("Vantry/vantry", "Vantry/libexec/vantry-service")
        } else {
            ("Vantry/Vantry.exe", "Vantry/libexec/vantry-service.exe")
        };
        assert!(expected.contains_key(head) && expected.contains_key(svc));
        assert!(report["componentBytes"]["helpers"].as_u64().unwrap() > 0);
    } else {
        assert_eq!(report["componentBytes"]["desktop"], 0);
        let plan = vantry_updater::InstallPlan::prepare(
            &archive,
            &root.join(format!("installed-{kind}")),
            "0",
            "1",
        )?;
        plan.install()?;
        let mut buf = String::new();
        File::open(plan.target().join("LICENSE"))?.read_to_string(&mut buf)?;
        assert!(buf.contains("MIT"));
    }
    Ok(())
}
#[test]
fn package_resolves_cargo_target_profile_and_helper_directory() -> Result<()> {
    let tmp = tempdir()?;
    let triple = "x86_64-pc-windows-msvc";
    let bin = tmp.path().join(triple).join("debug");
    fs::create_dir_all(&bin)?;
    for name in ["vantry.exe", "vantry-service.exe"] {
        fs::write(bin.join(name), name)?;
    }
    let helpers = tmp.path().join("native");
    fs::create_dir(&helpers)?;
    fs::write(helpers.join("vantry-remote-helper.exe"), b"helper")?;
    let out = tmp.path().join("release");
    let result = Command::new(env!("CARGO_BIN_EXE_xtask"))
        .env("CARGO_TARGET_DIR", tmp.path())
        .args([
            "package",
            "--skip-build",
            "--profile",
            "dev",
            "--target",
            triple,
            "--helper-dir",
            s(&helpers),
            "--out-dir",
            s(&out),
        ])
        .output()?;
    ensure!(
        result.status.success(),
        "{}",
        String::from_utf8_lossy(&result.stderr)
    );
    let report: serde_json::Value =
        serde_json::from_reader(File::open(out.join("provenance.json"))?)?;
    assert!(report["files"].as_array().unwrap().iter().any(|f| {
        f["component"] == "desktop"
            && f["source"]
                .as_str()
                .unwrap()
                .starts_with("cargo vantry-desktop/vantry")
    }));
    assert_eq!(
        fs::read(out.join("Vantry/remote-desktop/vantry-remote-helper.exe"))?,
        b"helper"
    );
    Ok(())
}
#[test]
fn offline_packages_contain_reported_files() -> Result<()> {
    let tmp = tempdir()?;
    let bin = tmp.path().join("binary");
    let helper = tmp.path().join("platform-helper");
    for path in [&bin, &helper] {
        fs::write(path, b"executable fixture")?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(path, fs::Permissions::from_mode(0o755))?;
        }
    }
    for (kind, target) in [
        ("desktop", "x86_64-unknown-linux-gnu"),
        ("desktop", "x86_64-pc-windows-msvc"),
        ("service", "x86_64-unknown-linux-gnu"),
        ("relay", "x86_64-unknown-linux-gnu"),
    ] {
        package_case(tmp.path(), kind, target, &helper, &bin)?;
    }
    // Shorthand platform aliases used by CI/scripts normalize to Cargo triples.
    for (alias, expected_triple) in [
        ("linux-x86_64", "x86_64-unknown-linux-gnu"),
        ("windows-x86_64", "x86_64-pc-windows-msvc"),
    ] {
        let out = tmp.path().join(format!("alias-{alias}"));
        cli(&[
            "package",
            "service",
            "--skip-build",
            "--profile",
            "dev",
            "--target",
            alias,
            "--service",
            s(&bin),
            "--out",
            s(&out),
        ])?;
        let report: serde_json::Value =
            serde_json::from_reader(File::open(out.join("size-report.json"))?)?;
        assert_eq!(report["target"], expected_triple);
    }
    #[cfg(all(target_os = "macos", target_arch = "aarch64"))]
    {
        let native = Path::new(env!("CARGO_BIN_EXE_xtask"));
        fs::copy(native, &helper)?;
        package_case(
            tmp.path(),
            "desktop",
            "aarch64-apple-darwin",
            &helper,
            native,
        )?;
    }
    Ok(())
}
