#!/usr/bin/env python3
"""Run anonymous public-image preparation without changing caller credentials."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PROXY = 'ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459'
HARNESS = r'''
source "$HELPER"
deploy_dir="$TEST_ROOT"
production_profiles_host_manifest() { :; }
verify_subsystem_provisioner_config_consistency() { :; }
verify_subsystem_control_plane_profiles() { :; }
compose() {
  if [[ "$*" == 'config --images docker-socket-proxy' ]]; then echo "$PROXY_IMAGE"; return 0; fi
  if [[ "$*" == 'ps -q subsystem-provisioner' ]]; then echo new-agent; return 0; fi
  if [[ "$*" == 'ps -q platform-api' ]]; then echo new-api; return 0; fi
  echo "$*" >> "$COMPOSE_LOG"
}
refresh_subsystem_control_plane_config
[[ "$DOCKER_CONFIG" == "$ORIGINAL_CONFIG" ]]
'''

with tempfile.TemporaryDirectory(prefix='public-proxy-prefetch-') as directory:
    fixture = Path(directory)
    tools = fixture / 'tools'
    tools.mkdir()
    credentials = fixture / 'operator-docker'
    credentials.mkdir()
    credentials_file = credentials / 'config.json'
    credentials_file.write_text('{"auths":{"ghcr.io":{"auth":"fixture-stale"},"acr.example":{"auth":"fixture-acr"}},"currentContext":"fixture-context"}\n')
    original = credentials_file.read_bytes()
    executable = tools / 'docker'
    executable.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv[1:]
with open(os.environ['DOCKER_LOG'], 'a') as log:
    log.write(json.dumps(args)+'\\n')
cache=pathlib.Path(os.environ['CACHE_IMAGE'])
if args[:2]==['image','inspect']:
    sys.exit(0 if cache.exists() else 1)
if args[:2]==['context','inspect']:
    print(os.environ.get('TEST_ENDPOINT','unix:///fixture-original-daemon.sock')); sys.exit(0)
if args and args[0]=='--config':
    assert args[2:5]==['--host','unix:///fixture-original-daemon.sock','pull'], args
    assert args[-1]==os.environ['EXPECTED_PROXY']
    config=pathlib.Path(args[1])
    assert config.stat().st_mode & 0o777 == 0o700
    assert (config/'config.json').stat().st_mode & 0o777 == 0o600
    assert json.loads((config/'config.json').read_text())=={'auths':{}}
    assert not os.environ.get('DOCKER_CONTEXT')
    pathlib.Path(os.environ['ANON_CONFIG']).write_text(str(config))
    if os.environ.get('PULL_FAIL')=='true': sys.exit(23)
    cache.touch(); sys.exit(0)
if args and args[0]=='ps':
    print('old-api' if 'com.docker.compose.service=platform-api' in ' '.join(args) else 'old-agent'); sys.exit(0)
if args and args[0]=='inspect': print('healthy'); sys.exit(0)
if args and args[0]=='exec': sys.exit(0)
sys.exit(1)
''')
    executable.chmod(0o755)
    compose_log = fixture / 'compose.log'
    docker_log = fixture / 'docker.log'
    cache = fixture / 'cached-image'
    anonymous_config = fixture / 'anon-config'
    env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
               HELPER=str(ROOT / 'bin/provisioner-config-refresh.sh'), TEST_ROOT=str(fixture),
               ORIGINAL_CONFIG=str(credentials), DOCKER_CONFIG=str(credentials),
               COMPOSE_LOG=str(compose_log), DOCKER_LOG=str(docker_log), CACHE_IMAGE=str(cache),
               ANON_CONFIG=str(anonymous_config), EXPECTED_PROXY=PROXY, PROXY_IMAGE=PROXY)
    # An explicit context takes precedence over DOCKER_HOST. The isolated config
    # must not accidentally pull to this competing daemon endpoint.
    env['DOCKER_HOST'] = 'unix:///wrong-daemon.sock'
    env['DOCKER_CONTEXT'] = 'fixture-context'

    def run(extra=None, succeeds=True):
        compose_log.write_text('')
        docker_log.write_text('')
        process = subprocess.run(['bash', '-Eeuo', 'pipefail', '-c', HARNESS],
                                 env=dict(env, **(extra or {})), capture_output=True, text=True)
        assert (process.returncode == 0) == succeeds, process.stdout + process.stderr
        assert credentials_file.read_bytes() == original
        if anonymous_config.exists():
            assert not Path(anonymous_config.read_text()).exists(), 'temporary credentials config must be removed'
        return compose_log.read_text(), [json.loads(line) for line in docker_log.read_text().splitlines()]

    calls, commands = run({'PULL_FAIL': 'true'}, succeeds=False)
    assert 'stop ' not in calls, 'public pull failure must precede stopping the live API'
    assert any(args[:1] == ['--config'] for args in commands)
    calls, commands = run()
    assert 'stop --timeout 60 platform-api' in calls
    assert any(args[:1] == ['--config'] for args in commands)
    calls, commands = run()
    assert not any(args[:1] == ['--config'] for args in commands), 'cached fixed digest must work offline'
    cache.unlink()
    calls, commands = run({'TEST_ENDPOINT': 'tcp://remote-daemon:2376'}, succeeds=False)
    assert 'stop ' not in calls
    assert not any(args[:1] == ['--config'] for args in commands)
    calls, commands = run({'PROXY_IMAGE': 'private.example/custom/socket-proxy:approved'})
    assert 'stop --timeout 60 platform-api' in calls
    assert not any(args[:1] == ['--config'] for args in commands), 'custom image authentication stays unchanged'

print('Anonymous pinned proxy pull, original daemon/credentials, cache and pre-stop failure gate: PASS')
