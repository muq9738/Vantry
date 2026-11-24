# vantry-desktop（NSH 原生桌面）

Wave 2 (P2) 起对接真实 `vantry-client` / `vantry-protocol` 的 NSH 桌面工作台；Wave 4B 接入 Core 工作台（`POST /v1/workbench`）、服务模式转发与本地 Core 自动启动。只依赖 NSH、`vantry-protocol`、`vantry-client`（以及已在清单中的通用库），不链接 `vantry-core`、`vantry-pi-runtime` 或 `pi-coding-agent`（`manifest_guard` 测试核对清单）。

## 模块

| 路径 | 职责 |
|---|---|
| `src/main.rs` | 解析 0.1.42 兼容命令行，启动 NSH 原生窗口（macOS AppKit / Linux GTK / Windows Win32）；`--peer` 也打开窗口 |
| `src/cli.rs` | `vantry [desktop] [选项]`：兼容 Go `flag`；`core`、`ctl`、`provider-worker`、`relay`、`rd-relay`、`apply-update` 连同原始参数转交服务程序；`--socket`、`--mock`；`Endpoint` 路由（本机 / 演示 / 配对主机）与 `desktop-state.json` 路径 |
| `src/service.rs` | 查找 `vantry-service` / `vantry-relay`（本程序目录 → `libexec` → `PATH`，跳过自身）；Unix 以 `exec` 透明替换进程（参数、信号、退出码不变），其他平台等待子进程并转交退出码；找不到时给出中文提示（退出码 127）；生成 `ensure-core` 参数（数据目录、非默认 socket、provider、model、`--controlled-delay` 等 Core 参数、工作区） |
| `src/autostart.rs` | 本机目标且非 `--mock` / `--attach-only` 时：`/v1/health` 不可达 → 运行 `vantry-service ensure-core …`（60 s 超时、`kill_on_drop`）→ 轮询 health 至就绪（20 s），连接层显示「正在启动本地 Core…」；失败时保留错误提示并继续重连 |
| `src/bridge.rs` | `CoreBridge` trait、错误分类（`ClientError` 映射）、事件类型名、`connect(Endpoint)`、`RemoteBridge`（远端传输未接入时返回 `not_ready`，窗口保持重试） |
| `src/bridge/client.rs` | `ClientBridge`：真实 `vantry_client::CoreClient`（Unix socket 上的协议 v1） |
| `src/bridge/mock.rs` | 内存 Core：流式 Markdown 回复、工具与审批、停止；排队 / 编辑 / 重排 / 转追加 / 取消；重命名、置顶、延后、归档、删除、分叉、回退、合并回原会话；演示数据含父子任务树 |
| `src/bridge/workbench.rs`、`workbench/` | `WorkbenchBridge`：文件树、搜索与只读预览、Git（状态、差异、暂存、撤销、切换分支、提交、PR 摘要）、终端会话、设置目录、附件，以及通用工作台动作 `workbench(request)` |
| `workbench/core_bench.rs` | `CoreWorkbench`：设置、MCP、插件、远端配对、远程桌面、更新、用量、环境、项目、附件走 Core；文件 / Git / 终端走本机目录。动作未知时先用 0.1.42 名称重试（`access.*`、`rd.host_*`、`plugins.page` …），工作台未挂载、端点未知或 Core 不可达时回退 `LocalWorkbench`，设置页标注未提供的部分 |
| `workbench/actions.rs` | 动作名、请求构造、宽松读取 `output` JSON（沙盒、用量、远端状态、远程桌面、插件面板 schema、环境、项目）、思考档位（`model:level`） |
| `workbench/local.rs`、`workbench/mock.rs` | `LocalWorkbench`（本机文件、`git`、shell 子进程、无 Core 时可答的动作）与 `MockWorkbench`（演示工作区，内存实现全部设置动作） |
| `src/state.rs`、`src/state/` | 全部状态迁移（无 I/O，可单测），见下 |
| `src/controller.rs` | 三个全局信号（会话 / 窗口 / 工具坞）、连接循环、`desktop-state.json` 去抖原子保存、主题、所有视图意图 |
| `src/theme.rs` | Stardust 浅色/深色语义色与排版尺寸 |
| `src/views/` | `sidebar`、`header`、`timeline`、`markdown`、`composer`（思考档位、审批模式、Steer / Queue、Esc 停止）、`dock`（文件搜索、分支切换、撤销确认）、`settings`（8 页实接）、`plugin_panel`（插件原生面板）、`remote_desktop`（会话控制栏）、`dialogs`、`approval_modal`（工具审批 / 私密掩码输入 / 交互终端） |

### 状态（`src/state/`）

| 文件 | 内容 |
|---|---|
| `state.rs` | `AppState`：协议实体折叠（线程、运行、工具、请求、输入、任务、`thread.rewound`）、发送 / 追加 / 排队 / 停止 / 审批、命令回执与重试 |
| `actions.rs` | 会话管理（乐观更新、拒绝时只恢复该字段）、队列（编辑、重排、转追加、取消）、附件、模型与权限 |
| `timeline.rs` | `TimelineIndex`：按运行增量维护行；输出增量只延长最后一行并只重解析其末段 Markdown；行 id 变化才递增 `structure` |
| `markdown.rs` | 块级（标题、段落、有序/无序/任务列表、代码块+语言、Diff、引用、分割线、表格）与行内（粗体、斜体、删除线、行内代码、链接）解析；`update` 只保留已定型块，从首个未定型块重解析 |
| `sidebar.rs` | 搜索与状态筛选、置顶 / 会话 / 延后 / 归档分组、父子委派树与任务徽章、折叠 |
| `workbench.rs` | 工具坞：文件树、Git 面板、终端会话（历史、输出上限）、预览 |
| `ui.rs` | 主题、设置页与目录、设置操作（忙碌键、结果反馈、MCP / 配对 / Pi 表单；密钥用 `Secret`，`Debug` 不输出内容）、插件面板（加载、动作、表单值、输出日志）、模型选择器、远程桌面会话、重命名与确认对话框 |
| `persist.rs` | `desktop-state.json` 读写（同目录临时文件 + fsync + rename，Unix 0600/0700）；不保存未确认命令 |

## 与 Core 对接

```rust
pub trait CoreBridge: Send + Sync + 'static {
    fn label(&self) -> String;
    fn health(&self) -> BridgeFuture<Health>;                       // GET  /v1/health
    fn snapshot(&self) -> BridgeFuture<Snapshot>;                   // GET  /v1/snapshot
    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt>;  // POST /v1/commands
    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()>; // GET /v1/events?after=
}
```

类型均为 `vantry_protocol` 的。错误映射：`ClientError::ResyncRequired` → `ResyncRequired`（重新加载快照）；`Api{code}` 保留错误码；`Transport` → `Unavailable`。`Unavailable` / `Protocol` / `Internal` / 未知码视为结果未知：保留原命令，重试沿用同一 `commandId` 与内容，不自动重发。

## 运行与测试

```sh
export PATH="$HOME/.cargo/bin:$PATH" CARGO_BUILD_JOBS=4
cargo check --locked -p vantry-desktop
cargo test --locked -p vantry-desktop
# 窗口：vantry --mock（演示 Core + 演示工作区）或 vantry --data-dir <目录>（连接已运行的 Core）
```

服务端合同（由 `vantry-service` 实现）：`vantry-service ensure-core --data-dir <目录> [--socket …] --provider … [--model …] [--controlled-delay …] [--pi-executable …] [--magpie-settings …] [--workspace-kind … --workspace … --repository … --branch … --base-ref …]`，Core 已运行或启动完成后以 0 退出；工作台动作名见 `workbench/actions.rs::names`。

尚未覆盖：远端传输（配对主机直连 / 中继，`--peer` 使用 `RemoteBridge`）、远程桌面媒体帧（画布容器 `vantry-remote-desktop-canvas`，会话状态已接 `rd.*`），以及真机 IME、长流式滚动、焦点、键盘 Esc 与掩码输入的人工验收。
