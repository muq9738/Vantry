<p align="center">
  <img src="resources/icon.png" width="88" alt="Vantry 图标" />
</p>

<h1 align="center">Vantry</h1>

<p align="center">
  面向 AI 编程与多 Agent 协作的原生桌面工作台。<br />
  将会话、代理、Git 变更和远程机器放在同一个工作空间。
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-237a57?style=flat-square" alt="开源许可：MIT" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.27.3%2B-007d9c?style=flat-square" alt="Go 1.27.3 或更高版本" /></a>
  <a href="https://github.com/muq9738/Vantry/releases/latest"><img src="https://img.shields.io/badge/download-releases-25292e?style=flat-square" alt="下载发行版" /></a>
</p>

<p align="center">
  <a href="README.md">English</a> · <strong>简体中文</strong> · <a href="README.zh-TW.md">繁體中文</a>
</p>

<p align="center">
  <a href="#快速开始">快速开始</a> ·
  <a href="#构建与开发">开发指南</a> ·
  <a href="#文档">文档</a> ·
  <a href="#开源许可">开源许可</a>
</p>

---

Vantry 使用 **Go + MyGo 构建原生界面**，由持久化的 **Core** 管理任务，通过 **Pi** 执行模型。会话和任务归所属 Core 管理，关闭窗口后工作仍可继续。沿用已有的 Pi 模型和凭据，在本机或已连接的机器上编程，并在同一工作台中审阅变更。

## 主要功能

| 场景 | 能力 |
| --- | --- |
| **编程工作台** | 流式会话、模型选择、多行输入、附件、技能选择与行内会话引用。 |
| **Agent 协作** | 持久化父子任务、独立 Git worktree、实时状态、结果交付与连续追问。 |
| **Git 与文件** | 文件浏览、差异查看、暂存、提交、分支管理，以及支持筛选和提交操作的图形日志。 |
| **多机工作** | 按机器和项目聚合会话，支持远程执行、配对码与可选的自建中继。 |
| **执行控制** | 工具审批、消息排队、运行中调整指令、持久事件与 Core 重启恢复。 |
| **插件扩展** | 原生页面、显式进程接口，以及远程桌面插件。 |

界面支持 **简体中文、英文与日文**。

### 远程桌面插件

远程桌面通过插件提供，支持原生 H.264 画面和键鼠控制，可使用局域网或自己的中继。按需开启共享，每次连接均由共享端确认；macOS 需要屏幕录制和辅助功能权限。应用不预设任何中继地址。

## 快速开始

### 下载

从 [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest) 获取安装包。

| 平台 | 当前支持情况 |
| --- | --- |
| **macOS · Apple Silicon** | macOS 13 或更高版本。解压后将 `Vantry.app` 移至 `/Applications`。当前发行版使用本地签名，尚未经过 Apple 公证。 |
| **Windows · x64** | ZIP 预览版。当前发行版由交叉编译生成，尚未通过 Windows 真机验证，也未包含原生远程桌面 helper。 |
| **Linux** | 暂无经过验证的桌面发行版。 |

### 开始使用

1. 在执行 AI 任务的电脑上安装并配置 **Pi CLI**。Vantry 使用其模型配置和凭据，应用本身不附带 Pi。
2. 打开 Vantry，选择项目文件夹和模型。可以直接使用目录，也可以新建独立 worktree。
3. 开始会话，查看工具执行过程，按需处理审批，并在 Git 面板中审阅修改。

跨机工作时，通过设备入口与另一台 Core 配对。需要共享屏幕时，使用远程桌面插件及其独立的共享码。

更新会在后台下载、校验（支持二进制增量包 `.delta`），确认 Core 空闲后支持一键重启并应用。详见 [更新机制](docs/updates.md)。

## 构建与开发

### 环境要求

- **Go 1.27.3 或更高版本**及 Git。
- 执行真实 AI 任务时，需要已配置模型服务的 **Pi CLI**（`@earendil-works/pi-coding-agent`）。
- 构建 macOS 原生远程桌面 helper 时，需要 **macOS、Xcode Command Line Tools** 和 Python 3。
- 随包远程桌面插件（[vantry-plugin-remote-desktop](https://github.com/muq9738/vantry-plugin-remote-desktop)）作为公开 Go 模块自动拉取，原生 helper 构建要求见 [插件开发说明](docs/plugin-repositories.md)。

### 从源码运行

```sh
git clone https://github.com/muq9738/Vantry.git
cd Vantry

go mod download
go build -o bin/vantry ./cmd/vantry

./bin/vantry desktop --workspace "$PWD"
```

### 打包 macOS 应用

先构建配套的原生 helper，再打包应用：

```sh
python3 scripts/build_remote_helper.py --platform darwin/arm64
go tool mygo build -platform darwin/arm64 -skip-dmg
```

产物位于 `dist/darwin-arm64/Vantry.app`。签名流程见 [macOS 打包](docs/packaging-macos.md)，Windows SDK 与 helper 要求见 [Windows 开发说明](docs/windows-remote-desktop.md)。

### 检查修改

```sh
go test ./... -timeout=120s
go vet ./...
```

部分硬件和集成检查需要显式启用。交叉编译成功不能代替对应平台的实际运行验证。

## 架构

| 组件 | 职责 |
| --- | --- |
| **Desktop** | MyGo 原生界面、会话时间线、输入区、文件浏览、Git 与插件页面。 |
| **Core** | 基于 SQLite 的会话、任务编排、审批、工作区、配对与事件交付。 |
| **Pi workers** | 在任务所属机器上执行模型与工具。 |
| **连接层** | 通过直连 TLS 或已配置的中继访问会话；桌面共享单独授权。 |

应用入口在 `cmd/vantry`，界面在 `internal/desktop`，任务行为主要在 `internal/core` 和 `internal/app`，公共接口在 `pkg/api`。原生界面渲染不依赖 HTML 前端。

## 文档

| 文档 | 内容 |
| --- | --- |
| [本地开发](docs/getting-started.md) | Core 生命周期、执行设置与 CLI 示例。 |
| [桌面界面](docs/desktop-ui.md) | 原生 Stardust 界面、交互与面板说明。 |
| [提供方与 Pi](docs/providers.md) | 外置 Pi CLI 探测、配置与安全升级。 |
| [Agent 编排](docs/orchestration-implementation.md) | 父子任务、结果交付与恢复。 |
| [远程连接](docs/native-connectivity.md) | 配对、远程会话、桌面共享与中继部署。 |
| [远程桌面](docs/remote-desktop.md) | 局域网直连、惰性中继回退与 macOS 权限。 |
| [更新机制](docs/updates.md) | 增量更新、安全重启应用与可选私有凭据。 |
| [行为与能力边界](docs/behavior-gaps.md) | 实现边界与后续工作。 |

## 致谢

Vantry 是基于 Go + MyGo 的独立原生实现，在设计思路与协议兼容上受到以下优秀开源项目的启发与支持：

- **[T3 Code](https://github.com/pingdotgg/t3code)** — 多任务编排设计启发与协议 2 会话兼容。
- **[MyGo](https://github.com/egoist/mygo)** — 原生桌面 UI 框架。
- **[Ghostty](https://github.com/ghostty-org/ghostty)** — 原生终端模拟库（`libghostty-vt`）。
- **[Pi CLI](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)** — 外置 AI 编程 Agent 运行时。
- **[Pion](https://github.com/pion/webrtc)**、**[Mermaid](https://mermaid.js.org)**、**[KaTeX](https://katex.org)** 与 **[Lucide](https://lucide.dev)** — WebRTC/TURN 传输、离线图表、公式渲染与图标资源。

## 开源许可

Vantry 的原创代码和文档采用 **[MIT License](LICENSE)**：允许使用、修改、再分发和商业使用，保留版权声明与许可文本即可。软件按原样提供，不附带担保。

依赖与单独维护的插件保留各自的许可条款和访问要求，详见[第三方声明](THIRD_PARTY_NOTICES.md)和 [`third_party/licenses`](third_party/licenses) 中的许可原文。
