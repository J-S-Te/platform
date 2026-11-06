#!/usr/bin/env python3
"""使用真实构建脚本和隔离Docker替身，验证显式Portal发行选择；不构建真实镜像。"""
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('portal_frontend_fixture', HERE / 'test-offline-frontend-selection.py')
FRONT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FRONT)
SPEC = importlib.util.spec_from_file_location('portal_media_fixture', HERE / 'test-delivery-inspect.py')
MEDIA = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MEDIA)


class PortalBuildModeTests(unittest.TestCase):
    # 用途：建立真实脚本和无网络、无套接字的临时Docker替身。
    # 参数：self为当前测试。
    # 返回值：无。
    def setUp(self):
        FRONT.OfflineFrontendSelection.setUp(self)
        self.env.pop('PORTAL_CUSTOMER_DIRECTORY_MODE', None)

    # 用途：执行实际子构建CLI，不把源码文本匹配冒充参数行为验证。
    # 参数：mode为显式选择，selection为模块集合，output为临时输出，success为预期。
    # 返回值：真实受控脚本结果。
    def build(self, mode=None, selection='customer-portal', output=None, success=True):
        args = ['bash', str(self.production / 'bin/build-offline-packages.sh'), '--component', 'frontend',
                '--version', 'portal-mode', '--output', str(output or self.output), '--selected-components', selection]
        if mode is not None:
            args += ['--portal-customer-directory-mode', mode]
        result = subprocess.run(args, env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    # 用途：证明默认crm保留旧依赖，CLI优先于env且local不强带CRM。
    # 参数：self为当前测试。
    # 返回值：无；前端实际构建参数或证据模式不一致时失败。
    def test_actual_build_default_cli_priority_and_env(self):
        self.build(output=self.root / 'default')
        self.env['PORTAL_CUSTOMER_DIRECTORY_MODE'] = 'invalid-env'
        result = self.build('local', output=self.root / 'local')
        self.assertIn('runtime/portal.env', result.stdout + result.stderr)
        info = (self.root / 'local/BUILD_INFO.txt').read_text()
        self.assertIn('PORTAL_CUSTOMER_DIRECTORY_MODE=local\n', info)
        manifest = json.loads((self.root / 'local/frontend-modules.json').read_text())
        self.assertEqual(manifest['compiled_modules'], ['basic-platform', 'customer_portal'])
        default = json.loads((self.root / 'default/frontend-modules.json').read_text())
        self.assertIn('customer_and_opportunity', default['compiled_modules'])
        self.env['PORTAL_CUSTOMER_DIRECTORY_MODE'] = 'local'
        self.build(output=self.root / 'environment')
        self.assertEqual(json.loads((self.root / 'environment/frontend-modules.json').read_text())['compiled_modules'], manifest['compiled_modules'])

    # 用途：即使前端模块集合相同也必须将模式绑定源码指纹并阻断错误复用。
    # 参数：self为当前测试。
    # 返回值：无；同输出复用不同模式或源码指纹遗漏时失败。
    def test_mode_binds_source_fingerprint_and_existing_output(self):
        selection = 'customer-portal customer-opportunity'
        self.build('crm', selection)
        before = self.log.read_text()
        self.build('local', selection, success=False)
        calls = [json.loads(line) for line in self.log.read_text()[len(before):].splitlines()]
        self.assertFalse(any(call[:2] == ['buildx', 'build'] for call in calls))
        other = self.root / 'other'
        self.build('local', selection, other)
        info = lambda path: dict(line.split('=', 1) for line in path.read_text().splitlines())
        self.assertNotEqual(info(self.output / 'BUILD_INFO.txt')['SOURCE_INPUT_SHA256'], info(other / 'BUILD_INFO.txt')['SOURCE_INPUT_SHA256'])

    # 用途：非法模式在任何Docker动作前拒绝。
    # 参数：self为当前测试。
    # 返回值：无。
    def test_invalid_mode_precedes_docker(self):
        self.build('automatic', success=False)
        self.assertFalse(self.log.exists() and self.log.read_text().strip())

    # 用途：执行根入口实际规范化和复用函数，核验前检引用的模式绑定。
    # 参数：self为当前测试。
    # 返回值：无；默认组合、LOCAL裁剪或共享摘要漂移时失败。
    def test_root_normalization_and_shared_preflight_fingerprint(self):
        source = (FRONT.FIXTURE.PRODUCTION.parents[2] / '一键制作离线镜像包.sh').read_text()
        normalize = re.search(r'normalize_components\(\) \{.*?\n\}', source, re.S).group(0)
        fingerprint = re.search(r'platform_assets_input_fingerprint\(\) \{.*?\n\}', source, re.S).group(0)
        digests = []
        for mode in ('crm', 'local'):
            script = ('set -euo pipefail; source "$1/bin/offline-build-inputs.sh"; source "$1/bin/offline-frontend-selection.sh"; '
                      'script_dir="$2"; portal_directory_mode="$3"; selected_optional_components="customer-portal customer-opportunity"; '
                      'optional_components="customer-opportunity customer-portal contract project settlement data-analysis"; '
                      'info(){ :; }; die(){ exit 2; }; eval "$4"; eval "$5"; '
                      'normalize_components customer-portal; platform_assets_input_fingerprint')
            run = subprocess.run(['bash', '-c', script, 'fixture', str(self.production), str(self.workspace), mode, normalize, fingerprint], capture_output=True, text=True, timeout=30)
            self.assertEqual(run.returncode, 0, run.stderr)
            selection, digest = run.stdout.strip().splitlines()
            self.assertEqual('customer-opportunity' in selection.split(), mode == 'crm')
            digests.append(digest)
        self.assertNotEqual(*digests)


    # 用途：执行根入口五个真实子构建调用，确认每条分支固定传递已解析模式。
    # 参数：self为当前测试；使用只记录参数的替身构建器。
    # 返回值：无；任一分支遗漏模式或重新读取env时失败。
    def test_root_child_invocations_keep_resolved_mode(self):
        source = (FRONT.FIXTURE.PRODUCTION.parents[2] / '一键制作离线镜像包.sh').read_text()
        calls = []
        lines = iter(source.splitlines())
        for line in lines:
            if not line.lstrip().startswith('bash "$builder" '):
                continue
            block = [line]
            while block[-1].rstrip().endswith(chr(92)):
                block.append(next(lines))
            calls.append('\n'.join(block))
        self.assertEqual(len(calls), 5)
        builder = self.root / 'record-builder.sh'
        builder.write_text('#!/bin/bash\nprintf "%s\\0" "$@" >> "$PROBE_CALLS"\nprintf "\\n" >> "$PROBE_CALLS"\n')
        log = self.root / 'root-builder-calls'
        script = ('set -euo pipefail; builder="$1"; portal_directory_mode=local; '
                  'component_name=frontend; version=portal-mode; output_dir="$2"; '
                  'selected_optional_components=customer-portal; license_assets_args=(--formal-build);\n'
                  + '\n'.join(calls))
        env = dict(self.env, PORTAL_CUSTOMER_DIRECTORY_MODE='crm', PROBE_CALLS=str(log))
        result = subprocess.run(['bash', '-c', script, 'fixture', str(builder), str(self.output)],
                                env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        for line in log.read_text().splitlines():
            args = line.split(chr(0))
            self.assertEqual(args[args.index('--portal-customer-directory-mode') + 1], 'local')



class PortalMediaModeTests(unittest.TestCase):
    # 用途：建立真实tar/SHA/OCI检查夹具，不使用真实镜像或已有介质。
    # 参数：self为当前测试。
    # 返回值：无。
    def setUp(self):
        MEDIA.DeliveryTests.setUp(self)
        MEDIA.DeliveryTests.add_package(self, 'customer-portal')

    add_package = MEDIA.DeliveryTests.add_package
    finish = MEDIA.DeliveryTests.finish

    # 用途：只将新的发行模式写入临时夹具并重新绑定总校验。
    # 参数：mode为声明模式，bind表示是否故意遗漏校验更新。
    # 返回值：实际只读交付检查结果。
    def inspect_mode(self, mode=None, bind=True):
        info = self.media / 'BUILD_INFO.txt'
        lines = [line for line in info.read_text().splitlines() if not line.startswith('PORTAL_CUSTOMER_DIRECTORY_MODE=')]
        if mode is not None:
            lines.append('PORTAL_CUSTOMER_DIRECTORY_MODE=' + mode)
        info.write_text('\n'.join(lines) + '\n')
        if bind:
            self.finish()
        return MEDIA.CHECK.inspect(self.media, 'prepare')

    # 用途：只有已校验BUILD_INFO明确local才能免除CRM介质依赖，不读取进程env取消门禁。
    # 参数：self为当前测试。
    # 返回值：无；默认CRM、非法值或未绑定声明被误放行时失败。
    def test_only_bound_local_mode_changes_media_dependency(self):
        self.assertEqual(self.inspect_mode()[1], 2)
        previous = os.environ.get('PORTAL_CUSTOMER_DIRECTORY_MODE')
        os.environ['PORTAL_CUSTOMER_DIRECTORY_MODE'] = 'local'
        try:
            self.assertEqual(self.inspect_mode()[1], 2)
        finally:
            if previous is None:
                os.environ.pop('PORTAL_CUSTOMER_DIRECTORY_MODE', None)
            else:
                os.environ['PORTAL_CUSTOMER_DIRECTORY_MODE'] = previous
        report, code = self.inspect_mode('local')
        self.assertEqual(code, 0, report)
        self.assertEqual(report['portal_build_directory_mode'], 'local')
        self.assertEqual(self.inspect_mode('invalid')[1], 2)
        self.inspect_mode('crm')
        self.assertEqual(self.inspect_mode('local', bind=False)[1], 2)


    # 用途：本地发行必须具有总清单和源码摘要绑定，不能只写自由枚举取消依赖。
    # 参数：self为当前测试。
    # 返回值：无；无源码绑定的local声明被误接受时失败。
    def test_local_mode_requires_source_binding(self):
        info = self.media / 'BUILD_INFO.txt'
        info.write_text('\n'.join(line for line in info.read_text().splitlines()
                                  if not line.startswith('SOURCE_INPUT_SHA256=')) + '\n')
        self.assertEqual(self.inspect_mode('local')[1], 2)


if __name__ == '__main__':
    unittest.main()
