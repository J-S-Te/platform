#!/usr/bin/env python3
"""Exercise release preflight and conditional Temporal network repair in isolation."""
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / 'bin/deploy-service.sh').read_text()


def executable(path, content):
    path.write_text(content)
    path.chmod(0o755)


with tempfile.TemporaryDirectory(prefix='deploy-service-preflight-') as directory:
    fixture = Path(directory)
    target = fixture / 'deployment'
    (target / 'bin').mkdir(parents=True)
    (target / 'subsystems.d').mkdir()
    (target / 'runtime').mkdir()
    (target / 'subsystems.d/fixture.yaml').write_text('fixture\n')
    (target / 'docker-compose.yml').write_text('services: {}\n')
    (target / '.env').write_text('COMPOSE_PROJECT_NAME=fixture\n')
    # Existing deployments predate the separate File Gateway target and pointer.
    release = target / '.release.env'
    release.write_text('PLATFORM_IMAGE=old-pointer\n')
    original = release.read_bytes()
    shutil.copyfile(ROOT / 'bin/compose-scope.sh', target / 'bin/compose-scope.sh')
    (target / 'bin/public-transport.sh').write_text('public_transport_prepare() { :; }\n')
    (target / 'bin/provisioner-config-refresh.sh').write_text('''require_control_plane_reload_clearance() {
  [[ ! -f "$deploy_dir/runtime/.control-plane-reload-required" ]]
}
''')
    # Run the actual script through its preflight, stopping before network mutation.
    executable(target / 'bin/deploy-service.sh', SCRIPT.split('ensure_application_network() {')[0]
               + '\n[[ -z "${FILE_GATEWAY_IMAGE:-}" ]]\necho preflight-complete\n')
    tools = fixture / 'tools'
    tools.mkdir()
    executable(tools / 'docker', '''#!/usr/bin/env bash
set -eu
if [[ "$1" == ps ]]; then
  [[ -z "${RECOVERY_CONTAINER:-}" ]] || printf '%s\\n' "$RECOVERY_CONTAINER"
  exit 0
fi
if [[ "$1" == inspect ]]; then
  [[ "${INSPECT_FAIL:-false}" != true ]] || exit 19
  if [[ "$*" == *'.Config.Image'* ]]; then
    printf '%s\\n' "${RECOVERY_IMAGE:-}"
    exit 0
  fi
  [[ "${TEMPORAL_STATE:-missing}" != attached ]] || echo attached
  exit 0
fi
[[ "$1" == compose ]]
[[ "${2:-}" != version ]] || exit 0
[[ "${PLATFORM_IMAGE:-}" == "$EXPECTED_PLATFORM" ]]
[[ "${FILE_GATEWAY_IMAGE:-}" == "$EXPECTED_GATEWAY" ]]
echo platform-api
''')
    if not shutil.which('flock'):
        executable(tools / 'flock', '#!/bin/sh\nexit 0\n')
    image = 'example.cr.aliyuncs.com/test/platform@sha256:' + 'a' * 64
    gateway = 'example.cr.aliyuncs.com/test/platform@sha256:' + 'b' * 64
    env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
               EXPECTED_PLATFORM=image, EXPECTED_GATEWAY=gateway)
    env.pop('FILE_GATEWAY_IMAGE', None)

    def preflight(platform=image, file_gateway=gateway, succeeds=True):
        process = subprocess.run(['bash', str(target / 'bin/deploy-service.sh'), 'platform',
                                  platform, file_gateway], env=env, capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        assert release.read_bytes() == original, 'preflight must not commit release pointers'
        return process

    assert 'preflight-complete' in preflight().stdout
    preflight(file_gateway='example.cr.aliyuncs.com/test/platform:latest', succeeds=False)
    marker = target / 'runtime/.control-plane-reload-required'
    marker.touch()
    preflight(succeeds=False)
    marker.unlink()

    helper = re.search(r'^ensure_temporal_application_network\(\) \{\n.*?^\}',
                       SCRIPT, re.MULTILINE | re.DOTALL).group()
    harness = '''compose() {
  if [[ "$1" == ps ]]; then
    [[ "${TEMPORAL_STATE:-missing}" == missing ]] || echo temporal-id
    return 0
  fi
  echo "$*" >> "$TEST_COMPOSE_LOG"
  [[ "${UP_FAIL:-false}" != true ]]
}
''' + helper + '\nensure_temporal_application_network\n'
    log = fixture / 'compose.log'

    def repair(state, extra=None, succeeds=True):
        log.write_text('')
        process = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c', harness],
                                 env=dict(env, TEMPORAL_STATE=state, TEST_COMPOSE_LOG=str(log),
                                          **(extra or {})), capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        return log.read_text().splitlines()

    assert repair('missing') == []
    assert repair('attached') == [], 'healthy Temporal must not restart for every release'
    assert repair('detached') == [
        'up -d --wait --wait-timeout 240 contract-mysql',
        'up -d --force-recreate --no-deps --wait --wait-timeout 240 temporal']
    assert repair('detached', {'INSPECT_FAIL': 'true'}, succeeds=False) == []
    assert repair('detached', {'UP_FAIL': 'true'}, succeeds=False) == [
        'up -d --wait --wait-timeout 240 contract-mysql']

    recovery = '\n'.join(re.findall(r'^acr_enterprise_or_new_personal=.*$|^acr_legacy_personal=.*$|^offline_local_registry=.*$',
                                    SCRIPT, re.MULTILINE))
    for name in ('release_image_value', 'restore_legacy_file_gateway_pointer'):
        recovery += '\n' + re.search(r'^' + name + r'\(\) \{\n.*?^\}',
                                     SCRIPT, re.MULTILINE | re.DOTALL).group()
    recovery += '''
service=platform
release_file="$TEST_RELEASE"
deploy_dir="$(dirname "$release_file")"
scope_project() { echo fixture; }
restore_legacy_file_gateway_pointer
'''
    restored = fixture / 'restored.env'
    old_gateway = 'example.cr.aliyuncs.com/test/platform@sha256:' + 'c' * 64

    def recover(container='', image=old_gateway, existing='', succeeds=True):
        restored.write_text('PLATFORM_IMAGE=old-pointer\n' + existing)
        before = restored.read_bytes()
        process = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c', recovery],
                                 env=dict(env, TEST_RELEASE=str(restored),
                                          RECOVERY_CONTAINER=container, RECOVERY_IMAGE=image),
                                 capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        if not succeeds:
            assert restored.read_bytes() == before
        return restored.read_text()

    assert recover() == 'PLATFORM_IMAGE=old-pointer\n'
    assert recover('gateway-id') == 'PLATFORM_IMAGE=old-pointer\nFILE_GATEWAY_IMAGE=' + old_gateway + '\n'
    recover('gateway-id', image='example.cr.aliyuncs.com/test/platform:latest', succeeds=False)
    recover('gateway-id\nsecond-id', succeeds=False)
    existing = 'FILE_GATEWAY_IMAGE=' + gateway + '\n'
    assert recover('gateway-id', existing=existing).endswith(existing)

print('Candidate scope, release gate, immutable rollback recovery and Temporal network recovery: PASS')
