#!/usr/bin/env python3
"""许可运行态门禁回归：正向事实集通过，缺卷/实例不一致/重启循环/受控部署未确认都必须失败。

现场故障（project_management 缺 /var/lib/commercial-license 卷 -> 每分钟重启 ->
平台只显示「部署 Agent 执行失败」）不能在离线部署里再复现，因此这里用 `--fixture` 事实集
把每一条失败模式固定成断言，并用 `--dry-run` + 假 docker 探针证明预演零副作用。
"""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

PRODUCTION = Path(__file__).resolve().parents[1]
SCRIPT = PRODUCTION / "bin" / "check-license-runtime.py"
INSTANCE = "4cd3dc79aabbccddeeff001122334455"
VOLUME = "basic-platform-production_license-state-project-api"


def container(service, application, *, instance=INSTANCE, volume=VOLUME, env_extra=None,
              status="running", health="healthy", restart_count=0, exit_code=0,
              mount_type="volume", with_mount=True):
    """构造一份容器事实；默认即“刚修复完成的健康容器”。"""
    env = {
        "COMMERCIAL_LICENSE_ENABLED": "true",
        "COMMERCIAL_LICENSE_INSTANCE_ID": instance,
        "COMMERCIAL_LICENSE_STATE_PATH": "/var/lib/commercial-license/%s-abc.json" % service,
    }
    env.update(env_extra or {})
    mounts = []
    if with_mount:
        mounts.append({"type": mount_type,
                       "name": VOLUME if mount_type == "volume" else "/srv/license-state",
                       "destination": "/var/lib/commercial-license"})
    return {
        "name": "basic-platform-production-%s-1" % service,
        "service": service,
        "application": application,
        "status": status,
        "health": health,
        "restart_count": restart_count,
        "exit_code": exit_code,
        "mounts": mounts,
        "env": env,
    }


def platform_containers():
    """基础平台 API（生产环境）与平台库，供检查 3/4 读取。"""
    return [
        {"name": "basic-platform-production-platform-api-1", "service": "platform-api",
         "status": "running", "health": "healthy",
         "env": {"APP_ENV": "production", "AUDIT_ENVIRONMENT_CODE": "prod"}},
        {"name": "basic-platform-production-platform-mysql-1", "service": "platform-mysql",
         "status": "running", "env": {"MYSQL_DATABASE": "basic_platform"}},
    ]


class LicenseRuntimeGateTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="uip-license-gate-")
        self.root = Path(self.temporary.name)
        self.deploy_dir = self.root / "deploy"
        self.deploy_dir.mkdir()
        # 部署目录既有约定：项目名来自 .env 的 COMPOSE_PROJECT_NAME。
        (self.deploy_dir / ".env").write_text(
            "COMPOSE_PROJECT_NAME=basic-platform-production\nMYSQL_ROOT_PASSWORD=not-a-real-secret\n",
            encoding="utf-8")

    def tearDown(self):
        self.temporary.cleanup()

    # 用途：把一份事实集落盘并运行门禁。
    # 参数：factories 是事实集函数；extra_args 追加命令行参数。
    # 返回值：subprocess 结果。
    def run_gate(self, fixture, *extra):
        path = self.root / "fixture.json"
        path.write_text(json.dumps(fixture), encoding="utf-8")
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--deploy-dir", str(self.deploy_dir),
             "--fixture", str(path), *extra],
            capture_output=True, text=True)

    def fixture(self, *, app_container=None, containers=None, verify=None,
                platform_instance=INSTANCE):
        return {
            "containers": (containers if containers is not None
                           else [app_container or container("project-api", "project_management")]
                           + platform_containers()),
            "platform_instance_id": platform_instance,
            "verify_deployment": verify if verify is not None else {"project_management": {"exit_code": 0}},
        }

    def assert_failure(self, result, code, *fragments):
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn(code, result.stdout, result.stdout + result.stderr)
        for fragment in fragments:
            self.assertIn(fragment, result.stdout, result.stdout + result.stderr)

    def test_healthy_runtime_passes(self):
        result = self.run_gate(self.fixture())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("通过：1 个已登记商业应用", result.stdout)
        # 证据只允许出现实例前缀，不允许出现完整实例标识或许可原文。
        self.assertNotIn(INSTANCE, result.stdout + result.stderr)

    def test_reimbursement_is_discovered_and_checked(self):
        """报销是第 7 个商业应用：必须进入自动发现集合，不能被未知应用过滤成假绿。"""
        containers = [container("reimbursement-api", "reimbursement")] + platform_containers()
        result = self.run_gate(self.fixture(containers=containers,
                                            verify={"reimbursement": {"exit_code": 0}}), "--json")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        evidence = json.loads(result.stdout)
        self.assertEqual([item["application"] for item in evidence["applications"]], ["reimbursement"])
        self.assertEqual(evidence["applications"][0]["name"], "报销管理系统")
        self.assertEqual(evidence["applications"][0]["verify_deployment"], "passed")
        self.assertEqual(evidence["failures"], [])

    def test_json_evidence_reports_masked_instance(self):
        result = self.run_gate(self.fixture(), "--json")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        evidence = json.loads(result.stdout)
        self.assertTrue(evidence["ok"])
        self.assertEqual(evidence["platform"]["license_instance"]["prefix"], INSTANCE[:8])
        self.assertEqual(evidence["applications"][0]["license_state_volume"], VOLUME)
        self.assertNotIn(INSTANCE, result.stdout)

    def test_missing_license_state_volume_is_rejected(self):
        """本次现场故障的直接根因：缺卷必须精确报出并提供可执行处置。"""
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management", with_mount=False)))
        self.assert_failure(
            result, "license-state-volume-missing",
            "project_management 的 API 容器缺少许可状态卷，容器会陷入重启循环；"
            "请重新执行受控更新以按固定记录重建",
            "项目服务内容管理系统")

    def test_bind_mount_is_not_accepted_as_license_state_volume(self):
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management", mount_type="bind")))
        self.assert_failure(result, "license-state-not-volume", "许可状态目录不是命名卷")

    def test_instance_mismatch_is_rejected(self):
        """容器带着别的实例的许可会持续拒绝并重启，必须报出不一致。"""
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management",
                                    instance="ffffffffffffffffffffffffffffffff")))
        self.assert_failure(result, "license-instance-mismatch", "许可实例与平台不一致",
                            "ffffffff", INSTANCE[:8])

    def test_missing_instance_environment_is_rejected(self):
        env = {"COMMERCIAL_LICENSE_INSTANCE_ID": ""}
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management", env_extra=env)))
        self.assert_failure(result, "license-instance-missing", "缺少合法的 COMMERCIAL_LICENSE_INSTANCE_ID")

    def test_disabled_license_runtime_is_rejected(self):
        """禁止把关闭 COMMERCIAL_LICENSE_ENABLED 当成绕过手段。"""
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management",
                                    env_extra={"COMMERCIAL_LICENSE_ENABLED": "false"})))
        self.assert_failure(result, "license-runtime-disabled", "禁止用关闭许可绕过门禁")

    def test_restart_loop_is_rejected(self):
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management",
                                    health="starting", restart_count=3)))
        self.assert_failure(result, "container-restart-loop", "已重启 3 次")

    def test_stopped_container_is_rejected_when_explicitly_required(self):
        """continue/verify <CLI参数> 走显式应用编码：容器没起来必须是失败。"""
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management",
                                    status="exited", health="none", exit_code=1)),
            "--apps", "project_management")
        self.assert_failure(result, "container-not-running", "未运行")

    def test_stopped_container_is_skipped_on_discovery(self):
        """显式停用（disable）只停容器：自动发现时按停用跳过，不能误判成部署失败。"""
        result = self.run_gate(self.fixture(
            app_container=container("project-api", "project_management",
                                    status="exited", health="none", exit_code=0)))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("按已停用跳过", result.stdout)
        self.assertIn("已显式停用", result.stdout)

    def test_missing_api_container_is_rejected(self):
        containers = [{"name": "basic-platform-production-project-worker-1",
                       "service": "project-worker", "application": "project_management"}] \
            + platform_containers()
        result = self.run_gate(self.fixture(containers=containers), "--apps", "project_management")
        self.assert_failure(result, "container-missing", "project_management 的 API 容器不存在")

    def test_verify_deployment_failure_is_reported_with_output(self):
        result = self.run_gate(self.fixture(
            verify={"project_management": {"exit_code": 1,
                                           "output": "受控部署尚未完成：等待 project_management/prod 确认"}}))
        self.assert_failure(result, "verify-deployment-failed", "受控部署核验未通过",
                            "受控部署尚未完成")

    def test_missing_license_deployment_row_is_rejected(self):
        fixture = self.fixture()
        fixture["license_deployment_missing"] = True
        result = self.run_gate(fixture)
        self.assert_failure(result, "license-instance-unreadable", "请先导入并激活许可")

    def test_platform_api_missing_blocks_gate(self):
        containers = [container("project-api", "project_management")] + [
            item for item in platform_containers() if item["service"] == "platform-mysql"]
        result = self.run_gate(self.fixture(containers=containers))
        self.assert_failure(result, "platform-api-missing", "基础平台 API 容器未运行")

    def test_non_production_environment_skips_controlled_deployment_check(self):
        """license-install 拒绝在非 production/prod 运行；门禁必须显式记为跳过而非伪造通过。"""
        containers = [container("project-api", "project_management"),
                      {"name": "basic-platform-local-api-1", "service": "api",
                       "status": "running", "health": "healthy",
                       "env": {"APP_ENV": "development", "AUDIT_ENVIRONMENT_CODE": "dev"}},
                      {"name": "basic-platform-local-mysql-1", "service": "mysql",
                       "status": "running", "env": {"MYSQL_DATABASE": "basic_platform"}}]
        result = self.run_gate(self.fixture(containers=containers), "--json")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        evidence = json.loads(result.stdout)
        self.assertIn("skipped", evidence["applications"][0]["verify_deployment"])

    def test_unknown_application_code_is_rejected(self):
        result = self.run_gate(self.fixture(), "--apps", "not_an_application")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("不支持的应用编码", result.stderr)

    def test_dry_run_has_no_docker_side_effects(self):
        """--dry-run 必须是纯计划：假 docker 探针一旦被调用就会留下标记文件。"""
        marker = self.root / "docker-invoked"
        fake_bin = self.root / "bin"
        fake_bin.mkdir()
        probe = fake_bin / "docker"
        probe.write_text("#!/usr/bin/env bash\nprintf 'invoked %s\\n' \"$*\" >> %s\nexit 1\n"
                         % ("%s", marker), encoding="utf-8")
        probe.chmod(probe.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
        environment = dict(os.environ)
        environment["PATH"] = "%s:%s" % (fake_bin, os.environ.get("PATH", ""))
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--deploy-dir", str(self.deploy_dir), "--dry-run",
             "--apps", "project_management"],
            capture_output=True, text=True, env=environment)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("零副作用", result.stdout)
        self.assertFalse(marker.exists(), "dry-run 不允许调用 docker")
        # 同一假探针在真实（非预演）模式下会失败，证明上面的“未被调用”不是因为探针没生效。
        live = subprocess.run(
            [sys.executable, str(SCRIPT), "--deploy-dir", str(self.deploy_dir),
             "--apps", "project_management"],
            capture_output=True, text=True, env=environment)
        self.assertNotEqual(live.returncode, 0, live.stdout + live.stderr)
        self.assertTrue(marker.exists(), "真实模式必须调用 docker")


if __name__ == "__main__":
    unittest.main()
