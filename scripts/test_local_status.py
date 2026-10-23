"""状态总览的回归测试：只读、判断正确、建议可执行。

用临时 PATH 上的 docker 替身驱动，不接触真实 Docker、数据库或状态文件。
"""
from pathlib import Path
import json
import os
import re
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/local-status.py"

DOCKER_STUB = """#!/bin/sh
# 替身：按环境变量回答，不接触真实引擎。
case "$1" in
  info) [ "$STUB_DOCKER_OK" = "1" ] && exit 0 || exit 1 ;;
  context) echo orbstack; exit 0 ;;
  ps)
    case "$*" in
      *"-aq"*) echo "$STUB_CONTAINER_IDS"; exit 0 ;;
      *) echo "$STUB_CONTAINERS"; exit 0 ;;
    esac ;;
  image)
    case "$*" in
      *inspect*) case " $STUB_IMAGES " in *" $3 "*) exit 0 ;; *) exit 1 ;; esac ;;
    esac
    exit 0 ;;
  inspect)
    case "$*" in
      *discovery*) echo "$STUB_DISCOVERY"; exit 0 ;;
      *application_code*) echo "$STUB_APPCODE"; exit 0 ;;
    esac
    echo "<no value>"; exit 0 ;;
  exec)
    case "$*" in
      *license_deployment*) echo "$STUB_INSTANCES"; exit 0 ;;
      *license_artifact*) echo "$STUB_ARTIFACTS"; exit 0 ;;
      *iam_account*) echo "$STUB_ACCOUNTS"; exit 0 ;;
    esac
    echo 0; exit 0 ;;
esac
exit 0
"""


class LocalStatusTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        binaries = root / "bin"
        binaries.mkdir()
        docker = binaries / "docker"
        docker.write_text(DOCKER_STUB)
        docker.chmod(0o755)
        self.binaries = binaries
        # 固定记录与批准夹具（脚本按固定路径读取，因此复制脚本到临时工作区）
        self.workspace = root / "ws"
        (self.workspace / "platform" / "scripts").mkdir(parents=True)
        (self.workspace / "platform" / "runtime").mkdir()
        (self.workspace / "platform" / "runtime-approvals").mkdir()
        (self.workspace / "platform" / "docker").mkdir()
        (self.workspace / "platform" / "docker" / ".env.local").write_text("MYSQL_USER=u\nMYSQL_PASSWORD=p\nMYSQL_DATABASE=d\n")
        self.script = self.workspace / "platform" / "scripts" / "local-status.py"
        self.script.write_text(SCRIPT.read_text(encoding="utf-8"))

    def write_pin(self, digest):
        (self.workspace / "platform" / "runtime" / "local-license.compose.json").write_text(
            json.dumps({"services": {"customer-api": {"image": digest}}, "volumes": {}}))

    def write_approval(self, name, digest):
        (self.workspace / "platform" / "runtime-approvals" / ("local-" + name + "-dev.json")).write_text(
            json.dumps({"services": [{"name": "customer-api", "image_digest": digest}],
                        "migration": {"service": "customer-migrate", "image_digest": digest}}))

    def run_status(self, **env):
        environment = {"PATH": str(self.binaries) + ":/usr/bin:/bin",
                       "STUB_DOCKER_OK": "1", "STUB_CONTAINERS": "api-1", "STUB_CONTAINER_IDS": "",
                       "STUB_IMAGES": "", "STUB_DISCOVERY": "", "STUB_APPCODE": "",
                       "STUB_INSTANCES": "1", "STUB_ARTIFACTS": "1", "STUB_ACCOUNTS": "1"}
        environment.update({k: str(v) for k, v in env.items()})
        return subprocess.run(["python3", str(self.script)], text=True,
                              capture_output=True, env=environment)

    def test_reports_docker_unavailable_with_action(self):
        result = self.run_status(STUB_DOCKER_OK="0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docker context use orbstack", result.stdout)

    def test_missing_pinned_image_is_reported_without_asking_to_republish(self):
        self.write_pin("sha256:" + "a" * 64)
        self.write_approval("customer_and_opportunity", "sha256:" + "b" * 64)
        result = self.run_status(STUB_IMAGES="sha256:" + "b" * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("固定记录待受控更新刷新", result.stdout)
        self.assertRegex(result.stdout, "(?:无需|不用)重新发布")
        self.assertNotIn("需重新生成并发布", result.stdout)

    def test_controlled_retry_suggestion_is_accepted_by_real_parser(self):
        self.write_pin("sha256:" + "a" * 64)
        self.write_approval("customer_and_opportunity", "sha256:" + "b" * 64)
        result = self.run_status(STUB_IMAGES="sha256:" + "b" * 64)
        match = re.search(r"bash scripts/subsystem\.sh retry [^；\n]+", result.stdout)
        self.assertIsNotNone(match, result.stdout)
        command = shlex.split(match.group(0).replace("<应用编码>", "customer_and_opportunity"))
        # --help 在参数解析阶段退出；不会认证、联网或触发受控更新。
        parsed = subprocess.run(command + ["--help"], cwd=ROOT, text=True,
                                capture_output=True, env={"PATH": "/usr/bin:/bin"})
        self.assertEqual(parsed.returncode, 0, parsed.stdout + parsed.stderr)

    def test_stale_approval_asks_for_republish(self):
        self.write_pin("sha256:" + "a" * 64)
        self.write_approval("customer_and_opportunity", "sha256:" + "c" * 64)
        result = self.run_status(STUB_IMAGES="sha256:" + "a" * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("需重新生成并发布", result.stdout)

    def test_uninitialized_license_is_reported(self):
        self.write_pin("sha256:" + "a" * 64)
        result = self.run_status(STUB_IMAGES="sha256:" + "a" * 64, STUB_INSTANCES="0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("授权实例未初始化", result.stdout)

    def test_missing_admin_is_reported(self):
        self.write_pin("sha256:" + "a" * 64)
        result = self.run_status(STUB_IMAGES="sha256:" + "a" * 64, STUB_ACCOUNTS="0")
        self.assertIn("bootstrap-admin", result.stdout)

    def test_ready_state_has_no_blockers(self):
        self.write_pin("sha256:" + "a" * 64)
        result = self.run_status(STUB_IMAGES="sha256:" + "a" * 64, STUB_DISCOVERY="v1",
                                STUB_APPCODE="crm", STUB_CONTAINER_IDS="c1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("控制面就绪", result.stdout)


if __name__ == "__main__":
    unittest.main()
