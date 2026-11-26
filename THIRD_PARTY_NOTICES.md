# Third-party notices

Vantry's original code and documentation are licensed under the [MIT License](LICENSE). Dependencies and independently maintained plugins retain their own terms; Vantry's license does not replace them.

Rust workspace dependencies are recorded in `Cargo.toml` and `Cargo.lock`. The desktop UI uses NSH (pure Rust UI set), Core uses transactional SQLite via `rusqlite`, the Pi provider embeds `pi-coding-agent` (`pi-rust`), and remote transports run natively on `rustls` / `tokio`. The notices below cover major components, protocol compatibility, and retained attributions. Original copyright and license texts are preserved.

| Project | Use | License |
|---|---|---|
| [NSH](https://github.com/longbridge/nsh) | Native GPU desktop UI framework (pure Rust UI set) | Apache-2.0 / MIT |
| [pi-rust](https://github.com/yannelli/pi-rust) | Embedded `pi-coding-agent` session runtime and TypeScript extension host | MIT |
| [T3 Code](https://github.com/pingdotgg/t3code) | Orchestration components and protocol compatibility | [MIT](third_party/licenses/orchestration.LICENSE) |
| [Lucide](https://lucide.dev) | Icon design heritage | [ISC](third_party/licenses/lucide.LICENSE) |
| [Mermaid](https://mermaid.js.org) | Diagram syntax and rendering support | [MIT](third_party/licenses/mermaid.LICENSE) |
| [KaTeX](https://katex.org) | Math formula rendering support | [MIT](third_party/licenses/katex.LICENSE) |

External runtime integrations (not bundled with Vantry binaries):

- **Qoder model provider** (`@magpie-community/opencode-qoder-auth`, MIT) is imported on demand from the [community plugin repository](https://github.com/magpie-community/plugins/tree/main/packages/qoder). Its package metadata and CLIProxyAPI license notice are retained in the installed directory.
- **Vantry Remote Desktop Plugin** (`github.com/muq9738/vantry-plugin-remote-desktop`): platform native H.264 / input helper maintained in its own repository with its own license terms.

Historical license files in `third_party/licenses/` (including MyGo, modernc/sqlite, Gorilla WebSocket, Pion TURN, go-qrcode, Ghostty, Alma Relay, bonjour-service, moejs and moego) remain for attribution of earlier Go/MyGo implementations (`v0.1.42` and prior). Their Go/MyGo packages are no longer part of the Rust workspace build.

Research snapshots, local validation records and private machine configuration are excluded from the repository and release source. Required upstream license texts and runtime dependency identities remain included; they must not be removed or renamed to imply authorship of third-party code.
