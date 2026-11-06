"""status 子命令（对应 lib/subsystem-status.sh 的 Shell 实现）。

status 复用 update 的参数解析与归一化规则，但显式拒绝 update 专属的
--issuer-alias，避免操作者误以为 status 会顺带切换认证提供方。
"""
from __future__ import annotations

import sys

from . import common, http, update, validation

STATUS_USAGE = """用法：subsystem.sh status [认证参数] --application-code CODE --environment ENV

查询基础平台记录的部署 Agent 状态。常见状态：
  PROVISIONING  首次接入记录已创建，Agent 尚未完成
  UPDATING      正在重建或重试
  READY         Agent 已成功完成，门户才会展示该子系统
  PROVISION_FAILED  最近一次 Agent 操作失败，可使用 retry 重试
  DRAINING      正在下线
  OFFBOARDED    基础设施已拆解，等待或已经完成 DB 清理
"""

STATUS_FIELDS = (
    ("application_code", "应用"),
    ("environment", "环境"),
    ("status", "状态"),
    ("operation", "最近操作"),
    ("generation", "代次"),
    ("attempt_count", "尝试次数"),
    ("last_error_code", "错误编码"),
    ("last_error", "错误说明"),
    ("updated_at", "更新时间"),
)


def print_state(body: bytes) -> None:
    envelope = http.parse_json(body, "状态")
    data = envelope.get("data") or {}
    print("子系统部署状态：")
    for key, label in STATUS_FIELDS:
        value = data.get(key)
        if value not in (None, ""):
            print("  %s：%s" % (label, value))
    print("  追踪号：%s" % (envelope.get("request_id") or "-"))


def run(argv) -> int:
    for argument in argv:
        if argument in ("-h", "--help"):
            sys.stdout.write(STATUS_USAGE)
            return 0

    config = common.Config()
    update.parse_args(argv, config)

    if config.issuer_alias.strip():
        common.log("ERROR", "status 不接受 --issuer-alias；该参数只对 update/retry/adopt 生效")
        sys.stderr.write(STATUS_USAGE)
        raise common.CliExit(2)

    with common.Workspace("subsystem-status.") as workspace:
        session = http.Session(config, workspace.path)
        workspace.session = session

        if update.required_configuration_missing(config):
            common.log("ERROR", "status 必须提供 --application-code 和 --environment")
            sys.stderr.write(STATUS_USAGE)
            raise common.CliExit(2)
        validation.normalize_update_config(config)

        session.ensure_authenticated()

        endpoint = "/subsystem-status?application_code=%s&environment=%s" % (
            config.application_code,
            config.environment,
        )
        try:
            status, body = session.get(endpoint, 120)
        except http.TransportError:
            session.diagnose_connection_failure("/subsystem-status")
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body, "/subsystem-status")
            raise common.CliExit(1)
        print_state(body)
    return 0
