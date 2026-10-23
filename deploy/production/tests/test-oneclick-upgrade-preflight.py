#!/usr/bin/env python3
"""使用临时介质和模拟 Docker 命令行验证实际升级及预检代码。"""
import hashlib
import importlib.util
import os
from pathlib import Path
import platform
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[4]
WRAPPER = ROOT / '服务器一键离线部署.sh'
SOURCE = WRAPPER.read_text()
INSTALLER = ROOT / 'platform/deploy/production/bin/install-assets.sh'
# 复用独立的真实 OCI 和归档夹具构造过程。检查器保持真实，只有宿主机 Docker 和发布操作使用严格测试替身。
FIXTURE_SPEC = importlib.util.spec_from_file_location('delivery_upgrade_fixtures', Path(__file__).with_name('test-delivery-inspect.py'))
FIXTURE = importlib.util.module_from_spec(FIXTURE_SPEC)
FIXTURE_SPEC.loader.exec_module(FIXTURE)
FIXTURE.VERSION = 'test'
COMMON = 'die() { echo "$*" >&2; exit 7; }; info() { :; };\n'
COMPOSE = 'services:\n  # BEGIN SUBSYSTEM: common\n  platform: {}\n  # END SUBSYSTEM: common\n'
DEPLOY = '''#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\\n' "$*" >> "$UPGRADE_EVENTS"
case "$1" in
  import)
    name="$(basename "$2")"
    component="${name%%-backend-*}"
    [[ "$name" != common-infrastructure-* ]] || component=common
    [[ "$name" != frontend-* ]] || component=frontend
    touch "runtime/.fixture-import-$component"
    if [[ "$(basename "$2")" == platform-backend-* ]]; then
      printf 'platform-test.imported\\n' > manifests/platform.latest
      printf 'IMAGE_REF=127.0.0.1:18250/uip/platform@sha256:%064d\\n' 0 > manifests/platform-test.imported
    fi ;;
  deploy) touch "runtime/.fixture-published-$2" ;;
  delivery-import-proof) [[ -f "runtime/.fixture-import-$2" ]] ;;
  delivery-proof) [[ -f "runtime/.fixture-published-$2" ]] ;;
  delivery-enabled) printf 'ENABLED\\n' ;;
  delivery-final-proof|delivery-license-proof) exit 1 ;;
  verify|status|upgrade|reload-control-plane) ;;
  *) exit 99 ;;
esac
'''
BACKUP = '#!/usr/bin/env bash\nprintf "backup\\n" >> "$UPGRADE_EVENTS"\n'


def function(name):
    return name + '() {' + SOURCE.split(name + '() {', 1)[1].split('\n}', 1)[0] + '\n}'


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def script(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o750)


class ComposeTests(unittest.TestCase):
    def test_rejects_invalid_pairs_and_common_state(self):
        variants = [
            COMPOSE.replace('END SUBSYSTEM: common', 'END SUBSYSTEM: other'),
            COMPOSE.replace('  # END SUBSYSTEM: common\n', ''),
            COMPOSE + '  # END SUBSYSTEM: common\n',
            COMPOSE + COMPOSE,
            COMPOSE.replace('  platform: {}', '  # platform: {}'),
            COMPOSE.replace('  platform: {}', '  # BEGIN SUBSYSTEM: nested\n  platform: {}\n  # END SUBSYSTEM: nested'),
            'services: {}\n',
        ]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            compose = path / 'compose'
            code = COMMON + function('detect_subsystem_block_states') + f'\ndetect_subsystem_block_states {shlex.quote(str(compose))} {shlex.quote(str(path / "states"))}'
            for text in variants + [COMPOSE]:
                compose.write_text(text)
                result = subprocess.run(['bash', '-c', 'set -Eeuo pipefail\n' + code], capture_output=True)
                self.assertEqual(result.returncode, 0 if text == COMPOSE else 7, text)

    def test_new_optional_blocks_default_disabled(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            optional = '  # BEGIN SUBSYSTEM: project\n  project: {}\n  # END SUBSYSTEM: project\n'
            (path / 'dist').write_text(COMPOSE + optional)
            (path / 'states').write_text('common=enabled\n')
            code = COMMON + function('detect_subsystem_block_states') + '\n' + function('synthesize_compose_with_trim')
            code += f'\nsynthesize_compose_with_trim {path}/dist {path}/states {path}/out'
            result = subprocess.run(['bash', '-c', 'set -Eeuo pipefail\n' + code], capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('  # project: {}\n', (path / 'out').read_text())
            self.assertIn('  platform: {}\n', (path / 'out').read_text())
            (path / 'states').write_text('common=enabled\nproject=enabled\n')
            result = subprocess.run(['bash', '-c', code], capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('  project: {}\n', (path / 'out').read_text())


@unittest.skipUnless(platform.system() == 'Linux' and os.geteuid() == 0, 'Linux root fixture required')
class UpgradeFlowTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.media = self.path / 'media'
        self.target = self.path / 'target'
        self.assets = self.path / 'assets'
        self.tools = self.path / 'tools'
        self.events = self.path / 'events'
        for path in [self.media, self.target, self.assets, self.tools, self.path / 'docker-root']:
            path.mkdir()
        for name in ['runtime', 'packages', 'manifests', 'backups']:
            (self.target / name).mkdir()
        (self.target / '.env').write_text('APP_PUBLIC_BASE_URL=http://192.0.2.10\n')
        (self.target / '.release.env').write_text('PLATFORM_IMAGE=old\n')
        (self.target / 'docker-compose.yml').write_text(COMPOSE)
        for path in [self.target, self.assets]:
            script(path / 'bin/deploy.sh', DEPLOY)
            script(path / 'bin/backup-all.sh', BACKUP)
        for tool in ('deployment-workflow.sh','deployment-session.py','delivery-runtime.sh','delivery-runtime.py',
                     'delivery-inspect.py','delivery-inspect-images.py','delivery-inspect-v2.py','delivery-files.py','subsystem-runtime.py'):
            shutil.copy2(INSTALLER.parent/tool,self.assets/'bin'/tool)
        profiles=self.assets/'subsystems.d'
        profiles.mkdir()
        for source in (INSTALLER.parent.parent/'subsystems.d').glob('*.yaml'):
            shutil.copy2(source,profiles/source.name)
        (self.assets / 'docker-compose.yml').write_text(COMPOSE +
            '  # BEGIN SUBSYSTEM: project\n  project: {}\n  # END SUBSYSTEM: project\n')
        (self.assets / 'CONTAINER_TIMEZONE.md').write_text('approved deployment document\n')
        shutil.copy2(INSTALLER, self.media / 'install-assets.sh')
        self.packages = {}
        for component, name in [('common', 'common-infrastructure-linux-amd64.tar.gz'),
                                ('platform', 'platform-backend-test-linux-amd64.tar.gz'),
                                ('frontend', 'frontend-test-linux-amd64.tar.gz'),
                                ('contract', 'contract-backend-test-linux-amd64.tar.gz'),
                                ('customer-opportunity', 'customer-opportunity-backend-test-linux-amd64.tar.gz'),
                                ('customer-portal', 'customer-portal-backend-test-linux-amd64.tar.gz')]:
            archive = self.media / name
            archive.write_bytes(FIXTURE.image_package(component, image_version='test')[0])
            self.sidecar(archive)
            self.packages[component] = name
        old_common = self.target / 'packages' / self.packages['common']
        old_common.write_bytes(b'old common transport')
        self.sidecar(old_common)
        self.old_common_digest = digest(old_common)
        self.old_common_sidecar = Path(str(old_common) + '.sha256').read_bytes()
        self.old_contract = self.target / 'packages/contract-backend-old-linux-amd64.tar.gz'
        self.old_contract.write_bytes(b'old contract transport')
        self.sidecar(self.old_contract)
        script(self.tools / 'docker', '''#!/usr/bin/env bash
set -Eeuo pipefail
case "$1 ${2:-}" in
  'compose version') echo v2.30.0 ;;
  'compose --project-directory')
    while (($#)); do
      if [[ "$1" == --file ]]; then file="$2"; break; fi
      shift
    done
    awk '/^[ ]+[a-z-]+: [{][}]/ {sub(/:$/, "", $1); print $1}' "$file" ;;
  'info --format')
    if [[ "${3:-}" == '{{range .DriverStatus}}{{if eq (index . 0) "driver-type"}}{{index . 1}}{{end}}{{end}}' ]]; then printf ''; else echo "$UPGRADE_DOCKER_ROOT"; fi ;;
  'info '| 'ps -q') exit 0 ;;
  'version --format') echo 29.8.1 ;;
  *) echo "unexpected docker $*" >&2; exit 88 ;;
esac
''')
        self.env = dict(os.environ, PATH=str(self.tools) + ':' + os.environ['PATH'],
                        UPGRADE_EVENTS=str(self.events), UPGRADE_DOCKER_ROOT=str(self.path / 'docker-root'),
                        TMPDIR=str(self.path))
        self.refresh_media()

    def sidecar(self, path):
        Path(str(path) + '.sha256').write_text(f'{digest(path)}  {path.name}\n')

    def refresh_media(self):
        archive = self.media / 'deployment-assets-test.tar.gz'
        with tarfile.open(archive, 'w:gz') as out:
            for child in self.assets.iterdir():
                out.add(child, arcname=child.name)
        self.sidecar(archive)
        values = ['FORMAT=1', 'PLATFORM_ASSETS_INPUT_SHA256=' + 'a' * 64]
        for component in ['common', 'platform', 'frontend', 'assets']:
            name = archive.name if component == 'assets' else self.packages[component]
            values += [f'{component.upper()}_PACKAGE={name}', f'{component.upper()}_PACKAGE_SHA256={digest(self.media / name)}']
        (self.media / 'PLATFORM_ASSETS_COMPATIBILITY.env').write_text('\n'.join(values) + '\n')
        files = sorted(p for p in self.media.iterdir() if p.name != 'SHA256SUMS')
        (self.media / 'SHA256SUMS').write_text(''.join(f'{digest(p)}  {p.name}\n' for p in files))

    def run_upgrade(self, *extra, media=None):
        return subprocess.run(['bash', str(WRAPPER), '--media', str(media or self.media), '--target', str(self.target),
                               '--upgrade', '--skip-host-deps', '--yes', *extra],
                              env=self.env, text=True, capture_output=True, timeout=30)

    def operations(self):
        return self.events.read_text().splitlines() if self.events.exists() else []

    def outer_archive(self):
        archive = self.path / 'uip-offline-delivery-test-linux-amd64.tar'
        with tarfile.open(archive, 'w') as out:
            out.add(self.media, arcname=self.media.name)
        self.sidecar(archive)
        return archive

    def test_check_only_outer_archive_then_apply_reuses_extraction(self):
        archive = self.outer_archive()
        shutil.rmtree(self.media)
        before = {p.relative_to(self.target): digest(p) for p in self.target.rglob('*') if p.is_file()}
        checked = self.run_upgrade('--check-only', media=archive)
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
        self.assertTrue((self.media / 'SHA256SUMS').is_file())
        self.assertEqual(self.operations(), [])
        self.assertEqual(before, {p.relative_to(self.target): digest(p) for p in self.target.rglob('*') if p.is_file()})
        applied = self.run_upgrade('--apply-compose', media=archive)
        self.assertEqual(applied.returncode, 0, applied.stdout + applied.stderr)
        self.assertIn('复用', applied.stderr)
        self.assertEqual(self.operations()[0], 'backup')
        self.assertIn('deploy platform', self.operations())
        self.assertIn('deploy frontend', self.operations())
        self.assertFalse(any('contract' in op for op in self.operations()))
        self.assertEqual(list(self.path.glob('.uip-delivery-extract.*')), [])

    def test_reused_outer_archive_tampered_payload_stops_before_backup(self):
        archive = self.outer_archive()
        (self.media / self.packages['common']).write_bytes(b'tampered payload')
        failed = self.run_upgrade('--apply-compose', media=archive)
        self.assertNotEqual(failed.returncode, 0, failed.stdout + failed.stderr)
        self.assertEqual(self.operations(), [])
        self.assertEqual(self.old_contract.read_bytes(), b'old contract transport')
        self.assertFalse((self.target / 'docker-compose.yml.dist').exists())

    def test_missing_requested_package_stops_before_backup(self):
        for extra in [(), ('--check-only',)]:
            result = self.run_upgrade('--upgrade-subsystems', 'project', *extra)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('缺少对应镜像包', result.stderr)
            self.assertEqual(self.operations(), [])

    def test_old_installer_document_mismatch_stops_before_backup(self):
        # 精确复现旧白名单契约；两个归档的哈希仍有效。
        path = self.media / 'install-assets.sh'
        path.write_text(path.read_text().replace('|CONTAINER_TIMEZONE\\.md', ''))
        self.refresh_media()
        for extra in [(), ('--check-only',)]:
            result = self.run_upgrade(*extra)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('资产包含非部署文件或不安全路径', result.stderr)
            self.assertEqual(self.operations(), [])
            self.assertFalse((self.target / 'docker-compose.yml.dist').exists())
            self.assertEqual(list(self.path.glob('uip-assets-preflight.*')), [])

    def test_invalid_old_compose_stops_before_backup(self):
        (self.target / 'docker-compose.yml').write_text(COMPOSE.replace('END SUBSYSTEM: common', 'END SUBSYSTEM: wrong'))
        result = self.run_upgrade()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('编排模块标记无效', result.stderr)
        self.assertEqual(self.operations(), [])

    def test_pending_asset_transaction_stops_before_backup(self):
        (self.target / 'runtime/.assets-install-transaction').mkdir()
        result = self.run_upgrade('--check-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('--recover', result.stderr)
        self.assertEqual(self.operations(), [])

    def test_successful_check_only_leaves_existing_target_unchanged(self):
        before = {p.relative_to(self.target): digest(p) for p in self.target.rglob('*') if p.is_file()}
        result = self.run_upgrade('--check-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        after = {p.relative_to(self.target): digest(p) for p in self.target.rglob('*') if p.is_file()}
        self.assertEqual(before, after)
        self.assertEqual(self.operations(), [])
        self.assertEqual(list(self.path.glob('uip-assets-preflight.*')), [])

    def test_legacy_assets_missing_workflow_tools_stop_before_backup_or_session(self):
        (self.assets / 'bin/deployment-session.py').unlink()
        self.refresh_media()
        result = self.run_upgrade('--apply-compose')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('分阶段安装工具', result.stderr)
        self.assertEqual(self.operations(), [])
        self.assertFalse(Path(str(self.target)+'.installation-state').exists())

    def test_base_upgrade_preserves_unrequested_business_and_archives_same_name(self):
        result = self.run_upgrade('--apply-compose')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        operations = self.operations()
        self.assertEqual(operations[0], 'backup')
        self.assertTrue(operations[1].endswith(self.packages['common']))
        self.assertTrue(operations[2].endswith(self.packages['platform']))
        self.assertIn('deploy platform', operations)
        self.assertIn('deploy frontend', operations)
        self.assertFalse(any('contract' in op for op in operations))
        self.assertEqual(self.old_contract.read_bytes(), b'old contract transport')
        self.assertFalse((self.target / 'packages' / self.packages['contract']).exists())
        archives = list((self.target / 'packages').glob('superseded-*'))
        self.assertEqual(len(archives), 1)
        saved = archives[0] / self.packages['common']
        self.assertEqual(digest(saved), self.old_common_digest)
        self.assertEqual(Path(str(saved) + '.sha256').read_bytes(), self.old_common_sidecar)
        self.assertEqual(digest(self.target / 'packages' / self.packages['common']), digest(self.media / self.packages['common']))
        self.assertIn('  # project: {}\n', (self.target / 'docker-compose.yml').read_text())
        # 固定名称的公共内容字节完全相同时，重试必须直接复用，不能产生另一个归档。
        result = self.run_upgrade('--apply-compose', '--resume')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(list((self.target / 'packages').glob('superseded-*'))), 1)

    def test_explicit_business_upgrade_archives_and_stages_only_selected(self):
        block = '  # BEGIN SUBSYSTEM: contract\n  contract-api: {}\n  # END SUBSYSTEM: contract\n'
        (self.target / 'docker-compose.yml').write_text(COMPOSE + block)
        with (self.assets / 'docker-compose.yml').open('a') as out:
            out.write(block)
        self.refresh_media()
        result = self.run_upgrade('--apply-compose', '--upgrade-subsystems', 'contract')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('upgrade contract', self.operations())
        self.assertFalse(self.old_contract.exists())
        saved = list((self.target / 'packages').glob('superseded-*/contract-backend-old-linux-amd64.tar.gz'))
        self.assertEqual(len(saved), 1)
        self.assertEqual(saved[0].read_bytes(), b'old contract transport')
        self.assertTrue((self.target / 'packages' / self.packages['contract']).exists())

    def test_disabled_or_absent_requested_block_stops_before_backup(self):
        for block in ['', '  # BEGIN SUBSYSTEM: contract\n  # contract-api: {}\n  # END SUBSYSTEM: contract\n']:
            (self.target / 'docker-compose.yml').write_text(COMPOSE + block)
            result = self.run_upgrade('--upgrade-subsystems', 'contract', '--check-only')
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('未启用或缺少模块', result.stderr)
            self.assertEqual(self.operations(), [])

    def test_missing_requested_api_stops_before_backup(self):
        block = '  # BEGIN SUBSYSTEM: contract\n  contract-migrate: {}\n  # END SUBSYSTEM: contract\n'
        (self.target / 'docker-compose.yml').write_text(COMPOSE + block)
        result = self.run_upgrade('--upgrade-subsystems', 'contract', '--check-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('contract-api', result.stderr)
        self.assertEqual(self.operations(), [])

    def test_invalid_new_template_stops_before_backup(self):
        (self.assets / 'docker-compose.yml').write_text(COMPOSE.replace('END SUBSYSTEM: common', 'END SUBSYSTEM: wrong'))
        self.refresh_media()
        result = self.run_upgrade('--check-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('编排模块标记无效', result.stderr)
        self.assertEqual(self.operations(), [])

    def test_requested_block_missing_from_new_template_stops_before_backup(self):
        block = '  # BEGIN SUBSYSTEM: contract\n  contract-api: {}\n  # END SUBSYSTEM: contract\n'
        (self.target / 'docker-compose.yml').write_text(COMPOSE + block)
        result = self.run_upgrade('--upgrade-subsystems', 'contract', '--check-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('交付编排模板', result.stderr)
        self.assertEqual(self.operations(), [])

    def test_portal_without_existing_crm_stops_before_backup(self):
        block = '  # BEGIN SUBSYSTEM: portal\n  portal-api: {}\n  # END SUBSYSTEM: portal\n'
        (self.target / 'docker-compose.yml').write_text(COMPOSE + block)
        result = self.run_upgrade('--upgrade-subsystems', 'customer-portal', '--check-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('CRM 未启用', result.stderr)
        self.assertEqual(self.operations(), [])

    def test_portal_only_upgrade_preserves_crm_package(self):
        blocks = ('  # BEGIN SUBSYSTEM: customer\n  customer-api: {}\n  # END SUBSYSTEM: customer\n'
                  '  # BEGIN SUBSYSTEM: portal\n  portal-api: {}\n  # END SUBSYSTEM: portal\n')
        (self.target / 'docker-compose.yml').write_text(COMPOSE + blocks)
        with (self.assets / 'docker-compose.yml').open('a') as out:
            out.write(blocks)
        old_crm = self.target / 'packages/customer-opportunity-backend-old-linux-amd64.tar.gz'
        old_crm.write_bytes(b'old CRM transport')
        self.sidecar(old_crm)
        self.refresh_media()
        result = self.run_upgrade('--apply-compose', '--upgrade-subsystems', 'customer-portal')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('upgrade customer-portal', self.operations())
        self.assertFalse(any('customer-opportunity' in op for op in self.operations()))
        self.assertEqual(old_crm.read_bytes(), b'old CRM transport')
        self.assertFalse((self.target / 'packages' / self.packages['customer-opportunity']).exists())


if __name__ == '__main__':
    unittest.main()
