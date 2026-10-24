#!/usr/bin/env python3
"""首次安装/数据库未就绪时的发布前门禁：真实抽取 deploy-service.sh 的助手函数运行。

覆盖三个现场故障模式：
1. compose 报“启动成功”但此刻 mysqld 不能执行 SQL（被杀/重启中）——必须判失败并输出诊断，
   而不是让 mysqldump 报出难以理解的 socket 错误；
2. 首次安装时目标库尚未创建——跳过发布前备份，不把首次发布判为失败；
3. 升级发布（库已存在）——仍然必须备份，且备份失败必须中止。
"""
import os
import re
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / 'bin/deploy-service.sh').read_text()


def extract(name):
    match = re.search(r'^%s\(\) \{\n.*?^\}' % re.escape(name), SCRIPT,
                      re.MULTILINE | re.DOTALL)
    assert match, 'helper not found in deploy-service.sh: ' + name
    return match.group()


# 首次安装的 MySQL 冷启动实测约 4.5 分钟；等待预算必须可配置且默认不低于 600s，
# 否则等待会在初始化完成前放弃，并把随后的备份失败误报成发布失败。
assert SCRIPT.count('BASIC_PLATFORM_DB_START_TIMEOUT') >= 10, \
    'database start budget must stay configurable on every DB wait/probe'
assert 'BASIC_PLATFORM_DB_START_TIMEOUT:-600' in SCRIPT, 'default DB start budget must be 600s'

HELPERS = '\n'.join(extract(n) for n in (
    'mysql_query_scalar',
    'diagnose_mysql_container',
    'require_mysql_ready',
    'backup_database_if_initialized',
))

HARNESS = r'''
set -Eeuo pipefail
compose() {
  case "$*" in
    *"mysql -uroot -Nse"*)
      [[ "${MOCK_DB_UP:-true}" == true ]] || return 1
      if [[ "$*" == *information_schema* ]]; then
        printf '%s\n' "${MOCK_SCHEMA_COUNT:-0}"
      else
        printf '1\n'
      fi
      ;;
    *"ps -q"*) printf 'fixture-container\n' ;;
    *) return 0 ;;
  esac
}
docker() { printf '状态=running 健康=starting 退出码=0 重启次数=3 OOMKilled=true\n'; }
free() { printf 'free stubbed\n'; }
backup_database() { printf 'BACKUP-CALLED\n'; }
service=platform
'''

CASES = r'''
echo "== case1: mysql 未就绪必须失败并输出诊断"
if MOCK_DB_UP=false require_mysql_ready platform-mysql 4 >/dev/null 2>"$TEST_ROOT/case1.err"; then
  echo "case1 unexpectedly succeeded" >&2; exit 1
fi
grep -q '无法执行 SQL' "$TEST_ROOT/case1.err" || { echo 'missing readiness failure reason' >&2; exit 1; }
grep -q '诊断：platform-mysql' "$TEST_ROOT/case1.err" || { echo 'missing container diagnostics' >&2; exit 1; }
grep -q 'OOMKilled=true' "$TEST_ROOT/case1.err" || { echo 'missing OOM evidence' >&2; exit 1; }
grep -q '内存基线' "$TEST_ROOT/case1.err" || { echo 'missing memory guidance' >&2; exit 1; }

echo "== case2: mysql 就绪则通过"
MOCK_DB_UP=true require_mysql_ready platform-mysql 4 >/dev/null || { echo 'ready mysql rejected' >&2; exit 1; }

echo "== case3: 首次安装（库不存在）跳过备份且不判失败"
out="$(MOCK_SCHEMA_COUNT=0 backup_database_if_initialized platform-mysql basic_platform)"
case "$out" in
  *BACKUP-CALLED*) echo 'first install unexpectedly ran a backup' >&2; exit 1 ;;
esac
case "$out" in
  *尚未创建*) : ;;
  *) echo 'first install skip reason missing' >&2; exit 1 ;;
esac

echo "== case4: 升级发布（库存在）必须备份"
out="$(MOCK_SCHEMA_COUNT=1 backup_database_if_initialized platform-mysql basic_platform)"
case "$out" in
  *BACKUP-CALLED*) : ;;
  *) echo 'existing database was not backed up' >&2; exit 1 ;;
esac

echo "== case5: 探测不到初始化状态必须失败"
if MOCK_DB_UP=false backup_database_if_initialized platform-mysql basic_platform >/dev/null 2>&1; then
  echo 'unreachable mysql was treated as initialized' >&2; exit 1
fi

echo 'mysql readiness and first-install backup gates passed'
'''

with tempfile.TemporaryDirectory(prefix='mysql-readiness-') as directory:
    root = Path(directory)
    script = HARNESS + '\n' + HELPERS + '\n' + CASES
    process = subprocess.run(['bash', '-c', script],
                             env={'TEST_ROOT': str(root), 'PATH': '/usr/bin:/bin'},
                             capture_output=True, text=True)
    if process.returncode != 0:
        raise SystemExit(process.stdout + process.stderr)
    print(process.stdout.strip())
