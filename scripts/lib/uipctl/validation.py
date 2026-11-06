"""uipctl 的平台地址、Origin 与目标参数校验。

对应 Shell 中内嵌的 Python 片段（lib/subsystem-http.sh 的 normalize_platform_origin、
lib/subsystem-update.sh 的 update_validate_and_normalize、
lib/subsystem-offboard.sh 的 offboard_validate_and_normalize）。

错误文案、多错误合并方式与退出码（2）与 Shell 版本逐字一致。
"""
from __future__ import annotations

import ipaddress
import re
import sys
from urllib.parse import urlsplit

from . import common

APPLICATION_CODE_PATTERN = r"[a-z][a-z0-9._-]{0,63}"

INSECURE_HTTP_MESSAGE = (
    "非回环平台 API 必须使用 HTTPS；可信局域网确需 HTTP 时，请同时设置 "
    "BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API=true 和 "
    "BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS=<精确主机列表>。"
    "该限制不影响 public-base-url 或 OAuth HTTP 回调。"
)


def origin_of(raw: str, allow_path: bool):
    """把 URL 收敛成纯 origin；不合法时返回 None（与 Shell 内嵌实现等价）。"""
    try:
        parsed = urlsplit(raw)
        port = parsed.port
    except ValueError:
        return None
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        return None
    if parsed.username is not None or parsed.password is not None:
        return None
    if parsed.query or parsed.fragment:
        return None
    if not allow_path and parsed.path not in {"", "/"}:
        return None
    if port is not None and not 1 <= port <= 65535:
        return None
    return "%s://%s" % (parsed.scheme, parsed.netloc)


def normalize_platform_origin(config: common.Config) -> None:
    """connect 使用的 Origin 归一化：未显式提供时按 api-base-url 推导并回写配置。"""
    api_base = config.api_base_url.strip()
    platform_origin = config.platform_origin.strip()
    if platform_origin:
        origin = origin_of(platform_origin, allow_path=False)
        if origin is None:
            print(
                "--platform-origin 必须是纯 origin（如 http://localhost:8081），"
                "不能带路径、query、fragment 或用户信息。",
                file=sys.stderr,
            )
            raise common.CliExit(2)
    else:
        origin = origin_of(api_base, allow_path=True)
        if origin is None:
            print("无法从 --api-base-url 推导 platform-origin，请显式传入 --platform-origin。", file=sys.stderr)
            raise common.CliExit(2)
        print("未提供 --platform-origin，按 --api-base-url 推导为 %s" % origin, file=sys.stderr)
    config.platform_origin = origin
    common.log("INFO", "平台请求 Origin：%s" % origin)


def validate_api_transport(config: common.Config) -> None:
    """管理员口令和 Cookie 只能通过 HTTPS 或本机回环 HTTP 发送。

    局域网确需直接访问 HTTP API 时必须显式开启并列出主机；该规则不限制
    public-base-url/OAuth 回调地址。
    """
    value = config.api_base_url
    try:
        parsed = urlsplit(value)
        host = (parsed.hostname or "").rstrip(".").lower()
        scheme = parsed.scheme
    except ValueError:
        host, scheme = "", ""
    if scheme == "https":
        return
    is_loopback = host == "localhost"
    try:
        is_loopback = is_loopback or ipaddress.ip_address(host).is_loopback
    except ValueError:
        pass
    if scheme == "http" and is_loopback:
        return
    enabled = config.allow_insecure_http_api.strip().lower() in {"1", "true", "yes", "on"}
    allowed = {
        item.strip().rstrip(".").lower()
        for item in config.insecure_http_api_allowed_hosts.split(",")
        if item.strip()
    }
    if scheme == "http" and enabled and host in allowed:
        return
    print(INSECURE_HTTP_MESSAGE, file=sys.stderr)
    raise common.CliExit(2)


def _validate_url_pair(api_base: str, platform_origin: str, errors) -> str:
    """校验 api-base-url / platform-origin 并返回补齐后的 platform-origin。"""
    if not api_base or len(api_base) > 512 or any(ch.isspace() for ch in api_base):
        errors.append("api-base-url 必填、最长 512 字符且不能包含空白")
    else:
        try:
            parsed = urlsplit(api_base)
            port = parsed.port
        except ValueError:
            errors.append("api-base-url 端口或 URL 格式无效")
        else:
            if parsed.scheme not in {"http", "https"} or not parsed.hostname:
                errors.append("api-base-url 必须是带主机名的 http/https URL")
            if parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment:
                errors.append("api-base-url 不能包含用户信息、query 或 fragment")
            if port is not None and not 1 <= port <= 65535:
                errors.append("api-base-url 端口必须在 1-65535 之间")
            if not platform_origin:
                platform_origin = "%s://%s" % (parsed.scheme, parsed.netloc)

    if platform_origin:
        try:
            origin = urlsplit(platform_origin)
            origin_port = origin.port
        except ValueError:
            errors.append("platform-origin 端口或 URL 格式无效")
        else:
            if origin.scheme not in {"http", "https"} or not origin.hostname:
                errors.append("platform-origin 必须是带主机名的 http/https origin")
            if (
                origin.username is not None
                or origin.password is not None
                or origin.query
                or origin.fragment
                or origin.path not in {"", "/"}
            ):
                errors.append("platform-origin 只能填写 origin，不能包含用户信息、路径、query 或 fragment")
            if origin_port is not None and not 1 <= origin_port <= 65535:
                errors.append("platform-origin 端口必须在 1-65535 之间")
    else:
        errors.append("无法从 api-base-url 推导 platform-origin，请显式传入 --platform-origin")
    return platform_origin


def _check_auth_options(config: common.Config) -> None:
    """认证参数互斥校验（在登录之前完成，避免半途失败）。"""
    if not config.cookie_file and not config.account.strip():
        common.fail("未使用 --cookie-file 时必须提供 --account", 2)
    if config.cookie_file and config.password_stdin:
        common.fail("--cookie-file 与 --password-stdin 不能同时使用", 2)
    if config.cookie_file and config.replace_existing_session:
        common.fail("--cookie-file 与 --replace-existing-session 不能同时使用", 2)


def normalize_update_config(config: common.Config) -> None:
    """update / retry / adopt / status 共用的归一化与前置校验。"""
    code = config.application_code.strip().lower()
    environment = config.environment.strip().lower()
    api_base = config.api_base_url.strip().rstrip("/")
    platform_origin = config.platform_origin.strip().rstrip("/")

    errors = []
    if not re.fullmatch(APPLICATION_CODE_PATTERN, code):
        errors.append("application-code 必须匹配 ^[a-z][a-z0-9._-]{0,63}$")
    if environment not in {"dev", "test", "staging", "prod"}:
        errors.append("environment 只能是 dev、test、staging 或 prod")
    platform_origin = _validate_url_pair(api_base, platform_origin, errors)
    if errors:
        print("\n".join(errors), file=sys.stderr)
        raise common.CliExit(2)

    config.application_code = code
    config.environment = environment
    config.api_base_url = api_base
    config.platform_origin = platform_origin

    validate_api_transport(config)
    _check_auth_options(config)


def normalize_offboard_config(config: common.Config) -> None:
    """offboard 的归一化与前置校验：环境白名单更严（无 dev），--confirm 必须精确匹配。"""
    code = config.application_code.strip().lower()
    environment = config.environment.strip().lower()
    confirmation = config.confirmation_code.strip()
    api_base = config.api_base_url.strip().rstrip("/")
    platform_origin = config.platform_origin.strip().rstrip("/")

    errors = []
    if not re.fullmatch(APPLICATION_CODE_PATTERN, code):
        errors.append("application-code 必须匹配 ^[a-z][a-z0-9._-]{0,63}$")
    if environment not in {"test", "staging", "prod"}:
        if environment == "dev":
            errors.append("为保护开发环境，environment 不允许使用 dev")
        else:
            errors.append("environment 只能是 test、staging 或 prod")
    if code == "platform":
        errors.append("不允许通过本脚本删除基础平台 platform 的环境")
    if confirmation != "%s/%s" % (code, environment):
        errors.append("--confirm 必须精确等于 <application-code>/<environment>")
    platform_origin = _validate_url_pair(api_base, platform_origin, errors)
    if errors:
        print("\n".join(errors), file=sys.stderr)
        raise common.CliExit(2)

    config.application_code = code
    config.environment = environment
    config.confirmation_code = confirmation
    config.api_base_url = api_base
    config.platform_origin = platform_origin

    validate_api_transport(config)
    _check_auth_options(config)
