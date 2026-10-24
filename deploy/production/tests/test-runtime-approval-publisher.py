#!/usr/bin/env python3
"""仅进行文件系统测试，不访问真实 Docker、API 或数据库。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import tarfile
import unittest
from unittest.mock import patch, MagicMock
from types import SimpleNamespace
import signal


SOURCE = Path(__file__).resolve().parents[1] / 'bin' / 'publish-runtime-approvals.py'
APPS = {
    'customer_and_opportunity': ['customer-api', 'customer-opportunity-alert-worker',
        'customer-owner-notification-worker', 'customer-contract-transfer-worker',
        'customer-presale-alert-worker', 'customer-presale-assignment-notification-worker',
        'customer-presale-progress-notification-worker', 'customer-notification-delivery-worker',
        'customer-presale-worker'],
    'contract_management': ['contract-api', 'contract-worker'],
    'project_management': ['project-api', 'project-sla-notifier'],
    'data_analysis': ['data-analysis-api', 'data-analysis-aggregation-worker', 'data-analysis-alert-worker'],
}
OLD_IMAGE, NEW_IMAGE, COVERAGE = ('sha256:' + c * 64 for c in 'abc')
RELEASE_FILE_NAMES = {f'runtime-license-{app}-prod.json' for app in APPS}
EXCLUDED = ['docker-socket-proxy', 'contract-mysql', 'file-gateway-mysql', 'keycloak-backup-metrics',
            'prometheus', 'keycloak', 'platform-mysql', 'customer-mysql', 'temporal', 'keycloak-db',
            'project-mysql', 'frontend', 'subsystem-provisioner', 'data-analysis-mysql', 'file-gateway',
            'platform-api', 'platform-worker']


def encoded(value):
    return (json.dumps(value, sort_keys=True, indent=2) + '\n').encode()


def put(path, raw):
    path.write_bytes(raw)
    path.chmod(0o600)


class PublisherTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='approval-publisher-')
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.root, self.bundle = self.base / 'deploy', self.base / 'bundle'
        for p in [self.root, self.bundle, self.root / 'bin', self.root / 'runtime',
                  self.root / 'runtime-approvals', self.root / 'backups']:
            p.mkdir(mode=0o700)
        # 测试绝不加载真实运行配置。
        for name in ['.env', '.release.env', 'docker-compose.yml', 'license-installation.json',
                     'bin/backup-all.sh', 'bin/lifecycle.sh', 'bin/provisioner-config-refresh.sh']:
            put(self.root / name, b'fixture-only-never-execute\n')
        put(self.root / '.env', ('COMPOSE_PROJECT_NAME=uip\nSUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=' + str(self.root) + '\n').encode())
        self.services, before = [], []
        for app, names in APPS.items():
            for name in names:
                item = dict(application=app, environment='prod', name=name,
                            image_digest=NEW_IMAGE, version='r4', protocol='1', replicas=1)
                self.services.append(item)
                if name != 'customer-contract-transfer-worker':
                    before.append(dict(item, image_digest=OLD_IMAGE, version='old'))
            release = dict(version=1, application=app, environment='prod',
                release_generation='r4', components=[dict(service_id=name, protocol=1,
                    image_digest=NEW_IMAGE, coverage_digest=COVERAGE) for name in names])
            name = f'runtime-license-{app}-prod.json'
            put(self.bundle / name, encoded(release))
            old = dict(release, release_generation='old', components=[dict(c, image_digest=OLD_IMAGE)
                for c in release['components'] if c['service_id'] != 'customer-contract-transfer-worker'])
            put(self.root / 'runtime-approvals' / name, encoded(old))
            put(self.root / name, encoded(old))
        infra = [dict(application='data_analysis', environment='prod', name='data-analysis-metabase',
                      image_digest='sha256:' + 'd' * 64, replicas=1, gateway_service='data-analysis-api')]
        evidence = dict(version=1, project='uip', installation_boundary=True,
                        excluded_services=sorted(EXCLUDED), services=self.services, infrastructure=infra)
        put(self.bundle / 'license-evidence.json', encoded(evidence))
        old_evidence = dict(evidence, services=before)
        put(self.bundle / 'upgrade-before.json', encoded(old_evidence))
        put(self.root / 'runtime-approvals' / 'license-evidence.json', encoded(old_evidence))
        put(self.root / 'license-evidence.json', encoded(old_evidence))
        put(self.bundle / 'approval-review.json', encoded(dict(operator='fixture-reviewer',
            candidate=False, input_reviewed=True, created_at='2026-10-19T01:00:00Z')))
        put(self.bundle / 'upgrade-review.json', encoded(dict(version=1,
            inventory_sha256='1' * 64, mapping_sha256='2' * 64, registry_sha256='3' * 64,
            target_facts_sha256='4' * 64, expected_sha256='5' * 64)))
        self.rehash()

    def rehash(self):
        rows = [hashlib.sha256(p.read_bytes()).hexdigest() + '  ' + p.name + '\n'
                for p in sorted(self.bundle.iterdir()) if p.name != 'SHA256SUMS']
        put(self.bundle / 'SHA256SUMS', ''.join(rows).encode())
        self.digest = hashlib.sha256((self.bundle / 'SHA256SUMS').read_bytes()).hexdigest()

    def run_cli(self, *args):
        env = dict(os.environ, PATH='/usr/bin:/bin:/usr/sbin:/sbin', PYTHONDONTWRITEBYTECODE='1')
        return subprocess.run([sys.executable, str(SOURCE), '--deploy-root', str(self.root),
            '--bundle', str(self.bundle), '--bundle-sha256', self.digest, *args],
            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, timeout=10)

    def snapshot(self):
        return {str(p.relative_to(self.root)): (p.read_bytes(), p.stat().st_ino, p.stat().st_mode)
                for p in self.root.rglob('*') if p.is_file()}

    def test_default_verifies_without_docker_or_filesystem_changes(self):
        # 意外默认启用的写入器会改变字节或 inode，或调用不存在的 Docker。
        before = self.snapshot()
        result = self.run_cli()
        self.assertEqual(result.returncode, 0, result.stderr)
        proof = json.loads(result.stdout)
        self.assertEqual(proof['mode'], 'verify-only')
        self.assertEqual(proof['consumers'], 16)
        self.assertRegex(proof['before_sha256'], r'^[a-f0-9]{64}$')
        self.assertEqual(self.snapshot(), before)

    def test_legacy_twenty_item_metadata_is_backed_up_not_reused_as_native_running_proof(self):
        config_id = 'sha256:' + 'e' * 64
        for name in sorted(self.bundle.iterdir()):
            if name.name not in RELEASE_FILE_NAMES:
                continue
            current = self.root / 'runtime-approvals' / name.name
            value = json.loads(current.read_text())
            for component in value['components']:
                component['image_digest'] = config_id
            put(current, encoded(value))
            put(self.root / name.name, encoded(value))
        old = json.loads((self.root / 'license-evidence.json').read_text())
        for item in old['services']:
            item['image_digest'] = config_id
        for app, names in {'customer_portal': ['portal-api', 'portal-report-worker', 'portal-invite-compensation-worker'],
                           'settlement': ['settlement-api', 'settlement-worker']}.items():
            for name in names:
                old['services'].append(dict(application=app, environment='prod', name=name, image_digest=config_id,
                    version='old', protocol='1', replicas=1))
        old['infrastructure'][0]['image_digest'] = config_id
        old['excluded_services'] = []
        original = encoded(old)
        put(self.root / 'license-evidence.json', original)
        put(self.root / 'runtime-approvals/license-evidence.json', original)
        result = self.run_cli()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)['legacy_inventory_drift'])
        publisher = self.publisher()
        with patch.object(os, 'geteuid', return_value=0):
            proof = publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertEqual((Path(proof['backup']) / 'runtime-approvals/license-evidence.json').read_bytes(), original)
        self.assertEqual((self.root / 'runtime-approvals/license-evidence.json').read_bytes(),
                         (self.bundle / 'license-evidence.json').read_bytes())

    def test_untrusted_manifest_and_candidate_do_not_mutate(self):
        before = self.snapshot()
        put(self.bundle / 'approval-review.json', encoded(dict(operator='fixture', candidate=True,
            input_reviewed=False, created_at='2026-10-19T01:00:00Z')))
        self.assertNotEqual(self.run_cli().returncode, 0)
        self.rehash()
        result = self.run_cli()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('formal upgrade', result.stderr)
        self.assertEqual(self.snapshot(), before)

    def publisher(self, failure=None):
        spec = importlib.util.spec_from_file_location('approval_publisher', SOURCE)
        module = importlib.util.module_from_spec(spec)
        with patch.object(sys, 'dont_write_bytecode', True):
            spec.loader.exec_module(module)
        self.assertTrue(hasattr(module, 'Publisher'), 'controlled apply implementation is missing')
        result, _ = module.verify(self.root, self.bundle, self.digest)
        self.module = module
        self.runner = FakeCommands(self, failure)
        return module.Publisher(self.root, self.bundle, self.digest, result['before_sha256'], self.runner)

    def test_apply_requires_explicit_maintenance_authorization(self):
        publisher = self.publisher()
        before = self.snapshot()
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaisesRegex(self.module.Rejected, 'maintenance confirmation'):
                publisher.publish('')
        self.assertEqual(self.snapshot(), before)
        self.assertEqual(self.runner.events, [])

    def test_apply_keeps_parent_inode_secrets_and_other_approvals_and_checks_agent_before_api(self):
        put(self.root / 'runtime-approvals' / 'runtime-license-settlement-prod.json', b'{"preserved":true}\n')
        publisher = self.publisher()
        inode = (self.root / 'runtime-approvals').stat().st_ino
        secret = (self.root / '.env').read_bytes()
        with patch.object(os, 'geteuid', return_value=0):
            proof = publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertEqual(proof['mode'], 'published')
        self.assertEqual((self.root / 'runtime-approvals').stat().st_ino, inode)
        self.assertEqual((self.root / '.env').read_bytes(), secret)
        self.assertEqual((self.root / 'license-installation.json').read_bytes(), b'fixture-only-never-execute\n')
        self.assertEqual((self.root / 'runtime-approvals' / 'runtime-license-settlement-prod.json').read_bytes(), b'{"preserved":true}\n')
        for name in [p.name for p in self.bundle.iterdir() if p.name in self.module.RELEASES | {'license-evidence.json'}]:
            self.assertEqual((self.root / 'runtime-approvals' / name).read_bytes(), (self.bundle / name).read_bytes())
            self.assertEqual((self.root / name).read_bytes(), (self.bundle / name).read_bytes())
            self.assertEqual((self.root / name).stat().st_mode & 0o777, 0o600)
        self.assertLess(self.runner.events.index('backup-verified'), self.runner.events.index('api-stop'))
        self.assertLess(self.runner.events.index('agent-proof'), self.runner.events.index('api-open'))
        self.assertEqual(self.runner.old_business_mutations, [])

    def test_backup_failure_cannot_stop_api_or_publish(self):
        publisher = self.publisher('backup')
        before = self.snapshot()
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaises(self.module.Rejected):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertEqual((self.root / 'license-evidence.json').read_bytes(), before['license-evidence.json'][0])
        self.assertNotIn('api-stop', self.runner.events)

    def test_api_stop_failure_is_reported_as_uncontained_not_falsely_closed(self):
        publisher = self.publisher('api-stop-fails')
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaisesRegex(self.module.Rejected, 'manual containment required'):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertNotIn('api-open', self.runner.events)
        self.assertNotEqual((self.root / 'license-evidence.json').read_bytes(),
                            (self.bundle / 'license-evidence.json').read_bytes())

    def test_old_compose_job_failure_and_agent_mismatch_remain_closed(self):
        for failure in ['not-ready', 'agent-mismatch', 'mount-rw']:
            with self.subTest(failure=failure):
                # 每个失败场景使用全新且独立的文件系统事务。
                case = PublisherTest('test_default_verifies_without_docker_or_filesystem_changes')
                case.setUp()
                self.addCleanup(case.doCleanups)
                publisher = case.publisher(failure)
                with patch.object(os, 'geteuid', return_value=0):
                    with self.assertRaises(case.module.Rejected):
                        publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
                self.assertNotIn('api-open', case.runner.events)
                self.assertTrue((case.root / 'runtime/.runtime-approval-publish/journal.json').is_file())
                self.assertTrue((case.root / 'runtime/.control-plane-reload-required').is_file())
                if failure == 'not-ready':
                    self.assertNotEqual((case.root / 'license-evidence.json').read_bytes(),
                                        (case.bundle / 'license-evidence.json').read_bytes())

    def test_unselected_stopped_or_oneoff_business_is_not_metadata_only_correction(self):
        for failure in ['stopped-portal', 'oneoff-settlement']:
            with self.subTest(failure=failure):
                case = PublisherTest('test_default_verifies_without_docker_or_filesystem_changes')
                case.setUp()
                self.addCleanup(case.doCleanups)
                publisher = case.publisher(failure)
                with patch.object(os, 'geteuid', return_value=0):
                    with self.assertRaisesRegex(case.module.Rejected, 'unselected business'):
                        publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
                self.assertNotIn('backup', case.runner.events)
                self.assertNotIn('api-stop', case.runner.events)

    def test_stale_cas_symlink_and_duplicate_json_fail_before_docker(self):
        publisher = self.publisher()
        put(self.root / 'license-evidence.json', b'changed-after-check\n')
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaisesRegex(self.module.Rejected, 'CAS'):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertEqual(self.runner.events, [])
        (self.bundle / 'upgrade-review.json').unlink()
        (self.bundle / 'upgrade-review.json').symlink_to(self.root / '.env')
        self.assertNotEqual(self.run_cli().returncode, 0)
        (self.bundle / 'upgrade-review.json').unlink()
        put(self.bundle / 'upgrade-review.json', b'{"version":1,"version":1}\n')
        self.rehash()
        self.assertIn('duplicate JSON key', self.run_cli().stderr)

    def test_foreign_deployment_config_cannot_be_published(self):
        publisher = self.publisher()
        put(self.root / '.env', b'COMPOSE_PROJECT_NAME=other-project\nSUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=/opt/other-deployment\n')
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaisesRegex(self.module.Rejected, 'deployment identity'):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertEqual(self.runner.events, [])

    def test_subprocesses_use_local_daemon_and_empty_config_not_operator_context(self):
        self.publisher()
        observed = []

        def process(args, **kwargs):
            env = kwargs['env']
            observed.append(dict(env))
            self.assertEqual(env.get('DOCKER_HOST'), 'unix:///var/run/docker.sock')
            self.assertNotIn('DOCKER_CONTEXT', env)
            self.assertEqual(list(Path(env['DOCKER_CONFIG']).iterdir()), [])
            self.assertEqual(env.get('HOME'), os.environ.get('HOME'))
            self.assertTrue(kwargs['start_new_session'])
            return SimpleNamespace(returncode=0, communicate=lambda timeout: (b'local-only\n', b''))

        with patch.dict(os.environ, DOCKER_CONTEXT='unexpected-remote', DOCKER_HOST='tcp://unapproved:2375'):
            with patch.object(subprocess, 'Popen', side_effect=process):
                self.assertEqual(self.module.Commands().call(['docker', 'info']), b'local-only\n')
        self.assertFalse(Path(observed[0]['DOCKER_CONFIG']).exists())

    def test_timeout_terminates_whole_maintenance_group_before_api_containment(self):
        self.publisher()
        process = MagicMock(returncode=None, pid=424242)
        process.poll.return_value = None
        process.__enter__.return_value = process
        attempts = [subprocess.TimeoutExpired(['fixture'], 1), (b'', b'')]

        def communicate(*args, **kwargs):
            value = attempts.pop(0)
            if isinstance(value, Exception):
                raise value
            return value

        process.communicate = communicate
        with patch.object(subprocess, 'Popen', return_value=process):
            with patch.object(os, 'killpg') as terminate:
                with self.assertRaises(subprocess.TimeoutExpired):
                    self.module.Commands().call(['fixture-only'], timeout=1)
                terminate.assert_called_once_with(424242, signal.SIGTERM)

    def test_interrupted_multi_file_publish_resumes_exact_transaction_not_a_fake_rollback(self):
        publisher = self.publisher()
        real_write = self.module.atomic_write
        armed = [True]

        def interrupted(path, raw):
            if armed[0] and path == self.root / 'runtime-approvals/runtime-license-project_management-prod.json':
                armed[0] = False
                raise self.module.Rejected('simulated filesystem rename failure')
            return real_write(path, raw)

        with patch.object(os, 'geteuid', return_value=0):
            with patch.object(self.module, 'atomic_write', side_effect=interrupted):
                with self.assertRaisesRegex(self.module.Rejected, 'rename failure'):
                    publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
            self.assertNotIn('api-open', self.runner.events)
            proof = publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE', resume=True)
            self.assertEqual(proof['mode'], 'published')
            events = list(self.runner.events)
            # 已完成的恢复不得停止或重新开放 API，也不得再次检查旧镜像。
            again = publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE', resume=True)
            self.assertEqual(again['mode'], 'published')
            self.assertEqual(self.runner.events, events)
        self.assertEqual(self.runner.events.count('backup'), 1)
        self.assertEqual(self.runner.old_business_mutations, [])

    def test_backup_capture_includes_authority_and_root_shadows_and_detects_corruption(self):
        publisher = self.publisher()
        self.assertTrue(hasattr(self.module, 'capture_approval_backup'), 'approval disaster backup wiring missing')
        destination = self.base / 'recovery'
        destination.mkdir(mode=0o700)
        self.module.capture_approval_backup(self.root, destination)
        self.module.verify_approval_backup(destination)
        self.assertEqual((destination / 'license-evidence.json').read_bytes(), (self.root / 'license-evidence.json').read_bytes())
        self.assertEqual((destination / 'runtime-approvals/license-evidence.json').read_bytes(),
                         (self.root / 'runtime-approvals/license-evidence.json').read_bytes())
        put(destination / 'runtime-approvals/license-evidence.json', b'corrupt\n')
        with self.assertRaisesRegex(self.module.Rejected, 'backup'):
            self.module.verify_approval_backup(destination)

    def test_resume_rejects_runtime_configuration_drift(self):
        publisher = self.publisher('agent-mismatch')
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaises(self.module.Rejected):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
            self.runner.failure = None
            put(self.root / '.release.env', b'changed-by-other-maintenance\n')
            with self.assertRaisesRegex(self.module.Rejected, 'configuration drift'):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE', resume=True)
        self.assertNotIn('api-open', self.runner.events)

    def test_interrupt_after_reload_closes_api_and_keeps_transaction(self):
        publisher = self.publisher('interrupt-after-open')
        with patch.object(os, 'geteuid', return_value=0):
            with self.assertRaises(KeyboardInterrupt):
                publisher.publish('PUBLISH_RUNTIME_APPROVALS_MAINTENANCE')
        self.assertEqual(self.runner.events[-1], 'api-stop')
        self.assertTrue((self.root / 'runtime/.control-plane-reload-required').is_file())

    def test_real_reload_hook_refuses_api_start_before_agent_proof(self):
        # 真实重载函数负责 API 顺序，仅子进程边界使用替身。
        helper = SOURCE.parent / 'provisioner-config-refresh.sh'
        transaction = self.root / 'runtime/.runtime-approval-publish'
        transaction.mkdir(mode=0o700)
        put(self.root / 'bin/publish-runtime-approvals.py', SOURCE.read_bytes())
        put(transaction / 'journal.json', b'fixture-for-shell-hook-boundary\n')
        calls = self.base / 'reload-events'
        script = '''set -Eeuo pipefail
source HELPER
deploy_dir=ROOT
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
production_profiles_host_manifest() { :; }
production_profiles_container_manifest() { :; }
prepare_managed_public_proxy_image() { :; }
verify_subsystem_provisioner_config_consistency() { :; }
verify_subsystem_control_plane_profiles() { :; }
subsystem_control_plane_container() { printf 'old-%s\\n' "$1"; }
docker() { [[ "$1" != inspect ]] || printf 'healthy\\n'; }
compose() {
  printf '%s\\n' "$*" >>CALLS
  if [[ "$1" == ps ]]; then printf 'new-%s\\n' "${@: -1}"; fi
}
python3() {
  [[ "$*" == *'--agent-proof new-subsystem-provisioner'* ]] || return 91
  printf 'AGENT_PROOF\\n' >>CALLS
  return 42
}
refresh_subsystem_control_plane_config
'''.replace('HELPER', shlex.quote(str(helper))).replace('ROOT', shlex.quote(str(self.root))).replace('CALLS', shlex.quote(str(calls)))
        result = subprocess.run(['bash', '-c', script], text=True, capture_output=True, timeout=10)
        events = calls.read_text()
        self.assertNotEqual(result.returncode, 0, 'reload reopened API despite failed Agent approval proof')
        self.assertIn('AGENT_PROOF', events, result.stderr)
        self.assertNotIn('120 --no-deps platform-api', events)

    def test_platform_upgrade_exception_cannot_cross_pending_approval_transaction(self):
        helper = SOURCE.parent / 'provisioner-config-refresh.sh'
        put(self.root / 'runtime/.control-plane-reload-required',
            ('RUNTIME_APPROVAL_PUBLISH_SHA256=' + self.digest + '\n').encode())
        code = ('set -Eeuo pipefail\nsource ' + shlex.quote(str(helper)) + '\n'
                'deploy_dir=' + shlex.quote(str(self.root)) + '\n'
                'DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE=true\n'
                'require_control_plane_reload_clearance platform-upgrade\n')
        result = subprocess.run(['bash', '-c', code], text=True, capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0, 'platform upgrade exception bypassed incomplete approval transaction')

    def test_real_disaster_backup_captures_and_verifies_runtime_approvals(self):
        for name in ['subsystems.d', 'subsystem-templates', 'mysql-init', 'fixture-keys']:
            (self.root / name).mkdir(mode=0o700)
        for name in ['jwt-ed25519-private.pem', 'jwt-ed25519-public.pem']:
            put(self.root / 'fixture-keys' / name, b'not-a-key-test-fixture\n')
        put(self.root / '.env', b'COMPOSE_PROJECT_NAME=uip\nPLATFORM_KEYS_DIR=./fixture-keys\n')
        backup_source = (SOURCE.parent / 'backup-all.sh').read_text()
        if sys.platform == 'darwin':
            # 宿主机使用 Bash 3，生产要求 Bash 4 动态文件描述符。仅在临时夹具中适配此语法，不修改源码。
            backup_source = backup_source.replace('exec {backup_lock_fd}>', 'backup_lock_fd=11\nexec 11>')
        (self.root / 'bin/backup-all.sh').write_text(backup_source)
        # 新资产或发布器安装前也会使用备份脚本，因此脚本必须自行采集批准文件，不能静默跳过或依赖尚未安装的维护入口。
        (self.root / 'bin/portal-report-backup.sh').write_text('backup_portal_reports() { :; }\n')
        (self.root / 'bin/restore-file-gateway.sh').write_text('#!/bin/bash\nexit 0\n')
        (self.root / 'bin/restore-file-gateway.sh').chmod(0o700)
        gateway = self.root / 'bin/backup-file-gateway.sh'
        gateway.write_text('''#!/bin/bash
set -eu
[[ "$1" == --stamp ]] || exit 80
mkdir -p "$FILE_GATEWAY_BACKUP_ROOT/$2"
printf 'fixture SQL\\n' | gzip -c >"$FILE_GATEWAY_BACKUP_ROOT/$2/database.sql.gz"
''')
        gateway.chmod(0o700)
        tools = self.base / 'tools'
        tools.mkdir(mode=0o700)
        if sys.platform == 'darwin':
            (tools / 'realpath').write_text('#!/bin/bash\n[[ "${1:-}" != -e ]] || shift\nexec /bin/realpath "$@"\n')
            (tools / 'realpath').chmod(0o700)
        (tools / 'flock').write_text('#!/bin/bash\nexit 0\n')
        (tools / 'flock').chmod(0o700)
        (tools / 'docker').write_text('''#!/bin/bash
set -eu
if [[ "$1" == compose ]]; then
  case "$*" in
    *'config --services') printf 'platform-mysql\\n' ;;
    *'ps -a -q platform-mysql') printf 'fixture-db\\n' ;;
    *'SELECT @@log_bin'*) printf '1\\n' ;;
    *'exec -T platform-mysql '*)
      printf '%s\\n' "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000001', SOURCE_LOG_POS=4;"
      printf 'fixture SQL\\n'
      ;;
    *) exit 81 ;;
  esac
elif [[ "$1" == inspect ]]; then printf 'true\\n'
elif [[ "$1" == volume ]]; then exit 0
elif [[ "$1" == version ]]; then printf 'fixture-only\\n'
else exit 82; fi
''')
        (tools / 'docker').chmod(0o700)
        env = dict(os.environ, PATH=str(tools) + ':' + os.environ['PATH'], PYTHONDONTWRITEBYTECODE='1',
                   COPYFILE_DISABLE='1', TMPDIR=str(self.base))
        result = subprocess.run(['bash', str(self.root / 'bin/backup-all.sh')], env=env,
                                text=True, capture_output=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        batches = list((self.root / 'backups/system').glob('*/MANIFEST'))
        self.assertEqual(len(batches), 1)
        batch = batches[0].parent
        with tarfile.open(batch / 'recovery-state.tar.gz') as archive:
            names = archive.getnames()
            self.assertIn('recovery-state/deploy/runtime-approvals/license-evidence.json', names,
                          'real backup omitted authoritative approvals')
            self.assertIn('recovery-state/deploy/license-evidence.json', names,
                          'real backup omitted legacy approval shadow')
        self.assertIn('RUNTIME_APPROVALS_CAPTURED=true', (batch / 'MANIFEST').read_text())


class FakeCommands:
    """仅替换子进程与 Docker 边界，事务及文件系统代码保持真实执行。"""
    def __init__(self, case, failure):
        self.case, self.failure = case, failure
        self.events, self.old_business_mutations = [], []

    def call(self, args, pass_fds=(), timeout=900):
        args = [str(x) for x in args]
        root = self.case.root
        if args[0] == 'bash' and args[1].endswith('backup-all.sh'):
            if '--verify-only' in args:
                self.events.append('backup-verified')
                return b'backup fixture verified\n'
            self.events.append('backup')
            if self.failure == 'backup':
                raise self.case.module.Rejected('backup dependency failed')
            batch = root / 'backups/system/fixture-batch'
            batch.mkdir(parents=True)
            return ('verified disaster recovery backup: ' + str(batch) + '\n').encode()
        if args[0] == 'bash' and args[1].endswith('lifecycle.sh'):
            self.events.append('agent-reload')
            if self.failure == 'agent-mismatch':
                raise self.case.module.Rejected('agent approval hash mismatch')
            # 替身开放 API 之前，先验证实际证明函数。
            self.case.module.verify_agent(root, 'fixture-agent', self)
            self.events.append('api-open')
            if self.failure == 'interrupt-after-open':
                raise KeyboardInterrupt()
            return b'reloaded\n'
        if args[:3] == ['docker', 'image', 'inspect']:
            image = args[-1]
            meta = dict(Id=image, Os='linux', Architecture='amd64', Config={'Labels': {
                'org.opencontainers.image.version': 'r4', 'com.basic-platform.license.protocol': '1'}})
            return json.dumps([meta]).encode()
        if args[:2] == ['docker', 'ps']:
            # 仅用 ps -q 会遗漏已停止或一次性服务，回归测试必须识别该问题。
            count = 33 + (1 if self.failure in {'stopped-portal', 'oneoff-settlement'} and '-aq' in args else 0)
            return ''.join(f'{i:012x}\n' for i in range(1, count + 1)).encode()
        if args[:2] == ['docker', 'inspect']:
            if '--format' in args or '-f' in args:
                if '{{json .Mounts}}' in args:
                    return json.dumps([dict(Type='bind', Source=str(root / 'runtime-approvals'),
                        Destination=str(root / 'runtime-approvals'), RW=self.failure == 'mount-rw')]).encode()
                return b'healthy\n'
            docs = json.loads((self.case.bundle / 'upgrade-before.json').read_text())
            containers = [dict(Id=('a' * 64), Image=x['image_digest'], State={'Running': True},
                Config={'Labels': {'com.docker.compose.project': 'uip', 'com.docker.compose.service': x['name'],
                    'com.docker.compose.oneoff': 'False'}}) for x in docs['services'] + docs['infrastructure']]
            containers.extend(dict(Image=OLD_IMAGE, State={'Running': True}, Config={'Labels': {
                'com.docker.compose.project': 'uip', 'com.docker.compose.service': name,
                'com.docker.compose.oneoff': 'False'}}) for name in EXCLUDED)
            if self.failure in {'stopped-portal', 'oneoff-settlement'} and len(args) > 35:
                containers.append(dict(Image=OLD_IMAGE, State={'Running': False}, Config={'Labels': {
                    'com.docker.compose.project': 'uip', 'com.docker.compose.service': 'portal-api' if self.failure == 'stopped-portal' else 'settlement-worker',
                    'com.docker.compose.oneoff': 'True' if self.failure == 'oneoff-settlement' else 'False'}}))
            for i, container in enumerate(containers, 1):
                container['Id'] = f'{i:012x}' + '0' * 52
            return json.dumps(containers).encode()
        if args[:2] == ['docker', 'exec']:
            self.events.append('agent-proof')
            snapshot = self.case.module.current_snapshot(root)
            files = sorted(name for name in snapshot if name.startswith('runtime-approvals/'))
            return ''.join(snapshot[name]['sha256'] + '  ' + str(root / name) + '\n' for name in files).encode()
        if args[:2] == ['docker', 'compose']:
            if 'stop' in args:
                service = args[-1]
                self.events.append('api-stop' if service == 'platform-api' else 'agent-stop')
                if self.failure == 'api-stop-fails' and service == 'platform-api':
                    raise self.case.module.Rejected('API stop failed')
                if service not in {'platform-api', 'subsystem-provisioner'}:
                    self.old_business_mutations.append(service)
                return b''
            if 'verify-deployment' in args:
                self.events.append('idle-check')
                if self.failure == 'not-ready':
                    raise self.case.module.Rejected('old deployment not READY')
                app = args[args.index('--application') + 1]
                return json.dumps(dict(application=app, environment='prod', status='READY', manifest_checksum='old-profile')).encode()
        raise AssertionError('unexpected external command: ' + repr(args))


if __name__ == '__main__':
    unittest.main()
