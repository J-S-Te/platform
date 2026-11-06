#!/usr/bin/env python3
"""执行真实离线构建脚本，使用无网络、无 Docker 套接字的严格夹具。"""
import importlib.util
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("platform_fixture", HERE / "test-offline-platform-selection.py")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)

EXTRACT_MANIFEST = r'''
if a[:1] == ['create']:
    print('fixture-stopped-frontend'); sys.exit(0)
if a[:1] == ['cp']:
    if a[1] != 'fixture-stopped-frontend:/usr/share/nginx/html/frontend-modules.json':
        fail('unexpected image file copy')
    codes = ['basic-platform']
    order = ['project_management', 'settlement', 'contract_management',
             'customer_and_opportunity', 'customer_portal', 'data_analysis']
    selection = state.get('frontend_selection', 'all')
    codes += order if selection == 'all' else [code for code in order if code in selection.split(',')]
    if os.environ.get('UIP_SELECTION_WRONG_MANIFEST') == '1': codes += ['data_analysis']
    pathlib.Path(a[2]).write_text(json.dumps({'schema_version': 1,
        'enabled_modules': codes, 'compiled_modules': codes}) + '\n')
    sys.exit(0)
if a[:1] == ['rm']:
    if a[1:] != ['fixture-stopped-frontend']: fail('unexpected container deletion')
    sys.exit(0)
'''
DOCKER = FIXTURE.DOCKER.replace("if a[:2] == ['buildx', 'build']:", EXTRACT_MANIFEST + "\nif a[:2] == ['buildx', 'build']:")
DOCKER = DOCKER.replace("    state['images'][ref] = {'architecture': os.environ.get", "    state['frontend_selection'] = next((arg.split('=', 1)[1] for arg in args if arg.startswith('VITE_ENABLED_MODULES=')), 'all')\n    state['images'][ref] = {'architecture': os.environ.get")


class OfflineFrontendSelection(unittest.TestCase):
    # 用途：建立真实脚本与最小隔离源码，不连接现有 Docker。
    # 参数：self 为当前测试。
    # 返回值：无。
    def setUp(self):
        FIXTURE.OfflinePlatformSelection.setUp(self)
        helper = FIXTURE.PRODUCTION / 'bin/offline-frontend-selection.sh'
        if helper.exists():
            shutil.copyfile(helper, self.production / 'bin' / helper.name)
        shutil.copyfile(FIXTURE.PRODUCTION.parents[2] / 'frontend/Dockerfile',
                        self.workspace / 'frontend/Dockerfile')
        (self.workspace / 'frontend/build').mkdir()
        (self.workspace / 'frontend/build/selection.mjs').write_text('export const choice = 1\n')
        docker = self.tools / 'docker'
        docker.write_text('#!' + sys.executable + '\n' + DOCKER)
        self.env['UIP_SELECTION_DEFAULT_ARCH'] = 'amd64'

    # 用途：执行真实前端离线构建并检查预期退出状态。
    # 参数：selection 为 CLI 选配文本，output 为隔离输出，success 为预期结果。
    # 返回值：实际子进程结果。
    def run_builder(self, selection=None, output=None, success=True):
        command = ['bash', str(self.production / 'bin/build-offline-packages.sh'),
                   '--component', 'frontend', '--version', 'frontend-selection',
                   '--output', str(output or self.output)]
        if selection is not None:
            command += ['--selected-components', selection]
        result = subprocess.run(command, env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    # 用途：验证实际 docker build 收到规范化组合及平台 only 选择。
    # 参数：self 为当前测试。
    # 返回值：无，断言失败时报告实际调用。
    def test_build_arguments_follow_selection_and_default(self):
        for index, (selection, expected) in enumerate([
                (None, 'all'), ('', 'none'), ('contract_management', 'contract_management'),
                ('project contract', 'project_management,contract_management'),
                ('customer-portal', 'customer_and_opportunity,customer_portal')]):
            with self.subTest(selection=selection):
                self.run_builder(selection, self.root / f'output-{index}')
                calls = [json.loads(line) for line in self.log.read_text().splitlines()]
                build = [call for call in calls if call[:2] == ['buildx', 'build']][-1]
                self.assertIn('VITE_ENABLED_MODULES=' + expected, build)

    # 用途：禁止同一源码输出目录以另一组合复用旧前端。
    # 参数：self 为当前测试。
    # 返回值：无，复用绕过时断言失败。
    def test_changed_selection_refuses_existing_output_before_build(self):
        self.run_builder('contract')
        before = self.log.read_text()
        self.run_builder('project', success=False)
        new_calls = [json.loads(line) for line in self.log.read_text()[len(before):].splitlines()]
        self.assertFalse(any(call[:2] == ['buildx', 'build'] for call in new_calls))

    # 用途：核对交付 manifest 来自镜像，并与输入及校验清单绑定。
    # 参数：self 为当前测试。
    # 返回值：无，清单缺失或错配时断言失败。
    def test_manifest_is_copied_checked_and_checksummed(self):
        self.run_builder('contract')
        self.assertTrue((self.output / 'frontend-modules.json').is_file())
        manifest = json.loads((self.output / 'frontend-modules.json').read_text())
        self.assertEqual(manifest['enabled_modules'], ['basic-platform', 'contract_management'])
        self.assertIn('frontend-modules.json', (self.output / 'SHA256SUMS').read_text())
        self.env['UIP_SELECTION_WRONG_MANIFEST'] = '1'
        self.run_builder('contract', self.root / 'wrong-manifest', success=False)
        self.assertFalse(list((self.root / 'wrong-manifest').glob('frontend-*.tar.gz')))

    # 用途：确认前端构建工具字节参与源码指纹。
    # 参数：self 为当前测试。
    # 返回值：无，遗漏新增构建目录时断言失败。
    def test_build_tool_changes_invalidate_source_baseline(self):
        self.run_builder('contract')
        (self.workspace / 'frontend/build/selection.mjs').write_text('export const choice = 2\n')
        self.run_builder('contract', success=False)


    # 用途：执行根入口的实际复用指纹函数，验证组合与参数绑定。
    # 参数：self 为当前测试。
    # 返回值：无，错误复用或编码漂移时断言失败。
    def test_root_reuse_fingerprint_binds_selection_and_registry(self):
        wrapper = FIXTURE.PRODUCTION.parents[2] / '一键制作离线镜像包.sh'
        function = re.search(r'platform_assets_input_fingerprint\(\) \{.*?\n\}',
                             wrapper.read_text(), re.S).group(0)
        fingerprints = []
        for selection, registry in [('contract', 'https://registry.npmjs.org'),
                                    ('contract_management contract', 'https://registry.npmjs.org'),
                                    ('project', 'https://registry.npmjs.org'),
                                    ('contract', 'https://npm.example.invalid')]:
            command = ['bash', '-c',
                       'set -euo pipefail; source "$1/bin/offline-build-inputs.sh"; '
                       'source "$1/bin/offline-frontend-selection.sh"; '
                       'script_dir="$2"; selected_optional_components="$3"; '
                       'OFFLINE_NPM_CONFIG_REGISTRY="$4"; eval "$5"; '
                       'platform_assets_input_fingerprint',
                       'fixture', str(self.production), str(self.workspace), selection, registry, function]
            result = subprocess.run(command, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            fingerprints.append(result.stdout.strip())
        self.assertEqual(fingerprints[0], fingerprints[1])
        self.assertNotEqual(fingerprints[0], fingerprints[2])
        self.assertNotEqual(fingerprints[0], fingerprints[3])


    # 用途：验证复用基线清单保持失败关闭，包括旧包缺失清单。
    # 参数：self 为当前测试。
    # 返回值：无，缺失或错配被放行时断言失败。
    def test_reused_manifest_requires_exact_selected_modules(self):
        manifest = self.root / 'baseline-modules.json'
        manifest.write_text(json.dumps({'schema_version': 1,
            'enabled_modules': ['basic-platform', 'contract_management'],
            'compiled_modules': ['basic-platform', 'contract_management']}))
        for selection, path, success in [('contract_management', manifest, True),
                                         ('project_management', manifest, False),
                                         ('contract_management', self.root / 'missing.json', False)]:
            command = ['bash', '-c', 'set -euo pipefail; source "$1"; '
                       'offline_validate_frontend_manifest "$2" "$3"', 'fixture',
                       str(self.production / 'bin/offline-frontend-selection.sh'), str(path), selection]
            result = subprocess.run(command, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
