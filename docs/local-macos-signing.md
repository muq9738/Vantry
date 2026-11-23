# macOS 本地证书签名指南

如果暂未使用 Apple Developer ID 签名与公证，可在构建机上使用自定义本地钥匙串签名身份（例如 `Vantry Local Signing`）保持稳定的代码签名身份，避免每次重建后重新授予屏幕录制与辅助功能权限。证书与私钥仅保留在构建机，目标机器只需接收签好的 `.app` / `.zip`，不需要私钥。后续切换到 Apple Developer ID 签名与公证时，系统可能会要求重新授予一次权限。

## 构建与签名

先构建未签名或 ad-hoc 签名的 App，再通过 `scripts/sign_local_macos.py` 调用原生 `codesign` 从内到外签署包内 Mach-O、嵌套 bundle 和主应用。脚本沿用包内已有 entitlements，使用本地证书时不请求苹果时间戳或公证。

```sh
python3 scripts/build_remote_helper.py --platform darwin/arm64
go tool mygo build -platform darwin/arm64 -skip-dmg -sign - -o dist/unsigned
python3 scripts/sign_local_macos.py dist/unsigned/darwin-arm64/Vantry.app \
  --certificate "$HOME/.config/vantry/signing/vantry-signing.pem" \
  --output dist/local-signed
```

全局 `mygo.json` 保留通用打包配置；需要固定签名身份时通过上述脚本显式指定本地证书，其他构建机无需拥有该身份。脚本不生成新证书、不改变信任设置、不导出私钥，也不上传或发布文件。若原生签名等待系统授权，脚本会结束命令并提示处理钥匙串；存在上次中断留下的 `.cstemp` 时请先重新构建。

产物为 `dist/local-signed/vantry-<版本>-local-arm64.zip`、公开证书和安装说明。将 zip 解压后把 `Vantry.app` 放入 `/Applications` 即可。本地自签名不是苹果公证，首次打开可能需要在系统设置“隐私与安全性”中手动允许；远程被控所需的屏幕录制和辅助功能由每台 Mac 的用户主动授权。

请勿将签名私钥或 PKCS#12 包提交到源码仓库、放入安装包或复制到目标机器。
