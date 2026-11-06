"""uipctl 的 HTTP / 会话层：Cookie、可信 Origin、JSON 请求与错误诊断。

对应 Shell 的 lib/subsystem-http.sh。请求头集合（Accept / Origin / Content-Type）、
Cookie 复用语义、凭据加密登录回退链与错误文案都与该 Shell 库保持一致。
"""
from __future__ import annotations

import http.client
import http.cookiejar as cookiejar
import getpass
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

from . import common

CREDENTIAL_HELPER = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "credential-envelope.py"
)

UNPARSABLE_RESPONSE = (
    "HTTP %s：平台返回了无法解析的响应。请检查 API 地址、反向代理和 api 日志。"
)

DIAGNOSIS = """可按以下顺序排查：
  1. 在 platform 目录执行：bash scripts/docker-local.sh ps
  2. 查看基础平台日志：bash scripts/docker-local.sh logs api subsystem-provisioner
  3. 确认 --api-base-url 指向平台 API（默认 http://localhost:8081/api/v1），不是子系统地址。
  4. 确认 --platform-origin 与平台 APP_CORS_ALLOWED_ORIGINS/OIDC_ISSUER 中允许的 origin 一致。
  5. 若刚更新了基础平台后端路由或迁移，执行：bash scripts/docker-local.sh refresh-api
"""


class TransportError(Exception):
    """网络层失败（连接被拒、超时等），由调用方决定诊断文案。"""


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """与 curl（未加 -L）一致：不跟随重定向，3xx 作为普通响应交给调用方。"""

    def redirect_request(self, request, file, code, message, headers, new_url):
        return None


def parse_json(body: bytes, what: str):
    """解析平台统一响应信封；无法解析时按失败退出（不打印伪成功）。"""
    try:
        return json.loads(body.decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - 平台响应不可控，统一转成诊断
        common.fail("无法解析%s响应：%s" % (what, exc), 1)


class Session:
    """一次 CLI 调用使用的平台会话（Cookie 文件只读写本次临时副本）。"""

    def __init__(self, config: common.Config, temp_dir: str) -> None:
        self.config = config
        self.temp_dir = temp_dir
        self.cookie_file = ""
        self.owns_session = False
        self.jar = None
        self.opener = None

    # ---------------------------------------------------------------- Cookie
    def _open_cookie_jar(self) -> None:
        jar = cookiejar.MozillaCookieJar(self.cookie_file)
        if os.path.getsize(self.cookie_file) > 0:
            try:
                jar.load(ignore_discard=True, ignore_expires=True)
            except Exception:
                # 用户提供的文件不是规矩的 Netscape 格式时，不中断登录链路；
                # 但绝不能把调用方的会话文件写坏（原文件只读复制，见 ensure_authenticated）。
                common.log("WARN", "Cookie 文件不是可解析的 Netscape 格式，已按空会话继续")
                jar = cookiejar.MozillaCookieJar(self.cookie_file)
        self.jar = jar
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(jar), _NoRedirect()
        )

    def _save_cookies(self) -> None:
        if not self.cookie_file or self.jar is None:
            return
        try:
            self.jar.save(ignore_discard=True, ignore_expires=True)
            os.chmod(self.cookie_file, 0o600)
        except OSError:
            pass

    # ---------------------------------------------------------------- 请求
    def request(
        self, method: str, endpoint: str, payload: bytes = None, max_time: int = 900
    ):
        """发出一次平台请求，返回 (HTTP 状态码, 响应体字节)。

        连接失败抛 TransportError，由调用方决定诊断与退出方式（不同接口的提示不同）。
        """
        url = self.config.api_base_url + endpoint
        data = payload if (payload is not None and method != "GET") else None
        request = urllib.request.Request(url, data=data, method=method)
        # 与 curl 的参数序列一致：Accept / Origin / Content-Type 三个固定头。
        request.add_header("Accept", "application/json")
        request.add_header("Origin", self.config.platform_origin)
        request.add_header("Content-Type", "application/json")
        try:
            response = self.opener.open(request, timeout=max_time)
        except urllib.error.HTTPError as exc:
            status = exc.code
            body = exc.read()
            exc.close()
        except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
            raise TransportError(str(exc))
        else:
            status = response.getcode()
            body = response.read()
            response.close()
        self._save_cookies()
        return status, body

    def post_json(self, endpoint: str, payload: bytes = None, max_time: int = 900):
        return self.request("POST", endpoint, payload, max_time)

    def get(self, endpoint: str, max_time: int = 120):
        return self.request("GET", endpoint, None, max_time)

    def delete_json(self, endpoint: str, payload: bytes = None, max_time: int = 120):
        return self.request("DELETE", endpoint, payload, max_time)

    # ---------------------------------------------------------------- 认证
    def ensure_authenticated(self) -> None:
        if self.config.cookie_file:
            if not os.access(self.config.cookie_file, os.R_OK):
                common.fail("Cookie 文件不可读：%s" % self.config.cookie_file, 2)
            # 用户提供的 Cookie 文件只读复制到本次临时目录；登录、刷新或退出
            # 都不能覆盖调用方的会话文件。
            self.cookie_file = os.path.join(self.temp_dir, "cookies.txt")
            with open(self.config.cookie_file, "rb") as source:
                content = source.read()
            common.write_private(self.cookie_file, content)
            self._open_cookie_jar()
            return
        self.cookie_file = os.path.join(self.temp_dir, "cookies.txt")
        common.write_private(self.cookie_file, b"")
        self._open_cookie_jar()
        self.login()

    def _read_password(self) -> str:
        if self.config.password_stdin:
            return common.read_line("")
        if not sys.stdin.isatty():
            common.fail("标准输入不是终端；请使用 --password-stdin 或 --cookie-file", 2)
        # 与 Shell 的 `read -r -s -p` 一致：提示写 stderr，输入不回显，读完后补一个换行。
        try:
            password = getpass.getpass("平台管理员密码: ")
        except EOFError:
            password = ""
        except KeyboardInterrupt:
            raise common.CliExit(130)
        sys.stderr.write("\n")
        sys.stderr.flush()
        return password

    def login(self) -> None:
        password = self._read_password()
        if not password:
            common.fail("密码不能为空", 2)
        payload = json.dumps(
            {
                "account": self.config.account.strip(),
                "password": password,
                "login_type": "password",
                "replace_existing_session": self.config.replace_existing_session,
            },
            ensure_ascii=False,
        ).encode("utf-8")
        encrypted = self._login_via_credential_envelope(payload)
        if encrypted is None:
            try:
                status, body = self.post_json("/auth/login", payload)
            except TransportError:
                self.diagnose_connection_failure("/auth/login")
                raise common.CliExit(1)
        else:
            status, body = encrypted
        if status != 200:
            self.print_api_error(status, body, "/auth/login")
            raise common.CliExit(1)
        self.owns_session = True
        common.log("INFO", "平台管理员认证成功")

    def _login_via_credential_envelope(self, plaintext: bytes):
        """使用 uip-credential/v1 封套登录。

        返回 (状态码, 响应体)；返回 None 表示平台不支持加密（503/404/405），
        调用方回退明文登录；其余错误直接以退出码 1 结束并把诊断留给调用方文案。
        """
        challenge_response = os.path.join(self.temp_dir, "credential-challenge-response.json")
        envelope_request = os.path.join(self.temp_dir, "login-envelope.json")
        response_key_file = os.path.join(self.temp_dir, "credential-response-key")
        meta_file = os.path.join(self.temp_dir, "credential-envelope-meta.json")
        decrypted_response = os.path.join(self.temp_dir, "login-response-plain.json")
        payload_file = os.path.join(self.temp_dir, "login.json")
        response_file = os.path.join(self.temp_dir, "login-response.json")

        try:
            status, body = self.post_json("/auth/credential-challenges", b'{"purpose":"LOGIN","resource_id":""}')
        except TransportError:
            self.diagnose_connection_failure("/auth/credential-challenges")
            raise common.CliExit(1)
        common.write_private(challenge_response, body)
        if status in (503, 404, 405):
            return None
        if status != 200:
            self.print_api_error(status, body, "/auth/credential-challenges")
            raise common.CliExit(1)

        common.write_private(payload_file, plaintext)
        completed = subprocess.run(
            [
                sys.executable,
                CREDENTIAL_HELPER,
                "encrypt",
                "--challenge",
                challenge_response,
                "--plaintext",
                payload_file,
                "--purpose",
                "LOGIN",
                "--resource-id",
                "",
                "--content-type",
                "application/json",
                "--request-out",
                envelope_request,
                "--response-key-out",
                response_key_file,
                "--meta-out",
                meta_file,
            ]
        )
        common.wipe(payload_file)
        if completed.returncode != 0:
            common.fail("平台要求加密凭据，但本地无法完成 uip-credential/v1 封装；未提交登录", 1)

        with open(envelope_request, "rb") as handle:
            envelope = handle.read()
        try:
            status, body = self.post_json("/auth/login", envelope)
        except TransportError:
            self.diagnose_connection_failure("/auth/login")
            raise common.CliExit(1)
        common.wipe(envelope_request)
        common.write_private(response_file, body)

        completed = subprocess.run(
            [
                sys.executable,
                CREDENTIAL_HELPER,
                "decrypt",
                "--response",
                response_file,
                "--response-key",
                response_key_file,
                "--meta-file",
                meta_file,
                "--output",
                decrypted_response,
            ]
        )
        if completed.returncode != 0:
            common.fail("凭据响应无法解析；为避免重复提交，未重试登录", 1)
        common.wipe(response_key_file)
        with open(decrypted_response, "rb") as handle:
            return status, handle.read()

    def logout_owned_session(self) -> None:
        """只退出本次脚本自己登录的会话；复用 Cookie 文件时不动调用方会话。"""
        if not (self.owns_session and self.cookie_file and os.path.isfile(self.cookie_file)):
            return
        try:
            request = urllib.request.Request(
                self.config.api_base_url + "/auth/logout", data=b"", method="POST"
            )
            request.add_header("Accept", "application/json")
            request.add_header("Origin", self.config.platform_origin)
            self.opener.open(request, timeout=15).close()
        except Exception:  # noqa: BLE001 - 退出登录失败不改变原命令结果
            pass
        self.owns_session = False

    # ---------------------------------------------------------------- 诊断
    def diagnose_connection_failure(self, endpoint: str) -> None:
        common.log("ERROR", "无法连接平台接口：%s%s" % (self.config.api_base_url, endpoint))
        sys.stderr.write(DIAGNOSIS)
        sys.stderr.flush()

    def print_api_error(self, status, body: bytes, endpoint: str = "") -> None:
        """打印平台业务错误：业务码、追踪号、响应体相关建议（与 Shell 版本逐字一致）。"""
        try:
            payload = json.loads(body.decode("utf-8"))
        except Exception:  # noqa: BLE001 - 响应不可解析时给出统一诊断
            print(UNPARSABLE_RESPONSE % status, file=sys.stderr)
            return
        if not isinstance(payload, dict):
            print(UNPARSABLE_RESPONSE % status, file=sys.stderr)
            return
        code = payload.get("code") or "UNKNOWN"
        message = payload.get("message") or "请求失败"
        request_id = payload.get("request_id") or "-"
        details = payload.get("details") or {}
        print("HTTP %s %s：%s（追踪号：%s）" % (status, code, message, request_id), file=sys.stderr)
        status_text = str(status)
        if code == "AUTH_CONCURRENT_SESSION":
            print(
                "处理建议：该管理员账号已在另一终端保留有效会话。先在原终端退出；若原终端已关闭且确认需要抢占会话，"
                "使用同一命令追加 --replace-existing-session。该选项会使原会话立即失效。",
                file=sys.stderr,
            )
        elif code == "IAM_SUBSYSTEM_ALREADY_ONBOARDED":
            if isinstance(details, dict):
                application_code = details.get("application_code") or "-"
                environment = details.get("environment") or "-"
                state = details.get("status") or "-"
                next_action = details.get("next_action") or "请使用环境、登录目标或 OAuth 客户端的更新接口变更配置。"
                print(
                    "当前已存在：应用 %s，环境 %s（状态：%s）。%s"
                    % (application_code, environment, state, next_action),
                    file=sys.stderr,
                )
            print(
                "处理建议：该环境的控制面记录已经存在。子系统代码、镜像、前端、后端、功能模块和业务迁移的正常更新均不需要执行接入或撤销脚本，"
                "现有统一登录配置会保留。只有永久下线该环境时才使用 subsystem.sh offboard；若确需变更 BaseURL、UpstreamURL、PathPrefix 或 OAuth 回调，"
                "请走受控配置变更流程（subsystem.sh update 之前先 PATCH），不能通过撤销后重建绕过。若此前部署 Agent 失败，请先使用 subsystem.sh status 查询，"
                "再使用 subsystem.sh retry。",
                file=sys.stderr,
            )
        elif code in {"IAM_CONFLICT", "IAM_VERSION_CONFLICT"}:
            print(
                "处理建议：存在资源冲突或配置已变更。检查应用编码、环境、路径前缀和 OAuth Client 是否被其他接入占用；"
                "不要通过重复执行脚本覆盖现有配置。",
                file=sys.stderr,
            )
        elif code in {"PLATFORM_DEPENDENCY_UNAVAILABLE", "PLATFORM_INTERNAL_ERROR"} or status_text.startswith("5"):
            print(
                "处理建议：基础平台 API 或受控 provisioner 当前不可用。执行 `bash scripts/docker-local.sh ps`，"
                "再执行 `bash scripts/docker-local.sh logs api subsystem-provisioner`；如刚更新后端，执行 "
                "`bash scripts/docker-local.sh refresh-api` 后重试。",
                file=sys.stderr,
            )
        elif code in {"AUTH_UNAUTHENTICATED", "AUTH_SESSION_EXPIRED"} or status_text == "401":
            print(
                "处理建议：登录状态无效或 Cookie 已过期。不要复用旧 Cookie，改用 --account 重新登录；"
                "使用 --cookie-file 时请确认文件来自同一基础平台地址。",
                file=sys.stderr,
            )
        elif status_text == "403" and endpoint in {"/auth/login", "/auth/credential-challenges"}:
            print(
                "处理建议：登录请求被同源保护拒绝（%s 缺失或不匹配可信 Origin 时一律返回 403）。当前请求 Origin 为 %s；"
                "请确认它已配置在平台 APP_CORS_ALLOWED_ORIGINS 中，并与 OIDC_ISSUER 的 origin 一致。"
                "可使用 --platform-origin 或 BASIC_PLATFORM_ORIGIN 修正。"
                % (endpoint, self.config.platform_origin or "（空）"),
                file=sys.stderr,
            )
        elif status_text == "403":
            print(
                "处理建议：当前账号没有子系统接入所需的基础平台管理权限。请由超级管理员核对应用、环境、登录目标和 OAuth 客户端的创建/更新/删除权限；"
                "若使用 Cookie 会话，也请确认请求 Origin 已列入平台 APP_CORS_ALLOWED_ORIGINS。",
                file=sys.stderr,
            )
        elif status_text == "422":
            print(
                "处理建议：检查参数：BaseURL 只能是对外 origin；UpstreamURL 必须为网关可达的内网 http/https 地址；"
                "PathPrefix 必须是非根绝对路径。",
                file=sys.stderr,
            )
        elif code == "PLATFORM_METHOD_NOT_ALLOWED" or status_text == "405":
            print(
                "处理建议：运行中的 API 镜像可能未包含当前接口。执行 `bash scripts/docker-local.sh refresh-api` 后重试。",
                file=sys.stderr,
            )
        elif code == "IAM_ENVIRONMENT_DELETE_BLOCKED":
            print("该环境仍有配置命名空间或审计回执。请先按保留策略处理，脚本不提供 --force。", file=sys.stderr)
