#!/usr/bin/env python3
"""Build a matching native RTC helper into platform-specific MyGo resources.

macOS builds Swift locally. Windows builds with MSVC/CMake on Windows, or takes
an explicitly provided Windows CI artifact when packaging from another OS.
No permissions are requested and no capture/input is started by this builder.
"""
import argparse
import os
import platform
from pathlib import Path
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--plugin-root', type=Path, help='explicit local plugin checkout for development')
parser.add_argument('--platform', choices=['darwin/arm64', 'darwin/amd64', 'darwin/universal', 'windows/amd64', 'windows/arm64'])
parser.add_argument('--archs', help='macOS source architectures, e.g. "arm64 x86_64"')
parser.add_argument('--helper-file', type=Path, help='matching prebuilt Windows CI helper for cross packaging')
args = parser.parse_args()
project = Path(__file__).resolve().parent.parent
native_os = {'Darwin': 'darwin', 'Windows': 'windows'}.get(platform.system())
arch = 'arm64' if platform.machine().lower() in ['arm64', 'aarch64'] else 'amd64'
target = args.platform or (f'{native_os}/{arch}' if native_os else '')
if not target:
    raise SystemExit('Specify a supported --platform and matching build environment')
goos, goarch = target.split('/')
if args.plugin_root:
    plugin = args.plugin_root.resolve()
else:
    plugin = Path(subprocess.check_output(['go', 'list', '-m', '-f', '{{.Dir}}', 'github.com/muq9738/vantry-plugin-remote-desktop'], cwd=project, text=True).strip())
name = 'vantry-remote-helper.exe' if goos == 'windows' else 'vantry-remote-helper'
output = project / 'resources' / f'{goos}-{goarch}' / 'remote-desktop' / name
output.parent.mkdir(parents=True, exist_ok=True)
with tempfile.TemporaryDirectory(prefix='vantry-native-build-') as temporary:
    temp = Path(temporary)
    if goos == 'darwin':
        if native_os != 'darwin' or args.helper_file:
            raise SystemExit('macOS helper compilation and signing require macOS')
        build = plugin / 'native/build.sh'
        if not build.is_file():
            raise SystemExit('Native Swift helper source is missing from this plugin version')
        archs = args.archs or {'arm64': 'arm64', 'amd64': 'x86_64', 'universal': 'arm64 x86_64'}[goarch]
        environment = dict(os.environ, OUT_DIR=temporary, PRODUCT_NAME=name, ARCHS=archs, MIN_MACOS='13.0')
        subprocess.run(['bash', str(build)], cwd=plugin, env=environment, check=True)
        source = temp / name
        subprocess.run(['codesign', '--verify', '--strict', str(source)], check=True)
    elif args.helper_file:
        source = args.helper_file.resolve()
        with source.open('rb') as binary:
            header = binary.read(64)
            if header[:2] != b'MZ' or len(header) != 64:
                raise SystemExit('The supplied helper is not a Windows executable')
            binary.seek(int.from_bytes(header[60:64], 'little'))
            pe = binary.read(6)
            if pe[:4] != b'PE\x00\x00' or int.from_bytes(pe[4:6], 'little') != {'amd64': 0x8664, 'arm64': 0xAA64}[goarch]:
                raise SystemExit('The Windows helper architecture does not match --platform')
    else:
        if native_os != 'windows':
            raise SystemExit('Windows requires MSVC/SDK on Windows, or --helper-file from Windows CI')
        native = plugin / 'native/windows'
        if not (native / 'CMakeLists.txt').is_file():
            raise SystemExit('Windows helper source is not available in this plugin version')
        build = temp / 'build'
        subprocess.run(['cmake', '-S', str(native), '-B', str(build), '-A', 'ARM64' if goarch == 'arm64' else 'x64'], check=True)
        subprocess.run(['cmake', '--build', str(build), '--config', 'Release'], check=True)
        source = build / 'Release' / name
    if not source.is_file():
        raise SystemExit('Native helper build did not produce the expected executable')
    shutil.copy2(source, output)
    if goos == 'darwin':
        output.chmod(0o755)
        subprocess.run(['codesign', '--verify', '--strict', str(output)], check=True)
# Remove only the builder's former platform-neutral output so a Mach-O helper
# cannot accidentally ship in a Windows package. Other resource files remain.
legacy = project / 'resources/remote-desktop/vantry-remote-helper'
if legacy.is_file():
    legacy.unlink()
print(f'Native H.264 helper ready ({target}): {output}')
