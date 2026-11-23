use super::*;
use serde_json::json;
use std::{collections::BTreeMap, path::Path, process::Command};

#[test]
fn paths_with_only_an_explicit_data_directory() {
    let path = std::env::current_dir().unwrap().join("explicit data");
    let paths = hostplatform::Paths::from_env(|key| {
        (key == "VANTRY_DATA_DIR").then(|| path.clone().into())
    })
    .unwrap();
    assert_eq!(paths.data, path);
    assert_eq!(paths.config, path);
    assert_eq!(paths.socket, path.join("core.sock"));
}
#[test]
fn rooted_paths_accept_normal_components_only() {
    for p in ["", ".", "..", "./..", "a/../b", "/absolute"] {
        assert!(hostfs::relative(Path::new(p)).is_err(), "{p}");
    }
    for p in ["new file.txt", "folder/-n", "./folder/x"] {
        assert!(hostfs::relative(Path::new(p)).is_ok(), "{p}");
    }
}
#[test]
fn execution_environment_retains_application_options() {
    let mut command = Command::new("git");
    command
        .env("GIT_DIR", "wrong")
        .env("GIT_CONFIG_COUNT", "1")
        .env("MY_VAR", "value");
    hostprocess::clean_environment(&mut command);
    let env: BTreeMap<_, _> = command.get_envs().collect();
    assert_eq!(env[std::ffi::OsStr::new("GIT_DIR")], None);
    assert_eq!(
        env[std::ffi::OsStr::new("GIT_CONFIG_COUNT")],
        Some("1".as_ref())
    );
    assert_eq!(env[std::ffi::OsStr::new("MY_VAR")], Some("value".as_ref()));
}
#[test]
fn catalog_rejects_unusable_forward_targets() {
    let e = environments::Entry {
        id: "remote".into(),
        name: "remote".into(),
        host: "alice@dev".into(),
        ..Default::default()
    };
    for socket in [
        "relative",
        "~other/sock",
        "~/",
        "/tmp/sock:other",
        "/tmp/sock\n",
    ] {
        let mut invalid = e.clone();
        invalid.socket_path = socket.into();
        assert!(invalid.validate().is_err(), "{socket:?}");
    }
    assert!(e.resolve_socket("").is_err());
    assert!(e.validate().is_ok());
}
#[test]
fn legacy_and_object_permissions_agree() {
    let m = plugins::Manifest {
        permissions: json!(["workspace:read", "network:*.example", "process:tool"]),
        ..Default::default()
    };
    let object = plugins::Manifest {
        permissions: json!({"workspaceRead":true,"network":["*.example"],"process":["tool"]}),
        ..Default::default()
    };
    for m in [m, object] {
        let p = m.grants().unwrap();
        assert!(p.workspace_read);
        assert_eq!(p.process, ["tool"]);
        assert!(p.allows_host("A.Example"));
        assert!(!p.allows_host("example"));
    }
    for bad in [
        json!(["network:*broken"]),
        json!(["process:../tool"]),
        json!({"unknown":true}),
    ] {
        assert!(
            plugins::Manifest {
                permissions: bad,
                ..Default::default()
            }
            .grants()
            .is_err()
        );
    }
}
#[test]
fn remote_desktop_descriptor_is_a_user_installation() {
    let m = plugins::remote_desktop_descriptor();
    assert_eq!(m.id, "vantry-remote-desktop");
    assert_eq!(m.version, "0.3.1");
    assert!(!m.builtin);
    assert!(m.command.is_empty());
    assert_eq!(m.navigation[0].native_panel, "remote-desktop");
}
