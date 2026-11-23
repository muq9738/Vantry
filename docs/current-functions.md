# 当前功能与跨机对齐清单

2026-11-17，Asia/Shanghai。两台机器使用同一套0.1.11源码与远控插件0.2.0，按实际调用链和本机启用记录核对。提交号仅用于追溯。

## 当前结论

Vantry 已有原生桌面、真实 Pi 执行、同 Core 多会话编排和持久父子任务。本轮把原先未接线的跨环境任务执行接入生产 `RunCore`：可通过已保存的 IDE 配对连接或 SSH 环境，把任务送到另一台 Core，投影结果、工具与审批，并沿用本地父任务的等待、交付和自动续行。

完整工作流仍有明确边界：活动任务换模型交接、checkpoint/rollback、完整上下文分叉，以及真人输入法与部分远程交互仍需补齐。跨机验收只计算Vantry自己的执行链路，外部协作通信不计入通过项。

整合源码现标记0.1.12、持久库schema10，同时保留 `provider.pi.scripted-tools.v1` 和 `workbench.skills.v1`。0.1.12合并两边开发线、真实技能目录、全机owner侧栏、IME/键盘与原生RTC接线。升级仍检查Core空闲，按 `/v1/status` capability 判断实际服务。

远程桌面提供按需权限面板与真实系统请求入口；安装/启动零请求，授权与组件状态分别显示。旧 Core 缺 `remote-desktop.permissions.v1` 时按忙闲合同升级，不复用缺接口服务；权限未齐时保持共享关闭，详见 [远程桌面](remote-desktop.md)。

## 功能总表

| 功能 | 当前源码与可操作行为 | 需要保留的边界 |
| --- | --- | --- |
| 原生桌面 | Go + MyGo；项目侧栏、连续会话、输入区、工作区卡片、独立 diff、设置页 | 主要验证 macOS Apple Silicon；Linux/Windows 不沿用 Mac 验收 |
| 会话管理 | 新建草稿、首次发送建线程、搜索、改名、置顶、归档、稍后处理、删除；分页摘要与时间线 | 删除活动工作有独立约束；窗口草稿与 Core 已提交消息不是同一份状态 |
| 模型与权限 | 从 Pi RPC 读模型/思考等级；新会话默认取 Pi settings；已有线程独立设置；auto/ask/auto-accept-edits 与 plan | 换模型影响后续运行，当前任务交接尚未实现；不是所有 Pi 用户扩展默认启用 |
| 真实执行 | Core → 独立 worker → Pi RPC；正文、推理、工具参数/输出、确认/选择/输入/编辑请求 | `controlled` 仅为受控验收；真实模型效果需单独证明 |
| 运行中输入 | 持久 queue/steer；队列可编辑、重排、取消、提升为 steer；沿原命令身份重试 | Core 接受不等于 Pi 已接收；交付不明不能盲目重发 |
| 原生脚本工具 | 显式加载 Pi codemode/tool_search；17 个 Vantry 工具首次声明 6 个，其余按需发现；结构化结果、并行读取与摘要过滤 | 内部调用保留审批、Plan、工具事件和取消；秘密输入仅允许直接调用；不替代持久编排 |
| 同机多会话 | capabilities、list/read/search/send/wait、launch、batch、delegate、cancel | 线程、Run、Attempt、任务交付分别有身份；禁止用运行中轮次结束代替整个子任务完成 |
| 父子任务 | 独立 worktree、权限继承、整组结果交付、手动确认/自动续行、追问新 generation、级联停止 | 共享目录仍需串行写；完整上下文 fork 不是普通任务委派 |
| 持久与恢复 | SQLite 单写、事件/投影/回执/outbox 同事务；订阅游标；独立 Pi worker；原 Attempt 恢复 | 关闭窗口、停止 Core、取消任务是不同操作；受控 Provider 不具备独立 worker 恢复协议 |
| IDE 机器连接 | 同窗口选执行环境；每环境独立快照、模型、草稿和事件游标；目标目录选择；配对/TLS、中继网关、SSH | 已建立线程固定归属原环境；不把本机附件句柄、模型和路径当作远端默认 |
| 跨机 Agent 编排 | 本轮新增生产接线：显式 environmentId 派发/读/发/等/建会话；远端输出、工具、审批投影；父任务续行 | 本地 proxy 与远端真实线程分开；未知目标不回落本机；物理跨机与真模型另列证据 |
| 工作区与 Git | 文件浏览/预览/搜索、变更和 diff、暂存/撤回/提交、分支与 worktree、本地合并，以及 Git 记录/同步相关入口 | 冲突、hooks、签名、远端认证按真实结果呈现；源码中的 checkpoint 辅助模块不等于完整回退流程 |
| 终端与附件 | 本地内嵌 PTY、系统终端、Core 私有文本/图片附件、图片回复 | 远端交互式 PTY 未完整对齐；附件不能直接跨 Core 使用 |
| 插件 | moejs 命令、进程协议、安装/更新/卸载、原生页面贡献 | 原生插件渲染器仍依赖宿主能力；IDE 机器连接属于本体 |
| 技能与全机侧栏 | 真实Pi技能目录、输入区选择；按机器/项目聚合会话，导航、重命名与拖动保持owner | 远端目录、插件路径和输入焦点不继承本机默认 |
| 远程桌面 | 局域网直连或自建 HTTPS/WSS 中继、原生硬件 H.264、MyGo 视口与键鼠回传 | 正式 App 需各自授予屏幕录制/辅助功能权限；Windows 硬件编解码需在目标真机验证 |
| 独立插件仓库 | 远程桌面插件独立仓库维护标签/Release/ZIP | 原生引擎随宿主重建，见 [插件开发与独立仓库](plugin-repositories.md) |
| 更新与辅助查询 | 更新查询/下载/安装入口、工作区使用摘要、PR/预览相关适配器 | 使用摘要包含字符/运行统计，不等于模型账单；孤立模块与已接入产品入口应分开核对 |

本轮整体 UI 由指定 Opus 5.5 在隔离工作区设计实现，主代理补验首字母导航与对比度、真实原生窗口和打包启用；见 [桌面设计](desktop-design-next.md)。

主要入口：[公开命令](../pkg/api/api.go)、[编排合同](../pkg/api/orchestration.go)、[工作台合同](../pkg/api/workbench.go)、[工作台服务](../internal/app/workbench.go)、[原生远程连接](native-connectivity.md)。

## 按用户全程操作验收

1. 打开应用，确认实际 Core 能力；选择执行环境与该环境的项目目录。新草稿使用目标 Pi 的默认模型。
2. 输入中文多行、引用文件或添加目标环境附件；首次发送才建立持久线程。检查正文/推理/工具层级与滚动跟随。
3. 运行中发送排队消息或调整指令，检查实际投递状态；需要确认的工具在审批前不执行。
4. 明确授权后派出同机或远端子任务，检查工作区、模型、权限、运行归属。父轮结束后应等待子任务，不能再起第二个同线程执行器。
5. 子任务完成或提问后进入正确会话，读取结果或应答。确认结果只产生一次父任务续行；追问保留子会话，开启下一代交付。
6. 在运行中关闭窗口、重开、断开连接或重启测试 Core。检查原 Run/Attempt 恢复、输出不重复、原 prompt 不重发。
7. 明确停止父任务，确认后代任务与尚未开始的输入都被取消；远端运行应收到持久 stop intent。
8. 查看修改与 diff，执行已授权 Git 操作；完成后归档线程。插件安装/更新/卸载另按独立仓库版本验收。

## 跨环境 API 与 Pi 工具

`POST /v1/workbench`，`{"action":"orchestration.environments"}` 返回可以选择的已保存环境；Pi 中对应 `vantry_environment_list`。结果不包含配对密钥。配对连接使用已有 peer ID；SSH 使用 `environments.json` 中的 entry ID。IDE 读到的目标 Core 实际环境 ID 也可以通过已有配对解析。

随后从一个本地父 Run 派发：

```json
{
  "clientRequestId": "remote-review-1",
  "callerThreadId": "LOCAL_PARENT_THREAD",
  "parentRunId": "LOCAL_PARENT_RUN",
  "environmentId": "LINKED_ENVIRONMENT_ID",
  "task": "只读目标项目，核对接口并返回结论。",
  "mode": "async",
  "isolation": "shared"
}
```

入口为 `/v1/orchestration/delegate`，Pi 工具为 `vantry_delegate_task`。不传模型/工作区时取目标 `environment.info`；可显式传目标绝对路径的 `workspace`，`worktree` 在目标准备。本机附件句柄会被拒绝。审批和规划模式受发起会话上限约束。

本地任务保留 `taskId/childThreadId/childRunId/childAttemptId`，状态结果另带 `environmentId/remoteThreadId/remoteRunId`；本地可导航的子线程是结果与审批的代理，不伪装成本机模型执行。后续任务追问沿用远端会话；切模型仍只影响后续运行。

`capabilities`、`threads/list`、`threads/read`、`threads/send`、`threads/wait` 与独立 `threads/launch` 可显式传 `environmentId`；batch 中每个 spec 可选自己的环境。远端 Caller 不套用本机同名线程，发送请求的幂等键包含源环境与发起线程。返回的远端线程链接包含 environmentId；桌面只在匹配的目标环境视图导航，目标未连接或只有 SSH catalog 时明确提示，不把同名线程打开到本机。

对已有远端线程发消息只保证那条会话启动、排队或收到调整；需要持久父任务自动唤醒时，应使用 `delegate`。`thread_send` 本身不创建另一个父子交付合同。

## 本轮验证与限制

已执行：

- `TestPairedEnvironmentDelegationProductionE2E`：两个生产 `RunCore`、真实保存配对和 TLS/Node 连接；目标默认设置、原父 Run 续行、运行中重启本地 Core、追问保留远端会话、同名线程隔离、远端 read/send/wait、幂等重试、权限上限、未知环境拒绝。
- `TestRemoteProxyRecoversEventsControlsAndCancellation`：远端 Core 工具/推理/审批事件，本地 Core 重启、原 prompt 只执行一次、远端真实 steer 回执、审批应答、文本和工具不重复、后续任务换模型，以及 stop outbox 到达远端。
- `TestRemoteProviderRejectsLocalAttachmentHandles`：拒绝本机附件句柄跨机使用。
- Pi 扩展实加载与 UDS 合同回归：目标 environmentId 和目标路径到达实际 Core 请求；全 Pi 包通过。
- 全仓测试扫描中，其余包通过；新增 app 试验曾错误要求受控 Provider 换模型，修正验收 fixture 后 app/environments 全包 `-race` 通过。一次全量并发中的插件 gh token 2s 超时单测已单独重验通过，后续全包扫描该插件包也通过。
- `go vet ./...`、`git diff --check` 通过。两端插件/UI/链接改动整合后，除一处字体度量硬阈值外的全包扫描通过；将其改为真实单行断言后完整 desktop 包通过。测试进程按前后基线核对，不以发出停止信号代替退出确认。

自动回归不调用付费模型。真实跨机 Pi 工具编排验证需在两台已配对的 Core 之间使用各自的 Pi 模型与凭据独立执行。

需要继续对齐的行为见 [行为与能力边界](behavior-gaps.md)。特别是：远端事件被裁剪到恢复游标之前时，当前实现明确报错，不拼接不完整历史；跨机自动负载均衡、逻辑项目聚合、附件迁移、远端 PTY 和完整 Provider 会话交接尚不在已通过项中。
