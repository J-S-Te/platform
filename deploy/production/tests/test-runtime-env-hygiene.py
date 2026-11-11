#!/usr/bin/env python3
"""运行配置门禁回归：重复键（同值/异值）与生产 worker 标志都必须被拦住。

两次现场真实故障固定成断言：

  1. `runtime/contract.env` 里 `CONTRACT_IMPORT_HISTORY_ENABLED`、
     `CONTRACT_IMPORT_CRM_PROVISION_ENABLED` 各出现两次 -> 接入报
     `validate runtime environment group`；
  2. `project-migrate` 服务把 `PROJECT_RUN_WORKER_WITH_API` 写成 `"true"`
     -> 生产校验报 `PROJECT_RUN_WORKER_WITH_API must be false in production`。

扫描范围同样被固定成断言：**只校验权威编排 `docker-compose.yml`**。非发布副本
（`compose.production.yaml`、评审样本 `compose.yaml`）不参与生产编排校验，出现时只告警、
退出码仍为 0——2026-11 现场曾把运维手工投放的 `compose.production.yaml` 的 `:-true`
当生产编排判失败，运维被迫手工改非发布文件。

另外覆盖：干净配置放行、`${...:-true}` 插值失败、`--apps` 过滤、无 runtime 目录时
明确跳过（不误报），以及 `--dry-run` 零副作用（不改文件、不产生新文件）。
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

PRODUCTION = Path(__file__).resolve().parents[1]
SCRIPT = PRODUCTION / "bin" / "check-runtime-env-hygiene.py"
WORKER = "PROJECT_RUN_WORKER_WITH_API"

# 一份干净的编排：迁移、API、Worker 与锚点全部写死 false。
CLEAN_COMPOSE = """\
services:
  project-migrate:
    image: project-management/project:pending
    environment:
      PM_HTTP_ADDR: :8082
      PROJECT_RUN_WORKER_WITH_API: "false"
  project-api:
    image: project-management/project:pending
    environment:
      PROJECT_RUN_WORKER_WITH_API: "false"
  project-sla-notifier:
    image: project-management/project:pending
    environment:
      PROJECT_RUN_WORKER_WITH_API: "false"
"""


def snapshot(root):
    """目录快照：相对路径 -> (内容摘要, 修改时间, 大小)，用于证明零副作用。"""
    entries = {}
    for path in sorted(Path(root).rglob("*")):
        if path.is_file() and not path.is_symlink():
            stat = path.stat()
            entries[str(path.relative_to(root))] = (
                hashlib.sha256(path.read_bytes()).hexdigest(), stat.st_mtime_ns, stat.st_size)
        else:
            entries[str(path.relative_to(root))] = ("dir", 0, 0)
    return entries


class RuntimeEnvHygieneGateTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="uip-runtime-env-gate-")
        self.root = Path(self.temporary.name)
        self.deploy_dir = self.root / "deploy"
        self.deploy_dir.mkdir()
        (self.deploy_dir / "runtime").mkdir()

    def tearDown(self):
        self.temporary.cleanup()

    # 用途：落盘一份 runtime env 与编排文件。
    # 参数：runtime 是 {文件名: 内容}，compose 是 {文件名: 内容}。
    # 返回值：无。
    def write_fixture(self, runtime=None, compose=None):
        for name, content in (runtime or {}).items():
            (self.deploy_dir / "runtime" / name).write_text(content, encoding="utf-8")
        for name, content in (compose or {}).items():
            (self.deploy_dir / name).write_text(content, encoding="utf-8")

    # 用途：运行门禁脚本。
    # 参数：extra 追加命令行参数。
    # 返回值：subprocess 结果。
    def run_gate(self, *extra):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--deploy-dir", str(self.deploy_dir), *extra],
            capture_output=True, text=True)

    # 用途：断言失败并给出预期诊断。
    # 参数：result 是进程结果；expected 是需要出现的片段。
    # 返回值：无。
    def assert_failure(self, result, *expected):
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode, 1, output)
        for fragment in expected:
            self.assertIn(fragment, output)

    # 现场故障 1：同值重复也必须失败（provisioner 只看行数，不看值）。
    def test_duplicate_key_same_value_fails(self):
        self.write_fixture(
            runtime={"contract.env": (
                "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
                "CONTRACT_IMPORT_CRM_PROVISION_ENABLED=true\n"
                "\n"
                "# CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
                "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
            )},
            compose={"docker-compose.yml": CLEAN_COMPOSE})
        result = self.run_gate()
        self.assert_failure(result, "runtime-env-duplicate-key", "runtime/contract.env",
                            "CONTRACT_IMPORT_HISTORY_ENABLED", "第 1、5 行", "值相同",
                            "validate runtime environment group", "先修配置再发布")
        # 注释行不参与计数：只有第 1、5 行，不能把第 4 行的注释算进去。
        self.assertNotIn("第 1、4", result.stdout + result.stderr)

    # 现场故障 1 的变体：不同值一样失败，两条重复键都要列出。
    def test_duplicate_key_different_values_fails(self):
        self.write_fixture(
            runtime={"contract.env": (
                "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
                "CONTRACT_IMPORT_CRM_PROVISION_ENABLED=false\n"
                "CONTRACT_IMPORT_HISTORY_ENABLED=false\n"
                "CONTRACT_IMPORT_CRM_PROVISION_ENABLED=true\n"
            )},
            compose={"docker-compose.yml": CLEAN_COMPOSE})
        result = self.run_gate()
        self.assert_failure(result, "runtime-env-duplicate-key",
                            "CONTRACT_IMPORT_HISTORY_ENABLED",
                            "CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "值不同")

    # 干净配置必须放行（含注释、空行与 export 前缀，均不误报）。
    def test_clean_runtime_and_compose_passes(self):
        self.write_fixture(
            runtime={
                "contract.env": (
                    "# 合同侧入站开关\n"
                    "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
                    "\n"
                    "export CONTRACT_IMPORT_CRM_PROVISION_ENABLED=false\n"
                ),
                "project.env": "PROJECT_RUN_WORKER_WITH_API=false\n",
            },
            compose={"docker-compose.yml": CLEAN_COMPOSE})
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("通过", result.stdout)
        self.assertIn("无重复键", result.stdout)

    # 现场故障 2：project-migrate 的 true 必须失败并指出服务与行号。
    def test_compose_true_for_migrate_fails(self):
        self.write_fixture(
            runtime={"project.env": "PROJECT_RUN_WORKER_WITH_API=false\n"},
            compose={"docker-compose.yml": CLEAN_COMPOSE.replace(
                '  project-migrate:\n    image: project-management/project:pending\n'
                '    environment:\n      PM_HTTP_ADDR: :8082\n'
                '      PROJECT_RUN_WORKER_WITH_API: "false"',
                '  project-migrate:\n    image: project-management/project:pending\n'
                '    environment:\n      PM_HTTP_ADDR: :8082\n'
                '      PROJECT_RUN_WORKER_WITH_API: "true"')})
        result = self.run_gate()
        self.assert_failure(result, "worker-flag-true", "docker-compose.yml", "project-migrate",
                            "must be false in production", "先修配置再发布")

    # 插值默认 true 等于生产里就是 true：必须失败。
    def test_compose_default_true_interpolation_fails(self):
        self.write_fixture(
            runtime={"project.env": "PROJECT_RUN_WORKER_WITH_API=false\n"},
            compose={"docker-compose.yml": CLEAN_COMPOSE.replace(
                '      PROJECT_RUN_WORKER_WITH_API: "false"',
                '      PROJECT_RUN_WORKER_WITH_API: ${PROJECT_RUN_WORKER_WITH_API:-true}', 1)})
        result = self.run_gate()
        self.assert_failure(result, "worker-flag-default-true", "project-migrate", ":-true")

    # 非发布副本 compose.production.yaml 里的 true 只告警、不失败（退出码 0）。
    # 它不在交付资产包内，可能是运维手工投放的历史 overlay，不参与生产编排校验。
    def test_production_compose_overlay_true_only_warns(self):
        self.write_fixture(
            runtime={"project.env": "PROJECT_RUN_WORKER_WITH_API=false\n"},
            compose={
                "docker-compose.yml": CLEAN_COMPOSE,
                "compose.production.yaml": (
                    "services:\n"
                    "  project-migrate:\n"
                    "    environment:\n"
                    '      PROJECT_RUN_WORKER_WITH_API: "true"\n'),
            })
        result = self.run_gate()
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode, 0, output)
        self.assertIn("告警", output)
        self.assertIn("compose.production.yaml", output)
        self.assertIn("非发布", output)
        self.assertIn("不参与", output)
        # 告警不是失败：既不能出现失败项，也不能把非发布副本写进 worker 标志证据。
        self.assertNotIn("运行配置门禁失败", output)
        self.assertIn("通过", output)

    # 部署目录只放 compose.production.yaml（且值为 :-true）时也必须放行：没有权威编排，
    # worker 标志检查按显式跳过处理，只对非发布副本告警。
    def test_only_production_compose_copy_does_not_fail(self):
        (self.deploy_dir / "runtime" / "project.env").write_text(
            "PROJECT_RUN_WORKER_WITH_API=false\n", encoding="utf-8")
        (self.deploy_dir / "compose.production.yaml").write_text(
            "services:\n"
            "  project-migrate:\n"
            "    environment:\n"
            "      PROJECT_RUN_WORKER_WITH_API: ${PROJECT_RUN_WORKER_WITH_API:-true}\n",
            encoding="utf-8")
        result = self.run_gate()
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode, 0, output)
        self.assertIn("compose.production.yaml", output)
        self.assertIn("非发布", output)
        self.assertIn("跳过生产 worker 标志检查", output)
        self.assertNotIn("运行配置门禁失败", output)

    # 评审对照样本 compose.yaml（文件头明文禁止用于部署）同样只告警、不失败。
    def test_review_sample_compose_yaml_only_warns(self):
        self.write_fixture(
            runtime={"project.env": "PROJECT_RUN_WORKER_WITH_API=false\n"},
            compose={
                "docker-compose.yml": CLEAN_COMPOSE,
                "compose.yaml": (
                    "services:\n"
                    "  project-migrate:\n"
                    "    environment:\n"
                    "      PROJECT_RUN_WORKER_WITH_API: ${PROJECT_RUN_WORKER_WITH_API:-true}\n"),
            })
        result = self.run_gate()
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode, 0, output)
        self.assertIn("compose.yaml", output)
        self.assertIn("告警", output)
        self.assertNotIn("运行配置门禁失败", output)

    # --json 证据里 warnings 与 failures 分离：非发布副本只进 warnings，证据 ok 为真。
    def test_json_evidence_separates_warnings_from_failures(self):
        self.write_fixture(
            runtime={"project.env": "PROJECT_RUN_WORKER_WITH_API=false\n"},
            compose={
                "docker-compose.yml": CLEAN_COMPOSE,
                "compose.production.yaml": (
                    "services:\n"
                    "  project-migrate:\n"
                    "    environment:\n"
                    '      PROJECT_RUN_WORKER_WITH_API: "true"\n'),
            })
        result = self.run_gate("--json")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        evidence = json.loads(result.stdout)
        self.assertTrue(evidence["ok"])
        self.assertEqual(evidence["failures"], [])
        self.assertEqual(evidence["non_release_compose_copies"], ["compose.production.yaml"])
        warning = evidence["warnings"][0]
        self.assertEqual(warning["code"], "non-release-compose-copy")
        self.assertEqual(warning["subject"], "compose.production.yaml")
        self.assertIn("不参与", warning["message"])
        # 权威编排是唯一的校验对象。
        self.assertEqual(evidence["compose_files"], ["docker-compose.yml"])
        self.assertTrue(all(item["ok"] for item in evidence["worker_flags"]))

    # --apps 只检查指定子系统的 runtime 文件；未指定的文件即使有重复也不判定。
    def test_apps_filter_limits_runtime_files(self):
        self.write_fixture(
            runtime={
                "contract.env": "CONTRACT_IMPORT_HISTORY_ENABLED=true\n",
                "settlement.env": "SETTLEMENT_X=true\nSETTLEMENT_X=true\n",
            },
            compose={"docker-compose.yml": CLEAN_COMPOSE})
        result = self.run_gate("--apps", "contract_management")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        # 不指定 --apps 时同一份配置必须失败，证明过滤不是放过问题。
        result = self.run_gate()
        self.assert_failure(result, "SETTLEMENT_X", "runtime/settlement.env")

    # 无 runtime 目录时明确说明并跳过，不能误报失败。
    def test_missing_runtime_directory_skips(self):
        (self.deploy_dir / "runtime").rmdir()
        self.write_fixture(compose={"docker-compose.yml": CLEAN_COMPOSE})
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("跳过重复键检查", result.stdout)
        self.assertIn("不视为失败", result.stdout)

    # --dry-run 零副作用：不解析、不写文件、不新增文件，且即使配置有故障也不失败。
    def test_dry_run_has_no_side_effects(self):
        self.write_fixture(
            runtime={"contract.env": "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
                                     "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"},
            compose={"docker-compose.yml": CLEAN_COMPOSE.replace('"false"', '"true"', 1)})
        before = snapshot(self.root)
        result = self.run_gate("--dry-run")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("预演", result.stdout)
        self.assertEqual(before, snapshot(self.root))

    # 失败时 --json 给出机器可读证据（人类日志转 stderr，不污染 stdout）。
    def test_json_evidence_on_failure(self):
        self.write_fixture(
            runtime={"contract.env": "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"
                                     "CONTRACT_IMPORT_HISTORY_ENABLED=true\n"},
            compose={"docker-compose.yml": CLEAN_COMPOSE})
        result = self.run_gate("--json")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        evidence = json.loads(result.stdout)
        self.assertEqual(evidence["schema"], "uip.runtime-env-hygiene/v1")
        self.assertFalse(evidence["ok"])
        duplicate = evidence["duplicate_keys"][0]
        self.assertEqual(duplicate["file"], "runtime/contract.env")
        self.assertEqual(duplicate["key"], "CONTRACT_IMPORT_HISTORY_ENABLED")
        self.assertEqual(duplicate["lines"], [1, 2])
        self.assertTrue(duplicate["same_value"])
        self.assertTrue(all(item["ok"] for item in evidence["worker_flags"]))
        self.assertNotIn("运行配置门禁失败", result.stdout)

    # 未知应用编码是用法错误（退出码 2），不能静默忽略过滤条件。
    def test_unknown_app_code_is_usage_error(self):
        self.write_fixture(runtime={"contract.env": "A=1\n"})
        result = self.run_gate("--apps", "not-an-application")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("不支持的应用编码", result.stderr)


if __name__ == "__main__":
    unittest.main()
