# Vantry

[English](README.md) · 简体中文 · [繁體中文](README.zh-TW.md)

Vantry 是用于 AI 编程的 Rust 原生工作区。NSH 桌面把会话、附件、终端和 Git 差异放在同一窗口。Core 在 SQLite 中保存会话，在任务所属机器上调度智能体，并运行内置 Pi。

## 安装

在 macOS 14 或更高版本的 Apple Silicon 机器上，下载安装镜像，将 Vantry.app 拖到“应用程序”。配置模型提供方、打开项目，即可开始会话。 [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest)

## 连接机器

配对后可以直连另一台机器的 Core，也可以使用已配置的中继。v0.1.42 客户端可沿用原有身份和配对。在旧桌面中刷新机器连接并选择主机即可。屏幕共享使用独立会话。

## 构建与测试

需要 Rust 1.97 或更高版本、Git 和目标平台的原生工具链。在仓库根目录运行以下命令。

```sh
cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
cargo test --workspace --locked
```

使用 `cargo run --locked -p xtask -- package desktop` 打包。Apple Silicon 安装包位于 `target/package-macos-arm64`。

[MIT License](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)
