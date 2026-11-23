# 本地启动与开发

Vantry 使用 Go、MyGo 原生桌面、SQLite 和 Unix domain socket。默认 Provider 已接入本机真实 Pi；Core、桌面窗口和每次真实执行的 worker 分属独立进程。关闭窗口只断开客户端；Core 重启后按原 Attempt 的持久事件游标接回执行，不重发已经提交的 prompt。`controlled` 仍用于离线演示和确定性测试。

本轮实际验证环境为 macOS / darwin-arm64、Go 1.27.3、MyGo v0.8.3。Linux 的平台构建与原生窗口尚未验收。

## 运行应用

本机已安装 0.1.3：

```sh
open /Applications/Vantry.app
```

源码打包后也可直接打开 `dist/darwin-arm64/Vantry.app`。输入区下方的“模型”按钮展开完整可用列表，顶部显示 Pi 默认模型的实际名称，列表中的默认项有标记。

Pi CLI 是本机外部依赖，使用它已有的模型与认证配置，不随应用复制凭据。应用优先在 PATH 中发现 Pi，也支持用户已有的 `~/.local/bin/pi`、Homebrew 安装；能为从 Finder 启动的进程找到 Pi 所用的 Node。默认模型读取 Pi 用户设置中的 `defaultProvider/defaultModel`，可在启动参数或界面的“执行设置”中覆盖。

首次使用时，在“执行设置”填入项目目录和模型，然后新建会话。设置用于新会话；已有线程沿用其持久 Provider、模型和工作区。真实执行必须有可用模型和工作区，缺失或准备失败会明确报错，不回落到其他目录。

默认模型目录来自 Pi RPC；新会话默认模型读取 Pi `settings.json` 的 `defaultProvider/defaultModel`，不恢复界面保存的上一模型，也不继承其他会话的手动选择。已有会话仍使用自己的模型配置，不要求存在 Magpie 应用配置。只有显式传入 `--magpie-settings PATH` 才限制为该文件中的 Pi 已选模型。当前 worker 仍禁用自动扩展和 MCP，扩展注册模型、命令与工具的完整兼容尚未完成；具体差距见 [行为与能力边界](behavior-gaps.md)。

从源码构建：

```sh
GOTOOLCHAIN=go1.27.3 go build -o bin/vantry ./cmd/vantry
./bin/vantry desktop --workspace "$PWD"
```

重新打包 `.app`：

```sh
GOTOOLCHAIN=go1.27.3 go tool mygo build -platform darwin/arm64 -skip-dmg
```

本轮已同步更新 `.app` 和 DMG；上述 `-skip-dmg` 适合快速开发重建，去掉该参数可重新生成安装映像。

## Core 与执行配置

开发时可在独立终端前台启动 Core，再附着窗口：

```sh
./bin/vantry core --data-dir /tmp/vantry-dev \
  --workspace "$PWD" --model PROVIDER/MODEL
```

```sh
./bin/vantry desktop --data-dir /tmp/vantry-dev --attach-only \
  --workspace "$PWD" --model PROVIDER/MODEL
```

未使用 `--attach-only` 时，桌面会附着已有 Core，或启动独立 Core。相同数据目录只允许一个 Core。默认数据目录为 `os.UserConfigDir()/Vantry`，macOS 对应 `~/Library/Application Support/Vantry`。目录必须属于当前用户、权限为 0700；socket 权限为 0600。

可通过 `VANTRY_DATA_DIR`、`VANTRY_PROVIDER`、`VANTRY_MODEL`、`VANTRY_PI_EXECUTABLE` 覆盖默认值。`--pi-executable` 可指定已有 Pi 路径。离线演示使用 `--provider controlled`。

前台 Core 用 Ctrl+C 停止。真实 Pi worker 会继续执行并保存结果，之后以相同 `--data-dir` 启动 Core 即可恢复交付。受控 Provider 没有外部恢复协议，旧活动运行仍标为 interrupted。显式 `ctl stop` 才会持久取消指定任务；关闭界面、停止 Core 和取消任务是不同操作。

## 原生任务编排

在父会话输入子任务指令并点击“派出子任务”，也可由父 Pi 通过 Vantry CLI 自动派发。子任务的配置默认继承父线程，Git 项目使用独立 worktree；显式指定的普通目录可以共享，写入执行按目录串行。

父会话中的任务区显示运行状态、结果和交付状态，可打开子会话、返回父任务、忽略结果，以及用当前草稿追问子任务。默认整组子任务终结后自动续行父任务；选中“手动确认结果”后，点击“确认结果并继续”才接收结果为父输入。忽略最后一个结果会结束等待，释放父会话的活动槽位。

追问沿用子线程及 Pi 会话，开启下一 generation。前一代结果尚未消费、或尚未成为持久父输入时，Core 会拒绝覆盖它。父任务续行保留原 Run，创建新的 Attempt，继承同一 Pi 会话。这里 acknowledged 表示 Core 已持久接受交付为输入，不表示模型已经理解结果。

运行中的父任务派发子任务后应结束当前 Provider 轮次，由 Core 保存等待与续行；父 Provider 尚未结算时，Core 不会启动第二个同线程 Pi。真实 Pi 的附加指令提供对应 CLI 用法，任务派发经过 Vantry Core。

默认 Cmd/Ctrl+Enter 发送，Enter 换行。会话草稿、选择和偏好保存在私有数据目录的 `desktop-state.json`，重开可恢复；未确认的客户端请求只保留在当前进程，不自动重发。已提交会话、队列、任务、工具、审批与交付状态保存在 Core。

运行中点击“排队”会持久保存消息，当前任务及父续行结束后按序执行；点击 Steer 给当前正在运行的 Pi 补充指令，界面收到真实回执后才显示“Pi 已接收”。停止任务会取消该线程尚未开始的队列。

“执行设置”可过滤和选择本机可用模型，并为新会话选择工具审批 `auto/ask`。`ask` 对 bash/write/edit 发起原生确认；未设置超时的请求保持可应答，显式超时使用 Pi 事件的原始时间，重连不会续期。工具卡片展示真实参数、状态和输出。“工作区”查看实际目录的未提交 Git 变更，“打开终端”启动 macOS Terminal。

## CLI

`ctl` 输出 JSON，`watch` 输出 NDJSON。`--data-dir` 放在 `ctl` 与具体操作之间：

```sh
./bin/vantry ctl --data-dir /tmp/vantry-dev health
./bin/vantry ctl --data-dir /tmp/vantry-dev create \
  --thread demo --title 编排测试 --workspace "$PWD" --command-id create-demo
./bin/vantry ctl --data-dir /tmp/vantry-dev send \
  --thread demo --message '分析这个项目，随后等待我派发独立检查。' --command-id send-demo
./bin/vantry ctl --data-dir /tmp/vantry-dev status
```

从发送回执取得父 `runId`，从快照取得当前 `attemptId`：

```sh
./bin/vantry ctl --data-dir /tmp/vantry-dev delegate \
  --parent-run PARENT_RUN_ID --parent-attempt PARENT_ATTEMPT_ID \
  --message '独立检查测试覆盖，并交回结论。' --resume-policy manual \
  --command-id delegate-review
```

可选 `--provider pi|controlled`、`--model PROVIDER/MODEL`、`--title`。Git 子任务默认独立 worktree；显式配置使用：

```sh
./bin/vantry ctl --data-dir /tmp/vantry-dev delegate \
  --parent-run PARENT_RUN_ID --message '执行独立检查。' \
  --workspace-kind worktree --repository /absolute/repository \
  --branch vantry/review --base-ref HEAD
```

从快照的 `tasks/cohorts` 取得稳定交付身份，再确认、忽略或追问：

```sh
./bin/vantry ctl --data-dir /tmp/vantry-dev ack \
  --parent-run PARENT_RUN_ID --generation 1 --delivery DELIVERY_MESSAGE_ID
./bin/vantry ctl --data-dir /tmp/vantry-dev dispose \
  --task TASK_ID --generation 1 --delivery DELIVERY_MESSAGE_ID
./bin/vantry ctl --data-dir /tmp/vantry-dev followup \
  --task TASK_ID --generation 1 --message '补查这个边界。'
./bin/vantry ctl --data-dir /tmp/vantry-dev stop --thread THREAD_ID --run RUN_ID
./bin/vantry ctl --data-dir /tmp/vantry-dev watch --after 0
```

父任务 Stop 会级联取消活动后代，并使未消费结果不再续行父任务。取消操作也有持久 Outbox，Core 在通知 worker 前崩溃或通知暂时失败，会在重启或后台重试中补发。

相同 commandId 与命令 JSON 返回原回执；同 ID 配不同内容返回 conflict。重复确认同一交付不会再创建父续行。早期 schema 1 的回执没有完整命令载荷：迁移保留历史，但无法核对的旧 ID 会拒绝重执行；请查询历史，避免用新 ID 盲目重发旧操作。

模型、队列、Steer、请求与工作区也可从 CLI 操作：

```sh
./bin/vantry ctl --data-dir /tmp/vantry-dev models
./bin/vantry ctl --data-dir /tmp/vantry-dev create \
  --thread approval-demo --workspace "$PWD" --approval ask
./bin/vantry ctl --data-dir /tmp/vantry-dev queue \
  --thread THREAD_ID --message '当前任务结束后执行的下一条' --command-id queue-once
./bin/vantry ctl --data-dir /tmp/vantry-dev cancel-input --input INPUT_ID
./bin/vantry ctl --data-dir /tmp/vantry-dev steer \
  --thread THREAD_ID --run RUN_ID --attempt ATTEMPT_ID --message '调整当前任务的要求'
./bin/vantry ctl --data-dir /tmp/vantry-dev respond --request REQUEST_ID --confirm
./bin/vantry ctl --data-dir /tmp/vantry-dev respond --request REQUEST_ID --confirm=false
./bin/vantry ctl --data-dir /tmp/vantry-dev respond --request REQUEST_ID --value '选项或文本'
./bin/vantry ctl --data-dir /tmp/vantry-dev respond --request REQUEST_ID --cancel
./bin/vantry ctl --data-dir /tmp/vantry-dev diff --thread THREAD_ID
./bin/vantry ctl --data-dir /tmp/vantry-dev terminal --thread THREAD_ID
```

上述四种 `respond` 示例是同一请求的不同回答方式，实际只选一种。request ID、input ID、Run/Attempt 身份从 `status` 快照读取。Core 接受控制命令仅表示已持久记录，实际投递结果看 `inputs/requests` 状态。

## 验证

```sh
go test -race ./... -timeout=60s
go vet ./...
go build -tags e2e -o bin/vantry-e2e ./cmd/vantry
python3 scripts/smoke_desktop.py \
  --binary bin/vantry-e2e --artifacts /tmp/vantry-smoke/native-smoke
python3 scripts/smoke_workbench.py \
  --binary bin/vantry-e2e --native --model PROVIDER/MODEL \
  --artifacts /tmp/vantry-smoke/workbench-native
python3 scripts/smoke_orchestration.py \
  --binary bin/vantry-e2e --model PROVIDER/MODEL \
  --artifacts /tmp/vantry-smoke/orchestration-real
python3 scripts/smoke_worker_recovery.py \
  --binary bin/vantry --model PROVIDER/MODEL \
  --artifacts /tmp/vantry-smoke/worker-recovery
```

`smoke_orchestration.py` 会调用真实模型，在临时 Git 仓库中操作真实原生窗口、关闭界面、SIGKILL Core、重启两次并检查结果与唯一续行。`smoke_worker_recovery.py` 在真实 Pi 的 detached bash 工具执行中 SIGKILL worker，检查进程清理、工作区释放和不重发 prompt。二者均不接触默认业务数据目录；成功时清理临时执行目录，失败时保留诊断现场。普通构建不包含 `MYGO_AUTOMATION` 插件。

`smoke_workbench.py --native` 使用真实 Pi 与原生窗口，验证模型目录、排队/取消、审批前不执行、Core 重启后审批、Steer、下一条执行、工具结果、diff 与第二次重启。省略 `--native` 可只通过 CLI/IPC 验收。
