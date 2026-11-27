<p align="center">
  <img src="resources/icon.png" width="88" alt="Vantry 图标" />
</p>

<h1 align="center">Vantry</h1>

<p align="center">
  面向 AI 编程的 Rust 原生桌面工作区。<br />
  在同一窗口中统筹对话、多智能体任务、终端、Git 差异与跨机环境。
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-237a57?style=flat-square" alt="License: MIT" /></a>
  <a href="Cargo.toml"><img src="https://img.shields.io/badge/Rust-1.97%2B-c6531a?style=flat-square" alt="Rust 1.97+" /></a>
  <a href="https://github.com/muq9738/Vantry/releases/latest"><img src="https://img.shields.io/github/v/release/muq9738/Vantry?style=flat-square&label=release&color=25292e" alt="最新版本" /></a>
</p>

<p align="center">
  <a href="README.md">English</a> · <strong>简体中文</strong> · <a href="README.zh-TW.md">繁體中文</a>
</p>

<p align="center">
  <a href="#核心能力">核心能力</a> ·
  <a href="#安装与上手">安装与上手</a> ·
  <a href="#架构概览">架构概览</a> ·
  <a href="#从源码构建">从源码构建</a> ·
  <a href="#文档">文档</a> ·
  <a href="#开源许可">开源许可</a>
</p>

---

Vantry 将纯 Rust 的 **[NSH](https://github.com/longbridge/nsh) 原生桌面**、基于 SQLite 的持久化 **Core** 与内置 **[Pi](https://github.com/yannelli/pi-rust)** 运行时融为一体——不依赖 Electron、WebView 或外置 Pi CLI。会话与任务归属各自机器上的 Core 而非单个窗口：随时关闭桌面不影响后台智能体执行，也可跨机重连并在同一工作区内审查所有代码变更。

## 核心能力

| 模块 | 能力亮点 |
| --- | --- |
| **原生编程工作台** | GPU 渲染的 Stardust（星尘）浅色/深色界面，支持流式 Markdown、表格、Diff、图表、行内 `@` 文件/图片/会话引用（单条最多 32 个附件并支持点击预览）、模型与思考档位切换及技能选择。 |
| **持久化执行与实时控制** | 任务运行中支持消息排队与实时 `steer` 引导（可编辑、重排、转为追加、取消）；提供 `auto` / `auto-accept-edits` / `ask` / `plan` 权限模式、模型专用密钥输入，以及关闭窗口或重启 Core 后的状态恢复。 |
| **多智能体任务编排** | 持久父子任务委派、独立 Git worktree 隔离、权限上限继承、会话概览实时查看子任务状态与输出、自动或手动结果交付回父会话、子任务追问与级联停止。 |
| **文件、Git 与终端** | 右侧工具坞集成文件树、搜索与只读预览、Git 状态/差异/暂存/分支切换/提交/PR 摘要，以及内嵌终端。 |
| **多机互联与远程协作** | 已配对的 Vantry 电脑通过局域网或 Alma 中继连接。Alma iOS 可以通过局域网或 Alma 兼容的 HTTPS 中继访问共享会话。 |
| **MCP、技能与插件** | 原生支持 Pi MCP（`~/.pi/agent/mcp.json` 与项目 `.pi/mcp.json`），采用 Codemode/按需工具检索、显式项目信任与连接探活；支持声明式原生插件面板及独立授权的远程桌面。 |

桌面界面内置 **简体中文**、**English** 与 **日本語**。

## 安装与上手

前往 [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest) 下载预构建安装包。

| 平台 | 安装方式与说明 |
| --- | --- |
| **macOS · Apple Silicon** | 需要 macOS 14（Sonoma）或更高版本。打开 `Vantry-<版本>-macos-arm64.dmg`，将 `Vantry.app` 拖入「应用程序」。同时提供独立的 `vantry-service` 无界面服务包。 |
| **Windows · x64** | 解压便携包 `vantry-<版本>-windows-amd64.zip` 并运行 `Vantry.exe`。同时提供独立的 `vantry-service` 服务包。 |
| **Linux** | 支持从源码构建。`vantry-service` 与 `vantry-relay` 可无界面运行；NSH 桌面需 GTK 4.14+，暂未提供预构建桌面发行包。 |

### 快速开始

1. **配置模型提供方** —— 打开 **设置 → 提供方** 填写 API Key 或自定义端点，或直接沿用 `~/.pi/agent`（`PI_CODING_AGENT_DIR`）中已有的 Pi 配置。Vantry 已内置 Pi 运行时，无需额外安装 Node.js 或外部 Pi CLI。
2. **打开项目** —— 选择本地项目目录（或创建独立 Git worktree），选定模型与思考档位。
3. **开始会话** —— 在对话流中审查折叠的工具调用、按需审批敏感操作、在运行中随时排队或追加引导，并在右侧 Git 面板审查所有变更。

应用更新在后台静默下载并完成 SHA-256 校验（支持二进制 `.delta` 增量补丁），在 Core 空闲时支持一键重启并生效；桌面启动时也会自动替换空闲的旧版本本地 Core，不会中断正在执行的任务。

## 跨机互联与远程桌面

- **会话配对** —— 两台电脑在「设置 → 环境」使用主机生成的完整链接配对，支持局域网和 Alma 公网中继，侧栏可按机器查看会话。手机在外网时，在所选会话的 Alma iOS 配对窗口填写 Alma 中继的公网地址。
- **远程桌面** —— 主机开启共享，另一台电脑使用局域网邀请链接连接。macOS 采集画面需要「屏幕录制」权限，键鼠控制需要「辅助功能」权限。

## 架构概览

Vantry 将界面渲染、持久状态管理与智能体执行隔离在不同进程中，确保关闭窗口不中断任务，Core 重启也不会重复提交 Prompt：

```text
NSH 桌面 (vantry) / CLI / 已配对客户端
       │  本地 IPC（Unix socket / Windows）· 远端 mTLS / 中继
       ▼
Core (vantry-service core) ── SQLite（state.sqlite：事件、回执、任务、outbox）
       │  拉起或重新附着持久 Attempt
       ▼
Worker (vantry-service provider-worker) ── 内置 pi-rust ── 模型服务与 MCP
```

### 工作区结构

| 目录 | 职责 |
| --- | --- |
| `apps/desktop`（`vantry`） | NSH 原生桌面界面与统一 CLI 入口（服务端模式自动转发给 `vantry-service`）。 |
| `apps/service`（`vantry-service`） | 持久化 Core 守护进程、持久 Pi worker、`ctl`、`peer-proxy` 与更新安装器。 |
| `apps/relay`（`vantry-relay`） | 独立自建会话与桌面中继服务（不引入 UI 与 Pi 依赖）。 |
| `crates/core` | SQLite 事务存储、命令幂等回执、父子任务调度器、outbox 与事件流。 |
| `crates/pi-runtime` | 内置 `pi-rust` 会话宿主、模型目录、MCP 桥接、审批交互与执行日志。 |
| `crates/protocol`、`crates/client` | v1 协议类型与支持 gzip 压缩、游标续传的 HTTP/NDJSON 客户端。 |
| `crates/connectivity` | 设备身份、电脑 TLS 配对与 Alma 公网中继、局域网桌面传输及 Alma 手机端到端加密连接。 |
| `crates/platform` | 工作区与 worktree 边界校验、Git 适配、PTY、插件宿主与系统能力。 |
| `crates/updater`、`tools/xtask` | 增量/完整更新引擎与发布打包工具（`package`、`make-delta`、`prepare-icon`）。 |

## 从源码构建

### 环境要求

- **Rust 1.97 或更高版本**（Edition 2024）、**Git**，以及目标平台的原生编译工具链（macOS 需 Xcode Command Line Tools；Windows 需 MSVC；Linux 桌面需 GTK 4.14+ 开发库）。

### 构建与运行

```sh
git clone https://github.com/muq9738/Vantry.git
cd Vantry

cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
```

常用启动模式：

```sh
# 使用内存演示 Core 预览界面（无需配置模型 API Key）
target/debug/vantry desktop --mock

# 仅连接已运行的本地 Core，不自动拉起
target/debug/vantry desktop --attach-only

# 直接打开已配对的远端机器（auto | direct | relay）
target/debug/vantry desktop --peer <PEER_ID> --peer-mode auto

# 独立运行无界面 Core 服务
target/debug/vantry-service core --data-dir /path/to/vantry-data
```

### 测试与打包

```sh
cargo test --workspace --locked
cargo run --locked -p xtask -- package desktop
```

- 在 Apple Silicon macOS 上，桌面产物（`Vantry.app`、`.dmg` 与更新用 `.zip`）输出在 `target/package-macos-arm64`。
- 其他目标平台输出至 `target/package-<产物>-<系统>-<架构>`（可通过 `--out-dir <DIR>` 指定）。`xtask package` 同时支持打包 `service` 与 `relay`。

## 文档

工程文档以中文为主。部分设计文档写于纯 Rust 迁移之前，仍保留核心协议与行为契约说明。

| 文档 | 主题 |
| --- | --- |
| [本地启动与开发](docs/getting-started.md) | 源码启动参数、演示/附着模式与本地打包。 |
| [Rust 架构迁移方案](docs/nsh-core-pi-rust-plan.md) | 进程边界、内置 `pi-rust` 集成与故障恢复契约。 |
| [桌面界面说明](docs/desktop-ui.md) | Stardust 视觉体系、输入交互与工具坞面板。 |
| [任务编排](docs/task-orchestration.md) | 父子任务委派、工作区隔离与结果续行规则。 |
| [MCP 集成](docs/mcp-integration.md) | Codemode 优先的 MCP 发现、项目信任与连接探活。 |
| [原生远程连接](docs/native-connectivity.md) | 电脑与 Alma iOS 的局域网和公网配对。 |
| [远程桌面](docs/remote-desktop.md) | 局域网屏幕共享与 macOS 权限。 |
| [更新机制](docs/updates.md) | 后台校验、`.delta` 增量补丁与 Core 空闲交接。 |
| [版本发布记录](docs/releases/) | 各版本变更说明与下载链接。 |

## 致谢

Vantry 得益于以下优秀开源项目的构建与启发：

- **[NSH](https://github.com/longbridge/nsh)** —— 纯 Rust 原生 GPU 桌面 UI 框架。
- **[pi-rust](https://github.com/yannelli/pi-rust)** —— 内置 `pi-coding-agent` 会话运行时与扩展宿主。
- **[T3 Code](https://github.com/pingdotgg/t3code)** —— 任务编排流程的设计参考。
- **[Lucide](https://lucide.dev)**、**[Mermaid](https://mermaid.js.org)** 与 **[KaTeX](https://katex.org)** —— 图标设计、图表与数学公式渲染支持。

## 开源许可

Vantry 的原创源码与文档采用 **[MIT License](LICENSE)** 开源。依赖组件与独立维护的插件保留各自授权条款，详见 [第三方声明](THIRD_PARTY_NOTICES.md) 与 [`third_party/licenses`](third_party/licenses)。
