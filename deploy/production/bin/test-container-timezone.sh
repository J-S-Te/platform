#!/usr/bin/env bash
# 离线回归测试，仅使用夹具和模拟 Docker 可执行文件。
set -Eeuo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-timezone.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
export TZ_TEST_ROOT="$test_root"
mkdir -p "$test_root/tools" "$test_root/deployment/runtime" "$test_root/deployment/bin"
python3 - "$test_root" "$script_dir/set-container-timezone.sh" <<'PY'
import json, pathlib, shutil, sys
root = pathlib.Path(sys.argv[1])
deployment = root / 'deployment'
shutil.copyfile(sys.argv[2], deployment / 'bin/set-container-timezone.sh')
shutil.copyfile(pathlib.Path(sys.argv[2]).parent / 'public-transport.sh', deployment / 'bin/public-transport.sh')
config = {'name': 'isolated-timezone-test', 'services': {
    'platform-api': {'image': 'offline/platform@sha256:abc', 'depends_on': {'mysql': {'condition': 'service_healthy'}}, 'environment': {'DSN': 'user:placeholder@tcp(mysql:3306)/app?parseTime=True&loc=UTC', 'TZ': 'UTC'}},
    'frontend': {'image': 'offline/frontend@sha256:def', 'depends_on': {'platform-api': {'condition': 'service_healthy'}}, 'environment': {}},
    'mysql': {'image': 'mysql:8.4', 'environment': {'TZ': 'UTC'}},
}}
(deployment / 'docker-compose.yml').write_text(json.dumps(config))
(root / 'resolved.json').write_text(json.dumps(config))
(deployment / '.env').write_text('COMPOSE_PROJECT_NAME=isolated-timezone-test\nPUBLIC_PLATFORM_HOST=127.0.0.1\nPUBLIC_SSO_HOST=127.0.0.1\nPUBLIC_ACCESS_MODE=ip\nTZ=UTC\nDSN="user:placeholder@tcp(mysql:3306)/app?parseTime=True&loc=UTC"\n')
(deployment / '.release.env').write_text('PLATFORM_IMAGE=offline/platform@sha256:abc\n')
(deployment / 'runtime/platform.env').write_text('DSN=user:placeholder@tcp(mysql:3306)/app?parseTime=true&loc=UTC\n')
(deployment / 'bin/deploy.sh').write_text('#!/usr/bin/env bash\nset -Eeuo pipefail\nprintf "%s\\n" "$@" > "$TZ_TEST_ROOT/delegated"\nexit "${TZ_TEST_DEPLOY_EXIT:-0}"\n')
(root / 'tools/docker').write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ['TZ_TEST_ROOT'])
args = sys.argv[1:]
with (root / 'docker-calls.jsonl').open('a') as log:
    log.write(json.dumps(args) + '\\n')
if any(x in args for x in ['pull', 'build', 'down', 'restart', 'run']):
    print('Forbidden direct mutation/network operation', file=sys.stderr); sys.exit(97)
if 'up' in args:
    if os.environ.get('TZ_TEST_CONTROLLED') != '1': sys.exit(97)
    assert '--no-build' in args and '--no-deps' in args and '--wait' in args
    assert args[args.index('--pull') + 1] == 'never'
    overlay = pathlib.Path(args[max(i for i, a in enumerate(args) if a == '--file') + 1])
    svc = args[-1]
    if os.environ.get('TZ_TEST_UP_FAIL') == svc and not (root / 'failed-once').exists():
        (root / 'failed-once').touch(); sys.exit(1)
    state_file = root / 'container-state.json'
    states = json.loads(state_file.read_text()) if state_file.exists() else {}
    merged = {'environment': {}}
    for i, arg in enumerate(args):
        if arg != '--file': continue
        service = json.loads(pathlib.Path(args[i + 1]).read_text()).get('services', {}).get(svc, {})
        merged['environment'].update(service.get('environment', {}))
        if 'image' in service: merged['image'] = service['image']
    states[svc] = merged
    state_file.write_text(json.dumps(states))
elif 'config' in args:
    if os.environ.get('TZ_TEST_CONFIG_FAIL') == '1': sys.exit(1)
    config = json.loads((root / 'resolved.json').read_text())
    if '--services' in args: print('\\n'.join(config['services']))
    elif '--images' in args: print('\\n'.join(v['image'] for v in config['services'].values()))
    elif '--quiet' not in args and '-q' not in args: print(json.dumps(config))
elif 'image' in args and 'inspect' in args:
    if os.environ.get('TZ_TEST_IMAGE_FAIL') == '1': sys.exit(1)
    print('sha256:abc')
elif 'inspect' in args:
    svc = args[-1].removesuffix('-id')
    state_file = root / 'container-state.json'
    states = json.loads(state_file.read_text()) if state_file.exists() else {}
    env = states.get(svc, {}).get('environment', {'TZ': 'UTC'})
    image = 'sha256:drift' if os.environ.get('TZ_TEST_DRIFT') == svc else 'sha256:abc'
    health = 'unhealthy' if os.environ.get('TZ_TEST_DEPENDENCY_FAIL') == svc else 'healthy'
    print(json.dumps([{'Image': image, 'State': {'Running': True, 'Health': {'Status': health}}, 'Config': {'Env': [k + '=' + str(v) for k, v in env.items() if v is not None]}}]))
elif 'ps' in args:
    if '--services' in args: print('platform-api\\nfrontend\\nmysql')
    elif '--format' in args and args[args.index('--format') + 1] == 'json':
        print(json.dumps({'Service':'platform-api','Name':'isolated-api','ID':'api-id','State':'running','Health':'healthy'}))
    elif os.environ.get('TZ_TEST_STOPPED') != args[-1]: print(args[-1] + '-id')
elif 'exec' in args:
    if os.environ.get('TZ_TEST_ZONE_FAIL') == '1': sys.exit(1)
    if 'date' in args: print('+0800')
elif args and args[0] in ['version', 'info']:
    print('{}')
else:
    print('Unexpected fake Docker command: ' + repr(args), file=sys.stderr); sys.exit(98)
''')
(root / 'tools/docker').chmod(0o755)
# The lock API is isolated as well; the fixture never obtains a production lock.
(root / 'tools/flock').write_text('#!/usr/bin/env bash\nexit "${TZ_TEST_LOCK_EXIT:-0}"\n')
(root / 'tools/flock').chmod(0o755)
PY
export PATH="$test_root/tools:$PATH"
target="$test_root/deployment/bin/set-container-timezone.sh"
deployment="$test_root/deployment"
snapshot() {
    python3 - "$deployment" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
for name in ['docker-compose.yml', '.env', '.release.env', 'runtime/platform.env']:
    print(name, hashlib.sha256((root / name).read_bytes()).hexdigest())
PY
}
expect_fail() {
    if bash "$target" "$@" > "$test_root/result" 2>&1; then
        printf 'Expected rejection: %s\n' "$*" >&2; exit 1
    fi
}
before="$(snapshot)"
bash "$target" check --dir "$deployment" > "$test_root/result"
[[ "$(snapshot)" == "$before" ]]
[[ ! -e "$deployment/runtime/compose.timezone.yaml" ]]
bash "$target" --check-only --dir "$deployment" > "$test_root/result"
[[ "$(snapshot)" == "$before" ]]
expect_fail configure --dir "$deployment" --tz ../../etc/passwd
expect_fail configure --dir "$deployment" --tz Invalid/Timezone
expect_fail --shift-history --dir "$deployment"
expect_fail --recreate --dir "$deployment"
expect_fail configure --dir
expect_fail configure --unknown
expect_fail configure --dir "$deployment" --compose "$test_root/resolved.json"
expect_fail configure --dir "$deployment" --service does-not-exist --yes
expect_fail configure --dir "$deployment" --service mysql --yes
expect_fail configure --dir "$deployment" --extra-file "$deployment/missing.yaml" --yes
[[ "$(snapshot)" == "$before" ]]
TZ_TEST_CONFIG_FAIL=1 expect_fail configure --dir "$deployment"
[[ ! -e "$deployment/runtime/compose.timezone.yaml" ]]
TZ_TEST_IMAGE_FAIL=1 expect_fail configure --dir "$deployment" --yes
TZ_TEST_ZONE_FAIL=1 expect_fail configure --dir "$deployment" --yes
TZ_TEST_LOCK_EXIT=1 expect_fail configure --dir "$deployment" --yes
[[ ! -e "$deployment/runtime/compose.timezone.yaml" ]]
ln -s "$test_root/resolved.json" "$deployment/runtime/compose.timezone.yaml"
expect_fail configure --dir "$deployment" --yes
rm "$deployment/runtime/compose.timezone.yaml"
TZ_TEST_IMAGE_FAIL=1 expect_fail check --dir "$deployment"
TZ_TEST_ZONE_FAIL=1 expect_fail check --dir "$deployment"
bash "$target" configure --dir "$deployment" --tz Asia/Shanghai --yes > "$test_root/result"
[[ "$(snapshot)" == "$before" ]]
python3 - "$deployment/runtime/compose.timezone.yaml" <<'PY'
import json, pathlib, stat, sys
p = pathlib.Path(sys.argv[1]); services = json.loads(p.read_text())['services']
assert services['platform-api']['environment']['TZ'] == 'Asia/Shanghai'
assert services['frontend']['environment']['TZ'] == 'Asia/Shanghai'
assert 'mysql' not in services, 'Database timezone must not change'
assert not (stat.S_IMODE(p.stat().st_mode) & 0o077), 'Override permissions must protect configuration'
assert all(set(s['environment']) == {'TZ'} for s in services.values()), 'DSN must not be overridden'
PY
first_override="$(cksum "$deployment/runtime/compose.timezone.yaml")"
first_backup_count="$(find "$deployment/runtime" -type d -name 'timezone-backup.*' | wc -l | tr -d ' ')"
bash "$target" configure --dir "$deployment" --tz Asia/Shanghai --yes > "$test_root/result"
[[ "$(cksum "$deployment/runtime/compose.timezone.yaml")" == "$first_override" ]]
[[ "$(find "$deployment/runtime" -type d -name 'timezone-backup.*' | wc -l | tr -d ' ')" == "$first_backup_count" ]]
[[ "$(snapshot)" == "$before" ]]
bash "$target" configure --dir "$deployment" --tz UTC --yes > "$test_root/result"
python3 - "$deployment/runtime" <<'PY'
import pathlib, stat, sys
backups = list(pathlib.Path(sys.argv[1]).glob('timezone-backup.*'))
assert len(backups) == 2, 'Each actual change must have its own backup'
assert any((b / 'compose.timezone.yaml').exists() for b in backups)
for b in backups:
    assert not (stat.S_IMODE(b.stat().st_mode) & 0o077)
    for f in b.iterdir(): assert not (stat.S_IMODE(f.stat().st_mode) & 0o077)
PY
bash "$target" apply --dir "$deployment" > "$test_root/result"
grep -qx 'timezone-apply' "$test_root/delegated"
TZ_TEST_DEPLOY_EXIT=42 expect_fail apply --dir "$deployment"
# 验证受保护部署入口背后的实现，仍只使用模拟容器。更新失败必须还原记录的原时区。
expect_fail internal-apply --dir "$deployment"
UIP_TIMEZONE_CONTROLLED_APPLY=1 TZ_TEST_CONTROLLED=1 TZ_TEST_STOPPED=platform-api expect_fail internal-apply --dir "$deployment"
UIP_TIMEZONE_CONTROLLED_APPLY=1 TZ_TEST_CONTROLLED=1 TZ_TEST_DRIFT=platform-api expect_fail internal-apply --dir "$deployment"
UIP_TIMEZONE_CONTROLLED_APPLY=1 TZ_TEST_CONTROLLED=1 TZ_TEST_DEPENDENCY_FAIL=mysql expect_fail internal-apply --dir "$deployment"
[[ ! -e "$test_root/container-state.json" ]]
bash "$target" configure --dir "$deployment" --tz Asia/Shanghai --yes > "$test_root/result"
UIP_TIMEZONE_CONTROLLED_APPLY=1 TZ_TEST_CONTROLLED=1 TZ_TEST_UP_FAIL=frontend expect_fail internal-apply --dir "$deployment"
python3 - "$test_root/container-state.json" <<'PY'
import json, pathlib, sys
states = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert states['platform-api']['environment']['TZ'] == 'UTC'
assert states['frontend']['environment']['TZ'] == 'UTC'
PY
UIP_TIMEZONE_CONTROLLED_APPLY=1 TZ_TEST_CONTROLLED=1 bash "$target" internal-apply --dir "$deployment" > "$test_root/result"
python3 - "$test_root/container-state.json" <<'PY'
import json, pathlib, sys
states = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert states['platform-api']['environment']['TZ'] == 'Asia/Shanghai'
assert states['frontend']['environment']['TZ'] == 'Asia/Shanghai'
assert 'mysql' not in states
PY
python3 - "$test_root/docker-calls.jsonl" "$deployment" <<'PY'
import json, pathlib, sys
calls = [json.loads(line) for line in pathlib.Path(sys.argv[1]).read_text().splitlines()]
assert any(str(pathlib.Path(sys.argv[2]).resolve() / 'docker-compose.yml') in c for c in calls), '--dir must determine compose path'
assert not any(any(x in c for x in ['pull', 'build', 'down', 'restart', 'run']) for c in calls)
ups = [c for c in calls if 'up' in c]
assert ups and all('--no-build' in c and '--no-deps' in c and c[c.index('--pull') + 1] == 'never' for c in ups)
assert [c[-1] for c in ups[-2:]] == ['platform-api', 'frontend'], 'Dependencies must be applied first'
PY
printf 'Offline timezone checks, preservation, idempotency and controlled apply tests passed\n'
