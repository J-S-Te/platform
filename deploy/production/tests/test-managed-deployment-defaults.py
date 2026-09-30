#!/usr/bin/env python3
"""Non-default deployment roots and narrowly managed proxy image compatibility."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PROXY = 'ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459'
LEGACY = 'tecnativa/docker-socket-proxy:v0.5.0'

with tempfile.TemporaryDirectory(prefix='managed-deployment-defaults-') as directory:
    fixture = Path(directory)
    tools = fixture / 'tools'
    tools.mkdir()
    (tools / 'docker').write_text('#!/bin/sh\nexit 0\n')
    (tools / 'docker').chmod(0o755)
    if not shutil.which('flock'):
        (tools / 'flock').write_text('#!/bin/sh\nexit 0\n')
        (tools / 'flock').chmod(0o755)
    env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'])
    target = fixture / 'custom root'
    target.mkdir()
    runtime = target / '.env'
    helper = ROOT / 'bin/provisioner-config-refresh.sh'

    def prepare(content, succeeds=True):
        runtime.write_text(content)
        process = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c',
                                  'source "$1"; deploy_dir="$2"; prepare_ci_deploy_root',
                                  'fixture', str(helper), str(target)],
                                 env=env, capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        if not succeeds:
            assert runtime.read_text() == content
        return runtime.read_text()

    root_line = 'SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=' + str(target) + '\n'
    gateway_line = 'FILE_GATEWAY_HOST_ROOT=' + str(target / 'data/file-gateway') + '\n'
    network_lines = 'FRONTEND_IPV4_ADDRESS=172.31.255.250\nAPP_TRUSTED_PROXIES=127.0.0.1/32,::1/128,172.31.255.1/32,172.31.255.250/32\n'
    base = '# existing configuration\nPRIVATE_FIXTURE=unchanged\n'
    assert prepare(base) == base + root_line + gateway_line + network_lines
    assert prepare(base + root_line) == base + root_line + gateway_line + network_lines
    assert prepare(base + root_line + 'FILE_GATEWAY_HOST_ROOT=\n') == base + root_line + gateway_line + network_lines
    custom = 'FILE_GATEWAY_HOST_ROOT=/mnt/company-files\n'
    assert prepare(base + root_line + custom) == base + root_line + custom + network_lines
    assert prepare(base + custom) == base + custom + root_line + network_lines
    prepare(base + 'FILE_GATEWAY_HOST_ROOT=\nFILE_GATEWAY_HOST_ROOT=/mnt/custom\n', succeeds=False)

    source = fixture / 'assets'
    (source / 'bin').mkdir(parents=True)
    (source / 'docker-compose.yml').write_text('services: {}\n')
    (source / 'bin/deploy.sh').write_text('#!/bin/bash\ntrue\n')
    archive = fixture / 'assets.tar.gz'
    subprocess.run(['tar', '-czf', str(archive), '-C', str(source), '.'], check=True)
    digest = subprocess.check_output(['sha256sum', str(archive)], text=True).split()[0]
    Path(str(archive) + '.sha256').write_text(digest + '  assets.tar.gz\n')
    installer = ROOT / 'bin/install-assets.sh'

    def install(name, image, expected_image, commented=False):
        host = fixture / name
        host.mkdir()
        prefix = '# ' if commented else ''
        block = ('  docker-socket-proxy:\n'
                 '    image: ' + image + ' # same managed version\n'
                 '    environment:\n      POST: "1"\n      AUTH: "0"\n'
                 '    networks: [docker-control]\n')
        selected = ('services:\n' + ''.join(prefix + line for line in block.splitlines(True))
                    + '  unrelated-service:\n    image: ' + LEGACY + '\n'
                    + '#  contract-api:\n#    image: operator-disabled\n')
        (host / 'docker-compose.yml').write_text(selected)
        process = subprocess.run(['bash', str(installer), str(archive), str(host)],
                                 env=env, capture_output=True, text=True)
        assert process.returncode == 0, process.stdout + process.stderr
        expected = selected if commented else selected.replace('image: ' + image + ' #',
                                                               'image: ' + expected_image + ' #', 1)
        assert (host / 'docker-compose.yml').read_text() == expected
        assert (host / 'docker-compose.yml.dist').read_text() == 'services: {}\n'
        if expected != selected:
            backup = next((host / 'backups').glob('install-assets.*'))
            assert (backup / 'docker-compose.yml').read_text() == selected
            # Recovery of a failed transaction must also restore this targeted
            # change, not leave a partially patched operator-selected Compose.
            transaction = host / 'runtime/.assets-install-transaction'
            transaction.mkdir()
            (transaction / 'backup').write_text(str(backup) + '\n')
            (transaction / 'touched').write_text('docker-compose.yml\n')
            (transaction / 'existing').write_text('docker-compose.yml\n')
            recovered = subprocess.run(['bash', str(installer), '--recover', str(host)],
                                       env=env, capture_output=True, text=True)
            assert recovered.returncode == 0, recovered.stdout + recovered.stderr
            assert (host / 'docker-compose.yml').read_text() == selected

    install('managed', LEGACY, PROXY)
    install('quoted-managed', "'docker.io/" + LEGACY + "'", "'" + PROXY + "'")
    install('custom', 'registry.internal/socket-proxy@sha256:' + 'a' * 64,
            'registry.internal/socket-proxy@sha256:' + 'a' * 64)
    install('custom-version', 'tecnativa/docker-socket-proxy:v0.4.1',
            'tecnativa/docker-socket-proxy:v0.4.1')
    install('disabled', LEGACY, LEGACY, commented=True)
    install('already-pinned', PROXY, PROXY)

    for name, address, disabled in [('legacy-frontend', '172.31.255.250', False),
                                     ('custom-frontend', '172.18.0.42', False),
                                     ('disabled-frontend', '172.31.255.250', True)]:
        host = fixture / name
        host.mkdir()
        block = ('  frontend:\n    networks:\n      application:\n'
                 '        ipv4_address: ' + address + ' # stable endpoint\n')
        suffix = ('  docker-socket-proxy:\n    image: custom/image\n    networks:\n'
                  '      application:\n        ipv4_address: 172.31.255.250 # unrelated custom scalar\n')
        selected = 'services:\n' + ''.join(('# ' if disabled else '') + line for line in block.splitlines(True)) + suffix
        (host / 'docker-compose.yml').write_text(selected)
        process = subprocess.run(['bash', str(installer), str(archive), str(host)],
                                 env=env, capture_output=True, text=True)
        assert process.returncode == 0, process.stdout + process.stderr
        expected = selected if disabled or address != '172.31.255.250' else selected.replace(address, '${FRONTEND_IPV4_ADDRESS:-172.31.255.250}', 1)
        assert (host / 'docker-compose.yml').read_text() == expected
        if expected != selected:
            backup = next((host / 'backups').glob('install-assets.*'))
            assert (backup / 'docker-compose.yml').read_text() == selected
            transaction = host / 'runtime/.assets-install-transaction'
            transaction.mkdir()
            (transaction / 'backup').write_text(str(backup) + '\n')
            (transaction / 'touched').write_text('docker-compose.yml\n')
            (transaction / 'existing').write_text('docker-compose.yml\n')
            subprocess.run(['bash', str(installer), '--recover', str(host)], env=env, check=True, capture_output=True)
            assert (host / 'docker-compose.yml').read_text() == selected

print('Deployment-root defaults, custom file storage, targeted pinned proxy update and recovery: PASS')
