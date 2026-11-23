# 插件开发与独立仓库

Vantry 主应用负责会话、Agent 编排、工作区和机器连接。插件通过原生页面声明或显式进程接口扩展能力，各插件在自己的仓库中维护源码、版本和许可。

## 远程桌面插件

[vantry-plugin-remote-desktop](https://github.com/muq9738/vantry-plugin-remote-desktop) 提供远程桌面引擎、原生 H.264 helper 和键鼠控制接口。Vantry 提供插件页面与宿主接线，随应用交付配套组件；用户通过插件入口使用、启用或停用远程桌面。

桌面共享与会话访问使用独立的配对和授权。插件不会在安装或启动应用时自动开启共享，也不预设任何中继域名。局域网可以直连；跨网使用用户自行配置的中继，连接由共享端逐次确认。

| 部分 | 维护与更新方式 |
| --- | --- |
| 插件引擎 | 独立 Go 模块，版本以主应用 `go.mod` 中的依赖为准。 |
| 原生 helper | 从配套插件模块构建，放入对应平台资源目录，随应用打包。 |
| 页面与设置 | 由宿主插件接口提供，保存在插件自己的数据范围内。 |
| 发布与许可 | 由插件仓库单独维护，不因主应用采用 MIT 而改变。 |

当前原生引擎与宿主一起编译，更新引擎或 helper 需要重新构建应用。普通用户运行发行版不需要 Go，也不需要访问插件源码仓库。

## 源码构建前置条件

远程桌面插件仓库（[vantry-plugin-remote-desktop](https://github.com/muq9738/vantry-plugin-remote-desktop)）作为公开 Go 模块维护，普通源码构建直接运行 `go mod download` 即可拉取，无需配置私有仓库凭据或 `GOPRIVATE`。

```sh
go mod download
```

依赖版本由 `go.mod` 选择，无需在文档命令中重复指定版本。不要将凭据写入源码、插件清单或构建输出。

### macOS

需要 macOS、Xcode Command Line Tools、Go 和 Python 3：

```sh
python3 scripts/build_remote_helper.py --platform darwin/arm64
go tool mygo build -platform darwin/arm64 -skip-dmg
```

构建脚本从依赖模块读取 helper 源码，输出至 `resources/darwin-arm64/remote-desktop/`。生成物不作为另一份插件源码维护，打包时由 MyGo 放入应用资源目录。

### Windows

在 Windows 上构建 helper 需要 MSVC、Windows SDK 和 CMake：

```powershell
python scripts/build_remote_helper.py --platform windows/amd64
go tool mygo build -platform windows/amd64 -skip-installer
```

在其他平台交叉打包时，使用已经构建好的匹配架构 helper：

```sh
python3 scripts/build_remote_helper.py --platform windows/amd64 \
  --helper-file /absolute/path/vantry-remote-helper.exe
go tool mygo build -platform windows/amd64 -skip-installer
```

Windows 的源码可用情况、硬件要求和验证边界见 [Windows 远程桌面说明](windows-remote-desktop.md)。

## 其他插件

独立插件使用 `owner/repo` 作为来源，各自发布 tag、Release 和安装包。需要访问私有插件时，使用 Core 所在机器已有的授权；安装来源用于检查更新。卸载只作用于本机安装副本，不会删除远端仓库。

插件实现在独立仓库维护；主仓库维护插件接口与宿主集成。
