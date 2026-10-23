#!/usr/bin/env python3
"""离线回归，不访问 Docker、服务器、凭据或数据库。"""
import pathlib
import hashlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[4]
WRAPPER = (ROOT / "服务器一键离线部署.sh").read_text()
DEPLOY = (ROOT / "platform/deploy/production/bin/deploy.sh").read_text()


def shell(code):
    return subprocess.run(["bash", "-c", code], text=True, capture_output=True)


class SafetyRegression(unittest.TestCase):
    def test_legacy_sidecar_uses_manifest_and_actual_payload(self):
        binding = WRAPPER.split('require_media_binding() {', 1)[1].split('\n}', 1)[0]
        sidecar = WRAPPER.split('require_media_sidecar() {', 1)[1].split('\n}', 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp)
            payload = path / 'package.tar.gz'
            companion = path / 'package.tar.gz.sha256'
            digest = hashlib.sha256(b'original').hexdigest()
            valid = f'{digest}  package.tar.gz\n'
            prefix = (f'set -Eeuo pipefail; media_dir={tmp!r}; die() {{ exit 7; }}; '
                      'info() { :; }; '
                      f'require_media_binding() {{{binding}\n}}; '
                      f'require_media_sidecar() {{{sidecar}\n}}; ')
            for content, value, binding_count, expected in [
                (b'original', valid, 1, 0),
                (b'changed', valid, 1, 7),
                (b'original', '0' * 64 + '  package.tar.gz\n', 1, 7),
                (b'original', valid + valid, 1, 7),
                (b'original', valid.replace('package.tar.gz', '../package.tar.gz'), 1, 7),
                (b'original', valid, 0, 7),
                (b'original', valid, 2, 7),
            ]:
                payload.write_bytes(content)
                companion.write_text(value)
                (path / 'SHA256SUMS').write_text(valid * binding_count)
                self.assertEqual(shell(prefix + 'require_media_sidecar package.tar.gz').returncode, expected)
            payload.write_bytes(b'original')
            companion.write_text(valid)
            bound_sidecar = hashlib.sha256(companion.read_bytes()).hexdigest()
            (path / 'SHA256SUMS').write_text(valid + f'{bound_sidecar}  package.tar.gz.sha256\n')
            self.assertEqual(shell(prefix + 'require_media_sidecar package.tar.gz').returncode, 0)
            # 显式绑定后，逻辑摘要相同但伴随字节不同也必须失败。
            companion.write_text(valid.replace('  ', ' '))
            self.assertEqual(shell(prefix + 'require_media_sidecar package.tar.gz').returncode, 7)
            companion.unlink()
            self.assertEqual(shell(prefix + 'require_media_sidecar package.tar.gz').returncode, 7)
            companion.symlink_to(payload)
            self.assertEqual(shell(prefix + 'require_media_sidecar package.tar.gz').returncode, 7)

    def test_upgrade_publishes_new_parser_before_reload(self):
        upgrade = WRAPPER.split('# 升级模式：已部署环境', 1)[1]
        self.assertLess(upgrade.index('import "$media_dir/${package_components[platform]}"'),
                        upgrade.index('  workflow_assets_install'))
        self.assertLess(upgrade.index('workflow_publish platform '),
                        upgrade.index('reload-control-plane'))
        publisher = WRAPPER.split('workflow_root_deploy() {', 1)[1].split('\n}', 1)[0]
        self.assertIn('DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE=true', publisher)
        self.assertNotIn('assert_marker_without_platform_upgrade', WRAPPER)
        self.assertIn('activator_args=(--deploy-dir "$target_dir")', WRAPPER)

    def test_sudo_forwards_license_arguments(self):
        sudo = WRAPPER.split('if ((EUID != 0)); then', 1)[1].split('\nfi', 1)[0]
        self.assertIn('sudo_args+=(--customer "$license_customer")', sudo)
        self.assertIn('sudo_args+=(--license "$license_file_arg")', sudo)

    def test_media_requires_explicit_unique_manifest_entry(self):
        fn = WRAPPER.split('require_media_binding() {', 1)[1].split('\n}', 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp)
            (path / 'license-activate.sh').write_text('fixture')
            prefix = f'media_dir={tmp!r}; die() {{ exit 7; }}; require_media_binding() {{{fn}\n}}; '
            for manifest, expected in [('', 7), ('a license-activate.sh\n', 0),
                                       ('a license-activate.sh\na license-activate.sh\n', 7)]:
                (path / 'SHA256SUMS').write_text(manifest)
                self.assertEqual(shell(prefix + 'require_media_binding license-activate.sh').returncode, expected)
            (path / 'license-activate.sh').unlink()
            (path / 'license-activate.sh').symlink_to(path / 'SHA256SUMS')
            self.assertEqual(shell(prefix + 'require_media_binding license-activate.sh').returncode, 7)

    def test_pending_gate_rejects_ordinary_and_business_publishes(self):
        gate = DEPLOY.split('command="${1:-menu}"', 1)[1].split('\ncase "$command" in', 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            marker = pathlib.Path(tmp) / 'marker'
            marker.touch()
            for command, arg, coordinated, expected in [
                ('deploy', 'platform', 'true', 0), ('deploy', 'platform', 'false', 7),
                ('deploy', 'contract', 'true', 7), ('upgrade', 'project', 'true', 7),
                ('prepare', 'contract', 'true', 7), ('verify', '', 'true', 7),
                ('import', 'any', 'false', 0), ('recover-platform', 'any', 'false', 0),
                ('status', '', 'false', 0)]:
                code = f'control_plane_reload_marker={str(marker)!r}; command={command!r}; DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE={coordinated}; die() {{ exit 7; }}; set -- x {arg!r}; {gate}'
                self.assertEqual(shell(code).returncode, expected, command + ':' + arg)

    def test_import_checks_actual_component_before_docker_load(self):
        check = DEPLOY.split('# Pending assets may need a new parser binary.', 1)[1].split('\n  if ! docker load', 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            marker = pathlib.Path(tmp) / 'marker'
            marker.touch()
            for component, expected in [('common', 0), ('platform', 0), ('frontend', 7), ('contract', 7)]:
                code = f'control_plane_reload_marker={str(marker)!r}; component={component!r}; die() {{ exit 7; }}; set -- archive; {check}'
                self.assertEqual(shell(code).returncode, expected)
        self.assertIn('import_package "${2:?请指定经过校验的 platform 离线镜像包}" platform', DEPLOY)

    def test_bound_candidate_cannot_be_replaced(self):
        check = DEPLOY.split('  if [[ "$component" == platform && -f "$control_plane_reload_marker" ]]; then', 1)[1].split('\n  fi', 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            marker = pathlib.Path(tmp) / 'marker'
            for value, expected in [('PLATFORM_UPGRADE_IMAGE=candidate-a\n', 0),
                                    ('PLATFORM_UPGRADE_IMAGE=candidate-b\n', 7),
                                    ('PLATFORM_UPGRADE_IMAGE=\n', 7),
                                    ('PLATFORM_UPGRADE_IMAGE=candidate-a\nPLATFORM_UPGRADE_IMAGE=candidate-a\n', 7)]:
                marker.write_text(value)
                code = f'control_plane_reload_marker={str(marker)!r}; pushed=candidate-a; die() {{ exit 7; }}; run() {{{check}\n}}; run'
                self.assertEqual(shell(code).returncode, expected)
        self.assertIn('平台恢复必须显式提供安全 SHA256 伴随文件', DEPLOY)


if __name__ == '__main__':
    unittest.main()
