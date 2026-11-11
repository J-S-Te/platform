#!/usr/bin/env python3
"""使用带扩展属性的夹具，由实际打包器创建外层 TAR 归档。"""
import hashlib
import os
from pathlib import Path
import platform
import shlex
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(os.environ.get('UIP_ENGINEERING_ROOT', Path(__file__).resolve().parents[4]))
SOURCE = (ROOT / '一键制作离线镜像包.sh').read_text()
ARCHIVE_CODE = 'output_parent=' + SOURCE.split('\noutput_parent=', 1)[1].split(
    '\nif tar -tf "$archive_path"', 1)[0]


class ArchiveMetadataTests(unittest.TestCase):
    # 用途：验证中文输出目录不影响发行根名，且排除宿主扩展属性。
    # 参数：无。
    # 返回值：无；归档正文、源扩展属性和 sidecar 不一致时断言失败。
    def test_outer_archive_omits_xattrs_and_preserves_file_and_checksum(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            delivery = parent / '交付-自定义 输出目录'
            delivery.mkdir()
            payload = delivery / 'BUILD_INFO.txt'
            payload.write_bytes(b'delivery fixture\n')
            proof = delivery / 'attestations/build.intoto.json'
            proof.parent.mkdir()
            proof.write_bytes(b'proof fixture\n')
            attr = 'com.apple.uip-test' if platform.system() == 'Darwin' else 'user.uip-test'
            if platform.system() == 'Darwin':
                # macOS 自带的 Python 未必暴露 os.setxattr；使用宿主原生工具。
                subprocess.run(['xattr', '-w', attr, 'attribute fixture', str(payload)], check=True)
            else:
                os.setxattr(payload, attr, b'attribute fixture')
            archive_name = 'uip-offline-delivery-test-r1-linux-amd64.tar'
            archive = parent / archive_name
            assignments = dict(output_dir=str(delivery), version='test-r1')
            code = 'set -Eeuo pipefail\numask 077\n' + '\n'.join(
                f'{name}={shlex.quote(value)}' for name, value in assignments.items())
            result = subprocess.run(['bash', '-c', code + '\n' + ARCHIVE_CODE],
                                    text=True, capture_output=True, timeout=20)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            with tarfile.open(archive) as handle:
                members = handle.getmembers()
                self.assertEqual({m.name.split('/')[0] for m in members}, {'test-r1'})
                self.assertTrue(all(not any('xattr' in key for key in member.pax_headers) for member in members))
                self.assertFalse(any(Path(member.name).name.startswith('._') for member in members))
                self.assertEqual(handle.extractfile('test-r1/BUILD_INFO.txt').read(), payload.read_bytes())
                self.assertEqual(handle.extractfile('test-r1/attestations/build.intoto.json').read(),
                                 proof.read_bytes())
                self.assertTrue(all(m.isdir() or m.isfile() for m in members))
            if platform.system() == 'Darwin':
                value = subprocess.check_output(['xattr', '-p', attr, str(payload)]).rstrip(b'\n')
            else:
                value = os.getxattr(payload, attr)
            self.assertEqual(value, b'attribute fixture')
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            self.assertEqual(Path(str(archive) + '.sha256').read_text(), f'{digest}  {archive_name}\n')
            self.assertEqual(list(parent.glob('.uip-archive-root.*')), [])
            server = (ROOT / '服务器一键离线部署.sh').read_text()
            validator = 'validate_outer_archive() {' + server.split(
                'validate_outer_archive() {', 1)[1].split('\n}', 1)[0] + '\n}'
            checked = subprocess.run(['bash', '-c',
                'set -Eeuo pipefail; die() { exit 7; };\n' + validator +
                '\nvalidate_outer_archive ' + shlex.quote(str(archive))],
                text=True, capture_output=True, timeout=20)
            self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
            self.assertEqual(checked.stdout.strip(), 'test-r1')


if __name__ == '__main__':
    unittest.main()
