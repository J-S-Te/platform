#!/usr/bin/env python3
"""回归旧版部署传输与执行顺序，不使用真实 Docker 或授权。"""
import importlib.util
import json
import os
from pathlib import Path
import platform
import shlex
import subprocess
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

    def setUp(self):
        fixture.UpgradeFlowTests.setUp(self)
        deploy = fixture.DEPLOY.replace('  deploy|verify|', '''  license-prepare)
    [[ "${UPGRADE_FREEZE_FAIL:-false}" != true ]] || exit 77 ;;
  license-migrate) exit 98 ;;
  deploy|verify|''')
        for directory in (self.target, self.assets):
            fixture.script(directory / 'bin/deploy.sh', deploy)
        fixture.script(self.media / 'license-activate.sh', '#!/usr/bin/env bash\nexit 97\n')
        self.refresh_media()

    def migration(self):
        block = '  # BEGIN SUBSYSTEM: contract\n  contract-api: {}\n  # END SUBSYSTEM: contract\n'
        (self.target / 'docker-compose.yml').write_text(fixture.COMPOSE + block)
        with (self.assets / 'docker-compose.yml').open('a') as out:
            out.write(block)
        self.plan = self.assets / 'license-installation.json'
        self.plan.write_text(json.dumps(dict(version=1, scenario='migrate', customer_id='gays-sq')))
        (self.assets / 'license-evidence.json').write_text('{"version":1}')
        self.old_evidence = self.assets / 'license-migration-evidence.json'
        self.old_evidence.write_text(json.dumps(dict(version=1, project='uip', installation_boundary=True,
                                                    services=[dict(application='contract_management', environment='prod')],
                                                    infrastructure=[], excluded_services=[])))
        self.approval = self.assets / 'runtime-license-contract_management-prod.json'
        self.approval.write_text('{"version":1,"application":"contract_management","environment":"prod","components":[]}')
        self.refresh_media()

    def migrate(self, *args):
        return self.run_upgrade('--legacy-license-upgrade', 'gays-sq', '--upgrade-subsystems', 'contract', *args)

    def assert_stopped_before_backup(self, result, reason):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(reason, result.stderr)
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

    def test_bridge_rejects_approval_without_plan(self):
        (self.assets / 'license-evidence.json').write_text('{"version":1}')
        self.refresh_media()
        self.assert_stopped_before_backup(self.run_upgrade('--legacy-license-bridge', 'gays-sq', '--check-only'), '不能携带迁移或运行批准')

    def test_bridge_rejects_bundled_license(self):
        (self.assets / 'license').mkdir()
        (self.assets / 'license/commercial-license.jws').write_text('fixture-JWS-only')
        self.refresh_media()
        self.assert_stopped_before_backup(self.run_upgrade('--legacy-license-bridge', 'gays-sq', '--check-only'), '不能携带授权安装计划或许可证')

    def test_bundled_license_is_preserved_without_automatic_activation(self):
        self.migration()
        (self.assets / 'license').mkdir()
        (self.assets / 'license/commercial-license.jws').write_text('fixture-JWS-only')
        self.refresh_media()
        result = self.migrate('--apply-compose')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.target / 'license/commercial-license.jws').read_text(), 'fixture-JWS-only')
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

    def test_missing_or_wrong_plan_stops_before_backup(self):
        self.migration()
        for plan, reason in [(None, 'license-installation.json'),
                             (dict(version=1, scenario='fresh', customer_id='gays-sq'), '必须是 migrate'),
                             (dict(version=1, scenario='migrate', customer_id='other'), '客户与命令一致')]:
            if plan is None:
                self.plan.unlink()
            else:
                self.plan.write_text(json.dumps(plan))
            self.refresh_media()
            self.assert_stopped_before_backup(self.migrate('--check-only'), reason)

    def test_missing_legacy_or_target_approval_stops_before_backup(self):
        self.migration()
        for path in (self.old_evidence, self.approval):
            content = path.read_bytes()
            path.unlink()
            self.refresh_media()
            self.assert_stopped_before_backup(self.migrate('--check-only'), path.name)
            path.write_bytes(content)

    def test_new_system_cannot_claim_legacy_migration(self):
        self.migration()
        evidence = json.loads(self.old_evidence.read_text())
        evidence['services'][0]['application'] = 'customer_and_opportunity'
        self.old_evidence.write_text(json.dumps(evidence))
        self.refresh_media()
        self.assert_stopped_before_backup(self.migrate('--check-only'), '不是批准的旧安装')

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
    def run_entry(self, command, *args, license_present=False, malformed=False):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            plan = dict(version=1, scenario='migrate', customer_id='gays-sq')
            if malformed:
                plan['scenario'] = 'unknown'
            (root / 'license-installation.json').write_text(json.dumps(plan))
            if license_present:
                (root / 'license').mkdir()
                (root / 'license/commercial-license.jws').write_text('fixture-JWS-only')
            prefix = 'set -Eeuo pipefail\ndie() { echo "$*" >&2; exit 7; }; require_initialized() { :; }; compose() { printf "%s\\n" "$*"; };\n'
            prefix += 'deploy_dir=' + shlex.quote(str(root)) + '\n'
            prefix += function(DEPLOY_SOURCE, 'prepare_license_installation') + '\n'
            prefix += function(DEPLOY_SOURCE, 'install_delivery_license') + '\n'
            prefix += 'command=' + shlex.quote(command) + '\nset -- ' + ' '.join(map(shlex.quote, (command, *args))) + '\n'
            prefix += 'case "$command" in' + DEPLOY_SOURCE.rsplit('case "$command" in', 1)[1]
            return subprocess.run(['bash', '-c', prefix], text=True, capture_output=True)

    def test_prepare_uses_installed_plan_and_only_includes_existing_license(self):
        result = self.run_entry('license-prepare')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('./license-install prepare --scenario migrate --customer gays-sq', result.stdout)
        self.assertNotIn('--file', result.stdout)
        result = self.run_entry('license-prepare', license_present=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('--file /delivery-license/commercial-license.jws', result.stdout)

    def test_migrate_and_activate_dispatch_and_activation_requires_license(self):
        result = self.run_entry('license-migrate')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('./license-install migrate', result.stdout)
        self.assertNotEqual(self.run_entry('license-activate').returncode, 0)
        result = self.run_entry('license-activate', license_present=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('./license-install activate --file /delivery-license/commercial-license.jws', result.stdout)

    def test_maintenance_rejects_override_arguments_or_invalid_plan(self):
        for command in ('license-prepare', 'license-migrate', 'license-activate'):
            result = self.run_entry(command, '--customer', 'other')
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn('run -T', result.stdout)
        result = self.run_entry('license-prepare', malformed=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('run -T', result.stdout)


if __name__ == '__main__':
    unittest.main()
