# 连接电脑和手机

## 两台 Vantry 电脑配对

在主机「设置 → 环境」生成并复制完整的 `vantry://connect#…` 配对链接，在另一台电脑的「连接另一台电脑」中粘贴。自动模式先尝试主机的局域网地址，优先当前网段的网卡。直连握手没有及时成功时，客户端再尝试主机登记的 Alma 中继。两条路径都使用邀请中的主机证书验证 TLS 连接。同一局域网内，中继离线也可以生成并兑换电脑邀请。重连时使用配对所得的设备凭据。主机开启设备批准时，在主机核对两端显示的验证码，再批准新设备。

启用电脑公网配对时，在主机 Core 的私有数据目录创建权限为 `0600` 的 `desktop-alma-relay.json`，将 `relay` 填为 Alma 中继的 HTTPS 基址，若中继要求主机注册凭据则填写 `registrationKey`。如有备用中继，在 `fallbacks` 数组中按顺序填写各自的 `relay` 和 `registrationKey`。Core 同时向它们注册。任一中继在线即可从公网兑换邀请，局域网内也可以直接配对。配对链接和设备身份都包含这些地址。重启 Core 后生成电脑配对链接。Core 会保存独立的电脑路由，已配对的电脑在主机重启后可以重连。手机继续使用「手机配对」中的邀请。

使用中继时，电脑会向已认证的主机刷新当前直连地址。回到同一局域网后，持续使用的连接池也会重新尝试直连。正在传输的请求可以完成，后续请求使用新选定的路径。

配对后，侧栏可按机器查看会话，命令在持有该会话的 Core 执行。屏幕共享使用独立的局域网链接。[远程桌面](remote-desktop.md)

### 用 Alma iOS 打开 Vantry 会话

本机 Vantry 桌面可以把一条已有会话分享给 Alma iOS。右键点侧栏里的会话，选择「用 Alma iOS 打开…」→「生成配对码」。也可以先选中会话，再打开「设置 → 环境 → Alma iOS → 手机配对…」。让 iPhone 与 Mac 连到同一局域网，在 Alma iOS 的添加连接入口扫描二维码。手机发送的文本进入所选 Vantry 会话，由该会话的 Provider 执行。Alma 桌面可以同时运行。

手机使用蜂窝网络时，在配对窗口展开中继设置，填写手机和电脑都能访问的 Alma 兼容 HTTPS 中继及其要求的注册凭据。若在反向代理上按路径部署中继，主机从配置的前缀访问 `/agent`，Alma iOS 会从同一域名和端口的根路径访问 `/c/<路由>/__alma_e2e__`。两者都需要转发 WebSocket 升级。使用公网邀请首次配对时，手机从已加密的 `/__alma_lan__` 读取主机当前网卡地址和监听端口。原配对的连接码在直连和中继上通用。已保存局域网地址的客户端可以从 Wi-Fi 直接重连，离开局域网后可使用公网入口。手机从加密连接内的健康检查和设备设置读取主机名 `vantry`。中继完成登记后，界面会显示二维码。Vantry 会显示通过加密握手的手机连接数。关闭配对窗口后共享继续生效，Core 重启会恢复原配对。在已有共享中选「重新配对…」可填写公网中继。新码在注册成功后生效，旧码和旧连接随即失效，会话和历史消息保留。「停止共享」会断开连接并撤销旧码。要改为分享另一条会话，先停止当前共享。

在 Alma 会话标题中点模型名，可以选择这台 Core 已配置的 Pi 模型。选择会保存到共享线程，并用于下一次发送，重新连接后仍然有效。普通 Gemini 模型使用 Pi 的系统提示词、对话历史和工具。只有 Gemini Web 入口逐字转发本轮用户输入，包括绑定网页的专用写作线程和显式配置的 `gemini-web`、`gemini-web-bridge` 后端。专用写作线程只显示「Gemini · Neon 高」。

专用写作线程需要主机提供本地 Node 适配器。将 `gemini-writing.json` 放在该 Core 的私有数据目录，填写线程 ID（`threadId`）、Node 与适配器的绝对路径（`node`、`adapter`），以及交付方式（合成稿用 `synthetic`，Drive 归档用 `drive`）。通过 `vantry-service ctl create` 创建同一 ID 的线程，并指定 `--provider gemini-writing --model gemini-writing/neon-high`。手机重连后可在原线程发送「取回上次写作」。

### Alma 手机接入联调

已有 Alma 配对的 iPhone 可以直接核对真机版本和连接状态。在 Alma 桌面的远程访问页查看中继和手机在线状态。记录承接连接的桌面服务，验收 Vantry 收发时再核对所选 Core 线程的运行记录。

以下参数适用于从 `v0.2.17` 基线加入 Alma 接入改动后构建的 Vantry Service。它主动连接 Alma 兼容中继，只向持有配对码的客户端开放指定 Core 线程。真机联调使用隔离的 Core 数据目录、已有的测试线程，以及手机能访问的 HTTPS 中继。启动该 Core 时传入两个参数。

```sh
vantry-service core --data-dir "$TEST_DATA" --provider controlled \
  --alma-thread "$TEST_THREAD" --alma-relay "$TEST_RELAY"
```

中继开启注册鉴权时，将注册凭据存入该私有数据目录中的 `alma-registration-key` 文件，并限制为本人可读。在电脑上打开同目录的 `alma-pairing.svg` 供手机扫码。`alma-pairing.json` 和二维码均是配对凭据。相同线程与中继可复用配对。更换共享线程、中继或使用启动参数 `--alma-reset-pairing` 会生成新配对。

此命令行入口支持中继协议 1、端到端加密、线程及分页历史、文本发送和流式回复。WebSocket 写入复用 Core 命令去重，重连后可从同一线程读取已提交内容。从 v0.2.22 开始，命令行共享和桌面配对都支持使用同一配对码在局域网直连。原版 iOS 的屏幕显示与实际换网行为需在真机验收。

回环互测可使用 Node 22 或以上版本、已编译的 Service 和已安装运行依赖的官方 `alma-relay` 1.5.0 检出目录。测试自行创建数据目录、测试线程和临时中继，并在结束后清理。[官方中继](https://github.com/yetone/alma-relay)

```sh
cargo build --locked -p vantry-service
node apps/service/tests/alma_interop.mjs target/debug/vantry-service /path/to/alma-relay
```

桌面重新配对、切换模型和消息原文可用同一隔离环境验证。

```sh
node apps/service/tests/alma_desktop_pairing.mjs target/debug/vantry-service /path/to/alma-relay
node apps/service/tests/alma_writing.mjs target/debug/vantry-service /path/to/alma-relay
```
