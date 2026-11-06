"""验证独立门户部署投影不会启动CRM邀请消费者。"""
import importlib.util
from pathlib import Path
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("runtime_policy", HERE / "subsystem-runtime.py")
POLICY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POLICY)


class PortalModeTests(unittest.TestCase):
    # 用途：核验本地门户省略CRM消费者，同时保留报告安全摄取。
    # 参数：无。
    # 返回值：无；默认模式变更或本地跨域消费者残留时失败。
    def test_local_mode_projects_only_own_business_consumers(self):
        text = (HERE.parent / "subsystems.d/customer_portal-prod.yaml").read_text()
        compose, conditional, files = POLICY.projection(text, "customer_portal")
        for mode in ("", "crm", "local"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                (root / "runtime").mkdir()
                path = root / "runtime/portal.env"
                path.write_text("PORTAL_CUSTOMER_DIRECTORY_MODE=" + mode + "\n")
                path.chmod(0o600)
                (root / "runtime/customer.env").write_text("\n".join(
                    key + "=synthetic-ready" for key in files["runtime/customer.env"]["required"]))
                enabled = set(compose["runtime_services"]) | {"customer-api"}
                actual = POLICY.selected_services(root, compose, conditional, files, enabled)
                self.assertEqual("portal-invite-compensation-worker" in actual, mode != "local")
                self.assertIn("portal-report-worker", actual)


    # 用途：拒绝未知来源及公开读取的模式文件，避免投影与Agent产生分歧。
    # 参数：无。
    # 返回值：无；不安全配置未被拒绝时失败。
    def test_invalid_or_public_mode_fails_closed(self):
        text = (HERE.parent / "subsystems.d/customer_portal-prod.yaml").read_text()
        compose, conditional, files = POLICY.projection(text, "customer_portal")
        for mode, permission in (("automatic", 0o600), ("local", 0o644)):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                (root / "runtime").mkdir()
                path = root / "runtime/portal.env"
                path.write_text("PORTAL_CUSTOMER_DIRECTORY_MODE=" + mode + "\n")
                path.chmod(permission)
                with self.assertRaises(ValueError):
                    POLICY.selected_services(root, compose, conditional, files, set(compose["runtime_services"]))


if __name__ == "__main__":
    unittest.main()
