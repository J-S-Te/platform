#!/usr/bin/env python3
"""仅为保留的活动平台 MySQL 编排补齐审计触发器所需启动参数。"""
import json
from pathlib import Path
import re
import sys

OPTION = '--log-bin-trust-function-creators=1'


def update_compose(source):
    """用途：定向补齐平台数据库启动参数，保留其他服务和现场配置。
    参数：source 为已读取的现场 Compose 文本。
    返回值：更新后的文本；不支持或冲突配置抛出异常且不写入文件。
    """
    lines = source.splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines)
              if re.fullmatch(r'  platform-mysql:\s*(?:#.*)?\n?', line)]
    if not starts:
        return source
    if len(starts) != 1:
        raise ValueError('平台数据库服务重复，须人工审核')
    start = starts[0]
    end = next((i for i in range(start + 1, len(lines))
                if re.match(r'^(?:  [^\s#]|[^\s#])', lines[i])), len(lines))
    block = lines[start:end]
    images = [i for i, line in enumerate(block) if line.startswith('    image:')]
    if len(images) != 1 or not re.fullmatch(
            r'    image: [\'"]?mysql:8\.4(?:@sha256:[a-f0-9]{64})?[\'"]?\s*\n?',
            block[images[0]]):
        raise ValueError('平台数据库须使用受管 MySQL 8.4 镜像')
    commands = [i for i, line in enumerate(block) if line.startswith('    command:')]
    if len(commands) > 1:
        raise ValueError('平台数据库启动参数重复，须人工审核')
    if commands:
        index = commands[0]
        raw = block[index].split(':', 1)[1].strip()
        try:
            arguments = json.loads(raw)
        except ValueError as error:
            raise ValueError('平台数据库自定义 command 须为 JSON 字符串数组') from error
        if not isinstance(arguments, list) or not all(isinstance(v, str) for v in arguments):
            raise ValueError('平台数据库 command 必须是字符串数组')
        configured = [v for v in arguments if v.startswith('--log-bin-trust-function-creators')]
        if configured:
            if configured != [OPTION]:
                raise ValueError('平台数据库触发器参数存在显式冲突，须人工审核')
            return source
        arguments.append(OPTION)
        block[index] = '    command: ' + json.dumps(arguments, ensure_ascii=False) + '\n'
    else:
        block.insert(images[0] + 1, '    command: ["' + OPTION + '"]\n')
    return ''.join(lines[:start] + block + lines[end:])


def main():
    """用途：向事务安装器输出校验后的编排，不直接改写现场文件。
    参数：命令行第一个参数为安装事务中的临时 Compose 路径。
    返回值：无；成功输出文本，失败输出非敏感原因并以非零状态退出。
    """
    try:
        value = update_compose(Path(sys.argv[1]).read_text())
    except (OSError, ValueError, IndexError) as error:
        print('平台数据库编排更新失败：' + str(error), file=sys.stderr)
        raise SystemExit(1)
    sys.stdout.write(value)


if __name__ == '__main__':
    main()
