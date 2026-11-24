# Vantry

English · [简体中文](README.zh-CN.md) · [繁體中文](README.zh-TW.md)

Vantry is a native Rust workspace for coding with AI agents. The NSH desktop brings conversations, attachments, a terminal and Git diffs into one window. The Core stores conversations in SQLite, schedules agents on their owning machine and runs the embedded Pi runtime.

## Install

Download the Apple Silicon disk image and drag Vantry.app into Applications (macOS 14 or later). Configure a model provider, open a project and start a conversation. [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest)

## Connect to a machine

A paired desktop can open another Core directly or through a configured relay. Existing v0.1.42 session pairings use the same identity and relay settings. From the older desktop, refresh machine connections and select the host. Screen sharing uses a separate session.

## Build and test

Use Rust 1.97 or later, Git and the native toolchain for your platform. Run these commands from the repository root.

```sh
cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
cargo test --workspace --locked
```

Package the desktop with `cargo run --locked -p xtask -- package desktop`. The macOS Apple Silicon output is in `target/package-macos-arm64`.

[MIT License](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)
