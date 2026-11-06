"""验证 subsystem.sh connect 的可信 Origin 归一化与 403 诊断文案。

用本地回环桩服务（127.0.0.1 随机端口）捕获 CLI 实际发出的请求头，不接触真实平台、
Cookie、账号或容器；不写入运行环境。
"""
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
import json
import os
import subprocess
import tempfile
import threading
import unittest


ROOT = Path(__file__).resolve().parents[1]
SUBSYSTEM = ROOT / "scripts" / "subsystem.sh"
HTTP_LIB = ROOT / "scripts" / "lib" / "subsystem-http.sh"

FORBIDDEN_BODY = json.dumps(
    {"code": "AUTH_FORBIDDEN", "message": "没有执行此操作的权限", "request_id": "01TESTOORIGIN000000000000"}
).encode()


class _StubPlatform:
    """回环桩：记录收到的请求（路径 + 完整头），按预设状态码回包。"""

    def __init__(self, status=500, body=b'{"code":"INTERNAL","message":"probe"}'):
        self.status = status
        self.body = body
        self.hits = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self):  # noqa: N802 - 按 HTTP 方法命名
                length = int(self.headers.get("Content-Length") or 0)
                self.rfile.read(length)
                outer.hits.append({"path": self.path, "headers": dict(self.headers)})
                self.send_response(outer.status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(outer.body)))
                self.end_headers()
                self.wfile.write(outer.body)

            def log_message(self, *args):  # 静音桩日志
                return

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self._server.server_address[1]
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def origin(self):
        return f"http://127.0.0.1:{self.port}"

    def close(self):
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)


class SubsystemConnectOriginTest(unittest.TestCase):
    def setUp(self):
        self.addCleanup(self._cleanup_stub)
        self.stub = _StubPlatform()
        self._stub_closed = False

    def _cleanup_stub(self):
        if not self._stub_closed:
            self._stub_closed = True
            self.stub.close()

    def _use_stub(self, status, body=FORBIDDEN_BODY):
        self._cleanup_stub()
        self.stub = _StubPlatform(status=status, body=body)

    def _run_connect(self, extra_args=(), unset_env=True):
        env = os.environ.copy()
        if unset_env:
            env.pop("BASIC_PLATFORM_ORIGIN", None)
            env.pop("PLATFORM_ORIGIN", None)
        command = [
            "bash", str(SUBSYSTEM), "connect",
            "--application-code", "project_management",
            "--environment", "prod",
            "--api-base-url", f"{self.stub.origin}/api/v1",
            "--account", "admin",
            "--password-stdin",
            *extra_args,
        ]
        return subprocess.run(
            command, input="stub-password\n", capture_output=True, text=True, env=env, timeout=120
        )

    def _credential_challenge_hits(self):
        return [hit for hit in self.stub.hits if hit["path"].endswith("/auth/credential-challenges")]

    def test_connect_derives_origin_from_api_base_url(self):
        """未给 --platform-origin 且未设 BASIC_PLATFORM_ORIGIN 时，按 API 地址推导可信 Origin。"""
        result = self._run_connect()

        hits = self._credential_challenge_hits()
        self.assertTrue(hits, f"未发出凭据挑战请求；stderr={result.stderr}")
        self.assertEqual(self.stub.origin, hits[0]["headers"].get("Origin"))

    def test_connect_uses_explicit_platform_origin(self):
        """显式 --platform-origin 优先于推导值。"""
        result = self._run_connect(["--platform-origin", "http://localhost:8081"])

        hits = self._credential_challenge_hits()
        self.assertTrue(hits, f"未发出凭据挑战请求；stderr={result.stderr}")
        self.assertEqual("http://localhost:8081", hits[0]["headers"].get("Origin"))

    def test_connect_rejects_origin_with_path_before_request(self):
        """带路径的 origin 永远过不了同源防线，应在发出请求前失败并说明原因。"""
        result = self._run_connect(["--platform-origin", f"{self.stub.origin}/portal"])

        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("platform-origin", result.stderr)
        self.assertEqual([], self.stub.hits, "非法 origin 不应发出任何请求")

    def test_credential_challenge_403_points_to_origin(self):
        """挑战端点的 403 应指向同源/Origin 配置，而不是误导成管理权限不足。"""
        self._use_stub(403)

        result = self._run_connect()

        self.assertIn("同源", result.stderr)
        self.assertIn(self.stub.origin, result.stderr)
        self.assertNotIn("基础平台管理权限", result.stderr)


class OriginErrorDiagnosticsTest(unittest.TestCase):
    """print_api_error 的 403 分支：两个登录端点都必须给出 Origin 提示。"""

    def _print_api_error(self, endpoint):
        with tempfile.TemporaryDirectory() as tmp:
            body = Path(tmp) / "body.json"
            body.write_text(
                json.dumps({"code": "AUTH_FORBIDDEN", "message": "没有执行此操作的权限", "request_id": "rid"}),
                encoding="utf-8",
            )
            script = (
                'log() { :; }; '
                'PLATFORM_ORIGIN="http://localhost:8081"; '
                f'. "{HTTP_LIB}"; '
                f'print_api_error 403 "{body}" "{endpoint}"'
            )
            return subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=60)

    def test_login_403_points_to_origin(self):
        result = self._print_api_error("/auth/login")
        self.assertIn("同源", result.stderr)
        self.assertNotIn("基础平台管理权限", result.stderr)

    def test_credential_challenges_403_points_to_origin(self):
        result = self._print_api_error("/auth/credential-challenges")
        self.assertIn("同源", result.stderr)
        self.assertNotIn("基础平台管理权限", result.stderr)


if __name__ == "__main__":
    unittest.main()
