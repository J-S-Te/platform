#!/usr/bin/env python3
"""使用文件系统和命令夹具验证备份失败门禁，不连接数据库。"""
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[4]
WRAPPER = (ROOT / '服务器一键离线部署.sh').read_text()
SERVICE = (ROOT / 'platform/deploy/production/bin/deploy-service.sh').read_text()


def function(source, name):
    return name + '() {' + source.split(name + '() {', 1)[1].split('\n}', 1)[0] + '\n}'


def shell(code, env=None):
    return subprocess.run(['bash', '-c', 'set -Eeuo pipefail\n' + code],
                          text=True, capture_output=True, timeout=15, env=env)


class BackupGates(unittest.TestCase):
    def test_root_backup_failure_and_missing_entry_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp)
            (target / 'bin').mkdir()
            fn = function(WRAPPER, 'backup_before_upgrade')
            code = (f'target_dir={shlex.quote(tmp)}; '
                    'die() { exit 7; }; info() { :; };\n' + fn + '\nbackup_before_upgrade; echo NEXT')
            self.assertEqual(shell(code).returncode, 7)
            entry = target / 'bin/backup-all.sh'
            for exit_code, expected in [(1, 7), (0, 0)]:
                entry.write_text(f'#!/bin/bash\nexit {exit_code}\n')
                result = shell(code)
                self.assertEqual(result.returncode, expected)
                self.assertEqual('NEXT' in result.stdout, exit_code == 0)
            entry.unlink()
            entry.symlink_to(target / 'bin/missing')
            self.assertEqual(shell(code).returncode, 7)

    def test_backup_precedes_asset_and_runtime_changes(self):
        upgrade = WRAPPER.split('if [[ "$deployment_mode" == upgrade ]]; then', 1)[1]
        backup_gate = upgrade.index('\n  workflow_run backup backup_before_upgrade')
        self.assertLess(backup_gate, upgrade.index('import "$media_dir/'))
        self.assertLess(backup_gate, upgrade.index('\n  workflow_assets_install'))
        gate = SERVICE.index('bash "$script_dir/backup-all.sh" --if-existing --deployment-lock-fd 9')
        self.assertLess(SERVICE.index('flock -w 900 9'), gate)
        self.assertLess(gate, SERVICE.index('    prepare_runtime_file "$contract_runtime_file"'))
        self.assertLess(gate, SERVICE.index('mv "$next_release" "$release_file"'))
        start = (ROOT / 'platform/deploy/production/bin/start-enabled.sh').read_text()
        self.assertNotIn('"$script_dir/backup-all.sh" || return', start)
        self.assertIn('"$script_dir/backup-all.sh" --deployment-lock-fd 7 || return', start)
        backup = (ROOT / 'platform/deploy/production/bin/backup-all.sh').read_text()
        gateway = (ROOT / 'platform/deploy/production/bin/backup-file-gateway.sh').read_text()
        self.assertIn('exec {backup_lock_fd}', backup)
        self.assertIn('exec {gateway_backup_lock_fd}', gateway)

    def test_workflow_backup_failure_blocks_following_changes(self):
        workflow = (ROOT / 'platform/deploy/production/bin/deployment-workflow.sh').read_text()
        for start_status, backup_status, done_status, expected in [
            (0, 0, 0, 0), (0, 7, 0, 7), (8, 0, 0, 8), (0, 0, 9, 9)
        ]:
            with self.subTest(start=start_status, backup=backup_status, done=done_status):
                code = (f'workflow_start() {{ echo START; return {start_status}; }};\n'
                        f'backup_before_upgrade() {{ echo BACKUP; return {backup_status}; }};\n'
                        f'workflow_done() {{ echo DONE; return {done_status}; }};\n'
                        + function(workflow, 'workflow_run')
                        + '\nworkflow_run backup backup_before_upgrade\necho NEXT')
                result = shell(code)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertEqual('BACKUP' in result.stdout, start_status == 0)
                self.assertEqual('DONE' in result.stdout, start_status == backup_status == 0)
                self.assertEqual('NEXT' in result.stdout, expected == 0)

    def test_release_database_dump_rejects_empty_or_failed_sql(self):
        fn = function(SERVICE, 'backup_database')
        for mode, expected in [('ok', 0), ('empty', 1), ('failed', 1), ('checksum-failed', 1)]:
            with tempfile.TemporaryDirectory() as tmp:
                (Path(tmp) / 'backups').mkdir()
                compose = ('compose() { '
                           + ('return 9;' if mode == 'failed' else
                              'return 0;' if mode == 'empty' else "printf 'CREATE TABLE t(id INT);\\n';") + ' };')
                checksum = ''
                if mode == 'checksum-failed':
                    checksum = 'sha256sum() { if [[ "$1" == --check ]]; then return 9; fi; command sha256sum "$@"; };'
                code = (f'deploy_dir={shlex.quote(tmp)}; service=contract; release_id=test; '
                        + compose + checksum + '\n' + fn + '\nbackup_database contract-mysql contract_management')
                result = shell(code)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertFalse(list((Path(tmp) / 'backups').glob('.contract*')))
                if mode == 'ok':
                    self.assertEqual((Path(tmp) / 'backups/contract-test.sql.gz').stat().st_mode & 0o777, 0o600)
                    self.assertEqual((Path(tmp) / 'backups/contract-test.sql.gz.sha256').stat().st_mode & 0o777, 0o600)
                self.assertIn('--all-databases', fn)

    def test_backup_records_snapshot_binlog_position(self):
        backup = (ROOT / 'platform/deploy/production/bin/backup-all.sh').read_text()
        keycloak = (ROOT / 'platform/deploy/production/bin/backup-keycloak-mysql.sh').read_text()
        for name, source in [('backup-all.sh', backup), ('backup-keycloak-mysql.sh', keycloak)]:
            with self.subTest(script=name):
                self.assertIn('--source-data=2', source)
                self.assertIn('--single-transaction', source)
                self.assertIn('SELECT @@log_bin', source)
                self.assertIn('binlog-position', source)
        self.assertIn('BINLOG_POSITIONS=required', backup)

    def test_fresh_detection_never_swallows_docker_failures_or_existing_data(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp)
            (target / 'bin').mkdir()
            tools = target / 'tools'
            tools.mkdir()
            if sys.platform == 'darwin':
                (tools / 'flock').write_text('#!/bin/bash\nexit 0\n')
                (tools / 'flock').chmod(0o700)
            source = ROOT / 'platform/deploy/production/bin/backup-all.sh'
            entry = target / 'bin/backup-all.sh'
            backup_source = source.read_text()
            if sys.platform == 'darwin':
                # 宿主机使用 Bash 3，生产要求 Bash 4 动态文件描述符；仅在临时夹具中适配。
                backup_source = backup_source.replace('exec {backup_lock_fd}>', 'backup_lock_fd=11\nexec 11>')
            entry.write_text(backup_source)
            (target / '.env').write_text('COMPOSE_PROJECT_NAME=fixture\n')
            (target / '.release.env').write_text('PLATFORM_IMAGE=fixture\n')
            (target / 'docker-compose.yml').write_text('services: {}\n')
            docker = tools / 'docker'
            docker.write_text('''#!/usr/bin/env bash
set -eu
if [[ "$1" == compose ]]; then
  case "$*" in
    *'config --services')
      [[ "$MODE" != config-failed ]] || exit 9
      printf 'platform-mysql\n'
      ;;
    *'ps -a -q platform-mysql')
      [[ "$MODE" != ps-failed ]] || exit 9
      [[ "$MODE" != stopped && "$MODE" != empty-dump && "$MODE" != dump-failed ]] || printf 'db-id\n'
      ;;
    *'exec -T platform-mysql'*)
      if [[ "$*" == *'SELECT @@log_bin'* ]]; then
        printf '1\n'
      elif [[ "$MODE" == dump-failed ]]; then
        exit 9
      elif [[ "$MODE" != empty-dump ]]; then
        printf '%s\n' "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000001', SOURCE_LOG_POS=4;"
        printf '%s\n' 'CREATE TABLE fixture (id INT);'
      fi
      ;;
    *) exit 99 ;;
  esac
elif [[ "$1" == volume ]]; then
  [[ "$MODE" != volumes-failed ]] || exit 9
  if [[ "$MODE" == retained && "$*" == *platform-mysql-data* ]]; then printf 'retained-volume\n'; fi
elif [[ "$1" == inspect ]]; then
  if [[ "$MODE" == stopped ]]; then printf 'false\n'; else printf 'true\n'; fi
else exit 98
fi
''')
            docker.chmod(0o700)
            for mode in ['fresh', 'config-failed', 'ps-failed', 'volumes-failed', 'retained', 'stopped', 'empty-dump', 'dump-failed']:
                env = {**os.environ, 'PATH': str(tools) + ':' + os.environ['PATH'], 'MODE': mode}
                result = shell(f'bash {shlex.quote(str(entry))} --if-existing', env)
                self.assertEqual(result.returncode == 0, mode == 'fresh', result.stdout + result.stderr)
                self.assertFalse(list((target / 'backups/system').glob('*/MANIFEST')))
            # 验证真正继承的 FD 9 能被接受，且不会重复加锁。
            (target / 'runtime').mkdir(exist_ok=True)
            env = {**os.environ, 'PATH': str(tools) + ':' + os.environ['PATH'], 'MODE': 'fresh'}
            code = (f'exec 9>{shlex.quote(str(target / "runtime/.deploy.lock"))}; flock -n 9; '
                    f'bash {shlex.quote(str(entry))} --if-existing --deployment-lock-fd 9')
            # 该路径依赖 Linux /proc 校验继承描述符；其他平台跳过，由 Linux CI 覆盖。
            if Path('/proc/self/fd').is_dir():
                self.assertEqual(shell(code, env).returncode, 0)


if __name__ == '__main__':
    unittest.main()
