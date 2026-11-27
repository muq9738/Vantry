# Vantry 更新与 T3 退出条件

0.1.17 起，macOS 更新在后台暂存并校验新包，不会直接覆盖运行中的程序；0.1.28 起，支持纯 Go 二进制增量包（`.delta`）与设置内一键“重启并更新”。

## 使用流程

1. 在设置中检查更新。若 Release 提供匹配的直连或跨版本增量包（`vantry-<旧版本>-to-<最新版本>-<平台>.delta`，或在多个版本间组合出更小的增量链），且下载量显著小于完整包，优先下载增量包；下载器支持中国大陆多镜像竞速、断流自动切换与 HTTP Range 断点续传。设置和更新弹窗实时显示下载大小、速度与节省的流量。增量校验或签名失败时自动回退到完整包。
2. 下载或增量重建完成后，先校验所有文件大小与 SHA-256（macOS 同步执行严格系统 `codesign --verify --deep --strict`），运行中的 `.app` 与 Core 均不会被中途改写。
3. 本机可写入安装目录时，下载按钮切换为 **“重启更新”**。点击后，桌面端向本机 Core 发起 `update.install`（携带当前桌面 PID 与 `--allow-busy-core`），拉起新包的 `apply-update --wait-pid <桌面PID>`，随即清理配对隧道并正常退出窗口。
4. `apply-update` 等待旧桌面进程退出；若本机 Core 空闲，请求 `shutdown-if-idle` 安全退场并一并升级；若本机 Core 正有进行中的任务或待审批项，则不中断任何运行中的任务，直接保留旧 Core 运行并原子替换 `.app` 后重新打开新版 Vantry（旧 Core 将在任务完成后的下次启动自动切换）。
5. 若目标目录不可写，则保留暂存目录与 `安装更新.command`，退出 Vantry 后运行即可；手动执行 `安装更新.command` 时仍要求所有活动任务结束。不会发送强制停止任务或强杀 Core 进程的信号。

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

正式发布时，使用 `xtask make-deltas` 并行针对近期多个稳定版本（跳表基线 `N-1, N-2, N-3, N-5, N-8`）生成并校验真正的后缀数组 bsdiff `.delta`（完全兼容 v1 协议），与版本化完整包一起上传，使跨版本升级可直接走单步增量包或极短增量链。发布命令见[发布流程](../.agents/skills/vantry-release/SKILL.md)。

## 与独立 T3 Code 共存

Vantry 的会话连接和独立 T3 Code 是两套业务身份。T3 的历史数据库和工作树不会自动迁入 Vantry，可各自独立保留；如需卸载独立 T3 Code，请先确认所有进行中的运行已结束，并仅移除 T3 专用组件，避免影响共用的系统工具。
