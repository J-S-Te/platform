#!/usr/bin/env python3
"""在隔离主机上执行实际 CI 资产传输与控制面重载步骤。"""
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / '.github/workflows/ci-cd.yml'


def run_step(name):
    lines = WORKFLOW.read_text().splitlines()
    start = lines.index('      - name: ' + name)
    finish = next((i for i in range(start + 1, len(lines))
                   if lines[i].startswith('      - name: ')), len(lines))
    run = lines.index('        run: |', start, finish) + 1
    return '\n'.join(line[10:] for line in lines[run:finish]) + '\n'


def executable(path, content):
    path.write_text(content)
    path.chmod(0o755)


with tempfile.TemporaryDirectory(prefix='ci-assets-sync-') as directory:
    fixture = Path(directory)
    source = fixture / 'runner/deploy/production'
    source.mkdir(parents=True)
    production = ROOT / 'deploy/production'
    directories = ('bin', 'subsystems.d', 'subsystem-templates', 'mysql-init',
                   'nginx', 'monitoring', 'tests')
    files = ('docker-compose.yml', '.env.example', '.release.env.example', '.gitignore',
             'ACCEPTANCE_CHECKLIST.md', 'README.md', 'OFFLINE_DEPLOYMENT.md',
             'OFFLINE_RUNBOOK.md', 'DEPLOYMENT_COMPATIBILITY.md', 'deployment-compatibility.json', 'DELIVERY_WORKFLOW.md', 'BACKUP_RECOVERY.md')
    for name in directories:
        (source / name).mkdir()
    for name in files:
        shutil.copyfile(production / name, source / name)
    shutil.copyfile(production / 'bin/install-assets.sh', source / 'bin/install-assets.sh')
    shutil.copyfile(production / 'bin/provisioner-config-refresh.sh', source / 'bin/provisioner-config-refresh.sh')
    shutil.copyfile(production / 'bin/frontend-network.py', source / 'bin/frontend-network.py')
    shutil.copyfile(production / 'bin/platform-mysql-command.py',
                    source / 'bin/platform-mysql-command.py')
    # 交付传输必须排除安装器拒绝的旧覆盖配置和验收证据。
    (source / 'compose.yaml').write_text('legacy overlay must not ship\n')
    (source / 'acceptance').mkdir()
    (source / 'subsystems.d/new.yaml').write_text('new manifest\n')
    executable(source / 'bin/deploy.sh', '''#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == reload-control-plane ]]
[[ "${FILE_GATEWAY_IMAGE:-}" =~ @sha256:[a-f0-9]{64}$ ]]
[[ -f runtime/.control-plane-reload-required ]]
[[ "${FAIL_RELOAD:-false}" != true ]] || exit 17
rm runtime/.control-plane-reload-required
echo paired-reload >> "$TEST_RELOAD_LOG"
''')
    tools = fixture / 'tools'
    tools.mkdir()
    executable(tools / 'docker', '''#!/bin/sh
case "$1" in
  info) exit 0 ;;
  network) [ "$2" = ls ] && exit 0 ;;
  ps) echo running-control-plane; exit 0 ;;
esac
exit 1
''')
    if not shutil.which('flock'):
        executable(tools / 'flock', '#!/bin/sh\nexit 0\n')
    executable(tools / 'ssh', '''#!/usr/bin/env python3
import io, os, subprocess, sys, tarfile
data = sys.stdin.buffer.read()
if os.environ.get('TAMPER_ARCHIVE') == 'true':
    incoming = tarfile.open(fileobj=io.BytesIO(data), mode='r:gz')
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz') as outgoing:
        for member in incoming:
            content = incoming.extractfile(member).read()
            if member.name == 'deployment-assets.tar.gz.sha256':
                content = b'0' * 64 + b'  deployment-assets.tar.gz\\n'
                member.size = len(content)
                member.pax_headers = {}
            outgoing.addfile(member, io.BytesIO(content))
    data = buffer.getvalue()
sys.exit(subprocess.run(sys.argv[-1], shell=True, executable='/bin/bash', input=data).returncode)
''')
    host = fixture / 'host with spaces'
    (host / 'subsystems.d').mkdir(parents=True)
    (host / 'subsystems.d/stale.yaml').write_text('stale manifest\n')
    selection = '''# operator disabled a subsystem
services:
  platform-mysql:
    image: mysql:8.4
    command: ["--character-set-server=utf8mb4", "--max-connections=120"]
    volumes: [platform-mysql-data:/var/lib/mysql]
  contract-mysql:
    image: mysql:8.4
    command: ["--max-connections=80"]
#  settlement-mysql:
#    image: mysql:8.4
#    command: ["--max-connections=60"]
'''
    expected_selection = selection.replace(
        '"--max-connections=120"]',
        '"--max-connections=120", "--log-bin-trust-function-creators=1"]',
    )
    (host / 'docker-compose.yml').write_text(selection)
    (host / '.env').write_text('COMPOSE_PROJECT_NAME=fixture\n')
    runtime = host / 'runtime'
    runtime.mkdir()
    (runtime / 'customer.env').write_text('PRIVATE_FIXTURE=keep\n')
    env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
               HOME=str(fixture / 'home'), DEPLOY_HOST='fixture', DEPLOY_USER='fixture',
               DEPLOY_SSH_KEY='fixture-key', DEPLOY_KNOWN_HOSTS='fixture-known-host',
               DEPLOY_PATH=str(host), TEST_RELOAD_LOG=str(fixture / 'reload.log'),
               FILE_GATEWAY_IMAGE_REF='registry.example/platform@sha256:' + 'a' * 64,
               IMAGE_REF='registry.example/platform@sha256:' + 'b' * 64,
               BACKEND_CHANGED='false')

    def execute(name, extra=None, succeeds=True):
        process = subprocess.run(['bash', '-c', run_step(name)], cwd=source.parents[1],
                                 env=dict(env, **(extra or {})), capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        return process

    sync = 'Sync production deployment assets'
    reload = 'Reload paired subsystem control plane'
    marker = runtime / '.control-plane-reload-required'
    execute(sync)
    assert (host / 'docker-compose.yml').read_text() == expected_selection, (
        'existing platform MySQL must receive trigger migration configuration '
        'without changing module selection or custom arguments'
    )
    assert (host / 'docker-compose.yml.dist').read_bytes() == (source / 'docker-compose.yml').read_bytes()
    assert not (host / 'subsystems.d/stale.yaml').exists()
    assert (host / 'subsystems.d/new.yaml').exists()
    assert (runtime / 'customer.env').read_text() == 'PRIVATE_FIXTURE=keep\n'
    assert ('SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=' + str(host) + '\n') in (host / '.env').read_text()
    assert marker.is_file()
    execute(reload)
    assert not marker.exists()
    assert (fixture / 'reload.log').read_text() == 'paired-reload\n'
    execute(sync)
    execute(reload, {'FAIL_RELOAD': 'true'}, succeeds=False)
    assert marker.is_file(), 'failed reload must retain deployment gate'
    execute(sync, {'FAIL_RELOAD': 'true'}, succeeds=False)
    assert marker.is_file()
    # 重试先重新加载已安装资产并消除旧门禁，再安装本归档并创建新的重载要求。
    execute(sync)
    assert marker.is_file()
    execute(reload)
    # 旧二进制无法解析新清单 schema。实际发布平台镜像时必须保留待处理标记，不能尝试使用旧版本重载。
    execute(sync)
    execute(reload, {'FAIL_RELOAD': 'true'}, succeeds=False)
    before_upgrade = (fixture / 'reload.log').read_text()
    execute(sync, {'BACKEND_CHANGED': 'true', 'FAIL_RELOAD': 'true'})
    assert marker.is_file()
    assert (fixture / 'reload.log').read_text() == before_upgrade
    # 待处理要求尚在时，普通安装仍须拒绝。
    execute(sync, {'FAIL_RELOAD': 'true'}, succeeds=False)
    execute(reload)
    failure = execute(sync, {'TAMPER_ARCHIVE': 'true'}, succeeds=False)
    assert 'SHA256' in failure.stderr, failure.stdout + failure.stderr
    assert not marker.exists()
    configured_env = (host / '.env').read_text()
    (host / '.env').write_text(configured_env.replace(str(host), '/wrong/deployment'))
    execute(sync, succeeds=False)
    assert not marker.exists()
    (host / '.env').write_text(configured_env + 'SUBSYSTEM_PRODUCTION_PROFILES_DIR=/wrong/profiles\n')
    execute(sync, succeeds=False)
    assert not marker.exists()

# 执行真实工作流中的检测脚本，确保服务编排变化不能只重载 API 而遗漏 Worker。
with tempfile.TemporaryDirectory(prefix='ci-platform-detect-') as directory:
    detection_root = Path(directory)
    detection_tools = detection_root / 'tools'
    detection_tools.mkdir()
    executable(detection_tools / 'git', r'''#!/bin/sh
cat "$TEST_CHANGED_PATHS_FILE"
''')
    detection_cases = [
        ('deploy/production/docker-compose.yml', 'true'),
        ('deploy/production/bin/platform-mysql-command.py', 'true'),
        ('internal/bootstrap/worker.go', 'true'),
        ('internal/bootstrap/worker_test.go', 'false'),
        ('deploy/production/subsystems.d/customer_portal-prod.yaml', 'false'),
        ('docs/runtime.md', 'false'),
        ('\n'.join('internal/module/file%d.go' % i for i in range(5000)), 'true'),
    ]
    for changed_paths, expected_change in detection_cases:
        output = detection_root / 'outputs'
        output.write_text('')
        changed_file = detection_root / 'changed-paths'
        changed_file.write_text(changed_paths)
        detection_env = dict(
            os.environ,
            PATH=str(detection_tools) + os.pathsep + os.environ['PATH'],
            TEST_CHANGED_PATHS_FILE=str(changed_file),
            GITHUB_OUTPUT=str(output),
        )
        process = subprocess.run(
            ['bash', '-e', '-o', 'pipefail', '-c', run_step('Detect backend image changes')],
            env=detection_env, capture_output=True, text=True,
        )
        assert process.returncode == 0, process.stderr
        assert output.read_text().strip() == 'backend_changed=' + expected_change, (
            changed_paths[:100], output.read_text(), expected_change,
        )

# 对实际文本更新器验证缺参、重复运行、裁剪及拒绝冲突，避免失败后留下部分改写。
update_compose = runpy.run_path(str(production / 'bin/platform-mysql-command.py'))['update_compose']
assert update_compose(expected_selection) == expected_selection
without_command = 'services:\n  platform-mysql:\n    image: mysql:8.4\n    volumes: [data:/var/lib/mysql]\n'
assert update_compose(without_command) == without_command.replace(
    '    image: mysql:8.4\n',
    '    image: mysql:8.4\n    command: ["--log-bin-trust-function-creators=1"]\n',
)
assert update_compose('#  platform-mysql:\nservices: {}\n') == '#  platform-mysql:\nservices: {}\n'
for invalid in (
    selection.replace('"--max-connections=120"', '"--log-bin-trust-function-creators=0"'),
    selection.replace('["--character-set-server=utf8mb4", "--max-connections=120"]', 'mysqld --custom'),
    selection.replace('    image: mysql:8.4', '    image: mysql:8.0', 1),
    selection + '  platform-mysql:\n    image: mysql:8.4\n',
):
    try:
        update_compose(invalid)
    except ValueError:
        pass
    else:
        raise AssertionError('conflicting or unsupported database configuration must fail closed')

print('CI asset transport, transaction install, module selection and reload gates: PASS')
