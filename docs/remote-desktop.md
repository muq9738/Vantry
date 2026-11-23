# 远程桌面：局域网与原生自建中继

远程桌面默认只在局域网内工作，不依赖公共服务器，也没有内置的私人 SSH 别名。跨网络访问通过使用者自建的原生 Go 中继（独立 `linkd` 或内置 `vantry-relay`）提供 HTTPS 信令与内置 TURN/WSS 转发；未配置中继时，不会探测、尝试或回退到任何外部服务器。

## 局域网优先与惰性中继回退

- 开启远程共享后，Core 会立即启动本地局域网桌面宿主；只有当已配对移动客户端在局域网直连协商失败后请求回退，或用户显式生成跨网访客共享码时，Core 才按需创建中继房间。因此即使未配置中继或中继暂时离线，也不影响局域网共享。
- 已配对的移动端客户端可直接复用当前电脑配对凭据访问已开启的远程桌面，由 Core 代理本机或中继房间并保留中继注册凭据；撤销设备授权、过期或断开移动端连接会立即关闭活跃媒体通道。
- 独立 `DESK-…` 共享码仍作为可选的访客分享方式保留，便于未配对的桌面端临时连接。

## 局域网连接

1. 两台电脑接入同一局域网。
2. 被控端在“远程桌面”页面主动打开“允许此电脑被控制”（无需任何中继配置）。
3. 根据本机系统提示完成授权；macOS 需要屏幕录制与辅助功能。
4. 被控端复制本次共享的 `vantry-lan://…` 连接地址，在观看端直接粘贴连接；`vantry-lan://` 链接总是直连，不会走中继。

`vantry-lan://…` 包含本次会话的访问凭据及用于连接目标的公开 TLS 证书。不要发到公共聊天或模型上下文。它不是账户、永久设备密钥或代码签名证书。

信令直接连接被控端自身的 TLS 监听，视频仍使用原生硬件 H.264/WebRTC，键鼠通过 DataChannel 返回。局域网模式不配置外部 STUN/TURN，不启动 SSH，也不会在连接失败后另选中继。关闭共享会关闭监听、断开会话并撤销此地址。

## 跨网络：原生 HTTPS / WSS 中继（当前架构）

跨网络共享使用使用者自建的 `linkd`（或内置 `vantry-relay`）桌面中继端点，详细部署与服务边界见 [native-connectivity.md](native-connectivity.md)。

- 跨网中继地址与 **设置 › 远程环境** 共用同一配置（远程桌面页只读显示，不再单独填写）；插件设置只保留该中继桌面模块的独立注册令牌。旧版本保存在插件里的 `relayURL` 仅在未配置远程环境中继时作为回退。共享端开启时局域网立即可用，中继房间仍按需惰性创建（私网非回环明文 `http://` 默认禁用，仅可通过 `VANTRY_ALLOW_INSECURE_LAN_RELAY=1` 或中继配置 `allowInsecurePrivateHTTP` 显式用于本地测试）。
- 共享端开启被控、通过真实屏幕录制与辅助功能检查后，中继签发单次可用的 `DESK-…` 桌面共享码及 `vantry://desktop#…` 链接；**会话配对码（`SESSION-…`）不能用于桌面共享**。
- 观看端输入 `DESK-…` 共享码或链接后，共享端界面会显示请求设备名，必须逐次明确点击允许本次观看与键鼠控制，中继才会开放信令转发与 TURN/WSS 媒体隧道；拒绝或观看端断开会立即关闭对应媒体隧道。
- 跨网媒体目前通过客户端进程内回环 TURN/TCP 入口，经 WSS 转发到中继内置的 Pion TURN（ICE 策略 `relay`，统计标记 `wss-tcp`）。受 TCP 队头阻塞影响，弱网延迟需结合实际 RTT/丢包测量，后续优先评估 ICE 直连 UDP 与 TURN/UDP，再以 TCP/WSS 兜底。

> [!NOTE]
> **已退役的历史 SSH / `relayrtc` / `coturn` 架构（仅作历史存档）**
> 早期版本的 `vantry-rtc://`、`ssh -W`、独立 `relayrtc/` 信令与外部 `coturn` 方案已退役（见 [native-connectivity.md](native-connectivity.md)）。若仍有遗留测试脚本或旧文档引用 `vantry-rtc://`，请以本文与 [native-connectivity.md](native-connectivity.md) 为准。

## 权限与安全边界

- macOS 上系统网络权限（允许本地网络访问与传入连接）与屏幕录制、辅助功能互相独立；局域网直连超时时，请检查两台电脑是否同网及系统防火墙是否放行 `Vantry.app`。
- 远程会话凭据与 API 不允许触发 `rd.*` 或 `plugins.*` 控制命令，远程桌面关闭共享后会立刻销毁会话房间与转发连接。
- 权限请求不会重置已有的辅助功能（Accessibility）授权。打开系统设置仅跳转对应面板；显式请求输入权限时仅通过 AX 注册当前 Core，并在异步授权提示期间只做状态探测，不会执行 TCC 重置。

## iOS video compatibility and live acceptance

The desktop plugin is pinned to revision `06e8a45`. On macOS it uses VideoToolbox Constrained High encoding with matching H.264 `640c33` negotiation; its reported SPS profile must match. This resolves the High-only offer that WebKit could not accept. Windows retains its existing High encoder configuration. The relay forwards the host's video configuration unchanged and requires no deployment change.

The installed signed Mac application subsequently passed a real iPhone simulator session through the configured HTTPS/WSS relay: explicit viewer approval, 1920 × 1080 video with decoded frame counts 3 → 33 → 61, and pointer/keyboard events received by a separate native Mac test window. The UI test passed in 53.170 seconds and removed all four temporary desktop pairings. The test share and capture/input fixtures were stopped; the simulator's conversation connection and all existing conversation authorizations were preserved. Local evidence is private and is not committed.
