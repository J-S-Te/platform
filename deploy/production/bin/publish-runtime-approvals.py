#!/usr/bin/env python3
"""仅限 root 使用的离线已审核运行批准发布工具，默认仅验证文件系统。

可信的 --bundle-sha256 是独立审核过的 SHA256SUMS 文件字节摘要，
不是签名或目标证明。本工具不导入授权、不执行 SQL、不登记组件，
不变更授权执行状态，也不发送 READY/ACK。
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile
import uuid
from datetime import datetime, timezone, timedelta


APPS = {
    'customer_and_opportunity': {'customer-api', 'customer-opportunity-alert-worker',
        'customer-owner-notification-worker', 'customer-contract-transfer-worker',
        'customer-presale-alert-worker', 'customer-presale-assignment-notification-worker',
        'customer-presale-progress-notification-worker', 'customer-notification-delivery-worker',
        'customer-presale-worker'},
    'contract_management': {'contract-api', 'contract-worker'},
    'project_management': {'project-api', 'project-sla-notifier'},
    'data_analysis': {'data-analysis-api', 'data-analysis-aggregation-worker', 'data-analysis-alert-worker'},
}
RELEASES = {f'runtime-license-{app}-prod.json' for app in APPS}
BUNDLE_FILES = RELEASES | {'license-evidence.json', 'upgrade-before.json',
                          'upgrade-review.json', 'approval-review.json'}
# 结算及门户文件仅为兼容旧版而逐字节保留，不属于此四应用升级入口，绝不在此确认；下方拒绝运行中但未选定的业务容器。
APPROVAL_FILES = RELEASES | {'runtime-license-customer_portal-prod.json',
    'runtime-license-settlement-prod.json', 'license-evidence.json', 'license-migration-evidence.json'}
# 此发布器刻意限定为已审核的四系统升级边界，不是通用安装或退役工具。
EXCLUDED = {'docker-socket-proxy', 'contract-mysql', 'file-gateway-mysql', 'keycloak-backup-metrics',
    'prometheus', 'keycloak', 'platform-mysql', 'customer-mysql', 'temporal', 'keycloak-db',
    'project-mysql', 'frontend', 'subsystem-provisioner', 'data-analysis-mysql', 'file-gateway',
    'platform-api', 'platform-worker'}
UNSELECTED_BUSINESS = {'portal-api', 'portal-report-worker', 'portal-invite-compensation-worker',
                       'settlement-api', 'settlement-worker', 'settlement-catalog-sync'}
HEX = re.compile(r'[a-f0-9]{64}')
DIGEST = re.compile(r'sha256:[a-f0-9]{64}')
IDENT = re.compile(r'[a-z][a-z0-9_-]{0,63}')


class Rejected(Exception):
    pass


def require(condition, message):
    if not condition:
        raise Rejected(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':')) + '\n').encode()


def safe_path(path, directory=False):
    path = Path(path)
    require(path.is_absolute() and path != Path('/') and str(path) == str(path.resolve()) and
            not any(c in str(path) for c in '\r\n\x00\\'), 'unsafe path')
    for part in (path, *path.parents):
        require(not part.is_symlink(), 'symbolic link refused')
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode), 'unsafe file type')
    require(info.st_mode & 0o022 == 0, 'group/world writable path refused')
    if not directory:
        require(info.st_nlink == 1, 'hard-linked file refused')
    return path


def regular(path, limit=65536):
    path = safe_path(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_size <= limit,
                'bounded regular file required')
        raw = os.read(fd, limit + 1)
        require(len(raw) <= limit, 'file too large')
        return raw
    finally:
        os.close(fd)


def pairs(items):
    value = {}
    for key, item in items:
        require(key not in value, 'duplicate JSON key')
        value[key] = item
    return value


def decode(raw):
    return json.loads(raw.decode('utf-8'), object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(Rejected('non-finite JSON')))


def fields(value, required, optional=()):
    require(isinstance(value, dict) and set(required) <= set(value) <= set(required) | set(optional),
            'invalid JSON schema')


def timestamp(value):
    require(isinstance(value, str), 'invalid timestamp')
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    require(parsed.tzinfo is not None and parsed.timestamp() > 0 and
            parsed <= datetime.now(timezone.utc) + timedelta(seconds=60), 'invalid/future timestamp')


def evidence(value, target, legacy=False):
    fields(value, {'version', 'project', 'installation_boundary', 'services', 'excluded_services'}, {'infrastructure'})
    require(type(value['version']) is int and value['version'] == 1 and
            value['installation_boundary'] is True and isinstance(value['project'], str) and
            IDENT.fullmatch(value['project']), 'invalid evidence boundary')
    excluded = value['excluded_services']
    require(isinstance(excluded, list) and len(excluded) <= 128 and
            all(isinstance(x, str) and IDENT.fullmatch(x) for x in excluded) and
            len(set(excluded)) == len(excluded), 'invalid exclusions')
    require(legacy or set(excluded) == EXCLUDED, 'complete reviewed 17 infrastructure exclusions required')
    expected = {name: app for app, names in APPS.items() for name in names}
    if not target:
        expected.pop('customer-contract-transfer-worker')
    services = value['services']
    require(isinstance(services, list) and (len(expected) <= len(services) <= len(expected) + 5 if legacy
            else len(services) == len(expected)), 'complete four-system evidence required')
    by_name = {}
    for item in services:
        fields(item, {'application', 'environment', 'name', 'image_digest', 'version', 'protocol', 'replicas'})
        name = item['name']
        known = expected.get(name) == item['application']
        if legacy and name in UNSELECTED_BUSINESS:
            known = item['application'] == ('customer_portal' if name.startswith('portal-') else 'settlement')
        require(isinstance(name, str) and known and
                name not in by_name and item['environment'] == 'prod' and
                isinstance(item['image_digest'], str) and DIGEST.fullmatch(item['image_digest']) and
                item['protocol'] == '1' and isinstance(item['version'], str) and
                re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', item['version']) and
                type(item['replicas']) is int and item['replicas'] == 1, 'invalid service evidence')
        by_name[name] = item
    require(set(expected) <= set(by_name), 'installed approval missing selected member')
    infra = value.get('infrastructure')
    require(isinstance(infra, list) and len(infra) == 1, 'Metabase evidence required')
    item = infra[0]
    fields(item, {'application', 'environment', 'name', 'image_digest', 'replicas', 'gateway_service'})
    require(item['application'] == 'data_analysis' and item['environment'] == 'prod' and
            item['name'] == 'data-analysis-metabase' and item['gateway_service'] == 'data-analysis-api' and
            type(item['replicas']) is int and item['replicas'] == 1 and
            isinstance(item['image_digest'], str) and DIGEST.fullmatch(item['image_digest']), 'invalid Metabase evidence')
    require(not (set(by_name) | {'data-analysis-metabase'}) & set(excluded), 'business exclusion refused')
    return by_name, item


def release(value, app, expected_names):
    fields(value, {'version', 'application', 'environment', 'components'}, {'release_generation', 'retire_service_ids'})
    require(type(value['version']) is int and value['version'] == 1 and value['application'] == app and
            value['environment'] == 'prod' and value.get('retire_service_ids', []) == [], 'invalid release scope/retirement')
    components = value['components']
    require(isinstance(components, list) and len(components) == len(expected_names), 'incomplete approved components')
    by_name = {}
    for component in components:
        fields(component, {'service_id', 'protocol', 'image_digest', 'coverage_digest'})
        name = component['service_id']
        require(isinstance(name, str) and name in expected_names and name not in by_name and
                type(component['protocol']) is int and component['protocol'] == 1 and
                all(isinstance(component[k], str) and DIGEST.fullmatch(component[k])
                    for k in ('image_digest', 'coverage_digest')), 'invalid component')
        by_name[name] = component
    return by_name


def load_bundle(directory, trusted_sha):
    directory = safe_path(directory, True)
    require(HEX.fullmatch(trusted_sha or '') is not None, 'trusted manifest SHA256 required')
    raw = regular(directory / 'SHA256SUMS')
    require(sha(raw) == trusted_sha, 'untrusted SHA256SUMS')
    require({p.name for p in directory.iterdir()} == BUNDLE_FILES | {'SHA256SUMS'}, 'unexpected/missing bundle files')
    hashes = {}
    for row in raw.decode('ascii').splitlines():
        match = re.fullmatch(r'([a-f0-9]{64})  ([A-Za-z0-9_.-]+)', row)
        require(match is not None and match[2] in BUNDLE_FILES and match[2] not in hashes, 'invalid SHA256SUMS')
        hashes[match[2]] = match[1]
    require(set(hashes) == BUNDLE_FILES, 'incomplete SHA256SUMS')
    contents = {name: regular(directory / name) for name in hashes}
    require(all(sha(contents[name]) == digest for name, digest in hashes.items()), 'bundle hash mismatch')
    docs = {name: decode(raw) for name, raw in contents.items()}
    review = docs['approval-review.json']
    fields(review, {'operator', 'candidate', 'created_at', 'input_reviewed'})
    require(review['candidate'] is False and review['input_reviewed'] is True and
            isinstance(review['operator'], str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}', review['operator']),
            'formal upgrade approval required')
    timestamp(review['created_at'])
    receipt = docs['upgrade-review.json']
    fields(receipt, {'version', 'inventory_sha256', 'mapping_sha256', 'registry_sha256',
                     'target_facts_sha256', 'expected_sha256'})
    require(type(receipt['version']) is int and receipt['version'] == 1 and
            all(isinstance(v, str) and HEX.fullmatch(v) for k, v in receipt.items() if k != 'version'), 'invalid upgrade receipt')
    target, infra = evidence(docs['license-evidence.json'], True)
    old, old_infra = evidence(docs['upgrade-before.json'], False)
    require(docs['upgrade-before.json']['project'] == docs['license-evidence.json']['project'] and
            docs['upgrade-before.json']['excluded_services'] == docs['license-evidence.json']['excluded_services'] and
            old_infra['image_digest'] == infra['image_digest'], 'upgrade boundary changed')
    for app, names in APPS.items():
        value = docs[f'runtime-license-{app}-prod.json']
        components = release(value, app, names)
        generation = value.get('release_generation')
        require(isinstance(generation, str) and IDENT.fullmatch(generation), 'explicit new generation required')
        require(all(c['image_digest'] == target[name]['image_digest'] for name, c in components.items()),
                'release and target evidence disagree')
    return docs, contents, hashes, target, old, infra


def current_snapshot(root, authority_required=True):
    result = {}
    directory = root / 'runtime-approvals'
    authority_names = set()
    if directory.exists() or directory.is_symlink() or authority_required:
        safe_path(directory, True)
        authority_names = {p.name for p in directory.iterdir()}
        require(authority_names <= APPROVAL_FILES, 'unknown authoritative approval file')
    for base, names in [(directory, authority_names), (root, APPROVAL_FILES)]:
        for name in sorted(names):
            path = base / name
            if not path.exists() and not path.is_symlink():
                continue
            raw = regular(path)
            info = path.stat()
            result[str(path.relative_to(root))] = {'sha256': sha(raw), 'mode': stat.S_IMODE(info.st_mode),
                'uid': info.st_uid, 'gid': info.st_gid}
    return result


def capture_approval_backup(root, destination):
    """仅捕获固定名称的公开批准文件，排除授权、实例信息及 Secrets。"""
    safe_path(root, True)
    safe_path(destination, True)
    snapshot = current_snapshot(root, False)
    contents = {name: regular(root / name) for name in snapshot}
    require(not (destination / 'runtime-approval-backup.json').exists(), 'backup metadata already exists')
    require(not (destination / 'runtime-approvals').exists() and
            not (destination / 'runtime-approvals').is_symlink() and
            not any((destination / name).exists() or (destination / name).is_symlink() for name in APPROVAL_FILES),
            'approval backup refuses overwrite')
    if (root / 'runtime-approvals').exists():
        (destination / 'runtime-approvals').mkdir(mode=0o700)
    for name, raw in contents.items():
        atomic_write(destination / name, raw)
    value = {'version': 1, 'authority_present': (root / 'runtime-approvals').exists(),
             'files': {name: item['sha256'] for name, item in snapshot.items()}}
    atomic_write(destination / 'runtime-approval-backup.json', canonical(value))
    verify_approval_backup(destination)


def verify_approval_backup(destination):
    safe_path(destination, True)
    value = decode(regular(destination / 'runtime-approval-backup.json'))
    fields(value, {'version', 'authority_present', 'files'})
    require(value['version'] == 1 and type(value['authority_present']) is bool and isinstance(value['files'], dict),
            'invalid approval backup metadata')
    snapshot = current_snapshot(destination, False)
    require(value['authority_present'] == (destination / 'runtime-approvals').exists() and
            value['files'] == {name: item['sha256'] for name, item in snapshot.items()}, 'approval backup proof mismatch')


def verify(root, bundle, trusted_sha, before_sha=None):
    root = safe_path(root, True)
    for name in ('.env', '.release.env', 'docker-compose.yml'):
        regular(root / name, 2 * 1024 * 1024)
    safe_path(root / 'runtime', True)
    safe_path(root / 'bin', True)
    docs, contents, hashes, target, old, infra = load_bundle(bundle, trusted_sha)
    identity = {}
    for line in regular(root / '.env', 2 * 1024 * 1024).decode('utf-8').splitlines():
        if '=' in line and not line.lstrip().startswith('#'):
            key, value = line.split('=', 1)
            if key in {'COMPOSE_PROJECT_NAME', 'SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT'}:
                require(key not in identity, 'duplicate deployment identity')
                identity[key] = value
    require(identity.get('COMPOSE_PROJECT_NAME', 'basic-platform-production') == docs['license-evidence.json']['project'] and
            identity.get('SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT', '/opt/unified-identity-platform') == str(root),
            'deployment identity mismatch')
    snapshot = current_snapshot(root)
    current_sha = sha(canonical(snapshot))
    require(before_sha is None or before_sha == current_sha, 'old approval CAS mismatch')
    previous_evidence = decode(regular(root / 'runtime-approvals' / 'license-evidence.json'))
    previous_services, _ = evidence(previous_evidence, False, legacy=True)
    require(previous_evidence['project'] == docs['upgrade-before.json']['project'], 'installed project boundary changed')
    legacy_drift = previous_evidence != docs['upgrade-before.json']
    for app, names in APPS.items():
        name = f'runtime-license-{app}-prod.json'
        previous = decode(regular(root / 'runtime-approvals' / name))
        expected = names - {'customer-contract-transfer-worker'}
        components = release(previous, app, expected)
        require(previous.get('release_generation', '') != docs[name]['release_generation'], 'generation replay refused')
        # 历史配置 ID 不要求等于原生描述符 ID。通过外部 CAS 冻结这些字节，并另行证明旧运行实例的真实 ID。
        require(all(c['image_digest'] == previous_services[n]['image_digest'] for n, c in components.items()),
                'installed release/evidence inconsistent')
        shadow = root / name
        require(not shadow.exists() or regular(shadow) == regular(root / 'runtime-approvals' / name), 'legacy approval shadow drift')
    if (root / 'license-evidence.json').exists():
        require(regular(root / 'license-evidence.json') == regular(root / 'runtime-approvals' / 'license-evidence.json'),
                'legacy evidence shadow drift')
    return {'mode': 'verify-only', 'consumers': 16, 'before_sha256': current_sha,
            'bundle_sha256': trusted_sha, 'legacy_inventory_drift': legacy_drift}, (docs, contents, hashes, target, old, infra, snapshot)


class Commands:
    def call(self, args, pass_fds=(), timeout=900):
        # 不要继承 Shell 启动代码、用户 Docker 上下文或凭据辅助工具。
        env = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
               'LANG': 'C.UTF-8', 'DOCKER_HOST': 'unix:///var/run/docker.sock', 'UIP_OFFLINE_DEPLOYMENT': 'true'}
        if 'HOME' in os.environ:
            env['HOME'] = os.environ['HOME']
        with tempfile.TemporaryDirectory(prefix='approval-docker-config-') as docker_config:
            env['DOCKER_CONFIG'] = docker_config
            process = subprocess.Popen([str(x) for x in args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       start_new_session=True, pass_fds=pass_fds, env=env)
            try:
                output, _ = process.communicate(timeout=timeout)
            except BaseException:
                # 仅终止包装器可能留下 Compose 子进程，使其在调用方已尝试故障隔离后重新开放 API。
                if process.poll() is None:
                    try:
                        os.killpg(process.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                    try:
                        process.communicate(timeout=5)
                    except subprocess.TimeoutExpired:
                        try:
                            os.killpg(process.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        process.communicate(timeout=5)
                raise
        require(process.returncode == 0, 'maintenance dependency failed (exit %d)' % process.returncode)
        require(len(output) <= 2 * 1024 * 1024, 'maintenance response too large')
        return output


def atomic_write(path, raw):
    safe_path(path.parent, True)
    if path.exists() or path.is_symlink():
        safe_path(path)
    fd, name = tempfile.mkstemp(prefix='.approval-write-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def journal_path(root):
    return root / 'runtime/.runtime-approval-publish/journal.json'


def configuration_snapshot(root):
    paths = [root / name for name in ('.env', '.release.env', 'docker-compose.yml')]
    profiles = root / 'subsystems.d'
    if profiles.exists() or profiles.is_symlink():
        safe_path(profiles, True)
        paths.extend(sorted(profiles.glob('*.yaml')))
    return {str(path.relative_to(root)): sha(regular(path, 2 * 1024 * 1024)) for path in paths}


def read_journal(root):
    value = decode(regular(journal_path(root)))
    fields(value, {'version', 'bundle_sha256', 'before_sha256', 'backup', 'system_backup',
                   'before', 'after', 'state', 'configuration'})
    require(value['version'] == 1 and HEX.fullmatch(value['bundle_sha256']) and
            HEX.fullmatch(value['before_sha256']) and isinstance(value['before'], dict) and
            isinstance(value['after'], dict) and value['state'] in
            {'prepared', 'publishing', 'published', 'agent-verified', 'complete'}, 'invalid publication journal')
    require(isinstance(value['configuration'], dict) and all(isinstance(k, str) and isinstance(v, str) and
            HEX.fullmatch(v) for k, v in value['configuration'].items()), 'invalid configuration binding')
    for mapping in (value['before'], value['after']):
        for name, info in mapping.items():
            require(name in APPROVAL_FILES or name.startswith('runtime-approvals/') and
                    name.removeprefix('runtime-approvals/') in APPROVAL_FILES, 'invalid journal target')
            fields(info, {'sha256', 'mode', 'uid', 'gid'})
            require(isinstance(info['sha256'], str) and HEX.fullmatch(info['sha256']) and
                    type(info['mode']) is int and 0 <= info['mode'] <= 0o777 and info['mode'] & 0o022 == 0 and
                    all(type(info[k]) is int and info[k] >= 0 for k in ('uid', 'gid')),
                    'invalid journal file proof')
    return value


def verify_agent(root, container, runner=None):
    """固定的 API 启动前钩子，不接受调用方提供的文件列表或命令。"""
    runner = runner or Commands()
    require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', container or ''), 'invalid Agent container')
    value = read_journal(root)
    require(value['state'] in {'published', 'agent-verified', 'complete'}, 'publication incomplete; API remains closed')
    require(current_snapshot(root) == value['after'], 'host approval set differs from committed transaction')
    mounts = decode(runner.call(['docker', 'inspect', '--format', '{{json .Mounts}}', container], timeout=60))
    destination = str(root / 'runtime-approvals')
    require(isinstance(mounts, list), 'Agent mount proof missing')
    bound = [m for m in mounts if isinstance(m, dict) and m.get('Destination') == destination]
    require(len(bound) == 1 and bound[0].get('Source') == destination and
            bound[0].get('Type') == 'bind' and bound[0].get('RW') is False,
            'Agent authority directory must use exact read-only bind')
    paths = sorted(name for name in value['after'] if name.startswith('runtime-approvals/'))
    # 所有路径均来自固定文件名白名单，不是 Shell 命令。
    script = 'set -eu; for file do [ -f "$file" ] && [ ! -L "$file" ]; sha256sum "$file"; done'
    raw = runner.call(['docker', 'exec', container, 'sh', '-ec', script, '_',
                       *[str(root / name) for name in paths]], timeout=60)
    expected = ''.join(value['after'][name]['sha256'] + '  ' + str(root / name) + '\n' for name in paths).encode()
    require(raw == expected, 'Agent approval hash mismatch; API remains closed')
    value['state'] = 'agent-verified' if value['state'] != 'complete' else 'complete'
    atomic_write(journal_path(root), canonical(value))


def reload_check(root, fd):
    require(os.geteuid() == 0 and fd >= 3, 'root inherited deployment lock required')
    expected = safe_path(root / 'runtime/.deploy.lock').stat()
    actual = os.fstat(fd)
    require(stat.S_ISREG(actual.st_mode) and actual.st_dev == expected.st_dev and actual.st_ino == expected.st_ino,
            'inherited lock points to wrong file')
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise Rejected('publication does not own deployment lock')
    value = read_journal(root)
    require(value['state'] == 'published' and current_snapshot(root) == value['after'], 'incomplete publication reload refused')
    require(regular(root / 'runtime/.control-plane-reload-required') ==
            ('RUNTIME_APPROVAL_PUBLISH_SHA256=' + value['bundle_sha256'] + '\n').encode(), 'publication reload marker mismatch')


class Publisher:
    def __init__(self, root, bundle, bundle_sha, before_sha, runner=None):
        self.root, self.bundle = Path(root), Path(bundle)
        self.bundle_sha, self.before_sha = bundle_sha, before_sha
        self.runner = runner or Commands()
        self.lock_fd = None

    def compose(self, *args):
        return ['docker', 'compose', '--project-directory', str(self.root), '--file',
                str(self.root / 'docker-compose.yml'), '--env-file', str(self.root / '.env'),
                '--env-file', str(self.root / '.release.env'), *args]

    def stop_api(self):
        self.runner.call(self.compose('stop', '--timeout', '60', 'platform-api'), timeout=90)

    def idle(self):
        # 现有 root CLI 观察持久化 READY 状态，而非仅进程健康；不准备购买、不对账成员，也不发送 READY 或 ACK。
        for app in sorted(APPS):
            value = decode(self.runner.call(self.compose('run', '-T', '--rm', '--no-deps', 'platform-api',
                './license-install', 'verify-deployment', '--application', app, '--timeout', '60s'), timeout=90))
            fields(value, {'application', 'environment', 'status', 'manifest_checksum'})
            require(value['application'] == app and value['environment'] == 'prod' and value['status'] == 'READY'
                    and isinstance(value['manifest_checksum'], str) and value['manifest_checksum'],
                    'old deployment not READY; operator must resolve old orchestration')

    def images(self, docs, target, old, infra):
        seen = set()
        for item in list(target.values()) + [infra]:
            image = item['image_digest']
            if image in seen:
                continue
            seen.add(image)
            images = decode(self.runner.call(['docker', 'image', 'inspect', image], timeout=60))
            require(isinstance(images, list) and len(images) == 1, 'target image missing')
            image_info = images[0]
            require(image_info.get('Id') == image and image_info.get('Os') == 'linux' and
                    image_info.get('Architecture') == 'amd64', 'target native image/platform mismatch')
            if item['name'] != 'data-analysis-metabase':
                labels = image_info.get('Config', {}).get('Labels') or {}
                require(labels.get('org.opencontainers.image.version') == item['version'] and
                        labels.get('com.basic-platform.license.protocol') == '1', 'target image labels mismatch')
        # 复用同一镜像的多个消费者必须独立确认元数据一致。
        for item in target.values():
            require(all(other['version'] == item['version'] for other in target.values()
                        if other['image_digest'] == item['image_digest']), 'shared image version disagreement')
        project = docs['upgrade-before.json']['project']
        raw_ids = self.runner.call(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project], timeout=60)
        ids = raw_ids.decode('ascii').splitlines()
        require(0 < len(ids) <= 128 and len(set(ids)) == len(ids) and
                all(re.fullmatch(r'[a-f0-9]{12,64}', x) for x in ids), 'ambiguous running inventory')
        containers = decode(self.runner.call(['docker', 'inspect', *ids], timeout=60))
        require(isinstance(containers, list) and len(containers) == len(ids), 'incomplete Compose container inventory')
        expected = dict(old)
        expected['data-analysis-metabase'] = docs['upgrade-before.json']['infrastructure'][0]
        found = set()
        all_targets = {name for names in APPS.values() for name in names} | {'data-analysis-metabase'}
        excluded = set(docs['upgrade-before.json']['excluded_services'])
        found_excluded, inspected_ids = set(), set()
        for container in containers:
            container_id = container.get('Id')
            require(isinstance(container_id, str) and re.fullmatch(r'[a-f0-9]{64}', container_id) and
                    container_id not in inspected_ids and sum(container_id.startswith(x) for x in ids) == 1,
                    'invalid/duplicate Compose container identity')
            inspected_ids.add(container_id)
            labels = container.get('Config', {}).get('Labels') or {}
            name = labels.get('com.docker.compose.service')
            require(labels.get('com.docker.compose.project') == project, 'foreign running container')
            require(name not in UNSELECTED_BUSINESS, 'unselected business container exists; explicit retirement review required')
            if name not in all_targets:
                require(name in excluded, 'unknown running service outside reviewed exclusions')
                found_excluded.add(name)
                continue
            require(name in expected and name not in found and container.get('State', {}).get('Running') is True and
                    str(labels.get('com.docker.compose.oneoff', '')).lower() == 'false' and
                    container.get('Image') == expected[name]['image_digest'], 'old running image/replica CAS mismatch')
            found.add(name)
        require(found == set(expected), 'old running membership incomplete')
        require(found_excluded == excluded, 'complete Compose infrastructure inventory changed')

    def publish(self, confirmation, resume=False):
        require(confirmation == 'PUBLISH_RUNTIME_APPROVALS_MAINTENANCE', 'explicit maintenance confirmation required')
        require(os.geteuid() == 0, 'root maintenance execution required')
        require(HEX.fullmatch(self.before_sha or '') is not None, 'explicit old approval CAS required')
        root = safe_path(self.root, True)
        safe_path(root / 'runtime', True)
        safe_path(root / 'backups', True)
        for path in [root, root / 'runtime', root / 'runtime-approvals', root / 'backups', root / 'bin',
                     *[root / name for name in ('.env', '.release.env', 'docker-compose.yml',
                        'bin/backup-all.sh', 'bin/lifecycle.sh', 'bin/provisioner-config-refresh.sh')]]:
            safe_path(path, path.is_dir())
            require(path.stat().st_uid == os.getuid(), 'maintenance targets must be owned by invoking root')
        lock = root / 'runtime/.deploy.lock'
        if lock.exists() or lock.is_symlink():
            safe_path(lock)
        self.lock_fd = os.open(lock, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        closed_gate = False
        try:
            try:
                fcntl.flock(self.lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise Rejected('global deployment lock busy')
            require(not (root / 'runtime/.assets-install-transaction').exists(), 'unfinished assets installation')
            transaction = root / 'runtime/.runtime-approval-publish'
            marker = root / 'runtime/.control-plane-reload-required'
            if resume:
                value = read_journal(root)
                require(value['bundle_sha256'] == self.bundle_sha and value['before_sha256'] == self.before_sha,
                        'resume must bind exact original transaction')
                require(configuration_snapshot(root) == value['configuration'], 'runtime configuration drift; resume refused')
                docs, contents, hashes, target, old, infra = load_bundle(self.bundle, self.bundle_sha)
                current = current_snapshot(root)
                require(set(current) == set(value['after']) and all(current[name] in
                    (value['before'].get(name), value['after'][name]) for name in current), 'resume refuses external approval drift')
                backup = safe_path(Path(value['backup']), True)
                require(backup.parent == root / 'backups' and backup.name.startswith('approval-publish-'), 'unsafe recovery backup')
                require(decode(regular(backup / 'before.json')) == value['before'], 'old approval backup metadata mismatch')
                for name, info in value['before'].items():
                    require(sha(regular(backup / name)) == info['sha256'], 'old approval backup corrupted')
                for name, raw in contents.items():
                    require(regular(backup / 'bundle' / name) == raw, 'saved formal bundle changed')
                require(marker.exists() or value['state'] == 'complete', 'publication marker missing')
                if value['state'] == 'complete':
                    return {'mode': 'published', 'consumers': 16, 'bundle_sha256': self.bundle_sha,
                            'backup': str(backup), 'business_updated': False, 'runtime_ack_observed': False}
            else:
                require(not transaction.exists() and not transaction.is_symlink() and not marker.exists() and not marker.is_symlink(),
                        'unfinished publication/reload; use exact transaction resume')
                proof, details = verify(root, self.bundle, self.bundle_sha, self.before_sha)
                docs, contents, hashes, target, old, infra, snapshot = details
                self.images(docs, target, old, infra)
                output = self.runner.call(['bash', str(root / 'bin/backup-all.sh'), '--deployment-lock-fd', str(self.lock_fd)],
                                          pass_fds=(self.lock_fd,), timeout=7200).decode('utf-8')
                batches = re.findall(r'^verified disaster recovery backup: (.+)$', output, re.MULTILINE)
                require(len(batches) == 1, 'verified disaster backup receipt missing')
                system_backup = safe_path(Path(batches[0]), True)
                require(system_backup.parent == root / 'backups/system', 'unexpected disaster backup location')
                self.runner.call(['bash', str(root / 'bin/backup-all.sh'), '--verify-only', '--backup', str(system_backup)], timeout=7200)
                require(current_snapshot(root) == snapshot, 'old approval CAS changed during backup')
                backup = root / 'backups' / ('approval-publish-' + uuid.uuid4().hex)
                backup.mkdir(mode=0o700)
                for name, info in snapshot.items():
                    dest = backup / name
                    dest.parent.mkdir(mode=0o700, exist_ok=True)
                    atomic_write(dest, regular(root / name))
                    require(sha(regular(dest)) == info['sha256'], 'independent old approval backup failed')
                atomic_write(backup / 'before.json', canonical(snapshot))
                (backup / 'bundle').mkdir(mode=0o700)
                for name, raw in contents.items():
                    atomic_write(backup / 'bundle' / name, raw)
                atomic_write(backup / 'bundle/SHA256SUMS', regular(self.bundle / 'SHA256SUMS'))
                after = dict(snapshot)
                for name in sorted(RELEASES | {'license-evidence.json'}):
                    for rel in ['runtime-approvals/' + name] + ([name] if name in snapshot else []):
                        after[rel] = {'sha256': hashes[name], 'mode': 0o600, 'uid': os.getuid(), 'gid': os.getgid()}
                value = dict(version=1, bundle_sha256=self.bundle_sha, before_sha256=self.before_sha,
                    backup=str(backup), system_backup=str(system_backup), before=snapshot, after=after, state='prepared',
                    configuration=configuration_snapshot(root))
                transaction.mkdir(mode=0o700)
                atomic_write(journal_path(root), canonical(value))
            closed_gate = True
            atomic_write(marker, ('RUNTIME_APPROVAL_PUBLISH_SHA256=' + self.bundle_sha + '\n').encode())
            self.stop_api()
            self.runner.call(self.compose('stop', '--timeout', '60', 'subsystem-provisioner'), timeout=90)
            # 停止两个进程会销毁旧内存队列。必须拒绝任何持久化 UPDATING、VERIFYING 或 FAILED 工作，不能假装空闲。
            self.idle()
            self.images(docs, target, old, infra)
            value['state'] = 'publishing'
            atomic_write(journal_path(root), canonical(value))
            for name in sorted(RELEASES | {'license-evidence.json'}):
                atomic_write(root / 'runtime-approvals' / name, contents[name])
                if name in value['before']:
                    atomic_write(root / name, contents[name])
            os.chmod(root / 'runtime-approvals', 0o700)
            require(current_snapshot(root) == value['after'], 'published approval set mismatch')
            value['state'] = 'published'
            atomic_write(journal_path(root), canonical(value))
            # lifecycle 继承同一个已打开的锁，绝不对新文件描述符递归执行 flock。
            self.runner.call(['bash', str(root / 'bin/lifecycle.sh'), 'approval-reload',
                              '--deployment-lock-fd', str(self.lock_fd)], pass_fds=(self.lock_fd,), timeout=420)
            committed = read_journal(root)
            require(committed['state'] == 'agent-verified', 'pre-API Agent proof missing')
            committed['state'] = 'complete'
            atomic_write(journal_path(root), canonical(committed))
            atomic_write(backup / 'publication-receipt.json', canonical(committed))
            if marker.exists():
                require(regular(marker) == ('RUNTIME_APPROVAL_PUBLISH_SHA256=' + self.bundle_sha + '\n').encode(), 'reload marker changed')
                marker.unlink()
            closed_gate = False
            return {'mode': 'published', 'consumers': 16, 'bundle_sha256': self.bundle_sha,
                    'backup': str(backup), 'business_updated': False, 'runtime_ack_observed': False}
        except BaseException as error:
            if closed_gate:
                try:
                    self.stop_api()
                except Exception:
                    raise Rejected('publication failed and API stop also failed; manual containment required') from error
            raise
        finally:
            os.close(self.lock_fd)
            self.lock_fd = None


def file_sha(path):
    digest = hashlib.sha256()
    with open(path, 'rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def print_bundle_diff(before, current):
    before, current = Path(before), Path(current)
    old = {p.name: file_sha(p) for p in before.iterdir() if p.is_file() and not p.is_symlink()} if before.is_dir() else {}
    new = {p.name: file_sha(p) for p in current.iterdir() if p.is_file() and not p.is_symlink()} if current.is_dir() else {}
    print('审核差异：新增 %d，删除 %d，变化 %d' % (len(set(new) - set(old)), len(set(old) - set(new)),
                                                len({n for n in set(old) & set(new) if old[n] != new[n]})))
    for name in sorted(set(new) - set(old)):
        print('  + ' + name)
    for name in sorted(set(old) - set(new)):
        print('  - ' + name)
    for name in sorted(n for n in set(old) & set(new) if old[n] != new[n]):
        print('  ~ ' + name)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deploy-root', required=True, type=Path)
    parser.add_argument('--bundle', type=Path)
    parser.add_argument('--bundle-sha256')
    parser.add_argument('--before-sha256')
    parser.add_argument('--diff-before', type=Path, help='只读打印候选 bundle 与旧批准目录的差异')
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument('--apply', action='store_true')
    mode.add_argument('--verify-only', action='store_true')
    parser.add_argument('--resume', action='store_true')
    parser.add_argument('--confirm', default='')
    parser.add_argument('--agent-proof', help=argparse.SUPPRESS)
    parser.add_argument('--reload-check-fd', type=int, help=argparse.SUPPRESS)
    parser.add_argument('--capture-approval-backup', type=Path, help=argparse.SUPPRESS)
    parser.add_argument('--verify-approval-backup', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    try:
        if args.reload_check_fd is not None:
            require(args.bundle is None and not args.apply and not args.agent_proof, 'invalid internal reload invocation')
            reload_check(args.deploy_root, args.reload_check_fd)
            return 0
        if args.capture_approval_backup or args.verify_approval_backup:
            require(args.bundle is None and not args.apply and not args.resume and not args.agent_proof,
                    'invalid approval backup invocation')
            if args.capture_approval_backup:
                capture_approval_backup(args.deploy_root, args.capture_approval_backup)
            else:
                verify_approval_backup(args.deploy_root)
            return 0
        if args.agent_proof:
            require(os.geteuid() == 0 and not args.apply and not args.resume and args.bundle is None,
                    'invalid pre-API proof invocation')
            verify_agent(args.deploy_root, args.agent_proof)
            return 0
        require(args.bundle is not None and args.bundle_sha256 is not None, 'reviewed bundle required')
        if args.diff_before:
            print_bundle_diff(args.diff_before, args.deploy_root / 'runtime-approvals')
        if args.apply:
            def terminated(_number, _frame):
                raise InterruptedError('publication terminated; inspect durable transaction before resuming')
            previous = signal.signal(signal.SIGTERM, terminated)
            try:
                proof = Publisher(args.deploy_root, args.bundle, args.bundle_sha256, args.before_sha256).publish(args.confirm, args.resume)
            finally:
                signal.signal(signal.SIGTERM, previous)
        else:
            require(not args.resume and not args.confirm, 'resume requires explicit apply')
            proof, _ = verify(args.deploy_root, args.bundle, args.bundle_sha256, args.before_sha256)
        print(json.dumps(proof, sort_keys=True))
        return 0
    except (Rejected, OSError, ValueError, TypeError, KeyError, subprocess.TimeoutExpired) as error:
        print('APPROVAL_PUBLISH_REJECTED: ' + str(error), file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print('APPROVAL_PUBLISH_INTERRUPTED: inspect retained transaction and API containment', file=sys.stderr)
        return 130


if __name__ == '__main__':
    sys.exit(main())
