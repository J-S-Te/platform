"""subsystem.sh status / update / offboard / onboard 摘要与通用参数校验的单元测试。

用途：在无 docker、无 mysql、无网络、无平台会话的条件下，用 127.0.0.1 回环桩服务验证
      subsystem.sh 的 status 解析（含拒绝 update 专属参数）、update 的「发起 → 轮询直到终态」
      与「超时/失败/等待预算非法」分支、offboard 的清理顺序、确认门槛与「基础设施已拆、
      DB 未清」半完成态的续跑（--skip-teardown），onboard 成功/失败摘要的 stdout/stderr 流向，
      以及 common 库的参数校验退出码。
      容器/网关/DB 的真实清理由平台侧部署 Agent 执行，本测试只断言 CLI 发出的受控 API
      调用序列；PATH 中放置假 docker/rm/sleep 以证明 CLI 不做本地 docker 操作、不删除
      临时目录之外的路径，并能断言「先查询、后等待」的轮询节奏。
      connect 子命令已有独立测试（test_subsystem_connect_origin.py）。
参数：无。直接 `python3 scripts/test_subsystem_lifecycle.py` 运行。
返回值：unittest 退出码（0 表示全部通过，1 表示存在失败）。
"""
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import urlsplit
import json
import os
import re
import shutil
import subprocess
import tempfile
import threading
import unittest


ROOT = Path(__file__).resolve().parents[1]
SUBSYSTEM = ROOT / "scripts" / "subsystem.sh"

APPLICATION_CODE = "contract_management"
ENVIRONMENT = "prod"
APPLICATION_ID = "app-01HXTEST"
ENVIRONMENT_ID = "env-01HXTEST"
ENVIRONMENT_VERSION = 3

STUB_PASSWORD = "stub-admin-password"


def _envelope(data, request_id="req-subsystem", code="OK", message="ok"):
    """构造平台统一响应信封。

    参数：data —— data 字段内容；request_id —— 追踪号；code/message —— 业务码与说明。
    返回值：dict，可被脚本的 python3 解析片段直接读取。
    """
    return {"code": code, "message": message, "request_id": request_id, "data": data}


def _status_body(status, **extra):
    """构造 /subsystem-status 的响应体（应用 + 环境 + 部署状态）。

    参数：status —— 部署状态字符串；extra —— 额外覆盖字段（如 last_error）。
    返回值：dict 响应信封。
    """
    data = {
        "application_code": APPLICATION_CODE,
        "environment": ENVIRONMENT,
        "status": status,
        "operation": "UPDATE",
        "generation": 3,
        "attempt_count": 1,
        "updated_at": "2026-02-01T03:04:05Z",
    }
    data.update(extra)
    return _envelope(data)


DEFAULT_RESPONSES = {
    "challenge": (404, {"code": "NOT_FOUND", "message": "未启用凭据加密", "request_id": "req-challenge"}),
    "login": (200, {"code": "OK", "message": "登录成功", "request_id": "req-login"}),
    "logout": (200, {"code": "OK", "message": "已退出", "request_id": "req-logout"}),
    "status": (200, _status_body("READY", public_url="https://contract.example.test")),
    "update": (202, _envelope({"status": "UPDATING"}, request_id="req-update")),
    "retry": (202, _envelope({"status": "UPDATING"}, request_id="req-retry")),
    "adoption": (202, _envelope({"status": "UPDATING"}, request_id="req-adopt")),
    "onboarding": (202, _envelope({"status": "PROVISIONING"}, request_id="req-onboard")),
    "teardown": (200, _envelope({"status": "OFFBOARDED"}, request_id="req-teardown")),
    "applications": (200, _envelope({"items": [{"application_id": APPLICATION_ID, "code": APPLICATION_CODE}],
                                     "total": 1}, request_id="req-apps")),
    "environments": (200, _envelope({"items": [{"environment": ENVIRONMENT,
                                                "environment_id": ENVIRONMENT_ID,
                                                "version": ENVIRONMENT_VERSION}],
                                     "total": 1}, request_id="req-envs")),
    "delete_environment": (200, _envelope({}, request_id="req-del-env")),
    "delete_application": (200, _envelope({}, request_id="req-del-app")),
}


class _StubPlatform:
    """回环桩：按路由记录请求并可回放「响应序列」，用于模拟轮询/多页场景。"""

    def __init__(self):
        self.hits = []
        self.responses = dict(DEFAULT_RESPONSES)
        self._cursors = {}
        self.event_log = None  # 由测试注入：与假 sleep 共用的时序事件文件，用于断言「先查询后等待」
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def _dispatch(self, method):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                route = urlsplit(self.path).path
                hit = {
                    "method": method,
                    "path": self.path,
                    "route": route,
                    "key": _route_key(method, route),
                    "headers": dict(self.headers),
                    "body": raw.decode("utf-8", "replace"),
                }
                outer.hits.append(hit)
                if outer.event_log and hit["key"] == "status":
                    # 与假 sleep 写同一个文件，得到「查询 / 等待」的真实交错顺序。
                    with open(outer.event_log, "a", encoding="utf-8") as handle:
                        handle.write("status\n")
                status, payload = outer.next_response(hit["key"])
                data = json.dumps(payload).encode()
                self.send_response(status)
                if hit["key"] == "login":
                    self.send_header("Set-Cookie", "stub-session=1; Path=/; HttpOnly")
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):  # noqa: N802 - 按 HTTP 方法命名
                self._dispatch("GET")

            def do_POST(self):  # noqa: N802 - 按 HTTP 方法命名
                self._dispatch("POST")

            def do_DELETE(self):  # noqa: N802 - 按 HTTP 方法命名
                self._dispatch("DELETE")

            def log_message(self, *args):  # 静音桩日志
                return

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self._server.server_address[1]
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
        )
        self._thread.start()

    def next_response(self, key):
        """取出该路由的下一条响应；序列用尽后重复最后一条，未定义路由回 404。"""
        if key is None:
            return 404, {"code": "NOT_FOUND", "message": "stub 未定义该路由", "request_id": "req-404"}
        spec = self.responses.get(key, (404, {"code": "NOT_FOUND", "message": "未定义", "request_id": "req-404"}))
        if isinstance(spec, list):
            index = min(self._cursors.get(key, 0), len(spec) - 1)
            self._cursors[key] = self._cursors.get(key, 0) + 1
            return spec[index]
        return spec

    @property
    def api_base(self):
        return "http://127.0.0.1:%d/api/v1" % self.port

    @property
    def origin(self):
        return "http://127.0.0.1:%d" % self.port

    def hits_of(self, key):
        return [hit for hit in self.hits if hit["key"] == key]

    def business(self):
        """排除认证相关请求（挑战/登录/退出），只留子命令自身的业务调用，便于断言顺序。"""
        return [hit for hit in self.hits if hit["key"] not in {"challenge", "login", "logout"}]

    def close(self):
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def reset(self):
        """清空已记录的请求与响应序列游标，用于「首轮失败 → 续跑」这类多段场景。"""
        self.hits = []
        self._cursors = {}


def _route_key(method, route):
    """把 (方法, 路径) 归一化为响应表的键；无法识别时返回 None。"""
    if method == "POST" and route == "/api/v1/auth/credential-challenges":
        return "challenge"
    if method == "POST" and route == "/api/v1/auth/login":
        return "login"
    if method == "POST" and route == "/api/v1/auth/logout":
        return "logout"
    if method == "GET" and route == "/api/v1/subsystem-status":
        return "status"
    if method == "POST" and route == "/api/v1/subsystem-update":
        return "update"
    if method == "POST" and route == "/api/v1/subsystem-retry":
        return "retry"
    if method == "POST" and route == "/api/v1/subsystem-adoption":
        return "adoption"
    if method == "POST" and route == "/api/v1/subsystem-onboarding":
        return "onboarding"
    if method == "POST" and route == "/api/v1/subsystem-teardown":
        return "teardown"
    if method == "GET" and route == "/api/v1/applications":
        return "applications"
    if method == "GET" and re.fullmatch(r"/api/v1/applications/[^/]+/environments", route):
        return "environments"
    if method == "DELETE" and re.fullmatch(r"/api/v1/applications/[^/]+/environments/[^/]+", route):
        return "delete_environment"
    if method == "DELETE" and re.fullmatch(r"/api/v1/applications/[^/]+", route):
        return "delete_application"
    return None


class _StubEnvironment:
    """构造隔离沙箱：临时 TMPDIR、假 docker/rm/sleep 与哨兵文件。"""

    def __init__(self, root):
        self.root = Path(root)
        self.tmp_dir = self.root / "tmp"
        self.log_dir = self.root / "logs"
        self.repo_dir = self.root / "repo"
        self.bin_dir = self.root / "bin"
        for path in (self.tmp_dir, self.log_dir, self.repo_dir, self.bin_dir):
            path.mkdir()
        self.docker_log = self.log_dir / "docker-argv.log"
        self.rm_log = self.log_dir / "rm-argv.log"
        self.event_log = self.log_dir / "events.log"
        # 哨兵：CLI 绝不能删除临时目录之外的任何路径。
        self.keep_file = self.repo_dir / "keep.env"
        self.keep_file.write_text("KEEP=1\n", encoding="utf-8")
        self._write_stub("docker", '#!/bin/sh\nprintf "%s\\n" "$*" >> "$STUB_DOCKER_LOG"\nexit 0\n')
        # 假 sleep 立即返回，但把自己的参数追加到事件文件，便于断言轮询节奏。
        self._write_stub(
            "sleep",
            '#!/bin/sh\nprintf "sleep %s\\n" "$*" >> "$STUB_EVENT_LOG"\nexit 0\n',
        )
        self._write_stub(
            "rm",
            '#!/bin/sh\nprintf "%s\\n" "$*" >> "$STUB_RM_LOG"\nexec "$STUB_REAL_RM" "$@"\n',
        )

    def _write_stub(self, name, body):
        path = self.bin_dir / name
        path.write_text(body, encoding="utf-8")
        path.chmod(0o755)

    def docker_calls(self):
        """返回假 docker 收到的 argv 行；文件不存在表示从未调用。"""
        if not self.docker_log.exists():
            return []
        return [line for line in self.docker_log.read_text(encoding="utf-8").splitlines() if line]

    def rm_calls(self):
        """返回假 rm 收到的 argv 行；文件不存在表示从未调用。"""
        if not self.rm_log.exists():
            return []
        return [line for line in self.rm_log.read_text(encoding="utf-8").splitlines() if line]

    def events(self):
        """返回「状态查询」与「sleep 等待」的真实交错序列（按发生顺序）。"""
        if not self.event_log.exists():
            return []
        return [line for line in self.event_log.read_text(encoding="utf-8").splitlines() if line]


class SubsystemLifecycleTest(unittest.TestCase):
    """子命令级端到端（桩服务）断言。"""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="subsystem-test-")
        self.sandbox = _StubEnvironment(self.root)
        self.stub = _StubPlatform()
        self.stub.event_log = str(self.sandbox.event_log)
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        self.stub.close()
        shutil.rmtree(self.root, ignore_errors=True)

    # ---- 运行辅助 ----
    def _run(self, args, stdin_text="", wait_timeout="3"):
        """在隔离沙箱中调用 subsystem.sh，返回 CompletedProcess。

        参数：args —— 子命令与参数；stdin_text —— 写入 stdin 的内容；
              wait_timeout —— 注入 DEPLOY_WAIT_TIMEOUT，避免轮询跑到默认 900 秒。
        返回值：subprocess.CompletedProcess（text 模式）。
        """
        env = os.environ.copy()
        env["TMPDIR"] = str(self.sandbox.tmp_dir)
        env["PATH"] = "%s:%s" % (self.sandbox.bin_dir, env.get("PATH", ""))
        env["STUB_DOCKER_LOG"] = str(self.sandbox.docker_log)
        env["STUB_RM_LOG"] = str(self.sandbox.rm_log)
        env["STUB_EVENT_LOG"] = str(self.sandbox.event_log)
        env["STUB_REAL_RM"] = shutil.which("rm") or "/bin/rm"
        env["DEPLOY_WAIT_TIMEOUT"] = wait_timeout
        for name in ("BASIC_PLATFORM_API_BASE_URL", "BASIC_PLATFORM_ORIGIN",
                     "BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API",
                     "BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS"):
            env.pop(name, None)
        return subprocess.run(
            ["bash", str(SUBSYSTEM), *args], input=stdin_text, capture_output=True,
            text=True, env=env, timeout=60,
        )

    def _auth_args(self):
        return ["--api-base-url", self.stub.api_base, "--account", "admin", "--password-stdin"]

    def _update_args(self, extra=()):
        return ["update", *self._auth_args(),
                "--application-code", APPLICATION_CODE, "--environment", ENVIRONMENT,
                "--yes", *extra]

    def _offboard_args(self, extra=()):
        return ["offboard", *self._auth_args(),
                "--application-code", APPLICATION_CODE, "--environment", ENVIRONMENT,
                "--confirm", "%s/%s" % (APPLICATION_CODE, ENVIRONMENT), "--yes", *extra]

    def _onboard_args(self, extra=()):
        return ["onboard", *self._auth_args(),
                "--application-code", APPLICATION_CODE, "--application-name", "合同管理系统",
                "--environment", ENVIRONMENT,
                "--public-base-url", self.stub.origin,
                "--upstream-url", "http://contract-api:8081",
                "--path-prefix", "/contract_management",
                "--yes", *extra]

    def _poll_events(self):
        """返回本轮查询/等待事件序列，并把 sleep 形状的条目归一化为 ("sleep", 秒数)。"""
        events = []
        for line in self.sandbox.events():
            parts = line.split()
            if not parts:
                continue
            events.append(("sleep", parts[1]) if parts[0] == "sleep" else (parts[0], None))
        return events

    @staticmethod
    def _json(hit):
        return json.loads(hit["body"])

    def _assert_no_side_effects(self):
        """校验无副作用：无 docker 调用、无平台请求、rm 只清理自身临时目录、退出后目录为空。"""
        self.assertEqual([], self.sandbox.docker_calls(), "不应在本地调用 docker")
        self.assertEqual([], self.stub.hits, "不应向平台发出请求")
        self._assert_rm_within_temp_dir()
        self.assertEqual([], sorted(p.name for p in self.sandbox.tmp_dir.iterdir()), "临时目录未清空")

    def _assert_rm_within_temp_dir(self):
        """断言假 rm 收到的每个非选项参数都位于测试自建的 TMPDIR 内。"""
        for line in self.sandbox.rm_calls():
            for token in line.split():
                if token.startswith("-"):
                    continue
                self.assertTrue(token.startswith(str(self.sandbox.tmp_dir)),
                                "rm 目标越界：%s" % token)

    # ============================ 1. status ============================
    def test_status_prints_parsed_agent_state(self):
        """status 解析部署 Agent 状态 JSON 并逐项打印（中文标签 + 追踪号）。"""
        result = self._run(["status", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT], STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("子系统部署状态：", result.stdout)
        self.assertIn("  应用：%s" % APPLICATION_CODE, result.stdout)
        self.assertIn("  环境：%s" % ENVIRONMENT, result.stdout)
        self.assertIn("  状态：READY", result.stdout)
        self.assertIn("  最近操作：UPDATE", result.stdout)
        self.assertIn("  代次：3", result.stdout)
        self.assertIn("  追踪号：req-subsystem", result.stdout)

        status_hits = self.stub.hits_of("status")
        self.assertEqual(1, len(status_hits))
        self.assertEqual("GET", status_hits[0]["method"])
        self.assertIn("application_code=%s" % APPLICATION_CODE, status_hits[0]["path"])
        self.assertIn("environment=%s" % ENVIRONMENT, status_hits[0]["path"])
        self.assertEqual(self.stub.origin, status_hits[0]["headers"].get("Origin"))
        self.assertNotIn(STUB_PASSWORD, result.stdout + result.stderr)

    def test_status_prints_failed_state_with_error_fields(self):
        """PROVISION_FAILED 状态会打印错误编码与错误说明，便于直接定位失败原因。"""
        self.stub.responses["status"] = (200, _status_body(
            "PROVISION_FAILED", operation="RETRY", generation=4, attempt_count=2,
            last_error_code="PROVISION_TIMEOUT", last_error="等待子系统健康检查超时"))
        result = self._run(["status", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT], STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("  状态：PROVISION_FAILED", result.stdout)
        self.assertIn("  错误编码：PROVISION_TIMEOUT", result.stdout)
        self.assertIn("  错误说明：等待子系统健康检查超时", result.stdout)
        self.assertIn("  尝试次数：2", result.stdout)

    def test_status_non_200_exits_nonzero_with_diagnostic(self):
        """status 接口返回 404 时退出码 1，stderr 带状态码与追踪号，不打印伪成功。"""
        self.stub.responses["status"] = (404, {"code": "IAM_ENVIRONMENT_NOT_FOUND",
                                               "message": "环境不存在",
                                               "request_id": "req-status-404"})
        result = self._run(["status", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT], STUB_PASSWORD + "\n")
        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("HTTP 404 IAM_ENVIRONMENT_NOT_FOUND", result.stderr)
        self.assertIn("req-status-404", result.stderr)
        self.assertNotIn("子系统部署状态：", result.stdout)

    def test_status_rejects_update_only_issuer_alias(self):
        """status 不接受 --issuer-alias（update/retry/adopt 专属）：退出 2 且零 HTTP。"""
        result = self._run(["status", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT,
                            "--issuer-alias", "keycloak"], STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("status 不接受 --issuer-alias", result.stderr)
        self.assertNotIn("子系统部署状态：", result.stdout)
        self._assert_no_side_effects()

    # ============================ 2. update ============================
    def test_update_accepts_202_then_polls_until_ready(self):
        """update 收到 202 后轮询 /subsystem-status，READY 时打印成功摘要并退出 0。"""
        self.stub.responses["status"] = [
            (200, _status_body("UPDATING")),
            (200, _status_body("READY", public_url="https://contract.example.test")),
        ]
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("子系统已重新部署：", result.stdout)
        self.assertIn("  状态：READY", result.stdout)
        self.assertIn("  对外访问地址：https://contract.example.test", result.stdout)
        self.assertIn("  追踪号：req-subsystem", result.stdout)

        update_hit = self.stub.hits_of("update")[0]
        self.assertEqual("POST", update_hit["method"])
        self.assertEqual({"application_code": APPLICATION_CODE, "environment": ENVIRONMENT},
                         self._json(update_hit))
        self.assertEqual("application/json", update_hit["headers"].get("Content-Type"))
        self.assertEqual(self.stub.origin, update_hit["headers"].get("Origin"))
        self.assertIn("stub-session=1", update_hit["headers"].get("Cookie") or "")
        self.assertNotIn(STUB_PASSWORD, result.stdout + result.stderr)
        # 至少轮询两次：UPDATING 之后才 READY。
        self.assertGreaterEqual(len(self.stub.hits_of("status")), 2)

        # 轮询节奏：第一次查询立即发出（首次查询前没有 sleep），两次查询之间才有 sleep 5。
        events = self._poll_events()
        self.assertTrue(events, "轮询应至少发生一次状态查询")
        self.assertEqual(("status", None), events[0],
                         "首次查询前不应 sleep（旧实现固定先空等 5 秒）：%s" % events)
        status_indexes = [i for i, (kind, _) in enumerate(events) if kind == "status"]
        sleep_indexes = [i for i, (kind, _) in enumerate(events) if kind == "sleep"]
        self.assertGreaterEqual(len(status_indexes), 2)
        self.assertTrue(sleep_indexes, "两次查询之间应至少等待一次")
        for earlier, later in zip(status_indexes, status_indexes[1:]):
            self.assertTrue(
                any(earlier < index < later for index in sleep_indexes),
                "两次查询之间必须有 sleep：%s" % events,
            )
        self.assertEqual({"5"}, {events[index][1] for index in sleep_indexes},
                         "轮询间隔必须是 5 秒：%s" % events)

    def test_update_ignores_stale_ready_terminal_from_previous_round(self):
        """202 受理后首次查询可能读到上一轮的旧 READY：必须等观察到处理中状态后才认本次成功。"""
        self.stub.responses["status"] = [
            (200, _status_body("READY", public_url="https://stale.example.test")),
            (200, _status_body("UPDATING")),
            (200, _status_body("READY", public_url="https://after-update.example.test")),
        ]
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("https://after-update.example.test", result.stdout,
                      "应采用观察到处理中之后的新终态，而不是旧 READY")
        self.assertNotIn("https://stale.example.test", result.stdout)
        self.assertGreaterEqual(len(self.stub.hits_of("status")), 3)

    def test_update_ignores_stale_failed_terminal_from_previous_round(self):
        """上一轮的 PROVISION_FAILED 不能被当成本轮失败：观察到处理中后按新终态判定。"""
        self.stub.responses["status"] = [
            (200, _status_body("PROVISION_FAILED", last_error_code="PROVISION_FAILED",
                               last_error="上一次的旧失败")),
            (200, _status_body("UPDATING")),
            (200, _status_body("READY", public_url="https://contract.example.test")),
        ]
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertNotIn("后台部署失败：", result.stderr)
        self.assertNotIn("上一次的旧失败", result.stderr)
        self.assertIn("子系统已重新部署：", result.stdout)

    def test_update_defers_ambiguous_terminal_until_confirmed(self):
        """未观察到处理中状态且一直 READY 时，先多确认一次再判定成功，避免旧终态误判。"""
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("子系统已重新部署：", result.stdout)
        self.assertGreaterEqual(len(self.stub.hits_of("status")), 2,
                                "终态需要一次确认，不能首次查询就下结论")

    def test_update_retry_mode_uses_retry_endpoint(self):
        """retry 子命令走 /subsystem-retry 而不是 /subsystem-update。"""
        self.stub.responses["retry"] = (200, _envelope({"status": "READY"},
                                                       request_id="req-retry-ok"))
        result = self._run(["retry", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT, "--yes"], STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(1, len(self.stub.hits_of("retry")))
        self.assertEqual([], self.stub.hits_of("update"))
        self.assertEqual([], self.stub.hits_of("status"), "同步 200 响应不需要轮询")

    def test_update_polls_to_failure_state_and_exits_nonzero(self):
        """轮询到 PROVISION_FAILED 时把失败摘要（含错误码/说明）写到 stderr 并以退出码 1 结束。"""
        self.stub.responses["status"] = (200, _status_body(
            "PROVISION_FAILED", last_error_code="PROVISION_FAILED",
            last_error="容器启动失败", next_action="修复后使用 retry 子命令重试"))
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")

        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("后台部署失败：", result.stderr)
        self.assertIn("  状态：PROVISION_FAILED", result.stderr)
        self.assertIn("  错误说明：容器启动失败", result.stderr)
        self.assertNotIn("后台部署失败：", result.stdout, "失败摘要不应混入 stdout")
        self.assertNotIn("子系统已重新部署：", result.stdout)
        self.assertNotIn("等待部署完成超时", result.stderr)

    def test_update_wait_timeout_reports_still_running(self):
        """轮询在 DEPLOY_WAIT_TIMEOUT 内未进终态时退出 1，并提示可用 status 继续查询。"""
        self.stub.responses["status"] = (200, _status_body("UPDATING"))
        result = self._run(self._update_args(), STUB_PASSWORD + "\n", wait_timeout="1")

        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("等待部署完成超时", result.stderr)
        self.assertIn("status 子命令继续查询", result.stderr)
        self.assertNotIn("子系统已重新部署：", result.stdout)
        states = {hit["path"] for hit in self.stub.hits_of("status")}
        self.assertEqual({"status"}, {_route_key("GET", urlsplit(path).path) for path in states})

    def test_update_rejects_invalid_deploy_wait_timeout(self):
        """DEPLOY_WAIT_TIMEOUT 非纯数字或为 0 时退出 2，且在任何 HTTP 请求之前就失败。"""
        cases = [
            ("abc", "DEPLOY_WAIT_TIMEOUT 必须是纯数字秒数"),
            ("5m", "DEPLOY_WAIT_TIMEOUT 必须是纯数字秒数"),
            ("0", "DEPLOY_WAIT_TIMEOUT 必须大于 0 秒"),
        ]
        for value, expected in cases:
            with self.subTest(value=value):
                # bash 算术会把 "abc"/"5m" 求值为 0，旧实现因此静默立即超时并伪装成部署慢。
                result = self._run(self._update_args(), STUB_PASSWORD + "\n", wait_timeout=value)
                self.assertEqual(2, result.returncode, result.stdout + result.stderr)
                self.assertIn(expected, result.stderr)
                self.assertIn(value, result.stderr, "错误文案必须回显非法值")
                self._assert_no_side_effects()

    def test_update_non_202_error_exits_without_polling(self):
        """update 返回 409 时退出 1，打印业务码与追踪号，且不再轮询状态。"""
        self.stub.responses["update"] = (409, {"code": "IAM_CONFLICT",
                                               "message": "该环境正在部署中",
                                               "request_id": "req-conflict"})
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")
        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("HTTP 409 IAM_CONFLICT", result.stderr)
        self.assertIn("req-conflict", result.stderr)
        self.assertEqual([], self.stub.hits_of("status"))

    def test_update_sync_200_prints_success_without_polling(self):
        """平台同步返回 200 时直接打印成功摘要，不进入轮询。"""
        self.stub.responses["update"] = (200, _envelope(
            {"status": "READY", "public_url": "https://contract.example.test"},
            request_id="req-sync"))
        result = self._run(self._update_args(), STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("  追踪号：req-sync", result.stdout)
        self.assertEqual([], self.stub.hits_of("status"))

    def test_update_cancel_without_confirmation_calls_no_api(self):
        """非交互且未给 -y 时，update 在确认环节取消：退出 0，不发任何平台请求。"""
        result = self._run(["update", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT], "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("已取消", result.stderr)
        self._assert_no_side_effects()

    def test_update_sends_issuer_alias_when_given(self):
        """显式 --issuer-alias 才会写进 update 请求体。"""
        self.stub.responses["update"] = (200, _envelope({"status": "READY"}, request_id="req-alias"))
        result = self._run(self._update_args(["--issuer-alias", "keycloak"]), STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(
            {"application_code": APPLICATION_CODE, "environment": ENVIRONMENT, "issuer_alias": "keycloak"},
            self._json(self.stub.hits_of("update")[0]),
        )

    # ============================ 3. offboard ============================
    def test_offboard_deep_cleanup_order_and_payloads(self):
        """默认深清理顺序固定为：teardown（容器/env.local/网关）→ 删除环境 DB 记录。"""
        result = self._run(self._offboard_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(
            [("GET", "/api/v1/applications"),
             ("GET", "/api/v1/applications/%s/environments" % APPLICATION_ID),
             ("POST", "/api/v1/subsystem-teardown"),
             ("DELETE", "/api/v1/applications/%s/environments/%s" % (APPLICATION_ID, ENVIRONMENT_ID))],
            [(hit["method"], hit["route"]) for hit in self.stub.business()],
        )
        teardown = self.stub.hits_of("teardown")[0]
        self.assertEqual({"application_code": APPLICATION_CODE, "environment": ENVIRONMENT},
                         self._json(teardown))
        self.assertEqual(self.stub.origin, teardown["headers"].get("Origin"))
        delete = self.stub.hits_of("delete_environment")[0]
        self.assertEqual({"confirmation_code": "%s/%s" % (APPLICATION_CODE, ENVIRONMENT),
                          "version": ENVIRONMENT_VERSION}, self._json(delete))
        # 先查应用与环境（定位 id/version），再清理；顺序在 hits 里体现。
        self.assertLess(self.stub.hits.index(self.stub.hits_of("applications")[0]),
                        self.stub.hits.index(teardown))
        self.assertLess(self.stub.hits.index(self.stub.hits_of("environments")[0]),
                        self.stub.hits.index(delete))
        self.assertIn("已清理：停止容器 + 删除 .env.local + 移除门户网关入口", result.stdout)
        self.assertNotIn(STUB_PASSWORD, result.stdout + result.stderr)
        # 清理动作由平台部署 Agent 在服务端执行，CLI 本身不调 docker，只清理自己的临时文件。
        self.assertEqual([], self.sandbox.docker_calls())
        self._assert_rm_within_temp_dir()

    def test_offboard_leaves_local_env_and_gateway_cleanup_to_platform_agent(self):
        """容器 / .env.local / 网关 include 由平台部署 Agent 在服务端拆解，
        CLI 只在本地发起 teardown 请求；哨兵断言部署目录里的这些文件不会被脚本删除。"""
        env_local = self.sandbox.repo_dir / ".env.local"
        env_local.write_text("CONTRACT_DB_DSN=stub\n", encoding="utf-8")
        gateway_dir = self.sandbox.repo_dir / "gateway" / "conf.d"
        gateway_dir.mkdir(parents=True)
        include = gateway_dir / "contract_management-prod.conf"
        include.write_text("location /contract/ { proxy_pass http://stub; }\n", encoding="utf-8")

        result = self._run(self._offboard_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stderr)
        teardown_hits = self.stub.hits_of("teardown")
        self.assertEqual(1, len(teardown_hits), "停容器/删 .env.local/删网关应由一次 teardown 覆盖")
        self.assertEqual({"application_code": APPLICATION_CODE, "environment": ENVIRONMENT},
                         self._json(teardown_hits[0]))
        self.assertIn("深清理 %s/%s：停止容器、删除 .env.local、移除门户网关入口"
                      % (APPLICATION_CODE, ENVIRONMENT), result.stderr)
        self.assertTrue(env_local.exists(), ".env.local 不应被 CLI 在本地删除")
        self.assertTrue(include.exists(), "网关 include 不应被 CLI 在本地删除")
        self.assertEqual([], self.sandbox.docker_calls(), "CLI 不应直接调用 docker")
        self._assert_rm_within_temp_dir()

    def test_offboard_shallow_mode_skips_teardown(self):
        """--shallow 只删 DB 记录，不调用 teardown（容器/env.local/网关保留）。"""
        result = self._run(self._offboard_args(["--shallow"]), STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("模式：shallow", result.stderr)
        self.assertEqual([], self.stub.hits_of("teardown"))
        self.assertEqual(1, len(self.stub.hits_of("delete_environment")))

    def test_offboard_requires_exact_confirmation_code(self):
        """--confirm 必须精确等于 code/环境，否则退出 2 且不发任何请求。"""
        result = self._run(["offboard", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT,
                            "--confirm", "contract_management/staging", "--yes"], STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("--confirm 必须精确等于", result.stderr)
        self._assert_no_side_effects()

    def test_offboard_missing_confirmation_exits_two(self):
        """缺少 --confirm 时退出 2，不进入认证与清理流程。"""
        result = self._run(["offboard", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT, "--yes"], STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("offboard 必须提供 --application-code、--environment、--confirm", result.stderr)
        self._assert_no_side_effects()

    def test_offboard_rejects_dev_and_platform_environment(self):
        """dev 环境与基础平台 platform 都被拒绝（退出 2，无副作用）。"""
        cases = [
            (["--application-code", APPLICATION_CODE, "--environment", "dev",
              "--confirm", "%s/dev" % APPLICATION_CODE], "不允许使用 dev"),
            (["--application-code", "platform", "--environment", ENVIRONMENT,
              "--confirm", "platform/%s" % ENVIRONMENT], "不允许通过本脚本删除基础平台"),
        ]
        for extra, expected in cases:
            with self.subTest(expected=expected):
                result = self._run(["offboard", *self._auth_args(), *extra, "--yes"],
                                   STUB_PASSWORD + "\n")
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn(expected, result.stderr)
                self._assert_no_side_effects()

    def test_offboard_delete_failure_prints_resumable_command(self):
        """teardown 成功但 DELETE 失败时，错误信息要说明半完成态并给出可续跑命令。"""
        self.stub.responses["delete_environment"] = (500, {
            "code": "INTERNAL", "message": "删除环境失败", "request_id": "req-del-500"})
        result = self._run(self._offboard_args(), STUB_PASSWORD + "\n")

        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("HTTP 500 INTERNAL", result.stderr)
        # 明确半完成态语义：基础设施已拆，DB 记录仍在。
        self.assertIn("基础设施已拆", result.stderr)
        self.assertIn("DB 记录仍未清理", result.stderr)
        # 可续跑命令包含全部必要参数；--skip-teardown 保证重跑不重复 teardown。
        for fragment in ("bash scripts/subsystem.sh offboard",
                         "--application-code %s" % APPLICATION_CODE,
                         "--environment %s" % ENVIRONMENT,
                         "--confirm %s/%s" % (APPLICATION_CODE, ENVIRONMENT),
                         "--skip-teardown"):
            self.assertIn(fragment, result.stderr)
        # 续跑命令仍受确认门槛约束（不因续跑而绕过 -y/--confirm）。
        self.assertIn("--confirm", result.stderr)
        self.assertNotIn("子系统环境接入已撤销：", result.stdout)
        self.assertEqual(1, len(self.stub.hits_of("teardown")))
        self.assertEqual(1, len(self.stub.hits_of("delete_environment")))

    def test_offboard_skip_teardown_resumes_with_delete_only(self):
        """带 --skip-teardown 重跑：零 teardown 调用，只补发 DELETE 删除 DB 记录。"""
        self.stub.responses["delete_environment"] = (500, {
            "code": "INTERNAL", "message": "删除环境失败", "request_id": "req-del-500"})
        first = self._run(self._offboard_args(), STUB_PASSWORD + "\n")
        self.assertEqual(1, first.returncode, first.stdout + first.stderr)

        # 模拟平台恢复：清空上一轮记录，只保留 DB 删除成功。
        self.stub.reset()
        self.stub.responses["delete_environment"] = (200, _envelope({}, request_id="req-del-env"))

        result = self._run(self._offboard_args(["--skip-teardown"]), STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("模式：--skip-teardown（续跑）", result.stderr)
        self.assertEqual([], self.stub.hits_of("teardown"), "--skip-teardown 不得重复 teardown")
        self.assertEqual(1, len(self.stub.hits_of("delete_environment")))
        self.assertEqual(
            [("GET", "/api/v1/applications"),
             ("GET", "/api/v1/applications/%s/environments" % APPLICATION_ID),
             ("DELETE", "/api/v1/applications/%s/environments/%s" % (APPLICATION_ID, ENVIRONMENT_ID))],
            [(hit["method"], hit["route"]) for hit in self.stub.business()],
        )
        self.assertEqual({"confirmation_code": "%s/%s" % (APPLICATION_CODE, ENVIRONMENT),
                          "version": ENVIRONMENT_VERSION},
                         self._json(self.stub.hits_of("delete_environment")[0]))
        self.assertIn("子系统环境接入已撤销：", result.stdout)

    def test_offboard_skip_teardown_still_requires_confirmation(self):
        """--skip-teardown 不构成确认绕过：缺 --confirm 仍退出 2，未确认仍不调用任何接口。"""
        missing = self._run(["offboard", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT,
                            "--skip-teardown", "--yes"], STUB_PASSWORD + "\n")
        self.assertEqual(2, missing.returncode, missing.stdout + missing.stderr)
        self.assertIn("offboard 必须提供 --application-code、--environment、--confirm", missing.stderr)
        self._assert_no_side_effects()

        cancelled = self._run(["offboard", *self._auth_args(),
                               "--application-code", APPLICATION_CODE,
                               "--environment", ENVIRONMENT,
                               "--confirm", "%s/%s" % (APPLICATION_CODE, ENVIRONMENT),
                               "--skip-teardown"], "\n")
        self.assertEqual(0, cancelled.returncode, cancelled.stderr)
        self.assertIn("已取消", cancelled.stderr)
        self._assert_no_side_effects()

    # ============================ 3b. onboard 摘要流向 ============================
    def test_onboard_failure_summary_goes_to_stderr(self):
        """后台部署失败：失败摘要必须写 stderr，stdout 保持干净（便于脚本化消费）。"""
        self.stub.responses["status"] = (200, _status_body(
            "PROVISION_FAILED", last_error_code="PROVISION_FAILED",
            last_error="容器启动失败", next_action="修复后使用 retry 子命令重试"))
        result = self._run(self._onboard_args(), STUB_PASSWORD + "\n")

        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("后台部署失败：", result.stderr)
        self.assertIn("  应用/环境：%s / %s" % (APPLICATION_CODE, ENVIRONMENT), result.stderr)
        self.assertIn("  错误说明：容器启动失败", result.stderr)
        self.assertNotIn("后台部署失败：", result.stdout, "失败摘要不应写 stdout")
        self.assertNotIn("子系统接入完成", result.stdout)
        self.assertEqual(1, len(self.stub.hits_of("onboarding")))
        self.assertGreaterEqual(len(self.stub.hits_of("status")), 1)

    def test_onboard_success_summary_stays_on_stdout(self):
        """成功摘要保持写在 stdout（不随失败摘要一起改到 stderr）。"""
        self.stub.responses["status"] = (200, _status_body("READY"))
        result = self._run(self._onboard_args(), STUB_PASSWORD + "\n")

        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("子系统接入完成（后台编排终态）：", result.stdout)
        self.assertNotIn("后台部署失败：", result.stdout + result.stderr)

    def test_offboard_cancel_without_confirmation_calls_no_api(self):
        """未确认（非交互、无 -y）时默认不清理：退出 0，不调用任何清理接口。"""
        result = self._run(["offboard", *self._auth_args(),
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT,
                            "--confirm", "%s/%s" % (APPLICATION_CODE, ENVIRONMENT)], "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("已取消，未调用平台 API，未删除任何子系统配置", result.stderr)
        self._assert_no_side_effects()

    def test_offboard_teardown_failure_blocks_environment_deletion(self):
        """teardown 非 200 时退出 1，且不继续删除环境 DB 记录。"""
        self.stub.responses["teardown"] = (500, {"code": "PLATFORM_DEPENDENCY_UNAVAILABLE",
                                                 "message": "provisioner 不可用",
                                                 "request_id": "req-teardown-500"})
        result = self._run(self._offboard_args(), STUB_PASSWORD + "\n")
        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("HTTP 500 PLATFORM_DEPENDENCY_UNAVAILABLE", result.stderr)
        self.assertEqual([], self.stub.hits_of("delete_environment"))
        self.assertNotIn("子系统环境接入已撤销：", result.stdout)

    def test_offboard_unknown_application_deletes_nothing(self):
        """应用列表里找不到唯一匹配项时退出 1，不做任何删除。"""
        self.stub.responses["applications"] = (200, _envelope({"items": [], "total": 0},
                                                              request_id="req-apps-empty"))
        result = self._run(self._offboard_args(), STUB_PASSWORD + "\n")
        self.assertEqual(1, result.returncode, result.stdout + result.stderr)
        self.assertIn("未找到唯一的应用", result.stderr)
        self.assertEqual([], self.stub.hits_of("teardown"))
        self.assertEqual([], self.stub.hits_of("delete_environment"))

    def test_offboard_delete_application_refused_while_environments_remain(self):
        """--delete-application 在仍有其它环境时退出 2，且不删除 application。"""
        self.stub.responses["environments"] = [
            (200, _envelope({"items": [{"environment": ENVIRONMENT,
                                       "environment_id": ENVIRONMENT_ID,
                                       "version": ENVIRONMENT_VERSION}], "total": 1})),
            (200, _envelope({"items": [{"environment": "staging",
                                       "environment_id": "env-2",
                                       "version": 1}], "total": 1})),
        ]
        result = self._run(self._offboard_args(["--delete-application"]), STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("仍存在 1 个环境", result.stderr)
        self.assertEqual([], self.stub.hits_of("delete_application"))
        self.assertEqual(1, len(self.stub.hits_of("delete_environment")))

    def test_offboard_delete_application_after_last_environment(self):
        """--delete-application 在环境清空后追加删除 application（顺序在环境删除之后）。"""
        self.stub.responses["environments"] = [
            (200, _envelope({"items": [{"environment": ENVIRONMENT,
                                       "environment_id": ENVIRONMENT_ID,
                                       "version": ENVIRONMENT_VERSION}], "total": 1})),
            (200, _envelope({"items": [], "total": 0})),
        ]
        result = self._run(self._offboard_args(["--delete-application"]), STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        routes = [hit["route"] for hit in self.stub.business()]
        self.assertEqual(
            ["/api/v1/applications",
             "/api/v1/applications/app-01HXTEST/environments",
             "/api/v1/subsystem-teardown",
             "/api/v1/applications/app-01HXTEST/environments/env-01HXTEST",
             "/api/v1/applications/app-01HXTEST/environments",
             "/api/v1/applications/app-01HXTEST"],
            routes,
        )

    def test_offboard_removes_only_its_own_temp_files(self):
        """CLI 的 rm 只作用于自己创建的临时目录，仓库内任何路径都不会被删除。"""
        result = self._run(self._offboard_args(), STUB_PASSWORD + "\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertTrue(self.sandbox.keep_file.exists(), "临时目录之外的哨兵文件被删除")
        self._assert_rm_within_temp_dir()
        self.assertEqual([], sorted(p.name for p in self.sandbox.tmp_dir.iterdir()),
                         "退出后临时目录应被清理")

    # ============================ 4. common 参数校验 ============================
    def test_no_subcommand_prints_usage_and_exits_two(self):
        """无参数调用打印用法并以 2 退出，不产生副作用。"""
        result = self._run([])
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("用法：", result.stderr)
        self.assertIn("offboard", result.stderr)
        self._assert_no_side_effects()

    def test_unknown_subcommand_and_option_are_rejected(self):
        """未知子命令/未知选项都以 2 退出并给出用法，不会发出请求。"""
        cases = [
            (["frobnicate"], "未知子命令"),
            (["status", "--application-code", APPLICATION_CODE, "--nope"], "未知参数"),
            (["update", "--application-code", APPLICATION_CODE, "positional"], "未知参数：positional"),
        ]
        for args, expected in cases:
            with self.subTest(args=args):
                result = self._run(args)
                self.assertEqual(2, result.returncode, result.stdout + result.stderr)
                self.assertIn(expected, result.stderr)
                self._assert_no_side_effects()

    def test_missing_option_value_is_rejected(self):
        """选项缺少取值时以 2 退出并说明缺少参数名。"""
        cases = [
            (["update", "--application-code"], "--application-code 缺少参数"),
            (["status", "--api-base-url"], "--api-base-url 缺少参数"),
            (["offboard", "--confirm"], "--confirm 缺少参数"),
        ]
        for args, expected in cases:
            with self.subTest(args=args):
                result = self._run(args)
                self.assertEqual(2, result.returncode, result.stdout + result.stderr)
                self.assertIn(expected, result.stderr)
                self._assert_no_side_effects()

    def test_missing_required_configuration_is_rejected(self):
        """update/status/offboard 缺少必填配置时以 2 退出并打印各自用法。"""
        cases = [
            (["update", "--api-base-url", self.stub.api_base, "--account", "admin"], "必须提供 --application-code"),
            (["status", "--api-base-url", self.stub.api_base, "--account", "admin"], "必须提供 --application-code"),
            (["offboard", "--api-base-url", self.stub.api_base, "--account", "admin",
              "--application-code", APPLICATION_CODE], "必须提供 --application-code、--environment、--confirm"),
        ]
        for args, expected in cases:
            with self.subTest(args=args):
                result = self._run(args)
                self.assertEqual(2, result.returncode, result.stdout + result.stderr)
                self.assertIn(expected, result.stderr)
                self._assert_no_side_effects()

    def test_invalid_environment_value_is_rejected(self):
        """environment 只接受 dev/test/staging/prod（offboard 不接受 dev）。"""
        result = self._run(["update", *self._auth_args(),
                            "--application-code", APPLICATION_CODE, "--environment", "sandbox"],
                           STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("environment 只能是 dev、test、staging 或 prod", result.stderr)
        self._assert_no_side_effects()

    def test_invalid_application_code_is_rejected(self):
        """application-code 必须匹配 ^[a-z][a-z0-9._-]{0,63}$。"""
        result = self._run(["status", *self._auth_args(),
                            "--application-code", "Contract Management!",
                            "--environment", ENVIRONMENT], STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("application-code 必须匹配", result.stderr)
        self._assert_no_side_effects()

    def test_non_loopback_plain_http_api_is_rejected(self):
        """非回环地址必须用 HTTPS；明文 HTTP 在发请求前被拦下（2 退出）。"""
        result = self._run(["update", "--api-base-url", "http://192.0.2.10:8081/api/v1",
                            "--account", "admin", "--password-stdin",
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT, "--yes"], STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("必须使用 HTTPS", result.stderr)
        self._assert_no_side_effects()

    def test_cookie_file_conflicts_with_password_stdin(self):
        """--cookie-file 与 --password-stdin 互斥，在认证前就以 2 退出。"""
        cookie = Path(self.root) / "platform.cookie"
        cookie.write_text("# Netscape HTTP Cookie File\n", encoding="utf-8")
        result = self._run(["update", "--api-base-url", self.stub.api_base,
                            "--cookie-file", str(cookie), "--password-stdin",
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT, "--yes"], STUB_PASSWORD + "\n")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("--cookie-file 与 --password-stdin 不能同时使用", result.stderr)
        self._assert_no_side_effects()

    def test_missing_account_without_cookie_file_is_rejected(self):
        """既没有 --cookie-file 也没有 --account 时以 2 退出。"""
        result = self._run(["status", "--api-base-url", self.stub.api_base,
                            "--application-code", APPLICATION_CODE,
                            "--environment", ENVIRONMENT], "")
        self.assertEqual(2, result.returncode, result.stdout + result.stderr)
        self.assertIn("必须提供 --account", result.stderr)
        self._assert_no_side_effects()

    def test_status_help_prints_usage_and_exits_zero(self):
        """status -h 打印子命令用法并以 0 退出，不发起认证。"""
        result = self._run(["status", "--help"])
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn("用法：subsystem.sh status", result.stdout)
        self.assertEqual([], self.stub.hits)

    # ============================ 分类说明：未覆盖 ============================
    @unittest.skip("onboard 强依赖部署 Agent 的多阶段回调与凭证下发，"
                   "CLI 侧没有可注入的终态，无法在本套件内稳定断言；"
                   "connect/onboard 的入参校验已由 test_subsystem_connect_origin.py 覆盖")
    def test_onboard_requires_real_provisioner(self):
        """预留位：说明 onboard 子命令为何不在本套件覆盖范围内。"""
        self.fail("不应执行")


if __name__ == "__main__":
    unittest.main()
