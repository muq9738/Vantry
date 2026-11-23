# 执行 Provider：仅支持外置 Pi

Vantry 现已全面聚焦 **Pi**（`pi-coding-agent`），移除了原有 ACP（包括 `agy` / Antigravity、`qoder` 及自定义 ACP agent）适配层。为保持应用包体积轻巧（全量包控制在 **25 MB 以内，远低于 50 MB**），Vantry **不捆绑 Pi 二进制**，统一调用本机或自定义路径下的外置 `pi` CLI。

## 外置运行时与配置目录分工

- **轻量化外置执行**：自动探测本机 `PATH`、Homebrew、nvm（或 Windows npm / Scoop）安装的 `@earendil-works/pi-coding-agent`，也支持在「设置 → 提供方」手动固定自定义 `pi` 可执行路径；若本机尚未安装且存在可用的 Node.js / npm，也可在兼容验证通过后一键安装外置 Pi。
- **共享标准配置目录（不使用软链）**：Vantry 直接读写标准 `PI_CODING_AGENT_DIR`（默认 `~/.pi/agent`）下的 `auth.json`、`models.json` 与 `settings.json`，绝不创建软链接或覆盖已有配置。未配置模型时，首页与模型列表提供图形化「配置模型」向导（支持 DeepSeek、OpenAI、Anthropic、Gemini、OpenRouter、Kimi 及自定义兼容接口，或终端 OAuth 登录）。

## 零影响四重门禁：外置 Pi 安全升级

在「设置 → 提供方」检查或升级外置 Pi 时，Vantry 默认启用国内外镜像自动路由（自动探测官方 `registry.npmjs.org` 与淘宝 `registry.npmmirror.com`），并通过 **四重安全门禁** 严格保证「仅在确认升级对 Vantry 零影响时，才开放升级按钮」：

1. **版本兼容窗口检查（SemVer Policy）**：仅放行已认证的主版本区间（当前为 `1.x` 稳定版），自动拦截预发布版、跨主版本升级（如 `2.x`）与已知不兼容版本。
2. **本机工具链与安装形态校验**：对照 npm 包的 `engines.node` 校验本机 Node.js 版本，确认 `npm` 可用且当前外置 Pi 为 npm 全局安装（自定义非 npm 路径不盲目覆盖）。
3. **零活动任务门禁（`activeRuns == 0`）**：当 Core 存在正在执行的会话或子任务时，禁止触发并隐藏升级入口。
4. **临时沙箱预装 + Vantry 实时契约探测 + 失败自动回滚**：在系统临时目录通过 `npm install --prefix <tmp>` 隔离预装候选版本，注入嵌入的 `vantry_tools.ts` 与 `approval.ts` 启动 `pi --mode rpc --extension builtin:codemode --extension builtin:tool-search --tools +codemode,+tool_search`，验证 `vantry_approval_ready` 握手及 `get_available_models` / `get_state` / `get_available_thinking_levels`（零 LLM 调用、不触碰用户 `~/.pi/agent`）。四项均通过并缓存结果后才显示「升级 Pi」；真正 `npm install -g` 升级后若实机校验失败，将自动回滚到升级前版本。

## 为什么选择 `pi --mode rpc`

对照官方 `@earendil-works/pi-coding-agent` 架构文档：

- **SDK** 仅适用于 Node.js / Bun 同进程 TypeScript 宿主。
- **`pi --mode rpc`（JSONL over stdin/stdout）** 是官方推荐给 Go / Python / IDE 等跨语言外部宿主的持久化双向控制接入方式，完整复用外置 Pi 的模型目录、OAuth 凭证、扩展、MCP、`codemode`、`tool_search`、压缩（compaction）与失败重试。

Vantry 通过分离的 durable worker（`vantry provider-worker --job-dir`）托管每次 `pi --mode rpc` 运行：

1. **进程隔离与断线重连**：Core 不直接持有模型子进程；`events.jsonl`、控制文件与独立进程组保证观察窗口关闭或 Core 重启后不会重复提交 prompt。
2. **完整运行期控制**：支持 `agent_settled` 精确终态判定、运行中 `steer`、`extension_ui_request` 审批交互与工作区隔离（`shared` / `worktree`）。
3. **内置 Codemode 与按需发现**：无论用户扩展是否启用，worker 均显式加载 `builtin:codemode`、`builtin:tool-search` 与 `builtin:mcp`（除非 `VANTRY_PI_USER_MCP=0` 关闭），通过 `--tools +codemode,+tool_search` 将低频编排工具与外部 MCP 工具收拢到 QuickJS 沙盒按需检索执行。

> [!NOTE]
> `internal/desktop/workbench_chrome.go` 中的 `Antigravity` 仅用于「在外部编辑器中打开项目目录」，与执行 Provider 无关。

## 验证

```sh
GOTOOLCHAIN=go1.27.3 go vet ./...
GOTOOLCHAIN=go1.27.3 go test ./...
```
