<p align="center">
  <img src="resources/icon.png" width="88" alt="Vantry icon" />
</p>

<h1 align="center">Vantry</h1>

<p align="center">
  A native desktop workspace for AI coding and multi-agent collaboration.<br />
  Keep your conversations, agents, Git changes, and remote machines together.
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-237a57?style=flat-square" alt="License: MIT" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.27.3%2B-007d9c?style=flat-square" alt="Go 1.27.3 or later" /></a>
  <a href="https://github.com/muq9738/Vantry/releases/latest"><img src="https://img.shields.io/badge/download-releases-25292e?style=flat-square" alt="Download releases" /></a>
</p>

<p align="center">
  <strong>English</strong> · <a href="README.zh-CN.md">简体中文</a> · <a href="README.zh-TW.md">繁體中文</a>
</p>

<p align="center">
  <a href="#get-started">Get started</a> ·
  <a href="#build-and-develop">Development</a> ·
  <a href="#documentation">Documentation</a> ·
  <a href="#license">License</a>
</p>

---

Vantry pairs a **Go + MyGo native interface** with a persistent **Core** and **Pi-powered execution**. Conversations and tasks belong to their Core; closing a window leaves the work running. Use your existing Pi models and credentials, work locally or on a linked machine, and review changes in the same workspace.

## What you can do

| Area | Capabilities |
| --- | --- |
| **Coding workspace** | Streaming conversations, model selection, multiline input, attachments, skill selection, and inline thread references. |
| **Agent collaboration** | Persistent parent and child tasks, separate Git worktrees, live status, result delivery, and follow-up conversations. |
| **Git and files** | File browsing, diffs, staging, commits, branches, and a graph-based log with filters and commit actions. |
| **Multiple machines** | Conversations grouped by machine and project, remote execution, pairing codes, and optional self-hosted relays. |
| **Execution controls** | Tool approvals, queued messages, live steering, persistent events, and recovery after a Core restart. |
| **Plugins** | Native pages, explicit process-based integrations, and the remote-desktop plugin. |

The interface supports **English, Simplified Chinese, and Japanese**.

### Remote-desktop plugin

Remote desktop is provided by a plugin, with native H.264 streaming and keyboard and mouse control over LAN or your own relay. Enable sharing when needed; each connection requires confirmation on the sharing computer. On macOS, sharing requires Screen Recording and Accessibility permissions. No relay address is preset.

## Get started

### Download

Get a package from [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest).

| Platform | Availability |
| --- | --- |
| **macOS · Apple Silicon** | macOS 13 or later. Unzip the package and move `Vantry.app` to `/Applications`. Current releases use a local signing identity and are not Apple-notarized. |
| **Windows · x64** | Preview ZIP. The current release is cross-compiled, has not been validated on Windows hardware, and does not include the native remote-desktop helper. |
| **Linux** | No validated desktop release yet. |

### Set up your workspace

1. Install and configure **Pi CLI** on the computer that will run AI tasks. Vantry uses its model configuration and credentials; Pi is not bundled with the app.
2. Open Vantry, choose a project folder, and select a model. Use the folder directly or create a separate worktree.
3. Start a conversation. Review tool activity, approve requests when required, and inspect changes in the Git panel.

For remote work, pair another Core from the device controls. To share a screen, use the remote-desktop plugin and its separate sharing code.

Updates are downloaded and verified in the background (with optional binary delta patches), then applied on restart once Core is idle. See [Updates](docs/updates.md) for details.

## Build and develop

### Requirements

- **Go 1.27.3 or later** and Git.
- **Pi CLI** (`@earendil-works/pi-coding-agent`) with a configured model provider for real AI execution.
- **macOS and Xcode Command Line Tools**, plus Python 3, to build the macOS native remote-desktop helper.
- The bundled remote-desktop plugin ([vantry-plugin-remote-desktop](https://github.com/muq9738/vantry-plugin-remote-desktop)) is fetched automatically as a Go module. See [Plugin development](docs/plugin-repositories.md) for helper build details.

### Run from source

```sh
git clone https://github.com/muq9738/Vantry.git
cd Vantry

go mod download
go build -o bin/vantry ./cmd/vantry

./bin/vantry desktop --workspace "$PWD"
```

### Package for macOS

Build the matching native helper, then package the application:

```sh
python3 scripts/build_remote_helper.py --platform darwin/arm64
go tool mygo build -platform darwin/arm64 -skip-dmg
```

The app is written to `dist/darwin-arm64/Vantry.app`. See [macOS packaging](docs/packaging-macos.md) for signing and [Windows development](docs/windows-remote-desktop.md) for the Windows SDK and helper requirements.

### Check your changes

```sh
go test ./... -timeout=120s
go vet ./...
```

Some hardware and integration checks are opt-in. A successful cross-build does not establish runtime support on that platform.

## How it works

| Component | Responsibility |
| --- | --- |
| **Desktop** | Native MyGo interface, conversation timeline, composer, file browser, Git, and plugin views. |
| **Core** | SQLite-backed conversations, task orchestration, approvals, workspaces, pairing, and event delivery. |
| **Pi workers** | Model execution and tools on the machine that owns the task. |
| **Connections** | Direct TLS or configured relays for sessions; separate authorization for desktop sharing. |

Start with `cmd/vantry` for the application entry point, `internal/desktop` for the UI, `internal/core` and `internal/app` for task behavior, and `pkg/api` for shared contracts. Native UI rendering does not require an HTML frontend.

## Documentation

Most engineering guides are currently in Chinese.

| Guide | Topic |
| --- | --- |
| [Local development](docs/getting-started.md) | Core lifecycle, execution settings, and CLI examples. |
| [Desktop UI](docs/desktop-ui.md) | Native Stardust interface, keyboard shortcuts, and panels. |
| [Providers & Pi](docs/providers.md) | External Pi CLI discovery, configuration, and safe upgrades. |
| [Agent orchestration](docs/orchestration-implementation.md) | Parent and child tasks, delivery, and recovery. |
| [Remote connections](docs/native-connectivity.md) | Pairing, remote sessions, desktop sharing, and relay setup. |
| [Remote desktop](docs/remote-desktop.md) | LAN-first WebRTC sharing, lazy relay fallback, and macOS permissions. |
| [Updates](docs/updates.md) | Staged installation, delta updates, and optional private-fork tokens. |
| [Behavior and limits](docs/behavior-gaps.md) | Implementation boundaries and remaining work. |

## Acknowledgments

Vantry is an independent Go + MyGo implementation inspired by and compatible with great open-source projects in the ecosystem:

- **[T3 Code](https://github.com/pingdotgg/t3code)** — inspiration for orchestration workflows and protocol 2 session compatibility.
- **[MyGo](https://github.com/egoist/mygo)** — native desktop UI framework.
- **[Ghostty](https://github.com/ghostty-org/ghostty)** — native terminal emulation (`libghostty-vt`).
- **[Pi CLI](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)** — external AI coding agent runtime.
- **[Pion](https://github.com/pion/webrtc)**, **[Mermaid](https://mermaid.js.org)**, **[KaTeX](https://katex.org)**, and **[Lucide](https://lucide.dev)** — WebRTC/TURN transport, offline diagrams, math rendering, and icons.

## License

Vantry's original source code and documentation are available under the **[MIT License](LICENSE)**. You may use, modify, redistribute, and use them commercially, provided you retain the copyright and license notice. The software is provided without warranty.

Dependencies and separately maintained plugins retain their own licenses and access requirements. See [Third-party notices](THIRD_PARTY_NOTICES.md) and the license copies in [`third_party/licenses`](third_party/licenses).
