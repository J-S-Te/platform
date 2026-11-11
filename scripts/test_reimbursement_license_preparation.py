#!/usr/bin/env python3
"""仅在临时目录验证报销许可候选，不连接 Docker 或业务库。"""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location(
    "local_license", Path(__file__).with_name("prepare-local-runtime-license.py")
)
TOOL = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TOOL)
WORKSPACE = Path(__file__).resolve().parents[2]
TARGET = {
    "directory": "reimbursement",
    "services": {
        "reimbursement-api": "reimbursement/backend:local",
        "reimbursement-worker": "reimbursement/backend:local",
    },
    "migration": {
        "service": "reimbursement-migrate", "image": "reimbursement/backend:local"
    },
}


class ReimbursementPreparationTests(unittest.TestCase):
    # 用途：确认真实开发清单登记报销两个独立消费者及迁移入口。
    # 参数：self 为测试实例。
    # 返回值：无；清单缺失或错误时断言失败。
    def test_workspace_manifest_declares_reimbursement(self):
        apps = TOOL.load_manifest(WORKSPACE)
        self.assertEqual(apps["reimbursement"], TARGET)

    # 用途：验证七应用候选保持迁移与消费者分离、未经审核状态。
    # 参数：self 为测试实例。
    # 返回值：无；候选遗漏组件或隐式批准时断言失败。
    def test_seventh_application_candidate_is_complete_and_unapproved(self):
        manifest = json.loads(
            (WORKSPACE / "platform/config/local-license-components.json").read_text()
        )
        manifest["applications"]["reimbursement"] = TARGET
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            (root / "platform/config").mkdir(parents=True)
            (root / "platform/config/local-license-components.json").write_text(
                json.dumps(manifest)
            )
            (root / "platform/compose.local.yaml").write_text("services: {}\n")
            app = root / "reimbursement"
            app.mkdir()
            (app / "go.mod").write_text("module synthetic\n")
            (app / "Dockerfile").write_text("FROM scratch\n")
            metadata = {
                "id": "sha256:" + "a" * 64, "version": "test-only",
                "protocol": "1", "architecture": "arm64", "os": "linux",
            }
            with mock.patch.object(TOOL, "inspect_image", return_value=metadata):
                document = TOOL.build_document(root, "reimbursement", "test-only")
            self.assertEqual(len(document["components"]), 2)
            self.assertEqual(
                {item["service_id"] for item in document["components"]},
                set(TARGET["services"]),
            )
            self.assertEqual(document["migration"]["service"], "reimbursement-migrate")
            self.assertFalse(document["reviewed"])
            TOOL.validate_document(document)


if __name__ == "__main__":
    unittest.main()
