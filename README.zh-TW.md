# Vantry

[English](README.md) · [简体中文](README.zh-CN.md) · 繁體中文

Vantry 是用於 AI 程式開發的 Rust 原生工作區。NSH 桌面將對話、附件、終端機和 Git 差異整合在同一視窗。Core 在 SQLite 儲存對話，在任務所屬機器上排程代理程式，並執行內建 Pi。

## 安裝

在執行 macOS 14 或更新版本的 Apple Silicon 電腦上，下載磁碟映像檔，將 Vantry.app 拖入「應用程式」。設定模型供應商、開啟專案，即可開始對話。 [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest)

## 連線到其他機器

配對後可以直接連線到另一台機器的 Core，也可以使用已設定的中繼。v0.1.42 用戶端可沿用原有身分和配對。在舊版桌面重新整理機器連線並選取主機即可。螢幕共享使用獨立工作階段。

## 建置與測試

需要 Rust 1.97 或更新版本、Git 和目標平台的原生工具鏈。在儲存庫根目錄執行以下命令。

```sh
cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
cargo test --workspace --locked
```

使用 `cargo run --locked -p xtask -- package desktop` 打包。Apple Silicon 安裝包位於 `target/package-macos-arm64`。

[MIT License](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)
