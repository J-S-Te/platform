#!/usr/bin/env python3
"""验证受控运行投影在原生分析模式中省去嵌入基础设施。"""
import importlib.util
from pathlib import Path
import tempfile
import unittest
import subprocess
import sys
import shlex

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("runtime_policy", HERE / "subsystem-runtime.py")
POLICY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POLICY)
FEATURE = """
optional_features:
  - name: dashboard_embed
    runtime_file: runtime/data-analysis.env
    enabled_key: DASHBOARD_EMBED_ENABLED
    default_enabled: true
    required_existing_keys: [METABASE_EMBEDDING_SECRET]
    runtime_services: [data-analysis-metabase]
    initialization_services: [data-analysis-metabase-init]
"""


class OptionalFeaturesTests(unittest.TestCase):
    # 用途：提取真实发布入口的函数，隔离其主流程和外部命令。
    # 参数：name为待验证的Shell函数名。
    # 返回值：原函数定义；缺少函数时直接失败。
    def deployment_function(self, name):
        text = (HERE / "deploy-service.sh").read_text()
        start = text.index(name + "() {")
        return text[start:text.index("\n}", start) + 2]

    # 用途：读取真实清单构造独立临时运行环境。
    # 参数：flag为本轮明确选择的嵌入模式，secret为合成密钥。
    # 返回值：受控投影及临时部署根。
    def fixture(self, flag, secret=""):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name).resolve()
        (root / "runtime").mkdir()
        (root / "runtime/data-analysis.env").write_text(
            f"DASHBOARD_EMBED_ENABLED={flag}\nMETABASE_EMBEDDING_SECRET={secret}\n")
        (root / "runtime/data-analysis.env").chmod(0o600)
        text = (HERE.parent / "subsystems.d/data-analysis-prod.yaml").read_text()
        text = text.split("\noptional_features:")[0] + FEATURE
        return POLICY.projection(text, "data_analysis"), root

    # 用途：验证未安装Metabase不会阻断原生业务消费者启动投影。
    # 参数：无。
    # 返回值：无；多选基础设施或丢失业务消费者时失败。
    def test_native_only_requires_real_consumers(self):
        (compose, conditional, files), root = self.fixture("false")
        enabled = {"data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker"}
        self.assertEqual(set(POLICY.selected_services(root, compose, conditional, files, enabled)), enabled)

    # 用途：验证开启嵌入仍要求真实配置，未知开关失败关闭。
    # 参数：无。
    # 返回值：无；缺材料或非法开关被放行时失败。
    def test_enabled_missing_secret_and_invalid_flag_reject(self):
        for flag in ("true", "sometimes"):
            with self.subTest(flag=flag):
                (compose, conditional, files), root = self.fixture(flag)
                with self.assertRaises(ValueError):
                    POLICY.selected_services(root, compose, conditional, files, set(compose["runtime_services"]))

    # 用途：验证可选基础设施策略不能删除业务API或Worker。
    # 参数：无。
    # 返回值：无；非法策略被接受时失败。
    def test_cannot_hide_business_consumer(self):
        text = (HERE.parent / "subsystems.d/data-analysis-prod.yaml").read_text().split("\noptional_features:")[0]
        policy = FEATURE.replace("runtime_services: [data-analysis-metabase]", "runtime_services: [data-analysis-api]")
        with self.assertRaises(ValueError):
            POLICY.projection(text + policy, "data_analysis")

    # 用途：拒绝非私有运行文件，即使开关明确关闭也不能绕过文件边界。
    # 参数：无。
    # 返回值：无；可公开读取的运行文件被接受时失败。
    def test_rejects_non_private_feature_file(self):
        (compose, conditional, files), root = self.fixture("false")
        (root / "runtime/data-analysis.env").chmod(0o644)
        with self.assertRaises(ValueError):
            POLICY.selected_services(root, compose, conditional, files, set(compose["runtime_services"]))

    # 用途：验证恢复脚本实际调用的初始化投影入口，不启动已关闭的嵌入任务。
    # 参数：无。
    # 返回值：无；原生模式多选任务或嵌入模式漏选时失败。
    def test_initialization_command_obeys_explicit_mode(self):
        for flag, expected in (("false", ""), ("true", "data-analysis-metabase-init")):
            with self.subTest(flag=flag):
                _, root = self.fixture(flag, "synthetic-embed-secret")
                command = [sys.executable, str(HERE / "subsystem-runtime.py"), "initialization",
                           "--target", str(root), "--component", "data-analysis",
                           "--profiles-dir", str(HERE.parent / "subsystems.d")]
                result = subprocess.run(command, input=expected, text=True, capture_output=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), expected)
                if flag == "true":
                    missing = subprocess.run(command, input="", text=True, capture_output=True)
                    self.assertNotEqual(missing.returncode, 0)

    # 用途：运行实际发布函数，验证原生模式不会启动嵌入基础设施。
    # 参数：无。
    # 返回值：无；错误的初始化或进程选择使测试失败。
    def test_direct_publish_respects_native_runtime(self):
        for flag in ("false", "true"):
            with self.subTest(flag=flag):
                _, root = self.fixture(flag, "synthetic-secret")
                log = root / "calls.txt"
                script = f"""
set -eu
source {shlex.quote(str(HERE / 'compose-scope.sh'))}
deploy_dir={shlex.quote(str(root))}
profiles_dir={shlex.quote(str(HERE.parent / 'subsystems.d'))}
data_analysis_runtime_file={shlex.quote(str(root / 'runtime/data-analysis.env'))}
scope_services() {{ printf '%s\\n' data-analysis-api data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase data-analysis-metabase-init; }}
compose() {{ printf '%s\\n' "$*" >> {shlex.quote(str(log))}; }}
require_mysql_ready() {{ return 0; }}
backup_database_if_initialized() {{ return 0; }}
capture_data_analysis_role_config_hash() {{ return 0; }}
update_runtime_value() {{ return 0; }}
wait_for_health() {{ return 0; }}
verify_service_image() {{ return 0; }}
port_value() {{ printf '%s\\n' 18086; }}
docker() {{
  if [[ "$1" == run ]]; then printf 'claims_role_config_hash=sha256:%064d\\n' 0;
  elif [[ "$1" == image && "$2" == inspect ]]; then return 0;
  else return 1; fi
}}
data_analysis_image_refs=(api aggregation alert migration)
{self.deployment_function('deploy_data_analysis')}
deploy_data_analysis
"""
                result = subprocess.run(["bash", "-c", script], text=True, capture_output=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                commands = log.read_text()
                self.assertEqual("data-analysis-metabase" in commands, flag == "true")
                for service in ("data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker"):
                    self.assertIn(service, commands)

    # 用途：检查真实就绪函数只对已启用嵌入要求密钥。
    # 参数：无。
    # 返回值：无；原生模式被误阻断或嵌入模式缺密钥被放行时失败。
    def test_runtime_ready_requires_secret_only_for_embedding(self):
        for flag in ("false", "true"):
            with self.subTest(flag=flag):
                _, root = self.fixture(flag)
                script = f"""
set -eu
source {shlex.quote(str(HERE / 'compose-scope.sh'))}
deploy_dir={shlex.quote(str(root))}
profiles_dir={shlex.quote(str(HERE.parent / 'subsystems.d'))}
scope_services() {{ printf '%s\\n' data-analysis-api data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase; }}
data_analysis_env_value() {{
  if [[ "$1" == PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED ]]; then printf true;
  elif [[ "$1" != METABASE_EMBEDDING_SECRET ]]; then printf synthetic-material; fi
}}
require_runtime_value() {{ return 0; }}
{self.deployment_function('data_analysis_runtime_ready')}
data_analysis_runtime_ready
"""
                result = subprocess.run(["bash", "-c", script], text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, flag == "false", result.stderr)


if __name__ == "__main__":
    unittest.main()
