"""验证本地命令行真实契约与恢复建议，不写入运行环境。

完整入口在临时工作区运行。Docker 仅接受只读的 Compose 版本查询。
dev-approve 已退役：测试守住「返回非零、只打印下线说明、不委派发布路径」；
假 helper（输出参数 JSON 并可按 HELPER_EXIT 失败）仍留在临时工作区，
一旦发布路径被悄悄接回，会立刻由输出与退出码暴露。
"""
from pathlib import Path
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/docker-local.sh"


def interpreter_directory():
    """子进程 PATH 必须能找到当前解释器：CI 在 /usr/bin，容器常在 /usr/local/bin。"""
    directory = Path(sys.executable).resolve().parent
    if not (directory / "python3").exists():
        found = shutil.which("python3")
        if found:
            directory = Path(found).resolve().parent
    return str(directory)


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
        # 入口脚本 source 同目录的函数库与项目后台编排，临时工作区必须带上同一批文件。
        (scripts / "local-project-background.sh").write_bytes(
            (ROOT / "scripts/local-project-background.sh").read_bytes())
        lib_dir = scripts / "lib"
        lib_dir.mkdir()
        for lib in sorted((ROOT / "scripts/lib").glob("*.sh")):
            (lib_dir / lib.name).write_bytes(lib.read_bytes())
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
        self.env = {"PATH": os.pathsep.join([str(binaries), interpreter_directory(), "/usr/bin", "/bin"]),
                    "LOCAL_DEV_OPERATOR": "fixture-operator"}

    def run_approve(self, *arguments, **environment):
        return subprocess.run(["bash", str(self.script), "dev-approve", *arguments],
                              text=True, capture_output=True, env={**self.env, **environment})

    def test_dev_approve_reports_retirement_and_still_refuses_to_publish(self):
        # 入口已退役：任意参数都不再转发，也不委派工作区根目录与运维身份。
        result = self.run_approve("--applications", "customer_and_opportunity,project_management",
                                  "--operator", "reviewed operator", "--approve", "--replace")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("V1 运行批准已下线", combined)
        self.assertIn("无需人工批准", combined)
        self.assertIn("受控接入（subsystem.sh onboard/retry）", combined)
        # 未把工作区或运维身份交给 helper，也没有输出 JSON 参数数组。
        self.assertNotIn(str(self.workspace), combined)
        self.assertNotIn("fixture-operator", combined)
        for forbidden in ("--workspace", "--applications", "--operator", "--replace", "--approve"):
            self.assertNotIn(forbidden, combined, combined)

    def test_dev_approve_has_no_publication_path_that_could_return_helper_status(self):
        # 假 helper 若被重新接回，会打印参数 JSON 并按 HELPER_EXIT=7 退出；
        # 这里同时守住「退出码不是 helper 的」和「没有审批参数语义」。
        result = self.run_approve("--applications", "customer_and_opportunity", HELPER_EXIT="7")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertNotEqual(result.returncode, 7, result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("V1 运行批准已下线", combined)
        self.assertIn("受控接入", combined)
        try:
            delegated = json.loads(result.stdout)
        except json.JSONDecodeError:
            delegated = None
        self.assertIsNone(delegated, result.stdout)
        for forbidden in ("--approve", "--replace", '["'):
            self.assertNotIn(forbidden, combined, combined)

    def test_dev_approve_function_body_cannot_reintroduce_publication(self):
        # 防回归：函数体一旦重新出现批准 helper 或审批标志，这条守卫先红灯。
        body = function("dev_approve")
        self.assertIn("return 2", body)
        self.assertIn("已下线", body)
        self.assertNotIn("refresh-local-runtime-approvals", body)
        self.assertNotIn("--approve", body)
        self.assertNotIn("--replace", body)

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
                                env={"PATH": os.pathsep.join([interpreter_directory(), "/usr/bin", "/bin"])})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        match = re.search(r"bash scripts/subsystem\.sh retry [^\n]+", result.stdout)
        self.assertIsNotNone(match, result.stdout)
        command = shlex.split(match.group(0).replace("<应用编码>", "customer_and_opportunity"))
        parsed = subprocess.run(command + ["--help"], cwd=ROOT, text=True, capture_output=True,
                                env={"PATH": os.pathsep.join([interpreter_directory(), "/usr/bin", "/bin"])})
        self.assertEqual(parsed.returncode, 0, parsed.stdout + parsed.stderr)


if __name__ == "__main__":
    unittest.main()
