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
    # 用途：验证基础设施准备仅显式模式调用且继承已有发布锁。
    # 参数：self为隔离测试实例，helper只记录固定参数。
    # 返回值：无；默认发布触发准备或缺少能力验证时失败。
    def test_infrastructure_prepare_requires_version_and_inherited_lock(self):
        source = (BIN / "deploy-service.sh").read_text()
        self.assertIn("prepare_reimbursement_infrastructure() {", source)
        helper = function(source, "prepare_reimbursement_infrastructure")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            script = root / "prepare-reimbursement-infrastructure.py"
            script.write_text("import sys\nfrom pathlib import Path\n"
                              "if sys.argv[1:] == ['--capabilities']:\n"
                              " print('REIMBURSEMENT_INFRASTRUCTURE_PREPARE_V1')\n"
                              "else: Path(__file__).with_suffix('.called').write_text(' '.join(sys.argv[1:]))\n")
            variables = f"script_dir={shlex.quote(str(root))}\ndeploy_dir={shlex.quote(str(root))}\n"
            result = shell(variables + helper + "\nprepare_reimbursement_infrastructure\n")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(script.with_suffix('.called').read_text(),
                             f"--deploy-root {root} --deployment-lock-fd 9")
            script.write_text("print('OLD_VERSION')\n")
            result = shell(variables + helper + "\nprepare_reimbursement_infrastructure\n")
            self.assertNotEqual(result.returncode, 0)
        backup = source.index('elif ! bash "$script_dir/backup-all.sh"')
        call = source.index("  prepare_reimbursement_infrastructure || exit 1")
        runtime = source.index('    prepare_runtime_file "$reimbursement_runtime_file"')
        self.assertLess(backup, call)
        self.assertLess(call, runtime)

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
                             "prepare_only": "false",
                             "fail_if_runtime_not_ready": mode, "release_file": str(release),
                             "previous_release": str(previous), "reimbursement_runtime_file": str(runtime)}
                script = "\n".join(f"{key}={shlex.quote(value)}" for key, value in variables.items())
                script += '\nrefresh_subsystem_provisioner_config() { :; }\n'
                result = shell(script + helpers + "\n" + block + "\nexit 42\n")
                self.assertEqual(result.returncode, expected_code, result.stdout + result.stderr)
                self.assertEqual(release.read_text(), "old-digest\n" if missing and mode == "true"
                                 else "candidate-digest\n")
                self.assertEqual(previous.exists(), not missing)

    # 用途：验证平台明确声明暂存能力，旧版本不能被包装器误当支持。
    # 参数：self为当前隔离测试实例。
    # 返回值：无；能力查询读取环境或执行发布时失败。
    def test_prepare_capability_is_readonly(self):
        result = subprocess.run(["bash", str(BIN / "deploy-service.sh"), "--capabilities"],
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "REIMBURSEMENT_PREPARE_ONLY_V1")

    # 用途：验证暂存开关严格布尔且不能扩散到其他业务系统。
    # 参数：self为当前隔离测试实例。
    # 返回值：无；非法值或其他服务开启暂存被接受时失败。
    def test_prepare_flag_is_strict_and_reimbursement_only(self):
        source = (BIN / "deploy-service.sh").read_text()
        prefix = source[:source.index("acr_enterprise_or_new_personal=")]
        for service, flag, expected in (("reimbursement", "true", 0),
                                        ("reimbursement", "false", 0),
                                        ("reimbursement", "invalid", 2),
                                        ("reimbursement", "", 2),
                                        ("project", "true", 2)):
            with self.subTest(service=service, flag=flag):
                script = "export DEPLOY_PREPARE_ONLY=" + shlex.quote(flag) + "\n" + prefix
                result = subprocess.run(["bash", "-c", script, "fixture", service, "candidate"],
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    # 用途：验证同一锁内暂存出口不受运行配置完成程度影响且永不进入业务发布。
    # 参数：self为当前隔离测试实例。
    # 返回值：无；运行材料刚完成的并发场景进入业务分支时失败。
    def test_prepare_only_never_enters_business_deploy(self):
        source = (BIN / "deploy-service.sh").read_text()
        block = source[source.index("# 首次上线时子系统镜像会先于浏览器接入发布。"):source.index('echo "开始发布 $service"')]
        for runtime_ready in ("true", "false"):
            with self.subTest(runtime_ready=runtime_ready), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                release, previous = root / "release", root / "previous"
                release.write_text("candidate-digest\n"); previous.write_text("old-digest\n")
                variables = {"service": "reimbursement", "prepare_only": "true",
                             "image_ref": "candidate-digest", "fail_if_runtime_not_ready": "true",
                             "release_file": str(release), "previous_release": str(previous)}
                script = "\n".join(f"{key}={shlex.quote(value)}" for key, value in variables.items())
                script += "\nreimbursement_runtime_ready() { " + runtime_ready + "; }\n"
                script += "refresh_subsystem_provisioner_config() { :; }\n" + block
                result = shell(script + "\necho ATTEMPTED_BUSINESS_START\nexit 42\n")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("PREPARED", result.stdout)
                self.assertNotIn("ATTEMPTED_BUSINESS_START", result.stdout)
                self.assertEqual(release.read_text(), "candidate-digest\n")
                self.assertFalse(previous.exists())


if __name__ == "__main__":
    unittest.main()
