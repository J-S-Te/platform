#!/usr/bin/env bash
# 独立复制到服务器即可使用。仅默认盘点；不会调用部署目录内的脚本或 source .env。
set -Eeuo pipefail
umask 077
command -v python3 >/dev/null 2>&1 || { echo '错误：需要 Python 3（首装宿主依赖包含 python3）' >&2; exit 1; }
exec python3 - "${BASH_SOURCE[0]}" "$@" <<'PY'
import argparse
from contextlib import ExitStack
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile

SCRIPT = Path(sys.argv[1]).resolve()
PREFIX = 'com.docker.compose.'
DEFAULT_TARGET = '/opt/unified-identity-platform'
LIMIT = 16 * 1024 * 1024
PLAN_VERSION = 2
CANDIDATE_NAMES = {
    'uip-'+component+'-candidate' for component in
    ('contract', 'project', 'customer-opportunity', 'customer-portal', 'settlement', 'data-analysis')
}


class Refuse(Exception):
    pass


def fail(message):
    raise Refuse(message)


def now():
    return datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'),
                                    ensure_ascii=False).encode()).hexdigest()


def inside(path, root):
    return path == root or root in path.parents


def overlap(a, b):
    return inside(a, b) or inside(b, a)


def canonical(value):
    p = Path(value)
    if not p.is_absolute() or value.startswith('//') or str(p) != value or '..' in p.parts or any(ord(x)<32 or ord(x)==127 for x in value):
        fail('必须使用无 . / .. / 多余斜杠的绝对路径：' + value)
    for part in [p, *p.parents]:
        if part.is_symlink():
            fail('路径包含符号链接，拒绝操作：' + str(part))
    return p


def safe_target(value):
    p = canonical(value)
    if len(p.parts) < 3 or (len(p.parts) == 3 and p.parts[1] in ('home', 'Users')) or p in (Path.home(), Path('/var/lib'), Path('/var/tmp'),
                                 Path('/usr/local'), Path('/home'), Path('/Users')):
        fail('拒绝系统目录、用户主目录或过宽的目标：' + value)
    if any(inside(p, Path(x)) for x in ('/dev', '/proc', '/sys', '/etc', '/bin', '/sbin', '/lib', '/usr')):
        fail('目标位于系统目录：' + value)
    if inside(SCRIPT, p):
        fail('清理脚本必须放在待删除安装目录之外')
    if p.exists() and not p.is_dir():
        fail('目标不是目录：' + value)
    if any((p / x).exists() for x in ('.git', '.ai', '.agents', 'AGENTS.md')):
        fail('目标疑似源码/工作区，拒绝清理：' + value)
    if (p / 'SHA256SUMS').exists() and (p / 'install-assets.sh').exists():
        fail('目标是交付介质目录，拒绝删除原始安装材料')
    return p


# 用途：通过固定描述符读取有限大小的普通文件，拒绝跟随链接。
# 参数：path 为文件路径，limit 为字节上限，control 指定是否核验控制文件权限。
# 返回值：文件字节；类型、大小或控制文件权限异常时拒绝。
def read_regular(path, limit=LIMIT, control=False):
    canonical(str(path))
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            fail('不是有限大小的普通文件：' + str(path))
        if control and (info.st_uid != os.geteuid() or info.st_nlink != 1 or
                        stat.S_IMODE(info.st_mode) != 0o600):
            fail('计划/恢复记录必须属于当前执行用户，且为0600单链接文件：'+str(path))
        return stream.read(limit + 1)


def load_json(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                fail('JSON 含重复字段，拒绝使用')
            result[key] = value
        return result
    def invalid(_):
        fail('JSON 含非标准数值，拒绝使用')
    return json.loads(raw, object_pairs_hook=unique, parse_constant=invalid)


def env_values(path):
    if not path.exists():
        return {}
    values = {}
    # 只读取清理归属和路径所需字段；绝不求值、执行或输出其他配置。
    allowed = {'COMPOSE_PROJECT_NAME', 'PLATFORM_KEYS_DIR', 'FILE_GATEWAY_HOST_ROOT',
               'SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT', 'SUBSYSTEM_PRODUCTION_PROFILES_DIR',
               'UIP_IMPORT_TMPDIR', 'PUBLIC_HTTP_PORT', 'PUBLIC_HTTPS_PORT', 'KEYCLOAK_HTTP_PORT'}
    for line in read_regular(path).decode('utf-8').splitlines():
        match = re.match(r'^\s*(?:export\s+)?([A-Z_]+)\s*=(.*)$', line)
        if not match or match[1] not in allowed:
            continue
        key, value = match.groups()
        if key in values:
            fail('路径/归属配置重复，先人工核对：' + key)
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if any(x in value for x in ('$', '`')) or any(ord(x)<32 or ord(x)==127 for x in value):
            fail('路径/归属配置含动态表达式，不能安全推导：' + key)
        values[key] = value
    return values


def node(path):
    s = path.lstat()
    return [s.st_dev, s.st_ino, stat.S_IFMT(s.st_mode)]


def mount_paths():
    source = Path('/proc/self/mountinfo')
    if not source.exists():
        return []
    def unescape(text):
        return re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), text)
    return [Path(unescape(line.split()[4])) for line in source.read_text().splitlines()]


def scan_tree(root):
    if not root.exists():
        return []
    canonical(str(root))
    mounts = mount_paths()
    if any(inside(m, root) for m in mounts) or os.path.ismount(root):
        fail('清理目录含宿主挂载点，先人工卸载并重新盘点：' + str(root))
    entries = []
    root_dev = root.stat().st_dev
    def count(path):
        if path.name in ('.git', '.ai', '.agents'):
            fail('发现嵌套源码/工作区，拒绝清理：' + str(path))
        s = path.lstat()
        if s.st_dev != root_dev:
            fail('拒绝跨文件系统清理：' + str(path))
        if stat.S_ISLNK(s.st_mode):
            return 1, 0  # 只删除链接本身，绝不进入链接目标。
        if stat.S_ISDIR(s.st_mode):
            files = size = 0
            for child in path.iterdir():
                n, b = count(child)
                files += n
                size += b
            return files, size
        # Unix socket 可以由 Agent 产生；不打开、不跟随，最终只 unlink。
        if stat.S_ISREG(s.st_mode) or stat.S_ISSOCK(s.st_mode):
            return 1, s.st_size
        fail('发现设备/FIFO 等非预期产物：' + str(path))
    for child in sorted(root.iterdir()):
        files, size = count(child)
        entries.append({'path':str(child), 'files':files, 'bytes':size,
                        'kind':'directory' if child.is_dir() and not child.is_symlink() else 'file/link'})
    return entries


def docker(*args, as_json=True):
    try:
        result = subprocess.run(['docker', *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, timeout=90)
    except FileNotFoundError:
        fail('Docker 命令不存在；不能确认残留容器/卷，不允许仅删文件冒充重置')
    except subprocess.TimeoutExpired:
        fail('Docker 命令超时：' + ' '.join(args[:2]))
    if result.returncode:
        # Docker stderr 可能含容器配置或认证材料，不原样转存。
        fail('Docker 命令失败：' + ' '.join(args[:2]) + '；检查 daemon、权限及资源占用后重新盘点')
    try:
        return json.loads(result.stdout) if as_json else result.stdout.split()
    except ValueError:
        fail('Docker 返回的元数据格式无效：' + ' '.join(args[:2]))


def inspect_all(kind):
    identifiers = docker(kind, 'ls', '-aq' if kind == 'container' else '-q', '--no-trunc', as_json=False) if kind != 'volume' else docker('volume', 'ls', '-q', as_json=False)
    items = []
    for start in range(0, len(identifiers), 100):
        items.extend(docker(kind, 'inspect', *identifiers[start:start + 100]))
    return items


# 用途：提取清理归属元数据，避免输出容器环境中的凭据。
# 参数：c 为 Docker 容器检查结果。
# 返回值：包含资源身份、挂载和候选标识的脱敏字典。
def container_view(c):
    labels = (c.get('Config') or {}).get('Labels') or {}
    return {'id':c['Id'], 'name':c.get('Name', '').lstrip('/'), 'image':c.get('Image', ''),
            'candidate': c.get('Name', '').lstrip('/') in CANDIDATE_NAMES or
                         (labels.get('com.basic-platform.discovery') == 'v1' and
                          labels.get('com.basic-platform.environment') == 'prod' and
                          labels.get('com.basic-platform.service_role') == 'business' and
                          not labels.get(PREFIX+'project')),
            'created':c.get('Created', ''),
            'project':labels.get(PREFIX+'project', ''),
            'service':labels.get(PREFIX+'service', ''),
            'working_dir':labels.get(PREFIX+'project.working_dir', ''),
            'config_files':labels.get(PREFIX+'project.config_files', ''),
            'mounts':[{k:m[k] for k in ('Type','Name','Source','Destination','RW') if k in m}
                      for m in c.get('Mounts', [])]}


def bound_container(c, target):
    working = c['working_dir']
    configs = c['config_files'].split(',') if c['config_files'] else []
    return (working == str(target) and
            all(Path(x).is_absolute() and '..' not in Path(x).parts and inside(Path(x), target) for x in configs))


# 用途：从受保护的控制文件读取计划或恢复记录。
# 参数：path 为当前执行用户拥有的单链接、0600 普通文件路径。
# 返回值：文件原始字节；权限或类型不满足时拒绝。
def read_control(path):
    return read_regular(path, control=True)


# 用途：验证失败记录并恢复原先审核通过的资源身份。
# 参数：path 为执行记录路径，target 为本次安装目录。
# 返回值：原审核范围及记录摘要；记录不可信或不匹配时拒绝。
def parse_orphan_since(value):
    if not isinstance(value, str) or not value.strip():
        fail('孤立卷时间窗必须是非空的 ISO8601 时间')
    try:
        normalized = value.strip().replace('Z', '+00:00')
        # Docker 的 CreatedAt 可能是 RFC3339Nano（9 位小数）；Python 3.9 的
        # fromisoformat 只接受 3/6 位小数，保留微秒精度后再解析。
        normalized = re.sub(r'(\.\d{6})\d+', r'\1', normalized)
        parsed = datetime.fromisoformat(normalized)
    except ValueError as exc:
        raise Refuse('孤立卷时间窗不是合法 ISO8601 时间') from exc
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


# 用途：为“项目标签正确、但当前容器已不再引用”的孤立卷提供人工确认证据。
# 参数：volume 为 Docker 卷元数据，since 为 CreatedAt 下界。
# 返回值：(是否满足自动纳入条件, 拒绝原因)；空目录只表示当前没有数据，不证明历史用途。
def orphan_volume_evidence(volume, since):
    if volume.get('Driver') != 'local' or volume.get('Options'):
        return False, '数据卷使用非 local 驱动或挂载选项'
    created_raw = volume.get('CreatedAt')
    if not created_raw:
        return False, '数据卷缺少 CreatedAt，无法绑定时间窗'
    try:
        created = parse_orphan_since(created_raw)
    except Refuse:
        return False, '数据卷 CreatedAt 不是合法 ISO8601 时间'
    if since is not None and created < since:
        return False, '数据卷 CreatedAt 早于人工确认时间窗'
    mountpoint = volume.get('Mountpoint') or ''
    directory = Path(mountpoint)
    if not mountpoint or not directory.is_absolute() or directory.is_symlink() or not directory.is_dir():
        return False, '数据卷目录不存在、不是绝对路径或为符号链接'
    try:
        with os.scandir(directory) as entries:
            if next(entries, None) is not None:
                return False, '数据卷目录非空；不能按“空卷”自动纳入'
    except OSError as exc:
        return False, '无法读取数据卷目录：'+type(exc).__name__
    return True, ''


def recovery_scope(path, target):
    raw = read_control(canonical(path))
    record = load_json(raw)
    if not isinstance(record, dict):
        fail('恢复记录必须是 JSON 对象')
    if not isinstance(record.get('scope'), dict) or 'version' not in record.get('scope', {}):
        fail('该记录缺少 v2 scope/version，可能是旧版 v1 清理记录；v1 记录没有 v2 资源指纹，不能作为自动清理授权。请先在安装目录外阅读盘点结果，人工确认孤立卷/容器归属；需要自动清理时用新版本重新首装或单独维护。')
    scope = record.get('scope', {})
    if (record.get('status') not in ('FAILED', 'RUNNING') or
            record.get('target') != str(target) or scope.get('target') != str(target) or
            scope.get('version') != PLAN_VERSION or scope.get('blockers') or
            record.get('checksum') != 'sha256:'+digest(
                {k:v for k,v in record.items() if k != 'checksum'}) or
            scope.get('fingerprint') != fingerprint(scope)):
        fail('恢复记录不属于本目标的未完成v2清理，或摘要无效')
    return scope, hashlib.sha256(raw).hexdigest()


# 用途：统一卷身份表示，保证计划、恢复和删除阶段使用相同证据。
# 参数：volume 为 Docker 数据卷元数据。
# 返回值：包含完整标签、创建时间及驱动选项的身份字典。
def volume_identity(volume):
    view = {k:volume.get(k) for k in ('Name','Mountpoint','Driver','CreatedAt','Options')}
    view['Labels'] = volume.get('Labels') or {}
    if not volume.get('CreatedAt'):
        fail('数据卷缺少创建时间，无法绑定身份：'+volume['Name'])
    directory = canonical(volume.get('Mountpoint') or '')
    if not directory.is_dir():
        fail('数据卷目录不存在或不可访问，无法绑定身份：'+volume['Name'])
    view['DirectoryIdentity'] = node(directory)
    return view


# 用途：统一网络身份表示，兼容 Docker 返回的空标签形式。
# 参数：network 为 Docker 网络元数据。
# 返回值：网络 ID、名称和规范化标签字典。
def network_identity(network):
    return {'Id':network.get('Id'), 'Name':network.get('Name'),
            'Labels':network.get('Labels') or {}}


# 用途：校验中断标记，避免容器引用丢失后普通盘点绕过恢复范围。
# 参数：target 为安装目录，expected 为原执行记录路径或空值。
# 返回值：标记字典或空字典；不匹配时拒绝。
def incomplete_reset(target, expected):
    marker = target / '.first-install-reset.json'
    if not marker.exists() and not marker.is_symlink():
        return {}
    value = load_json(read_control(marker))
    if (not isinstance(value, dict) or value.get('version') != PLAN_VERSION or
            value.get('target') != str(target) or value.get('record') != expected):
        fail('发现未完成清理标记；必须以 --resume-from 引用原执行记录后重新审核')
    return value


# 用途：恢复明确审核过的资源，防止容器删除后丢失孤立资源归属。
# 参数：plan 为新盘点，scope 为原审核范围，volumes/networks 为当前资源。
# 返回值：无；身份变化加入阻塞，已删除的原资源无需再次处理。
def restore_reviewed_resources(plan, scope, volumes, networks):
    for kind, actuals, key, identity in (
            ('volumes', volumes, 'Name', volume_identity),
            ('networks', networks, 'Id', network_identity)):
        by_id = {item[key]:item for item in actuals}
        for category in (kind, 'unlabeled_'+kind):
            selected = {item[key] for item in plan[category]}
            for old in scope.get(category, []):
                if (kind == 'volumes' and old['Name'] == 'uip-offline-registry-data' or
                        kind == 'networks' and old['Name'] in
                        ('basic-platform-production', 'bridge', 'host', 'none')):
                    plan['blockers'].append('恢复范围包含固定共享资源，拒绝：'+old['Name'])
                    continue
                actual = by_id.get(old[key])
                # 网络 ID 不复用；同名不同 ID 也不能被误认为已删除。
                if actual is None and kind == 'networks':
                    actual = next((n for n in actuals if n.get('Name') == old['Name']), None)
                if actual is None:
                    continue
                expected = {k:old.get(k) for k in identity(actual)}
                if identity(actual) != expected:
                    plan['blockers'].append('原审核资源身份变化，拒绝恢复：'+old['Name'])
                elif old[key] not in selected:
                    plan[category].append(old)
                    selected.add(old[key])


# 用途：盘点失败首装及明确恢复范围，遇到不确定归属时阻断。
# 参数：target 为安装目录，requested_project 为原项目名，其余为审核选项。
# 返回值：脱敏计划；blockers 非空时不可执行。
def discover(target, requested_project=None, allow_unlabeled_resources=False,
             resume_from=None, active_record=None, active_scope=None,
             confirm_orphan_volumes=False, orphan_volume_since=None):
    if confirm_orphan_volumes and orphan_volume_since is None:
        fail('确认孤立卷必须提供 CreatedAt 时间窗')
    target = safe_target(str(target))
    scope, recovery_digest = recovery_scope(resume_from, target) if resume_from else ({}, '')
    if active_scope is not None:
        scope = active_scope
    state_dir = Path(str(target)+'.installation-state')
    history = Path(str(target)+'.installation-history')
    plan = {'version':PLAN_VERSION, 'created_at':now(), 'target':str(target), 'requested_project':requested_project,
            'resume_from':resume_from or '', 'reset_origin':scope.get('reset_origin') or resume_from or '',
            'candidate_containers':[],
            'project':'', 'filesystem':[], 'roots':[], 'containers':[], 'volumes':[],
            'unlabeled_volumes':[], 'networks':[], 'unlabeled_networks':[],
            'options':{'allow_unlabeled_resources':bool(allow_unlabeled_resources),
                       'confirm_orphan_volumes':bool(confirm_orphan_volumes),
                       'orphan_volume_since':orphan_volume_since.isoformat().replace('+00:00','Z') if orphan_volume_since else '',
                       'confirmed_orphan_volumes':[]},
            'preserved':[], 'blockers':[], 'proof':{}, 'daemon':{}}
    def block(message):
        plan['blockers'].append(message)
    try:
        incomplete_reset(target, plan['reset_origin'] or active_record)
    except Refuse as e:
        block(str(e))
    if resume_from:
        if any(inside(Path(resume_from), r) for r in (target, state_dir, history)):
            fail('恢复记录必须位于待删除目录之外')
        plan['proof']['recovery-record'] = recovery_digest
    for root in (target, state_dir, history):
        canonical(str(root))
        if root.exists():
            if not root.is_dir():
                fail('安装产物目录类型异常：' + str(root))
            plan['roots'].append({'path':str(root), 'identity':node(root)})
            plan['filesystem'].append({'path':str(root), 'entries':scan_tree(root)})
            if scope:
                original = next((r for r in scope['roots'] if r['path'] == str(root)), None)
                if not original or node(root) != original['identity']:
                    block('恢复目标目录身份变化，不能沿用原清理范围：'+str(root))
    if history.exists() and any(history.iterdir()):
        block('发现历史安装批次；此工具只重置失败首装，不能清理升级/增装环境')
    # 合法恢复范围已在上方核验现存根 inode；部分删除不能要求部署标识仍完整。
    marker = bool(scope) or all((target / x).is_file() and not (target / x).is_symlink()
                               for x in ('bin/deploy.sh', 'docker-compose.yml'))
    session_path = state_dir / 'session.json'
    fresh_session = False
    if state_dir.exists():
        for entry in state_dir.iterdir():
            if entry.name not in ('.lock', 'session.json') and not re.fullmatch(r'\.session-[a-f0-9]{32}\.tmp', entry.name):
                block('安装会话目录含未登记文件，先人工核对：'+str(entry))
    if session_path.exists():
        try:
            session = load_json(read_regular(session_path))
            identity = session['identity']
            if session.get('checksum') != 'sha256:'+digest({k:v for k,v in session.items() if k != 'checksum'}) or session.get('identity_digest') != 'sha256:'+digest(identity):
                fail('安装会话摘要不一致')
            if identity['target'] != str(target) or identity['mode'] != 'install':
                fail('安装会话不属于此目标的首装')
            if session.get('advisory', {}).get('action') == 'VERIFY_REAL_INSTALLATION':
                fail('会话显示部署已完成，不能作为失败首装清空')
            for value in (identity['media'], identity.get('options', {}).get('license_file_path', '')):
                if not value:
                    continue
                p = Path(value)
                if not p.is_absolute() or any(overlap(p, r) for r in (target, state_dir, history)):
                    fail('原始交付介质或输入许可证与删除目录重叠')
                plan['preserved'].append({'kind':'input', 'path':value, 'reason':'安装会话声明的原始输入'})
            marker = True
            fresh_session = True
        except (Refuse, ValueError, KeyError, TypeError) as e:
            block('会话不能证明失败首装归属：' + (str(e) if isinstance(e, Refuse) else '格式无效'))
    if (target / 'backups/system').exists() and any((target / 'backups/system').iterdir()) and not fresh_session:
        block('发现系统备份且没有可验证的未完成首装会话，按已有业务环境处理；不能使用失败首装重置')
    if target.exists() and any(target.iterdir()) and not marker:
        block('没有首装会话或完整部署标识；不能证明该目录可整目录删除')
    for relative in ('.env', '.release.env', 'docker-compose.yml', 'compose.yaml'):
        p = target / relative
        if p.exists() or p.is_symlink():
            try:
                plan['proof'][relative] = hashlib.sha256(read_regular(p)).hexdigest()
            except Refuse as e:
                block(str(e))
    if session_path.exists():
        plan['proof']['session.json'] = hashlib.sha256(read_regular(session_path)).hexdigest()
    if scope:
        for key, value in scope['proof'].items():
            if key != 'recovery-record' and key in plan['proof'] and plan['proof'][key] != value:
                block('恢复目标控制文件变化，不能沿用原清理范围：'+key)
    try:
        env = env_values(target / '.env')
        release_env = env_values(target / '.release.env')
        if release_env.get('COMPOSE_PROJECT_NAME') and release_env['COMPOSE_PROJECT_NAME'] != env.get('COMPOSE_PROJECT_NAME'):
            block('.release.env 覆盖了 Compose 项目名，先人工核对实际项目归属')
    except Refuse as e:
        block(str(e))
        env = {}
    project = env.get('COMPOSE_PROJECT_NAME') or requested_project or scope.get('project', '')
    if scope and (project != scope['project'] or
                  allow_unlabeled_resources and not scope['options']['allow_unlabeled_resources']):
        block('恢复时不得更换原项目或扩大未登记资源授权')
    if scope:
        allow_unlabeled_resources = scope['options']['allow_unlabeled_resources']
        plan['options']['allow_unlabeled_resources'] = allow_unlabeled_resources
    if requested_project and env.get('COMPOSE_PROJECT_NAME') and requested_project != env['COMPOSE_PROJECT_NAME']:
        block('--project 与安装目录中的 COMPOSE_PROJECT_NAME 冲突')
    if project and not re.fullmatch(r'[a-z0-9][a-z0-9_-]{0,62}', project):
        block('Compose 项目名格式无效')
    for key in ('PLATFORM_KEYS_DIR', 'FILE_GATEWAY_HOST_ROOT', 'SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT',
                'SUBSYSTEM_PRODUCTION_PROFILES_DIR', 'UIP_IMPORT_TMPDIR'):
        if env.get(key):
            p = Path(env[key])
            p = p if p.is_absolute() else target / p
            if '..' in p.parts or not inside(p, target):
                block(key+' 指向安装目录之外或含路径逃逸：'+str(p)+'；须单独核对，脚本不删除')
            else:
                try:
                    canonical(str(p))
                except Refuse as e:
                    block(str(e))
    ports = sorted({env[k]+'/tcp' for k in ('PUBLIC_HTTP_PORT', 'PUBLIC_HTTPS_PORT', 'KEYCLOAK_HTTP_PORT')
                    if re.fullmatch(r'[0-9]{1,5}', env.get(k, ''))})
    if ports:
        plan['preserved'].append({'kind':'host-firewall', 'path':', '.join(ports),
                                  'reason':'配置声明的入口端口；未记录安装前规则，不自动删除防火墙放行项'})
    plan['preserved'].extend([
        {'kind':'lock', 'path':str(target)+'.installation-run.lock',
         'reason':'保留0600空互斥锁 inode，避免新安装与清理使用两个不同锁；不影响重新首装'},
        {'kind':'host', 'path':'/var/lib/dpkg /var/lib/apt /etc/docker /etc/ufw /etc/firewalld',
         'reason':'系统依赖、Docker安装/配置和防火墙规则无本次部署前基线，不自动回滚'},
        {'kind':'temporary', 'path':'/tmp/uip-* /var/tmp/uip-host-dependencies.* <介质父目录>/.uip-delivery-extract.*',
         'reason':'可能是其他任务或交付介质；只列模式，不按通配符删除'},
        {'kind':'images', 'path':'Docker daemon 镜像缓存',
         'reason':'保留全部镜像和标签；无本次导入前基线，禁止全局 prune；重新部署可复用'}])
    manifests = target / 'manifests'
    if manifests.is_dir() and not manifests.is_symlink():
        for imported in sorted(manifests.glob('*.imported')):
            if imported.is_symlink():
                continue
            images = []
            for line in read_regular(imported, 1024*1024).decode('utf-8').splitlines():
                key, sep, value = line.partition('=')
                if sep and (key == 'SOURCE_IMAGE_IDS' or key == 'IMAGE_REF' or key.endswith('_IMAGE_REF')):
                    if re.fullmatch(r'[A-Za-z0-9._:/@,-]{1,16384}', value):
                        images.append(key+'='+value)
            if images:
                plan['preserved'].append({'kind':'imported-images', 'path':str(imported),
                                          'reason':'仅保留记录中的 Docker 镜像：'+'; '.join(images)+'；记录文件随安装目录删除'})
    try:
        contexts = docker('context', 'inspect')
        endpoint = contexts[0]['Endpoints']['docker']['Host']
        if not os.environ.get('DOCKER_CONTEXT'):
            endpoint = os.environ.get('DOCKER_HOST') or endpoint
        if not endpoint.startswith('unix://'):
            fail('只允许本机 Unix socket Docker daemon，拒绝远程 context/DOCKER_HOST')
        info = docker('info', '--format', '{{json .}}')
        if not info.get('ID') or not info.get('DockerRootDir'):
            fail('Docker 未返回 daemon ID 或数据根目录')
        plan['daemon'] = {'id':info['ID'], 'root':info['DockerRootDir'], 'endpoint':endpoint}
        if scope and plan['daemon'] != scope['daemon']:
            block('恢复记录的Docker daemon与当前不一致')
        if any(overlap(Path(info['DockerRootDir']), r) for r in (target, state_dir, history)):
            block('删除范围与 Docker 数据根目录重叠，拒绝操作')
        containers = [container_view(c) for c in inspect_all('container')]
        plan['candidate_containers'] = [c for c in containers if c['candidate']]
        for c in plan['candidate_containers']:
            block('发现候选/发现容器，缺少安装路径归属证据，须单独核对处理后重新盘点：'+c['name'])
        inferred = {c['project'] for c in containers if bound_container(c, target) and c['project']}
        if not project and len(inferred) == 1:
            project = inferred.pop()
        elif not project and len(inferred) > 1:
            block('同一目录对应多个 Compose 项目，须先人工核对')
        plan['project'] = project
        if not project:
            block('无法确定 Compose 项目；.env 丢失且无容器证据时，请核对后显式 --project <原项目名>')
        owned = [c for c in containers if c['project'] == project and bound_container(c, target)
                 and c['name'] != 'uip-offline-registry'] if project else []
        ids = {c['id'] for c in owned}
        containers_by_id = {c['id']: c for c in containers}
        for c in containers:
            if bound_container(c, target) and c['project'] != project:
                block('本安装目录还绑定了其他 Compose 项目：'+c['name']+' → '+c['project'])
            if c['project'] == project and c['id'] not in ids:
                block('同名项目存在非本安装目录的容器：'+c['name'])
            for m in c['mounts']:
                if m.get('Type') != 'bind':
                    continue
                source = Path(m.get('Source', ''))
                roots = (target, state_dir, history)
                if c['id'] not in ids and source.is_absolute() and any(overlap(source, r) for r in roots):
                    block('其他容器挂载了待删除路径：'+c['name']+' → '+str(source))
                if c['id'] in ids and not inside(source, target):
                    if str(source) not in ('/var/run/docker.sock', '/run/docker.sock', '/etc/localtime', '/etc/timezone'):
                        block('本安装容器使用外部 bind 路径：'+c['name']+' → '+str(source)+'；不自动删除')
        plan['containers'] = sorted(owned, key=lambda x:x['id'])
        referenced = {m['Name'] for c in owned for m in c['mounts'] if m.get('Type') == 'volume' and m.get('Name')}
        volumes = inspect_all('volume')
        networks = inspect_all('network')
        prior_volumes = {v['Name'] for v in scope.get('volumes', [])}
        prior_networks = {n['Id'] for n in scope.get('networks', [])}
        for v in volumes:
            if v['Name'] == 'uip-offline-registry-data':
                plan['preserved'].append({'kind':'shared-volume', 'name':v['Name'],
                                          'path':v.get('Mountpoint', ''),
                                          'reason':'固定共享Registry卷，不属于单次失败首装'})
                if v['Name'] in referenced:
                    block('本安装引用固定共享Registry卷，拒绝清理：'+v['Name'])
                continue
            labels = v.get('Labels') or {}
            ours = bool(project and labels.get(PREFIX+'project') == project and labels.get(PREFIX+'volume'))
            if ours:
                users = [c['name'] for c in containers if c['id'] not in ids and
                         any(m.get('Type') == 'volume' and m.get('Name') == v['Name'] for m in c['mounts'])]
                if users:
                    block('数据卷被其他容器共用：'+v['Name']+' → '+','.join(users))
                    continue
                if v.get('Driver') != 'local' or v.get('Options'):
                    block('数据卷使用插件/外部挂载选项，不能视为普通首装本机卷：'+v['Name'])
                    continue
                if v['Name'] not in referenced and v['Name'] not in prior_volumes:
                    if not confirm_orphan_volumes:
                        block('孤立卷只有项目名，无法证明属于本安装；需原失败执行记录 --resume-from，或人工确认时间窗后使用 --confirm-orphan-volumes：'+v['Name'])
                        continue
                    ok, reason = orphan_volume_evidence(v, orphan_volume_since)
                    if not ok:
                        block('孤立卷不满足人工确认条件：'+v['Name']+'；'+reason)
                        continue
                    plan['volumes'].append(volume_identity(v))
                    plan['options']['confirmed_orphan_volumes'].append(v['Name'])
                else:
                    plan['volumes'].append(volume_identity(v))
            elif v['Name'] in referenced:
                users = [c['name'] for c in containers if c['id'] not in ids and
                         any(m.get('Type') == 'volume' and m.get('Name') == v['Name'] for m in c['mounts'])]
                if users:
                    block('本安装挂载的数据卷被其他容器共用：'+v['Name']+' → '+','.join(users))
                elif v.get('Driver') != 'local' or v.get('Options'):
                    block('本安装挂载的卷使用插件/外部挂载选项：'+v['Name'])
                elif labels:
                    block('卷带有其他归属/自定义标签，不能当作无标签资源：'+v['Name'])
                elif allow_unlabeled_resources:
                    view = volume_identity(v)
                    view['reason'] = '被本安装容器引用但缺少 Compose 卷归属标签（可能由旧版/手工 Compose 创建）'
                    plan['unlabeled_volumes'].append(view)
                else:
                    block('本安装挂载的卷缺少项目归属或属于外部项目：'+v['Name']+
                          '；如确认由本次失败首装产生，可加 --allow-unlabeled-resources 重新盘点并审核')
        for n in networks:
            labels = n.get('Labels') or {}
            attached = set(n.get('Containers') or {})
            if project and labels.get(PREFIX+'project') == project and n.get('Name') != 'basic-platform-production':
                if not attached & ids and n['Id'] not in prior_networks:
                    block('孤立网络只有项目名，无法证明属于本安装；需原失败执行记录 --resume-from：'+n['Name'])
                if attached - ids:
                    block('项目网络连接着其他容器：'+n['Name'])
                plan['networks'].append(network_identity(n))
            elif n.get('Name') == 'basic-platform-production':
                plan['preserved'].append({'kind':'shared-network', 'name':n['Name'],
                                          'reason':'生产编排声明 external，共享且没有本次创建记录'})
            elif attached & ids and n.get('Name') not in ('bridge', 'host', 'none'):
                others = [containers_by_id[cid]['name'] for cid in sorted(attached - ids) if cid in containers_by_id]
                if attached - ids:
                    block('本安装容器还连接着其他容器在用的共享网络：'+n['Name']+' → '+','.join(others))
                elif labels:
                    block('网络带有其他归属/自定义标签，不能当作无标签资源：'+n['Name'])
                elif allow_unlabeled_resources:
                    plan['unlabeled_networks'].append({'Id':n.get('Id'), 'Name':n.get('Name'), 'Labels':labels,
                                                       'reason':'被本安装容器连接但缺少 Compose 网络归属标签（可能由旧版/手工 Compose 创建）'})
                else:
                    block('本安装容器连接了未登记的 Docker 网络：'+n['Name']+
                          '；如确认由本次失败首装产生，可加 --allow-unlabeled-resources 重新盘点并审核')
        if scope:
            restore_reviewed_resources(plan, scope, volumes, networks)
            selected_volumes = {v['Name'] for key in ('volumes','unlabeled_volumes') for v in plan[key]}
            selected_networks = {n['Id'] for key in ('networks','unlabeled_networks') for n in plan[key]}
            for c in containers:
                if c['id'] not in ids and any(m.get('Name') in selected_volumes
                                             for m in c['mounts'] if m.get('Type') == 'volume'):
                    block('原审核卷已被其他容器引用：'+c['name'])
            for n in networks:
                if n['Id'] in selected_networks and set(n.get('Containers') or {}) - ids:
                    block('原审核网络已被其他容器连接：'+n['Name'])
        for c in containers:
            if c['name'] == 'uip-offline-registry':
                plan['preserved'].append({'kind':'shared-container', 'name':c['name'], 'id':c['id'],
                                          'reason':'全机离线 Registry，不属于单个安装目录'})
    except (Refuse, KeyError, TypeError) as e:
        block(str(e) if isinstance(e, Refuse) else 'Docker 元数据缺少必要字段')
    plan['volumes'].sort(key=lambda x:x['Name'])
    plan['options']['confirmed_orphan_volumes'] = sorted(plan['options']['confirmed_orphan_volumes'])
    plan['networks'].sort(key=lambda x:x['Id'])
    plan['unlabeled_volumes'].sort(key=lambda x:x['Name'])
    plan['unlabeled_networks'].sort(key=lambda x:x['Id'])
    plan['fingerprint'] = fingerprint(plan)
    return plan


# 用途：绑定审核范围，排除允许持续增长的数据内容。
# 参数：plan 为本版本生成的完整清理计划。
# 返回值：资源与控制证据的 SHA256 摘要。
def fingerprint(plan):
    # 数据/日志可继续增长；授权范围由根目录 inode、配置和资源身份绑定，避免日志写入使预览恒失效。
    return digest({k:plan[k] for k in ('version','target','requested_project','project','roots','proof','daemon',
                                     'containers','volumes','unlabeled_volumes','networks','unlabeled_networks',
                                     'options','blockers','resume_from','reset_origin','candidate_containers')})


def save_new(path, value, roots):
    path = canonical(str(path))
    if any(inside(path, r) for r in roots):
        fail('盘点/执行记录必须保存在待删除目录之外：'+str(path))
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8') as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write('\n')
        stream.flush()
        os.fsync(stream.fileno())


# 用途：原子更新执行记录，写入中断时保留上一份完整恢复范围。
# 参数：path 为记录路径，value 为新记录，expected 为原记录身份。
# 返回值：更新后文件身份；写入失败或原记录被替换时拒绝。
def replace_record(path, value, expected):
    if node(path) != expected:
        fail('执行记录身份变化，停止清理')
    descriptor, name = tempfile.mkstemp(prefix='.'+path.name+'.', dir=path.parent)
    temporary = Path(name)
    identity = node(temporary)
    try:
        with os.fdopen(descriptor, 'w', encoding='utf-8') as stream:
            json.dump(value, stream, ensure_ascii=False, indent=2)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
        if node(path) != expected:
            fail('执行记录身份变化，停止清理')
        os.replace(temporary, path)
        return node(path)
    finally:
        if temporary.exists() and node(temporary) == identity:
            temporary.unlink()


def display(plan):
    print('失败首装产物盘点（当前未删除任何资源）')
    print('安装目录：'+plan['target']+'\nCompose项目：'+(plan['project'] or '未确定'))
    if plan['daemon']:
        print('Docker数据根目录：'+plan['daemon']['root'])
    print('\n拟删除的文件目录（包含配置、密钥、上传文件及目录内的备份）：')
    for root in plan['filesystem']:
        print('  [整个目录] '+root['path'])
        for entry in root['entries']:
            print('    {}  文件/链接数={} 大小={} bytes'.format(entry['path'],entry['files'],entry['bytes']))
    print('\n拟停止并删除的容器（包括迁移/一次性容器和已停止的旧容器）：')
    for c in plan['containers']:
        print('  {} {} 归属工作目录={}'.format(c['name'],c['id'],c['working_dir']))
    print('\n拟删除的数据卷（数据库、附件、报告、许可状态等将永久丢失）：')
    for v in plan['volumes']:
        print('  {} → {} 依据=Compose项目+卷标签'.format(v['Name'], v['Mountpoint']))
    confirmed_orphans = plan['options'].get('confirmed_orphan_volumes') or []
    if confirmed_orphans:
        print('\n人工确认纳入的孤立卷（仅项目标签、CreatedAt 在声明时间窗内且当前目录为空）：')
        for name in confirmed_orphans:
            print('  '+name+' 依据=--confirm-orphan-volumes + '+str(plan['options'].get('orphan_volume_since')))
    print('\n拟删除的未登记归属数据卷（仅当加 --allow-unlabeled-resources 且审阅确认由本次失败首装产生）：')
    for v in plan['unlabeled_volumes']:
        print('  {} → {} 依据={}'.format(v['Name'], v['Mountpoint'], v.get('reason','')))
    print('\n拟删除的项目专用网络：')
    for n in plan['networks']:
        print('  '+n['Name']+' '+n['Id'])
    print('\n拟删除的未登记归属项目网络（仅当加 --allow-unlabeled-resources 且审阅确认由本次失败首装产生）：')
    for n in plan['unlabeled_networks']:
        print('  {} {} 依据={}'.format(n['Name'], n['Id'], n.get('reason','')))
    print('\n保留/需单独核对：')
    for item in plan['preserved']:
        print('  {}：{}'.format(item.get('path') or item.get('name'), item['reason']))
    if plan['blockers']:
        print('\n禁止执行，须先解决：')
        for reason in plan['blockers']:
            print('  - '+reason)
    else:
        print('\n可生成待审阅的清理计划；仅限无真实业务数据的失败首装。')


def acquire(path, stack, create=False):
    canonical(str(path))
    if not path.exists() and not create:
        return
    flags = os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK
    fd = os.open(path, flags | (os.O_CREAT if create else 0), 0o600)
    stack.callback(os.close, fd)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size != 0 or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600:
        fail('部署锁不是当前执行用户的0600单链接普通文件：'+str(path))
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        fail('安装/发布/授权任务正在运行，未执行清理：'+str(path))


# 用途：在任何资源变更前核对删除能力和目录操作权限。
# 参数：plan 为审核范围，roots 为待删除目录。
# 返回值：无；缺少能力、所有权或目录权限时拒绝。
def deletion_preflight(plan, roots):
    if not shutil.rmtree.avoids_symlink_attacks:
        fail('当前 Python 不支持防符号链接替换的目录删除；未停止任何容器')
    if not roots[0].exists() and any(plan[k] for k in (
            'containers','volumes','unlabeled_volumes','networks','unlabeled_networks')):
        fail('安装目录缺失，无法持久化资源删除中断门禁；须先单独核对残留资源')
    for item in plan['roots']:
        root = canonical(item['path'])
        if node(root) != item['identity'] or root.stat().st_uid != os.geteuid():
            fail('删除根身份变化或不属于当前执行用户：'+str(root))
        if not os.access(root, os.R_OK | os.W_OK | os.X_OK):
            fail('删除根缺少读取/写入/遍历权限：'+str(root))
        for directory, dirs, _ in os.walk(root, followlinks=False):
            if not os.access(directory, os.R_OK | os.W_OK | os.X_OK):
                fail('子目录缺少删除权限：'+directory)
    for root in roots:
        if not os.access(root.parent, os.W_OK | os.X_OK):
            fail('删除目录的父路径不可写：'+str(root.parent))


# 用途：执行已审阅计划，记录完整恢复范围并在异常时保留失败证据。
# 参数：args 包含计划路径、目标确认和失败首装数据丢弃确认。
# 返回值：无；校验或删除失败抛错，不伪报成功。
def apply_plan(args):
    saved = load_json(read_control(Path(args.apply)))
    if saved.get('version') != PLAN_VERSION:
        fail('仅接受v2清理计划，请重新盘点；旧计划不能直接执行')
    if args.confirm_target != saved.get('target') or not args.discard_fresh_data:
        fail('必须显式 --confirm-target <计划中的完整安装路径> --discard-fresh-data；只允许丢弃失败首装数据')
    target = safe_target(saved['target'])
    roots = [target, Path(str(target)+'.installation-state'), Path(str(target)+'.installation-history')]
    if any(inside(Path(args.apply), root) for root in roots):
        fail('计划文件位于待删除目录内')
    if saved.get('fingerprint') != fingerprint(saved):
        fail('计划字段或摘要被修改，请重新生成计划')
    if saved['blockers']:
        fail('计划有未解决阻塞，不能执行；修复后重新盘点')
    allow_unlabeled = bool(saved.get('options', {}).get('allow_unlabeled_resources'))
    confirm_orphan = bool(saved.get('options', {}).get('confirm_orphan_volumes'))
    orphan_since_raw = saved.get('options', {}).get('orphan_volume_since') or ''
    orphan_since = parse_orphan_since(orphan_since_raw) if confirm_orphan else None
    current = discover(target, saved.get('requested_project'), allow_unlabeled, saved.get('resume_from'),
                       confirm_orphan_volumes=confirm_orphan, orphan_volume_since=orphan_since)
    if current['fingerprint'] != saved['fingerprint']:
        fail('安装配置、目录身份、Docker daemon或资源已变化；请重新盘点并审核新计划')
    deletion_preflight(current, roots)
    record = {'target':str(target), 'plan':str(Path(args.apply)), 'started_at':now(),
              'status':'RUNNING', 'steps':[], 'preserved':current['preserved'], 'scope':current}
    record['checksum'] = 'sha256:'+digest(record)
    result_path = Path(str(args.apply)+'.result-'+now()+'.json')
    save_new(result_path, record, roots)
    print('执行记录：'+str(result_path), flush=True)
    # 用途：持久化本次执行状态并保持恢复记录摘要一致。
    # 参数：无，使用当前执行记录和固定文件身份。
    # 返回值：无；记录被替换或写入失败时停止。
    def persist():
        nonlocal result_identity
        record['checksum'] = 'sha256:'+digest({k:v for k,v in record.items() if k != 'checksum'})
        result_identity = replace_record(result_path, record, result_identity)
    result_identity = node(result_path)
    def step(kind, name, action):
        record['steps'].append({'kind':kind, 'resource':name, 'status':'STARTED'})
        persist()
        action()
        record['steps'][-1]['status'] = 'DONE'
        persist()
        print('已完成 '+kind+' '+name, flush=True)
    try:
        with ExitStack() as stack:
            # 与现有首装长锁、会话短锁及发布锁协调。长锁不 unlink，防止 inode 分裂。
            acquire(Path(str(target)+'.installation-run.lock'), stack, create=True)
            for relative in (str(target)+'.installation-state/.lock', str(target)+'/runtime/.deploy.lock',
                             str(target)+'/runtime/.license-import.lock'):
                acquire(Path(relative), stack)
            current = discover(target, saved.get('requested_project'), allow_unlabeled, saved.get('resume_from'),
                               confirm_orphan_volumes=confirm_orphan, orphan_volume_since=orphan_since)
            if current['blockers'] or current['fingerprint'] != saved['fingerprint']:
                fail('取得锁后资源发生变化，未开始停止容器；请重新盘点')
            deletion_preflight(current, roots)
            marker = target / '.first-install-reset.json'
            if target.exists() and not marker.exists():
                save_new(marker, {'version':PLAN_VERSION, 'target':str(target),
                                 'record':current['reset_origin'] or str(result_path)}, [])
            def priority(c):
                return (0 if c['service'] == 'subsystem-provisioner' else
                        2 if 'mysql' in c['service'] else 1, c['name'])
            for c in sorted(current['containers'], key=priority):
                step('stop-container', c['id'], lambda c=c: docker('container','stop','--time','30',c['id'], as_json=False))
            # 停止 Agent 后复核，避免后台接入在预览后新建资源或增加共享关系。
            fresh = discover(target, saved.get('requested_project'), allow_unlabeled,
                             saved.get('resume_from'), str(result_path), current,
                             confirm_orphan_volumes=confirm_orphan, orphan_volume_since=orphan_since)
            if fresh['blockers'] or fresh['fingerprint'] != current['fingerprint']:
                fail('停止后资源集合变化，保留文件和数据卷；重新盘点后处理')
            for c in current['containers']:
                step('remove-container', c['id'], lambda c=c: docker('container','rm',c['id'], as_json=False))
            for v in current['volumes']:
                actual = docker('volume', 'inspect', v['Name'])[0]
                view = volume_identity(actual)
                if view != v:
                    fail('数据卷身份或挂载选项发生变化，停止：'+v['Name'])
                # Docker rm volume 自身也拒绝删除仍被任意容器引用的卷；绝不使用 --force。
                step('remove-volume', v['Name'], lambda v=v: docker('volume','rm',v['Name'], as_json=False))
            for v in current['unlabeled_volumes']:
                actual = docker('volume', 'inspect', v['Name'])[0]
                view = volume_identity(actual)
                if view != {k:v.get(k) for k in view} or (actual.get('Labels') or {}):
                    fail('未登记归属数据卷身份或标签发生变化，停止：'+v['Name'])
                if any(m.get('Type') == 'volume' and m.get('Name') == v['Name']
                       for c in inspect_all('container') for m in container_view(c)['mounts']):
                    fail('未登记归属数据卷仍被容器引用，停止：'+v['Name'])
                step('remove-unlabeled-volume', v['Name'],
                     lambda v=v: docker('volume','rm',v['Name'], as_json=False))
            for n in current['networks']:
                step('remove-network', n['Id'], lambda n=n: docker('network','rm',n['Id'], as_json=False))
            for n in current['unlabeled_networks']:
                actual = docker('network', 'inspect', n['Id'])[0]
                if actual.get('Name') != n['Name'] or (actual.get('Labels') or {}) != (n.get('Labels') or {}):
                    fail('未登记归属网络身份或标签发生变化，停止：'+n['Name'])
                if actual.get('Containers'):
                    fail('未登记归属网络仍连接容器，停止：'+n['Name'])
                step('remove-unlabeled-network', n['Id'],
                     lambda n=n: docker('network','rm',n['Id'], as_json=False))
            if not shutil.rmtree.avoids_symlink_attacks:
                fail('当前 Python 不支持防符号链接替换的目录删除；已保留安装目录')
            for c in [container_view(c) for c in inspect_all('container')]:
                if c['candidate'] or c['project'] == current['project'] or any(
                    m.get('Type') == 'bind' and any(overlap(Path(m.get('Source','')), r) for r in roots)
                    for m in c['mounts']):
                    fail('删除文件前发现新建/残留容器或共享挂载，停止：'+c['name'])
            for root in current['roots']:
                p = canonical(root['path'])
                if node(p) != root['identity']:
                    fail('待删除目录 inode 变化，停止文件清理：'+str(p))
                scan_tree(p)
                step('remove-directory', str(p), lambda p=p: shutil.rmtree(p))
            # 核验真实剩余资源，不以执行过 rm 作为成功证据。
            remaining = [c for c in inspect_all('container')
                         if container_view(c)['candidate'] or
                         ((c.get('Config') or {}).get('Labels') or {}).get(PREFIX+'project') == current['project']]
            remaining_volumes = [v for v in inspect_all('volume')
                                 if v['Name'] != 'uip-offline-registry-data' and
                                 (v.get('Labels') or {}).get(PREFIX+'project') == current['project']]
            remaining_networks = [n for n in inspect_all('network')
                                  if (n.get('Labels') or {}).get(PREFIX+'project') == current['project']
                                  and n.get('Name') != 'basic-platform-production']
            unlabeled_volume_names = {x['Name'] for x in current['unlabeled_volumes']}
            unlabeled_network_ids = {x['Id'] for x in current['unlabeled_networks']}
            remaining_unlabeled_volumes = [v['Name'] for v in inspect_all('volume')
                                           if v['Name'] in unlabeled_volume_names]
            remaining_unlabeled_networks = [n.get('Id') for n in inspect_all('network')
                                            if n.get('Id') in unlabeled_network_ids]
            lock_path = Path(str(target)+'.installation-run.lock')
            reinstall_ready = (not remaining and not remaining_volumes and not remaining_networks and
                               not remaining_unlabeled_volumes and not remaining_unlabeled_networks and
                               not any(r.exists() for r in roots) and
                               lock_path.is_file() and not lock_path.is_symlink() and
                               lock_path.stat().st_size == 0)
            record['reinstall_ready'] = reinstall_ready
            record['remaining'] = {'containers':[c['name'] for c in remaining],
                                   'volumes':[v['Name'] for v in remaining_volumes],
                                   'networks':[n['Name'] for n in remaining_networks],
                                   'unlabeled_volumes':remaining_unlabeled_volumes,
                                   'unlabeled_networks':remaining_unlabeled_networks,
                                   'roots':[str(r) for r in roots if r.exists()],
                                   'installation_lock_safe': lock_path.is_file() and not lock_path.is_symlink() and lock_path.stat().st_size == 0}
            if not reinstall_ready:
                fail('发现残留目录、容器、数据卷或网络；未宣告重置成功，请重新盘点并审核剩余产物')
            record['status'] = 'COMPLETED'
            record['finished_at'] = now()
            persist()
    except BaseException as e:
        record['status'] = 'FAILED'
        record['error'] = str(e) if isinstance(e, (Refuse, OSError)) else type(e).__name__
        persist()
        raise
    print('失败首装已重置。交付介质、镜像缓存、共享Registry/网络、宿主依赖和互斥锁保留。')
    print('重新首装前置检查：通过（本次审核范围已重置；安装长锁保留且可复用）。')
    print('未证明宿主机所有无标签孤立资源均不存在；本工具不是生产卸载或宿主清空。')
    print('再次部署：bash 服务器一键离线部署.sh --media <原交付介质路径> --target '+str(target)+' --install')
    print('重新首装会生成新密钥和实例身份；已签发商业许可证可能需要重新申请。')


# 用途：解析盘点、恢复与执行参数，保持显式审核和确认边界。
# 参数：无，读取命令行参数。
# 返回值：无；盘点存在阻塞退出2，执行错误退出1。
def main():
    parser = argparse.ArgumentParser(prog=SCRIPT.name, allow_abbrev=False,
        description='只清理失败首装产生的产物；默认只读盘点，绝不执行全局 Docker prune。',
        epilog='先 --target /opt/unified-identity-platform --plan-file /root/uip-reset.json；审核后 '
               '--apply /root/uip-reset.json --confirm-target /opt/unified-identity-platform --discard-fresh-data。'
               '不适用于已有业务、升级或增装。备份/诊断材料请先保存到安装目录之外。')
    parser.add_argument('--target', default=DEFAULT_TARGET, help='服务器安装目录（默认 %(default)s）')
    parser.add_argument('--project', help='.env 丢失且无法自动确定时，显式指定原 Compose 项目名；不可凭容器名前缀猜测')
    parser.add_argument('--allow-unlabeled-resources', action='store_true',
        help='仅在生成计划时使用：把缺少 Compose 归属标签、但只被本失败首装容器引用的数据卷/网络纳入删除计划；须逐项审阅并由操作者确认归属')
    parser.add_argument('--confirm-orphan-volumes', action='store_true',
        help='仅与 --plan-file 和 --orphan-volume-since 一起使用：把项目标签正确、当前不再被容器引用、CreatedAt 在时间窗内且目录为空的孤立卷纳入计划；仍需审阅后 --apply')
    parser.add_argument('--orphan-volume-since', metavar='ISO8601',
        help='人工确认的孤立卷 CreatedAt 下界；必须与 --confirm-orphan-volumes 同时提供')
    parser.add_argument('--plan-file', help='将完整路径/资源清单写入新的0600 JSON文件；禁止覆盖已有文件')
    parser.add_argument('--resume-from', metavar='RESULT_JSON',
        help='生成计划时引用本目标未完成的v2执行记录，恢复容器删除后孤立卷/网络的身份依据；仍须新计划审核')
    parser.add_argument('--format', choices=('human','json'), default='human')
    parser.add_argument('--apply', metavar='PLAN_JSON', help='重新核验并执行已审阅计划；此参数才允许删除')
    parser.add_argument('--confirm-target', help='必须与计划安装路径完全一致')
    parser.add_argument('--discard-fresh-data', action='store_true', help='明确确认目标是可丢弃数据的失败首装，包括数据库、文件、密钥、目录内备份和许可身份')
    args = parser.parse_args(sys.argv[2:])
    if args.apply:
        if args.resume_from or args.plan_file or args.project or args.allow_unlabeled_resources or args.confirm_orphan_volumes or args.orphan_volume_since or args.target != DEFAULT_TARGET or args.format != 'human':
            fail('--apply 不接受 --target/--project/--allow-unlabeled-resources/--confirm-orphan-volumes/--orphan-volume-since/--plan-file/--format；以计划的已审核范围为准')
        apply_plan(args)
        return
    if args.confirm_target or args.discard_fresh_data:
        fail('确认参数只能与 --apply 一起使用；默认操作只盘点')
    if args.allow_unlabeled_resources and not args.plan_file:
        fail('--allow-unlabeled-resources 只能与 --plan-file 一起使用；必须先审阅未登记归属的资源清单')
    if args.resume_from and not args.plan_file:
        fail('--resume-from 只能与新的 --plan-file 一起使用')
    if args.confirm_orphan_volumes:
        if not args.plan_file:
            fail('--confirm-orphan-volumes 只能与 --plan-file 一起使用；必须先审阅孤立卷计划')
        if not args.orphan_volume_since:
            fail('--confirm-orphan-volumes 必须同时提供 --orphan-volume-since 时间窗')
        orphan_since = parse_orphan_since(args.orphan_volume_since)
    else:
        if args.orphan_volume_since:
            fail('--orphan-volume-since 只能与 --confirm-orphan-volumes 一起使用')
        orphan_since = None
    plan = discover(Path(args.target), args.project, args.allow_unlabeled_resources, args.resume_from,
                    confirm_orphan_volumes=args.confirm_orphan_volumes, orphan_volume_since=orphan_since)
    if args.plan_file:
        save_new(Path(args.plan_file), plan, [Path(args.target), Path(args.target+'.installation-state'),
                                            Path(args.target+'.installation-history'),
                                            Path(args.target+'.installation-run.lock')])
    if args.format == 'json':
        print(json.dumps(plan, ensure_ascii=False, indent=2))
    else:
        display(plan)
        if args.plan_file:
            print('\n计划文件（0600）：'+args.plan_file)
    if plan['blockers']:
        sys.exit(2)


try:
    main()
except (Refuse, OSError, ValueError, KeyError, TypeError) as e:
    print('错误：'+str(e), file=sys.stderr)
    sys.exit(1)
except KeyboardInterrupt:
    print('清理已中断；查看执行记录，重新盘点剩余产物后再操作。', file=sys.stderr)
    sys.exit(130)
PY
