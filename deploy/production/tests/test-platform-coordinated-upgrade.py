#!/usr/bin/env python3
"""使用会拒绝旧版模式的 Agent 验证实际平台发布函数。"""
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / 'bin/deploy-service.sh').read_text()
PLATFORM = re.search(r'^deploy_platform\(\) \{\n.*?^\}', SCRIPT,
                     re.MULTILINE | re.DOTALL).group()

HARNESS = r'''
source "$HELPER"
deploy_dir="$TEST_ROOT"
mkdir -p "$deploy_dir/runtime" "$deploy_dir/files" "$deploy_dir/subsystems.d"
echo pending > "$deploy_dir/runtime/.control-plane-reload-required"
if [[ -n "${MARKER_IMAGE:-}" ]]; then
  echo "PLATFORM_UPGRADE_IMAGE=$MARKER_IMAGE" >> "$deploy_dir/runtime/.control-plane-reload-required"
fi
printf 'when_service: portal-api\nconditional_runtime_services: []\n' > "$deploy_dir/subsystems.d/portal.yaml"
image_ref="${PLATFORM_IMAGE}"
file_gateway_image_ref="$CANDIDATE_GATEWAY"
migrated=false
agent_started=false
api_started=false
log() { echo "$*" >> "$TEST_LOG"; }
env_value() {
  [[ "$1" != FILE_GATEWAY_HOST_ROOT ]] || echo "$deploy_dir/files"
}
stat() { echo 10001:10001; }
port_value() { echo 18080; }
backup_database() { log backup; [[ "$FAIL_PHASE" != backup ]]; }
# 新助手定义在 deploy_platform 之外，必须与 backup_database 一样在夹具中打桩，
# 否则抽取出来的函数体找不到它们（真实部署脚本里它们是同文件全局函数）。
require_mysql_ready() { :; }
diagnose_mysql_container() { :; }
backup_database_if_initialized() { backup_database "$@"; }
provision_iam_import_client() { :; }
initialize_platform_audit_hash_key() { log audit-key; [[ "$FAIL_PHASE" != audit-key ]]; }
ensure_temporal_application_network() { :; }
prepare_managed_public_proxy_image() { log proxy-prefetch; [[ "$FAIL_PHASE" != proxy ]]; }
wait_for_health() { log api-health; }
dump_subsystem_provisioner_debug() { :; }
dump_file_gateway_debug() { :; }
dump_platform_runtime_debug() { log runtime-diagnostics; }
compose() {
  case "$*" in
    'stop --timeout 60 platform-api') log stop-old-api ;;
    *'ps -q subsystem-provisioner'*) echo candidate-agent ;;
    *'ps -q platform-api'*) echo candidate-api ;;
    *'ps -q file-gateway'*) echo candidate-gateway ;;
    *'run --rm --no-deps platform-migrate ./migrate'*)
      log migrate
      [[ "$FAIL_PHASE" != migrate ]] || return 7
      migrated=true
      ;;
    *'up '*'subsystem-provisioner')
      log agent
      # The old binary really cannot parse the current schema. Do not remove
      # fields or waive readiness to make that old version start successfully.
      if [[ "$PLATFORM_IMAGE" == old-image ]] && grep -q when_service "$deploy_dir/subsystems.d/portal.yaml"; then
        echo 'legacy Agent: unknown fields when_service/conditional_runtime_services' >&2
        return 8
      fi
      [[ "$migrated" == true && "$FAIL_PHASE" != agent ]] || return 9
      agent_started=true
      ;;
    *'up '*'platform-api platform-worker')
      log api
      [[ "$migrated" == true && "$agent_started" == true && "$FAIL_PHASE" != api ]] || return 10
      api_started=true
      ;;
  esac
}
docker() { echo healthy; }
verify_subsystem_provisioner_config_consistency() { log agent-config; }
verify_service_image() {
  log "digest-$2"
  [[ "$FAIL_PHASE" != digest ]]
}
verify_service_stable() {
  log worker-stable
  [[ "$FAIL_PHASE" != stability ]]
}
verify_subsystem_control_plane_profiles() {
  log profiles
  [[ "$migrated" == true && "$agent_started" == true && "$api_started" == true && "$FAIL_PHASE" != profiles ]]
}
'''

with tempfile.TemporaryDirectory(prefix='platform-upgrade-') as directory:
    fixture = Path(directory)
    log = fixture / 'calls'
    marker = fixture / 'runtime/.control-plane-reload-required'
    image = 'example.cr.aliyuncs.com/test/platform@sha256:' + 'a' * 64
    gateway = 'example.cr.aliyuncs.com/test/platform@sha256:' + 'b' * 64
    env = dict(os.environ, HELPER=str(ROOT / 'bin/provisioner-config-refresh.sh'),
               TEST_ROOT=str(fixture), TEST_LOG=str(log), CANDIDATE_GATEWAY=gateway,
               PLATFORM_IMAGE=image, DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE='true', FAIL_PHASE='')
    script = HARNESS + '\n' + PLATFORM + '\nrequire_control_plane_reload_clearance platform-upgrade\ndeploy_platform\n'

    def run(extra=None, succeeds=True):
        log.write_text('')
        process = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c', script],
                                 env=dict(env, **(extra or {})), capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        assert marker.exists() != succeeds, 'marker may clear only after the entire candidate pair verifies'
        return log.read_text().splitlines(), process

    calls, _ = run()
    assert calls.index('migrate') < calls.index('agent') < calls.index('api') < calls.index('profiles')
    assert calls.index('migrate') < calls.index('stop-old-api') < calls.index('agent')
    assert calls.index('migrate') < calls.index('audit-key') < calls.index('stop-old-api')
    assert 'digest-subsystem-provisioner' in calls
    assert 'digest-platform-api' in calls and 'digest-platform-worker' in calls
    assert calls.index('worker-stable') < calls.index('profiles')
    calls, _ = run({'DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE': 'false'}, succeeds=False)
    assert calls == []
    calls, _ = run({'MARKER_IMAGE': gateway}, succeeds=False)
    assert calls == []
    run({'MARKER_IMAGE': image})
    calls, old = run({'PLATFORM_IMAGE': 'old-image'}, succeeds=False)
    assert 'legacy Agent: unknown fields' in old.stderr and 'api' not in calls
    for phase in ('backup', 'proxy', 'migrate', 'audit-key', 'agent', 'api', 'profiles', 'digest', 'stability'):
        calls, _ = run({'FAIL_PHASE': phase}, succeeds=False)
        if phase in ('migrate', 'agent'):
            assert 'api' not in calls
        if phase == 'migrate':
            assert 'agent' not in calls and 'stop-old-api' not in calls
        if phase == 'audit-key':
            assert 'agent' not in calls and 'stop-old-api' not in calls
        if phase == 'api':
            assert 'runtime-diagnostics' in calls, 'candidate logs must be captured before rollback'
        if phase == 'proxy':
            assert 'migrate' not in calls and 'stop-old-api' not in calls
        if phase == 'backup':
            assert 'migrate' not in calls and 'stop-old-api' not in calls
            assert 'agent' not in calls and 'api' not in calls
    # 这是旧二进制尝试失败后留下的同一持久化标记。
    run()

    # 使用真实事务安装器：只有显式升级不可变的平台镜像，才能在保留待处理控制面门禁的同时替换资产。
    source = fixture / 'assets'
    (source / 'bin').mkdir(parents=True)
    (source / 'docker-compose.yml').write_text('services: {}\n')
    (source / 'bin/deploy.sh').write_text('#!/bin/bash\ntrue\n')
    archive = fixture / 'assets.tar.gz'
    subprocess.run(['tar', '-czf', str(archive), '-C', str(source), '.'], check=True)
    digest = subprocess.check_output(['sha256sum', str(archive)], text=True).split()[0]
    Path(str(archive) + '.sha256').write_text(digest + '  assets.tar.gz\n')
    tools = fixture / 'tools'
    tools.mkdir()
    (tools / 'docker').write_text('#!/bin/sh\nexit 0\n')
    (tools / 'docker').chmod(0o755)
    if not shutil.which('flock'):
        (tools / 'flock').write_text('#!/bin/sh\nexit 0\n')
        (tools / 'flock').chmod(0o755)
    install_env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'])
    previous_marker = 'FORMAT=1\nASSETS_ARCHIVE_SHA256=' + 'c' * 64 + '\n'
    marker.write_text(previous_marker)
    (fixture / '.env').write_text('COMPOSE_PROJECT_NAME=fixture\n')

    def install(extra=None, succeeds=True):
        process = subprocess.run(['bash', str(ROOT / 'bin/install-assets.sh'), str(archive), str(fixture)],
                                 env=dict(install_env, **(extra or {})), capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        assert marker.is_file()
        return process

    install(succeeds=False)
    install({'ASSETS_PLATFORM_UPGRADE_IMAGE': 'platform:latest'}, succeeds=False)
    assert marker.read_text() == previous_marker
    install({'ASSETS_PLATFORM_UPGRADE_IMAGE': image})
    assert 'ASSETS_ARCHIVE_SHA256=' + digest in marker.read_text()
    assert 'PLATFORM_UPGRADE_IMAGE=' + image in marker.read_text()
    install(succeeds=False)

    # 受控安装中断后，资产事务恢复必须还原原有门禁，不能清除旧的待处理要求。
    transaction = fixture / 'runtime/.assets-install-transaction'
    transaction.mkdir()
    backup = fixture / 'backups/install-assets.previous'
    backup.mkdir()
    (transaction / 'backup').write_text(str(backup) + '\n')
    (transaction / 'touched').write_text('')
    (transaction / 'existing').write_text('')
    (transaction / 'archive-sha256').write_text(digest + '\n')
    (transaction / 'previous-reload-marker').write_text(previous_marker)
    process = subprocess.run(['bash', str(ROOT / 'bin/install-assets.sh'), '--recover', str(fixture)],
                             env=install_env, capture_output=True, text=True)
    assert process.returncode == 0, process.stdout + process.stderr
    assert marker.read_text() == previous_marker

    marker.write_text('PLATFORM_UPGRADE_IMAGE=' + image + '\n')
    normal_reload = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c',
                                    'source "$HELPER"; deploy_dir="$TEST_ROOT"; refresh_subsystem_control_plane_config'],
                                   env=env, capture_output=True, text=True)
    assert normal_reload.returncode != 0
    assert '普通重载不能替代' in normal_reload.stderr
    assert marker.is_file()

# 执行真实 CLI 校验和发布事务，仅模拟 Docker 拉取。代理键必须与平台镜像对一并提交和恢复。
CLI_PREFIX = SCRIPT.split('script_dir="$(cd --', 1)[0]
TX_START = SCRIPT.index('require_backup_space || exit 1\n')
TX_END = SCRIPT.index('if [[ "$service" == "frontend" ]]; then\n  compose_config_ok=', TX_START)
TRANSACTION = SCRIPT[TX_START:TX_END]
TRANSACTION_HARNESS = r'''
deploy_dir="$TEST_ROOT"
release_file="$deploy_dir/.release.env"
mkdir -p "$deploy_dir/backups/releases"
require_backup_space() { :; }
restore_legacy_file_gateway_pointer() { :; }
id() { echo 1000; }
docker() {
  [[ "$1" == pull ]] || return 91
  printf '%s\n' "$2" >> "$TEST_LOG"
  grep -Fxq "PLATFORM_IMAGE=$CANDIDATE_PLATFORM" "$release_file" || return 92
  grep -Fxq "FILE_GATEWAY_IMAGE=$CANDIDATE_GATEWAY" "$release_file" || return 93
  grep -Fxq "DOCKER_SOCKET_PROXY_IMAGE=$EXPECTED_PROXY" "$release_file" || return 94
  [[ "$2" != "${FAIL_PULL:-}" ]]
}
'''

with tempfile.TemporaryDirectory(prefix='platform-release-transaction-') as directory:
    base = Path(directory)
    platform = '127.0.0.1:5000/uip/platform@sha256:' + 'a' * 64
    gateway = '127.0.0.1:5000/uip/gateway@sha256:' + 'b' * 64
    proxy = '127.0.0.1:5000/uip/proxy@sha256:' + 'c' * 64
    old_proxy = '127.0.0.1:5000/uip/proxy@sha256:' + 'd' * 64
    old_release = 'PLATFORM_IMAGE=old-platform\nFILE_GATEWAY_IMAGE=old-gateway\nDOCKER_SOCKET_PROXY_IMAGE=' + old_proxy + '\n'

    def transaction(name, arguments, expected_proxy, fail_pull='', success=True):
        fixture = base / name
        fixture.mkdir()
        release = fixture / '.release.env'
        release.write_text(old_release)
        log = fixture / 'pulls'
        env = dict(os.environ, TEST_ROOT=str(fixture), TEST_LOG=str(log),
                   CANDIDATE_PLATFORM=platform, CANDIDATE_GATEWAY=gateway,
                   EXPECTED_PROXY=expected_proxy, FAIL_PULL=fail_pull)
        process = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c',
                                  CLI_PREFIX + '\n' + TRANSACTION_HARNESS + '\n' + TRANSACTION,
                                  'fixture-release', *arguments], env=env, capture_output=True, text=True)
        assert (process.returncode == 0) == success, process.stdout + process.stderr
        if success:
            assert 'PLATFORM_IMAGE=' + platform + '\n' in release.read_text()
            assert 'FILE_GATEWAY_IMAGE=' + gateway + '\n' in release.read_text()
            assert 'DOCKER_SOCKET_PROXY_IMAGE=' + expected_proxy + '\n' in release.read_text()
            assert list((fixture / 'backups/releases').glob('*.env'))[0].read_text() == old_release
        else:
            assert release.read_text() == old_release, 'failed pull must restore every release pointer'
        return log.read_text().splitlines() if log.exists() else []

    assert transaction('with-proxy', ['platform', platform, gateway, proxy], proxy) == [platform, gateway, proxy]
    assert transaction('legacy-pair', ['platform', platform, gateway], old_proxy) == [platform, gateway]
    for phase, reference in [('platform', platform), ('gateway', gateway), ('proxy', proxy)]:
        transaction('failed-' + phase, ['platform', platform, gateway, proxy], proxy, reference, success=False)
    assert transaction('invalid-proxy', ['platform', platform, gateway, 'proxy:latest'], old_proxy, success=False) == []

print('Coordinated platform migration, durable marker and atomic platform/gateway/proxy release: PASS')
