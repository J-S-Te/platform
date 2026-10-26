#!/usr/bin/env python3
"""判定项目后台状态是否可以在冷启动时安全归档。

背景：项目后台（sla-notifier）用一个持久状态机记录受控切换进度。若上一次操作停在
activating/prepared 阶段时容器被整体停机（例如本地 `docker stop` 全栈），下次 up 会
因为"无法重新证明排空"而拒绝启动。此模块只做**判定**，归档动作由调用方执行。

判定为 archive 必须同时满足：
  * 状态阶段属于未完成的中间态（activating / prepared / teardown-drained）；
  * 当前**没有运行中**的项目后台容器（停着的残留容器不算在途操作）。
任何一项不满足都返回 keep —— 仍有一个进程在跑时不允许抹掉状态。
"""
import json
import sys

INCOMPLETE_STAGES = {"activating", "prepared", "teardown-drained"}


def decide(state, running, existing):
    if not isinstance(state, dict):
        return "keep"
    stage = str(state.get("stage", ""))
    if stage not in INCOMPLETE_STAGES:
        return "keep"
    if running != 0:
        return "keep"
    return "archive"


def main(argv):
    if len(argv) != 4:
        print("usage: project_background_state.py <operation.json> <running> <existing>", file=sys.stderr)
        return 2
    path, running, existing = argv[1], int(argv[2]), int(argv[3])
    try:
        with open(path, encoding="utf-8") as stream:
            state = json.load(stream)
    except (OSError, ValueError):
        return 2
    print(decide(state, running, existing))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
