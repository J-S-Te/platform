"""验证公开传输双向更新以及 Compose 指定的子系统运行文件。"""

import runpy
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "bin" / "reconcile-public-transport-runtime.py"
RUNTIME = runpy.run_path(str(SCRIPT))
HTTP_PLATFORM = "http://192.0.2.10:8081"
HTTP_SSO = "http://192.0.2.10:18090"
HTTPS_PLATFORM = "https://192.0.2.10:18081"
HTTPS_SSO = "https://192.0.2.10:18445"


class PublicTransportRuntimeTests(unittest.TestCase):
    """验证部署参数的行为及状态收敛。"""

    def test_runtime_can_return_to_http(self):
        """用途：验证从 HTTPS 回到 HTTP 时同步入口与会话设置。
        参数：无外部配置，使用内存运行环境示例。
        返回值：无，变更缺失时断言失败。
        """
        source = (
            f'OIDC_ISSUER="{HTTPS_SSO}/realms/basic-platform"\n'
            f"OIDC_REDIRECT_URI={HTTPS_PLATFORM}/project_management/auth/callback\n"
            f"PORTAL_ACCOUNT_SECURITY_CENTER_URL={HTTPS_PLATFORM}/settings/security\n"
            "OIDC_SESSION_COOKIE_SECURE=true\n"
            "PORTAL_ALLOW_INSECURE_HTTP_SESSION=false\n"
            "OIDC_BACKCHANNEL_BASE_URL=http://keycloak:8080\n"
        )
        updated, keys = RUNTIME["update_file"](
            source, HTTPS_PLATFORM, HTTPS_SSO, HTTP_PLATFORM, HTTP_SSO,
        )
        self.assertIn(f'OIDC_ISSUER="{HTTP_SSO}/realms/basic-platform"', updated)
        self.assertIn(f"PORTAL_ACCOUNT_SECURITY_CENTER_URL={HTTP_PLATFORM}/settings/security", updated)
        self.assertIn("OIDC_SESSION_COOKIE_SECURE=false", updated)
        self.assertIn("PORTAL_ALLOW_INSECURE_HTTP_SESSION=true", updated)
        self.assertIn("OIDC_BACKCHANNEL_BASE_URL=http://keycloak:8080", updated)
        self.assertEqual(len(keys), 5)

    def test_external_app_retains_secure_cookie(self):
        """用途：验证使用独立来源的应用保留既有安全 Cookie。
        参数：无外部配置，使用独立应用环境样例。
        返回值：无，安全 Cookie 被覆盖时断言失败。
        """
        source = (
            "APP_PUBLIC_ORIGIN=https://independent.example.test\n"
            "OIDC_SESSION_COOKIE_SECURE=true\n"
            "OIDC_ALLOW_INSECURE_HTTP_SESSION=false\n"
        )
        result = RUNTIME["update_file"](
            source, HTTPS_PLATFORM, HTTPS_SSO, HTTP_PLATFORM, HTTP_SSO,
        )
        self.assertEqual(result, (source, []))

    def test_compose_runtime_file_override_is_used_once(self):
        """用途：验证自定义运行路径生效并合并重复绑定。
        参数：无外部配置，使用 Compose 解析后的环境变量示例。
        返回值：无，路径不一致时断言失败。
        """
        bindings = RUNTIME["runtime_paths"](Path("/deploy"), {
            "CONTRACT_RUNTIME_ENV_FILE": "override/shared.env",
            "PROJECT_RUNTIME_ENV_FILE": "/deploy/override/shared.env",
        })
        self.assertEqual(bindings.count(Path("/deploy/override/shared.env")), 1)
        self.assertNotIn(Path("/deploy/runtime/contract.env"), bindings)
        self.assertNotIn(Path("/deploy/runtime/project.env"), bindings)
        self.assertIn(Path("/deploy/runtime/reimbursement.env"), bindings)


if __name__ == "__main__":
    unittest.main()
