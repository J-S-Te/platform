#!/usr/bin/env python3
"""回归旧版部署传输与执行顺序，不使用真实 Docker 或授权。"""
import importlib.util
import base64
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[4]
SOURCE = (ROOT / '服务器一键离线部署.sh').read_text()
DEPLOY_SOURCE = (ROOT / 'platform/deploy/production/bin/deploy.sh').read_text()
spec = importlib.util.spec_from_file_location('upgrade_fixture', Path(__file__).with_name('test-oneclick-upgrade-preflight.py'))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


def function(source, name):
    return name + '() {' + source.split(name + '() {', 1)[1].split('\n}', 1)[0] + '\n}'


class LegacyArgumentTests(unittest.TestCase):
    def test_mode_and_conflict_validation(self):
        fn = function(SOURCE, 'configure_legacy_license_mode')
        defaults = dict(legacy_license_mode='bridge', legacy_license_customer='gays-sq',
                        deployment_mode='auto', license_customer='', license_file_arg='', upgrade_subsystems_raw='')
        cases = [({}, 0), ({'legacy_license_mode': 'upgrade', 'upgrade_subsystems_raw': 'contract'}, 0),
                 ({'license_customer': 'gays-sq'}, 0), ({'upgrade_subsystems_raw': 'contract'}, 7),
                 ({'license_file_arg': '/license.jws'}, 7), ({'license_customer': 'different'}, 7),
                 ({'legacy_license_mode': 'upgrade'}, 7), ({'legacy_license_customer': 'bad customer'}, 7),
                 ({'deployment_mode': 'install'}, 7), ({'deployment_mode': 'add-subsystems'}, 7)]
        for overrides, expected in cases:
            with self.subTest(overrides=overrides):
                values = dict(defaults, **overrides)
                code = 'set -Eeuo pipefail\ndie() { exit 7; };\n' + fn + '\n'
                code += '\n'.join(k + '=' + shlex.quote(v) for k, v in values.items())
                code += '\nconfigure_legacy_license_mode\nprintf "%s|%s" "$deployment_mode" "$license_customer"'
                out = subprocess.run(['bash', '-c', code], capture_output=True, text=True)
                self.assertEqual(out.returncode, expected, out.stderr)
                if expected == 0:
                    self.assertEqual(out.stdout, 'upgrade|gays-sq')

    def test_sudo_preserves_legacy_entry_and_no_automatic_full_set_actions(self):
        sudo = SOURCE.split('if ((EUID != 0)); then', 1)[1].split('\nfi', 1)[0]
        self.assertIn('sudo_args+=("--legacy-license-${legacy_license_mode}" "$legacy_license_customer")', sudo)
        self.assertNotIn('bash ./bin/deploy.sh license-migrate\n', SOURCE)
        self.assertNotIn('bash ./bin/deploy.sh license-activate\n', SOURCE)


@unittest.skipUnless(platform.system() == 'Linux' and os.geteuid() == 0, 'Linux root fixture required')
class LegacyFlowTests(unittest.TestCase):
    sidecar = fixture.UpgradeFlowTests.sidecar
    refresh_media = fixture.UpgradeFlowTests.refresh_media
    run_upgrade = fixture.UpgradeFlowTests.run_upgrade
    operations = fixture.UpgradeFlowTests.operations
    compatibility_fixture = fixture.UpgradeFlowTests.compatibility_fixture

    # 用途：限定旧合同迁移的测试交付范围，不夹带未批准的其他应用。
    # 参数：无。
    # 返回值：无；真实交付结构检查器继续运行。
    def setUp(self):
        fixture.UpgradeFlowTests.setUp(self)
        for component in ('customer-opportunity', 'customer-portal'):
            package = self.media / self.packages.pop(component)
            package.unlink()
            Path(str(package) + '.sha256').unlink()
        deploy = fixture.DEPLOY.replace('case "$1" in\n', '''case "$1" in
  license-prepare)
    [[ "${UPGRADE_FREEZE_FAIL:-false}" != true ]] || exit 77 ;;
  license-migrate) exit 98 ;;
''', 1)
        self.assertNotEqual(deploy, fixture.DEPLOY, '维护入口没有接入严格测试边界')
        for directory in (self.target, self.assets):
            fixture.script(directory / 'bin/deploy.sh', deploy)
        fixture.script(self.media / 'license-activate.sh', '#!/usr/bin/env bash\nexit 97\n')
        self.refresh_media()

    # 用途：按实际两组件 OCI 身份构造完整的结构测试批准与新旧安装证据。
    # 参数：无。
    # 返回值：无；这些材料仅用于临时夹具，不是正式签名、许可或现场批准。
    def migration(self):
        block = '  # BEGIN SUBSYSTEM: contract\n  contract-api: {}\n  # END SUBSYSTEM: contract\n'
        (self.target / 'docker-compose.yml').write_text(fixture.COMPOSE + block)
        with (self.assets / 'docker-compose.yml').open('a') as out:
            out.write(block)
        self.plan = self.assets / 'license-installation.json'
        self.plan.write_text(json.dumps(dict(version=1, scenario='migrate', customer_id='gays-sq')))
        _, facts = fixture.FIXTURE.image_package('contract', image_version='test')
        image = facts[fixture.FIXTURE.image_tags('contract')[0]]['config']
        services = [dict(application='contract_management', environment='prod', name=name,
                         image_digest=image, version='test', protocol='1', replicas=1)
                    for name in ('contract-api', 'contract-worker')]
        evidence = dict(version=1, project='uip', installation_boundary=True,
                        services=services, infrastructure=[], excluded_services=[])
        (self.assets / 'license-evidence.json').write_text(json.dumps(evidence))
        self.old_evidence = self.assets / 'license-migration-evidence.json'
        self.old_evidence.write_text(json.dumps(evidence))
        self.approval = self.assets / 'runtime-license-contract_management-prod.json'
        self.approval.write_text(json.dumps(dict(version=1, application='contract_management',
            environment='prod', components=[dict(service_id=item['name'], protocol=1,
            coverage_digest='sha256:'+'c'*64, image_digest=image) for item in services])))
        self.refresh_media()

    # 用途：创建仅结构有效的测试 JWS，真实签名与 ACK 门禁不以此替代。
    # 参数：无。
    # 返回值：无；许可证只写入本测试临时资产目录。
    def add_structural_license(self):
        (self.assets / 'license').mkdir()
        header = dict(alg='EdDSA', typ='commercial-license+jws', kid='test-only')
        claims = dict(protocol_version=1, issuer='unified-business-suite-vendor',
            product_id='unified-business-suite', license_id='fixture-license', customer_id='gays-sq',
            instance_id='test-instance', environment='production', version=1, issued_at=1700000000,
            not_before=1700000000, applications=[dict(code='contract_management', kind='FULL',
            not_before=1700000000, expires_at=2000000000)])
        parts = [json.dumps(header).encode(), json.dumps(claims).encode(), bytes(64)]
        self.license_bytes = b'.'.join(base64.urlsafe_b64encode(part).rstrip(b'=') for part in parts)
        (self.assets / 'license/commercial-license.jws').write_bytes(self.license_bytes)

    def migrate(self, *args):
        return self.run_upgrade('--legacy-license-upgrade', 'gays-sq', '--upgrade-subsystems', 'contract', *args)

    # 用途：同时确认准确拒绝原因、未备份/发布和旧业务包原样保留。
    # 参数：result 是入口结果，reason 是当前门禁应报告的具体原因或检查编号。
    # 返回值：无；任何安全断言不符时测试失败。
    def assert_stopped_before_backup(self, result, reason):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(reason, result.stdout + result.stderr)
        self.assertEqual(self.operations(), [])
        self.assertEqual(self.old_contract.read_bytes(), b'old contract transport')

    def test_bridge_preserves_business_and_prints_actual_request_command(self):
        result = subprocess.run(['bash', str(fixture.WRAPPER), '--media', str(self.media), '--target', str(self.target),
                                 '--legacy-license-bridge', 'gays-sq', '--apply-compose', '--skip-host-deps', '--yes'],
                                env=self.env, text=True, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn('license-prepare', self.operations())
        self.assertFalse(any('contract' in line for line in self.operations()))
        self.assertEqual(self.old_contract.read_bytes(), b'old contract transport')
        self.assertIn('--customer gays-sq --confirm-initialize', result.stdout)

    def test_bridge_rejects_migration_media_before_backup(self):
        self.migration()
        self.assert_stopped_before_backup(self.run_upgrade('--legacy-license-bridge', 'gays-sq', '--check-only'), 'bridge 介质不能携带授权安装计划')

    # 用途：验证 bridge 拒绝结构完整但与其范围不符的运行批准。
    # 参数：无。
    # 返回值：无；不得进入备份或发布。
    def test_bridge_rejects_approval_without_plan(self):
        # 完整批准不应被 bridge 接受；先保持统一结构检查可以通过。
        self.migration()
        self.plan.unlink()
        self.refresh_media()
        self.assert_stopped_before_backup(self.run_upgrade('--legacy-license-bridge', 'gays-sq', '--check-only'), '不能携带迁移或运行批准')

    # 用途：验证 bridge 拒绝随介质携带的结构测试许可证。
    # 参数：无。
    # 返回值：无；不得修改旧业务包。
    def test_bridge_rejects_bundled_license(self):
        self.migration()
        self.plan.unlink()
        self.add_structural_license()
        self.refresh_media()
        self.assert_stopped_before_backup(self.run_upgrade('--legacy-license-bridge', 'gays-sq', '--check-only'), '不能携带授权安装计划或许可证')

    # 用途：验证旧迁移保留随包许可材料但不自动激活。
    # 参数：无。
    # 返回值：无；字节保存或阶段边界不符时失败。
    def test_bundled_license_is_preserved_without_automatic_activation(self):
        self.migration()
        self.add_structural_license()
        self.refresh_media()
        result = self.migrate('--apply-compose')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'license/commercial-license.jws').read_bytes(), self.license_bytes)
        self.assertNotIn('license-activate', self.operations())
        self.assertIn('整个签署购买集合', result.stdout)

    def test_migration_freezes_before_business_package_replacement_import_or_stage(self):
        self.migration()
        result = self.migrate('--apply-compose')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        ops = self.operations()
        self.assertLess(ops.index('deploy platform'), ops.index('license-prepare'))
        self.assertLess(ops.index('license-prepare'), next(i for i, line in enumerate(ops) if self.packages['contract'] in line))
        self.assertLess(ops.index('license-prepare'), ops.index('upgrade contract'))
        self.assertNotIn('license-migrate', ops)
        self.assertIn('整个实际冻结旧集合', result.stdout)

    def test_freeze_failure_preserves_old_business_package_and_stops_staging(self):
        self.migration()
        self.env['UPGRADE_FREEZE_FAIL'] = 'true'
        result = self.migrate('--apply-compose')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('基线冻结失败', result.stderr)
        self.assertIn('deploy platform', self.operations())
        self.assertNotIn('upgrade contract', self.operations())
        self.assertFalse(any(self.packages['contract'] in op for op in self.operations()))
        self.assertEqual(self.old_contract.read_bytes(), b'old contract transport')
        self.assertFalse((self.target / 'packages' / self.packages['contract']).exists())

    def test_migration_check_only_does_not_initialize_freeze_or_mutate_target(self):
        self.migration()
        before = {p.relative_to(self.target): fixture.digest(p) for p in self.target.rglob('*') if p.is_file()}
        result = self.migrate('--check-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.operations(), [])
        self.assertEqual(before, {p.relative_to(self.target): fixture.digest(p) for p in self.target.rglob('*') if p.is_file()})

    # 用途：验证缺计划、错误迁移场景或客户不符均在备份前拒绝。
    # 参数：无。
    # 返回值：无；要求命中对应真实门禁而非任意失败。
    def test_missing_or_wrong_plan_stops_before_backup(self):
        self.migration()
        for plan, reason in [(None, 'license-installation.json'),
                             (dict(version=1, scenario='fresh', customer_id='gays-sq'),
                              '[FAIL] authorization.baseline'),
                             (dict(version=1, scenario='migrate', customer_id='other'), '客户与命令一致')]:
            if plan is None:
                self.plan.unlink()
            else:
                self.plan.write_text(json.dumps(plan))
            self.refresh_media()
            self.assert_stopped_before_backup(self.migrate('--check-only'), reason)

    # 用途：验证缺旧安装边界或目标批准命中准确材料门禁。
    # 参数：无。
    # 返回值：无；任何情况下不得修改旧包或执行发布。
    def test_missing_legacy_or_target_approval_stops_before_backup(self):
        self.migration()
        for path in (self.old_evidence, self.approval):
            content = path.read_bytes()
            path.unlink()
            self.refresh_media()
            expected = ('authorization.baseline' if path == self.old_evidence else
                        'authorization.release.contract_management')
            self.assert_stopped_before_backup(self.migrate('--check-only'), expected)
            path.write_bytes(content)

    # 用途：验证不能将其他系统身份拼入旧合同迁移边界。
    # 参数：无。
    # 返回值：无；结构有效的旧边界不含合同，必须由旧安装资格门禁拒绝。
    def test_new_system_cannot_claim_legacy_migration(self):
        self.migration()
        evidence = json.loads(self.old_evidence.read_text())
        evidence['services'] = [dict(application='customer_and_opportunity', environment='prod',
            name='customer-api', image_digest='sha256:' + 'b' * 64,
            version='old-crm', protocol='1', replicas=1)]
        self.old_evidence.write_text(json.dumps(evidence))
        self.refresh_media()
        self.assert_stopped_before_backup(self.migrate('--check-only'), '请求系统不是批准的旧安装')

    # 用途：另行保留应用与服务名不匹配的结构拒绝，不代替旧安装资格检查。
    # 参数：无。
    # 返回值：无；统一材料检查必须在备份前拒绝。
    def test_malformed_old_service_identity_is_rejected_by_structure_check(self):
        self.migration()
        evidence = json.loads(self.old_evidence.read_text())
        evidence['services'][0]['application'] = 'customer_and_opportunity'
        self.old_evidence.write_text(json.dumps(evidence))
        self.refresh_media()
        self.assert_stopped_before_backup(self.migrate('--check-only'), '[FAIL] authorization.baseline')

    def test_invalid_target_application_or_json_stops_before_backup(self):
        self.migration()
        for value in ('{"version":1,"application":"other","environment":"prod"}', '{bad'):
            self.approval.write_text(value)
            self.refresh_media()
            self.assert_stopped_before_backup(self.migrate('--check-only'), '批准')

    def test_old_maintenance_entry_rejected_during_preflight(self):
        self.migration()
        fixture.script(self.assets / 'bin/deploy.sh', fixture.DEPLOY)
        self.refresh_media()
        self.assert_stopped_before_backup(self.migrate('--check-only'), '缺少新版授权维护入口')


class MaintenanceEntryTests(unittest.TestCase):
    # 用途：执行完整真实维护入口，仅隔离 Docker 与宿主 stat 命令。
    # 参数：command/args 是真实子命令；其余参数控制临时许可证与计划状态。
    # 返回值：实际入口退出结果，初始化、参数、JSON 及许可校验均保持真实。
    def run_entry(self, command, *args, license_present=False, malformed=False,
                  initialized=True, runtime_mode=0o600):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            shutil.copytree(ROOT / 'platform/deploy/production/bin', root / 'bin',
                            ignore=shutil.ignore_patterns('__pycache__', '*.pyc', '.mimosa'))
            (root / 'docker-compose.yml').write_text('services: {}\n')
            (root / '.env').write_text('PUBLIC_ACCESS_MODE=ip\nPUBLIC_PLATFORM_HOST=192.0.2.10\n'
                                      'PUBLIC_SSO_HOST=192.0.2.10\nPUBLIC_HTTP_PORT=8081\n'
                                      'KEYCLOAK_HTTP_PORT=18090\n')
            (root / '.release.env').write_text('')
            for name in ('.env', '.release.env'):
                (root / name).chmod(runtime_mode)
            if not initialized:
                (root / '.env').unlink()
            tools = root / 'tools'
            tools.mkdir()
            docker = tools / 'docker'
            docker.write_text('#!' + sys.executable + r'''
import sys
import os,json
a=sys.argv[1:]
with open(os.environ['MAINTENANCE_DOCKER_LOG'],'a') as stream: stream.write(json.dumps(a)+'\n')
if a==['network','inspect','basic-platform-production']: sys.exit(0)
if a[:1]==['compose'] and 'run' in a and 'platform-api' in a:
    command=a[a.index('platform-api')+1:]
    if command[:1]==['./license-install'] and command[1:2] in (['prepare'],['migrate'],['activate']):
        print(' '.join(a)); sys.exit(0)
sys.exit('Unexpected Docker fixture call: '+repr(a))
''')
            docker.chmod(0o755)
            stat_command = tools / 'stat'
            stat_command.write_text('#!' + sys.executable + '''
import os,stat,sys
assert sys.argv[1:3]==['-c','%a'],sys.argv
print(format(stat.S_IMODE(os.stat(sys.argv[3]).st_mode),'o'))
''')
            stat_command.chmod(0o755)
            plan = dict(version=1, scenario='migrate', customer_id='gays-sq')
            if malformed:
                plan['scenario'] = 'unknown'
            (root / 'license-installation.json').write_text(json.dumps(plan))
            if license_present:
                (root / 'license').mkdir()
                (root / 'license/commercial-license.jws').write_text('fixture-JWS-only')
            log = root / 'docker-calls.jsonl'
            env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
                       MAINTENANCE_DOCKER_LOG=str(log))
            result = subprocess.run(['bash', str(root / 'bin/deploy.sh'), command, *args],
                                    env=env, text=True, capture_output=True, timeout=30)
            result.docker_calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
            return result

    # 用途：提取真实 Docker 边界收到的许可证参数，避免混淆 Compose 的 --file。
    # 参数：result 是完整部署入口的执行结果。
    # 返回值：唯一的 license-install 参数列表；缺失或多次调用时测试失败。
    def license_command(self, result):
        calls = [call for call in result.docker_calls if call[:1] == ['compose']]
        self.assertEqual(len(calls), 1, result.stdout + result.stderr)
        return calls[0][calls[0].index('platform-api') + 1:]

    # 用途：验证真实维护入口使用已安装计划，且仅在许可存在时传入许可文件。
    # 参数：无。
    # 返回值：无；精确核对许可证进程的参数范围。
    def test_prepare_uses_installed_plan_and_only_includes_existing_license(self):
        result = self.run_entry('license-prepare')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.license_command(result),
                         ['./license-install', 'prepare', '--scenario', 'migrate', '--customer', 'gays-sq'])
        result = self.run_entry('license-prepare', license_present=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.license_command(result),
                         ['./license-install', 'prepare', '--scenario', 'migrate', '--customer', 'gays-sq',
                          '--file', '/delivery-license/commercial-license.jws'])

    # 用途：确认完整维护入口的初始化门禁没有被测试替身绕过。
    # 参数：无。
    # 返回值：无；缺配置必须在 Docker 边界前阻断。
    def test_uninitialized_target_stops_before_docker(self):
        result = self.run_entry('license-prepare', initialized=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('configure', result.stderr)
        self.assertEqual(result.docker_calls, [])

    # 用途：确认 stat 适配器读取真实权限，不能把不安全配置视为 0600。
    # 参数：无。
    # 返回值：无；错误权限必须在 Docker 边界前阻断。
    def test_unsafe_configuration_permissions_stop_before_docker(self):
        result = self.run_entry('license-prepare', runtime_mode=0o644)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('0600', result.stderr)
        self.assertEqual(result.docker_calls, [])

    # 用途：验证真实迁移/激活路由正确，并保留缺许可证阻断。
    # 参数：无。
    # 返回值：无；错误操作不得抵达 Docker 边界。
    def test_migrate_and_activate_dispatch_and_activation_requires_license(self):
        result = self.run_entry('license-migrate')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.license_command(result), ['./license-install', 'migrate'])
        missing = self.run_entry('license-activate')
        self.assertNotEqual(missing.returncode, 0)
        self.assertEqual(missing.docker_calls, [])
        result = self.run_entry('license-activate', license_present=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.license_command(result),
                         ['./license-install', 'activate', '--file', '/delivery-license/commercial-license.jws'])

    # 用途：验证维护命令不能覆盖客户参数或接受非法场景。
    # 参数：无。
    # 返回值：无；非法输入必须在 Docker 边界前停止。
    def test_maintenance_rejects_override_arguments_or_invalid_plan(self):
        for command in ('license-prepare', 'license-migrate', 'license-activate'):
            result = self.run_entry(command, '--customer', 'other')
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.docker_calls, [])
        result = self.run_entry('license-prepare', malformed=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.docker_calls, [])


if __name__ == '__main__':
    unittest.main()
