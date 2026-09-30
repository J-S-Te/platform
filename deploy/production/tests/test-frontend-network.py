#!/usr/bin/env python3
"""Exercise real network helper with isolated Docker inventory, never a daemon."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='frontend-network-') as directory:
    host = Path(directory)
    tools = host / 'tools'
    tools.mkdir()
    docker = tools / 'docker'
    docker.write_text('''#!/usr/bin/env python3
import json, os, sys
a=sys.argv[1:]
if a[:2]==['network','ls']: print('basic-platform-production')
elif a[:2]==['network','inspect']: print(os.environ['NETWORK'])
elif a[0]=='ps': print('api')
elif a[0]=='inspect': print(json.dumps([{'Config':{'Labels':{'com.docker.compose.project':'basic-platform-production','com.docker.compose.service': 'frontend' if a[1]=='frontend' else 'other'},'Env':['APP_TRUSTED_PROXIES='+os.environ.get('API_TRUST','')]}}]))
else: sys.exit(1)
''')
    docker.chmod(0o755)
    env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'])
    runtime = host / '.env'
    def run(subnet, gateway, content='', containers=None, success=True, mode='prepare'):
        runtime.write_text(content)
        env['NETWORK'] = json.dumps([{'IPAM': {'Config': [{'Subnet': subnet, 'Gateway': gateway}]},
                                     'Containers': containers or {}}])
        process = subprocess.run(['python3', str(ROOT / 'bin/frontend-network.py'), mode, str(runtime)],
                                 env=env, capture_output=True, text=True)
        assert (process.returncode == 0) == success, process.stdout + process.stderr
        if not success:
            assert runtime.read_text() == content
        return runtime.read_text()
    loopback = '127.0.0.1/32,::1/128,'
    expected = 'FRONTEND_IPV4_ADDRESS=172.18.0.2\nAPP_TRUSTED_PROXIES=' + loopback + '172.18.0.1/32,172.18.0.2/32\n'
    peers = {'frontend': {'IPv4Address': '172.18.0.2/16'}, 'other': {'IPv4Address': '172.18.0.3/16'}}
    assert run('172.18.0.0/16', '172.18.0.1', containers=peers) == expected
    legacy = 'APP_TRUSTED_PROXIES=127.0.0.1/32,::1/128,172.16.0.0/12\n'
    assert run('172.18.0.0/16', '172.18.0.1', legacy, peers) == expected
    assert 'FRONTEND_IPV4_ADDRESS=172.31.255.250' in run('172.31.255.0/24', '172.31.255.1')
    assert 'FRONTEND_IPV4_ADDRESS=172.31.255.254' in run('172.31.255.0/24', '172.31.255.1', containers={'other': {'IPv4Address': '172.31.255.250/24'}})
    assert 'FRONTEND_IPV4_ADDRESS=192.168.42.2' in run('192.168.42.0/30', '192.168.42.1')
    run('192.168.42.0/30', '192.168.42.1', containers={'other': {'IPv4Address': '192.168.42.2/30'}}, success=False)
    run('172.18.0.0/16', '172.18.0.1', 'FRONTEND_IPV4_ADDRESS=172.31.255.250\n', success=False)
    run('172.18.0.0/16', '172.18.0.1', 'FRONTEND_IPV4_ADDRESS=172.18.0.3\n', peers, success=False)
    run('172.18.0.0/16', '172.18.0.1', 'FRONTEND_IPV4_ADDRESS=172.18.0.1\n', success=False)
    run('172.18.0.0/16', '172.18.0.1', 'APP_TRUSTED_PROXIES=172.18.0.0/16\n', peers, success=False)
    custom = 'FRONTEND_IPV4_ADDRESS=172.18.0.2\nAPP_TRUSTED_PROXIES=' + loopback + '172.18.0.1/32,172.18.0.2/32,203.0.113.5/32\n'
    assert run('172.18.0.0/16', '172.18.0.1', custom, peers) == custom
    run('172.18.0.0/16', '172.18.0.1', 'FRONTEND_IPV4_ADDRESS=\nFRONTEND_IPV4_ADDRESS=172.18.0.2\n', peers, success=False)
    compose = host / 'docker-compose.yml'
    compose.write_text('services:\n  frontend:\n    networks:\n      application:\n        ipv4_address: 172.18.1.10\n')
    run('172.18.0.0/16', '172.18.0.1', containers=peers, success=False)
    compose.unlink()
    env['API_TRUST'] = expected.split('APP_TRUSTED_PROXIES=')[1].strip()
    run('172.18.0.0/16', '172.18.0.1', expected, peers, mode='check')
    compose.write_text('services:\n  frontend:\n    networks:\n      application:\n        ipv4_address: 172.31.255.250\n')
    run('172.18.0.0/16', '172.18.0.1', expected, peers, success=False, mode='check')
    compose.unlink()
    env['API_TRUST'] = legacy.split('APP_TRUSTED_PROXIES=')[1].strip()
    run('172.18.0.0/16', '172.18.0.1', expected, peers, success=False, mode='check')
print('Existing network, exact trust, conflict validation and runtime readiness: PASS')
