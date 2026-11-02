#!/usr/bin/env python3
"""发布前备份策略的隔离回归：首次安装未完成才允许跳过灾难备份。"""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

PRODUCTION = Path(__file__).resolve().parents[1]
POLICY = PRODUCTION / "bin/release-backup-policy.py"


class ReleaseBackupPolicyTests(unittest.TestCase):
    # 用途：构造隔离的会话目录与会话工具替身。
    # 参数：无。
    # 返回值：无；临时目录在测试结束时清理。
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uip-backup-policy-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.state = self.base / "target.installation-state"
        self.state.mkdir()
        self.tool = self.base / "deployment-session.py"

    # 用途：写入返回指定会话文档的 deployment-session.py 替身。
    # 参数：document 是会话文档；code 是退出码；stdout 可覆盖原始输出。
    # 返回值：无。
    def write_tool(self, document=None, code=0, stdout=None):
        payload = json.dumps(document) if document is not None else "{}"
        text = stdout if stdout is not None else repr(payload)
        source = "import sys\nprint(" + text + ")\nraise SystemExit(" + str(code) + ")\n"
        self.tool.write_text(source, encoding="utf-8")

    # 用途：执行策略工具并返回标准输出。
    # 参数：无。
    # 返回值：backup 或 skip。
    def decision(self):
        result = subprocess.run([sys.executable, str(POLICY), "--state-dir", str(self.state),
                                 "--session-tool", str(self.tool)],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    # 用途：验证只有“首次安装且平台从未完成”才跳过备份。
    # 参数：无。
    # 返回值：无。
    def test_only_incomplete_first_install_skips_backup(self):
        cases = [
            ({"identity": {"mode": "install"}, "overall_status": "FAILED"}, "skip"),
            ({"identity": {"mode": "install"}, "overall_status": "RUNNING",
              "completed_stages": ["host-deps", "assets"]}, "skip"),
            ({"identity": {"mode": "install"}, "overall_status": "WAITING",
              "completed_stages": ["host-deps", "platform", "frontend"]}, "backup"),
            ({"identity": {"mode": "install"}, "overall_status": "WAITING",
              "historical_completed_stages": ["platform"]}, "backup"),
            ({"identity": {"mode": "install"}, "overall_status": "COMPLETED"}, "backup"),
            ({"identity": {"mode": "upgrade"}, "overall_status": "RUNNING"}, "backup"),
            ({"identity": {}, "overall_status": "RUNNING"}, "backup"),
        ]
        for document, expected in cases:
            with self.subTest(document=document):
                self.write_tool(document)
                self.assertEqual(self.decision(), expected)

    # 用途：验证判断依据不可用或损坏时退回“必须备份”。
    # 参数：无。
    # 返回值：无。
    def test_uncertain_state_requires_backup(self):
        self.write_tool({"identity": {"mode": "install"}, "overall_status": "FAILED"}, code=3)
        self.assertEqual(self.decision(), "backup")
        self.write_tool(stdout="not-json")
        self.assertEqual(self.decision(), "backup")
        self.assertFalse(self.tool.with_name("missing-tool.py").exists())
        missing = self.base / "missing-tool.py"
        result = subprocess.run([sys.executable, str(POLICY), "--state-dir", str(self.state),
                                 "--session-tool", str(missing)],
                                capture_output=True, text=True)
        self.assertEqual((result.returncode, result.stdout.strip()), (0, "backup"))


if __name__ == "__main__":
    unittest.main()
