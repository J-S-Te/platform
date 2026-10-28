#!/usr/bin/env python3
"""Read-only formal-build input checks. Never builds, pulls, signs or rewrites inputs."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

spec = importlib.util.spec_from_file_location('build_evidence', Path(__file__).with_name('build-evidence.py'))
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


def check(workspace, components, base_evidence=None):
    errors = []
    paths = sorted({p for c in components for p in evidence.DOCKERFILES[c]})
    for relative in paths:
        path = workspace / relative
        if path.is_symlink() or not path.is_file():
            errors.append(f'{relative}: 缺少安全的 Dockerfile')
            continue
        for line, ref in evidence.dockerfile_base_refs(path.read_text(), (base_evidence or {}).get(relative)):
            if not re.fullmatch(r'[^\s$]+@sha256:[0-9a-f]{64}', ref):
                errors.append(f'{relative}:{line}: 基础镜像未固定有效摘要：{ref}')
    return errors


def shared_diff(workspace, baseline):
    index = baseline / 'source-input-index.tsv'
    sums = baseline / 'SHA256SUMS'
    if not index.is_file() or index.is_symlink() or not sums.is_file():
        return ['旧基线缺少受校验源码索引，无法定位具体变化文件；不能据此判断必须重建哪些镜像']
    bindings = [line.split() for line in sums.read_text().splitlines() if line.split()[-1:] == ['source-input-index.tsv']]
    if len(bindings) != 1 or len(bindings[0]) != 2 or bindings[0][0] != evidence.sha256_file(index):
        return ['旧基线源码索引未被 SHA256SUMS 唯一有效绑定，无法定位具体差异']
    old = {}
    for line in index.read_text().splitlines():
        parts = line.split('\t')
        if len(parts) != 3 or not re.fullmatch(r'[0-9a-f]{64}', parts[0]) or not parts[1].isdigit() or parts[2] in old:
            return ['旧基线源码索引格式无效，无法定位具体差异']
        old[parts[2]] = parts[0]
    script = workspace / 'platform/deploy/production/bin/offline-build-inputs.sh'
    result = subprocess.run(['bash', '-c', 'source "$1"; cd "$2"; offline_build_input_paths shared',
                             'shared-diff', str(script), str(workspace)], capture_output=True)
    if result.returncode:
        return ['当前共享输入枚举失败，无法定位具体差异']
    current = {p.decode() for p in result.stdout.split(b'\0') if p}
    prefixes = ('platform/cmd/', 'platform/internal/', 'platform/migrations/', 'platform/third_party/license-core/',
                'frontend/src/', 'frontend/public/', 'frontend/nginx/',
                'platform/deploy/production/bin/', 'platform/deploy/production/monitoring/',
                'platform/deploy/production/mysql-init/', 'platform/deploy/production/nginx/',
                'platform/deploy/production/subsystem-templates/', 'platform/deploy/production/subsystems.d/')
    prior = {p for p in old if p.startswith(prefixes) and '/__pycache__/' not in p and '/.mimosa/' not in p and not p.endswith('.pyc')}
    changes = []
    for name in sorted(current | prior):
        path = workspace / name
        # Never follow a baseline-supplied traversal or symlink.
        if Path(name).is_absolute() or '..' in Path(name).parts:
            return ['旧基线源码索引路径非法，无法定位具体差异']
        if not path.resolve().is_relative_to(workspace.resolve()):
            return ['共享输入路径越出工作区，无法安全比较：' + name]
        if name not in current or not path.is_file() or path.is_symlink():
            if name in old:
                changes.append('删除/不再纳入共享输入：' + name)
        elif name not in old:
            changes.append('新增共享输入：' + name)
        elif evidence.sha256_file(path) != old[name]:
            changes.append('修改共享输入：' + name)
    return changes or ['现有索引未定位到文件差异；可能是历史输入规则变化，不能推断具体重建范围']


def inspect_platform_args():
    # Match offline_configure_platform_selection: CLI plus both APIs and override.
    def output(args):
        result = subprocess.run(['docker', *args], capture_output=True, text=True)
        return result.stdout.strip() if result.returncode == 0 else ''
    client = output(['version', '--format', '{{.Client.APIVersion}}'])
    server = output(['version', '--format', '{{.Server.APIVersion}}'])
    def supports(value):
        return bool(re.fullmatch(r'[0-9]{1,3}\.[0-9]{1,3}', value)) and tuple(map(int, value.split('.'))) >= (1, 49)
    if all(supports(v) for v in (client, server, os.environ.get('DOCKER_API_VERSION', client))):
        if '--platform' in output(['image', 'inspect', '--help']):
            return ['--platform', 'linux/amd64']
    return []


def registry_amd64_ref(image):
    def query(ref, field):
        result = subprocess.run(['docker', 'buildx', 'imagetools', 'inspect', ref, '--format', '{{json .' + field + '}}'],
                                capture_output=True, text=True, timeout=45)
        if result.returncode:
            raise ValueError('仓库解析失败（请检查网络、镜像源或仓库访问权限）')
        try:
            value = json.loads(result.stdout)
        except ValueError:
            raise ValueError('仓库返回的镜像描述不是有效 JSON')
        if not isinstance(value, dict):
            raise ValueError('仓库返回的镜像描述类型无效')
        return value
    manifest = query(image, 'Manifest')
    digest = manifest.get('digest', '')
    if not isinstance(digest, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
        raise ValueError('仓库清单缺少有效 digest')
    pinned = image + '@' + digest
    if 'manifests' in manifest:
        members = manifest['manifests']
        matches = [m for m in members if isinstance(m, dict) and isinstance(m.get('platform'), dict)
                   and m['platform'].get('os') == 'linux' and m['platform'].get('architecture') == 'amd64'
                   and isinstance(m.get('digest'), str) and re.fullmatch(r'sha256:[0-9a-f]{64}', m['digest'])] if isinstance(members, list) else []
        if not matches:
            raise ValueError('仓库索引中没有可核验的 linux/amd64 镜像')
    else:
        config = query(pinned, 'Image')
        if config.get('os') != 'linux' or config.get('architecture') != 'amd64':
            raise ValueError('仓库单平台镜像不是 linux/amd64')
    return pinned


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workspace', required=True, type=Path)
    parser.add_argument('--components')
    parser.add_argument('--shared-diff', type=Path)
    parser.add_argument('--check-identity', action='store_true')
    parser.add_argument('--base-image-evidence')
    parser.add_argument('--common-image', action='append', default=[])
    args = parser.parse_args()
    if args.shared_diff:
        print('\n'.join(shared_diff(args.workspace, args.shared_diff)), file=sys.stderr)
        return 0
    components = (args.components or '').replace(',', ' ').split()
    if not components or any(c not in evidence.DOCKERFILES for c in components):
        parser.error('未知或空组件列表')
    bases = evidence.read_base_image_evidence(args.base_image_evidence) if args.base_image_evidence else {}
    # Validate business/static inputs before querying any local common image.
    static_components = [c for c in components if c != 'common']
    if 'common' in components and 'platform' not in static_components:
        static_components.append('platform')
    errors = check(args.workspace, static_components, bases)
    if not errors and 'common' in components:
        resolved = []
        platform_args = inspect_platform_args()
        for image in args.common_image:
            result = subprocess.run(['docker', 'image', 'inspect', *platform_args, image, '--format', '{{json .}}'],
                                    capture_output=True, text=True)
            try:
                metadata = json.loads(result.stdout) if result.returncode == 0 else {}
            except ValueError:
                metadata = {}
            repository = image.rsplit(':', 1)[0]
            values = metadata.get('RepoDigests') if isinstance(metadata, dict) else []
            refs = [r for r in values if isinstance(r, str) and
                    re.fullmatch(re.escape(repository) + r'@sha256:[0-9a-f]{64}', r)] if isinstance(values, list) else []
            local_platform = (metadata.get('Os'), metadata.get('Architecture')) if isinstance(metadata, dict) else ('', '')
            descriptor = metadata.get('Descriptor') if isinstance(metadata, dict) else None
            if not any(local_platform) and isinstance(descriptor, dict) and isinstance(descriptor.get('platform'), dict):
                if descriptor.get('mediaType') in ('application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.v2+json') and isinstance(descriptor.get('digest'), str) and re.fullmatch(r'sha256:[0-9a-f]{64}', descriptor['digest']):
                    local_platform = (descriptor['platform'].get('os'), descriptor['platform'].get('architecture'))
            if local_platform == ('linux', 'amd64') and refs:
                resolved.extend(refs)
                continue
            # Cold cache/other architecture/index-only metadata is not a build failure.
            # Resolve registry metadata read-only; the builder still pulls and verifies actual bytes.
            try:
                resolved.append(registry_amd64_ref(image))
                print(f'公共镜像 {image} 已从仓库确认 linux/amd64；正式构建时自动准备（预检未拉取）')
            except (ValueError, subprocess.TimeoutExpired) as exc:
                errors.append(f'公共基础镜像 {image} 本地无可验证 linux/amd64，{exc}')
        if not args.common_image:
            errors.append('common 缺少实际基础镜像输入列表')
        if not errors:
            bases['platform/deploy/production/bin/offline-base.Dockerfile'] = resolved
            errors.extend(check(args.workspace, ['common'], bases))
    if args.check_identity:
        builder = os.environ.get('UIP_BUILD_BUILDER_ID', '')
        if not evidence.BUILDER_RE.fullmatch(builder):
            errors.append('UIP_BUILD_BUILDER_ID 缺失或无效；正式发布需要真实构建者身份')
        if not os.environ.get('UIP_BUILD_INVOCATION_ID', '').strip():
            errors.append('UIP_BUILD_INVOCATION_ID 缺失；正式发布需要本次构建标识')
    if errors:
        print('\n'.join(errors), file=sys.stderr)
        return 1
    print('正式构建输入预检通过（仍须构建后来源、签名和完整性核验）')
    return 0

if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, evidence.EvidenceError) as exc:
        print(f'正式构建输入预检失败：{exc}', file=sys.stderr)
        sys.exit(1)
