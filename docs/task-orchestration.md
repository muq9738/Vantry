# Pi 原生能力与 Vantry 任务编排

普通编码、文件检查、Git 拉取、本机构建和应用更新直接在当前会话与工作区执行，不需要创建委派任务。已保存其他机器或工具可用，不代表应该把本机工作搬过去。

## 两种不同的能力

- Vantry 委派（默认且唯一的子代理路径）：由 Core 管理持久 task、独立会话、工作区隔离、跨环境执行和结果交付。在父会话右上角的会话概览中展开子代理，结束后由 Core 唤醒父会话。
- Pi 原生子代理（pi-subagents 的 `subagent`、`subagent_supervisor`、`bg_wait`、`contact_supervisor`）：**默认关闭**。Vantry 在每个回合结束时关闭 Pi 进程，原生子代理的运行既不进入 Core，也无法在 Pi 退出后唤醒父会话，异步/后台运行会变成无人接收的孤儿。设置 `VANTRY_PI_NATIVE_SUBAGENTS=1` 可重新开启，此时提示词只允许前台、同 provider 同模型、本回合内完成的原生子代理。

生产 Core 默认沿用用户的 Pi 扩展和 MCP，只通过 `--exclude-tools` 去掉上述原生子代理工具（用户其它扩展工具与 codemode 不受影响）。设置 `VANTRY_PI_USER_EXTENSIONS=0` 或 `VANTRY_PI_USER_MCP=0` 可显式禁用对应资源。Skill 查询采用相同默认值。

默认直接暴露的编排工具为 `vantry_orchestrator_capabilities`、`vantry_delegate_task` 和 `vantry_task_status`。机器列表、取消、会话管理等按需发现；密钥输入保持独立的 model-only 原生入口。

## 执行规则

只在用户要求子代理/委派/并行，或项目规则允许边界清楚的独立工作时委派；任务大小或复杂度本身不是委派理由。脚本化 workflow 只在用户要求时使用。

首次 Vantry 委派前读取能力（每个会话一次，出错或用户改 provider 后再读）。能力查询或委派失败、provider/模型不可用时，原样报告给用户并停止该路径，不回退到原生子代理、workflow、其他 CLI 或其他模型。不要擅自切换 provider、模型或机器；远端默认模型与父会话不同且未指定 model 时，Core 拒绝创建任务，要求显式选择。

`vantry_thread_launch` 和 `vantry_create_threads` 只用于用户要求的新顶层会话。“并行”或“子代理”不等于新建顶层会话。子任务默认自行完成，不再创建子任务，除非 brief 明确要求。用户要求不用委派或减少委派时，取消多余任务，不启动替代任务。

Vantry 异步任务遵循返回的 `next`，结束当前 provider 回合后由 Core 唤醒，不轮询。替换 Vantry task 使用 `vantry_task_cancel`；Pi 原生子代理使用其原生控制方式。

指定 taskId 的状态查询可以跨同一会话的用户轮次；未指定 taskId 时仍只列当前运行的任务。确认结果仍须通过父线程校验。

## 回传与验证

远端事件处理仍使用 Vantry 的持久代理与游标机制。状态快照检查独立于事件消费，设置请求超时，并在订阅结束时取消；慢快照不能阻塞输出和终态事件。终态快照不能跳过尚未消费的事件、提前完成任务。

回归覆盖慢快照与订阅完成并发、输出先于完成、探测取消、代理恢复/steer/审批/停止、跨轮 task 查询，以及原生子代理可调用。配对生产链路使用本机临时 Core 验证，无真实远程机器或模型服务请求。

能力查询按 provider/model 查目录，思考等级（如 `:high`）是模型选项而不是目录键，查询前必须剥离，否则真实 Pi 目录会报“模型不在 Pi 当前可用目录中”。

使用 T3 Code 的工具使用边界作为对照，不把提示词对齐或局部修复称为整个调度架构等价。
