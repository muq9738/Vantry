# Codemode 优先的原生 MCP 管理

## 结论与架构定位

主流 Agent Harness（如 **Claude Code**、**OpenCode**、**T3 Code** 与 **Pi**）对 MCP 的共同演进方向是：**不再把几十个外部 MCP 工具的冗长 JSON Schema 全量塞进初始系统提示词，而是默认收敛到沙盒代码调用（Codemode）或按需检索（Tool Search / Deferred），并把原始长输出在沙盒内过滤裁剪后再返回给模型。**

Vantry 底层已只对接 Pi，因此直接以 Pi v1.5+ 原生支持的 **`builtin:mcp` + `builtin:codemode` + `builtin:tool-search`** 为唯一执行与协议底座，不重复造 MCP 客户端或 JS 沙盒：

| 层级 | 职责 | 说明 |
|---|---|---|
| **Pi 原生 `builtin:mcp`** | stdio / Streamable HTTP 传输、OAuth、凭证、重连、资源与工具注册 | 配置存放在 `~/.pi/agent/mcp.json`（全局用户）与工作区 `.pi/mcp.json`（项目），默认 `exposure: "codemode"` |
| **Pi 原生 `builtin:codemode`** | QuickJS 沙盒，提供 `describeNamespace`、`searchTools`、`describeTool`、`tools.mcp__*`、`Promise.allSettled` | 默认仅在系统提示词列出简洁的 `mcp_servers` 摘要，模型按需编写沙盒脚本并发调用和过滤大输出 |
| **Vantry Worker（Go）** | 传入 `--extension builtin:mcp`、`--extension builtin:codemode`、`--extension builtin:tool-search`，并注入 Codemode 引导指令 | 即使关闭用户第三方扩展（`--no-extensions`），只要未禁用 MCP，内置 `mcp` 与 `codemode` 仍可正常工作 |
| **Vantry Core / Desktop UI** | 配置读写（`mcp.list` / `mcp.upsert` / `mcp.remove`）、项目信任管理（`mcp.trust`）与按需探活（`mcp.probe`） | 所有凭据与命令行参数留在宿主 Core 机器，接口脱敏返回 |

## 能力明细

1. **Codemode 默认暴露模式与细粒度控制（`exposure` / `toolExposure`）**
   - `mcp.upsert` 新增完整服务（`command` 或 `url`）时，若未显式指定 `exposure`，默认显式写入 `"exposure": "codemode"`（支持 `codemode` / `deferred` / `direct` / `hidden`）。
   - 支持编辑 `description`（用于在系统提示词与 `searchTools` / `describeNamespace` 中帮助 Codemode 检索）及保留/校验 `toolExposure` 工具级覆盖、`timeout`、`cwd`、`oauth` 等 Pi 原生字段。
   - 兼容粘贴单个服务 JSON 对象，或仅含当前服务名的 `{"mcpServers": {"<name>": {...}}}` 标准片段。
   - 用户级服务在项目中修改启用状态或 `exposure` 时，只写 `.pi/mcp.json` 中的覆盖字段，不复制用户级 `env`、`headers` 或 URL 凭据；列表以 `project-override` 标示，移除覆盖即可恢复用户默认。

2. **全局与项目双作用域（`user` / `project`）**
   - 未打开项目目录时也可打开顶部 / 工具坞 **MCP** 面板，查看、添加、删除或探活全局 `~/.pi/agent/mcp.json` 服务。
   - 支持 `mcp.remove` 删除项目服务、恢复 `project-override` 继承或删除用户全局服务。

3. **显式项目信任桥接（`mcp.trust` → `~/.pi/agent/trust.json`）**
   - Pi 的 `pi --mode rpc` 非交互模式不会弹出终端 `/trust` 提示，未信任项目的 `.pi/mcp.json` 会被静默跳过。
   - `internal/mcpconfig/trust.go` 按 Pi `ProjectTrustStore` 规则校验最近祖先目录与 `defaultProjectTrust`，并在用户确认后原子写入 `~/.pi/agent/trust.json`（`0600`），不自动信任未知仓库。

4. **按需真实探活（`mcp.probe` → `pi mcp list --json`）**
   - 普通打开面板（`mcp.list`）只读配置、不主动拉起服务。
   - 点击「检测连接」触发 `mcp.probe`，在工作区上下文调用 Pi 原生 `pi mcp list --json`（即使部分服务返回退出码 `1` 也会解析完整 JSON 诊断），合并 `liveState`（如 `connected` / `needs_auth` / 错误信息）与已发现工具列表，URL 敏感部分继续脱敏。
   - OAuth 远程服务需要登录时提示用户通过 `pi mcp login <server>` 完成授权。

## 验证命令

```sh
GOTOOLCHAIN=go1.27.3 go vet ./...
GOTOOLCHAIN=go1.27.3 go test -race ./internal/mcpconfig/... ./internal/adapters/provider/pi/... ./internal/core/... ./internal/app/... ./internal/desktop/...
```
