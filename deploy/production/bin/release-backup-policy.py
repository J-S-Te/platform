#!/usr/bin/env python3
"""判断发布前是否必须执行完整灾难备份。

首次安装（identity.mode=install）且平台阶段从未完成时，目标上不存在可回滚的既有发布，
也没有业务数据；此时跳过灾难备份，避免要求从未启动的 File Gateway 数据库
（典型场景：首次安装失败后按原命令 --resume）。其余情况一律要求备份，
任何无法判断的情况也退回“必须备份”。
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys


# 用途：读取部署会话状态，任何异常都返回 None 让调用方按“必须备份”处理。
# 参数：state_dir 是 <target>.installation-state，tool 是 deployment-session.py。
# 返回值：会话文档字典或 None。
def load_document(state_dir, tool):
    if not state_dir.is_dir() or state_dir.is_symlink():
        return None
    if not tool.is_file() or tool.is_symlink():
        return None
    try:
        result = subprocess.run([sys.executable, str(tool), "status",
                                 "--state-dir", str(state_dir), "--format", "json"],
                                capture_output=True, text=True)
    except OSError:
        return None
    if result.returncode != 0:
        return None
    try:
        document = json.loads(result.stdout)
    except ValueError:
        return None
    return document if isinstance(document, dict) else None


# 用途：判断本次发布前是否必须完整备份。
# 参数：document 是部署会话文档（可为 None）。
# 返回值：必须备份返回 True；可证明是未完成的首次安装返回 False。
def backup_required(document):
    if document is None:
        return True
    identity = document.get("identity")
    if not isinstance(identity, dict) or identity.get("mode") != "install":
        return True
    if document.get("overall_status") == "COMPLETED":
        return True
    completed = []
    for key in ("completed_stages", "historical_completed_stages"):
        value = document.get(key)
        if isinstance(value, list):
            completed.extend(item for item in value if isinstance(item, str))
    return "platform" in completed


# 用途：命令行入口，stdout 只输出 backup 或 skip 供部署脚本判断。
# 参数：--state-dir 必填，--session-tool 默认使用同目录 deployment-session.py。
# 返回值：0；判断结果不通过退出码表达，避免与“调用失败”混淆。
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", required=True)
    parser.add_argument("--session-tool", help="deployment-session.py 路径（默认同目录）")
    args = parser.parse_args()
    tool = Path(args.session_tool) if args.session_tool else Path(__file__).with_name("deployment-session.py")
    document = load_document(Path(args.state_dir), tool)
    print("backup" if backup_required(document) else "skip")
    return 0


if __name__ == "__main__":
    sys.exit(main())
