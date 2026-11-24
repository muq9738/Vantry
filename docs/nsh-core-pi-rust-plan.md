# Vantry 全量 Rust 重构技术方案

**目标版本以 Vantry `v0.1.42`（`693d3ec`）为行为和数据基线。** 桌面改用 NSH，Core、Pi worker、CLI、独立中继、插件宿主和原有 Go 开发工具全部用 Rust 实现。Pi 使用 `yannelli/pi-rust` 的完整 `pi-coding-agent`，在执行进程内直接调用。最终源码中的 Go 文件数为零，构建、测试、打包、安装、升级和运行均可在没有 Go 工具链的环境完成。

本文是实施设计。已验证的是 Rust 会话库可以读取原有 Pi 配置、调用模型接口和执行一个 TypeScript 扩展。完整 NSH 桌面、Rust Core、现有 Vantry 扩展以及全 Rust 发行包的体积仍待实施验证。[Vantry 基线](https://github.com/muq9738/Vantry/tree/v0.1.42) · [NSH v0.6.1](https://github.com/longbridge/nsh/releases/tag/v0.6.1) · [pi-rust v0.12.31](https://github.com/yannelli/pi-rust/releases/tag/v0.12.31)

## 交付物与进程边界

| 交付物 | 运行职责 | 发布方式 |
|---|---|---|
| `Vantry` 桌面程序 | NSH 原生窗口、工作台、会话列表、IME、终端、Git、审批与远端环境选择 | 随桌面包发布，只链接 UI 和 Core 客户端 |
| `vantry-service` | 以 `core` 运行时持有 SQLite 和调度器，以 `provider-worker` 运行时执行一个 attempt。实现原有 `ctl`、更新及服务管理入口 | 桌面包内携带一份，也可作为无界面服务独立部署。既有 `vantry …` 命令由入口分派到相应角色 |
| `vantry-relay` | 自建中继服务。复用 Rust 网络库，沿用 `serve --config`、配置格式和 systemd 服务入口 | 独立服务端产物，按部署平台打包，依赖树排除桌面和 Pi 引擎 |
| 平台原生 helper | 采集、H.264 编解码、授权与输入注入。原有 macOS Swift、Windows 原生 helper 继续使用平台工具链构建 | 桌面包携带匹配架构的 helper。原来在 Go 插件中的 RTC、会话和控制代码迁入 Rust |

桌面包有两个 Vantry 主程序及平台 helper。Pi 引擎只链接进服务程序一次，Core 与 worker 通过不同进程角色使用同一份二进制。NSH 选纯 Rust 模板，Pi 侧保留运行 TypeScript 扩展所需的 QuickJS。[NSH Rust 模板](https://github.com/longbridge/nsh/blob/v0.6.1/crates/nsh-cli/src/templates/rust/main.rs) · [现有独立中继入口](https://github.com/muq9738/Vantry/blob/v0.1.42/deploy/relay/vantry-relay.service)

```text
NSH Desktop / CLI / 已配对客户端
       │  本地 HTTP over Unix socket，远端沿用既有配对和 TLS
       ▼
Rust Core ── state.sqlite（命令回执、任务、outbox、全局事件游标）
       │  启动或重附着 attempt，传递停止和审批，读取已落盘事件
       ▼
Rust worker ── Rust API ── pi-coding-agent ── 用户配置的模型端点
       ├── jobs/<attemptId>/           执行日志、控制交付和结果
       └── sessions/<threadId>.jsonl   Pi 会话历史
```

保留独立 worker 可以延续 0.1.42 的恢复能力。关闭窗口后任务继续。Core 被强退时，worker 仍可接收模型输出并写日志，需由 Core 批准的操作按原期限等待。Core 重启后重新附着同一个 attempt。若 worker 在提交 prompt 后崩溃且交付状态不明，记录中断并保留现场，用户可显式创建新 attempt。这样既能移去外部 Pi 进程和 stdio Pi RPC，也避免为恢复而重复调用模型或执行工具。[持久执行合同](https://github.com/muq9738/Vantry/blob/v0.1.42/pkg/provider/provider.go) · [既有恢复测试](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/core/durable_test.go)

## Go 源码迁移清单

按 `git ls-tree -r --name-only v0.1.42` 统计，标签中有 **336 个非测试 Go 文件和 241 个 Go 测试文件，共 577 个**。下表覆盖全部路径。测试列统计文件数，迁移按文件中实际断言的行为验收。

| 基线路径 | 非测试 | 测试 | Rust 落点与交付内容 |
|---|---:|---:|---|
| `cmd/vantry`、`pkg/api`、`pkg/provider` | 19 | 7 | CLI、协议类型和执行接口。保持命令参数、退出码、JSON 字段、错误及能力握手 |
| `internal/{app,core,scheduler,telemetry}` | 55 | 63 | Core、SQLite、实例锁、任务与父子调度、队列、用量和恢复 |
| `internal/adapters/provider`、`internal/mcpconfig` | 37 | 26 | pi-rust 适配、受控测试 Provider、配置合并、MCP、审批和执行沙箱 |
| `internal/desktop` | 112 | 74 | NSH 工作台、时间线、附件、Git、终端、设置、插件面板和远程桌面视图 |
| `internal/adapters/{attachments,files,git,pr,preview,workbench,workspace}`、`internal/projects` | 35 | 24 | 文件和附件、Git/PR、预览、worktree 归属、项目与工作台 |
| `internal/{hostfs,hostplatform,hostprocess,sshconfig}` | 18 | 9 | Unix 与 Windows 的进程归属、权限、文件锁、系统能力和 SSH 配置 |
| `internal/adapters/transport`、`internal/{connectivity,environments,remoteaccess,t3compat}` | 37 | 26 | 本机与 SSH 传输、配对、TLS、环境隔离和现有 T3 客户端互操作 |
| `cmd/vantry-relay`、`internal/{desktoprelay,relaycmd}` | 5 | 3 | 独立 relay、会话与桌面中继、信令、TURN 和服务生命周期 |
| `internal/plugins` | 7 | 4 | 原生与进程插件宿主、清单、安装、事件和权限 |
| `internal/updater`、`scripts/{make_delta,prepare_icon}.go` | 11 | 5 | Rust 更新器、差分包读写、离线安装、图标与发布工具 |
| **合计** | **336** | **241** | 发行阶段由 Rust 测试和平台端到端测试承担日常回归 |

迁移期可从历史标签运行 Go 基准程序，与候选 Rust 实现对照。切换完成后，活动源码树移除所有 `.go`、`go.mod`、`go.sum` 与 MyGo 构建入口，历史版本保留在 Git 标签中。原有 Python 黑盒测试可继续覆盖 Rust 产物。外部原生 helper 和用户的 TypeScript 扩展有独立接口，其 Vantry 宿主及原有 Go 部分均纳入上表。[原始依赖](https://github.com/muq9738/Vantry/blob/v0.1.42/go.mod) · [插件宿主与 helper](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/plugin-repositories.md)

## Cargo workspace 与依赖方向

| 建议目录 | 用途 |
|---|---|
| `apps/desktop`、`apps/service`、`apps/relay` | 分别构建桌面、服务和独立中继。桌面沿用现有 CLI 参数分派，服务端可在无图形环境运行 |
| `crates/protocol`、`crates/client` | Serde DTO、版本与能力、错误、快照、NDJSON 订阅和游标恢复。供桌面与命令行共用 |
| `crates/core` | 业务状态机、SQLite 事务、查询、调度、outbox 和恢复。通过接口使用执行器与工作区 |
| `crates/pi-runtime` | pi-rust 会话、模型目录、配置、工具回调、控制和执行日志。将上游 API 变化限制在此处 |
| `crates/platform`、`crates/connectivity` | 工作区与 Git、PTY、插件宿主、平台权限，以及本机/远端传输、设备配对和媒体桥接 |
| `crates/updater`、`tools/xtask` | 运行时更新与构建期打包，共用差分格式。图标、helper 收集和归档由构建工具执行 |

Core 不依赖 NSH，桌面只消费协议客户端，独立 relay 只复用所需的网络与平台模块。Pi 上游 workspace 设有 `publish = false`，建议用固定 Git revision `f11ea9d` 引入 `pi-coding-agent`，提交 `Cargo.lock`。NSH 以 `v0.6.1` 为基线并声明 Rust 1.97，联调先用已成功构建 pi-rust 的 Rust 1.100。Core 选与 Pi 依赖兼容的 `rusqlite`，统一 SQLite 原生链接依赖。[Pi 构建清单](https://github.com/yannelli/pi-rust/blob/v0.12.31/Cargo.toml) · [NSH 构建清单](https://github.com/longbridge/nsh/blob/v0.6.1/Cargo.toml)

## Pi 进程内直连与本地配置

1. **会话。** worker 在任务所属工作区，用 `SessionManager::open_with` 打开 `<DataDir>/sessions/<threadId>.jsonl`，应用明确的模型与 cwd，再调用 `create_app_runtime`。订阅会话事件、绑定扩展 UI 后调用 `prompt`。后续 turn 和父任务续行复用线程历史。fork、rewind、模型切换对照旧版会话分支与工具结果边界。[会话文件 API](https://github.com/yannelli/pi-rust/blob/v0.12.31/crates/pi-coding-agent/src/session/mod.rs) · [Vantry 分支逻辑](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/adapters/provider/pi/session_seed.go)
2. **流式控制与审批。** 文本、思考、工具起止、用量、错误和终态映射成已有事件。运行中调整指令调用 `steer`，停止调用 `abort`，排队消息由 Core 持久化。实现 `ExtensionUi`，按 attempt、request 和 control ID 完成确认、拒绝、超时、取消与重连。上游 Print/JSON 模式会对对话框返回默认值，必须实际接入交互回调。已有扩展依赖的模式枚举可与原行为兼容，调用路径直接走 Rust API。[控制与 steer](https://github.com/yannelli/pi-rust/blob/v0.12.31/crates/pi-coding-agent/src/agent_session/prompt.rs) · [扩展 UI 接口](https://github.com/yannelli/pi-rust/blob/v0.12.31/crates/pi-coding-agent/src/extension_ui.rs)
3. **模型与工具。** 保留 Magpie 模型选择、用户扩展与 MCP 的独立开关，以及 codemode、tool-search 和工具暴露规则。先实跑原有 `approval.ts` 与 `vantry_tools.ts` 的 ask、plan、编辑授权、委派、私密输入和终端行为。Vantry 自有工具可以用 `AppOptions.custom_tools` 或 `inline_extensions` 注册。工具按原 ID 调 Core，沙箱由执行进程及其子进程继承。第三方扩展按其运行时需求单独验证。[运行时入口](https://github.com/yannelli/pi-rust/blob/v0.12.31/crates/pi-coding-agent/src/app.rs) · [MCP 行为](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/mcp-integration.md)
4. **已有配置直接读，空目录首次保存时生成。** 在执行所属机器上优先解析 `PI_CODING_AGENT_DIR`，默认 `~/.pi/agent`。复用 `auth.json`、`settings.json`、`models.json` 及 MCP 配置，项目 `.pi/` 按 Pi 的信任规则生效。首次保存密钥或 OAuth 登录写入标准 `auth.json`，设置默认模型写入 `settings.json`，自定义端点或模型才写 `models.json`。按上游锁和权限合并，保留其他 provider 和未知字段，单文件原子替换。跨文件先写依赖项、最后切默认模型，失败可安全重试。用原版 Pi CLI 反读新配置，另验远端凭据留在所属机器。[Pi 配置规则](https://github.com/yannelli/pi-rust/blob/v0.12.31/README.md#configuration) · [0.1.42 首次配置](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/adapters/provider/pi/setup.go)
5. **内置版本与升级。** 设置页显示内置版本、模型配置和登录状态，Pi 随 Vantry 更新。0.1.42 本机桌面要求 `provider.pi.runtime.v1`，远端握手不要求。新增明确的内置引擎能力，新桌面按对端实际能力显示设置页。正式升级时配套切换本机 UI 和 Core，避免旧桌面把新 Core 当成缺失依赖而启动另一个实例。[已有握手逻辑](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/app/handshake.go)

## Rust Core 的数据与故障恢复

| 合同 | Rust 实现与验收 |
|---|---|
| 旧库可用 | 保留 `VANTRY_DATA_DIR`、`state.sqlite`、`core.sock` 与实例锁。首先兼容协议 v1 和 SQLite `user_version=11`，包括两种结构不同的历史 v3。按实际表和列迁移，保留 WAL、外键与唯一索引，区分数据库版本与公开状态 schema |
| 命令只提交一次 | 在同一事务写入业务投影、事件、回执和 outbox，提交后再通知订阅者。相同 commandId 和业务载荷返回原回执，冲突载荷拒绝。外部模型、网络及 Git 调用置于事务外 |
| 执行中断可判定 | 发出 prompt 前持久化提交意图。Core 只接受当前 attempt、claim 与连续的 provider sequence，并在同一事务提交输出和游标。保留 worker 存活、worker 已结束、投递状态不明三种处理路径 |
| 父子任务可续行 | 保留 task、cohort、generation、deliveryMessageId 与 continuation 的唯一性。测试父任务等待、子结果重复交付、取消和完成竞态，以及远端结果返回原有父线程 |
| 流式结果可补读 | 旧 observer 每 40 ms 重读整份日志，旧 worker 每条记录同步。Rust 接管两端后，以追加日志、已落盘序号和字节位置驱动增量读取。首段文字、审批、工具副作用意图和终态及时落盘，普通文本按短窗口合并。通知只唤醒读取，重启按游标补，残缺尾记录不进入已提交历史 |
| UI 与工作区一致 | 快照和增量按全局事件序号衔接，游标失效走 `resync_required`。附件、草稿、游标按环境隔离。worktree 核对目录、分支和归属，防止重启后写到主 checkout。新 UI 的虚拟列表以稳定 ID 更新，在真机验证 IME、滚动锚点、附件、Git 分块与终端输入 |

[基线数据库与迁移](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/core/store.go) · [命令事务](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/core/commands.go) · [编排合同](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/orchestration-implementation.md) · [旧日志观察器](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/adapters/provider/pi/observer.go) · [工作区边界](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/workspace-boundaries.md)

## 跨机、远程桌面与发布工具

**网络与媒体是独立移植项。** 用 Rust 实现现有 WebSocket/TLS、配对、直连、T3 会话接口和断线恢复。桌面共享继续使用独立授权。原有 Go 插件的 Host、Viewer、信令、TURN 及 Pion 交互都需要 Rust 实现。先用 `tokio-tungstenite` 和 Rust WebRTC 实现做互操作验证，重点测同网直连、TURN/TCP 经 WSS、H.264 协商、视频帧、键鼠、撤销和重连。用旧桌面或 iOS 客户端连接新服务，再用新桌面连接旧服务。迁移期间已有中继可服务旧用户，交付物包含可独立替代它的 Rust relay。[原中继实现](https://github.com/muq9738/Vantry/blob/v0.1.42/internal/desktoprelay/server.go) · [现有媒体合同](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/remote-desktop.md) · [Rust WebSocket](https://github.com/snapview/tokio-tungstenite) · [Rust WebRTC](https://github.com/webrtc-rs/webrtc)

**发布链有四处需要显式改动。**

| 0.1.42 入口 | 替换方法与检查 |
|---|---|
| `scripts/build_remote_helper.py` 默认执行 `go list -m` | 将插件原生源码固定到可追溯的版本，显式传入源码目录，由 Swift 或 MSVC/CMake 构建。Rust 替换该 Go 模块提供的运行时代码。仅提供一个从 Go 模块缓存复制出的 helper 无法验收干净环境构建 |
| `scripts/make_delta.go`、`scripts/prepare_icon.go` | 用 Rust `xtask` 生成差分包和图标。差分读写与更新器共用代码，测试旧包读入、完整包回退及签名验证。首次跨布局升级发布完整包 |
| MyGo、macOS Bundle 与 Windows ZIP | 将标识 `io.github.muq9738.vantry`、本地化、权限说明、图标与 notices 映射到新包。Windows 现有归档会排除第二个顶层 EXE，建议服务放在 `libexec/vantry-service.exe` 并显式打包。对解压产物和安装版分别运行 Core、worker、更新和 helper |
| 两套 GitHub Actions、烟测、部署文档 | 用 Rust 构建、测试及打包替换 `actions/setup-go`、`go vet/test`、`go tool mygo`，同步改触发路径与版本来源。既有烟测接收 Rust 二进制路径。独立 relay 用原有 systemd 参数演练启动、重启和优雅退出 |

[helper 构建脚本](https://github.com/muq9738/Vantry/blob/v0.1.42/scripts/build_remote_helper.py) · [macOS/Windows 发布流程](https://github.com/muq9738/Vantry/blob/v0.1.42/.github/workflows/release.yml) · [Windows 检查流程](https://github.com/muq9738/Vantry/blob/v0.1.42/.github/workflows/windows.yml) · [更新与降级边界](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/updates.md)

## 实施顺序

| 阶段 | 实际交付 | 退出条件 |
|---|---|---|
| P0　最小实包 | NSH 对话窗口、Rust worker、完整 pi-rust、现有资源和平台 helper。用隔离的 HOME 配置一个流式模型端点 | 安装包可启动，真实执行一次扩展工具，允许和拒绝均生效。记录 ZIP、安装体积、进程树和首段延迟 |
| P1　替换执行链 | 迁移分支上的 Go Core 启动 Rust worker，先沿用现有 job、控制、日志和结果格式 | 真模型、MCP、codemode、队列、steer、取消和委派通过。关闭 UI、强退 Core 后，同一 attempt 的初始 prompt 只投递一次，后续工具轮次正常完成 |
| P2　替换桌面 | NSH 通过现有协议接 Core，迁移会话、Git、终端、插件、设置和跨机视图。可与 P1 同时进行 | 硬件实测中文 IME、长流式输出、滚动、焦点、附件及重连，核对 0.1.42 的公共 CLI 入口 |
| P3　替换 Core | 在一致性数据副本上先对照读取与命令回放，再启用 Rust 调度、父子任务、恢复和持久订阅 | 旧 schema 及 v11 升级通过。比较回执、事件和投影，只映射非确定性的时间与生成 ID。分别强退 Core 和 worker，核实模型请求、工具执行和父任务续行次数 |
| P4　替换所有外围 Go 组件 | Rust 独立中继、RTC 与插件宿主、平台层、更新器、差分和图标工具全部进入发布链 | 新旧客户端跨机互通，Rust 中继可部署，正式包完成远程桌面与升级演练，三类发行物有明确的构建来源 |
| P5　移除 Go 并切换发行 | 对照完成后删除已移植的 Go 源码与测试，移除 Go/MyGo 模块和构建配置。历史基准留在标签 | 下节完成条件通过。日常开发、CI、用户安装和中继部署均独立于 Go |

切换前先通过已有空闲关停接口处理活动任务、排队输入、审批与待交付结果。测试或影子实例各用独立数据目录。用 SQLite Backup API 或停机后的一致性备份保存旧库，同时保留 sessions、jobs 和设备身份。先在副本验证“Rust 写入后旧版本仍能打开”，再交接生产目录唯一写锁。回退时保全切换后的新记录，旧应用与旧数据应能成套恢复。[基线升级流程](https://github.com/muq9738/Vantry/blob/v0.1.42/docs/updates.md)

## 完成条件

- **源码和依赖。** 发布提交中 `git ls-files -- '*.go'` 返回空，所有 Go 模块/工作区清单和 MyGo 打包入口均已移除。构建配置、Cargo build script、脚本和工作流不再调用 Go 或访问 Go 模块缓存。基线文档可保留历史说明。
- **可重复构建。** 在没有 Go、GOPATH 缓存和预置 Go 产物的干净构建环境，运行 `cargo test --workspace --locked`、`cargo build --workspace --release --locked`，再使用拟建的 `cargo run --locked -p xtask -- package` 打出各目标包。对归档内每个可执行、库和 helper 核对构建来源，覆盖静态链接和预编译 sidecar。
- **独立运行。** 在没有 Go 和外部 Pi CLI 的机器安装正式包，完成首次配置、模型请求、审批、关闭桌面、恢复、旧库打开与升级。独立部署 Rust 中继完成双机连接和撤销。自定义用户工具的运行依赖按其原有配置提供。
- **回归。** 241 个旧测试文件按行为映射到 Rust 单元、集成或平台端到端测试。覆盖 go build tags 下的平台差异、交付竞态、断线与重复输入。常规发布验收独立运行，旧 Go 基准只用于追溯差异。
- **平台。** macOS arm64 首发在正式签名包上验收权限与媒体。NSH 后端要求 macOS 14+，0.1.42 支持的 macOS 13 继续使用旧发行线。Windows 在真机验证 Job、ACL、输入、GPU 和更新。Linux 无界面服务单独验证，Linux 桌面按 GTK 4.14+ 验收。[NSH 平台矩阵](https://github.com/longbridge/nsh/blob/v0.6.1/README.md)

## 体积和性能预算

以下为此前在同一台 macOS arm64、相同应用资源和打包参数下得到的**混合试包测量**。全 Rust 发行物须在 P0 和 P5 各测一次。

| 比较对象 | 下载 ZIP | 安装后应用 |
|---|---:|---:|
| 原版 Vantry 0.1.42 | 18.87 MB | 42.02 MB |
| 0.1.42 加 Rust 轻量库 `pi-embedded` | 22.79 MB | 50.48 MB |
| 0.1.42 加 PiG | 35.91 MB | 78.47 MB |
| 0.1.42 加 Rust 完整会话库 | 36.31 MB | 80.62 MB |

完整会话库已经用本地模拟端点验证了 Pi 配置和一个 TypeScript 工具。轻量库的内置流式通道只实现 `openai-completions`，认证、网络与存储需宿主提供，因此选完整会话库。如果以 **25 MB ZIP** 作为目标，混合试包与该目标相差 **11.31 MB**。P0 用真实 NSH 包逐项记录桌面、服务、helper、资源和许可证占比，P5 用功能齐全的包给出最终结果。[轻量库边界](https://github.com/yannelli/pi-rust/blob/v0.12.31/crates/pi-embedded/src/openai.rs)

性能基准使用同机、同工作区和相同节奏的本地流式端点，分别记录进程启动到可输入、命令提交到模型端点收到请求、首个 SSE 到桌面首段呈现的 P50/P95。对长会话比较日志读取字节、同步次数、CPU、整组进程内存和并发 attempt 开销，再补一次真实模型端到端测量。目标是日志读取量随新增输出增长，同时保证已提交内容、审批和恢复行为完整。
