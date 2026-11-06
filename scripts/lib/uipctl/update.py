"""update / retry / adopt 子命令（对应 lib/subsystem-update.sh 的 Shell 实现）。

契约要点：
  * 202 受理后轮询 /subsystem-status 直到终态；先查询、后等待，间隔 5 秒。
  * DEPLOY_WAIT_TIMEOUT 必须是纯数字且 > 0，在任何请求之前校验。
  * 旧终态保护：202 之后首次查询可能读到上一轮的 READY/PROVISION_FAILED，
    只有观察到过处理中状态、或同一终态连续确认两次，才把终态当成本轮结果。
  * 成功摘要写 stdout，失败摘要写 stderr。
"""
from __future__ import annotations

import json
import os
import re
import sys
import time

from . import common, http, validation

UPDATE_USAGE = r"""用法：subsystem.sh update|retry|adopt [认证参数] --application-code CODE --environment ENV

update/retry 保留已有应用、环境登记及业务数据，并按受控部署流程刷新运行配置和容器。
adopt 把"已登记但从未部署"的应用环境纳入部署状态机（首次部署前使用）。
重试可能重新下发接入凭据与组件授权凭据，部署状态会随执行结果更新。
要修改 BaseURL / UpstreamURL / OAuth 配置，请先通过 PATCH /environments 与 /oauth-clients
接口完成受控变更，然后调用本子命令让正在运行的子系统容器应用新配置。

必填参数：
  --application-code CODE   Application.Code，例如 contract_management
  --environment ENV         Environment.Environment：dev/test/staging/prod

可选参数：
  --issuer-alias ALIAS      认证提供方别名，例如 keycloak 或 platform；
                            仅在该参数非空时写入 update/retry/adopt 请求体（payload），
                            留空则沿用平台现有的认证提供方配置

执行控制：
  -y, --yes                 跳过最终确认
  -h, --help                显示本帮助

示例（正常重建）：
  bash scripts/subsystem.sh update \
    --application-code contract_management \
    --environment prod \
    --account admin

部署失败后的重试：
  bash scripts/subsystem.sh retry \
    --application-code contract_management \
    --environment prod \
    --account admin
"""

VALUE_OPTIONS = {
    "--api-base-url": "api_base_url",
    "--platform-origin": "platform_origin",
    "--account": "account",
    "--cookie-file": "cookie_file",
    "--application-code": "application_code",
    "--environment": "environment",
    "--issuer-alias": "issuer_alias",
}

FLAG_OPTIONS = {
    "--password-stdin": "password_stdin",
    "--replace-existing-session": "replace_existing_session",
    "-y": "assume_yes",
    "--yes": "assume_yes",
}

ENDPOINTS = {
    "update": ("/subsystem-update", "重建"),
    "retry": ("/subsystem-retry", "重试部署"),
    "adopt": ("/subsystem-adoption", "收编（纳入部署状态机）"),
}


def parse_args(argv, config: common.Config) -> None:
    """解析 update/status 共用的参数集合。

    status 复用本解析器（与 Shell 的 parse_update_args 一致），因此
    "缺少参数" / "未知参数" 的文案在 update 与 status 之间完全相同。
    """
    index = 0
    while index < len(argv):
        argument = argv[index]
        if argument in VALUE_OPTIONS:
            if index + 1 >= len(argv):
                common.fail("%s 缺少参数" % argument, 2)
            setattr(config, VALUE_OPTIONS[argument], argv[index + 1])
            index += 2
            continue
        if argument in FLAG_OPTIONS:
            setattr(config, FLAG_OPTIONS[argument], True)
            index += 1
            continue
        if argument in ("-h", "--help"):
            sys.stdout.write(UPDATE_USAGE)
            raise common.CliExit(0)
        if argument == "--":
            break
        common.log("ERROR", "未知参数：%s" % argument)
        sys.stderr.write(UPDATE_USAGE)
        raise common.CliExit(2)


def required_configuration_missing(config: common.Config) -> bool:
    return not config.application_code.strip() or not config.environment.strip()


def resolve_deploy_wait_timeout() -> int:
    """校验并解析 DEPLOY_WAIT_TIMEOUT（秒），默认 900。

    非法值必须显式失败：Bash 算术会把 "abc"/"5m" 求值为 0，从而把配置错误
    静默伪装成「立即超时」。0 也一并拒绝——它意味着一次都等不了。
    """
    raw = os.environ.get("DEPLOY_WAIT_TIMEOUT", "")
    if not raw.strip():
        return 900
    if not re.fullmatch(r"[0-9]+", raw):
        common.fail("DEPLOY_WAIT_TIMEOUT 必须是纯数字秒数（例如 900），当前值：%s" % raw, 2)
    seconds = int(raw, 10)
    if seconds < 1:
        common.fail("DEPLOY_WAIT_TIMEOUT 必须大于 0 秒（0 无法等待任何部署），当前值：%s" % raw, 2)
    return seconds


def write_payload(config: common.Config) -> bytes:
    payload = {
        "application_code": config.application_code.strip(),
        "environment": config.environment.strip(),
    }
    if config.issuer_alias.strip():
        payload["issuer_alias"] = config.issuer_alias.strip()
    return json.dumps(payload, ensure_ascii=False).encode("utf-8")


def print_summary(config: common.Config) -> None:
    sys.stderr.write(
        "\n即将受控更新以下子系统（保留已有应用、环境登记及业务数据）：\n"
        "  应用：%s\n"
        "  环境：%s\n"
        "  平台 API：%s\n" % (config.application_code, config.environment, config.api_base_url)
    )
    if config.cookie_file:
        sys.stderr.write("  认证方式：复用 Cookie 文件（路径不回显）\n")
    else:
        sys.stderr.write("  认证方式：平台管理员账号 %s（密码不回显）\n" % config.account)
    sys.stderr.write("  提示：若已变更 BaseURL/UpstreamURL/PathPrefix/OAuth 配置，请先走 PATCH，再执行 update。\n")
    sys.stderr.flush()


def confirm(config: common.Config) -> bool:
    if config.assume_yes:
        return True
    sys.stderr.write("\n")
    sys.stderr.flush()
    answer = common.read_line("确认开始重建吗？输入 y 或 yes 继续，其他输入取消：")
    if answer in {"y", "Y", "yes", "Yes", "YES"}:
        return True
    common.log("INFO", "已取消，未调用平台 API。")
    return False


def _deployment_state(body: bytes) -> str:
    try:
        envelope = json.loads(body.decode("utf-8"))
        data = envelope.get("data") or {}
        state = data.get("status", "")
    except Exception:  # noqa: BLE001 - 状态不可解析时按未知处理，继续轮询到超时
        return ""
    return state if isinstance(state, str) else str(state)


def wait_for_deployment_completion(session, config: common.Config, timeout: int):
    """等待后台部署编排进入终态，返回 (结果码, 最终响应体)。

    结果码：0 = 成功终态（READY/OFFBOARDED），1 = PROVISION_FAILED，2 = 超时或查询失败。
    """
    endpoint = "/subsystem-status?application_code=%s&environment=%s" % (
        config.application_code,
        config.environment,
    )
    deadline = time.monotonic() + timeout
    saw_active = False
    deferred_terminal = False
    while True:
        try:
            status, body = session.get(endpoint, 30)
        except http.TransportError:
            session.diagnose_connection_failure("/subsystem-status")
            return 2, None
        if status != 200:
            session.print_api_error(status, body, "/subsystem-status")
            return 2, None
        state = _deployment_state(body)
        common.log("INFO", "部署状态：%s" % (state or "unknown"))
        if state in {"READY", "OFFBOARDED"}:
            if saw_active or deferred_terminal:
                return 0, body
            deferred_terminal = True
            common.log(
                "INFO",
                "状态为 %s，但本轮尚未观察到处理中状态；等待一次确认（避免把上一次的旧终态当成本次结果）" % state,
            )
        elif state == "PROVISION_FAILED":
            if saw_active or deferred_terminal:
                return 1, body
            deferred_terminal = True
            common.log(
                "INFO",
                "状态为 PROVISION_FAILED，但本轮尚未观察到处理中状态；等待一次确认（避免把上一次的旧终态当成本次结果）",
            )
        else:
            saw_active = True
            deferred_terminal = False
        # 未进终态：仅在预算内才 sleep，且 sleep 之后重新确认预算，
        # 避免越过截止时间再发一次查询。
        if time.monotonic() >= deadline:
            break
        common.sleep_seconds(5)
        if time.monotonic() >= deadline:
            break
    common.log(
        "ERROR",
        "等待部署完成超时（%ss）；部署仍在后台执行，可稍后用 status 子命令继续查询" % timeout,
    )
    return 2, None


def print_success(body: bytes) -> None:
    envelope = http.parse_json(body, "部署结果")
    data = envelope.get("data") or {}
    print("子系统已重新部署：")
    print("  状态：%s" % data.get("status", "-"))
    print("  对外访问地址：%s" % (data.get("public_url") or "-"))
    print("  追踪号：%s" % (envelope.get("request_id") or "-"))


def print_deployment_failed_summary(body) -> None:
    """打印后台部署失败的真实错误与下一步指引（写 stderr，不混入成功摘要）。"""
    envelope = None
    if body is not None:
        try:
            envelope = json.loads(body.decode("utf-8"))
        except Exception:  # noqa: BLE001 - 读不到最终状态时给出可操作提示
            envelope = None
    if not isinstance(envelope, dict):
        print("后台部署失败，且未能读取最终状态；请用 status 子命令查询。", file=sys.stderr)
        return
    data = envelope.get("data") or {}
    print("后台部署失败：", file=sys.stderr)
    print("  应用/环境：%s / %s" % (data.get("application_code", "-"), data.get("environment", "-")), file=sys.stderr)
    print("  状态：%s" % data.get("status", "-"), file=sys.stderr)
    print("  错误码：%s" % data.get("last_error_code", "-"), file=sys.stderr)
    print(
        "  错误说明：%s" % (data.get("last_error") or data.get("last_error_message") or "-"),
        file=sys.stderr,
    )
    next_action = data.get("next_action") or ""
    if next_action:
        print("  下一步：%s" % next_action, file=sys.stderr)
    print("  修复后使用 retry 子命令重试。", file=sys.stderr)


def run(argv, mode: str = "update") -> int:
    config = common.Config()
    parse_args(argv, config)
    # 在发出任何请求之前就校验等待预算：非法值不能等到 202 之后才暴露。
    timeout = resolve_deploy_wait_timeout()

    with common.Workspace("subsystem-update.") as workspace:
        session = http.Session(config, workspace.path)
        workspace.session = session

        if required_configuration_missing(config):
            common.log("ERROR", "update/retry/adopt 必须提供 --application-code 和 --environment")
            sys.stderr.write(UPDATE_USAGE)
            raise common.CliExit(2)
        validation.normalize_update_config(config)

        print_summary(config)
        if not confirm(config):
            return 0

        session.ensure_authenticated()

        payload = write_payload(config)
        endpoint, action = ENDPOINTS[mode]
        common.log(
            "INFO",
            "开始%s %s/%s 容器" % (action, config.application_code, config.environment),
        )
        try:
            status, body = session.post_json(endpoint, payload, 900)
        except http.TransportError:
            session.diagnose_connection_failure(endpoint)
            raise common.CliExit(1)

        if status == 202:
            common.log(
                "INFO",
                "部署请求已受理，正在后台执行；等待终态（DEPLOY_WAIT_TIMEOUT 可调，默认 900s）",
            )
            result, final_body = wait_for_deployment_completion(session, config, timeout)
            if result == 0:
                print_success(final_body)
                return 0
            print_deployment_failed_summary(final_body)
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body, endpoint)
            raise common.CliExit(1)

        print_success(body)
    return 0
