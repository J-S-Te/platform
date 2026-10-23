"""批量批准入口的回归测试：只读候选、失败关闭、不自动批准。

逐个用例替换 Docker 与单应用工具的边界，不触碰真实 Docker、镜像或批准文件。
"""
from pathlib import Path
import importlib.util
import json
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
MODULE = ROOT / "scripts/refresh-local-runtime-approvals.py"


def load_module():
    spec = importlib.util.spec_from_file_location("refresh_approvals", MODULE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


APPROVALS = "platform/runtime-approvals"


class RefreshApprovalsTest(unittest.TestCase):
    def setUp(self):
        self.module = load_module()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspace = Path(self.tmp.name)
        (self.workspace / APPROVALS).mkdir(parents=True)
        (self.workspace / "platform").mkdir(exist_ok=True)
        (self.workspace / "platform/compose.local.yaml").write_text("services: {}\n")
        self.compose_digest = self.module.digest_file(self.workspace / "platform/compose.local.yaml")

    def write_candidate(self, application, compose_digest=None, services=(("api", "sha256:" + "a" * 64),)):
        path = self.workspace / APPROVALS / ("local-" + application + "-dev.candidate.json")
        path.write_text(json.dumps({
            "application": application,
            "compose_sha256": compose_digest or self.compose_digest,
            "source_digest": "sha256:" + "b" * 64,
            "services": [{"name": name, "image_digest": digest} for name, digest in services],
            "migration": {"service": services[0][0] + "-migrate", "image_digest": services[0][1]},
        }))
        return path

    def write_manifest(self, applications):
        manifest = self.workspace / "platform" / "config"
        manifest.mkdir(parents=True, exist_ok=True)
        (manifest / "local-license-components.json").write_text(
            json.dumps({"applications": {name: {} for name in applications}}))

    def test_manifest_is_authoritative_so_never_approved_apps_are_covered(self):
        # 只看已有批准文件会漏掉"从未批准过"的应用（门户就是这样漏掉的），
        # 直到接入时才以 LICENSE_RUNTIME_APPROVAL_UNAVAILABLE 暴露。
        self.write_manifest(["crm", "customer_portal"])
        (self.workspace / APPROVALS / "local-crm-dev.json").write_text("{}")
        self.assertEqual(self.module.registered_applications(self.workspace),
                         ["crm", "customer_portal"])

    def test_falls_back_to_published_approvals_when_manifest_is_unreadable(self):
        (self.workspace / APPROVALS / "local-crm-dev.json").write_text("{}")
        (self.workspace / APPROVALS / "local-crm-dev.candidate.json").write_text("{}")
        (self.workspace / APPROVALS / "local-other-prod.json").write_text("{}")
        self.assertEqual(self.module.registered_applications(self.workspace), ["crm"])

    def test_candidate_with_present_images_is_publishable(self):
        self.write_candidate("crm")
        self.module.image_present = lambda digest: True
        report = self.module.inspect_candidate(self.workspace, "crm")
        self.assertTrue(report["ok"], report)
        self.assertEqual(report["problems"], [])

    def test_candidate_with_missing_image_is_blocked(self):
        self.write_candidate("crm")
        self.module.image_present = lambda digest: False
        report = self.module.inspect_candidate(self.workspace, "crm")
        self.assertFalse(report["ok"])
        self.assertTrue(any("固定镜像缺失" in item for item in report["problems"]), report)

    def test_candidate_with_stale_compose_is_blocked(self):
        self.write_candidate("crm", compose_digest="sha256:" + "c" * 64)
        self.module.image_present = lambda digest: True
        report = self.module.inspect_candidate(self.workspace, "crm")
        self.assertFalse(report["ok"])
        self.assertTrue(any("编排指纹" in item for item in report["problems"]), report)

    def test_missing_candidate_file_is_blocked(self):
        report = self.module.inspect_candidate(self.workspace, "crm")
        self.assertFalse(report["ok"])

    def run_main(self, applications, publish):
        prepared = []
        approved = []

        def fake_run_prepare(workspace, application, operator, approve, replace):
            target = approved if approve else prepared
            target.append(application)
            if not approve:
                self.write_candidate(application)
            return type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})()

        self.module.run_prepare = fake_run_prepare
        self.module.image_present = lambda digest: True
        arguments = ["--workspace", str(self.workspace), "--operator", "tester",
                     "--applications", applications]
        if publish:
            arguments.append("--approve")
        code = self.module.main(arguments)
        return code, prepared, approved

    def test_candidate_phase_never_publishes(self):
        code, prepared, approved = self.run_main("crm,settlement", publish=False)
        self.assertEqual(code, 0)
        self.assertEqual(prepared, ["crm", "settlement"])
        self.assertEqual(approved, [])

    def test_publish_requires_explicit_approve(self):
        code, prepared, approved = self.run_main("crm,settlement", publish=True)
        self.assertEqual(code, 0)
        self.assertEqual(approved, ["crm", "settlement"])

    def test_publish_is_refused_when_any_candidate_is_blocked(self):
        def fake_run_prepare(workspace, application, operator, approve, replace):
            if not approve:
                self.write_candidate(application)
            return type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})()

        self.module.run_prepare = fake_run_prepare
        self.module.image_present = lambda digest: False
        with self.assertRaises(SystemExit):
            self.module.main(["--workspace", str(self.workspace), "--operator", "tester",
                              "--applications", "crm", "--approve"])

    def test_prepare_failure_stops_the_batch(self):
        def failing_run_prepare(workspace, application, operator, approve, replace):
            return type("R", (), {"returncode": 1, "stdout": "", "stderr": "boom"})()

        self.module.run_prepare = failing_run_prepare
        with self.assertRaises(SystemExit):
            self.module.main(["--workspace", str(self.workspace), "--operator", "tester",
                              "--applications", "crm"])


if __name__ == "__main__":
    unittest.main()
