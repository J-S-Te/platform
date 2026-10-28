import importlib.util
import pathlib
import subprocess
import tempfile
import unittest

BIN = pathlib.Path(__file__).resolve().parents[1] / 'bin'

class PreflightTests(unittest.TestCase):
    def test_business_unpinned_reports_line(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / 'contract_management').mkdir()
            (root / 'contract_management/Dockerfile').write_text('FROM alpine:3.24\n')
            result = subprocess.run(['python3', str(BIN / 'release-preflight.py'), '--workspace', tmp,
                '--components', 'contract'], capture_output=True, text=True)
            self.assertIn('contract_management/Dockerfile:1', result.stderr)
            self.assertIn('未固定', result.stderr)


class IntegrationTests(unittest.TestCase):
    def setUp(self):
        import shutil, os
        self.temp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temp.name)
        self.addCleanup(self.temp.cleanup)
        self.workspace = self.root / 'workspace'
        self.workspace.mkdir()
        original = BIN.parents[3]
        for name in ['一键制作离线镜像包.sh', '服务器一键离线部署.sh', '系统运维工具.sh',
                     '清理首次部署产物.sh', '操作说明/失败首装清理与重新部署.md']:
            target = self.workspace / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(original / name, target)
        target_bin = self.workspace / 'platform/deploy/production/bin'
        target_bin.mkdir(parents=True)
        for name in ['build-evidence.py', 'release-preflight.py', 'offline-build-inputs.sh']:
            shutil.copyfile(BIN / name, target_bin / name)
        (target_bin / 'build-offline-packages.sh').write_text('exit 93\n')
        (target_bin / 'offline-base.Dockerfile').write_text('ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n')
        self.pinned = 'alpine@sha256:' + 'a' * 64
        for directory in ['platform', 'frontend', 'contract_management']:
            (self.workspace / directory).mkdir(exist_ok=True)
            (self.workspace / directory / 'Dockerfile').write_text('FROM ' + self.pinned + '\n')
        self.tools = self.root / 'tools'
        self.tools.mkdir()
        self.calls = self.root / 'calls'
        docker = self.tools / 'docker'
        docker.write_text('''#!/usr/bin/env python3
import os, sys, json
with open(os.environ['CALLS'], 'a') as f: f.write(' '.join(sys.argv[1:]) + '\\n')
a=sys.argv[1:]
if a == ['context','inspect','desktop-linux'] and os.environ.get('STUB_CONTEXT_FALLBACK') == '1': sys.exit(1)
if a[:2] == ['version','--format']:
 print(os.environ.get('STUB_API',''))
elif a == ['image','inspect','--help']:
 print('--platform')
elif a[:3] == ['buildx','imagetools','inspect']:
 if os.environ.get('STUB_REMOTE') != 'ok': sys.exit(1)
 if os.environ.get('STUB_SINGLE') == '1':
  print(json.dumps({'os':'linux','architecture':os.environ.get('STUB_REMOTE_ARCH','amd64')} if a[-1] == '{{json .Image}}' else {'schemaVersion':2,'digest':'sha256:'+'b'*64})); sys.exit(0)
 print(json.dumps({'schemaVersion':2,'digest':'sha256:'+'b'*64,'manifests':[{'digest':'sha256:'+'c'*64,'platform':{'os':'linux','architecture':os.environ.get('STUB_REMOTE_ARCH','amd64')}}]}))
elif a[:2] == ['image','inspect']:
 if os.environ.get('STUB_COLD') == '1': sys.exit(1)
 if '--platform' in a:
  a=a[:2]+a[4:]
  os.environ['STUB_ARCH']='amd64'
 metadata={'Os':'linux', 'Architecture':os.environ.get('STUB_ARCH','amd64'), 'RepoDigests':[a[2].rsplit(':',1)[0]+'@sha256:'+'a'*64]}
 if os.environ.get('STUB_DESCRIPTOR') == '1':
  metadata.update(Os='',Architecture='',Descriptor={'mediaType':'application/vnd.oci.image.manifest.v1+json','digest':'sha256:'+'d'*64,'platform':{'os':'linux','architecture':'amd64'}})
 print(json.dumps(metadata))
elif a and a[0]=='info': print('/tmp')
elif a[:2]==['context','show']: print('orbstack')
''')
        docker.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.tools)+os.pathsep+os.environ['PATH'],
                        CALLS=str(self.calls), UIP_BUILD_BUILDER_ID='test-builder', UIP_BUILD_INVOCATION_ID='test-invocation',
                        PYTHONDONTWRITEBYTECODE='1')

    def run_wrapper(self, *extra):
        return subprocess.run(['bash', str(self.workspace/'一键制作离线镜像包.sh'), '--version', 'test',
            '--output', str(self.root/'output'), '--components', 'contract', '--skip-host-deps', '--no-pause',
            *extra], env=self.env, capture_output=True, text=True)

    def test_unpinned_business_stops_before_build_save_even_with_pinned_common(self):
        (self.workspace/'contract_management/Dockerfile').write_text('FROM alpine:3.24\n')
        result = self.run_wrapper()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('contract_management/Dockerfile:1', result.stderr)
        calls = self.calls.read_text() if self.calls.exists() else ''
        self.assertNotIn('buildx build', calls)
        self.assertNotIn('save ', calls)
        self.assertFalse((self.root/'output').exists())

    def test_cli_generates_missing_invocation_not_builder_identity(self):
        self.env.pop('UIP_BUILD_INVOCATION_ID')
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_identity_stops_before_build(self):
        self.env.pop('UIP_BUILD_BUILDER_ID')
        result = self.run_wrapper()
        self.assertIn('UIP_BUILD_BUILDER_ID', result.stderr)
        self.assertFalse((self.root/'output').exists())

    def test_candidate_skips_formal_gate(self):
        (self.workspace/'contract_management/Dockerfile').write_text('FROM alpine:3.24\n')
        self.env.pop('UIP_BUILD_BUILDER_ID')
        result = self.run_wrapper('--candidate', '--preflight-only')
        self.assertNotIn('基础镜像未固定', result.stderr)
        self.assertIn('未签候选模式', result.stdout)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_alias_scratch_arg_platform_and_continuation(self):
        source = 'ARG BASE='+self.pinned+'\nFROM --platform=linux/amd64 ${BASE} AS builder\nFROM builder AS next\nFROM scratch AS blank\nFROM blank AS final\n'
        (self.workspace/'contract_management/Dockerfile').write_text(source)
        result = subprocess.run(['python3', '-B', str(BIN/'release-preflight.py'), '--workspace', str(self.workspace),
            '--components', 'contract'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        (self.workspace/'contract_management/Dockerfile').write_text('ARG BASE\nFROM ${BASE}\n')
        result = subprocess.run(['python3', '-B', str(BIN/'release-preflight.py'), '--workspace', str(self.workspace),
            '--components', 'contract'], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Dockerfile:2', result.stderr)

    def test_contract_change_changes_full_not_shared(self):
        for module in ['platform', 'contract_management', 'customer_and_opportunity', 'project_management', 'Settlement', 'data_analysis']:
            for folder in ['cmd', 'internal', 'migrations', 'authz']:
                (self.workspace/module/folder).mkdir(parents=True, exist_ok=True)
        for folder in ['frontend/src', 'frontend/public', 'frontend/nginx',
                       'platform/deploy/production/monitoring', 'platform/deploy/production/mysql-init',
                       'platform/deploy/production/nginx', 'platform/deploy/production/subsystem-templates',
                       'platform/deploy/production/subsystems.d']:
            (self.workspace/folder).mkdir(parents=True, exist_ok=True)
        def fingerprint(mode):
            result = subprocess.run(['bash', '-c', 'source "$1"; offline_build_input_fingerprint "$2" "$3"',
                'fingerprint', str(BIN/'offline-build-inputs.sh'), str(self.workspace), mode], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return result.stdout
        before = [fingerprint('full'), fingerprint('shared')]
        (self.workspace/'contract_management/Dockerfile').write_text('FROM changed\n')
        self.assertNotEqual(before[0], fingerprint('full'))
        self.assertEqual(before[1], fingerprint('shared'))

    def test_dynamic_evidence_only_binds_controlled_base_image(self):
        spec = importlib.util.spec_from_file_location('evidence', BIN/'build-evidence.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        refs = module.dockerfile_base_refs('ARG BASE_IMAGE\nARG EXTRA=alpine:latest\nFROM ${BASE_IMAGE} AS base\nFROM ${EXTRA}\n', [self.pinned])
        self.assertEqual(refs, [(3, self.pinned), (4, 'alpine:latest')])

    def test_cold_cache_registry_resolvable_needs_no_manual_pull(self):
        self.env.update(STUB_COLD='1', STUB_REMOTE='ok')
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = self.calls.read_text()
        self.assertIn('buildx imagetools inspect', calls)
        self.assertNotIn('pull ', calls)
        self.assertNotIn('buildx build', calls)
        self.assertNotIn('save ', calls)
        self.assertFalse((self.root/'output').exists())

    def test_cold_cache_unresolvable_fails_before_build(self):
        self.env['STUB_COLD'] = '1'
        result = self.run_wrapper('--preflight-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('仓库', result.stderr)
        self.assertNotIn('buildx build', self.calls.read_text())

    def test_orbstack_descriptor_platform_does_not_need_registry(self):
        self.env['STUB_DESCRIPTOR'] = '1'
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn('imagetools', self.calls.read_text())

    def test_single_manifest_checks_config_at_pinned_digest(self):
        self.env.update(STUB_COLD='1', STUB_REMOTE='ok', STUB_SINGLE='1')
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('mysql:8.4@sha256:'+'b'*64+' --format {{json .Image}}', self.calls.read_text())

    def test_single_manifest_wrong_platform_rejected(self):
        self.env.update(STUB_COLD='1', STUB_REMOTE='ok', STUB_SINGLE='1', STUB_REMOTE_ARCH='arm64')
        result = self.run_wrapper('--preflight-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('仓库单平台镜像不是 linux/amd64', result.stderr)

    def test_builder_descriptor_fallback_keeps_selector(self):
        script = (BIN/'build-offline-packages.sh').read_text()
        start = script.index('offline_image_inspect() {')
        end = script.index('offline_save_images() {', start)
        harness = """set -eu
offline_inspect_platform_args=(--platform linux/amd64)
docker() {
  echo "$*" >&2
  case "$*" in
    *Descriptor.Platform.OS*) echo linux/amd64 ;;
    *) echo / ;;
  esac
}
""" + script[start:end] + "offline_image_inspect mysql:8.4 --format '{{.Os}}/{{.Architecture}}'\n"
        result = subprocess.run(['bash', '-c', harness], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'linux/amd64')
        self.assertEqual(result.stderr.count('--platform linux/amd64'), 2)

    def test_registry_wrong_architecture_rejected(self):
        self.env.update(STUB_COLD='1', STUB_REMOTE='ok', STUB_REMOTE_ARCH='arm64')
        result = self.run_wrapper('--preflight-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('仓库索引中没有可核验的 linux/amd64', result.stderr)
        self.assertNotIn('buildx build', self.calls.read_text())

    def test_context_fallback_with_cold_cache(self):
        uname = self.tools/'uname'
        uname.write_text('#!/bin/sh\nif [ "$1" = "-m" ]; then echo arm64; else echo Darwin; fi\n')
        uname.chmod(0o755)
        self.env.update(STUB_COLD='1', STUB_REMOTE='ok', STUB_CONTEXT_FALLBACK='1')
        self.env.pop('UIP_DOCKER_CONTEXT', None)
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('回退到当前 context：orbstack', result.stdout)
        self.assertNotIn('pull ', self.calls.read_text())

    def test_formal_cached_image_without_digest_refreshes_source(self):
        script = (BIN/'build-offline-packages.sh').read_text()
        start = script.index('if want common; then')
        end = script.index('  common_images=()', start)
        block = script[start:end] + '\necho BUILD_STARTED\nfi\n'
        harness = """set -e
want() { return 0; }
offline_common_base_images() { echo image:1; }
prepared=false
ensure_local_platform_image() { echo "PREPARE:$1:${2:-false}"; if [ "${2:-false}" = true ]; then prepared=true; fi; }
offline_image_inspect() { if [ "$prepared" = true ]; then echo image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; fi; }
managed_proxy_transport=proxy:1
formal_build=true
""" + block
        result = subprocess.run(['bash', '-c', harness], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('PREPARE:image:1:true', result.stdout)
        self.assertIn('BUILD_STARTED', result.stdout)

    def test_all_common_prepared_before_first_build(self):
        script = (BIN/'build-offline-packages.sh').read_text()
        start = script.index('if want common; then')
        end = script.index('  common_images=()', start)
        block = script[start:end] + '\necho BUILD_STARTED\nfi\n'
        harness = """set -e
want() { return 0; }
offline_common_base_images() { printf '%s\n' first:1 last:1; }
ensure_local_platform_image() { echo "PREPARE:$1"; [ "$1" != last:1 ]; }
managed_proxy_transport=proxy:1
formal_build=false
""" + block
        result = subprocess.run(['bash', '-c', harness], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('PREPARE:last:1', result.stdout)
        self.assertNotIn('BUILD_STARTED', result.stdout)

    def test_modern_multiarch_selects_amd64(self):
        self.env.update(STUB_API='1.49', STUB_ARCH='arm64')
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('image inspect --platform linux/amd64', self.calls.read_text())
        self.assertFalse((self.root/'output').exists())

    def test_old_api_amd64_preflight_succeeds(self):
        self.env['STUB_API'] = '1.48'
        result = self.run_wrapper('--preflight-only')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn('image inspect --platform', self.calls.read_text())

    def test_arm_only_common_rejected(self):
        self.env['STUB_ARCH'] = 'arm64'
        result = self.run_wrapper('--preflight-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('linux/amd64', result.stderr)
        self.assertFalse((self.root/'output').exists())

    def test_shared_diff_and_missing_old_index(self):
        import hashlib
        spec = importlib.util.spec_from_file_location('preflight', BIN/'release-preflight.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        baseline = self.root/'baseline'
        baseline.mkdir()
        self.assertIn('缺少', module.shared_diff(self.workspace, baseline)[0])
        # Use a controlled enumerator; source-index binding and comparison remain real.
        (self.workspace/'platform/deploy/production/bin/offline-build-inputs.sh').write_text(
            "offline_build_input_paths() { printf '%s\\0' platform/Dockerfile frontend/Dockerfile; }\n")
        index = baseline/'source-input-index.tsv'
        data = (self.workspace/'platform/Dockerfile').read_bytes()
        index.write_text(hashlib.sha256(data).hexdigest()+'\t'+str(len(data))+'\tplatform/Dockerfile\n')
        (baseline/'SHA256SUMS').write_text(hashlib.sha256(index.read_bytes()).hexdigest()+'  source-input-index.tsv\n')
        (self.workspace/'platform/Dockerfile').write_text('FROM changed\n')
        result = module.shared_diff(self.workspace, baseline)
        self.assertIn('修改共享输入：platform/Dockerfile', result)
        self.assertIn('新增共享输入：frontend/Dockerfile', result)

if __name__ == '__main__':
    unittest.main()
