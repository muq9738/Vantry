# Vantry 行为与能力边界

本页区分已经接通的调用链与尚未完成的工作流。运行态应检查Core能力、实际结果及所用机器，不以版本、控件存在或测试数量替代验收。

## 已接通

| 能力 | 当前行为 | 保留的边界 |
| --- | --- | --- |
| 模型与默认值 | 从Pi RPC读取目录，新草稿按Pi settings默认，已有线程独立设置 | 可选择不等于每个模型都执行过；切模型影响后续运行 |
| Core升级 | 读取status/capabilities，空闲旧服务才轮换 | 活动任务不强杀，源码版本不代表所有机器已启用 |
| 编排 | 同Core线程查询、消息、等待、委派、取消、持久父子任务，及基于 `thread_watches` 的事件驱动父子/同级自动唤醒与有界续行回传 | Provider轮次结束与整个任务完成分别判断 |
| `/fork` 与上下文交接 | 支持从会话/指定轮次 `/fork` 分叉，选择新模型与可选独立 Git Worktree，并通过顶部溯源徽章回到父/源会话 | 不承诺活动 Provider turn 中途强行迁移 |
| Git 工作树与块级审查 | 支持单块（per-hunk）暂存/取消暂存/撤回，及独立 Worktree 状态检测与一键合并回主分支并清理 | 冲突合并仍保留 Worktree 并由用户手动解决 |
| 双引擎可视化工作台 | L1 原生 GPU 图表（`bar`/`stacked`/`stacked100`/`hbar`/`line`/`area`/`combo`/`scatter`/`bubble`/`heatmap`/`pie`/`donut`/`stats`，`chart.Time` 时间轴、`chart.MinMax` 下采样、`RefLines`、CSV/SVG 导出）与原生 TeX 行内/块级公式（`ExtDollarMath`）；L2 离线沙箱 `WKWebView` 右侧画布（`vantry-canvas://`，Mermaid v11、净化 SVG、CSP 隔离 HTML iframe、KaTeX、版本差异对比、2x 离屏快照与元素点击标注回传） | 原生桌面测试覆盖无窗口渲染；WKWebView 实时交互与离屏截图需在有图形窗环境的本机运行态实测 |
| queue/steer | 完成边界可按合同入队，交付不明有独立状态 | 不承诺活动Provider turn的任意重启/接管 |
| 跨环境 | 配对或SSH环境可派发，工具、输出、请求、结果回传并续行原父任务 | 未知目标不回落本机；proxy与真实远端线程分开 |
| UI和资源 | 全机owner侧栏、原生会话流、模型、技能、IME、附件、Git与终端入口 | 本机路径、附件句柄和配置不作为远端默认 |
| 远程桌面 | 局域网直连/原生中继 WebRTC、硬件H.264、原生窗口、受控OS输入及按需权限流程 | 正式App须用户授权；Windows真机结论单独记录 |

## 未完成或需继续验证

| 事项 | 当前边界 | 验收目标 |
| --- | --- | --- |
| Provider会话归属 | Thread/Run/Attempt与worker持久身份已有，完整会话分层仍需补齐 | 停止、恢复、模型变化和旧回调隔离保持一致 |
| 活动模型交接 | thread.model仅影响之后的新输入 | 明确当前turn终结后的上下文与目标模型接续 |
| 完整fork/handoff | 已支持 Core `ForkThread` 模型交接、可选 Worktree 隔离及桌面 `/fork` 对话框 | Pi session 复制边界与极端异常场景仍按具体运行态验证 |
| checkpoint/rollback | 已打通 `/rewind` 对话与工作区检查点回退及 `git.hunk.{stage,unstage,revert}` 单块操作 | 跨机器/远端代理检查点仍受目标机仓库状态约束 |
| 运行阶段与用量 | 文本、推理、工具、请求、control和终态已接通 | 完善压缩/重试/usage，不能以字符数代替账单 |
| 跨机资源 | 无自动负载均衡、完整逻辑项目聚合、附件迁移或远端PTY | 环境身份贯穿导航、配置和所有资源 |
| 远端历史裁剪 | 原Attempt可按游标重附着；历史不够时明确失败 | 权威快照重同步不丢失且不重复，不重发prompt |
| 平台与输入 | Mac自动化、受控Unicode/鼠标有证据，真人IME与Windows行为仍按环境验证 | 不以离屏fixture、交编或无GPU CI代替实机 |

Pi用户扩展/MCP仅在显式配置时启用；支持开关不代表兼容所有个人资源。离线 `WKWebView` 画布仅允许 `vantry-canvas://` 与内存嵌入式资源，阻断外网请求且不启用 `MCP Apps`。

更多功能说明见[功能清单](current-functions.md)、[远程桌面](remote-desktop.md)与[Windows 预览](windows-remote-desktop.md)。
