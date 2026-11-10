#!/usr/bin/env python3
"""隔离验证发布后镜像清理门禁，不调用 Docker 或真实发布环境。"""
import fcntl
import importlib.util
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest


PRODUCTION = Path(__file__).resolve().parents[1]
HELPER = PRODUCTION / "bin/release-image-prune.py"
SPEC = importlib.util.spec_from_file_location(
    "prune_test_session", PRODUCTION / "bin/deployment-session.py")
SESSION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SESSION)


class ReleaseImagePruneTests(unittest.TestCase):
    # 用途：准备隔离发布目录与只记录调用和锁状态的清理替身。
    # 参数：无。
    # 返回值：无；测试结束清理临时目录。
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uip-release-prune-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.media = self.root / "media"
        self.media.mkdir(mode=0o700)
        self.prepare_target("default")

    # 用途：为单个状态案例创建独立目标及无 Docker 清理替身。
    # 参数：name 为案例目录名。
    # 返回值：无。
    def prepare_target(self, name):
        parent = self.root / name
        parent.mkdir(mode=0o700)
        self.target = parent / "target"
        (self.target / "bin").mkdir(parents=True, mode=0o700)
        self.state = Path(str(self.target) + ".installation-state")
        self.lock = Path(str(self.target) + ".installation-run.lock")
        self.trace = parent / "prune-trace.json"
        fake = self.target / "bin/prune-images.sh"
        fake.write_text(
            "#!/usr/bin/env bash\n" + shlex.quote(sys.executable) + " - \"$@\" <<'PY'\n"
            "import fcntl, json, sys\n"
            "from pathlib import Path\n"
            "held = False\n"
            "with open(" + repr(str(self.lock)) + ", 'r+') as stream:\n"
            "    try:\n"
            "        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)\n"
            "    except BlockingIOError:\n"
            "        held = True\n"
            "Path(" + repr(str(self.trace)) + ").write_text(json.dumps(\n"
            "    {'args': sys.argv[1:], 'installation_lock_held': held}))\n"
            "PY\n", encoding="utf-8")

    # 用途：通过真实会话协议建立指定状态，避免伪造已完成记录。
    # 参数：status 为待测会话状态。
    # 返回值：无。
    def write_session(self, status):
        identity = {"version": 1, "target": str(self.target), "media": str(self.media),
                    "delivery_id": "sha256:" + "a" * 64, "mode": "upgrade",
                    "components": ["platform"],
                    "options": {"skip_host_deps": False, "skip_configure": False,
                                "apply_compose": False, "legacy_license_mode": "",
                                "customer": "", "license_file_path": "",
                                "license_file_sha256": ""}}
        source = self.target.parent / "identity.json"
        source.write_text(json.dumps(identity))
        source.chmod(0o600)
        session = SESSION.begin(str(self.state), str(source), "verify", False, False)
        if status == "ABANDONED":
            SESSION.abandon(str(self.state), session, "PRODUCT_DEFECT")
        elif status != "PENDING":
            SESSION.update(str(self.state), session, "verify", "RUNNING", "")
            if status != "RUNNING":
                SESSION.update(str(self.state), session, "verify",
                               "DONE" if status == "COMPLETED" else status,
                               "" if status == "COMPLETED" else "RUNTIME_PROOF_FAILED")

    # 用途：执行发布清理门禁并核验维护动作不影响发布成功。
    # 参数：extra 为可选命令行参数。
    # 返回值：清理门禁命令的输出结果。
    def run_helper(self, *extra):
        result = subprocess.run(
            [sys.executable, str(HELPER), "--deploy-dir", str(self.target), *extra],
            capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result

    # 用途：验证无会话或真实完成后可清理，且清理全程阻止新安装取锁。
    # 参数：无。
    # 返回值：无；清理参数、锁或只读约束不符时失败。
    def test_only_completed_or_absent_session_runs_prune_under_install_lock(self):
        for status in ("ABSENT", "COMPLETED"):
            with self.subTest(status=status):
                self.prepare_target(status)
                if status == "COMPLETED":
                    self.write_session(status)
                before = (self.state / "session.json").read_bytes() if self.state.exists() else None
                self.run_helper()
                self.assertEqual(json.loads(self.trace.read_text()), {
                    "args": ["--keep-hours", "72", "--apply"],
                    "installation_lock_held": True})
                if before is None:
                    self.assertFalse(self.state.exists())
                else:
                    self.assertEqual((self.state / "session.json").read_bytes(), before)

    # 用途：验证未完成或已放弃会话明确延后，不删除回退所需镜像。
    # 参数：无。
    # 返回值：无；任一状态触发清理时失败。
    def test_incomplete_and_abandoned_sessions_defer_prune(self):
        for status in ("PENDING", "RUNNING", "WAITING", "FAILED", "ABANDONED"):
            with self.subTest(status=status):
                self.prepare_target(status)
                self.write_session(status)
                before = (self.state / "session.json").read_bytes()
                result = self.run_helper()
                self.assertIn(status, result.stderr)
                self.assertIn("延后", result.stderr)
                self.assertFalse(self.trace.exists())
                self.assertEqual((self.state / "session.json").read_bytes(), before)

    # 用途：验证锁忙、损坏或不安全状态均关闭自动清理。
    # 参数：无。
    # 返回值：无；清理被执行或缺少明确提示时失败。
    def test_busy_corrupt_and_unsafe_state_defer_prune(self):
        for condition in ("busy", "corrupt", "permissions", "symlink", "empty"):
            with self.subTest(condition=condition):
                self.prepare_target(condition)
                if condition == "busy":
                    with self.lock.open("w") as stream:
                        self.lock.chmod(0o600)
                        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        result = self.run_helper()
                else:
                    self.write_session("COMPLETED")
                    record = self.state / "session.json"
                    if condition == "corrupt":
                        record.write_text("{broken")
                    elif condition == "permissions":
                        record.chmod(0o644)
                    elif condition == "symlink":
                        record.unlink()
                        record.symlink_to(self.target.parent / "missing.json")
                    else:
                        record.unlink()
                    result = self.run_helper()
                self.assertIn("延后", result.stderr)
                self.assertFalse(self.trace.exists())

    # 用途：验证仍透传原镜像保留策略，清理失败只作为维护告警。
    # 参数：无。
    # 返回值：无；参数丢失或失败影响发布时失败。
    def test_retention_override_and_prune_failure_are_maintenance_only(self):
        fake = self.target / "bin/prune-images.sh"
        with fake.open("a") as stream:
            stream.write("exit 9\n")
        result = self.run_helper("--keep-hours", "96")
        self.assertEqual(json.loads(self.trace.read_text())["args"],
                         ["--keep-hours", "96", "--apply"])
        self.assertIn("不影响发布", result.stderr)


if __name__ == "__main__":
    unittest.main()
