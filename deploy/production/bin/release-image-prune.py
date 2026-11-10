#!/usr/bin/env python3
"""发布成功后仅在无安装会话或会话完成时执行既有镜像保留清理。

自动入口与一键安装共享执行长锁，检查与整个清理子进程不可分割。
原 prune-images.sh 手工入口不受本工具保护；人工清理前仍须停止部署并核对回退材料。
"""
import argparse
import importlib.util
import os
from pathlib import Path
import stat
import subprocess
import sys


# 用途：输出固定维护提示，避免清理延后影响已成功的发布。
# 参数：reason 是状态码或已验证的会话状态。
# 返回值：无。
def deferred(reason):
    print("镜像保留清理延后 [" + reason + "]；不影响发布成功。", file=sys.stderr)


# 用途：读取同目录会话实现，复用安装长锁与完整状态验证。
# 参数：无。
# 返回值：可信会话模块；工具文件不安全或不可用时抛出错误。
def session_module():
    path = Path(__file__).with_name("deployment-session.py")
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid())
            or info.st_mode & 0o022):
        raise ValueError("unsafe session tool")
    spec = importlib.util.spec_from_file_location("uip_prune_session", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# 用途：只读判断同级会话是否不存在或已真实完成。
# 参数：session 为会话模块，state_dir 为与发布目标绑定的会话目录。
# 返回值：可清理返回 true，否则输出延后原因并返回 false。
def session_allows_prune(session, state_dir):
    if not os.path.lexists(state_dir):
        return True
    with session.locked_directory(state_dir) as directory:
        value = session.load_state(directory, state_dir)
        status = session.overall(value)
    if status != "COMPLETED":
        deferred(status)
        return False
    return True


# 用途：在安装执行长锁内完成会话检查及整个既有清理子进程。
# 参数：deploy_dir 为规范部署根，keep_hours 为原清理脚本的保留小时数。
# 返回值：无；不满足安全条件时延后，清理失败只记录维护告警。
def prune_when_idle(deploy_dir, keep_hours):
    session = session_module()
    session.canonical_path(deploy_dir)
    state_dir = deploy_dir + ".installation-state"
    with session.idle_runner(state_dir):
        if not session_allows_prune(session, state_dir):
            return
        tool = Path(deploy_dir) / "bin/prune-images.sh"
        info = tool.lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid())
                or info.st_mode & 0o022):
            deferred("UNSAFE_PRUNE_TOOL")
            return
        result = subprocess.run(
            ["bash", str(tool), "--keep-hours", keep_hours, "--apply"], check=False)
        if result.returncode != 0:
            print("镜像保留清理失败；不影响发布成功。", file=sys.stderr)


# 用途：解析发布后维护参数，所有安全拒绝均作为延后而不改变发布结果。
# 参数：argv 为命令参数；为空时使用进程参数。
# 返回值：正常执行或维护延后返回零；非法命令参数由解析器拒绝。
def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument("--deploy-dir", required=True)
    parser.add_argument("--keep-hours", default="72")
    args = parser.parse_args(argv)
    if not args.keep_hours.isascii() or not args.keep_hours.isdigit():
        parser.error("--keep-hours 必须是非负整数")
    try:
        prune_when_idle(args.deploy_dir, args.keep_hours)
    except (OSError, ValueError, ImportError, AttributeError, TypeError,
            KeyError, OverflowError, RecursionError) as error:
        deferred(getattr(error, "code", "UNSAFE_OR_UNAVAILABLE_STATE"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
