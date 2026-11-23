# Third-party notices

Vantry's original code and documentation are licensed under the [MIT License](LICENSE). Dependencies and independently maintained plugins retain their own terms; Vantry's license does not replace them.

The Go dependencies are recorded in go.mod/go.sum. Remote networking and plugin navigation run natively without a bundled JavaScript runtime. The native terminal bundles libghostty-vt through MyGo. The notices below cover dependencies, orchestration components, and protocol compatibility. They do not imply product affiliation or endorsement. Original copyright and license texts are preserved.

| Project | Use | License |
|---|---|---|
| [T3 Code](https://github.com/pingdotgg/t3code) | Orchestration components and protocol 2 session compatibility over HTTP, OAuth and WebSocket | [MIT](third_party/licenses/orchestration.LICENSE) |
| [MyGo](https://github.com/egoist/mygo) | Native UI framework `v0.8.3` | [MIT](third_party/licenses/mygo.LICENSE) |
| [Lucide](https://lucide.dev) | Native icon geometry from `lucide-static@0.564.0` in `internal/desktop/lucide_icons.go` | [ISC](third_party/licenses/lucide.LICENSE) |
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | SQLite driver dependency `v1.44.3` | [BSD-3-Clause](third_party/licenses/modernc-sqlite.LICENSE) |
| [Gorilla WebSocket](https://github.com/gorilla/websocket) | Native WebSocket control and data transports | [BSD-2-Clause](third_party/licenses/gorilla-websocket.LICENSE) |
| [Pion TURN](https://github.com/pion/turn) | Embedded TURN server and existing Pion WebRTC transport | [MIT](third_party/licenses/pion-turn.LICENSE) |
| [go-qrcode](https://github.com/skip2/go-qrcode) | Native pairing QR generation | [BSD-3-Clause](third_party/licenses/go-qrcode.LICENSE) |
| [Ghostty](https://github.com/ghostty-org/ghostty) | Native terminal emulator library, bundled by MyGo | [MIT](third_party/licenses/ghostty.LICENSE) |
| [Mermaid](https://mermaid.js.org) | Offline sandboxed diagram renderer (`11.12.2`) in `internal/desktop/webcanvas/assets/` | [MIT](third_party/licenses/mermaid.LICENSE) |
| [KaTeX](https://katex.org) | Offline math formula renderer (`0.16.21`) in `internal/desktop/webcanvas/assets/` | [MIT](third_party/licenses/katex.LICENSE) |

External runtime integrations (not bundled with Vantry binaries):

- **Pi CLI** (`@earendil-works/pi-coding-agent`): invoked as an external user-installed CLI over RPC; retains its own license and terms.
- **Vantry Remote Desktop Plugin** (`github.com/muq9738/vantry-plugin-remote-desktop`): maintained in its own repository with its own license terms.

Historical license files for Alma Relay, bonjour-service, moejs and moego remain for attribution of earlier implementations. Their network/runtime packages are no longer part of the current application build.

Research snapshots, local validation records and private machine configuration are excluded from the repository and release source. Required upstream license texts and runtime dependency identities remain included; they must not be removed or renamed to imply authorship of third-party code.
