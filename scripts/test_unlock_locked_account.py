"""解锁锁定账号脚本（scripts/unlock-locked-account.sh）的单元测试。

用途：在完全离线、无平台、无数据库、无 docker 的条件下，用 127.0.0.1 回环桩服务验证
      unlock-locked-account.sh 的登录、锁定账号查询、解锁、复核四条 HTTP 交互及其
      错误分支、退出码、凭据安全与临时文件清理行为。
参数：无。直接 `python3 scripts/test_unlock_locked_account.py` 运行。
返回值：unittest 退出码（0 表示全部通过，1 表示存在失败）。
"""
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import urlsplit
import json
import os
import shutil
import stat
import subprocess
import tempfile
import threading
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "unlock-locked-account.sh"

LOCKED_ROW = {
    "account_id": "acc-01HXTEST",
    "account_name": "admins",
    "locked_until": "2026-01-01T00:00:00Z",
    "failure_count": 5,
}


def extract_function(name):
    """从脚本源码中按函数名截取定义文本，避免在测试里复制一份实现。

    参数：name —— shell 函数名，如 json_escape / json_total。
    返回值：从 `name() {` 到对应行首 `}` 的源码文本。
    """
    text = SCRIPT.read_text(encoding="utf-8")
    start = text.index("%s() {" % name)
    end = text.index("\n}\n", start) + 2
    return text[start:end]


def _envelope(items, total=None, request_id="req-unlock"):
    """构造平台统一响应信封 {"code","message","data","request_id"}。

    参数：items —— 列表数据；total —— 总数字段，默认取 items 长度；request_id —— 追踪号。
    返回值：dict，可直接被脚本的 locked_rows/collect_locked 解析。
    """
    return {
        "code": "OK",
        "message": "ok",
        "request_id": request_id,
        "data": {"items": list(items), "total": len(items) if total is None else total},
    }


class _StubPlatform:
    """回环桩：记录每个请求的方法/路径/头/请求体，并按预设返回状态码与响应体。"""

    def __init__(self):
        self.hits = []
        self.login_status = 200
        self.login_body = {"code": "OK", "message": "登录成功", "request_id": "req-login"}
        self.login_cookie = "stub-session-token"
        self.me_status = 200
        self.locked_sequence = [_envelope([])]
        self.locked_calls = 0
        self.unlock_status = 200
        self.unlock_body = {"code": "OK", "message": "已解除锁定", "request_id": "req-unlock-ok"}
        self.on_request = None
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def _dispatch(self, method):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                hit = {
                    "method": method,
                    "path": self.path,
                    "route": urlsplit(self.path).path,
                    "headers": dict(self.headers),
                    "body": raw.decode("utf-8", "replace"),
                }
                outer.hits.append(hit)
                if outer.on_request is not None:
                    outer.on_request(hit)
                status, payload, extra_headers = outer.respond(hit)
                data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
                self.send_response(status)
                for name, value in extra_headers:
                    self.send_header(name, value)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):  # noqa: N802 - 按 HTTP 方法命名
                self._dispatch("GET")

            def do_POST(self):  # noqa: N802 - 按 HTTP 方法命名
                self._dispatch("POST")

            def log_message(self, *args):  # 静音桩日志
                return

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self._server.server_address[1]
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
        )
        self._thread.start()

    def respond(self, hit):
        """返回 (状态码, 响应体, 附加头列表)；未匹配的路径回 404。"""
        route = hit["route"]
        if route == "/api/v1/auth/login" and hit["method"] == "POST":
            headers = []
            if self.login_status == 200 and self.login_cookie:
                headers.append(("Set-Cookie", "{}=%s; Path=/; HttpOnly".format("bp_session") % self.login_cookie))
            return self.login_status, self.login_body, headers
        if route == "/api/v1/auth/me" and hit["method"] == "GET":
            return self.me_status, {"code": "OK", "message": "ok", "request_id": "req-me"}, []
        if route == "/api/v1/security/locked-accounts" and hit["method"] == "GET":
            index = min(self.locked_calls, len(self.locked_sequence) - 1)
            self.locked_calls += 1
            return 200, self.locked_sequence[index], []
        if route.startswith("/api/v1/security/locked-accounts/") and route.endswith("/unlock"):
            return self.unlock_status, self.unlock_body, []
        return 404, {"code": "NOT_FOUND", "message": "stub 未定义该路由", "request_id": "req-404"}, []

    @property
    def origin(self):
        return "http://127.0.0.1:%d" % self.port

    def close(self):
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)


class UnlockLockedAccountTest(unittest.TestCase):
    """主流程、错误分支、凭据安全与临时文件的端到端（桩服务）断言。"""

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="unlock-test-")
        self.addCleanup(self._cleanup)
        self.stub = _StubPlatform()
        self.sandbox_snapshots = []

    def _cleanup(self):
        self.stub.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ---- 运行辅助 ----
    def _run(self, args, stdin_text="", caller_umask=None):
        """以受控 TMPDIR 调用脚本；返回 CompletedProcess，并记录 TMPDIR 是否已清空。

        参数：caller_umask —— 可选，调用方 umask（如 "000"），用于验证脚本不依赖调用方 umask。
        """
        env = os.environ.copy()
        env["TMPDIR"] = self.tmpdir
        env.pop("BASIC_PLATFORM_ORIGIN", None)
        command = ["bash", str(SCRIPT), "--base-url", self.stub.origin, *args]
        if caller_umask is not None:
            command = ["bash", "-c", "umask %s; exec \"$@\"" % caller_umask, "_", *command]
        result = subprocess.run(
            command, input=stdin_text, capture_output=True, text=True, env=env, timeout=60
        )
        self.leftovers = sorted(p.name for p in Path(self.tmpdir).iterdir())
        return result

    def _password_run(self, args=("--admin", "localadmin", "--target", "admins"),
                      stdin_text="Admin#123\n", caller_umask=None):
        return self._run([*args, "--password-stdin", "--yes"], stdin_text, caller_umask=caller_umask)

    def _hits(self, method=None, route_suffix=None):
        return [
            hit for hit in self.stub.hits
            if (method is None or hit["method"] == method)
            and (route_suffix is None or hit["route"].endswith(route_suffix))
        ]

    @staticmethod
    def _json(hit):
        return json.loads(hit["body"])

    # ---- 1. 登录 → 查询 → 解锁 → 复核 主链路 ----
    def test_login_then_unlock_uses_expected_requests(self):
        """主链路：登录成功后查询锁定账号、按 account_id 解锁、复核已不在列表，全程退出码 0。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        result = self._password_run()

        self.assertEqual(0, result.returncode, result.stderr)
        routes = [(hit["method"], hit["route"]) for hit in self.stub.hits]
        self.assertEqual(
            [
                ("POST", "/api/v1/auth/login"),
                ("GET", "/api/v1/security/locked-accounts"),
                ("POST", "/api/v1/security/locked-accounts/acc-01HXTEST/unlock"),
                ("GET", "/api/v1/security/locked-accounts"),
            ],
            routes,
        )
        self.assertIn("page=1", self._hits("GET", "/locked-accounts")[0]["path"])
        self.assertIn("page_size=100", self._hits("GET", "/locked-accounts")[0]["path"])
        self.assertIn("解锁成功", result.stderr)
        self.assertIn("复核通过", result.stderr)

    def test_login_request_body_and_headers(self):
        """登录请求体是 {account,password,login_type}，并带 Accept/Origin 头、不带 Cookie。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        result = self._password_run()
        self.assertEqual(0, result.returncode, result.stderr)

        login = self._hits("POST", "/auth/login")[0]
        self.assertEqual(
            {"account": "localadmin", "password": "Admin#123", "login_type": "password"},
            self._json(login),
        )
        self.assertEqual("application/json", login["headers"].get("Content-Type"))
        self.assertEqual("application/json", login["headers"].get("Accept"))
        self.assertEqual(self.stub.origin, login["headers"].get("Origin"))
        self.assertIsNone(login["headers"].get("Cookie"))

    def test_unlock_reuses_session_cookie_and_sends_no_body(self):
        """登录拿到的会话 Cookie 由 Cookie jar 复用到后续请求；解锁 POST 不带请求体。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        result = self._password_run()
        self.assertEqual(0, result.returncode, result.stderr)

        queries = self._hits("GET", "/locked-accounts")
        unlock = self._hits("POST", "/unlock")
        self.assertEqual(1, len(unlock))
        for hit in (*queries, *unlock):
            self.assertIn("stub-session-token", hit["headers"].get("Cookie") or "")
            self.assertEqual(self.stub.origin, hit["headers"].get("Origin"))
        self.assertEqual("", unlock[0]["body"])
        # 密码只在登录那一次出现，后续请求不重复提交凭据。
        self.assertEqual(1, sum(1 for hit in self.stub.hits if "Admin#123" in hit["body"]))

    def test_account_id_argument_skips_name_lookup(self):
        """--account-id 直接指定目标时仍会先查锁定列表，但解锁路径使用显式 id。"""
        self.stub.locked_sequence = [_envelope([])]
        result = self._run(
            ["--admin", "localadmin", "--account-id", "acc-explicit", "--password-stdin", "--yes"],
            "p@ss\n",
        )
        # 显式 id 不校验是否在锁定列表，直接解锁（404/409 兜底在解锁响应里处理）。
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(
            ["/api/v1/security/locked-accounts/acc-explicit/unlock"],
            [hit["route"] for hit in self._hits("POST", "/unlock")],
        )

    def test_explicit_account_id_absent_from_list_reports_that_fact(self):
        """--account-id 不在锁定列表时：文案说明未在列表、仍按该 ID 尝试，不冒充默认 --target。"""
        self.stub.locked_sequence = [_envelope([])]
        result = self._run(
            ["--admin", "localadmin", "--account-id", "acc-explicit", "--password-stdin", "--yes"],
            "p@ss\n",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        target_lines = [line for line in result.stderr.splitlines()
                        if line.startswith("[unlock] 目标账号：")]
        self.assertEqual(1, len(target_lines), result.stderr)
        self.assertIn("acc-explicit", target_lines[0])
        self.assertIn("未在锁定列表中", target_lines[0])
        self.assertIn("仍按指定 ID 尝试", target_lines[0])
        # 不能把默认 target（admins）当成真实目标打印。
        self.assertNotIn("admins", target_lines[0])
        self.assertEqual(1, len(self._hits("POST", "/unlock")))

    def test_explicit_account_id_in_list_prints_account_name(self):
        """--account-id 命中锁定列表时打印真实 account_name 与 account_id。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        result = self._run(
            ["--admin", "localadmin", "--account-id", "acc-01HXTEST", "--password-stdin", "--yes"],
            "p@ss\n",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        target_lines = [line for line in result.stderr.splitlines()
                        if line.startswith("[unlock] 目标账号：")]
        self.assertEqual(1, len(target_lines), result.stderr)
        self.assertIn("admins", target_lines[0])
        self.assertIn("acc-01HXTEST", target_lines[0])
        self.assertIn("在锁定列表中", target_lines[0])
        self.assertNotIn("未在锁定列表中", target_lines[0])

    def test_target_and_account_id_both_reported(self):
        """同时给 --target 与 --account-id 时，目标文案打印两者，避免歧义。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        result = self._run(
            ["--admin", "localadmin", "--target", "ops",
             "--account-id", "acc-01HXTEST", "--password-stdin", "--yes"],
            "p@ss\n",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        target_lines = [line for line in result.stderr.splitlines()
                        if line.startswith("[unlock] 目标账号：")]
        self.assertEqual(1, len(target_lines), result.stderr)
        line = target_lines[0]
        self.assertIn("--target ops", line)
        self.assertIn("--account-id", line)
        self.assertIn("acc-01HXTEST", line)
        self.assertIn("admins", line)

    def test_unknown_option_is_usage_error_with_exit_code_two(self):
        """未知参数属于用法错误：退出码 2，且不发任何请求。"""
        result = self._run(["--admin", "localadmin", "--target", "admins", "--bogus"])
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("用法", result.stdout)
        self.assertEqual([], self.stub.hits)

    def test_usage_documents_exit_codes(self):
        """--help 输出包含 0/1/2/3 四个退出码说明（含 3=目标未锁定/未找到）。"""
        result = subprocess.run(
            ["bash", str(SCRIPT), "--help"], capture_output=True, text=True, timeout=60
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("退出码", result.stdout)
        for expected in ("0  成功", "1  错误", "2  用法错误", "3  目标未锁定或未找到"):
            self.assertIn(expected, result.stdout)

    def test_login_body_stays_valid_json_for_control_characters(self):
        """账号/密码含 TAB、换行、控制字符时登录体仍是合法 JSON，并能原样还原。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        cases = [
            ("local\tadmin", "p@ss\tword"),
            ("local\nadmin", "only-ascii"),
            ('a"b\\c', "p\x01ss"),
        ]
        for account, password in cases:
            with self.subTest(account=account, password=password):
                self.stub.hits.clear()
                self.stub.locked_calls = 0
                self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
                # 密码经 --password-stdin 只读一行，所以换行只出现在 --admin 一侧。
                result = self._run(
                    ["--admin", account, "--target", "admins", "--password-stdin", "--yes"],
                    password + "\n",
                )
                self.assertEqual(0, result.returncode, result.stderr)
                login = self._hits("POST", "/auth/login")[0]
                # 请求体是原始字节解码结果；含裸 TAB/换行时这里会抛 JSONDecodeError。
                self.assertEqual(
                    {"account": account, "password": password, "login_type": "password"},
                    self._json(login),
                )

    def test_multiple_total_fields_use_data_total_for_pagination(self):
        """响应里出现多个 "total" 时按 .data.total 判断分页（wrapped 总数 250 → 继续翻第 2 页）。"""
        page_one = (
            b'{"code":"OK","message":"ok","request_id":"req-p1",'
            b'"data":{"items":[{"account_id":"acc-01HXTEST","account_name":"admins",'
            b'"locked_until":"2026-01-01T00:00:00Z","failure_count":5}],"total":250},'
            b'"page":{"total":1,"page_size":100}}'
        )
        self.stub.locked_sequence = [page_one, _envelope([], total=0)]
        result = self._run(
            ["--admin", "localadmin", "--list", "--password-stdin", "--yes"], "pw\n"
        )
        self.assertEqual(0, result.returncode, result.stderr)
        paths = [hit["path"] for hit in self._hits("GET", "/locked-accounts")]
        self.assertEqual(2, len(paths), paths)
        self.assertIn("page=1", paths[0])
        self.assertIn("page=2", paths[1])
        self.assertIn("acc-01HXTEST\tadmins", result.stdout)

    # ---- 2. 登录失败：非零退出、可诊断、不发解锁请求 ----
    def test_login_failure_stops_before_any_unlock(self):
        """登录 401/403/422/429/5xx 均非零退出、错误文案可诊断，且不查询列表、不解锁。"""
        cases = [
            (401, "登录失败"),
            (403, "Origin"),
            (422, "凭据加密"),
            (429, "限流"),
            (500, "HTTP 500"),
        ]
        for status, expected in cases:
            with self.subTest(status=status):
                self.stub.hits.clear()
                self.stub.locked_calls = 0
                self.stub.login_status = status
                result = self._password_run()
                self.assertNotEqual(0, result.returncode, result.stderr)
                self.assertIn(expected, result.stderr)
                self.assertEqual([], self._hits("POST", "/unlock"))
                self.assertEqual([], self._hits("GET", "/locked-accounts"))
                self.assertNotIn("解锁成功", result.stderr)

    def test_login_failure_body_is_truncated_into_diagnostic(self):
        """非枚举状态码应把响应体片段（≤200 字节）带进 stderr，便于定位。"""
        self.stub.login_status = 500
        self.stub.login_body = {"code": "PLATFORM_INTERNAL_ERROR", "message": "database is down"}
        result = self._password_run()
        self.assertNotEqual(0, result.returncode, result.stderr)
        self.assertIn("database is down", result.stderr)

    # ---- 3. 账号未锁定 / 已解锁 / 目标不存在 ----
    def test_target_missing_from_locked_list_exits_three_without_unlocking(self):
        """目标账号不在锁定列表（未锁定/已解锁/名字不一致）时退出码 3，不解锁。"""
        self.stub.locked_sequence = [_envelope([])]
        result = self._password_run()
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("锁定列表里没有 admins", result.stderr)
        self.assertEqual([], self._hits("POST", "/unlock"))

    def test_list_only_prints_table_and_never_unlocks(self):
        """--list 只打印锁定清单（表头 + 行）到 stdout，不发起解锁。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW])]
        result = self._run(["--admin", "localadmin", "--list", "--password-stdin", "--yes"], "pw\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("account_id\taccount_name\tlocked_until\tfailure_count", result.stdout)
        self.assertIn("acc-01HXTEST\tadmins", result.stdout)
        self.assertEqual([], self._hits("POST", "/unlock"))

    # ---- 4. 解锁接口非 2xx ----
    def test_unlock_rejected_exits_nonzero_without_success_message(self):
        """解锁返回 403/404/409/500 时非零退出，且不打印“解锁成功”/“复核通过”。"""
        cases = [(403, "platform:locked-account:unlock"), (404, "账号不存在"), (409, "没有生效中的锁定")]
        for status, expected in cases:
            with self.subTest(status=status):
                self.stub.hits.clear()
                self.stub.locked_calls = 0
                self.stub.locked_sequence = [_envelope([LOCKED_ROW])]
                self.stub.unlock_status = status
                result = self._password_run()
                self.assertNotEqual(0, result.returncode, result.stderr)
                self.assertIn(expected, result.stderr)
                self.assertNotIn("解锁成功", result.stderr)
                self.assertNotIn("复核通过", result.stderr)
                # 只查询列表一次（确认目标），解锁失败后不再复核。
                self.assertEqual(1, len(self._hits("GET", "/locked-accounts")))

    def test_unlock_server_error_surfaces_status_and_body(self):
        """解锁接口 500 时把状态码与响应体片段写进 stderr。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW])]
        self.stub.unlock_status = 500
        self.stub.unlock_body = {"code": "PLATFORM_INTERNAL_ERROR", "message": "unlock backend failed"}
        result = self._password_run()
        self.assertNotEqual(0, result.returncode, result.stderr)
        self.assertIn("HTTP 500", result.stderr)
        self.assertIn("unlock backend failed", result.stderr)

    def test_recheck_failure_reports_unlocked_still_locked(self):
        """解锁返回 200 但复核仍能查到该账号时，脚本以非零退出并说明复核未通过。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW])]
        result = self._password_run()
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("复核未通过", result.stderr)
        self.assertIn("acc-01HXTEST", result.stderr)

    # ---- 5. 会话 Cookie 模式 ----
    def test_cookie_stdin_authenticates_without_password(self):
        """--cookie-stdin 用会话 Cookie 认证：GET /auth/me 200 后直接查询并解锁，不发登录请求。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        result = self._run(["--cookie-stdin", "--target", "admins", "--yes"], "bp_session=abc123\n")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual([], self._hits("POST", "/auth/login"))
        me = self._hits("GET", "/api/v1/auth/me")
        self.assertEqual(1, len(me))
        self.assertIn("bp_session=abc123", me[0]["headers"].get("Cookie") or "")
        unlock = self._hits("POST", "/unlock")
        self.assertEqual(1, len(unlock))
        self.assertIn("bp_session=abc123", unlock[0]["headers"].get("Cookie") or "")
        self.assertIn("使用浏览器会话 Cookie 认证", result.stderr)

    def test_invalid_cookie_exits_before_querying(self):
        """Cookie 无效（/auth/me 401）时非零退出，提示重新登录，且不查询锁定列表。"""
        self.stub.me_status = 401
        result = self._run(["--cookie-stdin", "--target", "admins", "--yes"], "bp_session=expired\n")
        self.assertNotEqual(0, result.returncode, result.stderr)
        self.assertIn("会话 Cookie 无效或已过期", result.stderr)
        self.assertEqual([], self._hits("GET", "/locked-accounts"))
        self.assertEqual([], self._hits("POST", "/unlock"))

    # ---- 6. 凭据安全与临时文件 ----
    def test_password_never_appears_in_output_and_tempdir_is_cleaned(self):
        """密码不出现在 stdout/stderr；退出后 0700 临时目录被整体删除。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        snapshot = {}

        def capture(hit):
            """在解锁请求到达时快照临时目录内容与权限。"""
            if hit["route"].endswith("/unlock"):
                entries = {}
                for path in Path(self.tmpdir).iterdir():
                    if path.is_dir():
                        entries[path.name] = {
                            "mode": stat.S_IMODE(path.stat().st_mode),
                            "files": {child.name: stat.S_IMODE(child.stat().st_mode)
                                      for child in path.iterdir()},
                        }
                snapshot.update(entries)

        self.stub.on_request = capture
        result = self._password_run(("--admin", "localadmin", "--target", "admins"))
        self.assertEqual(0, result.returncode, result.stderr)

        secret = "Admin#123"
        self.assertNotIn(secret, result.stdout)
        self.assertNotIn(secret, result.stderr)
        # 运行期只创建一个 unlock.* 临时目录，权限 0700。
        self.assertEqual(1, len(snapshot))
        workdir_name = next(iter(snapshot))
        self.assertTrue(workdir_name.startswith("unlock."), workdir_name)
        self.assertEqual(0o700, snapshot[workdir_name]["mode"])
        # 密码模式不落会话文件；登录请求体文件在登录完成后即删除。
        self.assertNotIn("session-cookie.txt", snapshot[workdir_name]["files"])
        self.assertNotIn("login.json", snapshot[workdir_name]["files"])
        # 退出后临时目录被清理干净。
        self.assertEqual([], self.leftovers)

    def test_cookie_source_file_is_0600_and_removed_on_exit(self):
        """Cookie 模式把会话值写入 0600 临时文件（不进 argv），退出后随目录删除。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        snapshot = {}

        def capture(hit):
            if hit["route"].endswith("/unlock"):
                for path in Path(self.tmpdir).iterdir():
                    if path.is_dir():
                        snapshot["files"] = {child.name: stat.S_IMODE(child.stat().st_mode)
                                             for child in path.iterdir()}
                        snapshot["cookie_body"] = (path / "session-cookie.txt").read_text()
                self.assertNotIn("super-secret-cookie-value", hit["path"])

        self.stub.on_request = capture
        result = self._run(
            ["--cookie-stdin", "--target", "admins", "--yes"], "bp_session=super-secret-cookie-value\n"
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(0o600, snapshot["files"].get("session-cookie.txt"))
        self.assertIn("bp_session\tsuper-secret-cookie-value", snapshot["cookie_body"])
        self.assertNotIn("super-secret-cookie-value", result.stdout)
        self.assertNotIn("super-secret-cookie-value", result.stderr)
        self.assertEqual([], self.leftovers)

    def test_temp_files_are_0600_regardless_of_caller_umask(self):
        """调用方 umask=000 时，登录体/响应体/Cookie jar 仍全部是 0600。"""
        self.stub.locked_sequence = [_envelope([LOCKED_ROW]), _envelope([])]
        seen = {}

        def capture(hit):
            """在登录与解锁请求到达时快照临时目录内文件权限。"""
            if hit["route"] not in ("/api/v1/auth/login",) and not hit["route"].endswith("/unlock"):
                return
            stage = "login" if hit["route"] == "/api/v1/auth/login" else "unlock"
            for path in Path(self.tmpdir).iterdir():
                if path.is_dir():
                    seen[stage] = {child.name: stat.S_IMODE(child.stat().st_mode)
                                   for child in path.iterdir()}

        self.stub.on_request = capture
        result = self._password_run(caller_umask="000")
        self.assertEqual(0, result.returncode, result.stderr)

        self.assertEqual(0o600, seen["login"].get("login.json"), seen["login"])
        self.assertEqual(0o600, seen["unlock"].get("body"), seen["unlock"])
        self.assertEqual(0o600, seen["unlock"].get("cookies"), seen["unlock"])

    def test_missing_cookie_source_is_rejected(self):
        """--cookie-stdin 读到空值时立即失败，不发任何请求。"""
        result = self._run(["--cookie-stdin", "--target", "admins", "--yes"], "\n")
        self.assertNotEqual(0, result.returncode, result.stderr)
        self.assertIn("未从标准输入读到会话 Cookie", result.stderr)
        self.assertEqual([], self.stub.hits)

    def test_mutually_exclusive_cookie_sources(self):
        """--cookie 与 --cookie-stdin 同时出现时拒绝执行，不发请求。"""
        result = self._run(
            ["--cookie", "bp_session=x", "--cookie-stdin", "--target", "admins", "--yes"], "y\n"
        )
        self.assertNotEqual(0, result.returncode, result.stderr)
        self.assertIn("只能指定一个", result.stderr)
        self.assertEqual([], self.stub.hits)


class JsonEscapeTest(unittest.TestCase):
    """json_escape 的转义行为（直接提取脚本中的真实函数文本，避免复制实现）。"""

    TEMPLATE = '{"account":"%s","password":"%s","login_type":"password"}'

    def _escape(self, value, env=None, bash=None):
        """在子 shell 里执行脚本中的 json_escape，返回其 stdout（未加外层引号）。"""
        script = extract_function("json_escape") + '\njson_escape "$1"\n'
        return subprocess.run(
            [bash or "bash", "-c", script, "_", value],
            capture_output=True, text=True, timeout=60, env=env,
        )

    def _round_trip(self, value, env=None, bash=None):
        """把转义结果嵌进脚本的登录体模板，用 json.loads 还原出 password。"""
        result = self._escape(value, env=env, bash=bash)
        self.assertEqual(0, result.returncode, result.stderr)
        return json.loads(self.TEMPLATE % ("localadmin", result.stdout))["password"]

    def test_quotes_and_backslashes_are_escaped(self):
        """双引号转成 \\"，反斜杠转成 \\\\，两者组合时互不干扰。"""
        self.assertEqual('pa\\"ss', self._escape('pa"ss').stdout)
        self.assertEqual("pa\\\\ss", self._escape("pa\\ss").stdout)
        self.assertEqual('a\\"b\\\\c', self._escape('a"b\\c').stdout)

    def test_newline_carriage_return_and_tab_are_escaped(self):
        """\\n / \\r / \\t 分别转成短转义，不再把裸控制字符写进 JSON。"""
        self.assertEqual("a\\nb", self._escape("a\nb").stdout)
        self.assertEqual("a\\rb", self._escape("a\rb").stdout)
        self.assertEqual("a\\tb", self._escape("a\tb").stdout)
        self.assertEqual("a\\n\\r\\tb", self._escape("a\n\r\tb").stdout)

    def test_backspace_and_formfeed_are_escaped(self):
        """\\b 与 \\f 转成短转义（旧实现完全不转义这两个字符）。"""
        self.assertEqual("a\\bb", self._escape("a\x08b").stdout)
        self.assertEqual("a\\fb", self._escape("a\x0cb").stdout)

    def test_other_control_characters_use_unicode_escape(self):
        """其余 <0x20 控制字符转成 \\u00XX，JSON 里不出现裸控制字符。"""
        self.assertEqual("a\\u0001b", self._escape("a\x01b").stdout)
        self.assertEqual("a\\u001fb", self._escape("a\x1fb").stdout)
        escaped = self._escape("\x01\x02\x03").stdout
        self.assertNotIn("\x01", escaped)
        self.assertEqual("\\u0001\\u0002\\u0003", escaped)

    def test_chinese_and_emoji_are_kept_as_literal_utf8(self):
        """中文/emoji 原样保留（ensure_ascii=False 语义），普通 ASCII 也不变。"""
        self.assertEqual("管理员密码", self._escape("管理员密码").stdout)
        self.assertEqual("\U0001f600p@ss", self._escape("\U0001f600p@ss").stdout)
        self.assertEqual("p@ssw0rd!#$%^&*()", self._escape("p@ssw0rd!#$%^&*()").stdout)

    def test_login_payload_round_trips_through_json_loads(self):
        """引号、反斜杠、\\n\\r\\t\\b\\f、\\x01、中文、emoji 都能被脚本登录体模板完整还原。"""
        values = [
            'pa"ss', "pa\\ss", 'a"b\\c', "a\nb", "a\rb", "a\tb",
            "a\x08b", "a\x0cb", "a\x01b", 'both"\n\\', "管理员密码", "\U0001f600-p@ss",
        ]
        for value in values:
            with self.subTest(value=value):
                self.assertEqual(value, self._round_trip(value))

    def test_fallback_without_python3_covers_documented_escapes(self):
        """无 python3 的降级实现确定性覆盖 \\ " \\n \\r \\t，且拼出的 JSON 合法。"""
        tmpdir = tempfile.mkdtemp(prefix="unlock-nopy-")
        self.addCleanup(shutil.rmtree, tmpdir, ignore_errors=True)
        bash = shutil.which("bash")
        self.assertIsNotNone(bash)
        os.symlink(bash, Path(tmpdir) / "bash")
        env = {**os.environ, "PATH": tmpdir}
        self.assertIsNone(shutil.which("python3", path=tmpdir))

        cases = {
            'pa"ss': 'pa\\"ss',
            "pa\\ss": "pa\\\\ss",
            "a\nb": "a\\nb",
            "a\rb": "a\\rb",
            "a\tb": "a\\tb",
            "a\n\r\tb": "a\\n\\r\\tb",
        }
        for value, expected in cases.items():
            with self.subTest(value=value):
                result = self._escape(value, env=env, bash=str(Path(tmpdir) / "bash"))
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertEqual(expected, result.stdout)
                payload = self.TEMPLATE % ("localadmin", result.stdout)
                self.assertEqual(value, json.loads(payload)["password"])

    def test_fallback_boundary_leaves_other_control_characters_unescaped(self):
        """降级边界（已在脚本注释中写明）：无 python3 时 \\b \\f \\x01 不被转义，JSON 非法。"""
        tmpdir = tempfile.mkdtemp(prefix="unlock-nopy-")
        self.addCleanup(shutil.rmtree, tmpdir, ignore_errors=True)
        bash = shutil.which("bash")
        os.symlink(bash, Path(tmpdir) / "bash")
        env = {**os.environ, "PATH": tmpdir}
        escaped = self._escape("a\x01b", env=env, bash=str(Path(tmpdir) / "bash")).stdout
        self.assertEqual("a\x01b", escaped)
        with self.assertRaises(json.JSONDecodeError):
            json.loads(self.TEMPLATE % ("localadmin", escaped))


class JsonTotalTest(unittest.TestCase):
    """json_total 从响应信封取分页总数（直接提取脚本中的真实函数文本）。"""

    def _total(self, body, env=None, bash="bash"):
        script = extract_function("json_total") + '\njson_total "$1"\n'
        result = subprocess.run(
            [bash, "-c", script, "_", body],
            capture_output=True, text=True, timeout=60, env=env,
        )
        self.assertEqual(0, result.returncode, result.stderr)
        return result.stdout.strip()

    def test_prefers_data_total_over_other_total_fields(self):
        """多个 "total" 字段并存时取 .data.total，而不是最后一个出现的数字。"""
        body = ('{"code":"OK","total":999,"data":{"items":[],"total":250},'
                '"page":{"total":1,"page_size":100}}')
        self.assertEqual("250", self._total(body))
        body_last = '{"data":{"items":[],"total":250},"page":{"total":1}}'
        self.assertEqual("250", self._total(body_last))

    def test_falls_back_to_top_level_total_and_handles_missing(self):
        """data.total 缺失时回退顶层 total；都缺失或响应不是 JSON 时输出为空。"""
        self.assertEqual("7", self._total('{"total":7,"data":{"items":[]}}'))
        self.assertEqual("", self._total('{"data":{"items":[]}}'))
        self.assertEqual("", self._total("not json"))
        self.assertEqual("", self._total('{"data":{"total":"250"}}'))

    def test_grep_fallback_without_python3_takes_first_total(self):
        """无 python3 时用 grep 取第一个 "total" 数字字段（不会取最后一个）。"""
        tmpdir = tempfile.mkdtemp(prefix="unlock-nopy-")
        self.addCleanup(shutil.rmtree, tmpdir, ignore_errors=True)
        bash = shutil.which("bash")
        os.symlink(bash, Path(tmpdir) / "bash")
        for tool in ("grep", "sed"):
            os.symlink(shutil.which(tool), Path(tmpdir) / tool)
        env = {**os.environ, "PATH": tmpdir}
        body = '{"data":{"items":[],"total":250},"page":{"total":1}}'
        self.assertEqual("250", self._total(body, env=env, bash=str(Path(tmpdir) / "bash")))
        self.assertEqual("", self._total('{"data":{"items":[]}}', env=env,
                                         bash=str(Path(tmpdir) / "bash")))


if __name__ == "__main__":
    unittest.main()
