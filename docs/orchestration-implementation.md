# 真实任务编排实施合同

本轮以“在 Vantry 派出真实子任务，关闭 UI 后继续执行，Core 强退重启后交付一次结果并续行父任务”为设计目标。本文记录并行实现的接口合同。

## 组件边界

- 主代理：`pkg/api`、`pkg/provider`，应用装配、CLI、原生桌面与端到端验收。
- DeepSeek 独立 worktree：仅 `internal/core/`。负责迁移、父子调度、cohort、结果确认、续行、事件游标、恢复及其测试。
- Kimi 独立 worktree：仅 `internal/adapters/provider/pi/` 与 `internal/adapters/workspace/`。负责真实 Pi、独立 worker、持久事件日志、工作区准备及其测试。
- 只读审查：核查编排、交付和故障恢复行为，独立写入者不修改其他组件工作区。

新增代码不使用摘要、哈希或内容锁。现有命令幂等需改为以 commandId 和实际命令 JSON/业务字段核对；不得为新命令扩散旧摘要实现。已有历史记录不批量重写，不可确认的旧命令 ID 拒绝重新执行，明确返回冲突。

## 共同 API

协议保持增量兼容 version 1。`pkg/api/api.go` 是字段真源。

- `thread.create` 支持 `providerId` (`pi`/`controlled`)、`model`、`workspace`。线程上的配置持久化，Run 固定使用已解析配置。
- `task.delegate`：`parentRunId`、`message`、可选 `title/providerId/model/workspace`，`resumePolicy` 为 `auto`（默认）或 `manual`。创建 task、child thread/run/attempt、cohort、outbox 与回执一次提交。`parentAttemptId` 非空时必须匹配当前父 Attempt，隔离旧工具回调。新子线程继承模型；Git 仓库默认独立 worktree；普通目录显式共享并串行使用。
- `task.followup`：`taskId/message`，开启下一 generation，复用 child thread，创建新 child run；禁止覆盖旧 generation 尚未消费的交付。旧 generation 记录仍能查询。
- `task.acknowledge`：`parentRunId/generation/deliveryMessageId` 必须共同匹配。将同 cohort 可消费结果 acknowledged，并原子创建唯一父 continuation Attempt/outbox。ack 表示 Core 已接受交付为父输入，不代表模型理解内容。重复/过期确认不能重复续行。
- `task.dispose`：`taskId/generation/deliveryMessageId` 使该交付不再消费；不得把 disposed 结果加入后续父输入。无结果或 generation 不匹配拒绝。
- `run.stop` 支持 waiting_children，取消父任务时级联活动后代并 dispose 尚未消费交付。取消单一子任务仍交付其终态，不能复活已取消父任务。
- Snapshot 的 `tasks/cohorts` 是持久投影；task/cohort 事件载荷分别为 `{task: ...}` / `{cohort: ...}`；Run 事件延用 `{run: ...}`。UI 从快照/已提交事件显示任务和结果。

同一父 Run 的开放 generation 收集并行子任务。每个 task 与 cohort 记录 generation；cohort 生成一个持久交付消息 ID。子终态提交后正常 reconcile，启动屏障与后台 worker 也幂等扫描“已终态但尚未交付”的窗口。整组终结才 auto ack；manual 留待用户确认。父 Provider 尚未返回时可以交付，只有父当前 Attempt 结算后才创建续行，避免父会话并发。

父 Run 在 Provider 返回而仍有未消费子任务时变为 waiting_children，保持同线程唯一活动 Run。续行保持 ParentRunID，创建新 AttemptID、递增 ordinal、重置 provider cursor；续行消息保存在 attempt/outbox 以免覆盖原用户输入，并含子任务身份、generation、终态与结果。结果不依赖 UI 内存。父 Run 原输出可保留并追加。

## Provider 与恢复

原 `provider.Provider.Run` 保留用于旧测试及 controlled。`provider.Durable` 的合同见 `pkg/provider/provider.go`：

- `RunEvents(ctx, Request, emit)` 启动或附着同一个 Attempt，绝不在已有执行标记后重发 prompt。
- Event.Sequence 在该 Attempt 内从 1 递增。Core 在同一事务检查 Run/Attempt/claim、游标并写入输出与 provider cursor。重复事件不追加；间隙明确报错。`Request.AfterSequence` 提供最后已提交位置。
- `Recoverable(Request)` 仅判断该请求所属 Provider 的恢复能力；不执行外部操作。只有支持恢复的 Provider 才保留旧活动 Run/Attempt，恢复时重置 claim 重新附着；普通受控 Provider 沿用 interrupted。
- `ErrDetached` 表示 Core 观察器停止，worker 仍可执行，不结算为失败/中断。`Stop` 是独立持久停止意图，按 Attempt 发送给 worker；Core 停止观察不等于用户停止。
- Kimi 实现 `pi.New(Options) *Provider`，Options：`DataDir`、`Executable`（Vantry 自身可执行文件）、`PiExecutable`、`SocketPath`、`DefaultProviderID`、`DefaultModel`、`DefaultWorkspace *api.WorkspaceSpec`、`ControlledDelay time.Duration`。Provider 同时路由 `pi` 和 `controlled`（后者可调用原受控适配器），Recoverable 仅 pi。
- 隐藏入口 `pi.RunWorker(ctx, jobDir) error`，Vantry 主入口在 `provider-worker --job-dir PATH` 调用。worker 脱离 Core 进程组，不写核心数据库；请求、事件日志、完成状态、取消标记保存在私有 DataDir/jobs/AttemptID。worker 持有文件锁；启动意图已持久但无法确定实际投递时明确 interrupted/failed，不盲重发。
- Pi 通过现有本机 CLI 与凭据运行，显式模型/provider、cwd、session 文件；禁止输出凭据。不同 thread 使用不同 session，父 continuation 沿用父 thread session。真实工具和错误必须被消费，不能只提取最终 stdout 然后假装成功。
- Provider 的工作区准备先于 Pi 启动；directory/worktree 规范化、记录归属，Git 准备可恢复。失败不回落主 checkout。未确认归属或脏现场保留。活跃工作区写入用稳定目录归属加文件锁串行化。
- 父 Pi 的附加指令提供 Vantry CLI 的 task.delegate 用法与 parent run/attempt/data-dir。这是调用真实 Core 的入口；派发后结束本轮让 Core 持久等待。不得用 Pi 自己的临时 subagent 代替 Vantry task。

## 验收

组件测试覆盖：命令重复与冲突；并行 cohort；过期 generation/消息确认；manual ack、dispose、followup；父 Stop 与子完成竞争；Provider 结束/结算/交付/确认各崩溃窗口；重复日志重放；非恢复 Provider 中断；schema 1 升级。

真实独立进程验收使用临时 Git 仓库与私有数据目录，显式选择已发现 Pi 模型：创建父任务、派发子任务、关闭原生 UI，证明 worker 与 Core 持续；执行中 SIGKILL Core，证明 worker 持续并产出；重启 Core 读回日志，确认同一 child attempt 未重发、交付消息唯一、父 continuation 唯一，最终输出包含子结果。重开原生 UI 读取同一持久状态。记录实际通过/失败/未测，不将模拟测试当真实模型验收。
