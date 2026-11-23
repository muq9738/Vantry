# vantry-desktop（NSH 原生桌面）

Wave 1 (P0)：最小可用 NSH 对话桌面。只依赖 NSH、`vantry-protocol`、`vantry-client`。

## 模块

| 路径 | 职责 |
|---|---|
| `src/main.rs` | 解析 0.1.42 兼容命令行，启动 NSH 原生窗口（macOS AppKit / Linux GTK / Windows Win32） |
| `src/cli.rs` | `vantry [desktop] [选项]`：兼容 Go `flag` 的 `-name` / `--name=value`；`core`、`ctl` 等模式归 `vantry-service`；新增 `--socket`、`--mock` |
| `src/bridge.rs` | `CoreBridge` trait、错误码、`connect(Endpoint)` |
| `src/bridge/wire.rs` | 协议 v1 DTO（与 Go `pkg/api` 逐字段对齐），`vantry-protocol` 提供 DTO 后改为再导出 |
| `src/bridge/unix_http.rs` | `<数据目录>/core.sock` 上的 HTTP/1.1：`/v1/health`、`/v1/snapshot`、`/v1/commands`、`/v1/events?after=`（NDJSON） |
| `src/bridge/mock.rs` | 内存 Core：`--mock` 与测试；流式回复、工具、审批、停止、幂等 commandId、`resync_required` |
| `src/state.rs`、`src/state/` | `AppState` 及全部状态迁移（无 I/O，可单测）：事件折叠、时间线、审批、发送/追加/排队/停止、断线恢复 |
| `src/controller.rs` | 全局信号、连接循环（快照 → 订阅 → 退避重连 / 重新同步）、视图调用的意图 |
| `src/theme.rs` | Stardust 浅色/深色语义色（shadcn 令牌 + `PALETTE`）与排版尺寸 |
| `src/views/` | `sidebar`、`header`、`timeline`、`composer`、`approval_modal` |

## 与 `vantry-client` 对接

桌面只通过 `Arc<dyn CoreBridge>` 访问 Core：

```rust
pub trait CoreBridge: Send + Sync + 'static {
    fn label(&self) -> String;
    fn health(&self) -> BridgeFuture<Health>;                       // GET  /v1/health
    fn snapshot(&self) -> BridgeFuture<Snapshot>;                   // GET  /v1/snapshot
    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt>;  // POST /v1/commands
    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()>; // GET /v1/events?after=
}
```

`vantry-client` 实现该 trait（或 trait 移入 client）后，只需在 `bridge::connect` 中构造它；`wire` 改为再导出 `vantry-protocol` 的类型。错误需映射到 `ErrorCode`：`ResyncRequired` 触发重新加载快照，`Unavailable`/`Protocol`/`Internal` 视为结果未知（同一命令原样重试，不生成新 commandId）。

## 运行与测试

```sh
export PATH="$HOME/.cargo/bin:$PATH" CARGO_BUILD_JOBS=4
cargo check -p vantry-desktop
cargo test -p vantry-desktop
# 窗口：vantry --mock（演示 Core）或 vantry --data-dir <目录>（连接已运行的 Core）
```

尚未覆盖（Wave 2）：自动启动 Core（`EnsureCore`）、远程环境、右侧面板与工具坞、附件、Markdown 渲染、设置、`desktop-state.json` 持久化，以及真机 IME/滚动的人工验收。
