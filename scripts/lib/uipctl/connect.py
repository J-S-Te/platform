"""connect 子命令（对应 lib/subsystem-connect.sh 的 Shell 实现）。

统一接入向导：预览 → 确认 → 提交/恢复 → 轮询进度。
动作（ONBOARD / ADOPT / UPDATE / RETRY）由服务端判定，CLI 不选择动作。

注意（保持既有契约，不在本次迁移中改动）：Shell 版本把预览与进度的完整输出
管道给 `tail -n 1` 只取状态行，因此这些文本在历史实现里并不会出现在 stdout。
本次迁移保持同样的输出行为，避免改变脚本化调用方已经依赖的 stdout 形状。
"""
from __future__ import annotations

import json
import sys

from . import common, http, validation

CONNECT_USAGE = """用法：
  subsystem.sh connect --application-code CODE --environment ENV [选项]

说明：
  统一接入向导（预览 → 确认 → 自动执行 → 查看结果／继续恢复）。
  动作（ONBOARD / ADOPT / UPDATE / RETRY）由服务端判定，调用方不选择动作；
  缺少批准、许可证、镜像或认证观察期未结束时进入"等待处理"。

选项：
  --application-code CODE   目标应用编码（如 contract_management、settlement）
  --environment ENV         目标环境（dev/test/staging/prod）
  --public-origin URL       对外公开地址（首次接入或公开入口变更时提供）
  --auth-provider ALIAS     认证提供方（platform/keycloak）；默认沿用服务端绑定
  --initial-admin USER_ID   首次接入的初始管理员
  --idempotency-key KEY     幂等键；默认按"目标+预览版本"生成，重复执行返回原操作
  --resume OPERATION_ID     材料补齐后继续该操作
  --no-watch                只提交/恢复，不轮询进度
  --watch-interval SECONDS  轮询间隔；默认 5
  --watch-max ATTEMPTS      最多轮询次数；默认 60
  -y, --yes                 跳过确认（非交互执行）

通用认证参数与其他子命令一致（--api-base-url/--platform-origin/--account/--password-stdin/--cookie-file）。
"""

VALUE_OPTIONS = {
    "--api-base-url": "api_base_url",
    "--platform-origin": "platform_origin",
    "--account": "account",
    "--cookie-file": "cookie_file",
    "--application-code": "application_code",
    "--environment": "environment",
    "--public-origin": "connect_public_origin",
    "--auth-provider": "issuer_alias",
    "--initial-admin": "connect_initial_admin",
    "--idempotency-key": "connect_idempotency_key",
    "--resume": "connect_resume_id",
    "--watch-interval": "connect_watch_interval",
    "--watch-max": "connect_watch_max",
}

FLAG_OPTIONS = {
    "--password-stdin": "password_stdin",
    "--replace-existing-session": "replace_existing_session",
    "-y": "assume_yes",
    "--yes": "assume_yes",
}

TERMINAL_STATES = {"SUCCEEDED", "FAILED", "WAITING"}


def parse_args(argv, config: common.Config) -> None:
    index = 0
    while index < len(argv):
        argument = argv[index]
        if argument in VALUE_OPTIONS:
            if index + 1 >= len(argv):
                common.fail("%s 需要参数" % argument, 2)
            setattr(config, VALUE_OPTIONS[argument], argv[index + 1])
            index += 2
            continue
        if argument == "--no-watch":
            config.connect_watch = False
            index += 1
            continue
        if argument in FLAG_OPTIONS:
            setattr(config, FLAG_OPTIONS[argument], True)
            index += 1
            continue
        if argument in ("-h", "--help"):
            sys.stdout.write(CONNECT_USAGE)
            raise common.CliExit(0)
        common.log("ERROR", "未知参数：%s" % argument)
        sys.stderr.write(CONNECT_USAGE)
        raise common.CliExit(2)


def _data(envelope):
    return envelope.get("data") or {}


def preview_version(body: bytes) -> str:
    envelope = http.parse_json(body, "接入条件预览")
    return _data(envelope).get("preview_version") or ""


def preview_execution_path(body: bytes) -> str:
    envelope = http.parse_json(body, "接入条件预览")
    return _data(envelope).get("execution_path") or ""


def preview_display_only(body: bytes) -> bool:
    envelope = http.parse_json(body, "接入条件预览")
    return _data(envelope).get("display_only") is True


def operation_status(body: bytes) -> str:
    envelope = http.parse_json(body, "接入操作")
    status = _data(envelope).get("status")
    return status if isinstance(status, str) else ""


def operation_id(body: bytes) -> str:
    envelope = http.parse_json(body, "接入操作")
    return _data(envelope).get("operation_id") or ""


def confirm_execution() -> bool:
    answer = common.read_line("确认按上述判定执行？[y/N] ")
    return answer in {"y", "Y", "yes", "YES"}


def watch_operation(session: http.Session, config: common.Config, operation: str) -> None:
    """轮询到终态或「等待处理」；--no-watch 时只查一次。"""
    endpoint = "/subsystem-access/operations/%s" % operation
    max_attempts = common.parse_positive_int(config.connect_watch_max) if config.connect_watch else 1
    attempt = 0
    while attempt < max_attempts:
        attempt += 1
        try:
            status, body = session.get(endpoint, 120)
        except http.TransportError:
            session.diagnose_connection_failure(endpoint)
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body, endpoint)
            raise common.CliExit(1)
        state = operation_status(body)
        if state in TERMINAL_STATES:
            return
        if not config.connect_watch:
            return
        common.sleep_seconds(config.connect_watch_interval)
    common.log(
        "WARN",
        "轮询达到上限（%d 次）；操作仍在执行，可稍后重新查询或使用 --resume 继续" % max_attempts,
    )


def run(argv) -> int:
    for argument in argv:
        if argument in ("-h", "--help"):
            sys.stdout.write(CONNECT_USAGE)
            return 0

    config = common.Config()
    parse_args(argv, config)

    with common.Workspace("subsystem-connect.") as workspace:
        session = http.Session(config, workspace.path)
        workspace.session = session

        if not config.application_code or not config.environment:
            common.log("ERROR", "connect 必须提供 --application-code 和 --environment")
            sys.stderr.write(CONNECT_USAGE)
            raise common.CliExit(2)

        # 登录链路第一步是 POST /auth/credential-challenges，它受平台同源防线保护：
        # 缺失 Origin 一律 403。未显式提供时按 api-base-url 推导，绝不把空 Origin 发出去。
        validation.normalize_platform_origin(config)

        session.ensure_authenticated()

        # 恢复模式：材料补齐后继续既有操作（服务端会重新检查一次）。
        if config.connect_resume_id:
            resume_endpoint = "/subsystem-access/operations/%s/resume" % config.connect_resume_id
            try:
                # 与 curl 的 `--request POST`（无 --data）一致：空体 + Content-Length: 0。
                status, body = session.post_json(resume_endpoint, b"", 900)
            except http.TransportError:
                session.diagnose_connection_failure(resume_endpoint)
                raise common.CliExit(1)
            if status != 200:
                session.print_api_error(status, body, resume_endpoint)
                raise common.CliExit(1)
            watch_operation(session, config, config.connect_resume_id)
            return 0

        # 1) 只读条件检查（不创建资源、不部署）。
        preview_payload = json.dumps(
            {
                "application_code": config.application_code,
                "environment": config.environment,
            },
            ensure_ascii=False,
        ).encode("utf-8")
        try:
            status, body = session.post_json("/subsystem-access/preview", preview_payload, 900)
        except http.TransportError:
            session.diagnose_connection_failure("/subsystem-access/preview")
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body, "/subsystem-access/preview")
            raise common.CliExit(1)

        version = preview_version(body)
        # 执行入口为既有受控入口时，统一入口不得提交（与页面行为一致）：只给出提示并退出。
        if preview_execution_path(body) == "legacy":
            common.log(
                "ERROR",
                "该动作当前由既有受控更新入口执行；请按上方提示执行，完成后可重新检查条件。",
            )
            raise common.CliExit(2)
        if preview_display_only(body):
            return 0

        # 2) 确认执行（动作由服务端判定，CLI 不选择动作）。
        if not config.assume_yes and not confirm_execution():
            common.log("INFO", "已取消；未创建操作")
            return 0

        # 3) 提交操作（幂等键未显式提供时按「目标+预览版本」生成）。
        idempotency_key = config.connect_idempotency_key
        if not idempotency_key:
            idempotency_key = "connect:%s:%s:%s" % (
                config.application_code,
                config.environment,
                version,
            )
        submit = {
            "application_code": config.application_code,
            "environment": config.environment,
            "preview_version": version,
            "idempotency_key": idempotency_key,
        }
        if config.connect_public_origin:
            submit["public_origin"] = config.connect_public_origin
        if config.issuer_alias:
            submit["auth_provider"] = config.issuer_alias
        if config.connect_initial_admin:
            submit["initial_admin_id"] = config.connect_initial_admin
        try:
            status, body = session.post_json(
                "/subsystem-access/operations",
                json.dumps(submit, ensure_ascii=False).encode("utf-8"),
                900,
            )
        except http.TransportError:
            session.diagnose_connection_failure("/subsystem-access/operations")
            raise common.CliExit(1)
        if status not in (202, 200):
            if status == 409:
                common.log("ERROR", "条件已变化或该目标已有执行中操作；请重新执行 connect 以获取最新预览。")
            session.print_api_error(status, body, "/subsystem-access/operations")
            raise common.CliExit(1)
        operation = operation_id(body)
        if not operation:
            common.log("INFO", "服务端判定无需创建操作（目标已接入且配置一致）")
            return 0
        common.log("INFO", "接入操作已受理：%s" % operation)
        watch_operation(session, config, operation)
    return 0
