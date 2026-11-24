# 本地运行 Vantry

Vantry 的 NSH 桌面、持久化 Core 和内置 Pi worker 均使用 Rust。桌面关闭后，Core 可以继续运行任务。模型配置和凭据由任务所属机器提供。

## 从源码启动

安装 Rust 1.97 或更高版本、Git 和当前系统的原生编译工具链后，在仓库根目录执行：

```sh
cargo build --locked -p vantry-desktop -p vantry-service
target/debug/vantry desktop --workspace "$PWD"
```

`--mock` 可打开演示界面。`--attach-only` 让桌面连接已有 Core。要使用已配对的远端 Core，运行 `target/debug/vantry desktop --peer <ID> --peer-mode auto`。`relay` 模式可用于单独检查中继连接。

## 测试与打包

```sh
cargo test --locked --workspace
cargo run --locked -p xtask -- package desktop
```

macOS Apple Silicon 安装镜像生成在 `target/package-macos-arm64`。打开镜像并将 Vantry.app 拖入“应用程序”。更新前应让当前 Core 完成正在运行的任务。
