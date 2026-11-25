# Vantry 更新与 T3 退出条件

0.1.17 起，macOS 更新在后台暂存并校验新包，不会直接覆盖运行中的程序；0.1.28 起，支持纯 Go 二进制增量包（`.delta`）与设置内一键“重启并更新”。

## 使用流程

1. 在设置中检查更新。若 Release 提供匹配的 `vantry-<当前版本>-to-<最新版本>-<平台>.delta`，且下载量小于完整包，优先下载增量包。设置显示下载大小和节省的流量。增量校验或签名失败时自动回退到完整包。
2. 下载或增量重建完成后，先校验所有文件大小与 SHA-256（macOS 同步执行严格系统 `codesign --verify --deep --strict`），运行中的 `.app` 与 Core 均不会被中途改写。
3. 本机可写入安装目录时，下载按钮切换为 **“重启并更新”**。点击后，Vantry 先核对 Core 无活动任务，再以后台会话拉起新包的 `apply-update --wait-pid <桌面PID>` 并正常退出窗口。
4. `apply-update` 等待旧桌面进程退出；若仅剩空闲 Core，检查 Core 状态并请求 `shutdown-if-idle` 接口安全退场，确认全部目标进程退出后再原子替换 `.app` 并重新打开 Vantry。
5. 若目标目录不可写或通过远程环境访问，则保留暂存目录与 `安装更新.command`，退出 Vantry 后运行即可。活动任务、排队输入、审批或 Core 拒绝退出都会阻止安装，并保留已暂存目录；不会发送强制停止任务或强杀进程的信号。

下载失败、增量重建偏差或安装被拒绝均不会修改原应用。签名失败、归档越界、SHA-256 不匹配或下载不完整也会拒绝。

## 公开仓库与可选只读更新凭据

公开仓库的 GitHub Releases 可直接匿名检查更新与下载，无需配置凭据。

如果你在私有 Fork 中分发构建，更新器也支持可选的只读 GitHub Token（本机专用文件优先、随包凭据后备）：

- 本机默认：macOS 的 `~/Library/Application Support/Vantry/update-token`，可用 `VANTRY_UPDATE_TOKEN_FILE` 覆盖；权限为 0600。本机文件存在但格式或权限不正确时显示具体错误。
- 随包文件：macOS 的 `Vantry.app/Contents/Resources/update-token`；Windows/Linux 为可执行文件旁的 `update-token`。
- 凭据不进入 Git、日志或配对链接。若私有构建需要随包凭据，请仅授予目标仓库的 **Contents: Read-only** 与 **Metadata: Read-only** 权限，并在打包前以 0600 权限放入 `resources/update-token`（已加入 `.gitignore`）。

更新器忽略通用 `GITHUB_TOKEN`、`GH_TOKEN`、`gh auth token`、Git credential helper 和 SSH 身份，不会改动用户的开发用 `gh` 登录。每次点击“检查更新”都会重新读取凭据配置。

## 没有 Release 与认证错误

仓库可读、但 `releases/latest` 为 404 时，程序进一步检查仓库可见性，并报告“尚未发布正式 Release”，不会把它误报成密钥失效。仅私有仓库不可见时才提示检查只读凭据。

正式发布时，用上一稳定版和新版实际分发的桌面 ZIP 生成 macOS arm64、Windows amd64 的 `.delta`，与版本化完整包一起上传。`xtask make-delta` 会在输出补丁前重建并校验文件。其他来源版本仍可下载完整包。发布命令见[发布流程](../.agents/skills/vantry-release/SKILL.md)。

## 与独立 T3 Code 共存

Vantry 的会话连接和独立 T3 Code 是两套业务身份。T3 的历史数据库和工作树不会自动迁入 Vantry，可各自独立保留；如需卸载独立 T3 Code，请先确认所有进行中的运行已结束，并仅移除 T3 专用组件，避免影响共用的系统工具。
