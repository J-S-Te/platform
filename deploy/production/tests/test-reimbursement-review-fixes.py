#!/usr/bin/env python3
"""报销评审缺陷的纯本地脚本回归，不运行部署入口或外部服务。"""
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest

BIN = Path(__file__).resolve().parents[1] / "bin"


# 用途：提取真实脚本函数以隔离其顶层环境与部署副作用。
# 参数：source为源码，name为目标函数名称。
# 返回值：完整函数声明；函数缺失则测试报错。
def function(source, name):
    start = source.index(name + "() {\n")
    return source[start:source.index("\n}\n", start) + 3]


# 用途：执行仅含被测分支和受限记录器的本地脚本。
# 参数：body为固定测试脚本。
# 返回值：标准输出、错误和退出码，不访问业务运行配置。
def shell(body):
    return subprocess.run(["bash", "-c", "set -eu\n" + body],
                          text=True, capture_output=True, timeout=10)


class ReimbursementReviewFixes(unittest.TestCase):
    # 用途：验证两包并存及各单包都按所属组件准备且不会交叉导入。
    # 参数：无。
    # 返回值：无；索引冲突或禁用组件被准备时失败。
    def test_package_selection_keeps_components_separate(self):
        source = (BIN / "deploy-package.sh").read_text()
        for selected in ("reimbursement settlement data-analysis", "reimbursement", "settlement"):
            with self.subTest(selected=selected), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                for name in ("common-infrastructure", "platform-backend-qa", "frontend-qa",
                             "reimbursement-backend-qa", "settlement-backend-qa", "data-analysis-backend-qa"):
                    (root / (name + "-linux-amd64.tar.gz")).touch()
                script = '\n'.join(function(source, name) for name in
                                   ("discover_package", "package_index", "auto_install_packages"))
                script += f'\npackage_dir={shlex.quote(directory)}\ndeploy_dir="$package_dir/absent"\n'
                script += f'enabled="common platform frontend {selected}"\n'
                script += r'''
require_initialized() { :; }
scope_services() { :; }
scope_report() { :; }
scope_enabled() { [[ " $enabled " == *" $1 "* ]]; }
workflow_enabled() { return 1; }
temporal_consumers_enabled() { return 1; }
wait_for_required_service_health() { :; }
verify() { :; }
import_package() { imported="$(basename "$1")"; }
deploy_component() { :; }
prepare_subsystem() { printf 'PREPARED:%s:%s\n' "$1" "$imported"; }
die() { echo "$*" >&2; exit 7; }
auto_install_packages
'''
                result = shell(script)
                self.assertEqual(result.returncode, 0, result.stderr)
                prepared = [line for line in result.stdout.splitlines() if line.startswith("PREPARED:")]
                expected = [f"PREPARED:{component}:{component}-backend-qa-linux-amd64.tar.gz"
                            for component in selected.split()]
                self.assertEqual(prepared, expected)

    # 用途：验证未接入首次暂存、严格拒绝与已接入继续发布的真实分支。
    # 参数：无；所有发布指针和运行材料均在临时目录内合成。
    # 返回值：无；错误回滚、提前启动或把严格失败当成功时失败。
    def test_first_publish_staging_and_strict_mode(self):
        source = (BIN / "deploy-service.sh").read_text()
        block = source[source.index("# 首次上线时子系统镜像会先于浏览器接入发布。"):source.index('echo "开始发布 $service"')]
        helpers = ""
        if "reimbursement_runtime_ready() {" in source:
            helpers = function(source, "reimbursement_runtime_ready")
        keys = """MYSQL_DSN PUBLIC_BASE_URL PLATFORM_BASE_URL PLATFORM_ENVIRONMENT_CODE
OIDC_ISSUER OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_TENANT_ID OIDC_REDIRECT_URI
OIDC_SESSION_ENCRYPTION_KEY_BASE64 PROJECT_REFERENCE_CLIENT_ID PROJECT_REFERENCE_CLIENT_SECRET
PROJECT_SPEND_CLIENT_ID PROJECT_SPEND_CLIENT_SECRET FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET
PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET""".split()
        for mode, missing, expected_code in (("false", True, 0), ("true", True, 1),
                                             ("true", False, 42)):
            with self.subTest(mode=mode, missing=missing), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                release, previous, runtime = root / "release", root / "previous", root / "synthetic.env"
                release.write_text("candidate-digest\n")
                previous.write_text("old-digest\n")
                runtime.write_text("\n".join(f"{key}=fixture" for key in keys
                                            if not missing or key != "OIDC_CLIENT_SECRET") + "\n")
                variables = {"service": "reimbursement", "image_ref": "candidate-digest",
                             "fail_if_runtime_not_ready": mode, "release_file": str(release),
                             "previous_release": str(previous), "reimbursement_runtime_file": str(runtime)}
                script = "\n".join(f"{key}={shlex.quote(value)}" for key, value in variables.items())
                script += '\nrefresh_subsystem_provisioner_config() { :; }\n'
                result = shell(script + helpers + "\n" + block + "\nexit 42\n")
                self.assertEqual(result.returncode, expected_code, result.stdout + result.stderr)
                self.assertEqual(release.read_text(), "old-digest\n" if missing and mode == "true"
                                 else "candidate-digest\n")
                self.assertEqual(previous.exists(), not missing)


if __name__ == "__main__":
    unittest.main()
