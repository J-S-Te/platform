#!/usr/bin/env python3
"""只用临时资产包验证本批兼容性读取与旧声明隔离。"""
import hashlib
import io
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(os.environ.get('UIP_ENGINEERING_ROOT', Path(__file__).resolve().parents[4]))
SOURCE = (ROOT / '服务器一键离线部署.sh').read_text()
PLAN = Path(__file__).resolve().parents[1] / 'bin/deployment-plan.py'


# 用途：提取实际服务器入口函数供隔离行为验证。
# 参数：name 为待执行的生产函数名。
# 返回值：函数源码；缺失函数保留为未定义，以暴露尚未实现的能力。
def function(name):
    marker = name + '() {'
    if marker not in SOURCE:
        return ''
    return marker + SOURCE.split(marker, 1)[1].split('\n}', 1)[0] + '\n}'


class MediaCompatibilityTests(unittest.TestCase):
    # 用途：建立隔离介质、声明输出和旧部署目录。
    # 参数：无。
    # 返回值：无；所有文件均位于自动清理的临时目录。
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.archive = self.path / 'assets.tar.gz'
        self.output = self.path / 'current.json'
        self.target = self.path / 'target'
        (self.target / 'bin').mkdir(parents=True)
        shutil.copy2(PLAN, self.target / 'bin/deployment-plan.py')
        self.old = dict(schema='uip.deployment-compatibility/v1', release='old',
                        assets_contract='1', config_contract='1',
                        database=dict(migrate_to=1, previous_program_after_migration='compatible'),
                        recovery=dict(type='backward', requires_backup=True))
        (self.target / 'deployment-compatibility.json').write_text(json.dumps(self.old))
        self.current = dict(self.old, release='new',
                            database=dict(migrate_to=2, previous_program_after_migration='unknown'),
                            recovery=dict(type='forward-only', requires_backup=True))

    # 用途：创建真实 gzip TAR 以覆盖声明类型和重复成员边界。
    # 参数：members 为成员名、正文与 TAR 类型三元组集合。
    # 返回值：无；归档仅写入隔离临时目录。
    def pack(self, members):
        with tarfile.open(self.archive, 'w:gz') as archive:
            for name, raw, kind in members:
                member = tarfile.TarInfo(name)
                member.type = kind
                member.size = len(raw) if kind == tarfile.REGTYPE else 0
                member.linkname = 'elsewhere' if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE) else ''
                archive.addfile(member, io.BytesIO(raw))

    # 用途：执行实际安全声明提取函数并复核绑定摘要。
    # 参数：expected 为期望资产包摘要，省略时采用当前夹具摘要。
    # 返回值：子进程退出码和输出，不访问运行环境。
    def extract(self, expected=None):
        digest = expected or hashlib.sha256(self.archive.read_bytes()).hexdigest()
        code = 'set -Eeuo pipefail\ndie() { echo "$*" >&2; exit 7; }\n'
        code += function('extract_media_compatibility')
        code += '\nextract_media_compatibility ' + ' '.join(
            shlex.quote(str(value)) for value in (self.archive, self.output, digest))
        return subprocess.run(['bash', '-c', code], text=True, capture_output=True, timeout=10)

    # 用途：验证声明正文与受保护文件权限保持准确。
    # 参数：无。
    # 返回值：无；任何未提取或正文变化都会断言失败。
    def test_extracts_unique_current_declaration(self):
        raw = json.dumps(self.current).encode()
        self.pack([('./deployment-compatibility.json', raw, tarfile.REGTYPE)])
        result = self.extract()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.output.read_bytes(), raw)
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o600)

    # 用途：拒绝缺失、重复、链接、目录和超限声明，不产生可用输出。
    # 参数：无。
    # 返回值：无；所有危险边界必须失败关闭。
    def test_rejects_missing_duplicate_link_and_oversized_declarations(self):
        raw = json.dumps(self.current).encode()
        name = 'deployment-compatibility.json'
        variants = [[], [(name, raw, tarfile.REGTYPE), ('./' + name, raw, tarfile.REGTYPE)],
                    [(name, b'', tarfile.SYMTYPE)], [(name, b'', tarfile.LNKTYPE)],
                    [(name, b'', tarfile.DIRTYPE)], [(name, b'x' * (128 * 1024 + 1), tarfile.REGTYPE)]]
        for members in variants:
            with self.subTest(members=[(n, len(v), k) for n, v, k in members]):
                self.pack(members)
                result = self.extract()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertFalse(self.output.exists())

    # 用途：阻止资产包在校验后被替换，以及声明覆盖现有文件或链接。
    # 参数：无。
    # 返回值：无；绑定不符与不安全输出都不得写文件。
    def test_rejects_changed_assets_and_existing_output(self):
        self.pack([('deployment-compatibility.json', b'{}', tarfile.REGTYPE)])
        result = self.extract('0' * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.output.exists())
        self.output.symlink_to(self.target / 'deployment-compatibility.json')
        result = self.extract()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(self.output.read_text())['release'], 'old')

    # 用途：验证正式门禁使用本批声明，不能静默接受旧部署的 compatible。
    # 参数：无。
    # 返回值：无；本批未知兼容性必须阻断普通升级。
    def test_formal_gate_uses_current_assets_instead_of_old_target(self):
        self.output.write_text(json.dumps(self.current))
        code = 'set -Eeuo pipefail\ndie() { echo "$*" >&2; exit 7; }\n'
        code += 'workflow_update() { :; }\n' + function('compatibility_gate')
        assignments = dict(target_dir=self.target, media_dir=self.path,
                           media_compatibility_file=self.output, forward_recovery='false',
                           recovery_evidence='', plan_baseline='')
        code += '\n' + '\n'.join(f'{key}={shlex.quote(str(value))}'
                                    for key, value in assignments.items())
        result = subprocess.run(['bash', '-c', code + '\ncompatibility_gate'],
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 7, result.stdout + result.stderr)
        self.assertIn('默认阻断升级', result.stdout, result.stderr)


    # 用途：验证顶层副本不一致时拒绝，成功与失败均清理私有目录。
    # 参数：无。
    # 返回值：无；未绑定副本不能成为本批声明来源。
    def test_preparation_checks_optional_top_copy_and_cleans_private_directory(self):
        self.pack([('deployment-compatibility.json', json.dumps(self.current).encode(), tarfile.REGTYPE)])
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        lock = self.path / 'compatibility.env'
        lock.write_text('ASSETS_PACKAGE_SHA256=' + digest + '\n')
        preparation = 'media_compatibility_file=""' + SOURCE.split(
            '\nmedia_compatibility_file=""', 1)[1].split('\nprintf ', 1)[0]
        code = 'set -Eeuo pipefail\ndie() { echo "$*" >&2; exit 7; }\n'
        for name in ('extract_media_compatibility', 'cleanup_media_compatibility',
                     'compatibility_value', 'require_media_binding'):
            code += function(name) + '\n'
        values = dict(media_dir=self.path, compatibility_lock=lock, deployment_mode='upgrade',
                      TMPDIR=self.path)
        code += '\n'.join(f'{key}={shlex.quote(str(value))}' for key, value in values.items())
        code += '\nassets=(' + shlex.quote(str(self.archive)) + ')\n' + preparation
        code += '\ncat "$media_compatibility_file"\n'
        top = self.path / 'deployment-compatibility.json'
        for value, bound, expected in [(None, False, 0), (self.current, True, 0),
                                       (self.old, True, 7), (self.current, False, 7)]:
            with self.subTest(top=value, bound=bound):
                top.unlink(missing_ok=True)
                if value is not None:
                    top.write_text(json.dumps(value))
                manifest = hashlib.sha256(top.read_bytes()).hexdigest() if value else '0' * 64
                (self.path / 'SHA256SUMS').write_text(
                    f'{manifest}  deployment-compatibility.json\n' if bound else '')
                result = subprocess.run(['bash', '-c', code], text=True, capture_output=True)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                if expected == 0:
                    self.assertEqual(json.loads(result.stdout)['release'], 'new')
                self.assertEqual(list(self.path.glob('uip-compatibility.*')), [])


    # 用途：验证计划基线与 check-only 接线均将本批声明交给真实计划工具。
    # 参数：无。
    # 返回值：无；屏蔽宿主 Docker 后，只断言真实计划报告的本批兼容性结论。
    def test_baseline_and_check_only_report_current_declaration(self):
        self.output.write_text(json.dumps(self.current))
        (self.target / '.env').write_text('')
        deploy = self.target / 'bin/deploy.sh'
        deploy.write_text('shift\nexec python3 "$(dirname "$0")/deployment-plan.py" --target "$PWD" "$@"\n')
        # 隔离 PATH 禁止计划工具发现宿主 Docker；dirname 也只使用本机普通工具。
        tools = self.path / 'tools'
        tools.mkdir()
        (tools / 'python3').symlink_to(sys.executable)
        (tools / 'dirname').symlink_to(shutil.which('dirname'))
        (tools / 'bash').symlink_to('/bin/bash')
        values = dict(target_dir=self.target, media_dir=self.path, PATH=tools,
                      media_compatibility_file=self.output, forward_recovery='false',
                      recovery_evidence='', resume_installation='false', deployment_mode='upgrade',
                      delivery_output_format='human', assets_preflight_skipped='false')
        prefix = 'set -Eeuo pipefail\ndie() { echo "$*" >&2; exit 7; }\n'
        prefix += 'warn() { :; }; info() { :; }; requested_subsystems=(contract)\n'
        prefix += '\n'.join(f'{key}={shlex.quote(str(value))}' for key, value in values.items()) + '\n'
        check_code = ('if [[ "$check_only" == true ]]; then\n  # 已安装目标' + SOURCE.split(
            'if [[ "$check_only" == true ]]; then\n  # 已安装目标', 1)[1]).split(
                "\nprintf ", 1)[0]
        cases = [('check_only=false\nplan_baseline=' + shlex.quote(str(self.path / 'baseline.json')) +
                  '\n' + function('deployment_baseline_gate') + '\ndeployment_baseline_gate'),
                 'check_only=true\nplan_baseline=""\n' + check_code]
        for case in cases:
            with self.subTest(path='check-only' if 'check_only=true' in case else 'baseline'):
                result = subprocess.run(['/bin/bash', '-c', prefix + case],
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 7, result.stdout + result.stderr)
                self.assertIn('默认阻断升级', result.stdout, result.stderr)


    # 用途：验证 workflow 接管 EXIT 后仍清理本批声明并保留真实退出码。
    # 参数：无。
    # 返回值：无；成功和失败退出都不得遗留私有声明目录。
    def test_workflow_exit_cleans_declaration_after_replacing_initial_trap(self):
        workflow = PLAN.with_name('deployment-workflow.sh')
        for result_code in (0, 23):
            with self.subTest(exit_code=result_code):
                directory = self.path / f'compatibility-{result_code}'
                directory.mkdir()
                (directory / 'deployment-compatibility.json').write_text(json.dumps(self.current))
                code = 'set -Eeuo pipefail\n' + function('cleanup_media_compatibility')
                code += '\nsource ' + shlex.quote(str(workflow))
                code += '\nunset UIP_INSTALL_SESSION\nmedia_compatibility_dir=' + shlex.quote(str(directory))
                code += '\ntrap cleanup_media_compatibility EXIT\ntrap workflow_exit EXIT\n'
                result = subprocess.run(['bash', '-c', code + f'exit {result_code}'],
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, result_code, result.stdout + result.stderr)
                self.assertFalse(directory.exists(), result.stdout + result.stderr)


    # 用途：验证续跑和新会话安装替换 target 声明后，本批私有声明仍保持稳定。
    # 参数：无。
    # 返回值：无；每次运行使用不同私有路径，且正式门禁仍按本批未知兼容性阻断。
    def test_resume_and_new_session_keep_private_declaration_after_asset_replacement(self):
        raw = json.dumps(self.current).encode()
        self.pack([('deployment-compatibility.json', raw, tarfile.REGTYPE)])
        lock = self.path / 'compatibility.env'
        lock.write_text('ASSETS_PACKAGE_SHA256=' + hashlib.sha256(self.archive.read_bytes()).hexdigest() + '\n')
        preparation = 'media_compatibility_file=""' + SOURCE.split(
            '\nmedia_compatibility_file=""', 1)[1].split('\nprintf ', 1)[0]
        prefix = 'set -Eeuo pipefail\ndie() { echo "$*" >&2; exit 7; }\n'
        prefix += 'workflow_update() { :; }\n'
        for name in ('extract_media_compatibility', 'cleanup_media_compatibility',
                     'compatibility_value', 'require_media_binding', 'compatibility_gate'):
            prefix += function(name) + '\n'
        values = dict(target_dir=self.target, media_dir=self.path, compatibility_lock=lock,
                      deployment_mode='upgrade', TMPDIR=self.path, forward_recovery='false',
                      recovery_evidence='', plan_baseline='')
        prefix += '\n'.join(f'{key}={shlex.quote(str(value))}' for key, value in values.items())
        prefix += '\nassets=(' + shlex.quote(str(self.archive)) + ')\n'
        observed = self.path / 'stable-copy.json'
        path_record = self.path / 'private-path.txt'
        target_file = self.target / 'deployment-compatibility.json'
        paths = []
        for resume, new_session in (('true', 'false'), ('false', 'true')):
            with self.subTest(resume=resume, new_session=new_session):
                code = prefix + f'resume_installation={resume}\nnew_installation_session={new_session}\n'
                code += preparation + '\n'
                code += 'printf "%s" "$media_compatibility_file" > ' + shlex.quote(str(path_record)) + '\n'
                code += 'rm -- ' + shlex.quote(str(target_file)) + '\n'
                code += 'printf "%s" ' + shlex.quote(json.dumps(self.old)) + ' > ' + shlex.quote(str(target_file)) + '\n'
                code += 'cp "$media_compatibility_file" ' + shlex.quote(str(observed)) + '\ncompatibility_gate'
                result = subprocess.run(['bash', '-c', code], text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 7, result.stdout + result.stderr)
                self.assertIn('默认阻断升级', result.stdout, result.stderr)
                self.assertEqual(observed.read_bytes(), raw)
                self.assertEqual(json.loads(target_file.read_text())['release'], 'old')
                paths.append(path_record.read_text())
                self.assertNotEqual(Path(paths[-1]), target_file)
                self.assertFalse(Path(paths[-1]).parent.exists())
        self.assertNotEqual(paths[0], paths[1])


if __name__ == '__main__':
    unittest.main()
