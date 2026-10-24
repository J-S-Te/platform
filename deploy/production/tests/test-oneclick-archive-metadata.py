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

ROOT = Path(__file__).resolve().parents[4]
SOURCE = (ROOT / '一键制作离线镜像包.sh').read_text()
ARCHIVE_CODE = SOURCE.split('temporary_archive="$(mktemp ', 1)[1]
ARCHIVE_CODE = 'temporary_archive="$(mktemp ' + ARCHIVE_CODE.split('\nif tar -tf "$archive_path"', 1)[0]


class ArchiveMetadataTests(unittest.TestCase):
    def test_outer_archive_omits_xattrs_and_preserves_file_and_checksum(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            delivery = parent / 'delivery'
            delivery.mkdir()
            payload = delivery / 'BUILD_INFO.txt'
            payload.write_bytes(b'delivery fixture\n')
            attr = 'com.apple.uip-test' if platform.system() == 'Darwin' else 'user.uip-test'
            if platform.system() == 'Darwin':
                # macOS 自带的 Python 未必暴露 os.setxattr；使用宿主原生工具。
                subprocess.run(['xattr', '-w', attr, 'attribute fixture', str(payload)], check=True)
            else:
                os.setxattr(payload, attr, b'attribute fixture')
            archive_name = 'uip-offline-delivery-test-linux-amd64.tar'
            archive = parent / archive_name
            assignments = dict(output_parent=str(parent), output_name=delivery.name,
                               archive_name=archive_name, archive_path=str(archive))
            code = 'set -Eeuo pipefail\numask 077\n' + '\n'.join(
                f'{name}={shlex.quote(value)}' for name, value in assignments.items())
            result = subprocess.run(['bash', '-c', code + '\n' + ARCHIVE_CODE],
                                    text=True, capture_output=True, timeout=20)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            with tarfile.open(archive) as handle:
                members = handle.getmembers()
                self.assertTrue(all(not any('xattr' in key for key in member.pax_headers) for member in members))
                self.assertFalse(any(Path(member.name).name.startswith('._') for member in members))
                self.assertEqual(handle.extractfile('delivery/BUILD_INFO.txt').read(), payload.read_bytes())
            if platform.system() == 'Darwin':
                value = subprocess.check_output(['xattr', '-p', attr, str(payload)]).rstrip(b'\n')
            else:
                value = os.getxattr(payload, attr)
            self.assertEqual(value, b'attribute fixture')
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            self.assertEqual(Path(str(archive) + '.sha256').read_text(), f'{digest}  {archive_name}\n')


if __name__ == '__main__':
    unittest.main()
