<p align="center">
  <img src="resources/icon.png" width="88" alt="Vantry icon" />
</p>

<h1 align="center">Vantry</h1>

<p align="center">
  A native Rust workspace for coding with AI agents.<br />
  Conversations, multi-agent tasks, terminal, Git diffs, and paired machines in one window.
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-237a57?style=flat-square" alt="License: MIT" /></a>
  <a href="Cargo.toml"><img src="https://img.shields.io/badge/Rust-1.97%2B-c6531a?style=flat-square" alt="Rust 1.97 or later" /></a>
  <a href="https://github.com/muq9738/Vantry/releases/latest"><img src="https://img.shields.io/github/v/release/muq9738/Vantry?style=flat-square&label=release&color=25292e" alt="Latest release" /></a>
</p>

<p align="center">
  <strong>English</strong> · <a href="README.zh-CN.md">简体中文</a> · <a href="README.zh-TW.md">繁體中文</a>
</p>

<p align="center">
  <a href="#features">Features</a> ·
  <a href="#install">Install</a> ·
  <a href="#architecture">Architecture</a> ·
  <a href="#build-from-source">Build from source</a> ·
  <a href="#documentation">Documentation</a> ·
  <a href="#license">License</a>
</p>

---

Vantry pairs a pure-Rust **[NSH](https://github.com/longbridge/nsh) native desktop** with a persistent SQLite **Core** and an embedded **[Pi](https://github.com/yannelli/pi-rust)** runtime — no Electron, no WebView, and no external Pi CLI required. Conversations and tasks belong to their owning Core rather than a window: close the desktop anytime while agents keep running, reconnect from another machine, and review every change in the same workspace.

## Features

| Area | Highlights |
| --- | --- |
| **Native workbench** | GPU-rendered Stardust UI (light & dark) with streaming Markdown, tables, diffs, charts, inline `@` file/image/thread references (up to 32 attachments with instant preview), model & thinking-level controls, and a workspace skill picker. |
| **Durable execution** | Queue follow-ups or live-steer running agents (edit, reorder, promote to steer, cancel), configure tool approval policies (`auto`, `auto-accept-edits`, `ask`, `plan`), handle model-only secret prompts, and recover cleanly after window closes or Core restarts. |
| **Multi-agent tasks** | Delegate persistent child tasks with isolated Git worktrees, inherited permission bounds, live subtask progress in the session overview, automatic or manual result delivery back to the parent thread, follow-up turns, and cascading cancellation. |
| **Files, Git & terminal** | Right-side workbench dock with a file tree, search and read-only preview, Git status/diffs/staging/branching/commits/PR summaries, and an integrated terminal. |
| **Multi-machine work** | Work across paired Vantry computers over LAN or an Alma relay. Share a conversation with Alma iOS over LAN or an Alma-compatible HTTPS relay. |
| **MCP & plugins** | Native Pi MCP support (`~/.pi/agent/mcp.json` and project `.pi/mcp.json`) with Codemode/tool-search discovery, explicit project trust, and on-demand connection probing; declarative native plugin panels and separate-session remote desktop. |

The desktop interface supports **English**, **简体中文**, and **日本語**.

## Install

Download a prebuilt package from [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest).

| Platform | Package & notes |
| --- | --- |
| **macOS · Apple Silicon** | Requires macOS 14 (Sonoma) or later. Open `Vantry-<version>-macos-arm64.dmg` and drag `Vantry.app` into **Applications**. A standalone `vantry-service` archive is also published for headless hosts. |
| **Windows · x64** | Extract the portable `vantry-<version>-windows-amd64.zip` and run `Vantry.exe`. A standalone `vantry-service` archive is also available. |
| **Linux** | Build from source. `vantry-service` and `vantry-relay` run headless; the NSH desktop targets GTK 4.14+. No prebuilt desktop package yet. |

### Quick start

1. **Configure a model provider** — Open **Settings → Providers** to add an API key or custom endpoint, or reuse an existing Pi configuration in `~/.pi/agent` (`PI_CODING_AGENT_DIR`). Because Pi is embedded in Vantry, no external Node.js or Pi CLI installation is needed.
2. **Open a project** — Pick a local directory (or create an isolated Git worktree) and choose your model and thinking level.
3. **Start a conversation** — Review grouped tool calls, approve sensitive actions when prompted, queue or steer instructions mid-run, and inspect diffs in the Git dock.

Updates download and verify in the background (with optional binary `.delta` patches) and apply on restart once Core is idle. On launch, the desktop also replaces an idle outdated local Core automatically while leaving active runs untouched.

## Multi-machine and remote desktop

- **Paired Core sessions** — Open **Settings → Environments** to pair two computers using the host’s invitation link over LAN or an Alma relay. The sidebar groups conversations by machine. To use a phone outside the LAN, share the conversation with Alma iOS and enter the public URL of your Alma relay.
- **Remote desktop** — Start screen sharing on the host and open its LAN invitation on the other computer. On macOS, screen capture and keyboard or mouse input require **Screen Recording** and **Accessibility** permissions.

## Architecture

Vantry separates UI rendering, durable state management, and agent execution across distinct processes so closing a window never interrupts running work and Core restarts never duplicate prompts:

```text
NSH Desktop (vantry) / CLI / Paired Clients
       │  Local IPC (Unix socket / Windows) · Remote mTLS / Relay
       ▼
Core (vantry-service core) ── SQLite (state.sqlite: events, receipts, tasks, outbox)
       │  Spawns & reattaches durable attempts
       ▼
Worker (vantry-service provider-worker) ── embedded pi-rust ── Model Providers & MCP
```

### Workspace layout

| Path | Role |
| --- | --- |
| `apps/desktop` (`vantry`) | NSH native desktop UI and CLI entrypoint (forwards service subcommands). |
| `apps/service` (`vantry-service`) | Persistent Core daemon, durable Pi provider worker, `ctl`, `peer-proxy`, and updater. |
| `apps/relay` (`vantry-relay`) | Standalone self-hosted session & desktop relay (no UI or Pi dependencies). |
| `crates/core` | Transactional SQLite store, command idempotency, task scheduler, outbox, and event stream. |
| `crates/pi-runtime` | Embedded `pi-rust` session host, model catalog, MCP bridge, approvals, and job journal. |
| `crates/protocol`, `crates/client` | Shared v1 protocol types and compressed HTTP/NDJSON client with cursor resync. |
| `crates/connectivity` | Device identity, native TLS pairing over LAN or an Alma relay, LAN desktop transport and encrypted Alma mobile connections. |
| `crates/platform` | Workspace/worktree boundaries, Git adapter, PTY, plugin host, and OS integration. |
| `crates/updater`, `tools/xtask` | Binary delta/full updater and release packaging (`package`, `make-delta`, `prepare-icon`). |

## Build from source

### Requirements

- **Rust 1.97 or later** (Edition 2024), **Git**, and your platform's native toolchain (Xcode Command Line Tools on macOS; MSVC on Windows; GTK 4.14+ development libraries for Linux desktop).

### Build and run

```sh
git clone https://github.com/muq9738/Vantry.git
cd Vantry

cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
```

Useful launch modes:

```sh
# Preview the UI with an in-memory demo Core (no model provider required)
target/debug/vantry desktop --mock

# Attach to an already-running Core without auto-starting one
target/debug/vantry desktop --attach-only

# Open a paired remote machine directly (auto | direct | relay)
target/debug/vantry desktop --peer <PEER_ID> --peer-mode auto

# Run a headless Core daemon
target/debug/vantry-service core --data-dir /path/to/vantry-data
```

### Test and package

```sh
cargo test --workspace --locked
cargo run --locked -p xtask -- package desktop
```

- On Apple Silicon macOS, desktop artifacts (`Vantry.app`, `.dmg`, and update `.zip`) are written to `target/package-macos-arm64`.
- On other platforms, output is written to `target/package-<product>-<os>-<arch>` (use `--out-dir <DIR>` to override). `xtask package` also supports `service` and `relay`.

## Documentation

Engineering guides are primarily written in Chinese. Several design docs predate the pure-Rust migration and still document the underlying protocol and behavioral contracts.

| Guide | Topic |
| --- | --- |
| [Getting started](docs/getting-started.md) | Source launch flags, mock/attach modes, and local packaging. |
| [Rust architecture plan](docs/nsh-core-pi-rust-plan.md) | Process boundaries, embedded `pi-rust` integration, and recovery contracts. |
| [Desktop UI](docs/desktop-ui.md) | Stardust visual language, composer behavior, and workbench panels. |
| [Task orchestration](docs/task-orchestration.md) | Parent/child delegation, isolation, and continuation rules. |
| [MCP integration](docs/mcp-integration.md) | Codemode-first MCP discovery, project trust, and probing. |
| [Remote connections](docs/native-connectivity.md) | Pair Vantry computers and connect Alma iOS over LAN or an Alma relay. |
| [Remote desktop](docs/remote-desktop.md) | LAN screen sharing and macOS permissions. |
| [Updates](docs/updates.md) | Staged verification, `.delta` patches, and idle Core handoff. |
| [Release notes](docs/releases/) | Version-by-version changes and release downloads. |

## Acknowledgments

Vantry builds on and draws inspiration from great open-source projects:

- **[NSH](https://github.com/longbridge/nsh)** — Pure-Rust native GPU desktop UI framework.
- **[pi-rust](https://github.com/yannelli/pi-rust)** — Embedded `pi-coding-agent` session runtime and extension host.
- **[T3 Code](https://github.com/pingdotgg/t3code)** — Inspiration for orchestration workflows.
- **[Lucide](https://lucide.dev)**, **[Mermaid](https://mermaid.js.org)**, and **[KaTeX](https://katex.org)** — Iconography, diagrams, and math rendering.

## License

Vantry's original source code and documentation are licensed under the **[MIT License](LICENSE)**. Dependencies and separately maintained plugins retain their own terms; see [Third-party notices](THIRD_PARTY_NOTICES.md) and [`third_party/licenses`](third_party/licenses).
