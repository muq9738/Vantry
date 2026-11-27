<p align="center">
  <img src="resources/icon.png" width="88" alt="Vantry 圖示" />
</p>

<h1 align="center">Vantry</h1>

<p align="center">
  面向 AI 程式開發的 Rust 原生桌面工作區。<br />
  在單一視窗中整合對話、多代理任務、終端機、Git 差異與跨機環境。
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-237a57?style=flat-square" alt="License: MIT" /></a>
  <a href="Cargo.toml"><img src="https://img.shields.io/badge/Rust-1.97%2B-c6531a?style=flat-square" alt="Rust 1.97+" /></a>
  <a href="https://github.com/muq9738/Vantry/releases/latest"><img src="https://img.shields.io/github/v/release/muq9738/Vantry?style=flat-square&label=release&color=25292e" alt="最新版本" /></a>
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a> · <strong>繁體中文</strong>
</p>

<p align="center">
  <a href="#核心功能">核心功能</a> ·
  <a href="#安裝與上手">安裝與上手</a> ·
  <a href="#架構概覽">架構概覽</a> ·
  <a href="#從原始碼建置">從原始碼建置</a> ·
  <a href="#文件">文件</a> ·
  <a href="#開源授權">開源授權</a>
</p>

---

Vantry 結合純 Rust 的 **[NSH](https://github.com/longbridge/nsh) 原生桌面**、以 SQLite 驅動的持久化 **Core** 與內建 **[Pi](https://github.com/yannelli/pi-rust)** 執行時期——不依賴 Electron、WebView 或外置 Pi CLI。對話與任務歸屬各自機器上的 Core 而非單一視窗：隨時關閉桌面不影響背景代理程式執行，也可跨機重新連線並在同一個工作區內審查所有變更。

## 核心功能

| 模組 | 功能亮點 |
| --- | --- |
| **原生開發工作台** | GPU 繪製的 Stardust（星塵）淺色／深色介面，支援串流 Markdown、表格、Diff、圖表、行內 `@` 檔案／圖片／對話參照（單則最多 32 個附件並支援點擊預覽）、模型與思考等級切換及技能選擇。 |
| **持久化執行與即時控制** | 執行中支援訊息佇列與即時 `steer` 引導（可編輯、重新排序、轉為追加、取消）；提供 `auto` / `auto-accept-edits` / `ask` / `plan` 權限模式、模型專用金鑰輸入，以及關閉視窗或重啟 Core 後的狀態復原。 |
| **多代理任務編排** | 持久父子任務委派、獨立 Git worktree 隔離、權限上限繼承、於對話概覽即時檢視子任務進度與輸出、自動或手動交付結果回父對話、子任務追問與級聯停止。 |
| **檔案、Git 與終端機** | 右側工具塢整合檔案樹、搜尋與唯讀預覽、Git 狀態／差異／暫存／分支切換／提交／PR 摘要，以及內嵌終端機。 |
| **多機連線與遠端協作** | 已配對的 Vantry 電腦透過區域網路或 Alma 中繼連線。Alma iOS 可以透過區域網路或相容 Alma 的 HTTPS 中繼存取共用對話。 |
| **MCP、技能與外掛** | 原生支援 Pi MCP（`~/.pi/agent/mcp.json` 與專案 `.pi/mcp.json`），採用 Codemode／按需工具探索、明確專案信任與連線探測；支援宣告式原生外掛面板與獨立授權的遠端桌面。 |

桌面介面內建 **简体中文**、**English** 與 **日本語**。

## 安裝與上手

前往 [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest) 下載預建置安裝套件。

| 平台 | 安裝方式與說明 |
| --- | --- |
| **macOS · Apple Silicon** | 需要 macOS 14（Sonoma）或更新版本。開啟 `Vantry-<版本>-macos-arm64.dmg`，將 `Vantry.app` 拖入「應用程式」。另提供獨立的 `vantry-service` 無介面服務封裝。 |
| **Windows · x64** | 解壓縮可攜式 `vantry-<版本>-windows-amd64.zip` 並執行 `Vantry.exe`。另提供獨立的 `vantry-service` 服務封裝。 |
| **Linux** | 支援從原始碼建置。`vantry-service` 與 `vantry-relay` 可無介面執行；NSH 桌面需 GTK 4.14+，目前尚未提供預建置桌面發行套件。 |

### 快速開始

1. **設定模型供應商** —— 開啟 **設定 → 供應商** 填入 API Key 或自訂端點，或直接沿用 `~/.pi/agent`（`PI_CODING_AGENT_DIR`）中既有的 Pi 設定。Vantry 已內建 Pi 執行時期，不需額外安裝 Node.js 或外部 Pi CLI。
2. **開啟專案** —— 選取本機專案資料夾（或建立獨立 Git worktree），選擇模型與思考等級。
3. **開始對話** —— 檢視彙整的工具呼叫、按需核准敏感操作、於執行途中佇列或追加引導，並在右側 Git 面板審查所有修改。

應用程式更新會於背景靜默下載並進行 SHA-256 驗證（支援二進位 `.delta` 增量修補），於 Core 閒置時支援一鍵重啟套用；桌面啟動時也會自動替換閒置的舊版本機 Core，不干擾進行中的任務。

## 跨機連線與遠端桌面

- **對話配對** —— 兩台電腦在「設定 → 環境」使用主機產生的完整連結配對，支援區域網路與 Alma 公網中繼，側欄可依機器查看對話。手機使用行動網路時，在所選對話的 Alma iOS 配對視窗填入 Alma 中繼的公開網址。
- **遠端桌面** —— 在主機開啟共用，另一台電腦使用區域網路邀請連結連線。macOS 擷取畫面需要「螢幕錄製」權限，鍵鼠控制需要「輔助使用」權限。

## 架構概覽

Vantry 將介面繪製、持久狀態管理與代理程式執行隔離於不同程序，確保關閉視窗不中斷工作，重啟 Core 也不會重複送出 Prompt：

```text
NSH 桌面 (vantry) / CLI / 已配對用戶端
       │  本機 IPC（Unix socket / Windows）· 遠端 mTLS / 中繼
       ▼
Core (vantry-service core) ── SQLite（state.sqlite：事件、回執、任務、outbox）
       │  啟動或重新附著持久 Attempt
       ▼
Worker (vantry-service provider-worker) ── 內建 pi-rust ── 模型服務與 MCP
```

### 工作區結構

| 目錄 | 職責 |
| --- | --- |
| `apps/desktop`（`vantry`） | NSH 原生桌面介面與統一 CLI 入口（服務子命令自動轉交 `vantry-service`）。 |
| `apps/service`（`vantry-service`） | 持久化 Core 服務、持久 Pi worker、`ctl`、`peer-proxy` 與更新安裝程式。 |
| `apps/relay`（`vantry-relay`） | 獨立自建對話與桌面中繼服務（不含 UI 與 Pi 相依套件）。 |
| `crates/core` | SQLite 交易儲存、命令冪等回執、父子任務排程器、outbox 與事件串流。 |
| `crates/pi-runtime` | 內建 `pi-rust` 對話宿主、模型目錄、MCP 橋接、核准互動與執行日誌。 |
| `crates/protocol`、`crates/client` | v1 協定型別與支援 gzip 壓縮、游標續傳的 HTTP/NDJSON 用戶端。 |
| `crates/connectivity` | 裝置身分、電腦 TLS 配對與 Alma 公網中繼、區域網路桌面傳輸及 Alma 手機端對端加密連線。 |
| `crates/platform` | 工作區與 worktree 邊界檢查、Git 轉接層、PTY、外掛宿主與系統整合。 |
| `crates/updater`、`tools/xtask` | 增量／完整更新引擎與發行封裝工具（`package`、`make-delta`、`prepare-icon`）。 |

## 從原始碼建置

### 環境需求

- **Rust 1.97 或更新版本**（Edition 2024）、**Git**，以及目標平台的原生建置工具鏈（macOS 需 Xcode Command Line Tools；Windows 需 MSVC；Linux 桌面需 GTK 4.14+ 開發函式庫）。

### 建置與執行

```sh
git clone https://github.com/muq9738/Vantry.git
cd Vantry

cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
```

常用啟動模式：

```sh
# 使用記憶體示範 Core 預覽介面（無需設定模型 API Key）
target/debug/vantry desktop --mock

# 僅連線已執行的本機 Core，不自動啟動
target/debug/vantry desktop --attach-only

# 直接開啟已配對的遠端機器（auto | direct | relay）
target/debug/vantry desktop --peer <PEER_ID> --peer-mode auto

# 獨立執行無介面 Core 服務
target/debug/vantry-service core --data-dir /path/to/vantry-data
```

### 測試與封裝

```sh
cargo test --workspace --locked
cargo run --locked -p xtask -- package desktop
```

- 在 Apple Silicon macOS 上，桌面產物（`Vantry.app`、`.dmg` 與更新用 `.zip`）輸出於 `target/package-macos-arm64`。
- 其他目標平台輸出至 `target/package-<產物>-<系統>-<架構>`（可搭配 `--out-dir <DIR>` 指定）。`xtask package` 亦支援封裝 `service` 與 `relay`。

## 文件

工程文件以中文為主。部分設計文件撰寫於純 Rust 重構前，仍保留底層協定與行為契約參考。

| 文件 | 主題 |
| --- | --- |
| [本機啟動與開發](docs/getting-started.md) | 原始碼啟動參數、示範／附著模式與本機封裝。 |
| [Rust 架構重構方案](docs/nsh-core-pi-rust-plan.md) | 程序邊界、內建 `pi-rust` 整合與故障復原契約。 |
| [桌面介面說明](docs/desktop-ui.md) | Stardust 視覺語言、輸入互動與工具塢面板。 |
| [任務編排](docs/task-orchestration.md) | 父子任務委派、工作區隔離與結果續行規則。 |
| [MCP 整合](docs/mcp-integration.md) | Codemode 優先的 MCP 探索、專案信任與連線探測。 |
| [原生遠端連線](docs/native-connectivity.md) | 電腦與 Alma iOS 的區域網路與公網配對。 |
| [遠端桌面](docs/remote-desktop.md) | 區域網路螢幕共用與 macOS 權限。 |
| [更新機制](docs/updates.md) | 背景驗證、`.delta` 增量修補與 Core 閒置交接。 |
| [版本發行紀錄](docs/releases/) | 各版本變更說明與下載連結。 |

## 致謝

Vantry 建構於並受啟發於以下優秀的開源專案：

- **[NSH](https://github.com/longbridge/nsh)** —— 純 Rust 原生 GPU 桌面 UI 框架。
- **[pi-rust](https://github.com/yannelli/pi-rust)** —— 內建 `pi-coding-agent` 對話執行時期與擴充宿主。
- **[T3 Code](https://github.com/pingdotgg/t3code)** —— 任務編排流程的設計參考。
- **[Lucide](https://lucide.dev)**、**[Mermaid](https://mermaid.js.org)** 與 **[KaTeX](https://katex.org)** —— 圖示設計、圖表與數學公式繪製支援。

## 開源授權

Vantry 的原創原始碼與文件採用 **[MIT License](LICENSE)** 授權。相依元件與獨立維護的外掛保有各自授權條款，詳見 [第三方聲明](THIRD_PARTY_NOTICES.md) 與 [`third_party/licenses`](third_party/licenses)。
