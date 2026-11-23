# macOS 桌面打包

Vantry 面向 Apple Silicon（macOS 13+）提供原生 `.app` 与 `.dmg` 打包；远程会话和桌面共享采用原生 Go 与独立配对码，见 [原生连接说明](native-connectivity.md)。本地固定签名证书流程见 [本地签名](local-macos-signing.md)。

## 更新器机制

应用内只下载、验签和暂存（支持二进制增量包 `.delta`），不替换运行中的程序；确认 Core 空闲后通过一键重启或安装命令原子替换并重开，详见 [更新流程与权限](updates.md)。

## 构建产物

- `dist/darwin-arm64/Vantry.app`：应用包，内含可执行文件、`libghostty-vt` 动态库及原生硬件 H.264 helper。
- `dist/local-signed/vantry-<版本>-local-arm64.zip`：使用本地证书签署的分发包。

包内自带可执行文件、PNG / ICNS 图标和当前第三方说明，运行不需要 Go 或 MyGo CLI。窗口会自动附着或启动独立 Core，沿用当前原型的数据目录与生命周期。构建产物在 `.gitignore` 的 `dist/` 下，未作为源文件提交。

通用构建配置默认使用 ad-hoc 签名。2026-11-17 起，本机与 Mac mini 的 0.1.13 安装已改用固定的 `Vantry Local Signing` 自签名身份；构建、分发与第三台机器的待办见[三台 Mac 共用本地签名](local-macos-signing.md)。这些本地包没有使用 Developer ID 或进行 Apple 公证。

## 可复现构建

MyGo CLI 作为 `go.mod` 的 tool 固定到已依赖的 MyGo `v0.8.3`，无需全局安装。插件与原生 helper 的构建说明见 [独立插件仓库](plugin-repositories.md)；运行已打包应用不需要 Go。

```sh
python3 scripts/build_remote_helper.py
GOTOOLCHAIN=go1.27.3 go tool mygo build -platform darwin/arm64
```

配置位于 [mygo.json](../mygo.json)：

- 主入口 `./cmd/vantry`。
- 名称 `Vantry`，标识符 `io.github.muq9738.vantry`，当前源码版本 `0.1.16`。
- 原生界面，无前端构建步骤；生产包不包含测试自动化插件。
- 网络不再打包 Node 桥接、alma-relay npm 包、bonjour-service 或 moejs。旧网络资源目录已从打包排除；官方远控描述随 Go 程序提供。
- 最低系统版本与当前 Go 构建的 Mach-O 最低系统版本一致，均为 macOS 13。
- 生成 app、ICNS、资源目录、签名和 DMG；没有配置上传、发布或自动更新。

仅快速重建 `.app` 时可添加 `-skip-dmg`；使用 `-o DIRECTORY` 可先在独立目录构建，再更新本机产物。

## 2026-11-17 MacBook 0.1.12 按需授权实际启用

构建源c271013；包含权限UI/显式API、按业务请求ID限定取消补偿、真实permission capability及平台文案。正式module v0.2.1，无replace，helper0.1.1。全仓test/vet、深层签名、helper签名及DMG验证通过；只读临时Core检查3项能力与授权状态，不发OS请求。

SQLite在线备份后正常退出旧97508/97519，启用Core35039；schema10，9线程/5run全部ID保留，IDE配对与插件设置保留。远控更新0.2.1，草稿0.1.0。本机正式screen granted/input denied，CanHost/Hostingfalse；等待用户在插件页主动点允许，自动请求数0。备份位于 本机私有工件目录，安装/验证工件同会话。主repo未推送，原0.1.11包保留可回退。

## 2026-11-17 MacBook 0.1.11 实际启用

合并MacBook与Studio两边历史到本地分支 `integration/macbook-0.1.11`，构建源为 `4583a8d`，主仓库未推送。生产源码树与Studio交接的06e1a5b一致；保留本机既有文档历史。正式require独立插件v0.2.0，无replace；本机从已发布module编译并签名helper到 `Contents/Resources/remote-desktop/vantry-remote-helper`。

全仓普通 `go test ./...`、vet、App/helper严格签名及DMG校验通过。临时打包Core验证schema10和两项skills/codemode能力，真实被控启动明确返回缺少辅助功能权限，没有绕过。SQLite在线备份、默认Core空闲后关闭旧App/Core并更新；新Core97508 ready，切换时9线程/5runs的ID全部保留。支持的插件API更新远控0.2.0并写本机sshConfigFile，草稿0.1.0不变。备份、原App、构建/验证/安装记录位于 本机私有工件目录；备份0700，敏感文件0600。

## 图标资源

原图保存为 `1254×1254` PNG，位于 [assets/branding/icon-original.png](../assets/branding/icon-original.png)，便于重新生成。

[prepare_icon.go](../scripts/prepare_icon.go) 仅将与画布外缘连通的近黑背景转为透明，保留白色圆角底、人物与紫色符号，缩放为 `1024×1024` 的 [resources/icon.png](../resources/icon.png)。MyGo 再生成应用包中的 `Contents/Resources/AppIcon.icns`。

```sh
go run -mod=readonly scripts/prepare_icon.go
```
- 包内可执行文件自动启动独立 Core，健康状态 ready；关闭桌面进程后 Core 仍可连接。
- 检查结束后关闭本次启动的桌面与 Core，没有访问默认业务数据目录。

实际结果与运行时图标保存在 本机私有验证记录（不随源码分发），包括 本机私有验证记录（不随源码分发）、本机私有验证记录（不随源码分发）、签名和 DMG 校验日志。Swift 与 Python 辅助文件仅用于读取本机运行信息，不属于 app 运行依赖。
