"""subsystem.sh 已迁移子命令的 Python 实现（仅标准库，兼容 Python 3.9）。

模块划分（与 Shell 库一一对应，便于逐字对照）：
  common.py      日志、退出码、参数模型、终端交互、临时工作目录  ← lib/subsystem-common.sh
  http.py        会话 / Cookie / Origin / JSON 请求与错误诊断      ← lib/subsystem-http.sh
  validation.py  平台地址、Origin、application/environment 归一化
  connect.py     connect 子命令（统一接入向导）
  status.py      status 子命令
  update.py      update / retry / adopt 子命令
  offboard.py    offboard 子命令
  cli.py         子命令分发与顶层用法

onboard 仍由 Shell 实现（scripts/lib/subsystem-onboard.sh），本轮未迁移。
"""

__all__ = []
