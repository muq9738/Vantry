# 工作区与 Git 环境边界

本记录说明 Vantry 为任务准备隔离工作目录时的边界合同，以及本轮（DeepSeek 实现路线）补齐的验收点。范围只涉及 `internal/adapters/workspace/`、`internal/adapters/git/command.go` 和 `internal/adapters/provider/pi/runtime.go` 及其测试。

## 边界合同

`KindWorktree` 的准备和打开都不能落到主 checkout、别的项目或别的分支上：

- 来源仓库必须是它自己的顶层 checkout。子目录、软链接到主 checkout 的路径都会在规范化阶段被解析并拒绝。
- `Path` 不能等于 `Repository`；软链接路径解析后再比较，不接受“换个写法绕过”。
- 已存在且归属未确认的目录只报错，不删除、不改写、不擅自采纳。
- 已准备目录必须是来源仓库的 linked worktree：`--absolute-git-dir` 与 `--git-common-dir` 不同，且 common dir 与来源仓库一致。因此主 checkout、其他项目的 worktree、`git init` 出来的独立仓库都会被拒绝。
- 分支必须与记录一致；detached HEAD 视为不匹配。
- 来源仓库为另一个 linked worktree 时允许（Git 语义上仍是同一个仓库），但结果仍固定在该 common dir，不会回到主 checkout。

`Prepare` 与 `Existing` 使用同一套校验：

- 新建准备由 `canonicalRepository` 校验来源为顶层 checkout。
- 历史 `prepared.json` 经 `verifyPrepared` 走同样的校验（`canonicalRepository` + `verifyWorktree`）。不一致的记录返回错误，但不会批量改写、删除或“修复”历史记录。
- `Existing` 不创建目录、不写状态；准备记录缺失或不可用时返回错误，绝不退回到来源仓库。

## Git 环境过滤

启动 Vantry 的宿主（Finder、Core 父进程、脚本）可能带着 `GIT_DIR`、`GIT_WORK_TREE`、`GIT_INDEX_FILE` 等变量。这些变量会让 `git -C` 指向另一个仓库或 index，因此：

- `workspace.Environment()` 只剔除仓库路由变量，保留用户 hooks、凭据、SSH 命令、签名和身份配置。
- `workspace.GitEnvironment()` 在过滤结果上追加 `GIT_TERMINAL_PROMPT=0`、`GIT_MERGE_AUTOEDIT=no`，供 Vantry 自己执行的 Git 命令使用（`workspace.runGit`、`branchExists`、`internal/adapters/git` 的全部命令）。
- Pi 子进程复用 `workspace.Environment()`：只去掉路由变量，不额外改变 Pi 自己的 Git 交互行为（Pi 仍可按用户配置提示凭据），也继续保留 Finder 场景下按可执行文件位置查找 Node 并前置 `PATH` 的行为。

没有通过禁用 hooks、签名或凭据来让测试通过。

## 验收

测试全部使用临时仓库和注入环境变量，没有触碰真实仓库、SSH 或模型调用。

- `TestEnvironmentDropsRepositoryRoutingAndKeepsUserConfiguration`：过滤项与保留项的正反断言。
- `TestPrepareIgnoresInheritedRepositoryRouting`：注入指向诱饵仓库的 `GIT_DIR`/`GIT_WORK_TREE`/`GIT_INDEX_FILE`/`GIT_COMMON_DIR` 后，准备结果仍属于来源仓库，诱饵仓库没有分支和 worktree，未提交文件在重复准备与 `Existing` 后保留，来源 checkout 保持 clean。
- `TestWorktreePreparationRejectsPrimaryCheckoutAndSubdirectories`：主 checkout、软链接到主 checkout、来源为子目录三种请求都被 `Prepare` 和 `Existing` 拒绝，且不产生就绪记录、不改动来源文件。
- `TestPrepareFromLinkedWorktreeStaysInTheSameRepository`：以 linked worktree 为来源时仍落在同一 common dir，分支正确。
- `TestHistoricalPreparedRecordsAreRejectedWithoutBeingRewritten`：主 checkout、其他项目的 worktree、错误分支、来源为子目录的历史记录都被拒绝，`prepared.json` 字节不变，未提交文件保留。
- `TestHistoricalRecordPathMustMatchItsIntent`：记录路径与意图不一致时拒绝。
- `TestCommandIgnoresInheritedRepositoryRouting`（git 包）：注入路由变量后 `git` 命令、`Inspect`、提交仍作用于指定 checkout，诱饵仓库不受影响，pre-commit hook 照常执行。
- `TestRuntimeEnvironmentDropsLauncherRepositoryRouting`（pi 包）：Pi 子进程实际运行时看不到路由变量，仍能看到用户身份配置，`PATH` 仍包含 Finder 场景解析到的 Node 目录。

执行结果（darwin/arm64）：`go test ./internal/adapters/workspace ./internal/adapters/git ./internal/adapters/provider/pi` 全通过；上述重点用例在 `-race` 下通过；`go build ./...`、`go vet` 同三包通过。

## 范围外观察（未改动）

- `internal/app`、CLI/transport 的整合由父会话负责，本记录不覆盖。
- Core 的 durable steering 只做只读检查，未发现需要在本路线内修改的问题。
