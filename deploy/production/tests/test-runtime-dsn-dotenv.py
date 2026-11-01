#!/usr/bin/env python3
"""用真实 Compose 的配置解析核对数据库密码读取及 DSN 文件往返，不启动容器。"""
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / 'bin/deploy-service.sh').read_text()


def extract(name):
    """用途：抽取正式发布脚本的真实助手函数，避免执行发布主流程。
    参数：name 为要执行的助手函数名。
    返回值：函数源文本；缺失时抛出断言错误。
    """
    match = re.search(r'^' + re.escape(name) + r'\(\) \{\n.*?^\}',
                      SOURCE, re.MULTILINE | re.DOTALL)
    assert match, 'missing runtime DSN helper'
    return match.group()


def run_helper(directory, command, extra=None):
    """用途：在独立临时目录执行真实配置助手，不接触运行环境。
    参数：directory 为夹具目录；command 为调用语句；extra 为合成输入环境。
    返回值：捕获的子进程结果；不向日志输出配置或凭据。
    """
    harness = ('deploy_dir="$1"\nruntime_file="$1/.env"\n'
               'release_file="$1/.release.env"\n'
               + extract('dsn_env_value') + '\n'
               + extract('write_runtime_dsn_value') + '\n' + command)
    return subprocess.run(['bash', '-Eeuo', 'pipefail', '-c', harness,
                           'fixture', str(directory)], text=True,
                          capture_output=True, env={**os.environ, **(extra or {})})


with tempfile.TemporaryDirectory(prefix='runtime-dsn-dotenv-') as temporary:
    directory = Path(temporary)
    (directory / '.release.env').write_text('RELEASE_VERSION=fixture\n')
    (directory / 'runtime').mkdir()
    for label, definition, expected in [
        ('single-quoted', "'synthetic-password'", 'synthetic-password'),
        ('double-quoted-comment', '"synthetic value" # note', 'synthetic value'),
        ('unquoted', 'synthetic-plain', 'synthetic-plain'),
        ('literal-dollar', "'synthetic-$NOT_DEFINED'", 'synthetic-$NOT_DEFINED'),
        ('duplicate-last-wins', 'first\nPROJECT_MYSQL_PASSWORD=last', 'last'),
    ]:
        (directory / '.env').write_text('PROJECT_MYSQL_PASSWORD=' + definition + '\n')
        result = run_helper(directory, 'dsn_env_value PROJECT_MYSQL_PASSWORD')
        assert result.returncode == 0, label + ': helper failed'
        assert result.stdout.rstrip('\n') == expected, label + ': differs from Compose password'

    for case_number, password in enumerate(['synthetic-plain', 'synthetic-$NOT_DEFINED',
                     "synthetic-'quote", 'synthetic-"quote', r'synthetic-\path', r'synthetic-\n', r'synthetic-\\path', "synthetic-\\'quote"]):
        target = directory / 'runtime/project.env'
        target.write_text('# keep comment\nMYSQL_DSN=old\nKEEP=unchanged\n')
        dsn = 'project:' + password + '@tcp(project-mysql:3306)/project_management'
        result = run_helper(directory,
                            'write_runtime_dsn_value "$deploy_dir/runtime/project.env" '
                            'MYSQL_DSN "$SYNTHETIC_DSN"', {'SYNTHETIC_DSN': dsn})
        assert result.returncode == 0, 'runtime write failed'
        compose = directory / 'probe.yaml'
        compose.write_text('services:\n  probe:\n    image: scratch\n'
                           '    env_file:\n      - runtime/project.env\n')
        parsed = subprocess.run(['docker', 'compose', '--project-directory', temporary,
                                 '--env-file', str(directory / '.env'), '-f', str(compose),
                                 'config', '--format', 'json'], capture_output=True, text=True)
        assert parsed.returncode == 0, 'runtime Compose parse failed'
        # Compose config 按官方实现将 $ 转义为 $$，此处核对解码后的容器环境值。
        decoded = json.loads(parsed.stdout)['services']['probe']['environment']['MYSQL_DSN']
        assert decoded.replace('$$', '$') == dsn, \
            'runtime DSN does not survive Compose parsing, fixture=' + str(case_number)
        assert stat.S_IMODE(target.stat().st_mode) == 0o600, 'runtime mode is not 0600'
        assert '# keep comment' in target.read_text(), 'runtime comment lost'
        assert 'KEEP=unchanged' in target.read_text(), 'unrelated runtime value lost'
        assert not list((directory / 'runtime').glob('.runtime-dsn.*')), 'temporary file leaked'

        if os.environ.get('QA_RUNTIME_DSN_REAL_CONTAINER') == 'true':
            # 只用本轮既有隔离 Nginx 镜像；不联网、不映射端口、不挂载业务数据。
            image = ('nginx:1.27-alpine@sha256:'
                     '65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10')
            compose.write_text('services:\n  probe:\n    image: ' + image + '\n'
                               '    network_mode: none\n    env_file:\n'
                               '      - runtime/project.env\n')
            actual = subprocess.run(
                ['docker', 'compose', '--project-name', 'qa-runtime-dsn',
                 '--project-directory', temporary, '-f', str(compose),
                 'run', '--rm', '--no-deps', '--pull', 'never',
                 '--entrypoint', '/bin/sh', 'probe', '-c', 'printf "%s" "$MYSQL_DSN"'],
                capture_output=True, text=True)
            assert actual.returncode == 0, 'isolated runtime container failed'
            assert actual.stdout == dsn, 'real container received a different DSN'

    original = target.read_bytes()
    rejected = run_helper(directory,
                          'write_runtime_dsn_value "$deploy_dir/runtime/project.env" '
                          'MYSQL_DSN "$SYNTHETIC_DSN"', {'SYNTHETIC_DSN': 'line1\nline2'})
    assert rejected.returncode != 0, 'multiline DSN accepted'
    assert target.read_bytes() == original, 'rejected input changed the existing file'
    invalid = run_helper(directory, 'dsn_env_value INVALID-KEY')
    assert invalid.returncode != 0, 'invalid variable name accepted'

print('runtime DSN: quoted source and literal secret round trips passed')
