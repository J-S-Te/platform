#!/usr/bin/env python3
"""使用真实配置函数与临时文件验证升级保留关键材料，不连接业务库。"""
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
LINUX = [ROOT / "scripts/bootstrap-linux.sh", ROOT / "scripts/backend-only-linux.sh"]
COMMON = ROOT / "deploy/production/bin/deploy-common.sh"


def function(source, name, optional=False):
    """用途：提取待测脚本的实际函数，避免复制生产实现。
    参数：source 为源码，name 为函数名，optional 表示允许已删除的旧辅助函数。
    返回值：实际函数文本；必需函数缺失时断言失败。
    """
    found = re.search(r"^" + re.escape(name) + r"\(\) \{\n.*?^\}", source, re.M | re.S)
    if not found and optional:
        return ""
    assert found, f"missing function {name}"
    return found.group()


def run_functions(directory, functions, body):
    """用途：只执行真实函数边界，隔离日志、属主调整及外部初始化命令。
    参数：directory 为本用例临时目录，functions 为函数文本，body 为调用步骤。
    返回值：子进程结果；不执行完整部署或读取运行环境凭据。
    """
    setup = r'''
set -Eeuo pipefail
deploy_dir="$1"; ENV_FILE="$1/.env"; ENV_EXAMPLE="$1/.env.example"
DRY_RUN=false; TEMP_PATHS=(); RUNTIME_GROUP=fixture
log() { :; }
run_cmd() { :; }
die() { echo "$*" >&2; exit 1; }
'''
    return subprocess.run(
        ["bash", "-c", setup + functions + "\n" + body, "fixture", str(directory)],
        capture_output=True, text=True, timeout=15,
    )


class UpgradeConfigTests(unittest.TestCase):
    """通过文件字节与外部调用结果检查关键配置保留。"""

    def test_linux_bootstrap_preserves_config_on_every_outcome(self):
        """用途：阻止已有管理员、首次成功或失败时清空原始密码。
        参数：无；场景覆盖两套 Linux 部署入口。
        返回值：无；文件被改写、错误被吞掉或越过状态检查时失败。
        """
        for script in LINUX:
            source = script.read_text()
            functions = "\n".join(function(source, name, name.startswith("clear_")) for name in [
                "get_env_value", "set_env_value", "clear_bootstrap_admin_password",
                "bootstrap_first_super_admin",
            ])
            for status, initialized, expected in [(0, 0, 0), (3, 0, 0), (3, 1, 1), (5, 0, 5)]:
                for password in ["original-fixture-only", "", None]:
                    with self.subTest(script=script.name, status=status, password=password):
                        with tempfile.TemporaryDirectory() as tmp:
                            root = Path(tmp)
                            binary = root / "bootstrap-admin"
                            binary.write_text("#!/bin/sh\nexit 0\n")
                            binary.chmod(0o700)
                            content = "MYSQL_PASSWORD=untouched-fixture-only\n"
                            if password is not None:
                                content += f"IAM_BOOTSTRAP_ADMIN_PASSWORD={password}\n"
                            (root / ".env").write_text(content)
                            body = f'''
run_bootstrap_admin_status() {{ return {status}; }}
load_bootstrap_admin_input() {{ BOOTSTRAP_ADMIN_PASSWORD=process-only; }}
run_bootstrap_admin_initialize() {{ return {initialized}; }}
bootstrap_first_super_admin "$deploy_dir/bootstrap-admin"
'''
                            result = run_functions(root, functions, body)
                            self.assertEqual(result.returncode, expected, result.stderr)
                            self.assertEqual((root / ".env").read_text(), content)

    def test_existing_generated_secrets_remain_byte_identical(self):
        """用途：升级不得为已有配置中的空值、缺项或占位材料重新造密码。
        参数：无；使用虚构密码、引号、export 和空字段。
        返回值：无；升级配置任一字节改变时失败。
        """
        source = COMMON.read_text()
        functions = "\n".join(function(source, n) for n in ["env_get", "env_set", "env_set_generated"])
        for line in ["", "IAM_BOOTSTRAP_ADMIN_PASSWORD=\n",
                     "IAM_BOOTSTRAP_ADMIN_PASSWORD=PENDING_restore_original\n",
                     "export IAM_BOOTSTRAP_ADMIN_PASSWORD='fixture $ & \\ only'\n"]:
            with self.subTest(line=line), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                content = "# original\nMYSQL_PASSWORD=untouched\n" + line
                (root / ".env").write_text(content)
                result = run_functions(root, functions,
                    'env_set_generated "$ENV_FILE" IAM_BOOTSTRAP_ADMIN_PASSWORD new-fixture false')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((root / ".env").read_text(), content)

    def test_first_configuration_can_generate_secret(self):
        """用途：保留首次创建配置时的随机凭据初始化能力。
        参数：无；模板只包含虚构占位值。
        返回值：无；首次配置没有填入候选值时失败。
        """
        source = COMMON.read_text()
        functions = "\n".join(function(source, n) for n in ["env_get", "env_set", "env_set_generated"])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / ".env").write_text("IAM_BOOTSTRAP_ADMIN_PASSWORD=REPLACE_WITH_PASSWORD\n")
            result = run_functions(root, functions,
                'env_set_generated "$ENV_FILE" IAM_BOOTSTRAP_ADMIN_PASSWORD first-fixture true')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / ".env").read_text(), "IAM_BOOTSTRAP_ADMIN_PASSWORD=first-fixture\n")

    def test_template_upgrade_does_not_fill_secret_defaults(self):
        """用途：已有开发配置不能被模板中的演示密码或密钥补写。
        参数：无；保留普通新增配置的兼容补齐能力。
        返回值：无；敏感项变化或普通新增项丢失时失败。
        """
        source = LINUX[0].read_text()
        functions = "\n".join(function(source, n) for n in [
            "get_env_value", "set_env_value", "merge_env_defaults",
        ])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / ".env").write_text("MYSQL_PASSWORD=\nIAM_BOOTSTRAP_TOKEN=\n")
            (root / ".env.example").write_text(
                "MYSQL_PASSWORD=demo-password\nIAM_BOOTSTRAP_TOKEN=demo-token\nNEW_OPTION=enabled\n")
            result = run_functions(root, functions, "merge_env_defaults false")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / ".env").read_text(),
                "MYSQL_PASSWORD=\nIAM_BOOTSTRAP_TOKEN=\nNEW_OPTION=enabled\n")

    def test_missing_required_upgrade_secret_fails_without_writing(self):
        """用途：缺失数据库凭据时失败并要求恢复，不能随机补造。
        参数：无；仅使用缺少密码的临时配置。
        返回值：无；静默成功、配置改变或泄漏候选密码时失败。
        """
        source = LINUX[0].read_text()
        functions = "\n".join(function(source, n) for n in [
            "get_env_value", "set_env_value", "ensure_env_secret",
        ])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            content = "MYSQL_PASSWORD=\n"
            (root / ".env").write_text(content)
            result = run_functions(root, functions, "ensure_env_secret MYSQL_PASSWORD hex false")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((root / ".env").read_text(), content)

    def test_offline_existing_admin_needs_no_initial_password(self):
        """用途：已有管理员更新不依赖初始化密码，也不得重新调用创建。
        参数：无；部署与数据库边界使用有限命令替身。
        返回值：无；要求旧密码、调用初始化或改写文件时失败。
        """
        common = COMMON.read_text()
        source = (ROOT / "deploy/production/bin/deploy-import.sh").read_text()
        functions = function(common, "env_get") + "\n" + function(source, "deploy_component")
        for status, password, expected in [(0, None, 0), (3, None, 1), (5, None, 5),
                                           (3, "PENDING_restore_original", 1)]:
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                content = "MYSQL_PASSWORD=preserved-fixture\n"
                if password is not None:
                    content += (f"IAM_BOOTSTRAP_ADMIN_PASSWORD={password}\n"
                        "IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME=Fixture\n"
                        "IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME=fixture\n")
                (root / ".env").write_text(content)
                (root / "deploy-service.sh").write_text("#!/bin/sh\nexit 0\n")
                (root / "deploy-service.sh").chmod(0o700)
                body = f'bootstrap_status_fixture={status}\n' + r'''
    runtime_file="$ENV_FILE"; script_dir="$deploy_dir"
    control_plane_reload_marker="$deploy_dir/absent-marker"
    require_initialized() { :; }; scope_require() { :; }
    latest_image() { echo 'fixture@sha256:aaaaaaaa'; }
    imported_file_gateway_image() { echo 'gateway@sha256:bbbbbbbb'; }
    imported_docker_socket_proxy_image() { :; }
    scope_services() { :; }
    compose() {
      if [[ "$*" == *'--status'* ]]; then return "$bootstrap_status_fixture"; fi
      echo 'unexpected administrator initialization' >&2; return 9
    }
    deploy_component platform
    '''
                result = run_functions(root, functions, body)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertEqual((root / ".env").read_text(), content)

    def test_existing_configuration_cannot_recreate_lost_jwt_keys(self):
        """用途：已有配置丢失整对签名密钥时要求恢复，防止静默更换身份材料。
        参数：无；文件、目录和命令全部位于隔离夹具。
        返回值：无；允许重新生成或修改原配置时失败。
        """
        source = LINUX[0].read_text()
        functions = "\n".join(function(source, n) for n in [
            "get_env_value", "set_env_value", "merge_env_defaults", "ensure_env_secret",
            "resolve_env_path", "configure_environment",
        ])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            content = ("APP_ENV=development\nMYSQL_PASSWORD=fixture-only\n"
                "IAM_MOBILE_ENCRYPTION_KEY=fixture-only\nIAM_BOOTSTRAP_TOKEN=\n"
                f"FILE_STORAGE_ROOT={tmp}/uploads\nLOG_DIRECTORY={tmp}/logs\n"
                f"AUTH_JWT_PRIVATE_KEY_PATH={tmp}/keys/private.pem\n"
                f"AUTH_JWT_PUBLIC_KEY_PATH={tmp}/keys/public.pem\n")
            (root / ".env").write_text(content)
            result = run_functions(root, functions,
                'require_command() { :; }; configure_environment')
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((root / ".env").read_text(), content)

    def test_retained_runtime_material_is_existing_deployment_evidence(self):
        """用途：主配置丢失但子系统材料仍在时，不得误判为全新安装。
        参数：无；Docker 查询替身返回没有容器。
        返回值：无；忽略保留材料时失败。
        """
        source = (ROOT / "deploy/production/bin/offline-configure.sh").read_text()
        functions = "\n".join([function(COMMON.read_text(), "env_get"),
            function(source, "configuration_platform_exists"),
            function(source, "configuration_material_exists")])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "runtime").mkdir()
            (root / "runtime/contract.env").write_text("OIDC_CLIENT_SECRET=retained-fixture\n")
            result = run_functions(root, functions, r'''
manifest_dir="$deploy_dir/manifests"; release_file="$deploy_dir/.release.env"
docker() { return 1; }
configuration_material_exists
''')
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
