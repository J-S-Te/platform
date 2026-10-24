#!/usr/bin/env python3
"""为安装检查点生成当前文件系统证明，绝不将数据作为代码执行。"""
import argparse
import hashlib
import os
from pathlib import Path, PurePosixPath
import stat
import sys
import tarfile


def regular(path):
    path = Path(path)
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise ValueError('UNSAFE_PATH')
    if not stat.S_ISREG(path.stat().st_mode):
        raise ValueError('NOT_REGULAR')
    return path


def digest(stream):
    result = hashlib.sha256()
    for chunk in iter(lambda: stream.read(1024 * 1024), b''):
        result.update(chunk)
    return result.digest()


def same_file(left, right):
    with regular(left).open('rb') as a, regular(right).open('rb') as b:
        return digest(a) == digest(b)


def assets_match(archive, target):
    if (target/'runtime/.assets-install-transaction').exists():
        return False
    count = 0
    seen = set()
    with tarfile.open(regular(archive), 'r:gz') as bundle:
        for member in bundle:
            name = member.name.removeprefix('./')
            if member.isdir() and name in ('', '.'):
                continue
            parts = PurePosixPath(name).parts
            if not parts or name.startswith('/') or '..' in parts or name in seen:
                raise ValueError('INVALID_ASSETS')
            seen.add(name)
            if member.isdir():
                continue
            if not member.isfile() or member.size > 64 * 1024 * 1024:
                raise ValueError('INVALID_ASSETS')
            # Compose 刻意合并操作人的模块选择；其真实服务和镜像证明与不可变资产分开处理。
            if name == 'docker-compose.yml':
                continue
            destination = target/name
            if ('/' not in name and name.startswith('runtime-license-')) or name in ('license-evidence.json','license-migration-evidence.json'):
                destination = target/'runtime-approvals'/name
            with bundle.extractfile(member) as expected, regular(destination).open('rb') as actual:
                if digest(expected) != digest(actual):
                    return False
            count += 1
    return count > 0


def config_match(target):
    for name in ('.env', '.release.env'):
        item=regular(target/name)
        if stat.S_IMODE(item.stat().st_mode) != 0o600 or item.stat().st_size == 0:
            return False
    return True


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=('assets','package','config'))
    parser.add_argument('--target',required=True,type=Path)
    parser.add_argument('--source',type=Path)
    args=parser.parse_args()
    try:
        if args.action == 'assets':
            valid=assets_match(args.source,args.target)
        elif args.action == 'package':
            valid=same_file(args.source,args.target/'packages'/args.source.name) and same_file(str(args.source)+'.sha256',args.target/'packages'/(args.source.name+'.sha256'))
        else:
            valid=config_match(args.target)
        return 0 if valid else 1
    except (OSError,ValueError,tarfile.TarError,TypeError):
        print('CHECKPOINT_PROOF_FAILED：当前文件证据不完整或与交付不一致。',file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
