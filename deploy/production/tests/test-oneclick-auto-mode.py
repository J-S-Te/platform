#!/usr/bin/env python3
"""仅使用隔离文件运行实际 Shell 选择与确认函数。"""
import hashlib
import os
import pathlib
import pty
import select
import shlex
import subprocess
import tarfile
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[4]
SOURCE = (ROOT / '服务器一键离线部署.sh').read_text()


def function(name):
    return name + '() {' + SOURCE.split(name + '() {', 1)[1].split('\n}', 1)[0] + '\n}'


def run(code):
    return subprocess.run(['bash', '-c', 'set -Eeuo pipefail\n' + code],
                          capture_output=True, text=True, timeout=10)


COMMON = 'die() { echo "$*" >&2; exit 7; }; info() { echo "$*" >&2; };\n'


class AutoModeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = pathlib.Path(self.temp.name)
        self.target = self.path / 'target'

    def deployment(self, mode='auto'):
        return run(COMMON + function('resolve_deployment_mode') +
                   f'\ntarget_dir={shlex.quote(str(self.target))}; deployment_mode={mode}; '
                   'interactive_upgrade=false; resolve_deployment_mode; '
                   'printf "%s:%s" "$deployment_mode" "$interactive_upgrade"')

    def installed(self):
        (self.target / 'bin').mkdir(parents=True)
        for name in ['docker-compose.yml', '.env', '.release.env', 'bin/deploy.sh']:
            (self.target / name).write_text('fixture')
        (self.target / 'bin/deploy.sh').chmod(0o700)

    def test_empty_target_is_install(self):
        result = self.deployment()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'install:false')

    def test_existing_target_is_interactive_upgrade(self):
        self.installed()
        result = self.deployment()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'upgrade:true')

    def test_partial_target_fails_without_install_fallback(self):
        self.target.mkdir()
        (self.target / '.env').touch()
        self.assertEqual(self.deployment().returncode, 7)

    def test_symlink_config_fails(self):
        self.installed()
        (self.target / '.env').unlink()
        (self.target / '.env').symlink_to(self.target / '.release.env')
        self.assertEqual(self.deployment().returncode, 7)

    def test_nonexecutable_deploy_fails(self):
        self.installed()
        (self.target / 'bin/deploy.sh').chmod(0o600)
        self.assertEqual(self.deployment().returncode, 7)

    def test_explicit_modes_are_preserved(self):
        for mode in ['install', 'upgrade', 'add-subsystems']:
            result = self.deployment(mode)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, mode + ':false')

    def test_noninteractive_confirmation_fails_even_with_yes(self):
        result = run(COMMON + function('confirm_upgrade_action') +
                     '\nassume_yes=true; confirm_upgrade_action confirm')
        self.assertEqual(result.returncode, 7)

    def test_interactive_accept_and_cancel(self):
        for answer, expected in [('UPGRADE', 0), ('yes', 7), ('NO', 7)]:
            master, slave = pty.openpty()
            try:
                proc = subprocess.Popen(['bash', '-c', COMMON +
                                         function('confirm_upgrade_action') +
                                         '\nconfirm_upgrade_action confirm'],
                                        stdin=slave, stdout=slave, stderr=slave)
                os.close(slave)
                slave = None
                try:
                    self.assertTrue(select.select([master], [], [], 5)[0])
                    os.read(master, 4096)
                    os.write(master, (answer + '\n').encode())
                    self.assertEqual(proc.wait(timeout=5), expected)
                finally:
                    if proc.poll() is None:
                        proc.kill()
                        proc.wait()
            finally:
                os.close(master)
                if slave is not None:
                    os.close(slave)

    def media(self, media_input=''):
        # 真实 TAR 校验使用 Linux CI 提供、兼容 GNU 的 sha256sum。
        return run(COMMON + '\n' + function('validate_outer_archive') + '\n' +
                   function('verify_existing_media_manifest') + '\n' +
                   function('extract_outer_archive') + '\n' + function('discover_media') +
                   f'\nscript_dir={shlex.quote(str(self.path))}; '
                   f'media_input={shlex.quote(str(media_input))}; discover_media')

    def archive(self, name='v1'):
        directory = self.path / name
        directory.mkdir()
        (directory / 'SHA256SUMS').write_text('fixture manifest\n')
        archive = self.path / f'uip-offline-delivery-{name}-linux-amd64.tar'
        with tarfile.open(archive, 'w') as handle:
            handle.add(directory, arcname=name)
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        pathlib.Path(str(archive) + '.sha256').write_text(f'{digest}  {archive.name}\n')
        return directory, archive

    def test_archive_only_extracts_then_repeat_reuses(self):
        directory, _ = self.archive()
        (directory / 'SHA256SUMS').unlink()
        directory.rmdir()
        for _ in range(2):
            result = self.media()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), str(directory))

    def test_explicit_archive_extracts_then_repeat_reuses(self):
        directory, archive = self.archive()
        (directory / 'SHA256SUMS').unlink()
        directory.rmdir()
        for attempt in range(2):
            result = self.media(archive)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), str(directory))
            if attempt:
                self.assertIn('复用', result.stderr)
        self.assertEqual(list(self.path.glob('.uip-delivery-extract.*')), [])

    def test_explicit_archive_rejects_changed_missing_or_linked_manifest(self):
        for kind in ['changed', 'missing', 'symlink']:
            with self.subTest(kind=kind):
                directory, archive = self.archive(kind)
                manifest = directory / 'SHA256SUMS'
                original = manifest.read_bytes()
                manifest.unlink()
                if kind == 'changed':
                    manifest.write_text('changed')
                elif kind == 'symlink':
                    external = self.path / 'external-manifest'
                    external.write_bytes(original)
                    manifest.symlink_to(external)
                result = self.media(archive)
                self.assertEqual(result.returncode, 7, result.stderr)
                self.assertEqual(list(self.path.glob('.uip-delivery-extract.*')), [])

    def test_explicit_archive_rejects_target_directory_symlink(self):
        directory, archive = self.archive()
        moved = self.path / 'original-directory'
        directory.rename(moved)
        directory.symlink_to(moved, target_is_directory=True)
        result = self.media(archive)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertTrue(directory.is_symlink())
        self.assertEqual((moved / 'SHA256SUMS').read_text(), 'fixture manifest\n')

    def test_explicit_archive_rejects_missing_or_wrong_outer_sidecar(self):
        for kind in ['missing', 'wrong']:
            with self.subTest(kind=kind):
                _, archive = self.archive(kind)
                sidecar = pathlib.Path(str(archive) + '.sha256')
                if kind == 'missing':
                    sidecar.unlink()
                else:
                    sidecar.write_text(f'{"0" * 64}  {archive.name}\n')
                self.assertEqual(self.media(archive).returncode, 7)

    def test_explicit_archive_rejects_missing_or_duplicate_archive_manifest(self):
        for count in [0, 2]:
            with self.subTest(count=count):
                directory, archive = self.archive(f'manifest-{count}')
                with tarfile.open(archive, 'w') as handle:
                    handle.add(directory, arcname=directory.name, recursive=False)
                    for _ in range(count):
                        handle.add(directory / 'SHA256SUMS', arcname=f'{directory.name}/SHA256SUMS')
                digest = hashlib.sha256(archive.read_bytes()).hexdigest()
                pathlib.Path(str(archive) + '.sha256').write_text(f'{digest}  {archive.name}\n')
                self.assertEqual(self.media(archive).returncode, 7)

    def test_stale_directory_beside_new_archive_fails(self):
        self.archive('v2')
        old = self.path / 'v1'
        old.mkdir()
        (old / 'SHA256SUMS').touch()
        self.assertEqual(self.media().returncode, 7)

    def test_same_name_changed_manifest_fails(self):
        directory, _ = self.archive()
        (directory / 'SHA256SUMS').write_text('changed')
        self.assertEqual(self.media().returncode, 7)

    def test_multiple_archives_fail_even_with_single_directory(self):
        self.archive()
        second = self.path / 'uip-offline-delivery-v2-linux-amd64.tar'
        second.touch()
        self.assertEqual(self.media().returncode, 7)

    def test_missing_archive_sidecar_fails_even_when_extracted(self):
        _, archive = self.archive()
        pathlib.Path(str(archive) + '.sha256').unlink()
        self.assertEqual(self.media().returncode, 7)

    def test_directory_only_is_supported(self):
        directory = self.path / 'v1'
        directory.mkdir()
        (directory / 'SHA256SUMS').touch()
        result = self.media()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), str(directory))


if __name__ == '__main__':
    unittest.main()
