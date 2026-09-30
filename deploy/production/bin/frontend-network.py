#!/usr/bin/env python3
"""Prepare exact proxy peers without replacing a long-lived Docker network."""
import ipaddress
import json
import pathlib
import re
import subprocess
import sys

DEFAULT_IP = '172.31.255.250'
DEFAULT_TRUST = '127.0.0.1/32,::1/128,172.31.255.1/32,172.31.255.250/32'
LEGACY_TRUST = '127.0.0.1/32,::1/128,172.16.0.0/12'


def docker(*args):
    result = subprocess.run(['docker', *args], capture_output=True, text=True, timeout=15)
    if result.returncode:
        raise ValueError('Docker 网络或容器检查失败，拒绝更改配置')
    return json.loads(result.stdout)


def config(path):
    values = {}
    for line in path.read_text().splitlines():
        if not line.strip() or line.lstrip().startswith('#') or '=' not in line:
            continue
        key, value = line.split('=', 1)
        if key in ('FRONTEND_IPV4_ADDRESS', 'APP_TRUSTED_PROXIES', 'COMPOSE_PROJECT_NAME'):
            if key in values:
                raise ValueError('重复的网络配置键：' + key)
            values[key] = value.strip().strip('"\'')
    return values


def resolve(values):
    # A missing network is permitted only after a successful network inventory;
    # an unavailable daemon must never be mistaken for a new installation.
    result = subprocess.run(['docker', 'network', 'ls', '--filter',
                             'name=^basic-platform-production$', '--format', '{{.Name}}'],
                            capture_output=True, text=True, timeout=15)
    if result.returncode:
        raise ValueError('Docker 网络列表检查失败')
    if not result.stdout.strip():
        network = {'IPAM': {'Config': [{'Subnet': '172.31.255.0/24', 'Gateway': '172.31.255.1'}]},
                   'Containers': {}}
    else:
        inspected = docker('network', 'inspect', 'basic-platform-production')
        if len(inspected) != 1:
            raise ValueError('共享网络检查返回不唯一结果')
        network = inspected[0]
    ipv4 = [entry for entry in network['IPAM']['Config']
            if ipaddress.ip_network(entry['Subnet']).version == 4]
    if len(ipv4) != 1:
        raise ValueError('共享网络必须有唯一 IPv4 子网')
    subnet = ipaddress.ip_network(ipv4[0]['Subnet'])
    gateway = ipaddress.ip_address(ipv4[0]['Gateway'])
    if gateway not in subnet or gateway in (subnet.network_address, subnet.broadcast_address):
        raise ValueError('共享网络网关配置无效')
    project = values.get('COMPOSE_PROJECT_NAME') or 'basic-platform-production'
    peers = network.get('Containers') or {}
    owners = {}
    frontend = None
    for container_id, peer in peers.items():
        address = peer.get('IPv4Address', '').split('/')[0]
        if not address:
            continue
        inspected = docker('inspect', container_id)
        labels = inspected[0].get('Config', {}).get('Labels') or {}
        is_frontend = (labels.get('com.docker.compose.project') == project and
                       labels.get('com.docker.compose.service') == 'frontend')
        owners[ipaddress.ip_address(address)] = is_frontend
        if is_frontend:
            if frontend is not None:
                raise ValueError('共享网络存在多个当前项目 frontend 容器')
            frontend = ipaddress.ip_address(address)
    def usable(address):
        return (address.version == 4 and address in subnet and
                address not in (gateway, subnet.network_address, subnet.broadcast_address) and
                (address not in owners or owners[address]))
    explicit = values.get('FRONTEND_IPV4_ADDRESS')
    if explicit:
        selected = ipaddress.ip_address(explicit)
        if not usable(selected):
            raise ValueError('FRONTEND_IPV4_ADDRESS 不在子网内或被其他容器占用')
    elif usable(ipaddress.ip_address(DEFAULT_IP)):
        selected = ipaddress.ip_address(DEFAULT_IP)
    elif frontend is not None and usable(frontend):
        selected = frontend
    else:
        # Inspect only a bounded tail, not millions of addresses on large subnets.
        selected = next((ipaddress.ip_address(number) for number in
                         range(int(subnet.broadcast_address) - 1,
                               max(int(subnet.network_address), int(subnet.broadcast_address) - 256), -1)
                         if usable(ipaddress.ip_address(number))), None)
        if selected is None:
            raise ValueError('共享网络没有可安全固化的前端地址')
    required = {str(gateway) + '/32', str(selected) + '/32'}
    trust = values.get('APP_TRUSTED_PROXIES')
    if not trust or trust in (DEFAULT_TRUST, LEGACY_TRUST):
        trust = '127.0.0.1/32,::1/128,' + str(gateway) + '/32,' + str(selected) + '/32'
    else:
        exact = {str(ipaddress.ip_network(part.strip(), strict=False))
                 for part in trust.split(',')}
        if not required.issubset(exact):
            raise ValueError('自定义 APP_TRUSTED_PROXIES 未精确信任实际网关和前端；拒绝覆盖')
    return str(selected), trust


def main():
    mode, filename = sys.argv[1:]
    path = pathlib.Path(filename)
    values = config(path)
    address, trust = resolve(values)
    compose = path.parent / 'docker-compose.yml'
    frontend = False
    if compose.exists():
        for line in compose.read_text().splitlines():
            if re.match(r'^  [^\s#]', line):
                frontend = bool(re.match(r'^  frontend:\s*(?:#.*)?$', line))
            if frontend and re.match(r'^        ipv4_address:', line):
                literal = line.split(':', 1)[1].split('#', 1)[0].strip().strip('"\'')
                accepted = ('${FRONTEND_IPV4_ADDRESS:-172.31.255.250}', address)
                if mode == 'prepare':
                    accepted += (DEFAULT_IP,)
                if literal not in accepted:
                    raise ValueError('自定义 Compose 前端地址与配置不一致；拒绝覆盖')
    if mode == 'prepare':
        lines = [line for line in path.read_text().splitlines()
                 if line.split('=', 1)[0] not in ('FRONTEND_IPV4_ADDRESS', 'APP_TRUSTED_PROXIES')]
        prefix = ('\n'.join(lines) + '\n') if lines else ''
        path.write_text(prefix + 'FRONTEND_IPV4_ADDRESS=' + address +
                        '\nAPP_TRUSTED_PROXIES=' + trust + '\n')
    elif mode == 'check':
        if values.get('FRONTEND_IPV4_ADDRESS') != address or values.get('APP_TRUSTED_PROXIES') != trust:
            raise ValueError('平台尚未固化实际网络配置')
        project = values.get('COMPOSE_PROJECT_NAME') or 'basic-platform-production'
        result = subprocess.run(['docker', 'ps', '-q', '--filter',
                                 'label=com.docker.compose.project=' + project,
                                 '--filter', 'label=com.docker.compose.service=platform-api'],
                                capture_output=True, text=True, timeout=15)
        ids = result.stdout.split()
        if result.returncode or len(ids) != 1:
            raise ValueError('平台 API 容器不唯一或未运行')
        instance = docker('inspect', ids[0])[0]
        actual = [item.split('=', 1)[1] for item in instance['Config'].get('Env', [])
                  if item.startswith('APP_TRUSTED_PROXIES=')]
        if actual != [trust]:
            raise ValueError('平台 API 尚未加载当前精确信任配置')
    else:
        raise ValueError('未知网络检查模式')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, IndexError, subprocess.TimeoutExpired) as error:
        print('前端网络配置验证失败：' + str(error), file=sys.stderr)
        sys.exit(1)
