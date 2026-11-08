#!/usr/bin/env python3
"""监控挂载定向补齐回归（缺口：活动编排只挂 2 个规则文件，prometheus 静默 fail-open）。

背景
----
活动编排 `docker-compose.yml` 是**现场保留**的（`install-assets.sh` 只写 `.dist`，保留模块
裁剪），所以它的 prometheus 服务不会随资产包整体更新。现场曾经停在“只挂 prometheus.yml +
keycloak/file-gateway 两个规则文件”，而 `monitoring/prometheus/prometheus.yml` 的
`rule_files` 声明了 4 个文件：缺文件时 prometheus 不报错，`/api/v1/rules` 直接少一半；
platform job 还需要 `runtime/prometheus-secrets/platform-metrics-token` 的只读挂载，
否则永远报 `unable to read authorization credentials`。

本用例驱动真实的 `bin/install-assets.sh`，验证：

1. 历史编排缺的 2 个规则挂载 + 凭据目录挂载被定向补齐，其余行（镜像、自定义卷、裁剪）不变；
2. 重复安装幂等，不会写出重复挂载；
3. `runtime/prometheus-secrets` 缺失时只建空目录（0750），**绝不**生成 token，
   安装成功（fail-safe：监控挂载不得让资产安装失败，也不得让 prometheus 起不来）；
4. 结构不受支持（flow 式 volumes、重复 prometheus 服务）或凭据目录不是普通目录时，
   安装仍然成功、编排保持原样并给出提示——宁可 job 报缺凭据，也不阻断发布。

不连接任何业务环境，不读取真实凭据。
"""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

RULE_FILES = ('keycloak-alerts.yml', 'file-gateway-alerts.yml',
              'platform-alerts.yml', 'integration-alerts.yml')
RULE_MOUNTS = {name: f'      - ./monitoring/prometheus/{name}:/etc/prometheus/{name}:ro\n'
               for name in RULE_FILES}
SECRETS_MOUNT = '      - ./runtime/prometheus-secrets:/etc/prometheus/secrets:ro\n'

# 现场编排的最小复刻：只有 prometheus.yml + 2 个历史规则文件，凭据目录未挂载。
OLD = '''# operator module selection
services:
  prometheus:
    image: prom/prometheus:v3.5.0
    volumes:
      - ./monitoring/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - ./monitoring/prometheus/keycloak-alerts.yml:/etc/prometheus/keycloak-alerts.yml:ro
      - ./monitoring/prometheus/file-gateway-alerts.yml:/etc/prometheus/file-gateway-alerts.yml:ro
      - prometheus-data:/prometheus
    networks: [application]
    restart: unless-stopped
  platform-api:
    image: fixture/platform:old
    environment:
      PRIVATE_FIXTURE: unchanged
'''


class PrometheusMountTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='prometheus-mounts-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source'
        (self.source / 'bin').mkdir(parents=True)
        (self.source / 'bin/deploy.sh').write_text('#!/bin/bash\ntrue\n')
        (self.source / 'docker-compose.yml').write_text('services: {}\n')
        rules = self.source / 'monitoring' / 'prometheus'
        rules.mkdir(parents=True)
        (rules / 'prometheus.yml').write_text('rule_files:\n')
        for name in RULE_FILES:
            (rules / name).write_text('groups: []\n')
        self.target = self.root / 'target'
        (self.target / 'runtime').mkdir(parents=True)
        (self.target / '.env').write_text('PRIVATE_FIXTURE=must-not-change\n')
        (self.target / 'docker-compose.yml').write_text(OLD)
        self.tools = self.root / 'tools'
        self.tools.mkdir()
        self.executable('docker', '#!/bin/sh\ncase "$1" in info|ps) exit 0;; esac\nexit 1\n')
        if not shutil.which('flock'):
            self.executable('flock', '#!/bin/sh\nexit 0\n')
        self.env = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ['PATH'])

    def executable(self, name, content):
        path = self.tools / name
        path.write_text(content)
        path.chmod(0o755)

    def install(self):
        archive = self.root / 'assets.tar.gz'
        with tarfile.open(archive, 'w:gz') as output:
            output.add(self.source, arcname='.')
        Path(str(archive) + '.sha256').write_text(
            hashlib.sha256(archive.read_bytes()).hexdigest() + '  assets.tar.gz\n')
        return subprocess.run(['bash', str(ROOT / 'bin/install-assets.sh'),
                               str(archive), str(self.target)],
                              env=self.env, text=True, capture_output=True)

    def mode(self, path):
        return oct(path.stat().st_mode)[-3:]

    def test_missing_rule_and_credential_mounts_are_added(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        expected = OLD.replace(
            '      - prometheus-data:/prometheus\n',
            '      - prometheus-data:/prometheus\n' + RULE_MOUNTS['platform-alerts.yml'] +
            RULE_MOUNTS['integration-alerts.yml'] + SECRETS_MOUNT)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), expected)
        # 凭据目录存在但为空：安装方不得凭空生成 token。
        secrets = self.target / 'runtime/prometheus-secrets'
        self.assertTrue(secrets.is_dir())
        self.assertEqual(self.mode(secrets), '750')
        self.assertEqual(list(secrets.iterdir()), [])
        self.assertEqual((self.target / '.env').read_text(), 'PRIVATE_FIXTURE=must-not-change\n')

    def test_install_is_idempotent(self):
        self.assertEqual(self.install().returncode, 0)
        first = (self.target / 'docker-compose.yml').read_text()
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), first)
        self.assertEqual(first.count('integration-alerts.yml:/etc/prometheus/'), 1)
        self.assertEqual(first.count('runtime/prometheus-secrets:/etc/prometheus/secrets'), 1)

    def test_existing_custom_mount_source_is_kept(self):
        custom = OLD.replace(
            '      - prometheus-data:/prometheus\n',
            '      - /data/prometheus-alerts.yml:/etc/prometheus/platform-alerts.yml:ro\n'
            '      - prometheus-data:/prometheus\n')
        (self.target / 'docker-compose.yml').write_text(custom)
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        updated = (self.target / 'docker-compose.yml').read_text()
        # 现场自定义来源保留，不写重复目标；缺的 integration 规则和凭据目录仍补齐。
        self.assertIn('      - /data/prometheus-alerts.yml:/etc/prometheus/platform-alerts.yml:ro\n',
                      updated)
        self.assertNotIn('      - ./monitoring/prometheus/platform-alerts.yml', updated)
        self.assertIn(RULE_MOUNTS['integration-alerts.yml'], updated)
        self.assertIn(SECRETS_MOUNT, updated)

    def test_unsupported_structure_is_reported_not_fatal(self):
        unsupported = (
            OLD.replace(
                '    volumes:\n',
                '    volumes: ["./monitoring/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro"]\n'),
            OLD + '  prometheus:\n    image: duplicate\n',
            OLD.replace('    volumes:\n', '    <<: *custom\n'),
        )
        for old in unsupported:
            with self.subTest(old=old):
                (self.target / 'docker-compose.yml').write_text(old)
                result = self.install()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual((self.target / 'docker-compose.yml').read_text(), old)
                self.assertIn('prometheus', result.stderr)

    def test_absent_prometheus_service_is_untouched(self):
        trimmed = OLD[:OLD.index('  prometheus:')] + '  platform-api:\n    image: fixture/platform:old\n'
        (self.target / 'docker-compose.yml').write_text(trimmed)
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), trimmed)

    def test_credential_directory_symlink_is_not_mounted(self):
        secrets = self.target / 'runtime/prometheus-secrets'
        secrets.symlink_to(self.target / 'elsewhere')
        (self.target / 'elsewhere').mkdir()
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        updated = (self.target / 'docker-compose.yml').read_text()
        self.assertIn(RULE_MOUNTS['platform-alerts.yml'], updated)
        self.assertNotIn('runtime/prometheus-secrets:/etc/prometheus/secrets', updated)
        self.assertIn('prometheus-secrets', result.stderr)


if __name__ == '__main__':
    unittest.main()
