# Windows 远程桌面实现与验证

Windows 远程桌面实现（预览）已合入主分支与公开插件仓库 [vantry-plugin-remote-desktop](https://github.com/muq9738/vantry-plugin-remote-desktop)。

## 平台交互

权限面板共用按需状态机，系统动作按平台区分。Windows普通DXGI共享和SendInput没有macOS TCC全局权限页；无需授权不代表原生媒体组件可用。Probe还检查当前交互桌面及实际硬件编解码能力，缺组件、缺视频硬件或锁屏均保持 `canHost:false`。不自动提权、不切换安全桌面、不在安装或启动时共享。

SDK媒体helper为DXGI/D3D11捕获、GPU NV12转换、Media Foundation硬件H.264编码和DXVA解码；只有实际合成编解码验证后才报告hardware。显示器ID来自DISPLAYn，输入和捕获采用同一物理像素坐标。原生输入使用SendInput，翻译公开Mac键码/CGFlags，正确处理Unicode、扩展键、左右修饰键及释放。Windows系统负责双击判定。

子进程为后台窗口；helper与SSH不会在普通探测时弹控制台。共享开关的有效意图不跨页或重启保留，取消补偿按业务请求ID限定。

## 应用系统层

Windows私有目录校验当前用户SID和DACL，文件打开防重解析点替换，文件锁使用LockFileEx。项目/Git/Pi/插件执行归属自有Job Object；工作区级Job保护并发，嵌套attempt级Job区分孤儿结算与后来的合法写入者。Windows npm的pi.cmd被解析为直接node.exe+CLI脚本，模型目录、技能查询与worker使用相同规则，不拼接cmd.exe命令。

Mac抽取分支继续使用原flock、Setpgid/Setsid、继承锁描述符和SIGTERM。完成移植后本机完整普通Go测试1426项及vet通过，Windows主程序、平台测试二进制交编通过。Windows内核ACL/Job运行测试没有执行，不能把交编当作运行结果。

## 构建与资源

每个平台的helper放入MyGo平台资源目录，避免Mac的Mach-O文件进入Windows包：

- macOS：`resources/darwin-arm64/remote-desktop/vantry-remote-helper`，打包到Contents/Resources/remote-desktop。
- Windows：`resources/windows-amd64/remote-desktop/vantry-remote-helper.exe`，打包到Vantry.exe旁的remote-desktop目录。

```sh
# Mac构建helper与App
python3 scripts/build_remote_helper.py --platform darwin/arm64
go tool mygo build -platform darwin/arm64

# Mac用已验证Windows CI产物交叉打包；不构建或签署Windows内核能力
python3 scripts/build_remote_helper.py --platform windows/amd64 --helper-file /absolute/path/vantry-remote-helper.exe
go tool mygo build -platform windows/amd64 -skip-installer -o /absolute/preview/output
```

Windows开发机可直接调用同一脚本，用已有MSVC/Windows SDK和CMake编译正式插件模块的native/windows。builder校验PE机器类型，Mac构建签名；构建不请求系统权限。

## 当前证据与缺口

- 主Go程序实际生成Windows/amd64 PE，MyGo完整GUI打包成功，含Windows helper、ghostty-vt.dll及远程连接资源。
- 插件Windows输入CI已运行纯编码/取消/2100次只读显示器回调复用/Unicode与普通按键混合释放测试，未发真实输入。
- Windows媒体CI44034373302由MSVC/SDK构建helper，协议和验收工具测试通过；`captureInvoked:false`。
- 同一CI的hardwareEncode/hardwareDecode为false，状态HARDWARE_SKIPPED，不能宣称硬件媒体成功。没有软件回退。
- 两个已连接环境都是Mac，没有已连接Windows GPU会话或私有GPU runner。真实Windows捕获、混合DPI/旋转/光标、硬编硬解、输入/UIPI及延迟仍需实机验收。
- Windows 远程 IDE/Agent 的运行行为尚未在 Windows 真机完整验证。

macOS 的按需权限流程说明见 [远程桌面](remote-desktop.md)。Windows 不以 macOS 结果替代真机验证。
