<p align="center">
  <img src="resources/icon.png" width="88" alt="Vantry 圖示" />
</p>

<h1 align="center">Vantry</h1>

<p align="center">
  面向 AI 程式設計與多 Agent 協作的原生桌面工作台。<br />
  將對話、代理、Git 變更與遠端機器整合在同一個工作空間。
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-237a57?style=flat-square" alt="開源授權：MIT" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.27.3%2B-007d9c?style=flat-square" alt="Go 1.27.3 或以上版本" /></a>
  <a href="https://github.com/muq9738/Vantry/releases/latest"><img src="https://img.shields.io/badge/download-releases-25292e?style=flat-square" alt="下載發行版" /></a>
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a> · <strong>繁體中文</strong>
</p>

<p align="center">
  <a href="#快速開始">快速開始</a> ·
  <a href="#建置與開發">開發指南</a> ·
  <a href="#文件">文件</a> ·
  <a href="#開源授權">開源授權</a>
</p>

---

Vantry 採用 **Go + MyGo 原生介面**，搭配持久化的 **Core** 與 **Pi 執行引擎**。對話與任務由所屬的 Core 管理，關閉視窗後工作仍會持續執行。沿用你既有的 Pi 模型設定與憑證，在本機或已連線的遠端機器上開發，並在同一工作台中檢視變更。

## 主要功能

| 領域 | 能力 |
| --- | --- |
| **程式設計工作台** | 串流對話、模型選擇、多行輸入、附件、技能選擇與行內對話引用。 |
| **Agent 協作** | 持久化父子任務、獨立 Git worktree、即時狀態、結果交付與連續追問。 |
| **Git 與檔案** | 檔案瀏覽、差異檢視、暫存、提交、分支管理，以及支援篩選與提交操作的圖形日誌。 |
| **多機協作** | 依機器與專案分組管理對話，支援遠端執行、配對碼與可選的自建中繼。 |
| **執行控制** | 工具審批、訊息排隊、執行中即時調整指令、持久事件與 Core 重啟恢復。 |
| **外掛擴充** | 原生頁面、顯式處理程序介面，以及遠端桌面外掛。 |

介面支援 **英文、簡體中文與日文**。

### 遠端桌面外掛

遠端桌面透過獨立外掛提供，支援原生 H.264 畫面與鍵鼠控制，可經由區域網路直連或自建中繼運作。按需開啟共用，每次連線皆須於共用端確認；macOS 需授予螢幕錄製與輔助使用權限。應用程式不預設任何中繼位址。

## 快速開始

### 下載

從 [GitHub Releases](https://github.com/muq9738/Vantry/releases/latest) 取得安裝套件。

| 平台 | 支援狀態 |
| --- | --- |
| **macOS · Apple Silicon** | macOS 13 或以上版本。解壓縮後將 `Vantry.app` 移至 `/Applications`。目前發行版採本機簽章，尚未經過 Apple 公證。 |
| **Windows · x64** | ZIP 預覽版。目前發行版由交叉編譯產生，尚未完成 Windows 實機驗證，亦未內含原生遠端桌面 helper。 |
| **Linux** | 目前尚無已驗證的桌面發行版。 |

### 開始使用

1. 在執行 AI 任務的電腦上安裝並設定 **Pi CLI**（`@earendil-works/pi-coding-agent`）。Vantry 直接使用其模型設定與憑證，應用程式本身不內建 Pi。
2. 開啟 Vantry，選擇專案資料夾與模型。可直接使用原目錄，或建立獨立的 Git worktree。
3. 開始對話，檢視工具執行活動、按需處理審批，並在 Git 面板中審查修改。

跨機器協作時，從裝置控制入口與另一台 Core 配對。需要共用螢幕時，使用遠端桌面外掛及其獨立的共用碼。

更新會在背景下載並驗證（支援二進位增量套件 `.delta`），確認 Core 閒置後支援一鍵重啟並套用。詳見 [更新機制](docs/updates.md)。

## 建置與開發

### 環境需求

- **Go 1.27.3 或以上版本**與 Git。
- 執行實際 AI 任務時，需具備已設定模型服務的 **Pi CLI**（`@earendil-works/pi-coding-agent`）。
- 建置 macOS 原生遠端桌面 helper 時，需安裝 **macOS、Xcode Command Line Tools** 與 Python 3。
- 隨附遠端桌面外掛（[vantry-plugin-remote-desktop](https://github.com/muq9738/vantry-plugin-remote-desktop)）以公開 Go 模組自動下載，原生 helper 建置說明見 [外掛開發說明](docs/plugin-repositories.md)。

### 從原始碼執行

```sh
git clone https://github.com/muq9738/Vantry.git
cd Vantry

go mod download
go build -o bin/vantry ./cmd/vantry

./bin/vantry desktop --workspace "$PWD"
```

### 封裝 macOS 應用程式

先建置對應的原生 helper，再封裝應用程式：

```sh
python3 scripts/build_remote_helper.py --platform darwin/arm64
go tool mygo build -platform darwin/arm64 -skip-dmg
```

產物輸出至 `dist/darwin-arm64/Vantry.app`。簽章流程見 [macOS 封裝說明](docs/packaging-macos.md)，Windows SDK 與 helper 需求見 [Windows 開發說明](docs/windows-remote-desktop.md)。

### 檢查變更

```sh
go test ./... -timeout=120s
go vet ./...
```

部分硬體與整合測試需顯式啟用。交叉編譯通過並不代表目標平台已完成實際執行驗證。

## 架構概覽

| 元件 | 職責 |
| --- | --- |
| **Desktop** | MyGo 原生介面、對話時間軸、輸入區、檔案瀏覽、Git 與外掛頁面。 |
| **Core** | 基於 SQLite 的對話、任務編排、審批、工作區、配對與事件交付。 |
| **Pi workers** | 在任務所屬機器上執行模型請求與工具。 |
| **連線層** | 透過直連 TLS 或自建中繼存取對話；遠端桌面共用採用獨立授權。 |

應用程式進入點位於 `cmd/vantry`，介面位於 `internal/desktop`，任務行為主要在 `internal/core` 與 `internal/app`，公開介面契約位於 `pkg/api`。原生介面繪製不依賴 HTML 前端。

## 文件

工程文件目前以中文為主。

| 文件 | 主題 |
| --- | --- |
| [本機開發](docs/getting-started.md) | Core 生命週期、執行設定與 CLI 範例。 |
| [桌面介面](docs/desktop-ui.md) | 原生 Stardust 介面、快捷鍵與面板說明。 |
| [提供方與 Pi](docs/providers.md) | 外置 Pi CLI 偵測、設定與安全升級。 |
| [Agent 編排](docs/orchestration-implementation.md) | 父子任務、結果交付與恢復。 |
| [遠端連線](docs/native-connectivity.md) | 配對、遠端對話、桌面共用與中繼部署。 |
| [遠端桌面](docs/remote-desktop.md) | 區域網路直連、惰性中繼回退與 macOS 權限。 |
| [更新機制](docs/updates.md) | 增量更新、安全重啟套用與可選私有憑證。 |
| [行為與能力邊界](docs/behavior-gaps.md) | 實作邊界與待完成項目。 |

## 致謝

Vantry 是基於 Go + MyGo 的獨立原生實作，在設計理念與協定相容性上受到以下優秀開源專案的啟發與協助：

- **[T3 Code](https://github.com/pingdotgg/t3code)** — 多任務編排設計靈感與協定 2 對話相容。
- **[MyGo](https://github.com/egoist/mygo)** — 原生桌面 UI 框架。
- **[Ghostty](https://github.com/ghostty-org/ghostty)** — 原生終端機模擬函式庫（`libghostty-vt`）。
- **[Pi CLI](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)** — 外置 AI 程式設計 Agent 執行環境。
- **[Pion](https://github.com/pion/webrtc)**、**[Mermaid](https://mermaid.js.org)**、**[KaTeX](https://katex.org)** 與 **[Lucide](https://lucide.dev)** — WebRTC/TURN 傳輸、離線圖表、公式繪製與圖示資源。

## 開源授權

Vantry 的原創原始碼與文件採用 **[MIT License](LICENSE)** 授權：允許使用、修改、再散布與商業使用，僅須保留版權聲明與授權條款。本軟體按「現狀」提供，不附帶任何明示或默示擔保。

相依函式庫與獨立維護的外掛保有各自之授權條款，詳見 [第三方聲明](THIRD_PARTY_NOTICES.md) 與 [`third_party/licenses`](third_party/licenses) 目錄中的授權原文。
