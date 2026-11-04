#!/usr/bin/env python3
"""Portal 历史部署资产升级及运行配置传播回归，不连接业务环境。"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
MAPPING = ('      APP_TRUSTED_PROXIES: ${APP_TRUSTED_PROXIES:-'
           '127.0.0.1/32,::1/128,172.31.255.1/32,172.31.255.250/32}\n')
OLD = '''# operator selection
services:
  portal-api:
    image: fixture/portal:old
    environment:
      PRIVATE_FIXTURE: unchanged
    ports: ["18084:8091"]
  portal-migrate:
    image: fixture/portal:old
    command: ["./production-migrate", "--schema", "portal"]
#  portal-disabled:
#    image: never-enable
'''


class PortalAssetTests(unittest.TestCase):
    # 用途：准备真实安装器使用的隔离归档及历史现场文件。
    # 参数：无。
    # 返回值：无；所有文件由临时目录自动清理。
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='portal-proxy-assets-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source'
        (self.source / 'bin').mkdir(parents=True)
        (self.source / 'bin/deploy.sh').write_text('#!/bin/bash\ntrue\n')
        (self.source / 'docker-compose.yml').write_text('services: {}\n')
        self.target = self.root / 'target'
        (self.target / 'runtime').mkdir(parents=True)
        (self.target / '.env').write_text('PRIVATE_FIXTURE=must-not-change\n')
        (self.target / 'runtime/portal.env').write_text('PORTAL_FIXTURE=keep\n')
        (self.target / 'docker-compose.yml').write_text(OLD)
        self.tools = self.root / 'tools'
        self.tools.mkdir()
        self.executable('docker', '#!/bin/sh\ncase "$1" in info|ps) exit 0;; esac\nexit 1\n')
        if not shutil.which('flock'):
            self.executable('flock', '#!/bin/sh\nexit 0\n')
        self.env = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ['PATH'])

    # 用途：创建仅用于隔离外部命令的测试可执行文件。
    # 参数：name 为命令名，content 为脚本正文。
    # 返回值：无。
    def executable(self, name, content):
        path = self.tools / name
        path.write_text(content)
        path.chmod(0o755)

    # 用途：执行真实安装器，验证事务与兼容更新行为。
    # 参数：recover 为是否执行安装器原生恢复。
    # 返回值：带退出码与已捕获输出的进程结果。
    def install(self, recover=False):
        archive = self.root / 'assets.tar.gz'
        with tarfile.open(archive, 'w:gz') as output:
            output.add(self.source, arcname='.')
        Path(str(archive) + '.sha256').write_text(
            hashlib.sha256(archive.read_bytes()).hexdigest() + '  assets.tar.gz\n')
        return subprocess.run(['bash', str(ROOT / 'bin/install-assets.sh'),
                               '--recover' if recover else str(archive), str(self.target)],
                              env=self.env, text=True, capture_output=True)

    # 用途：复现旧活动编排缺少映射，并验证升级不触碰配置秘密。
    # 参数：无。
    # 返回值：无；缺少任一 Portal 映射时断言失败。
    def test_upgrade_existing_environment_and_missing_environment(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        expected = OLD.replace('      PRIVATE_FIXTURE:', MAPPING + '      PRIVATE_FIXTURE:')
        expected = expected.replace('  portal-migrate:\n',
                                    '  portal-migrate:\n    environment:\n' + MAPPING)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), expected)
        self.assertEqual((self.target / '.env').read_text(), 'PRIVATE_FIXTURE=must-not-change\n')
        self.assertEqual((self.target / 'runtime/portal.env').read_text(), 'PORTAL_FIXTURE=keep\n')
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), expected)

    # 用途：保留显式自定义代理及裁剪的服务，不重复写入。
    # 参数：无。
    # 返回值：无；现场值或禁用状态改变时断言失败。
    def test_custom_proxy_and_disabled_services_are_preserved(self):
        custom = OLD.replace('      PRIVATE_FIXTURE:',
                             '      APP_TRUSTED_PROXIES: "10.2.3.4/32"\n      PRIVATE_FIXTURE:')
        custom = custom.replace('  portal-migrate:', '#  portal-migrate:')
        # 使用真实裁剪布局：服务整块注释，不只注释服务名。
        custom = custom[:custom.index('#  portal-migrate:')] + (
            '#  portal-migrate:\n#    image: never-enable\n')
        (self.target / 'docker-compose.yml').write_text(custom)
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), custom)

    # 用途：拒绝无法安全定向升级的结构且不触碰安装前文件。
    # 参数：无。
    # 返回值：无；非法结构成功或留下部分更新时断言失败。
    def test_unsupported_and_duplicate_structure_is_atomic(self):
        for old in (
            OLD + '  portal-api:\n    image: duplicate\n',
            OLD.replace('    environment:\n', '    environment: {X: value}\n'),
            OLD.replace('      PRIVATE_FIXTURE: unchanged', '      - X=value'),
            OLD.replace('      PRIVATE_FIXTURE: unchanged',
                        '      APP_TRUSTED_PROXIES: x\n      APP_TRUSTED_PROXIES: y'),
            OLD.replace('    environment:\n', '    environment:\n    environment:\n'),
            OLD.replace('  portal-migrate:\n', '  portal-migrate:\n    <<: *custom\n'),
        ):
            with self.subTest(old=old):
                (self.target / 'docker-compose.yml').write_text(old)
                result = self.install()
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual((self.target / 'docker-compose.yml').read_text(), old)
                self.assertFalse((self.target / 'runtime/.assets-install-transaction').exists())

    # 用途：模拟事务提交失败并通过原生恢复保留历史现场配置。
    # 参数：无。
    # 返回值：无；恢复失败或配置改变时断言失败。
    def test_failed_commit_can_recover_previous_compose(self):
        self.executable('mv', '''#!/bin/bash
if [[ "${!#}" == "$TEST_TARGET/docker-compose.yml" ]]; then exit 29; fi
exec /bin/mv "$@"
''')
        self.env['TEST_TARGET'] = str(self.target)
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.target / 'runtime/.assets-install-transaction').exists())
        result = self.install(recover=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'docker-compose.yml').read_text(), OLD)
        self.assertEqual((self.target / '.env').read_text(), 'PRIVATE_FIXTURE=must-not-change\n')


class PortalPropagationTests(unittest.TestCase):
    # 用途：执行部署脚本中的真实门禁函数，替换只读 Docker 数据来源。
    # 参数：config 为渲染模型，addresses 为前端网络，actual 为容器环境，component 为可选入口。
    # 返回值：门禁子进程；不调用真实 Docker 或运行部署入口。
    def gate(self, config, addresses=None, actual=None, component=None):
        source = (ROOT / 'bin/deploy-customer-opportunity.sh').read_text()
        marker = '# portal_proxy_preflight 用途：'
        self.assertTrue(marker in source, 'Portal 发布缺少运行配置传播门禁')
        functions = source[source.index(marker):source.index('# 单模块入口')]
        with tempfile.TemporaryDirectory(prefix='portal-proxy-gate-') as directory:
            root = Path(directory)
            (root / 'config.json').write_text(json.dumps(config))
            (root / 'addresses.json').write_text(json.dumps(addresses or {
                'fixture-network': {'IPAddress': '172.18.255.250', 'GlobalIPv6Address': ''}}))
            (root / 'actual.json').write_text(json.dumps(actual if actual is not None else [
                'APP_TRUSTED_PROXIES=127.0.0.1/32,172.18.255.250/32']))
            harness = '''set -Eeuo pipefail
compose() {
  if [[ "$1" == config ]]; then cat "$TEST_ROOT/config.json"; else echo fixture-container; fi
}
docker() {
  if [[ "$*" == *NetworkSettings* ]]; then cat "$TEST_ROOT/addresses.json"
  else cat "$TEST_ROOT/actual.json"; fi
}
source() { return 0; }
publish_registered_customer() { echo published-fixture; }
'''
            code = functions + '\nportal_proxy_preflight && verify_portal_proxy_runtime\n'
            if component:
                code = source[:source.index('\nusage()')]
            return subprocess.run(['bash', '-c', harness + code, '--',
                                   component or '', 'fixture/portal@sha256:' + 'a' * 64],
                                  env=dict(os.environ, TEST_ROOT=str(root)),
                                  capture_output=True, text=True)

    # 用途：构造与实际 docker compose config 相同形态的最小模型。
    # 参数：proxy 为 Portal 显式渲染后的可信代理值。
    # 返回值：包含共享网络与两类 Portal 服务的模型。
    def config(self, proxy='127.0.0.1/32,172.18.255.250/32'):
        return {'services': {
            'portal-api': {'environment': {'APP_TRUSTED_PROXIES': proxy},
                           'networks': {'application': {}}},
            'portal-migrate': {'environment': {'APP_TRUSTED_PROXIES': proxy}},
            'frontend': {'networks': {'application': {}}}},
            'networks': {'application': {'name': 'fixture-network'}}}

    # 用途：允许真实入口地址被信任且运行容器使用渲染权威值。
    # 参数：无。
    # 返回值：无；正确地址链被误拒绝时断言失败。
    def test_frontend_coverage_and_runtime_value_pass(self):
        result = self.gate(self.config())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        custom = '172.18.0.0/16'
        result = self.gate(self.config(custom), actual=['APP_TRUSTED_PROXIES=' + custom])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    # 用途：拒绝缺键、未覆盖真实前端、迁移不一致或容器值漂移。
    # 参数：无。
    # 返回值：无；健康新镜像也不能绕过缺失配置门禁。
    def test_invalid_effective_or_runtime_configuration_fails(self):
        configs = [self.config(''), self.config('127.0.0.1/32'),
                   self.config('172.31.255.250/32'), self.config('not-a-cidr')]
        missing = self.config()
        missing['services']['portal-api']['environment'] = {}
        configs.append(missing)
        mismatch = self.config()
        mismatch['services']['portal-migrate']['environment']['APP_TRUSTED_PROXIES'] = '::1/128'
        configs.append(mismatch)
        disconnected = self.config()
        disconnected['services']['frontend']['networks'] = {'other': {}}
        configs.append(disconnected)
        for config in configs:
            with self.subTest(config=config):
                result = self.gate(config)
                self.assertNotEqual(result.returncode, 0)
        for actual in ([], ['APP_TRUSTED_PROXIES=127.0.0.1/32'],
                       ['APP_TRUSTED_PROXIES=x', 'APP_TRUSTED_PROXIES=y']):
            result = self.gate(self.config(), actual=actual)
            self.assertNotEqual(result.returncode, 0)
        result = self.gate(self.config(), addresses={
            'other-network': {'IPAddress': '172.18.255.250'}})
        self.assertNotEqual(result.returncode, 0)

    # 用途：验证单组件入口只在发布 Portal 时执行前后配置门禁。
    # 参数：无。
    # 返回值：无；CRM 被误拦截或 Portal 带错配置成功时断言失败。
    def test_single_component_entry_checks_only_portal(self):
        result = self.gate(self.config(''), actual=[], component='customer-opportunity')
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.gate(self.config(), component='customer-portal')
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.gate(self.config(''), component='customer-portal')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('published-fixture', result.stdout)
        result = self.gate(self.config(), actual=[], component='customer-portal')
        self.assertNotEqual(result.returncode, 0)

    # 用途：核验生产权威编排通过实际 Compose 解析传播现场网段。
    # 参数：无。
    # 返回值：无；API、迁移或平台的有效代理值不一致时断言失败。
    def test_production_compose_uses_custom_proxy_source(self):
        expected = '127.0.0.1/32,::1/128,172.18.0.1/32,172.18.255.250/32'
        env = dict(os.environ, APP_TRUSTED_PROXIES=expected,
                   BASIC_PLATFORM_RUNTIME_ENV_FILE=str(ROOT / '.env.example'),
                   PLATFORM_IMAGE='fixture/platform:test', FRONTEND_IMAGE='fixture/frontend:test',
                   FILE_GATEWAY_IMAGE='fixture/gateway:test')
        result = subprocess.run(['docker', 'compose', '--env-file', str(ROOT / '.env.example'),
                                 '--file', str(ROOT / 'docker-compose.yml'),
                                 'config', '--format', 'json'],
                                env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        model = json.loads(result.stdout)
        for name in ('portal-api', 'portal-migrate', 'platform-api'):
            self.assertEqual(model['services'][name]['environment']['APP_TRUSTED_PROXIES'], expected)
        network = model['networks']['application']['name']
        result = self.gate(model, addresses={network: {'IPAddress': '172.18.255.250'}},
                           actual=['APP_TRUSTED_PROXIES=' + expected])
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == '__main__':
    unittest.main()
