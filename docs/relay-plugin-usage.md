# SSH 远程连接与命令插件

本文记录当前可运行的行为。`docs/native-plugin-architecture.md` 中的完整远程中继、外部进程插件等仍是目标设计；不能据此推断已经实现。

## SSH 连接

远端需要先独立运行 Vantry Core，并让 SSH 登录用户能够访问它的私有 Unix socket。例如，在远端由服务管理器或持久终端会话运行：

```sh
vantry core --data-dir /absolute/path/to/vantry-data --provider pi
```

在本机使用已配置并完成主机信任的 SSH 主机名：

```sh
vantry desktop --remote my-ssh-host --remote-socket /absolute/path/to/vantry-data/core.sock
```

连接器使用本机 OpenSSH 的 `-N -T -L` 转发到私有本地 Unix socket，临时目录权限为 `0700`。远端路径必须是绝对路径，不能含冒号、换行或 NUL。它不会启动远端 Core；Core 必须拥有独立于这条 SSH 连接的生命周期。

单次连接使用 `BatchMode=yes` 和 `StrictHostKeyChecking=yes`，不接受交互密码或自动建立新的主机信任。为准确管理进程，连接器在本次命令中关闭 SSH 连接复用和认证后后台化；不修改用户 SSH 配置文件。健康探测最长等待 12 秒，调用者可用更短的 context 截止时间。SSH 首次启动失败或退出会立即返回具体原因；存活的 SSH 无法打开远端 socket 时，连接超时错误同时包含最近的 HTTP 探测结果和 SSH stderr 尾部。

连接成功后，SSH 意外退出会每隔一秒重新启动；正常退出码 `0` 也视为转发意外结束。SSH keepalive 配置为 15 秒、连续 3 次无响应后断开，故网络黑洞的发现并不即时。连接器保留最多 4 KiB 的 stderr 尾部用于诊断，避免持续错误输出无限占用内存。该诊断只保存在当前进程内，不写入磁盘日志。

`Connect(ctx, host, socket)` 的 `ctx` 管理整个连接生命周期。取消它或调用 `Relay.Close()` 会取消本地 HTTP 请求与订阅、终止并等待本地 SSH 子进程、清理转发目录和 socket。`Close()` 可重复或并发调用。远端没有收到 `run.stop` 或停止 Core 的请求；已经提交给独立 Core 的任务继续由 Core 执行。

### 请求与订阅恢复

SSH 重启恢复的是连接能力。已经失败的 HTTP 请求和订阅不会在适配器内部重发：

1. 客户端保存最后已消费的事件 `Sequence`。
2. 流断开后，等待健康探测恢复，再调用 `Subscribe(ctx, lastSequence)` 补放并接续实时事件。
3. 若收到 `resync_required`，重新获取快照再订阅。
4. 写命令响应不确定时，只能以原 `CommandID` 和原请求内容重试，不能生成新 ID 重新提交同一工作。

当前 Core 的事件日志尚不清理，且对未来游标尚未做拒绝校验。本轮测试验证正常游标补放、实时接续、非法负游标错误和命令幂等；没有据此宣称完成过期游标恢复协议。

`Relay.LastError() error` 提供最近一次 SSH 启动/退出异常；重连后仍保留历史错误，应结合 `Health` 判断当前可达状态。`Relay.Done() <-chan struct{}` 在子进程已回收并完成清理尝试后关闭；清理失败可通过 `Close()` 的返回值读取。

### 与公网中继的差距

这是已有 SSH 登录能力之上的 socket 转发。本段记录早期 SSH 实现；设备配对、网关与 TLS 的当前接线见 [原生连接说明](native-connectivity.md)。传输加密与主机认证由 OpenSSH 提供；SSH socket 转发与应用层中继协议分别验收。

## 安装并运行独立命令插件

独立进程/命令插件在各自的 GitHub 仓库中维护。在插件中心输入插件仓库地址（`owner/repo`）即可安装，插件保存在 **运行 Core 的机器** 的 `<data-dir>/plugins/<id>/`。远程环境使用远端 Core 的插件目录，安装不会自动执行插件。仓库及更新约定见 [独立插件仓库](plugin-repositories.md)。

安装后可使用已选数据目录显式运行：

```sh
VANTRY_DATA_DIR=/absolute/path/to/vantry-data
vantry ctl --data-dir "$VANTRY_DATA_DIR" plugins
vantry ctl --data-dir "$VANTRY_DATA_DIR" plugin-run --plugin draft-cleanup --message '{"title":"本周草稿","body":"第一段。\n\n第二段。","tags":["记录","记录"],"checklist":[{"text":"复核内容"}]}'
```

桌面中可将插件仓库 `input.json` 的内容粘贴到输入框，然后在插件面板刷新列表，显式运行“整理 JSON 草稿”。无需将这份草稿发送给模型。插件返回标准化的 `draft`、可复制的 `markdown`、段落/标签/待办统计及修正提示。运行不会写回草稿、发送消息或读写文件；输出是否加入输入框由用户操作决定。

示例接受两种现有调用形式：

- 直接 API 输入：`{"title":"标题","body":"正文","tags":[],"checklist":[]}`。
- 当前桌面/CLI 输入：`{"message":"上述草稿的 JSON 字符串","threadId":"...","title":"..."}`。插件解析 `message`，其中草稿自己的标题优先；外层会话标题与 ID 不参与修改。

`body` 必须是字符串；其余字段可省略。示例整理换行和末尾空白、去除重复/空白标签、移除空白待办。缺少标题会使用“未命名草稿”。不合法 JSON 或字段类型错误会返回具体错误。

### 单文件插件合同

```json
{
  "id": "draft-cleanup",
  "name": "整理 JSON 草稿",
  "description": "整理 JSON 草稿并返回 Markdown",
  "entry": "index.ts"
}
```

`id` 必须与目录名相同，`name` 不能为空白，`entry` 必须是当前目录内的普通 `.js` 或 `.ts` 文件。插件目录、配置和入口不支持符号链接。配置最多 64 KiB，源码最多 1 MiB；读取在分配完整文件内容前受大小限制。没有 `plugin.json`、坏 JSON、错误 ID、缺失入口和编译失败都会保留在 `plugins.list` 结果中，通过现有 `PluginInfo.Error` 显示；一个坏插件不会隐藏其他插件。

入口导出 `run(input)`。列表检查配置、入口、编译、静态模块导入和 `run` 导出名，不执行顶层代码；运行时才检查 `run` 是否可调用及顶层初始化是否成功。TS 支持取决于 moejs 的类型擦除能力，不运行 TypeScript 类型检查，不提供 Node/npm 模块解析，也不安装 `node:fs`、进程或网络 API。需要运行时模块导入的插件会明确失败。

每次调用创建全新的 runtime。模块全局变量和 `globalThis` 状态不跨调用共享；共享内建原型不可修改。输入为空时为 `{}`，否则必须是至多 1 MiB 的合法 JSON。字符串结果直接返回，其他结果按 moejs 的 JSON 序列化语义输出紧凑 JSON；结果最多 1 MiB，顶层 `undefined` 会报错。紧凑输出避免深层缩进在 runtime 外放大内存分配。

调用期限最多两秒，覆盖发现后的编译、模块初始化、`run`、微任务和结果序列化；`Host.Run` 的预算从发现开始计时，在可取消的阶段观察该截止时间。宿主通过 `CompileWith` 的 context 中止编译，通过 `Runtime.Interrupt` 中止执行，并等待中断回调结束再释放调用数据。调用者的更短截止时间或取消同样生效；Go 调用方可以用 `errors.Is` 区分 `context.Canceled` 与 `context.DeadlineExceeded`。

moejs runtime 的内存预算为 16 MiB，超限返回可由 `errors.Is(err, moejs.ErrMemoryLimit)` 识别的错误。这个预算是语言运行时的近似分配/存活量限制，**不是整个 Go 进程的硬性 RSS 上限**，也不覆盖编译器内存。文件大小和编译期限提供另外的边界；这里没有 OS worker 进程隔离。

### 异步语义

`run` 必须同步返回结果。原生 Promise（包括已经 fulfilled 的 Promise 和 `async function` 的返回值）与带可调用 `then` 的返回对象都会明确报错，不会被当成空对象或成功结果。顶层仍在等待的 Promise 也报错。moejs 会在调用返回前处理已有微任务；其中的无限循环可被取消，未处理的 Promise 拒绝会报告原因。已捕获的拒绝不使同步结果失败。此宿主不安装事件循环、定时器、fetch 或后台工作能力。

## 接线合同与剩余工作

公开 API DTO 保持不变。父会话可做以下接线：

- `internal/app/workbench.go` 中的 `plugins.list` 改为 `Host.ListContext(ctx)`，让请求取消传入发现和编译。原 `List()` 继续兼容，但没有调用者 context。
- 桌面远程窗口启动层目前只检查 `command.Start()`，后台丢弃 `command.Wait()` 错误和 stderr；需要接收并显示远程窗口的连接失败。适配器的详细错误已通过 `Connect` 返回。
- 远程客户端的连接错误提示可结合 `LastError()` 与 `Health`；`Done()` 用于生命周期收尾，不是单次断线通知。订阅恢复仍使用现有客户端快照/游标流程。

本轮不增加插件权限平台、自动执行钩子、后台插件服务或跨进程插件执行器。

## 验证

```sh
go test ./internal/adapters/transport/sshrelay ./internal/plugins
go test -race ./internal/adapters/transport/sshrelay ./internal/plugins
```

SSH 测试用 `/tmp` 下的短路径私有目录、真实 SQLite Core/UDS HTTP 服务和受控 SSH 替身。替身使用真实 socket 双向复制，在独立子进程内模拟转发、认证失败、退出和挂起；PATH 中没有真实 SSH 回退。测试覆盖断开期间任务完成、重连失败后的再次恢复、事件游标补放和实时接续、重复命令、取消、并发关闭及 socket 清理。未连接真实 SSH 账号，也未调用模型。

插件测试覆盖正常与坏配置、大小限制/符号链接、编译与运行错误、Promise 行为、默认超时、编译/初始化/执行/序列化取消、内存预算、并发调用状态隔离、默认能力缺失，以及把示例复制到临时安装目录后分别运行原始 JSON 和桌面/CLI 输入格式。
