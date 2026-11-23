# vantry-desktop（NSH 原生桌面）

Wave 2 (P2)：对接真实 `vantry-client` / `vantry-protocol` 的 NSH 桌面工作台。只依赖 NSH、`vantry-protocol`、`vantry-client`（以及已在清单中的通用库），不链接 Core 或 Pi。

## 模块

| 路径 | 职责 |
|---|---|
| `src/main.rs` | 解析 0.1.42 兼容命令行，启动 NSH 原生窗口（macOS AppKit / Linux GTK / Windows Win32）；`--remote` / `--peer` 也打开窗口 |
| `src/cli.rs` | `vantry [desktop] [选项]`：兼容 Go `flag`；`core`、`ctl` 等模式归 `vantry-service`；`--socket`、`--mock`；`Endpoint` 路由（本机 / 演示 / SSH / 配对主机）与 `desktop-state.json` 路径 |
| `src/bridge.rs` | `CoreBridge` trait、错误分类（`ClientError` 映射）、事件类型名、`connect(Endpoint)`、`RemoteBridge`（远端传输未接入时返回 `not_ready`，窗口保持重试） |
| `src/bridge/client.rs` | `ClientBridge`：真实 `vantry_client::CoreClient`（Unix socket 上的协议 v1） |
| `src/bridge/mock.rs` | 内存 Core：流式 Markdown 回复、工具与审批、停止；排队 / 编辑 / 重排 / 转追加 / 取消；重命名、置顶、延后、归档、删除、分叉、回退、合并回原会话；演示数据含父子任务树 |
| `src/bridge/workbench.rs`、`workbench/` | `WorkbenchBridge`：文件树与只读预览、Git（状态、差异、暂存、提交、PR 摘要）、终端会话、设置目录、附件；`LocalWorkbench`（本机文件、`git`、shell 子进程）与 `MockWorkbench` |
| `src/state.rs`、`src/state/` | 全部状态迁移（无 I/O，可单测），见下 |
| `src/controller.rs` | 三个全局信号（会话 / 窗口 / 工具坞）、连接循环、`desktop-state.json` 去抖原子保存、主题、所有视图意图 |
| `src/theme.rs` | Stardust 浅色/深色语义色与排版尺寸 |
| `src/views/` | `sidebar`、`header`、`timeline`、`markdown`、`composer`、`dock`、`settings`、`remote_desktop`、`dialogs`、`approval_modal` |

### 状态（`src/state/`）

| 文件 | 内容 |
|---|---|
| `state.rs` | `AppState`：协议实体折叠（线程、运行、工具、请求、输入、任务、`thread.rewound`）、发送 / 追加 / 排队 / 停止 / 审批、命令回执与重试 |
| `actions.rs` | 会话管理（乐观更新、拒绝时只恢复该字段）、队列（编辑、重排、转追加、取消）、附件、模型与权限 |
| `timeline.rs` | `TimelineIndex`：按运行增量维护行；输出增量只延长最后一行并只重解析其末段 Markdown；行 id 变化才递增 `structure` |
| `markdown.rs` | 块级（标题、段落、有序/无序/任务列表、代码块+语言、Diff、引用、分割线、表格）与行内（粗体、斜体、删除线、行内代码、链接）解析；`update` 只保留已定型块，从首个未定型块重解析 |
| `sidebar.rs` | 搜索与状态筛选、置顶 / 会话 / 延后 / 归档分组、父子委派树与任务徽章、折叠 |
| `workbench.rs` | 工具坞：文件树、Git 面板、终端会话（历史、输出上限）、预览 |
| `ui.rs` | 主题、设置页与目录、模型选择器、远程桌面视图状态、重命名与确认对话框 |
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

尚未覆盖：自动启动 Core（`EnsureCore`）、远端传输（SSH 转发、配对、中继）、远程桌面媒体流（画布容器已预留 `vantry-remote-desktop-canvas`）、插件原生面板渲染、附件内容上传到 Core（当前只生成附件引用），以及真机 IME、长流式滚动、焦点的人工验收。
