"""验证 native 部署脚本共享库的抽取结果与防漂移约束。

用途：native 部署脚本（bootstrap-linux.sh / backend-only-linux.sh）把逐字相同的函数
抽取到 scripts/lib/native-deploy.sh 后，本用例保证共享库仍是纯函数定义、抽取过的函数
没有在入口脚本里重复出现，并保证升级保留相关的关键函数仍留在脚本内且两端逐字相同。
参数：无；全部用例只读脚本源码、只写临时目录，用 bash -c 执行真实函数文本。
返回值：无；抽取结果、函数语义或防漂移约束被破坏时用例失败。
"""
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = ROOT / "scripts"
BOOTSTRAP = SCRIPTS / "bootstrap-linux.sh"
BACKEND = SCRIPTS / "backend-only-linux.sh"
OPS = SCRIPTS / "ops-linux.sh"
LAN_ACCESS = SCRIPTS / "lan-access.sh"
NATIVE_LIB = SCRIPTS / "lib" / "native-deploy.sh"
ACCESS_LIB = SCRIPTS / "lib" / "access-mode.sh"

BASH = shutil.which("bash") or "/bin/bash"

# 从 bootstrap-linux.sh / backend-only-linux.sh 逐字抽取到 lib/native-deploy.sh 的函数。
EXTRACTED = [
    "run_cmd",
    "require_command",
    "mysql_version",
    "mysql_option_value",
    "mysql_defaults_file",
    "is_local_mysql_host",
    "valid_mysql_account_host",
    "initialize_database",
    "is_bootstrap_admin_placeholder",
    "load_bootstrap_admin_input",
    "run_bootstrap_admin_status",
    "run_bootstrap_admin_initialize",
    "source_revision",
]

# 测试 upgrade 配置保留的离线用例用正则从脚本文本里提取这几个函数；一旦移进库，CI 直接失败。
PRESERVED_INLINE = ["get_env_value", "set_env_value", "bootstrap_first_super_admin"]


def function(source, name):
    """用处：从脚本文本提取真实函数体，避免复制生产实现。
    参数：source 为脚本源码，name 为函数名。
    返回值：函数文本（与 deploy/production/tests/test-upgrade-config-preservation.py 相同的提取方式）。
    """
    found = re.search(r"^" + re.escape(name) + r"\(\) \{\n.*?^\}", source, re.M | re.S)
    assert found, f"missing function {name}"
    return found.group()


def has_function(source, name):
    """用途：判断脚本文本里是否仍定义该函数。
    参数：source 为脚本源码，name 为函数名。
    返回值：定义返回 True，否则 False。
    """
    return re.search(r"^" + re.escape(name) + r"\(\) \{", source, re.M) is not None


def run_bash(setup, body, *args, env=None, timeout=30):
    """用途：在独立 bash 进程中执行真实函数文本与调用步骤。
    参数：setup 为变量/替身定义，body 为调用步骤，args 透传为 $1..$n，env 覆盖环境变量。
    返回值：subprocess.CompletedProcess；不启动 Docker、数据库或网络请求。
    """
    script = "set -Eeuo pipefail\n" + setup + "\n" + body + "\n"
    return subprocess.run(
        [BASH, "-c", script, "fixture", *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        env=env,
    )


def lib_functions(*names):
    """用途：拼接 lib/native-deploy.sh 中的指定函数文本。
    参数：names 为函数名。
    返回值：可直接被 bash 加载的函数定义文本。
    """
    source = NATIVE_LIB.read_text(encoding="utf-8")
    return "\n".join(function(source, name) for name in names)


class NativeDeployLibraryTest(unittest.TestCase):
    """共享库的静态约束：纯函数定义、无重复实现、两侧都已加载。"""

    def test_library_has_no_top_level_side_effects(self):
        """库只能有注释和函数定义，禁止顶层执行语句，保证 source 时零副作用。"""
        definition = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\(\) \{$")
        depth = 0
        for number, line in enumerate(NATIVE_LIB.read_text(encoding="utf-8").splitlines(), 1):
            stripped = line.strip()
            if depth == 0 and stripped and not stripped.startswith("#"):
                self.assertRegex(
                    stripped,
                    definition,
                    f"lib/native-deploy.sh:{number} 顶层存在非定义语句：{stripped}",
                )
            depth += line.count("{") - line.count("}")
        self.assertEqual(0, depth, "lib/native-deploy.sh 花括号不平衡")

    def test_library_defines_every_extracted_function(self):
        """抽取清单里的函数必须都在库里，避免删除后调用点直接缺函数。"""
        source = NATIVE_LIB.read_text(encoding="utf-8")
        missing = [name for name in EXTRACTED if not has_function(source, name)]
        self.assertEqual([], missing)

    def test_extracted_functions_are_not_redefined_in_entry_scripts(self):
        """防止去重回退：同一函数不得同时留在库和入口脚本里。"""
        for script in (BOOTSTRAP, BACKEND):
            source = script.read_text(encoding="utf-8")
            duplicated = [name for name in EXTRACTED if has_function(source, name)]
            self.assertEqual([], duplicated, f"{script.name} 仍重复定义抽取过的函数")

    def test_both_entry_scripts_source_the_library(self):
        """两个入口脚本都必须用 BASH_SOURCE 推导的目录加载共享库。"""
        for script in (BOOTSTRAP, BACKEND):
            source = script.read_text(encoding="utf-8")
            self.assertIn('# shellcheck source=lib/native-deploy.sh', source)
            self.assertIn('source "${SCRIPT_DIR}/lib/native-deploy.sh"', source)


class NativeDeployFunctionTest(unittest.TestCase):
    """共享库纯函数的真实行为：只调用脚本里的函数文本，不复制实现。"""

    def test_mysql_option_value_escapes_backslash_and_quote(self):
        """mysql 选项值必须按反斜杠、双引号顺序转义，否则凭据文件会被截断。"""
        setup = lib_functions("mysql_option_value")
        cases = {
            "plain": '"plain"',
            'has"quote': '"has\\"quote"',
            "back\\slash": '"back\\\\slash"',
            "": '""',
        }
        for value, expected in cases.items():
            with self.subTest(value=value):
                result = run_bash(setup, 'mysql_option_value "$1"', value)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertEqual(expected, result.stdout)

    def test_mysql_defaults_file_writes_protected_client_section(self):
        """凭据文件必须写成 0600 的 [client] 段，并省略空的 password/host/port。"""
        setup = lib_functions("mysql_option_value", "mysql_defaults_file")
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "client.cnf"
            result = run_bash(
                setup,
                'mysql_defaults_file "$1" app "p\\"w" 10.0.0.9 3307 && cat "$1" && stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"',
                str(target),
            )
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertIn("[client]", result.stdout)
            self.assertIn('user="app"', result.stdout)
            self.assertIn('password="p\\"w"', result.stdout)
            self.assertIn('host="10.0.0.9"', result.stdout)
            self.assertIn("protocol=TCP", result.stdout)
            self.assertIn("port=3307", result.stdout)
            self.assertTrue(result.stdout.rstrip().endswith("600"))

    def test_mysql_defaults_file_omits_empty_optionals(self):
        """没有 password/host/port 时不得写出空字段，否则 mysql 客户端会报错。"""
        setup = lib_functions("mysql_option_value", "mysql_defaults_file")
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "client.cnf"
            result = run_bash(setup, 'mysql_defaults_file "$1" app "" && cat "$1"', str(target))
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertNotIn("password", result.stdout)
            self.assertNotIn("host", result.stdout)
            self.assertNotIn("port=", result.stdout)

    def test_is_local_mysql_host_matches_only_loopback(self):
        """只有 127.0.0.1、localhost、::1 可走本机 socket 认证路径。"""
        setup = lib_functions("is_local_mysql_host")
        body = 'if is_local_mysql_host "$1"; then echo local; else echo remote; fi'
        for host, expected in [
            ("127.0.0.1", "local"),
            ("localhost", "local"),
            ("::1", "local"),
            ("10.0.0.9", "remote"),
            ("db.internal", "remote"),
            ("", "remote"),
        ]:
            with self.subTest(host=host):
                result = run_bash(setup, body, host)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertEqual(expected, result.stdout.strip())

    def test_valid_mysql_account_host_rejects_unsafe_characters(self):
        """账号主机值参与 SQL 拼接，只允许字母数字和 ._:%- 字符。"""
        setup = lib_functions("valid_mysql_account_host")
        body = 'if valid_mysql_account_host "$1"; then echo ok; else echo reject; fi'
        for value, expected in [
            ("127.0.0.1", "ok"),
            ("db.internal", "ok"),
            ("%", "ok"),
            ("192.168.1.%", "ok"),
            ("::1", "ok"),
            ("10.0.0.1' OR 1=1 --", "reject"),
            ("host name", "reject"),
            ("path/to/host", "reject"),
            ("", "reject"),
        ]:
            with self.subTest(value=value):
                result = run_bash(setup, body, value)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertEqual(expected, result.stdout.strip())

    def test_mysql_version_parses_client_version(self):
        """mysql_version 只解析 mysql --version 输出中的版本号，不连接数据库。"""
        setup = lib_functions("mysql_version")
        with tempfile.TemporaryDirectory() as tmp:
            bin_dir = Path(tmp)
            fake = bin_dir / "mysql"
            fake.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = "--version" ]; then\n'
                "  echo 'mysql  Ver 8.0.36 for Linux on x86_64 (MySQL Community Server - GPL)'\n"
                "  exit 0\n"
                "fi\n"
                "exit 1\n",
                encoding="utf-8",
            )
            fake.chmod(0o700)
            env = os.environ.copy()
            env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"
            result = run_bash(setup, "mysql_version", env=env)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertEqual("8.0.36", result.stdout.strip())

    def test_mysql_version_fails_without_client(self):
        """没有 mysql 客户端时返回非零，调用方据此提示安装依赖。"""
        setup = lib_functions("mysql_version")
        with tempfile.TemporaryDirectory() as tmp:
            env = os.environ.copy()
            env["PATH"] = tmp
            result = run_bash(setup, "mysql_version", env=env)
            self.assertNotEqual(0, result.returncode)

    def test_require_command_reports_missing_command(self):
        """缺命令时记录错误并返回非零，不直接终止调用者。"""
        setup = "log() { echo \"$*\"; }\n" + lib_functions("require_command")
        ok = run_bash(setup, 'require_command bash')
        self.assertEqual(0, ok.returncode, ok.stderr)
        missing = run_bash(setup, 'require_command definitely-not-a-command-fixture')
        self.assertEqual(1, missing.returncode, missing.stderr)
        self.assertIn("缺少命令", missing.stdout)

    def test_run_cmd_honours_dry_run_and_verbose_redirect(self):
        """run_cmd 必须只在非 dry-run 时执行命令，并把输出写入日志文件。"""
        setup = (
            'log_command() { echo "CMD:$*"; }\n'
            "DRY_RUN=false\n"
            "VERBOSE=false\n"
            + lib_functions("run_cmd")
        )
        with tempfile.TemporaryDirectory() as tmp:
            marker = Path(tmp) / "marker"
            log_file = Path(tmp) / "run.log"
            dry = run_bash(
                setup.replace("DRY_RUN=false", "DRY_RUN=true"), 'run_cmd touch "$1"', str(marker)
            )
            self.assertEqual(0, dry.returncode, dry.stderr)
            self.assertFalse(marker.exists(), "dry-run 不得执行命令")
            self.assertIn("CMD:touch", dry.stdout)

            real = run_bash(
                "LOG_FILE=" + str(log_file) + "\n" + setup,
                'run_cmd sh -c "echo executed"',
            )
            self.assertEqual(0, real.returncode, real.stderr)
            self.assertIn("executed", log_file.read_text(encoding="utf-8"))

    def test_source_revision_uses_git_short_hash(self):
        """存在 git 仓库时输出版本号，否则回退为 source，避免部署字段为空。"""
        setup = lib_functions("source_revision")
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            env = os.environ.copy()
            env.update(
                GIT_AUTHOR_NAME="fixture",
                GIT_AUTHOR_EMAIL="fixture@example.invalid",
                GIT_COMMITTER_NAME="fixture",
                GIT_COMMITTER_EMAIL="fixture@example.invalid",
            )
            for command in (
                ["git", "init", "-q", "."],
                ["git", "add", "-A"],
                ["git", "commit", "-q", "--allow-empty", "-m", "fixture"],
            ):
                subprocess.run(command, cwd=str(repo), check=True, capture_output=True, env=env)
            revision = subprocess.run(
                ["git", "rev-parse", "--short=12", "HEAD"],
                cwd=str(repo),
                check=True,
                capture_output=True,
                text=True,
                env=env,
            ).stdout.strip()
            result = run_bash('PROJECT_ROOT="$1"\n' + setup, "source_revision", str(repo), env=env)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertEqual(revision, result.stdout.strip())

    def test_source_revision_falls_back_without_git(self):
        """没有 git 命令时必须回退为 source，而不是返回空值。"""
        setup = lib_functions("source_revision")
        with tempfile.TemporaryDirectory() as tmp:
            env = os.environ.copy()
            env["PATH"] = tmp
            result = run_bash('PROJECT_ROOT="$1"\n' + setup, "source_revision", tmp, env=env)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertEqual("source", result.stdout.strip())


class UpgradePreservationDriftTest(unittest.TestCase):
    """升级保留用例依赖的函数必须留在脚本内，且两端逐字相同。"""

    def test_preservation_functions_stay_inline_in_both_scripts(self):
        """get_env_value/set_env_value/bootstrap_first_super_admin 不能移进共享库。"""
        lib_source = NATIVE_LIB.read_text(encoding="utf-8")
        for name in PRESERVED_INLINE:
            with self.subTest(function=name):
                self.assertFalse(has_function(lib_source, name), f"{name} 不应移入共享库")
                for script in (BOOTSTRAP, BACKEND):
                    self.assertTrue(
                        has_function(script.read_text(encoding="utf-8"), name),
                        f"{script.name} 缺少离线升级用例依赖的 {name}",
                    )

    def test_preservation_functions_are_identical_across_linux_scripts(self):
        """bootstrap_first_super_admin 两端逐字相同，单边修改即失败。"""
        bootstrap = BOOTSTRAP.read_text(encoding="utf-8")
        backend = BACKEND.read_text(encoding="utf-8")
        self.assertEqual(
            function(bootstrap, "bootstrap_first_super_admin"),
            function(backend, "bootstrap_first_super_admin"),
        )

    def test_shared_env_helpers_keep_same_observable_behaviour(self):
        """get_env_value/set_env_value 两端本就存在差异（bootstrap 支持 ENV_EXAMPLE 回退与
        dry-run 提示），不能断言逐字相同；改为锁定二者共有的读写语义，单边改坏即失败。
        """
        bootstrap = BOOTSTRAP.read_text(encoding="utf-8")
        backend = BACKEND.read_text(encoding="utf-8")

        read_body = 'get_env_value MYSQL_PASSWORD'
        with tempfile.TemporaryDirectory() as tmp:
            env_file = Path(tmp) / ".env"
            env_file.write_text("MYSQL_PASSWORD=fixture-value\nOTHER=kept\n", encoding="utf-8")
            results = []
            for source in (bootstrap, backend):
                setup = 'ENV_FILE="$1"; log() { :; }\n' + function(source, "get_env_value")
                results.append(run_bash(setup, read_body, str(env_file)))
            self.assertEqual(0, results[0].returncode, results[0].stderr)
            self.assertEqual(0, results[1].returncode, results[1].stderr)
            self.assertEqual(results[0].stdout, results[1].stdout)
            self.assertEqual("fixture-value", results[0].stdout.strip())

        with tempfile.TemporaryDirectory() as tmp:
            outputs = []
            for index, source in enumerate((bootstrap, backend)):
                env_file = Path(tmp) / f".env.{index}"
                env_file.write_text("MYSQL_PASSWORD=old\nOTHER=kept\n", encoding="utf-8")
                setup = (
                    'ENV_FILE="$1"; DRY_RUN=false; TEMP_PATHS=(); log() { :; }\n'
                    + function(source, "set_env_value")
                )
                result = run_bash(setup, "set_env_value MYSQL_PASSWORD new-value", str(env_file))
                self.assertEqual(0, result.returncode, result.stderr)
                outputs.append(env_file.read_text(encoding="utf-8"))
            self.assertEqual(outputs[0], outputs[1])
            self.assertEqual("MYSQL_PASSWORD=new-value\nOTHER=kept\n", outputs[0])


class LinuxScriptHelpSmokeTest(unittest.TestCase):
    """入口脚本冒烟：只跑帮助入口，不执行任何变更。"""

    def _run_help(self, script):
        env = os.environ.copy()
        with tempfile.TemporaryDirectory() as tmp:
            env["BASIC_PLATFORM_BOOTSTRAP_LOG"] = str(Path(tmp) / "bootstrap.log")
            env["BASIC_PLATFORM_BACKEND_LOG"] = str(Path(tmp) / "backend.log")
            env["BASIC_PLATFORM_OPS_LOG_DIRECTORY"] = tmp
            return subprocess.run(
                [BASH, str(script), "--help"], capture_output=True, text=True, timeout=60, env=env
            )

    def test_help_prints_usage_and_exits_zero(self):
        """三个 Linux 入口的 --help 都必须退出 0 且输出用法。"""
        for script in (BOOTSTRAP, BACKEND, OPS):
            with self.subTest(script=script.name):
                result = self._run_help(script)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertTrue((result.stdout + result.stderr).strip(), "帮助输出为空")
                self.assertIn("用法", result.stdout + result.stderr)


class LanAccessDedupTest(unittest.TestCase):
    """lan-access.sh 与 lib/access-mode.sh 的 IPv4 语义必须同源。"""

    def test_is_ipv4_semantics_from_library(self):
        """is_ipv4 只接受四段 0-255 的十进制点分地址。"""
        setup = function(ACCESS_LIB.read_text(encoding="utf-8"), "is_ipv4")
        body = 'if is_ipv4 "$1"; then echo ok; else echo reject; fi'
        for value, expected in [
            ("192.168.1.25", "ok"),
            ("127.0.0.1", "ok"),
            ("0.0.0.0", "ok"),
            ("255.255.255.255", "ok"),
            ("256.168.1.25", "reject"),
            ("192.168.1", "reject"),
            ("192.168.1.25.9", "reject"),
            ("192.168.01.25", "ok"),
            ("", "reject"),
            ("localhost", "reject"),
            ("192.168.1.2x", "reject"),
        ]:
            with self.subTest(value=value):
                result = run_bash(setup, body, value)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertEqual(expected, result.stdout.strip())

    def test_lan_access_delegates_detection_to_access_mode_library(self):
        """lan-access 不得再自带 is_ipv4/detect_lan_address 副本。"""
        source = LAN_ACCESS.read_text(encoding="utf-8")
        for name in ("is_ipv4", "detect_lan_address"):
            self.assertFalse(has_function(source, name), f"lan-access.sh 仍自带 {name} 副本")
        self.assertIn('# shellcheck source=lib/access-mode.sh', source)
        self.assertIn('source "${script_dir}/lib/access-mode.sh"', source)
        self.assertIn("detect_current_lan_ipv4", source)
        self.assertTrue(has_function(ACCESS_LIB.read_text(encoding="utf-8"), "detect_current_lan_ipv4"))


if __name__ == "__main__":
    unittest.main()
