"""报销本地接入与运维边界测试：只运行真实纯函数及临时目录，不访问 Docker。"""
from pathlib import Path
from itertools import product
import re
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
ENTRY = ROOT / "scripts/docker-local.sh"
HELPER = ROOT / "scripts/lib/reimbursement.sh"
OPS = ROOT.parent / "系统运维工具.sh"


def function(path, name):
    """用途：提取实际脚本函数，避免执行入口主流程。
    参数：path为源码路径，name为函数名称。
    返回值：函数源码；函数缺失时测试失败。
    """
    source = path.read_text()
    match = re.search(r"^" + name + r"\(\) \{\n.*?^\}", source, re.M | re.S)
    if not match:
        raise AssertionError(f"{path.name} 缺少函数 {name}")
    return match.group() + "\n"


def shell(body, **env):
    """用途：隔离运行 shell 行为，禁止通过 PATH 调用真实 Docker。
    参数：body为待测函数和命令，env为临时测试变量。
    返回值：标准输出、错误和退出码。
    """
    return subprocess.run(
        ["bash", "-c", "set -Eeuo pipefail\n"
         "log() { printf '%s\\n' \"$*\"; }\n"
         "fail() { printf '%s\\n' \"$*\" >&2; exit 7; }\n"
         "docker() { printf 'UNEXPECTED_DOCKER\\n' >&2; return 99; }\n" + body],
        env={"PATH": "/usr/bin:/bin", **env}, text=True, capture_output=True, timeout=10,
    )


class LocalReimbursementTests(unittest.TestCase):
    """本地运行配置保留及已接入服务恢复。"""

    def source(self):
        """用途：加载无副作用的真实函数库。
        参数：无。
        返回值：加载脚本；缺失实现时断言失败。
        """
        self.assertTrue(HELPER.is_file(), "缺少报销本地接入 helper")
        return "\n".join(f"source {shlex.quote(str(p))}" for p in (
            ROOT / "scripts/lib/env-templates.sh",
            ROOT / "scripts/lib/runtime-validation.sh", HELPER)) + "\n"

    def test_template_creation_is_private_and_preserves_existing_values(self):
        """用途：核验首次模板权限、幂等和不生成密钥。
        参数：无。
        返回值：无；覆盖已有配置或生成运行材料时失败。
        """
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "runtime.env"
            result = shell(self.source() + '''
reimbursement_env_file="$TARGET"
reimbursement_template_file="$TEMPLATE"
compose_project=fixture
docker() { [[ "$1 $2" == 'volume ls' ]]; }
ensure_reimbursement_env_file
''', TARGET=str(path), TEMPLATE=str(ROOT / "docker/.env.reimbursement.local.example"))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(path.read_bytes(), (ROOT / "docker/.env.reimbursement.local.example").read_bytes())
            path.write_text("OIDC_CLIENT_SECRET=fixture-kept\n")
            result = shell(self.source() + '''
reimbursement_env_file="$TARGET"
reimbursement_template_file="$TEMPLATE"
ensure_reimbursement_env_file
''', TARGET=str(path), TEMPLATE="/missing-template")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(path.read_text(), "OIDC_CLIENT_SECRET=fixture-kept\n")

    def test_retained_volume_or_symlink_refuses_template_creation(self):
        """用途：核验配置丢失或路径不安全时不伪造新运行材料。
        参数：无。
        返回值：无；保留卷或符号链接被放行时失败。
        """
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "runtime.env"
            body = self.source() + '''
reimbursement_env_file="$TARGET"
reimbursement_template_file="$TEMPLATE"
compose_project=fixture
docker() { printf 'fixture_reimbursement-mysql-data\\n'; }
ensure_reimbursement_env_file
'''
            result = shell(body, TARGET=str(path), TEMPLATE=str(ROOT / "docker/.env.reimbursement.local.example"))
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(path.exists())
            kept = Path(tmp) / "kept.env"
            kept.write_text("kept\n")
            path.symlink_to(kept)
            result = shell(body, TARGET=str(path), TEMPLATE="/missing")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(kept.read_text(), "kept\n")

    def start(self, *, configured=True, pinned=True, omit="", failing=""):
        """用途：用临时已接入配置运行真实恢复函数。
        参数：configured/pinned控制接入和固定状态，omit缺失键，failing失败服务。
        返回值：恢复流程退出码及命令记录。
        """
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "runtime.env"
            keys = ["MYSQL_DSN", "OIDC_ISSUER", "OIDC_BACKCHANNEL_BASE_URL",
                    "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_TENANT_ID", "OIDC_REDIRECT_URI",
                    "OIDC_SESSION_ENCRYPTION_KEY_BASE64", "PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID",
                    "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
                    "PROJECT_REFERENCE_CLIENT_ID", "PROJECT_REFERENCE_CLIENT_SECRET",
                    "PROJECT_SPEND_CLIENT_ID", "PROJECT_SPEND_CLIENT_SECRET",
                    "PLATFORM_OWNER_DIRECTORY_CLIENT_ID", "PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET",
                    "PLATFORM_AUDIT_CLIENT_ID", "PLATFORM_AUDIT_CLIENT_SECRET",
                    "PLATFORM_NOTIFICATION_CLIENT_ID", "PLATFORM_NOTIFICATION_CLIENT_SECRET",
                    "FILE_GATEWAY_CLIENT_ID", "FILE_GATEWAY_CLIENT_SECRET",
                    "REIMBURSEMENT_MYSQL_PASSWORD", "REIMBURSEMENT_MYSQL_ROOT_PASSWORD"]
            path.write_text("\n".join(f"{key}=fixture-{key}" for key in keys
                                      if key != omit and (configured or key != "OIDC_CLIENT_ID")) + "\n")
            return shell(self.source() + '''
reimbursement_env_file="$TARGET"
frontend_public_origin=http://localhost:8081
runtime_target_environment() { printf dev; }
validate_local_runtime_target() { :; }
validate_keycloak_runtime() { :; }
pinned_local_service() { [[ "$PINNED" == true ]]; }
compose_run() {
    printf 'COMPOSE %s\\n' "$*"
    [[ -z "$FAILING" || " $* " != *" $FAILING "* ]]
}
start_reimbursement_if_configured
''', TARGET=str(path), PINNED=str(pinned).lower(), FAILING=failing)

    def test_unregistered_application_skips_every_compose_operation(self):
        """用途：保证未接入报销不运行数据库、API或worker。
        参数：无。
        返回值：无；存在任何Compose命令时失败。
        """
        result = self.start(configured=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("COMPOSE", result.stdout)
        self.assertIn("尚未", result.stdout)

    def test_missing_pin_or_credential_blocks_before_start(self):
        """用途：拒绝用候选镜像或不完整机器凭据启动。
        参数：无。
        返回值：无；门禁失败后仍启动时失败。
        """
        for options in ({"pinned": False}, {"omit": "PROJECT_SPEND_CLIENT_SECRET"}):
            with self.subTest(options=options):
                result = self.start(**options)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("COMPOSE", result.stdout)
                self.assertNotIn("fixture-", result.stderr)

    def test_registered_application_restores_both_members_without_migration(self):
        """用途：核验仅恢复已接入数据库与双进程，不复制受控迁移流程。
        参数：无。
        返回值：无；缺进程、自动构建或执行迁移时失败。
        """
        result = self.start()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [line for line in result.stdout.splitlines() if line.startswith("COMPOSE")]
        self.assertEqual(len(calls), 2)
        self.assertTrue(calls[0].endswith("reimbursement-mysql"))
        self.assertTrue(calls[1].endswith("reimbursement-api reimbursement-worker"))
        for line in calls:
            self.assertIn("--no-build", line)
            self.assertFalse(set(line.split()) & {"run", "build", "migrate", "catalog-sync"})

    def test_database_failure_stops_before_api_and_worker(self):
        """用途：核验数据库未就绪时不会继续恢复双进程。
        参数：无。
        返回值：无；数据库失败被忽略时失败。
        """
        result = self.start(failing="reimbursement-mysql")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("reimbursement-api reimbursement-worker", result.stdout)

    def test_build_and_discovery_include_reimbursement(self):
        """用途：核验真实构建函数和探测映射包含报销。
        参数：无。
        返回值：无；构建或探测遗漏报销时失败。
        """
        for name in ("build_images", "build_candidate_images"):
            body = function(ENTRY, name)
            result = shell('''
force_build=true
prepare_base_images() { :; }
compose() { printf 'COMPOSE %s\\n' "$*"; }
compose_default_local_images() { :; }
''' + body + name + "\n")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("reimbursement-api", result.stdout)
        result = shell(function(ENTRY, "subsystem_discovery_service") + "subsystem_discovery_service reimbursement\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "reimbursement-api")

    def test_compose_scopes_database_keys_and_conditionally_enables_profile(self):
        """用途：核验报销专属键可用且不会用完整env污染其他系统插值。
        参数：无。
        返回值：无；配置或profile遗漏、未接入误启用时失败。
        """
        for ready, portal, project in product((False, True), repeat=3):
            with tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp) / "reimbursement.env"
                path.write_text("OIDC_TENANT_ID=reimbursement-tenant\n"
                                "REIMBURSEMENT_MYSQL_PASSWORD=fixture-password\n"
                                "REIMBURSEMENT_MYSQL_ROOT_PASSWORD=fixture-root\n")
                result = shell("source " + shlex.quote(str(ROOT / "scripts/lib/env-templates.sh")) + "\n" + '''
project_root="$TEMP"
compose_file=/dev/null
compose_project=fixture
env_file=/dev/null; contract_env_file=/dev/null; customer_env_file=/dev/null
portal_env_file=/dev/null; project_env_file=/dev/null; data_analysis_env_file=/dev/null
settlement_env_file=/dev/null; reimbursement_env_file="$TARGET"
data_analysis_configured() { return 1; }
portal_configured() { [[ "$PORTAL" == true ]]; }
project_configured() { [[ "$PROJECT" == true ]]; }
reimbursement_configured() { [[ "$READY" == true ]]; }
OIDC_TENANT_ID=existing-tenant
docker() {
    printf 'TENANT=%s\\n' "$OIDC_TENANT_ID"
    printf 'DB=%s|%s\\n' "${REIMBURSEMENT_MYSQL_PASSWORD:-}" "${REIMBURSEMENT_MYSQL_ROOT_PASSWORD:-}"
    printf '%s\\n' "$@"
}
''' + function(ROOT / "scripts/lib/compose.sh", "compose") + '''
compose config --quiet
printf 'AFTER=%s|%s\\n' "${REIMBURSEMENT_MYSQL_PASSWORD-unset}" "${REIMBURSEMENT_MYSQL_ROOT_PASSWORD-unset}"
''', TEMP=tmp, TARGET=str(path), READY=str(ready).lower(),
                    PORTAL=str(portal).lower(), PROJECT=str(project).lower())
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn(str(path), result.stdout.splitlines())
                self.assertIn("TENANT=existing-tenant", result.stdout.splitlines())
                self.assertIn("DB=fixture-password|fixture-root", result.stdout.splitlines())
                self.assertIn("AFTER=unset|unset", result.stdout.splitlines())
                self.assertEqual("reimbursement" in result.stdout.splitlines(), ready)
                self.assertEqual("portal" in result.stdout.splitlines(), portal)
                self.assertEqual("project" in result.stdout.splitlines(), project)

    def test_compose_dependencies_reference_real_service_names(self):
        """用途：核验报销依赖引用本地真实平台服务。
        参数：无。
        返回值：无；使用网络别名作为服务名时失败。
        """
        source = (ROOT / "compose.local.yaml").read_text()
        block = source.split("\n  reimbursement-api:\n", 1)[1].split("\n  reimbursement-worker:", 1)[0]
        depends = block.split("    depends_on:\n", 1)[1].split("    labels:\n", 1)[0]
        self.assertIn("      api:\n", depends)
        self.assertNotIn("      platform-api:\n", depends)


class ReimbursementOperationsTests(unittest.TestCase):
    """服务器运维入口的报销选择和非法目标拒绝。"""

    def test_reimbursement_mapping_and_validation_preserve_unknown_rejection(self):
        """用途：核验日志、修复与升级目标接受报销并拒绝未知值。
        参数：无。
        返回值：无；目标映射错误或扩大白名单时失败。
        """
        body = "die() { exit 7; }\n" + "".join(function(OPS, name) for name in (
            "validate_subsystem", "validate_repair_target", "validate_upgrade_component", "application_code"))
        for name in ("validate_subsystem", "validate_repair_target", "validate_upgrade_component"):
            for target, succeeds in (("reimbursement", True), ("reimbursement-unknown", False), ("", False)):
                result = shell(body + f"{name} {shlex.quote(target)}\n")
                self.assertEqual(result.returncode == 0, succeeds, result.stderr)
        result = shell(body + "application_code reimbursement\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "reimbursement")

    def test_menu_can_select_reimbursement(self):
        """用途：核验交互菜单真正返回报销子系统参数。
        参数：无。
        返回值：无；显示报销但不能选择时失败。
        """
        result = shell(function(OPS, "select_subsystem") + "select_subsystem <<< 7\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "reimbursement")
        self.assertIn("报销", result.stderr)


if __name__ == "__main__":
    unittest.main()
