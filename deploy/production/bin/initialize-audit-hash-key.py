#!/usr/bin/env python3
"""在发布锁与数据库历史检查之后初始化缺失审计密钥，已有密钥永不替换。"""
import argparse
import base64
import os
from pathlib import Path
import re
import secrets
import stat
import sys
import tempfile

KEY = 'PLATFORM_AUDIT_HASH_KEY'
ASSIGNMENT = re.compile(r'^\s*(?:export\s+)?' + KEY + r'\s*=(.*)$')


def read_private(path):
    """用途：读取受保护的普通配置文件，拒绝链接及宽权限材料。
    参数：path 为运行配置或初始化备份路径。
    返回值：文件文本及元数据；类型、权限或大小不符时拒绝。
    """
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 1024 * 1024:
        raise ValueError('配置或备份必须为受保护的普通文件（0600）')
    return path.read_text(), info


def configured_key(content):
    """用途：解析并验证唯一审计密钥，不将密钥放入错误消息。
    参数：content 为当前配置或备份文本。
    返回值：有效密钥或空字符串；重复、格式错误和过短密钥均拒绝。
    """
    values = [match.group(1).strip() for line in content.splitlines()
              if (match := ASSIGNMENT.fullmatch(line))]
    if len(values) > 1:
        raise ValueError('审计密钥配置重复，须人工核对')
    value = values[0] if values else ''
    if len(value) > 1 and value[0] == value[-1] and value[0] in "\"'":
        value = value[1:-1]
    if not value:
        return ''
    try:
        decoded = base64.b64decode(value + '=' * (-len(value) % 4), validate=True)
    except ValueError as error:
        raise ValueError('已有审计密钥不是标准 base64，拒绝替换') from error
    if len(decoded) < 32:
        raise ValueError('已有审计密钥少于 32 字节，拒绝替换')
    return value


def sync_directory(path):
    """用途：同步目录项，保证先备份再发布在断电重启后仍保持顺序。
    参数：path 为备份或运行配置的父目录。
    返回值：无；同步失败则停止发布。
    """
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def prepare_backup(path, owner):
    """用途：先持久化一次性密钥备份并读回校验，重试复用原候选。
    参数：path 为私有初始化备份文件，owner 为运行配置属主。
    返回值：已备份的密钥；备份不安全、损坏或持久化失败时拒绝发布。
    """
    if not stat.S_ISDIR(path.parent.lstat().st_mode):
        raise ValueError('备份目录必须是实际目录')
    if path.exists() or path.is_symlink():
        key = configured_key(read_private(path)[0])
        if not key:
            raise ValueError('初始化备份缺少密钥，拒绝覆盖')
        return key
    key = base64.b64encode(secrets.token_bytes(32)).decode()
    payload = KEY + '=' + key + '\n'
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, 'w') as output:
        if os.geteuid() == 0:
            os.fchown(output.fileno(), owner.st_uid, owner.st_gid)
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    sync_directory(path.parent)
    if read_private(path)[0] != payload:
        raise ValueError('审计密钥备份读回校验失败')
    return key


def initialize(env_file, backup, history, check):
    """用途：有序完成密钥校验、历史保护、备份和运行配置原子发布。
    参数：env_file 为运行配置，backup 为初始化备份，history 为非旧摘要历史标志，check 为只读模式。
    返回值：已配置或初始化成功返回零，只读缺钥返回三；任何错误拒绝继续。
    """
    content, owner = read_private(env_file)
    if configured_key(content):
        return 0
    if check:
        return 3
    if history != '0':
        raise ValueError('已有非 SHA-256 审计历史但缺密钥，必须恢复原密钥')
    key = prepare_backup(backup, owner)
    lines = content.splitlines(keepends=True)
    if any(ASSIGNMENT.fullmatch(line.rstrip('\n')) for line in lines):
        lines = [KEY + '=' + key + '\n' if ASSIGNMENT.fullmatch(line.rstrip('\n')) else line
                 for line in lines]
    else:
        if content and not content.endswith('\n'):
            lines.append('\n')
        lines.append(KEY + '=' + key + '\n')
    descriptor, temporary = tempfile.mkstemp(prefix='.audit-env.', dir=env_file.parent)
    try:
        with os.fdopen(descriptor, 'w') as output:
            if os.geteuid() == 0:
                os.fchown(output.fileno(), owner.st_uid, owner.st_gid)
            output.writelines(lines)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, env_file)
        sync_directory(env_file.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return 0


def main():
    """用途：向发布脚本提供无密钥输出的初始化或只读检查入口。
    参数：命令行指定运行配置、备份、历史标志及只读检查选项。
    返回值：无；退出码表达已配置、缺配置或失败，不输出材料内容。
    """
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--env', type=Path, required=True)
    parser.add_argument('--backup', type=Path)
    parser.add_argument('--has-hmac-history', choices=['0', '1'])
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    try:
        if not args.check and args.backup is None:
            raise ValueError('初始化必须指定独立备份路径')
        code = initialize(args.env, args.backup, args.has_hmac_history, args.check)
    except (OSError, ValueError) as error:
        print('审计密钥初始化失败：' + str(error), file=sys.stderr)
        raise SystemExit(1)
    raise SystemExit(code)


if __name__ == '__main__':
    main()
