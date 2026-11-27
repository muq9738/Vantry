use anyhow::Result;
use std::{
    cmp::Ordering::*,
    fs::{self, File},
    io::Cursor,
    path::Path,
};
use tempfile::tempdir;
use vantry_updater::*;

#[test]
fn versions_channels_and_platform_assets() -> Result<()> {
    for (a, b, want) in [
        ("v0.1.4", "0.1.3", Greater),
        ("V1.0+meta", "1", Equal),
        ("1.0-beta.10", "1-beta.2", Greater),
        ("1-rc.1", "1", Less),
        ("1-0", "1-a", Less),
        ("1-a", "1-a.1", Less),
        ("dev", "0.0.1", Less),
    ] {
        assert_eq!(compare_versions(a, b), want);
        assert_eq!(compare_versions(b, a), want.reverse());
    }
    let github=br#"{"tag_name":"v2","assets":[{"name":"vantry-2-arm64.dmg"},{"name":"vantry-2-darwin-arm64.zip","size":20},{"name":"vantry-1-to-2-darwin-arm64.delta","size":3}]}"#;
    let mut release = parse_release(github, Channel::Stable)?;
    let (full, delta) = release.assets_for("vantry", "darwin-arm64", "v1");
    assert_eq!(full.unwrap().size, 20);
    assert_eq!(delta.unwrap().size, 3);
    release.assets.iter_mut().find(|a| a.name.ends_with(".delta")).unwrap().size = 21;
    assert!(release.assets_for("vantry", "darwin-arm64", "v1").1.is_none());
    assert!(release.assets_for("vantry", "linux-amd64", "1").0.is_none());
    let manifest=br#"{"releases":[{"version":"2.1","channel":"stable"},{"version":"3-beta.2","channel":"beta"},{"version":"4-nightly","channel":"nightly"},{"version":"100","draft":true}]}"#;
    for (channel, ver) in [
        (Channel::Stable, "2.1"),
        (Channel::Beta, "3-beta.2"),
        (Channel::Nightly, "4-nightly"),
    ] {
        assert_eq!(parse_release(manifest, channel)?.version(), ver);
    }
    Ok(())
}
#[test]
fn reads_go_writer_binary_fixture() -> Result<()> {
    // Produced by scripts/make_delta.go with these two trees (includes shifted bsdiff blocks).
    let tmp = tempdir()?;
    let base = tmp.path().join("old");
    let out = tmp.path().join("new");
    fs::create_dir(&base)?;
    let code: Vec<u8> = (0..65536)
        .map(|i| (i * 131 + (i >> 5) * 17) as u8)
        .collect();
    fs::write(base.join("code"), &code)?;
    apply_delta(
        Cursor::new(include_bytes!("fixtures/go-v1.delta")),
        "1",
        "v2",
        &base,
        &out,
    )?;
    assert_eq!(
        fs::read(out.join("code"))?,
        [&code[16384..], b"updated", &code[..16384]].concat()
    );
    assert_eq!(fs::read(out.join("added"))?, b"new file\n");
    Ok(())
}
#[test]
fn delta_roundtrip_mutation_permissions_and_bad_input() -> Result<()> {
    let tmp = tempdir()?;
    let path = |p| tmp.path().join(p);
    let (old, new, out) = (path("old"), path("new"), path("out"));
    fs::create_dir(&old)?;
    fs::create_dir_all(new.join("assets"))?;
    let mut state = 13u32;
    let old_bytes: Vec<_> = (0..3_000_000)
        .map(|_| {
            state ^= state << 13;
            state ^= state >> 17;
            state ^= state << 5;
            state as u8
        })
        .collect();
    let mut code = old_bytes.clone();
    code[361] = 22;
    code.splice(24680..24680, b"extra call".iter().copied());
    code.rotate_left(740_021); // A linker can also reorder whole sections.
    for (dir, bytes) in [(&old, &old_bytes), (&new, &code)] {
        fs::write(dir.join("code"), bytes)?;
        fs::write(dir.join("asset"), b"kept")?;
        fs::create_dir(dir.join("Contents"))?;
        fs::write(dir.join("Contents/CodeResources"), b"stapled ticket")?;
    }
    fs::write(old.join("remove"), b"gone")?;
    fs::write(old.join("move"), b"one resource")?;
    fs::write(new.join("assets/moved"), b"one resource")?;
    fs::write(new.join("empty"), b"")?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::{PermissionsExt, symlink};
        fs::set_permissions(new.join("code"), fs::Permissions::from_mode(0o751))?;
        symlink("../code", new.join("assets/link"))?;
    }
    let mut patch = vec![];
    write_delta(&mut patch, "v1", "2", &old, &new)?;
    assert!(patch.len() < 100_000);
    fs::write(old.join("Contents/CodeResources"), b"restapled locally")?;
    apply_delta(Cursor::new(&patch), "1", "V2", &old, &out)?;
    assert_eq!(fs::read(out.join("code"))?, code);
    assert_eq!(fs::read(out.join("assets/moved"))?, b"one resource");
    assert!(!out.join("remove").exists());
    assert_eq!(fs::metadata(out.join("empty"))?.len(), 0);
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            fs::metadata(out.join("code"))?.permissions().mode() & 0o777,
            0o751
        );
        assert_eq!(
            fs::read_link(out.join("assets/link"))?,
            Path::new("../code")
        );
    }
    let fail = path("failed");
    assert!(apply_delta(Cursor::new(&patch), "9", "2", &old, &fail).is_err());
    assert!(
        apply_delta(
            Cursor::new(&patch[..patch.len() - 1]),
            "1",
            "2",
            &old,
            &fail
        )
        .is_err()
    );
    fs::write(old.join("asset"), b"oops")?;
    assert!(apply_delta(Cursor::new(&patch), "1", "2", &old, &fail).is_err());
    assert!(!fail.exists());
    Ok(())
}
fn zip(path: &Path, entries: &[(&str, &[u8])]) -> Result<()> {
    use std::io::Write;
    let mut zip = zip::ZipWriter::new(File::create(path)?);
    for (name, data) in entries {
        zip.start_file(
            *name,
            zip::write::SimpleFileOptions::default().unix_permissions(0o755),
        )?;
        zip.write_all(data)?;
    }
    zip.finish()?;
    Ok(())
}
#[test]
fn offline_plan_and_install_lock() -> Result<()> {
    let tmp = tempdir()?;
    let target = tmp.path().join("installed");
    fs::create_dir(&target)?;
    fs::write(target.join("old"), b"old")?;
    let archive = tmp.path().join("service.zip");
    zip(&archive, &[("release/bin", b"new")])?;
    let plan = InstallPlan::prepare(&archive, &target, "1", "2")?;
    assert!(target.join("old").exists());
    assert_eq!(fs::read(plan.staged().join("bin"))?, b"new");
    let lock = UpdateLock::acquire(&target)?;
    assert!(plan.install().is_err());
    drop(lock);
    plan.install()?;
    assert!(!target.join("old").exists());
    assert_eq!(fs::read(target.join("bin"))?, b"new");
    Ok(())
}
#[test]
fn extraction_rejects_unsafe_paths_and_links() -> Result<()> {
    let tmp = tempdir()?;
    let archive = tmp.path().join("release.zip");
    let target = tmp.path().join("install");
    zip(&archive, &[("../outside", b"bad")])?;
    assert!(InstallPlan::prepare(&archive, &target, "1", "2").is_err());
    assert!(!tmp.path().join("outside").exists());
    #[cfg(unix)]
    {
        let mut zip = zip::ZipWriter::new(File::create(&archive)?);
        zip.add_symlink(
            "release/link",
            "../sibling",
            zip::write::SimpleFileOptions::default(),
        )?;
        zip.finish()?;
        assert!(InstallPlan::prepare(&archive, &target, "1", "2").is_err());
    }
    assert!(!target.exists());
    Ok(())
}
#[test]
fn credentials_rotate_and_invalid_override_does_not_fall_back() -> Result<()> {
    let tmp = tempdir()?;
    let local = tmp.path().join("local");
    let bundled = tmp.path().join("bundled");
    fs::write(&bundled, b"fixture-bundled")?;
    let read = || read_update_credentials(Some(&local), Some(&bundled));
    assert_eq!(read()?.as_deref(), Some("fixture-bundled"));
    fs::write(&local, b"fixture-local")?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&local, fs::Permissions::from_mode(0o644))?;
        assert!(read().is_err());
        fs::set_permissions(&local, fs::Permissions::from_mode(0o600))?;
    }
    assert_eq!(read()?.as_deref(), Some("fixture-local"));
    fs::write(&local, b"not a valid value")?;
    assert!(read().is_err());
    assert_eq!(
        bundled_token_file(Path::new("Vantry.app/Contents/MacOS/Vantry")).unwrap(),
        Path::new("Vantry.app/Contents/Resources/update-token")
    );
    Ok(())
}

#[test]
fn delta_threshold_rejects_low_savings_and_github_proxy_candidates() -> Result<()> {
    assert!(delta_worth_using(750, 1000, 1));
    assert!(!delta_worth_using(760, 1000, 1));
    assert!(delta_worth_using(650, 1000, 2));
    assert!(!delta_worth_using(660, 1000, 2));

    let releases = parse_releases(
        br#"[
            {"tag_name":"v0.2.18","assets":[{"name":"vantry-0.2.17-to-0.2.18-darwin-arm64.delta","size":10700000}]},
            {"tag_name":"v0.2.19","assets":[
                {"name":"vantry-0.2.18-to-0.2.19-darwin-arm64.delta","size":10700000},
                {"name":"vantry-0.2.19-darwin-arm64.zip","size":24100000}
            ]}
        ]"#,
        Channel::Stable,
    )?;
    // 21.4 MB / 24.1 MB (88.8%) across 2 steps is not worth the extra hop + disk rebuild.
    assert!(
        plan_delta_chain(
            &releases,
            "vantry",
            "darwin-arm64",
            "0.2.17",
            "0.2.19",
            Some(24_100_000)
        )
        .is_empty()
    );

    let proxies = resolve_github_proxies(Some("https://gh-proxy.com"));
    assert_eq!(proxies, vec!["https://gh-proxy.com/".to_owned()]);
    assert!(resolve_github_proxies(Some("none")).is_empty());

    let gh_url = reqwest::Url::parse(
        "https://github.com/muq9738/Vantry/releases/download/v0.2.19/vantry-0.2.19-darwin-arm64.zip",
    )?;
    let candidates = github_download_candidates(&gh_url, false, &proxies);
    assert_eq!(candidates.len(), 2);
    assert!(candidates[0].1);
    assert_eq!(
        candidates[0].0.as_str(),
        "https://gh-proxy.com/https://github.com/muq9738/Vantry/releases/download/v0.2.19/vantry-0.2.19-darwin-arm64.zip"
    );
    assert!(!candidates[1].1);
    assert_eq!(candidates[1].0, gh_url);

    // Authenticated or non-GitHub URLs are never rewritten.
    let auth_candidates = github_download_candidates(&gh_url, true, &proxies);
    assert_eq!(auth_candidates.len(), 1);
    assert!(!auth_candidates[0].1);

    let local_url = reqwest::Url::parse("http://127.0.0.1:8080/delta")?;
    let local_candidates = github_download_candidates(&local_url, false, &proxies);
    assert_eq!(local_candidates.len(), 1);
    assert!(!local_candidates[0].1);
    Ok(())
}

#[test]
fn cross_version_direct_and_skiplist_delta_planning() -> Result<()> {
    let releases = parse_releases(
        br#"[
            {"tag_name":"v0.2.20","assets":[
                {"name":"vantry-0.2.15-to-0.2.20-darwin-arm64.delta","size":5200000}
            ]},
            {"tag_name":"v0.2.22","assets":[
                {"name":"vantry-0.2.21-to-0.2.22-darwin-arm64.delta","size":4800000}
            ]},
            {"tag_name":"v0.2.23","assets":[
                {"name":"vantry-0.2.22-to-0.2.23-darwin-arm64.delta","size":4200000},
                {"name":"vantry-0.2.21-to-0.2.23-darwin-arm64.delta","size":6300000},
                {"name":"vantry-0.2.20-to-0.2.23-darwin-arm64.delta","size":6500000},
                {"name":"vantry-0.2.23-darwin-arm64.zip","size":24100000}
            ]}
        ]"#,
        Channel::Stable,
    )?;

    // Direct 0.2.21 -> 0.2.23 (6.3 MB, 1 hop) beats 2-hop chain (4.8 + 4.2 = 9.0 MB).
    let direct = plan_delta_chain(
        &releases,
        "vantry",
        "darwin-arm64",
        "0.2.21",
        "0.2.23",
        Some(24_100_000),
    );
    assert_eq!(direct.len(), 1);
    assert_eq!(direct[0].from, "0.2.21");
    assert_eq!(direct[0].to, "0.2.23");

    // 8 versions behind (0.2.15 -> 0.2.23) bridges in 2 skip-list hops (11.7 MB = 48.5% < 65%).
    let skiplist = plan_delta_chain(
        &releases,
        "vantry",
        "darwin-arm64",
        "0.2.15",
        "0.2.23",
        Some(24_100_000),
    );
    assert_eq!(skiplist.len(), 2);
    assert_eq!((skiplist[0].from.as_str(), skiplist[0].to.as_str()), ("0.2.15", "0.2.20"));
    assert_eq!((skiplist[1].from.as_str(), skiplist[1].to.as_str()), ("0.2.20", "0.2.23"));
    Ok(())
}

#[test]
fn bsdiff_compresses_shifted_binary_with_frequent_relocations() -> Result<()> {
    let tmp = tempdir()?;
    let (old, new, out) = (tmp.path().join("old"), tmp.path().join("new"), tmp.path().join("out"));
    fs::create_dir(&old)?;
    fs::create_dir(&new)?;

    // Simulate an ARM64 binary where a 4-byte branch immediate changes every 24 bytes
    // (no 32-byte verbatim anchor exists anywhere!) AND functions shift by +137 bytes.
    let mut state = 0x1234_5678u32;
    let mut base: Vec<u8> = (0..200_000)
        .map(|_| {
            state ^= state << 13;
            state ^= state >> 17;
            state ^= state << 5;
            state as u8
        })
        .collect();
    let mut target = vec![0xAAu8; 137];
    target.extend_from_slice(&base);
    for off in (0..base.len()).step_by(24) {
        target[137 + off] = target[137 + off].wrapping_add(4);
    }
    base[0] ^= 0xFF;

    fs::write(old.join("app.bin"), &base)?;
    fs::write(new.join("app.bin"), &target)?;

    let mut patch = Vec::new();
    write_delta(&mut patch, "0.2.21", "0.2.23", &old, &new)?;
    // Real bsdiff captures all shifted bytes via fuzzy diffing, compressing well below 25% of raw size.
    assert!(
        patch.len() < 40_000,
        "expected tight bsdiff patch, got {} bytes",
        patch.len()
    );
    apply_delta(Cursor::new(&patch), "0.2.21", "0.2.23", &old, &out)?;
    assert_eq!(fs::read(out.join("app.bin"))?, target);
    Ok(())
}
