"""uipctl 入口：子命令分发与顶层用法。

`scripts/subsystem.sh` 只做子命令识别，把已迁移的六条命令（connect/status/
update/retry/adopt/offboard）`exec python3 -m uipctl`；本模块的错误文案与退出码
与 Shell 入口保持一致（无子命令/未知子命令退出 2，帮助退出 0）。
"""
from __future__ import annotations

import sys

from . import common
from .connect import run as run_connect
from .offboard import run as run_offboard
from .status import run as run_status
from .update import run as run_update

TOP_LEVEL_USAGE = """用法：
  subsystem.sh <connect|onboard|update|retry|status|offboard> [通用认证参数] [子命令参数]

子命令：
  connect    统一接入向导（预览 → 确认 → 自动执行 → 查看结果／继续恢复）；动作由服务端判定
  onboard    首次接入子系统（创建 DB 记录 + 写入 .env.local + 启动容器 + 配置门户网关）
  update     一键重建子系统容器；DB 字段变更请先走 PATCH
  retry      重试失败的部署 Agent 操作；不会重复创建 Application/OAuth Client
  status     查询部署 Agent 状态（PROVISIONING/READY/PROVISION_FAILED 等）
  offboard   深清理子系统（停止容器 + 删除 .env.local + 删除门户网关入口 + 删除 DB 记录）
             --shallow   退回旧版语义：只删 DB 记录（仅供紧急修复使用）
             --skip-teardown  跳过 teardown，只删 DB 记录（teardown 已成功、DB 未清时的续跑）

通用认证参数：
  --api-base-url URL             平台 API 根地址；默认 BASIC_PLATFORM_API_BASE_URL
  --platform-origin URL          Cookie 写请求的可信 Origin；默认 BASIC_PLATFORM_ORIGIN，仍未设置时按 api-base-url 推导
  --account ACCOUNT              具备对应权限的平台管理员账号
  --password-stdin               从标准输入读取一行密码；未指定时安全交互输入
  --cookie-file FILE             复用已登录的平台 Cookie 文件
  --replace-existing-session     撤销该账号原有会话后登录

平台 API 传输保护：
  HTTPS 及 localhost/回环 HTTP 默认允许。可信局域网必须同时设置
  BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API=true 和
  BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS=<精确主机列表>。
  此规则不限制 --public-base-url 或 OAuth HTTP 回调。

凭据加密：
  平台启用 CREDENTIAL_ENCRYPTION_MODE=required 时，登录会自动申请 challenge
  并以 uip-credential/v1 加密提交，不先发送明文口令；旧平台缺少该能力时回退明文。

通用控制：
  -y, --yes                      跳过确认；适用于 CI
  -h, --help                     显示本帮助

执行 onboard / update / retry / status / offboard -h 查看子命令各自的参数说明。
"""

SUPPORTED_COMMANDS = "connect、onboard、update、retry、status、offboard"

HANDLERS = {
    "connect": run_connect,
    "status": run_status,
    "update": lambda argv: run_update(argv, mode="update"),
    "retry": lambda argv: run_update(argv, mode="retry"),
    "adopt": lambda argv: run_update(argv, mode="adopt"),
    "offboard": run_offboard,
}


def main(argv=None) -> int:
    arguments = list(sys.argv[1:] if argv is None else argv)
    if not arguments:
        sys.stderr.write(TOP_LEVEL_USAGE)
        sys.stderr.flush()
        return 2

    command = arguments[0]
    remaining = arguments[1:]
    if command in ("-h", "--help", "help"):
        sys.stdout.write(TOP_LEVEL_USAGE)
        sys.stdout.flush()
        return 0

    handler = HANDLERS.get(command)
    if handler is None:
        common.log("ERROR", "未知子命令：%s。支持：%s" % (command, SUPPORTED_COMMANDS))
        sys.stderr.write(TOP_LEVEL_USAGE)
        sys.stderr.flush()
        return 2

    try:
        return handler(remaining)
    except common.CliExit as exc:
        return exc.code
