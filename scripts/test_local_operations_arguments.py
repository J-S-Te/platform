"""验证实际本地命令行委派与恢复建议，不写入运行环境。

完整入口在临时工作区运行。Docker 仅接受只读的 Compose 版本查询；
批准辅助工具只记录参数，不执行发布。
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
SCRIPT = ROOT / "scripts/docker-local.sh"


def function(name):
    source = SCRIPT.read_text(encoding="utf-8")
    return name + "() {" + source.split(name + "() {", 1)[1].split("\n}\n", 1)[0] + "\n}\n"


class LocalOperationsArgumentsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspace = Path(self.tmp.name) / "workspace with spaces"
        self.platform = self.workspace / "platform"
        scripts = self.platform / "scripts"
        scripts.mkdir(parents=True)
        docker_dir = self.platform / "docker"
        docker_dir.mkdir()
        for name in ("contract_management", "customer_and_opportunity", "project_management", "Settlement"):
            (self.workspace / name).mkdir()
        for path in (self.platform / "compose.local.yaml", docker_dir / ".env.lan.disabled",
                     scripts / "portal-gateway.sh", docker_dir / "portal-apps-locations.conf"):
            path.touch()
        self.script = scripts / "docker-local.sh"
        self.script.write_bytes(SCRIPT.read_bytes())
        (scripts / "refresh-local-runtime-approvals.py").write_text(
            "import json, os, sys\n"
            "print(json.dumps(sys.argv[1:]))\n"
            "sys.exit(int(os.environ.get('HELPER_EXIT', '0')))\n")
        binaries = Path(self.tmp.name) / "bin"
        binaries.mkdir()
        docker = binaries / "docker"
        docker.write_text('#!/bin/sh\n[ "$*" = "compose version" ] && exit 0\n'
                          'echo "Unexpected Docker call: $*" >&2\nexit 99\n')
        docker.chmod(0o755)
        self.env = {"PATH": str(binaries) + os.pathsep + "/usr/bin:/bin",
                    "LOCAL_DEV_OPERATOR": "fixture-operator"}

    def run_approve(self, *arguments, **environment):
        return subprocess.run(["bash", str(self.script), "dev-approve", *arguments],
                              text=True, capture_output=True, env={**self.env, **environment})

    def test_dev_approve_delegates_workspace_root_and_preserves_options(self):
        # 此处的 platform 目录会使消费者查找 platform/platform/config。
        result = self.run_approve("--applications", "customer_and_opportunity,project_management",
                                  "--operator", "reviewed operator", "--approve", "--replace")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout), [
            "--workspace", str(self.workspace), "--operator", "fixture-operator",
            "--applications", "customer_and_opportunity,project_management",
            "--operator", "reviewed operator", "--approve", "--replace"])

    def test_dev_approve_keeps_publication_explicit_and_returns_helper_failure(self):
        result = self.run_approve("--applications", "customer_and_opportunity", HELPER_EXIT="7")
        self.assertEqual(result.returncode, 7, result.stdout + result.stderr)
        self.assertNotIn("--approve", json.loads(result.stdout))
        self.assertNotIn("--replace", json.loads(result.stdout))

    def test_control_plane_recovery_retry_suggestion_is_accepted_by_real_parser(self):
        # 执行真实恢复函数，仅替换运维边界。
        boundaries = (
            "ensure_platform_env_file", "ensure_contract_env_file", "build_images",
            "compose_up_wait", "compose_run", "bootstrap_admin_if_needed",
            "verify_local_file_gateway", "ensure_subsystem_discovery_candidates")
        harness = "set -Eeuo pipefail\nlog() { printf '%s\\n' \"$*\"; }\n"
        harness += "".join(name + "() { :; }\n" for name in boundaries)
        harness += function("start_control_plane_only") + "start_control_plane_only\n"
        result = subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                                env={"PATH": "/usr/bin:/bin"})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        match = re.search(r"bash scripts/subsystem\.sh retry [^\n]+", result.stdout)
        self.assertIsNotNone(match, result.stdout)
        command = shlex.split(match.group(0).replace("<应用编码>", "customer_and_opportunity"))
        parsed = subprocess.run(command + ["--help"], cwd=ROOT, text=True, capture_output=True,
                                env={"PATH": "/usr/bin:/bin"})
        self.assertEqual(parsed.returncode, 0, parsed.stdout + parsed.stderr)


if __name__ == "__main__":
    unittest.main()
