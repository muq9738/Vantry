#!/usr/bin/env python3
"""Sign a Vantry .app with an existing local code-signing identity and zip it.

Build first with MyGo's ad-hoc signer, then run this script. It reuses the
identity already in Keychain; it never generates certificates, exports private
keys, changes trust settings, or publishes anything. Only the .app, a public
certificate (when supplied), and installation notes go into the distribution.
"""
import argparse
import os
from pathlib import Path
import plistlib
import shutil
import signal
import subprocess
import sys

MACHO = {b"\xfe\xed\xfa\xce", b"\xce\xfa\xed\xfe", b"\xfe\xed\xfa\xcf", b"\xcf\xfa\xed\xfe", b"\xca\xfe\xba\xbe", b"\xbe\xba\xfe\xca", b"\xca\xfe\xba\xbf", b"\xbf\xba\xfe\xca"}
BUNDLES = {".app", ".framework", ".bundle", ".xpc", ".appex", ".plugin"}


def run(args, timeout=30):
    process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, start_new_session=True)
    try:
        output, _ = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.communicate(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
        raise RuntimeError("Command timed out and exited. Complete any macOS Keychain authorization, then retry: " + args[0])
    if process.returncode:
        raise RuntimeError(output.strip() or "Command failed: " + args[0])
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("app", type=Path, help="freshly built Vantry.app; it will be signed in place")
    parser.add_argument("--identity", default="Vantry Local Signing")
    parser.add_argument("--certificate", type=Path, help="public PEM certificate only; optional copy beside the archive")
    parser.add_argument("--output", type=Path, default=Path("dist/local-signed"))
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise RuntimeError("Signing requires macOS")
    app = args.app.resolve()
    if app.name != "Vantry.app" or not (app / "Contents/Info.plist").is_file():
        raise RuntimeError("Pass a built Vantry.app")
    if str(app).startswith("/Applications/"):
        raise RuntimeError("Sign a build copy before installing it in Applications")
    if next(app.rglob("*.cstemp"), None) is not None:
        raise RuntimeError("An interrupted signing left temporary files in the bundle; build a fresh copy before signing")
    certificate = args.certificate.expanduser().resolve() if args.certificate else None
    if certificate:
        public = certificate.read_text()
        if "PRIVATE KEY" in public or "BEGIN CERTIFICATE" not in public:
            raise RuntimeError("--certificate accepts a public PEM certificate only")
    with (app / "Contents/Info.plist").open("rb") as stream:
        info = plistlib.load(stream)
    resources = app / "Contents/Resources"
    native = []
    containers = []
    for path in resources.rglob("*"):
        if path.is_symlink():
            continue
        if path.is_file():
            with path.open("rb") as stream:
                if stream.read(4) in MACHO:
                    native.append(path)
        elif path.is_dir() and path.suffix in BUNDLES:
            containers.append(path)
    for path in sorted(native, key=lambda p: len(p.parts), reverse=True):
        run(["codesign", "--force", "--sign", args.identity, "--timestamp=none", "--preserve-metadata=entitlements", str(path)])
    for path in sorted(containers, key=lambda p: len(p.parts), reverse=True):
        if any(path in binary.parents for binary in native):
            run(["codesign", "--force", "--sign", args.identity, "--timestamp=none", "--preserve-metadata=entitlements", str(path)])
    run(["codesign", "--force", "--sign", args.identity, "--timestamp=none", "--preserve-metadata=entitlements", str(app)])
    run(["codesign", "--verify", "--deep", "--strict", str(app)])
    for path in [app, *native]:
        details = run(["codesign", "--display", "--verbose=2", str(path)])
        if "Authority=" + args.identity not in details.splitlines():
            raise RuntimeError("A bundled executable did not receive the requested signing identity: " + str(path))
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    version = info.get("CFBundleShortVersionString", "local")
    binary = app / "Contents/MacOS" / info["CFBundleExecutable"]
    architecture = run(["lipo", "-archs", str(binary)]).strip().replace(" ", "-")
    archive = output / f"vantry-{version}-local-{architecture}.zip"
    run(["ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", str(app), str(archive)], timeout=60)
    if certificate:
        shutil.copyfile(certificate, output / "Vantry-Local-Signing.pem")
    (output / "安装说明.txt").write_text(
        "将 zip 解压，把 Vantry.app 放入 /Applications。\n"
        "这是本地自签名版本，未经过 Apple 公证。首次打开若被拦截，请在系统设置的“隐私与安全性”中手动允许。\n"
        "只有开启远程被控时，才在系统设置里授予屏幕录制和辅助功能。\n"
        "随包 PEM 文件是公开证书；私钥仅保留在构建机，不需要分发。\n"
        "以后复用同一本地签名身份；切换到 Apple Developer ID 证书时，可能需要重新授权。\n",
        encoding="utf-8",
    )
    print(f"Signed Vantry {version} ({architecture}) with {args.identity}")
    print(f"Strict verification passed for the app and {len(native)} nested Mach-O files")
    print(f"Distribution archive: {archive}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError) as error:
        sys.exit(str(error))
