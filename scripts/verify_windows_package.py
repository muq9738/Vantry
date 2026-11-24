#!/usr/bin/env python3
"""在 Windows 上验证打包后的全 Rust Vantry 便携目录及可选安装包。

验证步骤：
1. 在最小 PATH（仅 System32）下运行，防止开发机环境掩盖缺失 DLL。
2. 核验随包资源（Vantry.exe、vantry-service.exe、许可文件、可选 remote-desktop helper PE 架构）。
3. 在带空格和中文字符的数据目录中真实启动 Core，验证 SQLite 初始化/迁移、本机 IPC、创建与读取线程。
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, 'reconfigure'):
        _stream.reconfigure(encoding='utf-8', errors='replace')


def clean_windows_env():
    env = {
        key: value
        for key, value in os.environ.items()
        if not key.startswith('VANTRY_')
    }
    system_root = Path(os.environ.get('SystemRoot', r'C:\Windows'))
    # 构建机的工具链 PATH 不能掩盖缺少随包 DLL 的问题。
    env['PATH'] = str(system_root / 'System32')
    return env


def verify_pe_architecture(binary_path, expected_arch='amd64'):
    expected_machine = {'amd64': 0x8664, 'arm64': 0xAA64}[expected_arch]
    with binary_path.open('rb') as stream:
        header = stream.read(64)
        if len(header) != 64 or header[:2] != b'MZ':
            raise AssertionError(f'不是有效的 Windows PE 文件: {binary_path}')
        pe_offset = int.from_bytes(header[60:64], 'little')
        stream.seek(pe_offset)
        pe = stream.read(6)
        if len(pe) != 6 or pe[:4] != b'PE\x00\x00':
            raise AssertionError(f'缺少 PE 头: {binary_path}')
        machine = int.from_bytes(pe[4:6], 'little')
        if machine != expected_machine:
            raise AssertionError(
                f'PE 架构不匹配: {binary_path} got 0x{machine:04x}, want 0x{expected_machine:04x}'
            )


def resolve_service_exe(folder):
    for rel in ['libexec/vantry-service.exe', 'vantry-service.exe']:
        candidate = folder / rel
        if candidate.is_file():
            return candidate
    raise AssertionError(
        f'缺少随包服务程序: {folder / "libexec" / "vantry-service.exe"} (或 {folder / "vantry-service.exe"})'
    )


def check_layout(folder, expected_arch='amd64', require_helper=False):
    for rel in ['Vantry.exe', 'LICENSE', 'THIRD_PARTY_NOTICES.md']:
        target = folder / rel
        if not target.is_file():
            raise AssertionError(f'缺少随包文件: {target}')
    verify_pe_architecture(folder / 'Vantry.exe', expected_arch)
    verify_pe_architecture(resolve_service_exe(folder), expected_arch)

    licenses_dir = folder / 'third_party' / 'licenses'
    if not licenses_dir.is_dir():
        licenses_dir = folder / 'licenses'
    if not licenses_dir.is_dir() or not any(licenses_dir.iterdir()):
        raise AssertionError(f'缺少第三方许可目录或为空: {licenses_dir}')

    helper = folder / 'remote-desktop' / 'vantry-remote-helper.exe'
    if require_helper and not helper.is_file():
        raise AssertionError(f'缺少 Windows 远程桌面 helper: {helper}')
    if helper.is_file():
        verify_pe_architecture(helper, expected_arch)


def smoke_core(exe_path, env):
    help_out = subprocess.check_output([str(exe_path), '--help'], env=env, timeout=30)
    if b'Vantry' not in help_out and b'vantry' not in help_out:
        raise AssertionError(f'--help 输出异常: {help_out[:200]!r}')

    with tempfile.TemporaryDirectory(prefix='vantry-win-smoke-') as directory:
        data_dir = Path(directory) / 'Vantry 数据目录 #1'
        core_log = Path(directory) / 'core.log'
        with core_log.open('w+', encoding='utf-8') as log_stream:
            process = subprocess.Popen(
                [
                    str(exe_path),
                    'core',
                    '--data-dir',
                    str(data_dir),
                    '--provider',
                    'controlled',
                ],
                stdout=log_stream,
                stderr=subprocess.STDOUT,
                env=env,
            )
            try:
                deadline = time.monotonic() + 25
                health = None
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        log_text = core_log.read_text(encoding='utf-8', errors='replace')
                        raise AssertionError(
                            f'Core 提前退出 (code={process.returncode}):\n{log_text}'
                        )
                    probe = subprocess.run(
                        [str(exe_path), 'ctl', '--data-dir', str(data_dir), 'health'],
                        env=env,
                        capture_output=True,
                        timeout=10,
                    )
                    if probe.returncode == 0 and probe.stdout.strip():
                        health = json.loads(probe.stdout.decode('utf-8'))
                        break
                    time.sleep(0.25)
                if not health or health.get('state') != 'ready':
                    log_text = core_log.read_text(encoding='utf-8', errors='replace')
                    raise AssertionError(
                        f'等待 Core health 就绪超时 (last={health}):\n{log_text}'
                    )

                created_raw = subprocess.check_output(
                    [
                        str(exe_path),
                        'ctl',
                        '--data-dir',
                        str(data_dir),
                        'create',
                        '--title',
                        'Windows 冒烟测试',
                        '--provider',
                        'controlled',
                    ],
                    env=env,
                    timeout=15,
                )
                created = json.loads(created_raw.decode('utf-8'))
                if not created:
                    raise AssertionError(f'ctl create 返回空结果: {created_raw!r}')

                threads_raw = subprocess.check_output(
                    [str(exe_path), 'ctl', '--data-dir', str(data_dir), 'threads'],
                    env=env,
                    timeout=15,
                )
                threads = json.loads(threads_raw.decode('utf-8'))
                items = threads.get('threads') or []
                if not any(item.get('title') == 'Windows 冒烟测试' for item in items):
                    raise AssertionError(f'ctl threads 未找到刚创建的线程: {threads}')

                db_path = data_dir / 'state.sqlite'
                if not db_path.is_file():
                    raise AssertionError(f'未生成 SQLite 数据库文件: {db_path}')
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


def verify(package, arch='amd64', require_helper=False, skip_installer=False):
    if os.name != 'nt':
        raise RuntimeError('此检查必须在 Windows 执行')
    package = package.resolve()
    env = clean_windows_env()

    check_layout(package, expected_arch=arch, require_helper=require_helper)
    smoke_core(package / 'Vantry.exe', env)

    if not skip_installer:
        installers = sorted(
            p for p in package.glob('*.exe')
            if p.name.lower() != 'vantry.exe' and ('setup' in p.name.lower() or 'installer' in p.name.lower())
        )
        if installers:
            installer = installers[0]
            with tempfile.TemporaryDirectory(prefix='vantry-user-install-') as directory:
                installed = Path(directory) / 'Vantry 安装测试 不乱码'
                # NSIS 的 /D= 必须位于最后，包含空格时也不能给路径加引号。
                command = subprocess.list2cmdline([str(installer), '/S']) + ' /D=' + str(installed)
                subprocess.run(command, check=True, timeout=120)
                check_layout(installed, expected_arch=arch, require_helper=require_helper)
                smoke_core(installed / 'Vantry.exe', env)
                uninstaller = installed / 'Uninstall.exe'
                if uninstaller.is_file():
                    # NSIS 的 _?= 必须位于最后且不加引号，确保卸载器同步执行完毕再退出。
                    uninstall_cmd = (
                        subprocess.list2cmdline([str(uninstaller), '/S'])
                        + ' _?='
                        + str(installed)
                    )
                    subprocess.run(uninstall_cmd, check=True, timeout=60)

    print('Windows 便携目录、Core/SQLite 启动冒烟及可选个人安装检查通过')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--package', type=Path, default=Path('dist/windows-amd64'))
    parser.add_argument('--arch', choices=['amd64', 'arm64'], default='amd64')
    parser.add_argument('--require-helper', action='store_true')
    parser.add_argument('--skip-installer', action='store_true')
    args = parser.parse_args()
    verify(
        args.package,
        arch=args.arch,
        require_helper=args.require_helper,
        skip_installer=args.skip_installer,
    )
