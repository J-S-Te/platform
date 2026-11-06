"""offboard 子命令（对应 lib/subsystem-offboard.sh 的 Shell 实现）。

清理顺序固定：applications 查询 → environments 查询 → teardown（可选）→
DELETE environment → [DELETE application]。所有危险操作门槛（--confirm 精确匹配、
-y/--yes、dev/platform 保护、不提供 --force）保持不变；Shell 侧不执行任何本地
docker / 文件删除动作，真正的拆解由平台部署 Agent 在服务端完成。
"""
from __future__ import annotations

import json
import sys
from urllib.parse import quote

from . import common, http, validation

OFFBOARD_USAGE = """用法：subsystem.sh offboard [认证参数] [撤销参数]

offboard 是深清理子系统的入口。默认会依次：
  1. 调用 POST /subsystem-teardown 停止容器、删除 .env.local、移除门户网关入口、重新加载 nginx
  2. 调用 DELETE /applications/<id>/environments/<env_id> 删除 DB 记录
  3. 若指定 --delete-application，再调用 DELETE /applications/<id> 删除整个 application

撤销参数：
  --application-code CODE       要撤销的 Application.Code
  --environment ENV             要撤销的环境，只允许 test、staging 或 prod
  --confirm CODE/ENV            必填，必须精确等于 <application-code>/<environment>
  --shallow                     退回旧版语义：只删 DB 记录（仅供紧急修复使用）
  --skip-teardown               跳过第 1 步 teardown，只删 DB 记录；
                                用于 teardown 已成功、DB 记录删除失败后的续跑
  --delete-application          在 environment 删除后顺带删除 application（需 environment 已是最后一个）

安全边界：
  1. 不允许删除 dev，也不允许删除基础平台 platform。
  2. 不提供 --force。若环境仍有关联配置命名空间或审计回执，平台会拒绝删除。
  3. 默认深清理：停止容器 + 删除 .env.local + 删除门户网关入口 + 重新加载 nginx + 删除 DB 记录。
  4. 只适用于该环境永久下线；绝不能为了代码、镜像、功能模块或日常发布而撤销后重建。
  5. teardown 成功但 DB 记录删除失败时，会出现「基础设施已拆、DB 记录仍在」的半完成态；
     按错误提示用 --skip-teardown 续跑即可只补做 DB 删除，不会重复 teardown。
     --skip-teardown 不构成确认绕过：仍要求 --confirm 精确匹配，并且仍需 -y/--yes 或交互确认。
"""

VALUE_OPTIONS = {
    "--api-base-url": "api_base_url",
    "--platform-origin": "platform_origin",
    "--account": "account",
    "--cookie-file": "cookie_file",
    "--application-code": "application_code",
    "--environment": "environment",
    "--confirm": "confirmation_code",
}

FLAG_OPTIONS = {
    "--password-stdin": "password_stdin",
    "--replace-existing-session": "replace_existing_session",
    "-y": "assume_yes",
    "--yes": "assume_yes",
    "--shallow": "shallow",
    "--skip-teardown": "skip_teardown",
    "--delete-application": "delete_application",
}


def parse_args(argv, config: common.Config) -> None:
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
            sys.stdout.write(OFFBOARD_USAGE)
            raise common.CliExit(0)
        if argument == "--":
            break
        common.log("ERROR", "未知参数：%s" % argument)
        sys.stderr.write(OFFBOARD_USAGE)
        raise common.CliExit(2)


def required_configuration_missing(config: common.Config) -> bool:
    return (
        not config.application_code.strip()
        or not config.environment.strip()
        or not config.confirmation_code.strip()
    )


def application_list_endpoint(application_code: str) -> str:
    return "/applications?keyword=" + quote(application_code, safe="") + "&page=1&page_size=100"


def select_application_id(application_code: str, body: bytes) -> str:
    try:
        envelope = json.loads(body.decode("utf-8"))
        data = envelope.get("data") or {}
        items = data.get("items") or []
    except Exception as exc:  # noqa: BLE001 - 与 Shell 一致：解析失败直接终止删除流程
        print("无法解析应用列表响应：%s" % exc, file=sys.stderr)
        raise common.CliExit(1)
    matches = [item for item in items if isinstance(item, dict) and item.get("code") == application_code]
    application_id = matches[0].get("application_id") if len(matches) == 1 else None
    if not isinstance(application_id, str) or not application_id:
        print("未找到唯一的应用 %s；请先确认接入状态。" % application_code, file=sys.stderr)
        raise common.CliExit(1)
    return application_id


def select_environment(expected_environment: str, body: bytes):
    """返回 (environment_id, version)；不唯一或缺字段时终止删除。"""
    try:
        envelope = json.loads(body.decode("utf-8"))
        data = envelope.get("data") or {}
        items = data.get("items") or []
    except Exception as exc:  # noqa: BLE001
        print("无法解析环境列表响应：%s" % exc, file=sys.stderr)
        raise common.CliExit(1)
    matches = [item for item in items if isinstance(item, dict) and item.get("environment") == expected_environment]
    if len(matches) != 1:
        print("未找到唯一的环境 %s；不会执行删除。" % expected_environment, file=sys.stderr)
        raise common.CliExit(1)
    item = matches[0]
    environment_id = item.get("environment_id")
    version = item.get("version")
    if not isinstance(environment_id, str) or not environment_id or not isinstance(version, int) or version <= 0:
        print("环境记录缺少有效 id 或 version；不会执行删除。", file=sys.stderr)
        raise common.CliExit(1)
    return environment_id, version


def count_remaining_environments(body: bytes) -> int:
    envelope = http.parse_json(body, "环境列表")
    data = envelope.get("data") or {}
    items = data.get("items") or []
    return len([item for item in items if isinstance(item, dict) and item.get("environment")])


def print_summary(config: common.Config) -> None:
    sys.stderr.write(
        "\n即将撤销以下子系统接入（不可恢复）：\n"
        "  应用：%s\n"
        "  环境：%s\n"
        "  --confirm：%s\n" % (config.application_code, config.environment, config.confirmation_code)
    )
    if config.shallow:
        sys.stderr.write("  模式：shallow — 仅删除 DB 记录（环境派生的 LoginTarget 与 OAuth Client）\n")
        sys.stderr.write("  容器 / .env.local / 门户网关：不会清理\n")
    elif config.skip_teardown:
        sys.stderr.write("  模式：--skip-teardown（续跑）— 跳过 teardown，仅删除 DB 记录\n")
        sys.stderr.write("  容器 / .env.local / 门户网关：视为已清理，本次不会重复拆解\n")
    else:
        sys.stderr.write(
            "  模式：deep（默认）— 停止容器 + 删除 .env.local + 移除门户网关入口 + 重新加载 nginx + 删除 DB 记录\n"
        )
    if config.delete_application:
        sys.stderr.write("  附加：--delete-application — 在 environment 删除后顺带删除 application\n")
    sys.stderr.flush()


def confirm(config: common.Config) -> bool:
    if config.assume_yes:
        return True
    sys.stderr.write("\n")
    sys.stderr.flush()
    answer = common.read_line("确认开始深清理吗？输入 y 或 yes 继续，其他输入取消：")
    if answer in {"y", "Y", "yes", "Yes", "YES"}:
        return True
    common.log("INFO", "已取消，未调用平台 API，未删除任何子系统配置")
    return False


def print_success(config: common.Config) -> None:
    print("子系统环境接入已撤销：")
    print("  应用：%s" % config.application_code)
    print("  环境：%s" % config.environment)
    print(
        "  已清理：停止容器 + 删除 .env.local + 移除门户网关入口 + 重新加载 nginx + "
        "删除该环境派生的 LoginTarget 与 OAuth Client 配置"
    )
    print("  保留：Application（除非同时指定 --delete-application）、其他环境（含 dev）、子系统业务数据")


def print_db_delete_resume_hint(config: common.Config) -> None:
    """「DB 记录删除」失败时给出可续跑命令（只补做 DB 删除，不重复 teardown）。"""
    resume_command = (
        "bash scripts/subsystem.sh offboard --application-code %s --environment %s --confirm %s --skip-teardown"
        % (config.application_code, config.environment, config.confirmation_code)
    )
    if config.skip_teardown:
        common.log(
            "ERROR",
            "当前状态：基础设施已拆（本次已跳过 teardown），DB 记录仍未清理"
            "（环境及派生的 LoginTarget / OAuth Client 配置）。",
        )
    else:
        common.log(
            "ERROR",
            "当前状态：基础设施已拆，DB 记录仍未清理（环境及派生的 LoginTarget / OAuth Client 配置）。",
        )
    common.log(
        "ERROR",
        "补齐认证参数（--account 或 --cookie-file）后重跑以下命令，只补做 DB 删除且不会重复 teardown：",
    )
    common.log("ERROR", "  %s --account <平台管理员账号> -y" % resume_command)
    common.log(
        "ERROR",
        "该续跑命令仍要求 --confirm 精确等于 <application-code>/<environment>，并需要 -y/--yes 或交互确认。",
    )


def run(argv) -> int:
    config = common.Config()
    parse_args(argv, config)

    with common.Workspace("subsystem-offboard.") as workspace:
        session = http.Session(config, workspace.path)
        workspace.session = session

        if required_configuration_missing(config):
            common.log("ERROR", "offboard 必须提供 --application-code、--environment、--confirm")
            sys.stderr.write(OFFBOARD_USAGE)
            raise common.CliExit(2)
        validation.normalize_offboard_config(config)

        print_summary(config)
        if not confirm(config):
            return 0

        session.ensure_authenticated()

        # 1) 定位 application：必须唯一匹配代码。
        try:
            status, body = session.get(application_list_endpoint(config.application_code), 120)
        except http.TransportError:
            common.log("ERROR", "无法连接应用查询接口：%s/applications" % config.api_base_url)
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body)
            raise common.CliExit(1)
        application_id = select_application_id(config.application_code, body)

        # 2) 定位 environment：必须唯一匹配环境，并取到乐观锁 version。
        environments_endpoint = "/applications/%s/environments?page=1&page_size=100" % application_id
        try:
            status, body = session.get(environments_endpoint, 120)
        except http.TransportError:
            common.log(
                "ERROR",
                "无法连接环境查询接口：%s/applications/%s/environments" % (config.api_base_url, application_id),
            )
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body)
            raise common.CliExit(1)
        environment_id, environment_version = select_environment(config.environment, body)

        # 3) teardown：默认深清理；--shallow / --skip-teardown 都跳过。
        if not config.shallow and not config.skip_teardown:
            teardown_payload = json.dumps(
                {
                    "application_code": config.application_code.strip(),
                    "environment": config.environment.strip(),
                },
                ensure_ascii=False,
            ).encode("utf-8")
            common.log(
                "INFO",
                "深清理 %s/%s：停止容器、删除 .env.local、移除门户网关入口、重新加载 nginx"
                % (config.application_code, config.environment),
            )
            try:
                status, body = session.post_json("/subsystem-teardown", teardown_payload, 300)
            except http.TransportError:
                session.diagnose_connection_failure("/subsystem-teardown")
                raise common.CliExit(1)
            if status != 200:
                session.print_api_error(status, body, "/subsystem-teardown")
                raise common.CliExit(1)
            common.log("INFO", "容器与网关清理完成")

        # 4) 删除环境 DB 记录（含派生的 LoginTarget 与 OAuth Client 配置）。
        delete_endpoint = "/applications/%s/environments/%s" % (application_id, environment_id)
        delete_payload = json.dumps(
            {"confirmation_code": config.confirmation_code, "version": int(environment_version)},
            ensure_ascii=False,
        ).encode("utf-8")
        common.log(
            "INFO",
            "开始删除 %s/%s 的 DB 记录（环境派生的 LoginTarget 与 OAuth Client 配置）"
            % (config.application_code, config.environment),
        )
        try:
            status, body = session.delete_json(delete_endpoint, delete_payload, 120)
        except http.TransportError:
            common.log(
                "ERROR",
                "无法连接环境删除接口：%s/applications/%s/environments/%s"
                % (config.api_base_url, application_id, environment_id),
            )
            print_db_delete_resume_hint(config)
            raise common.CliExit(1)
        if status != 200:
            session.print_api_error(status, body, delete_endpoint)
            print_db_delete_resume_hint(config)
            raise common.CliExit(1)

        # 5) 可选：删除 application（必须已无其他 environment）。
        if config.delete_application:
            try:
                status, body = session.get(environments_endpoint, 120)
            except http.TransportError:
                common.log(
                    "ERROR",
                    "无法重新查询环境列表：%s/applications/%s/environments"
                    % (config.api_base_url, application_id),
                )
                raise common.CliExit(1)
            if status != 200:
                session.print_api_error(status, body)
                raise common.CliExit(1)
            remaining_count = count_remaining_environments(body)
            if remaining_count != 0:
                common.log(
                    "ERROR",
                    "application 仍存在 %d 个环境，--delete-application 不会执行。请先清理其他环境。"
                    % remaining_count,
                )
                raise common.CliExit(2)
            common.log("INFO", "开始删除 application %s" % config.application_code)
            try:
                status, body = session.delete_json("/applications/%s" % application_id, None, 60)
            except http.TransportError:
                common.log(
                    "ERROR",
                    "无法连接 application 删除接口：%s/applications/%s"
                    % (config.api_base_url, application_id),
                )
                raise common.CliExit(1)
            if status != 200:
                session.print_api_error(status, body, "/applications/%s" % application_id)
                raise common.CliExit(1)

        print_success(config)
    return 0
