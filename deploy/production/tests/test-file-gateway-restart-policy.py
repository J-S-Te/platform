#!/usr/bin/env python3
"""验证重启策略的持久化，避免将迁移或密钥初始化任务设为常驻进程。"""
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[1]


def service_block(text, name):
    blocks = list(re.finditer(r'(?ms)^  ' + re.escape(name) + r':\n.*?(?=^  [a-zA-Z0-9_-]+:|^\S|\Z)', text))
    if len(blocks) != 1:
        raise AssertionError('Expected exactly one service: ' + name)
    return blocks[0].group()


class GatewayRestartPolicyTest(unittest.TestCase):
    def test_gateway_and_database_survive_daemon_restart_in_both_templates(self):
        for filename in ('docker-compose.yml', 'compose.yaml'):
            for service in ('file-gateway', 'file-gateway-mysql'):
                with self.subTest(filename=filename, service=service):
                    block = service_block((ROOT / filename).read_text(), service)
                    self.assertEqual(re.findall(r'^    restart:\s*(.*?)\s*$', block, re.M), ['unless-stopped'])
                    self.assertIn('healthcheck:', block)
                    self.assertIn('volumes:', block)

    def test_one_shot_operations_remain_one_shot(self):
        canonical = (ROOT / 'docker-compose.yml').read_text()
        for service in ('platform-key-init', 'platform-migrate', 'contract-migrate'):
            with self.subTest(service=service):
                self.assertIn('restart: "no"', service_block(canonical, service))


if __name__ == '__main__':
    unittest.main()
