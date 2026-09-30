#!/usr/bin/env python3
"""Execute the actual CI asset transport and reload steps against an isolated host."""
import os
from pathlib import Path
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
             'OFFLINE_RUNBOOK.md', 'DEPLOYMENT_COMPATIBILITY.md', 'BACKUP_RECOVERY.md')
    for name in directories:
        (source / name).mkdir()
    for name in files:
        shutil.copyfile(production / name, source / name)
    shutil.copyfile(production / 'bin/install-assets.sh', source / 'bin/install-assets.sh')
    shutil.copyfile(production / 'bin/provisioner-config-refresh.sh', source / 'bin/provisioner-config-refresh.sh')
    # Transport must exclude old overlays and acceptance evidence rejected by the installer.
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
    selection = '# operator disabled a subsystem\nservices: {}\n'
    (host / 'docker-compose.yml').write_text(selection)
    (host / '.env').write_text('COMPOSE_PROJECT_NAME=fixture\n')
    runtime = host / 'runtime'
    runtime.mkdir()
    (runtime / 'customer.env').write_text('PRIVATE_FIXTURE=keep\n')
    env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
               HOME=str(fixture / 'home'), DEPLOY_HOST='fixture', DEPLOY_USER='fixture',
               DEPLOY_SSH_KEY='fixture-key', DEPLOY_KNOWN_HOSTS='fixture-known-host',
               DEPLOY_PATH=str(host), TEST_RELOAD_LOG=str(fixture / 'reload.log'),
               FILE_GATEWAY_IMAGE_REF='registry.example/platform@sha256:' + 'a' * 64)

    def execute(name, extra=None, succeeds=True):
        process = subprocess.run(['bash', '-c', run_step(name)], cwd=source.parents[1],
                                 env=dict(env, **(extra or {})), capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        return process

    sync = 'Sync production deployment assets'
    reload = 'Reload paired subsystem control plane'
    marker = runtime / '.control-plane-reload-required'
    execute(sync)
    assert (host / 'docker-compose.yml').read_text() == selection
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
    # A retry first reloads the previously installed assets and consumes the old
    # gate, then installs this archive and creates its new reload requirement.
    execute(sync)
    assert marker.is_file()
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

print('CI asset transport, transaction install, module selection and reload gates: PASS')
