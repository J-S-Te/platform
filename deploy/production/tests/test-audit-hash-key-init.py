#!/usr/bin/env python3
"""在私有临时目录验证真实审计密钥初始化器，不接触业务库或运行配置。"""
import base64
from pathlib import Path
import re
import stat
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / 'bin/initialize-audit-hash-key.py'


def execute(env_file, backup, history='0', check=False):
    """用途：执行真实初始化器并捕获输出，检查密钥不会进入日志。
    参数：env_file 为临时配置，backup 为私有备份路径，history 为历史摘要标志，check 为只读模式。
    返回值：子进程结果；调用方按测试场景核验退出状态。
    """
    command = ['python3', '-B', str(HELPER), '--env', str(env_file)]
    command += ['--check'] if check else ['--backup', str(backup), '--has-hmac-history', history]
    return subprocess.run(command, capture_output=True, text=True)


with tempfile.TemporaryDirectory(prefix='audit-key-init-') as directory:
    root = Path(directory)
    env_file, backup = root / '.env', root / 'audit-key.initial.env'
    initial = '# existing configuration\nAPP_ENV=production\nPLATFORM_AUDIT_HASH_KEY=\n'
    env_file.write_text(initial)
    env_file.chmod(0o600)
    assert execute(env_file, backup, check=True).returncode == 3
    result = execute(env_file, backup, history='1')
    assert result.returncode != 0 and not backup.exists()
    assert env_file.read_text() == initial, 'lost historical key must never be replaced'
    result = execute(env_file, backup)
    assert result.returncode == 0, result.stderr
    key = env_file.read_text().split('PLATFORM_AUDIT_HASH_KEY=', 1)[1].strip()
    assert len(base64.b64decode(key, validate=True)) == 32
    assert key not in result.stdout + result.stderr
    assert backup.read_text() == 'PLATFORM_AUDIT_HASH_KEY=' + key + '\n'
    assert stat.S_IMODE(backup.stat().st_mode) == 0o600
    assert stat.S_IMODE(env_file.stat().st_mode) == 0o600
    assert '# existing configuration\nAPP_ENV=production\n' in env_file.read_text()
    previous = env_file.read_bytes(), backup.read_bytes()
    assert execute(env_file, backup, history='1').returncode == 0
    assert (env_file.read_bytes(), backup.read_bytes()) == previous, 'configured key must remain stable'
    env_file.write_text(initial)
    assert execute(env_file, backup).returncode == 0
    assert (env_file.read_bytes(), backup.read_bytes()) == previous, 'interrupted initialization must reuse backup'
    for invalid in ('not-base64', base64.b64encode(b'short').decode()):
        env_file.write_text('PLATFORM_AUDIT_HASH_KEY=' + invalid + '\n')
        assert execute(env_file, backup).returncode != 0
        assert env_file.read_text() == 'PLATFORM_AUDIT_HASH_KEY=' + invalid + '\n'
    env_file.write_text(initial + 'PLATFORM_AUDIT_HASH_KEY=\n')
    assert execute(env_file, backup).returncode != 0, 'duplicate keys must fail closed'
    env_file.write_text(initial)
    backup.chmod(0o644)
    assert execute(env_file, backup).returncode != 0, 'insecure backup must not be reused'
    backup.unlink()
    backup.symlink_to(root / 'missing-target')
    assert execute(env_file, backup).returncode != 0
    backup.unlink()
    env_file.unlink()
    target = root / 'target.env'
    target.write_text(initial)
    env_file.symlink_to(target)
    assert execute(env_file, backup).returncode != 0
    assert target.read_text() == initial
    assert not list(root.glob('.audit-env.*'))

# 提取实际发布函数验证“只读检查→历史查询→初始化”的接线和查询失败关闭。
script = (ROOT / 'bin/deploy-service.sh').read_text()
function = re.search(r'^initialize_platform_audit_hash_key\(\) \{\n.*?^\}',
                     script, re.MULTILINE | re.DOTALL).group()
with tempfile.TemporaryDirectory(prefix='audit-key-publish-') as directory:
    root = Path(directory)
    (root / 'backups').mkdir()
    env_file, backup = root / '.env', root / 'backups/platform-audit-hash-key.initial.env'
    env_file.write_text('APP_ENV=production\nMYSQL_DATABASE=audit_fixture\n')
    env_file.chmod(0o600)
    harness = '''set -Eeuo pipefail
deploy_dir="$1"
runtime_file="$deploy_dir/.env"
script_dir="$2"
env_value() { sed -n "s/^$1=//p" "$runtime_file"; }
mysql_query_scalar() {
  [[ "$1" == platform-mysql && "$2" == *"audit_event"* && "$2" == *"SHA-256"* ]] || return 9
  [[ "$2" == *"audit_fixture.audit_event"* ]] || return 9
  [[ "$2" == *"audit_fixture.audit_archive_item"* ]] || return 9
  [[ "$2" == *"audit_fixture.audit_event_chain"* ]] || return 9
  [[ "$2" == *"audit_archive_item"* && "$2" == *"purged_at IS NOT NULL"* ]] || return 9
  [[ "$2" == *"audit_event_chain"* ]] || return 9
  [[ "${TEST_HISTORY}" != fail ]] || return 8
  printf '%s' "${TEST_HISTORY}"
}
''' + function + '\ninitialize_platform_audit_hash_key\n'
    for history, expected in [('fail', False), ('invalid', False), ('1', False), ('0', True), ('1', True)]:
        result = subprocess.run(
            ['bash', '-c', 'TEST_HISTORY="$3"\n' + harness, 'bash', str(root), str(ROOT / 'bin'), history],
            capture_output=True, text=True,
        )
        assert (result.returncode == 0) == expected, result.stdout + result.stderr
        assert backup.exists() == expected

print('Audit hash key initialization, protected backup, stable reuse and loss guards: PASS')
