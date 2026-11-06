#!/usr/bin/env python3
"""验证 platform/scripts/ops-linux.sh 的破坏性路径不会误删、误切换或留下半成品。

被测对象是脚本里的真实函数文本：每个用例按函数名从 ops-linux.sh 抽取函数，在最小
harness（临时目录 + PATH 假命令 + 同名空实现替换身份/网络边界）里执行，不运行 main，
不接触 systemd、Nginx、MySQL、真实 /etc、/opt、/var，也不修改被测脚本。

只能在 Linux/GNU coreutils 语义下运行的用例（中缀模板 mktemp、readlink -m、mv -T）
在 BSD 主机上跳过，并在报告里说明；跳过原因写进 skipTest 文案。
"""
from pathlib import Path
import hashlib
import os
import platform
import re
import shutil
import stat
import subprocess
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "ops-linux.sh"

# 夹具口令只用于证明“不出现在 argv/stdout/stderr/日志”，不是任何真实凭据。
FIXTURE_PASSWORD = "fixture-password-must-not-leak"
FIXTURE_SECRET = "fixture-secret-must-not-leak"

COREUTILS_CANDIDATES = (
    "/opt/homebrew/opt/coreutils/libexec/gnubin",
    "/usr/local/opt/coreutils/libexec/gnubin",
)

# harness 全局：只补齐脚本依赖的状态，路径全部落在临时沙箱内。
# 不安装 on_error 的 ERR trap，让用例直接读到真实退出码。
PRELUDE = r'''
set -Eeuo pipefail
SANDBOX="$1"
DEPLOY_ROOT="${DEPLOY_ROOT:-${SANDBOX}/deploy}"
BACKUP_DIR="${BACKUP_DIR:-${SANDBOX}/backup}"
ENV_FILE="${ENV_FILE:-${SANDBOX}/basic-platform.env}"
LOG_FILE="${LOG_FILE:-}"
LOG_DIRECTORY="${LOG_DIRECTORY:-${SANDBOX}/logs}"
BOOTSTRAP_SCRIPT="${BOOTSTRAP_SCRIPT:-${SANDBOX}/bootstrap.sh}"
CURRENT_STEP="隔离回归"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080}"
API_SERVICE="basic-platform-api.service"
WORKER_SERVICE="basic-platform-worker.service"
RUN_USER="basic-platform"
LOG_LINES=100
FOLLOW_LOGS=false
ASSUME_YES="${ASSUME_YES:-true}"
RELEASE_ID="${RELEASE_ID:-}"
ROLLBACK_TARGET="${ROLLBACK_TARGET:-}"
RETAIN_DAYS="${RETAIN_DAYS:-}"
SKIP_CODE_VERIFY=false
'''

# 身份边界：真实 require_root 只对 EUID 0 放行，测试以普通用户运行，
# 因此用同名空实现替换；root 门禁本身由 RootGateTest 单独验证。
ROOT_BOUNDARY = r'''
require_root() { :; }
'''

# 平台边界：require_linux 依赖真实 uname，用例只验证设置校验。
LINUX_BOUNDARY = r'''
require_linux() { :; }
'''

# 网络边界：真实 wait_for_ready 通过 curl 打真实健康端点，隔离环境无法断言，
# 用可按 READY_MODE 控制返回值的同名桩替代。
READY_BOUNDARY = r'''
READY_COUNTER="${SANDBOX}/ready-calls"
wait_for_ready() {
    local calls
    calls=$(cat "${READY_COUNTER}" 2>/dev/null || printf '0')
    calls=$((calls + 1))
    printf '%s' "$calls" >"${READY_COUNTER}"
    if [[ "${READY_MODE:-ok}" == "fail" ]]; then
        return 1
    fi
    if [[ "${READY_MODE:-ok}" == "fail-then-ok" && "$calls" -eq 1 ]]; then
        return 1
    fi
    return 0
}
'''

ROLLBACK_FUNCTIONS = [
    "log", "fatal", "confirm", "require_command", "validate_release_id",
    "current_release_path", "require_current_release", "replace_current_link",
    "command_rollback",
]

INITIALIZE_FUNCTIONS = [
    "log", "fatal", "validate_service_name", "validate_absolute_path",
    "canonical_path", "reject_dangerous_directory", "initialize_log",
    "initialize_settings",
]

BACKUP_DATABASE_FUNCTIONS = [
    "log", "fatal", "require_command", "require_env_file", "read_env_value",
    "require_nonempty_env_value", "backup_database",
]

BACKUP_UPLOADS_FUNCTIONS = [
    "log", "fatal", "require_command", "require_env_file", "read_env_value",
    "require_nonempty_env_value", "canonical_path", "reject_dangerous_directory",
    "validate_backup_dir_layout", "backup_uploads",
]

PRUNE_FUNCTIONS = [
    "log", "fatal", "confirm", "canonical_path", "require_env_file", "read_env_value",
    "require_nonempty_env_value", "validate_backup_dir_layout", "command_prune_backups",
]


def function(name, optional=False):
    """用途：抽取待测脚本里的真实函数文本，避免在测试中复制生产实现。
    参数：name 为函数名；optional 为 True 时允许该函数已被删除。
    返回值：函数源码字符串；必需函数缺失时断言失败。
    """
    source = SCRIPT.read_text(encoding="utf-8")
    found = re.search(r"^" + re.escape(name) + r"\(\) \{\n.*?^\}", source, re.M | re.S)
    if not found:
        if optional:
            return ""
        raise AssertionError(f"scripts/ops-linux.sh 缺少函数 {name}；测试需同步更新")
    return found.group() + "\n"


def gnu_coreutils_directory():
    """用途：在 macOS 等非 GNU 主机上定位提供 readlink -m、mv -T、中缀模板 mktemp 的目录。
    参数：无。
    返回值：GNU coreutils 目录 Path；主机自带 GNU 工具时返回 None。
    """
    for candidate in COREUTILS_CANDIDATES:
        directory = Path(candidate)
        if all((directory / name).exists() for name in ("readlink", "mv", "mktemp", "sha256sum")):
            return directory
    return None


def real_path():
    """用途：构造只含真实命令的搜索路径，供包装器转交真实实现。
    参数：无。
    返回值：PATH 字符串；非 GNU 主机优先使用 brew 安装的 coreutils。
    """
    directories = []
    gnu = gnu_coreutils_directory()
    if gnu is not None:
        directories.append(str(gnu))
    directories.extend(["/usr/bin", "/bin", "/usr/sbin", "/sbin"])
    return os.pathsep.join(directories)


def gnu_semantics_available(path):
    """用途：确认 PATH 上的 readlink、mv、mktemp 具备脚本依赖的 GNU 语义。
    参数：path 为待探测的 PATH 字符串。
    返回值：True 表示可以复现 Linux 行为；False 表示本主机只能跳过相关用例。
    """
    probe = (
        "set -e\n"
        "readlink -m -- / >/dev/null\n"
        "mv --version | grep -q 'GNU coreutils'\n"
        "mktemp --version | grep -q 'GNU coreutils'\n"
        "command -v sha256sum >/dev/null\n"
        "command -v gzip >/dev/null\n"
        "command -v tar >/dev/null\n"
    )
    completed = subprocess.run(["bash", "-c", probe], env={"PATH": path},
                               text=True, capture_output=True)
    return completed.returncode == 0


def operand_path(argument):
    """用途：从 `--file=/path` 形式的参数里取出路径部分，供沙箱边界断言使用。
    参数：argument 为单个命令行参数。
    返回值：去掉选项前缀后的参数文本。
    """
    if argument.startswith("-") and "=" in argument:
        return argument.split("=", 1)[1]
    return argument


def files_under(directory):
    """用途：列出目录下所有普通文件的相对路径，用于零副作用断言。
    参数：directory 为待扫描目录。
    返回值：相对路径字符串集合；目录不存在时返回空集合。
    """
    directory = Path(directory)
    if not directory.exists():
        return set()
    return {str(path.relative_to(directory)) for path in directory.rglob("*") if path.is_file()}


def tree_snapshot(directory):
    """用途：记录目录树的条目名与文件大小，证明被拒绝的操作没有副作用。
    参数：directory 为待快照目录。
    返回值：{相对路径: 大小或 'dir'} 字典；目录不存在时返回空字典。
    """
    directory = Path(directory)
    if not directory.exists():
        return {}
    snapshot = {}
    for path in sorted(directory.rglob("*")):
        snapshot[str(path.relative_to(directory))] = path.stat().st_size if path.is_file() else "dir"
    return snapshot


def age_file(path, days):
    """用途：把文件修改时间推到 days 天前，用于构造 `find -mtime +N` 的过期备份。
    参数：path 为目标文件；days 为天数。
    返回值：无。
    """
    stamp = time.time() - days * 86400
    os.utime(path, (stamp, stamp))


class OpsSandboxTestCase(unittest.TestCase):
    """用途：提供隔离沙箱、假命令目录和“只执行真实函数”的执行入口。"""

    def setUp(self):
        """用途：建立临时沙箱与假命令目录。
        参数：无。
        返回值：无；清理通过 addCleanup 注册。
        """
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.sandbox = Path(self.tmp.name).resolve()
        self.bin = self.sandbox / "bin"
        self.bin.mkdir()
        self.deploy = self.sandbox / "deploy"
        self.backup = self.sandbox / "backup"
        self.env_file = self.sandbox / "basic-platform.env"
        self.call_log = self.sandbox / "commands.log"
        self.base_path = real_path()
        (self.sandbox / "home").mkdir()
        self._gnu = None

    def require_gnu(self):
        """用途：缺少 GNU 语义时跳过用例，避免 BSD 语义被误当成 Linux 回归。
        参数：无。
        返回值：无；无法复现时调用 skipTest。
        """
        if self._gnu is None:
            self._gnu = gnu_semantics_available(self.base_path)
        if not self._gnu:
            self.skipTest(
                "当前主机缺少 GNU coreutils（readlink -m / mv -T / 中缀模板 mktemp / sha256sum），"
                "无法复现脚本的 Linux 行为；请用报告中的 docker 命令在 Linux 下复验")

    def require_linux_host(self):
        """用途：脚本只支持 Linux；在 BSD 主机上 canonical_path 会把 /var 改写成 /private/var。
        参数：无。
        返回值：无；非 Linux 主机跳过依赖该前提的用例。
        """
        if platform.system() != "Linux":
            self.skipTest(
                f"脚本仅支持 Linux，{platform.system()} 上 readlink -m 会改写系统目录前缀，"
                "无法复现拒绝顺序；请用报告中的 docker 命令在 Linux 下复验")

    def wrap(self, *names):
        """用途：在临时 bin 里生成记录 argv 后转交真实命令的包装器，用于证明作用范围。
        参数：names 为需要审计的真实命令名。
        返回值：无；真实命令缺失时跳过用例。
        """
        for name in names:
            resolved = shutil.which(name, path=self.base_path)
            if resolved is None:
                self.skipTest(f"当前主机缺少真实命令 {name}，无法审计其作用范围")
            path = self.bin / name
            path.write_text(
                "#!/bin/sh\n"
                "{\n"
                "    printf '%s' \"" + name + "\"\n"
                "    for argument in \"$@\"; do printf '\\t%s' \"$argument\"; done\n"
                "    printf '\\n'\n"
                '} >>"${OPS_CALL_LOG}"\n'
                'exec "' + resolved + '" "$@"\n',
                encoding="utf-8")
            path.chmod(0o755)

    def fake(self, name, body):
        """用途：在临时 bin 里生成假命令。
        参数：name 为命令名；body 为 sh 语句文本。
        返回值：无。
        """
        path = self.bin / name
        path.write_text("#!/bin/sh\nset -u\n" + body, encoding="utf-8")
        path.chmod(0o755)

    @staticmethod
    def record_snippet(name):
        """用途：生成把 argv 以制表符分隔写入调用日志的 sh 片段，供假命令复用。
        参数：name 为记录用的命令名。
        返回值：sh 语句文本。
        """
        return ("{\n"
                f'    printf \'%s\' "{name}"\n'
                "    for argument in \"$@\"; do printf '\\t%s' \"$argument\"; done\n"
                "    printf '\\n'\n"
                '} >>"${OPS_CALL_LOG}"\n')

    def recorded_calls(self):
        """用途：读取包装器与假命令记录的调用。
        参数：无。
        返回值：[[命令名, 参数, ...], ...]；没有调用时返回空列表。
        """
        if not self.call_log.exists():
            return []
        calls = []
        for line in self.call_log.read_text(encoding="utf-8").splitlines():
            if line.strip():
                calls.append(line.split("\t"))
        return calls

    def commands_named(self, name):
        """用途：筛选某个命令的调用记录。
        参数：name 为命令名。
        返回值：该命令的调用列表。
        """
        return [call for call in self.recorded_calls() if call[0] == name]

    def assert_only_sandbox_paths(self):
        """用途：证明假命令与包装器看到的绝对路径都在临时沙箱内。
        参数：无。
        返回值：无；出现沙箱外绝对路径时失败。
        """
        boundary = str(self.sandbox)
        for call in self.recorded_calls():
            for argument in call[1:]:
                candidate = operand_path(argument)
                if not candidate.startswith("/"):
                    continue
                self.assertTrue(candidate == boundary or candidate.startswith(boundary + "/"),
                                f"命令 {call[0]} 触及沙箱外路径：{candidate}")

    def environment(self, **overrides):
        """用途：构造子进程环境，不继承宿主配置以免误连真实服务。
        参数：overrides 为额外环境变量。
        返回值：环境变量字典。
        """
        environment = {
            "PATH": os.pathsep.join([str(self.bin), self.base_path]),
            "HOME": str(self.sandbox / "home"),
            "LC_ALL": "C",
            "OPS_CALL_LOG": str(self.call_log),
            "SANDBOX": str(self.sandbox),
        }
        environment.update(overrides)
        return environment

    def run_functions(self, names, body, boundaries="", **environment):
        """用途：只执行抽取出的真实函数，不进入 main，也不触发真实系统服务。
        参数：names 为函数名序列；body 为调用步骤；boundaries 为同名边界桩；environment 为环境变量覆盖。
        返回值：CompletedProcess；stdout/stderr 为脚本真实输出。
        """
        script = PRELUDE + boundaries
        script += "".join(function(name) for name in names)
        script += "\n" + body.rstrip() + "\n"
        return subprocess.run(["bash", "-c", script, "ops-fixture", str(self.sandbox)],
                              text=True, capture_output=True, timeout=120,
                              env=self.environment(**environment))

    def write_env(self, **values):
        """用途：写入被测脚本读取的环境文件。
        参数：values 为 KEY=VALUE；值为字符串，可带引号。
        返回值：环境文件路径。
        """
        text = "".join(f"{key}={value}\n" for key, value in values.items())
        self.env_file.write_text(text, encoding="utf-8")
        return self.env_file

    def tracked_files(self):
        """用途：列出参与零副作用断言的文件，排除 harness 自身的假命令与日志文件。
        参数：无。
        返回值：沙箱内相对路径集合。
        """
        ignored_prefixes = ("bin/", "home/", "logs/")
        ignored_names = {"commands.log", "basic-platform.env"}
        return {path for path in files_under(self.sandbox)
                if not path.startswith(ignored_prefixes) and path not in ignored_names}

    def make_release(self, name, missing=(), mode=0o755):
        """用途：在 releases 下构造带 bin/api、bin/worker、bin/migrate 的发布目录。
        参数：name 为版本目录名；missing 为故意缺失的二进制；mode 为二进制权限。
        返回值：发布目录 Path。
        """
        release = self.deploy / "releases" / name
        (release / "bin").mkdir(parents=True, exist_ok=True)
        (release / "release-info").write_text(f"release={name}\n", encoding="utf-8")
        for binary in ("api", "worker", "migrate"):
            if binary in missing:
                continue
            path = release / "bin" / binary
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(mode)
        return release

    def current_target(self):
        """用途：读取 current 符号链接当前解析到的发布目录。
        参数：无。
        返回值：解析后的 Path；链接缺失或不是符号链接时返回 None。
        """
        link = self.deploy / "current"
        if not link.is_symlink():
            return None
        return link.resolve()

    def assert_current_release(self, name):
        """用途：断言 current 仍指向指定发布目录，用于证明失败路径没有切换指针。
        参数：name 为预期版本目录名。
        返回值：无；指针变化时失败。
        """
        expected = (self.deploy / "releases" / name).resolve()
        self.assertEqual(self.current_target(), expected,
                         f"current 指针偏离预期发布版本 {name}")


class BackupSafetyTest(OpsSandboxTestCase):
    """用途：回归备份失败不留半成品，也不触碰真实备份目录或泄漏口令。"""

    def setUp(self):
        """用途：准备 GNU 语义、备份子目录与审计包装器。
        参数：无。
        返回值：无。
        """
        super().setUp()
        self.require_gnu()

    def fake_mysqldump(self, exit_code):
        """用途：生成假 mysqldump，记录 argv 与口令传递方式，并按需失败。
        参数：exit_code 为假命令退出码。
        返回值：无。
        """
        self.fake("mysqldump",
                  self.record_snippet("mysqldump") +
                  'if [ -n "${MYSQL_PWD:-}" ]; then\n'
                  '    printf \'mysql-pwd-via-env\\n\' >>"${OPS_CALL_LOG}"\n'
                  'else\n'
                  '    printf \'mysql-pwd-missing\\n\' >>"${OPS_CALL_LOG}"\n'
                  'fi\n'
                  'printf \'INSERT INTO fixture VALUES (1);\\n\'\n'
                  f'exit {exit_code}\n')

    def write_mysql_env(self):
        """用途：写入备份所需的最小环境文件。
        参数：无。
        返回值：环境文件路径。
        """
        return self.write_env(MYSQL_HOST="127.0.0.1", MYSQL_PORT="3306",
                              MYSQL_DATABASE="fixture_db", MYSQL_USERNAME="fixture_user",
                              MYSQL_PASSWORD=FIXTURE_PASSWORD)

    def test_backup_database_failure_leaves_no_partial_file(self):
        """用途：假 mysqldump 失败时，备份目录不得留下半截或空的 .sql.gz。
        参数：无；使用假 mysqldump（退出码 1）与真实 gzip。
        返回值：无；出现残留文件、错误不可见或越过沙箱时失败。
        """
        self.write_mysql_env()
        self.fake_mysqldump(1)
        self.wrap("gzip", "rm", "mv")

        result = self.run_functions(BACKUP_DATABASE_FUNCTIONS, "backup_database\n")

        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("MySQL 备份失败", result.stdout + result.stderr)
        self.assertEqual(files_under(self.backup / "mysql"), set(),
                         "备份失败后 mysql 目录必须为空（不能留半截或空文件）")
        self.assertEqual(files_under(self.sandbox / "deploy"), set())
        self.assert_only_sandbox_paths()
        self.assertIn("mysql-pwd-via-env", self.call_log.read_text(encoding="utf-8"))
        self.assertNotIn(FIXTURE_PASSWORD, self.call_log.read_text(encoding="utf-8"),
                         "口令不能出现在 mysqldump argv 中")

    def test_backup_database_success_publishes_complete_archive_only(self):
        """用途：成功路径只留下完整备份与校验文件，不留隐藏临时文件。
        参数：无；使用假 mysqldump（成功）与真实 gzip、mv、sha256sum。
        返回值：无；备份为空、命名不符或校验和不对时失败。
        """
        self.write_mysql_env()
        self.fake_mysqldump(0)
        self.wrap("gzip", "sha256sum", "mv", "rm")

        result = self.run_functions(BACKUP_DATABASE_FUNCTIONS, "backup_database\n")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

        names = sorted(path.name for path in (self.backup / "mysql").iterdir())
        self.assertEqual(len(names), 2, names)
        archives = [name for name in names if name.endswith(".sql.gz")]
        self.assertEqual(len(archives), 1, names)
        archive = self.backup / "mysql" / archives[0]
        self.assertRegex(archives[0], r"^fixture_db-\d{8}T\d{6}Z\.sql\.gz$",
                         "备份文件名必须是脚本约定的 <库名>-<UTC 时间戳>.sql.gz")
        self.assertGreater(archive.stat().st_size, 0, "备份文件不能为空")
        self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o600)
        checksum = archive.with_name(archive.name + ".sha256")
        self.assertTrue(checksum.exists(), names)
        self.assertIn(hashlib.sha256(archive.read_bytes()).hexdigest(),
                      checksum.read_text(encoding="utf-8"))
        self.assert_only_sandbox_paths()

    def test_backup_uploads_failure_leaves_no_partial_archive(self):
        """用途：假 tar 写入半截文件后失败时，uploads 目录不得留下残片。
        参数：无；使用假 tar（先写目标文件再退出 1）。
        返回值：无；出现残留 tar.gz 或错误不可见时失败。
        """
        storage = self.sandbox / "storage"
        (storage / "tenant").mkdir(parents=True)
        (storage / "tenant" / "attachment.txt").write_text("fixture\n", encoding="utf-8")
        self.write_env(FILE_STORAGE_ROOT=str(storage))
        self.fake("tar",
                  'target=""\n'
                  'for argument in "$@"; do\n'
                  '    case "$argument" in\n'
                  '        --file=*) target="${argument#--file=}" ;;\n'
                  '    esac\n'
                  'done\n'
                  + self.record_snippet("tar") +
                  'printf \'partial-archive\\n\' >"$target"\n'
                  'exit 1\n')
        self.wrap("rm", "mv", "chmod")

        result = self.run_functions(BACKUP_UPLOADS_FUNCTIONS, "backup_uploads\n")

        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("本地上传文件备份失败", result.stdout + result.stderr)
        self.assertEqual(files_under(self.backup / "uploads"), set(),
                         "上传备份失败后 uploads 目录必须为空")
        self.assert_only_sandbox_paths()
        tar_calls = self.commands_named("tar")
        self.assertEqual(len(tar_calls), 1, "假 tar 应被调用一次")
        operands = [operand_path(argument) for argument in tar_calls[0][1:]]
        self.assertIn(str(storage.parent), operands, "tar 只能在临时上传目录的父目录上操作")
        self.assertIn(storage.name, operands)

    def test_backup_uploads_success_publishes_verifiable_archive(self):
        """用途：成功路径留下可解压、带校验和的完整归档。
        参数：无；使用主机真实 tar 与 sha256sum。
        返回值：无；归档为空、缺校验文件或残留隐藏文件时失败。
        """
        storage = self.sandbox / "storage"
        (storage / "tenant").mkdir(parents=True)
        (storage / "tenant" / "attachment.txt").write_text("fixture\n", encoding="utf-8")
        self.write_env(FILE_STORAGE_ROOT=str(storage))
        self.wrap("tar", "sha256sum", "mv", "rm", "chmod")

        result = self.run_functions(BACKUP_UPLOADS_FUNCTIONS, "backup_uploads\n")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

        names = sorted(path.name for path in (self.backup / "uploads").iterdir())
        self.assertEqual(len(names), 2, names)
        archives = [name for name in names if name.endswith(".tar.gz")]
        self.assertEqual(len(archives), 1, names)
        archive = self.backup / "uploads" / archives[0]
        self.assertRegex(archives[0], r"^uploads-\d{8}T\d{6}Z\.tar\.gz$")
        self.assertGreater(archive.stat().st_size, 0, "归档文件不能为空")
        checksum = archive.with_name(archive.name + ".sha256")
        self.assertTrue(checksum.exists(), names)
        self.assertIn(hashlib.sha256(archive.read_bytes()).hexdigest(),
                      checksum.read_text(encoding="utf-8"))
        self.assert_only_sandbox_paths()

    def test_backup_missing_environment_value_fails_before_creating_archive(self):
        """用途：环境文件缺少非空配置时不得生成任何备份文件。
        参数：无；环境文件故意缺少 MYSQL_PASSWORD。
        返回值：无；产生文件或调用 mysqldump 时失败。
        """
        self.write_env(MYSQL_HOST="127.0.0.1", MYSQL_PORT="3306",
                       MYSQL_DATABASE="fixture_db", MYSQL_USERNAME="fixture_user")
        self.fake_mysqldump(0)
        self.wrap("gzip", "sha256sum", "mv", "rm")

        result = self.run_functions(BACKUP_DATABASE_FUNCTIONS, "backup_database\n")

        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertEqual(files_under(self.backup), set())
        self.assertFalse(self.backup.exists(), "配置校验失败前不得创建备份目录")
        self.assertEqual(self.commands_named("mysqldump"), [], "缺失配置时不得调用 mysqldump")

    def test_missing_environment_value_should_report_specific_error(self):
        """用途：环境文件缺少必需配置时，stderr 必须出现具体键名，而不是只剩通用失败提示。
        参数：无；环境文件故意缺少 MYSQL_PASSWORD，并审计 chmod/mysqldump 调用。
        返回值：无；stderr 未出现键名、产生文件或调用 mysqldump 时失败。
        """
        self.write_env(MYSQL_HOST="127.0.0.1", MYSQL_PORT="3306",
                       MYSQL_DATABASE="fixture_db", MYSQL_USERNAME="fixture_user")
        self.fake_mysqldump(0)
        self.wrap("gzip", "sha256sum", "mv", "rm", "chmod")

        result = self.run_functions(BACKUP_DATABASE_FUNCTIONS, "backup_database\n")

        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("环境文件缺少非空配置", result.stderr,
                      "具体错误必须写到 stderr，不能被命令替换吞掉")
        self.assertIn("MYSQL_PASSWORD", result.stderr, "stderr 必须指明缺失的键名")
        self.assertEqual(self.commands_named("mysqldump"), [], "缺失配置时不得调用 mysqldump")
        self.assertEqual(self.commands_named("chmod"), [], "缺失配置时不得改动备份目录权限")
        self.assertFalse(self.backup.exists(), "缺失配置时不得创建备份目录")

    def test_backup_password_never_reaches_stdout_stderr_or_log(self):
        """用途：备份成功与失败都不得把口令写进输出或运维日志。
        参数：无；LOG_FILE 指向临时日志文件。
        返回值：无；口令出现在任何输出通道时失败。
        """
        self.write_mysql_env()
        log_file = self.sandbox / "ops.log"
        for exit_code in (0, 1):
            with self.subTest(mysqldump_exit=exit_code):
                self.call_log.unlink(missing_ok=True)
                self.fake_mysqldump(exit_code)
                self.wrap("gzip", "sha256sum", "mv", "rm")
                result = self.run_functions(BACKUP_DATABASE_FUNCTIONS, "backup_database\n",
                                            LOG_FILE=str(log_file))
                combined = result.stdout + result.stderr
                self.assertNotIn(FIXTURE_PASSWORD, combined, "口令不能出现在 stdout/stderr")
                if log_file.exists():
                    self.assertNotIn(FIXTURE_PASSWORD, log_file.read_text(encoding="utf-8"),
                                     "口令不能写入运维日志")


class RollbackSafetyTest(OpsSandboxTestCase):
    """用途：回归回退的指针切换与失败恢复，确保 current 不会指向不存在的版本。"""

    def setUp(self):
        """用途：准备两个可用发布版本、current 指针与假 systemctl。
        参数：无。
        返回值：无。
        """
        super().setUp()
        self.require_gnu()
        self.release_a = self.make_release("a")
        self.release_b = self.make_release("b")
        os.symlink(self.release_a, self.deploy / "current")
        self.fake("systemctl",
                  self.record_snippet("systemctl") + 'exit 0\n')
        self.wrap("ln", "mv")

    def rollback(self, target, **environment):
        """用途：在隔离环境里执行真实的 command_rollback。
        参数：target 为 --target 值；environment 为额外环境变量（如 READY_MODE）。
        返回值：CompletedProcess。
        """
        overrides = {"ROLLBACK_TARGET": target}
        overrides.update(environment)
        return self.run_functions(ROLLBACK_FUNCTIONS, "command_rollback\n",
                                  boundaries=ROOT_BOUNDARY + READY_BOUNDARY, **overrides)

    def test_rollback_missing_target_keeps_current_pointer(self):
        """用途：releases/<target> 不存在时非零退出，current 保持原指向。
        参数：无；target 指向未发布的版本。
        返回值：无；指针变化或错误不可见时失败。
        """
        result = self.rollback("20990101.1")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("目标发布版本不存在", result.stdout + result.stderr)
        self.assert_current_release("a")
        self.assertEqual(self.commands_named("systemctl"), [], "目标缺失时不得重启服务")

    def test_rollback_rejects_target_escaping_releases_directory(self):
        """用途：`--target` 含 `..`、`/` 或指向 releases 外的符号链接时拒绝且不切指针。
        参数：无；覆盖三种逃逸写法。
        返回值：无；任何一种写法改变指针或重启服务时失败。
        """
        outside = self.sandbox / "outside-release"
        (outside / "bin").mkdir(parents=True)
        for binary in ("api", "worker", "migrate"):
            path = outside / "bin" / binary
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(0o755)
        os.symlink(outside, self.deploy / "releases" / "escape")

        cases = {
            "../outside-release": "发布版本号仅允许字母、数字、点、下划线和短横线",
            "a/b": "发布版本号仅允许字母、数字、点、下划线和短横线",
            "..": "目标发布版本越出了 releases 目录",
            "escape": "目标发布版本越出了 releases 目录",
        }
        for target, expected in cases.items():
            with self.subTest(target=target):
                result = self.rollback(target)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(expected, result.stdout + result.stderr)
                self.assert_current_release("a")
                self.assertEqual(self.commands_named("systemctl"), [])

    def test_rollback_tampered_target_directory_is_rejected(self):
        """用途：目标目录被篡改成普通文件时拒绝回退，旧指针必须保持。
        参数：无；releases/b 被替换为同名文件。
        返回值：无；指针变化或残留临时链接时失败。
        """
        shutil.rmtree(self.release_b)
        self.release_b.write_text("not-a-release\n", encoding="utf-8")

        result = self.rollback("b")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("目标发布版本不存在", result.stdout + result.stderr)
        self.assert_current_release("a")
        self.assertEqual(list(self.deploy.glob(".current.new.*")), [],
                         "失败路径不能留下临时符号链接")

    def test_rollback_reports_missing_current_release_before_switching(self):
        """用途：current 指针缺失时，回退必须把具体原因写到 stderr，而不是被命令替换吞掉。
        参数：无；删除 deploy/current 后回退到可用的 b。
        返回值：无；stderr 缺原因、创建 current 或重启服务时失败。
        """
        (self.deploy / "current").unlink()

        result = self.rollback("b")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("未找到当前发布版本", result.stderr,
                      "具体原因必须在 stderr 可见，不能只剩 set -e 的静默退出")
        self.assertEqual(self.commands_named("systemctl"), [], "未确认原版本前不得重启服务")
        self.assertFalse(os.path.lexists(self.deploy / "current"), "失败路径不得创建 current")

    def test_rollback_switches_current_atomically_on_success(self):
        """用途：回退到 b 后 current 指向 b，且不留临时链接。
        参数：无；wait_for_ready 返回成功。
        返回值：无；指针未切换、残留临时链接或未重启服务时失败。
        """
        result = self.rollback("b", READY_MODE="ok")

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assert_current_release("b")
        self.assertEqual(list(self.deploy.glob(".current.new.*")), [])
        restarts = self.commands_named("systemctl")
        self.assertEqual(len(restarts), 1, restarts)
        self.assertEqual(restarts[0][1:], ["restart", "basic-platform-api.service",
                                          "basic-platform-worker.service"])
        self.assert_only_sandbox_paths()

    def test_rollback_restores_previous_pointer_when_target_not_ready(self):
        """用途：目标版本未就绪时自动恢复原指针，并明确提示人工研判数据库兼容性。
        参数：无；wait_for_ready 首次失败、恢复后成功。
        返回值：无；指针停在目标版本或未提示时失败。
        """
        result = self.rollback("b", READY_MODE="fail-then-ok")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("已自动恢复原版本", result.stdout + result.stderr)
        self.assert_current_release("a")
        self.assertEqual(list(self.deploy.glob(".current.new.*")), [])
        self.assertEqual(len(self.commands_named("systemctl")), 2, "切换与恢复各重启一次")

    def test_rollback_reports_manual_intervention_when_both_versions_fail(self):
        """用途：新旧版本都不就绪时必须保留原指针并要求人工介入。
        参数：无；wait_for_ready 始终失败。
        返回值：无；指针停在目标版本或错误信息缺失时失败。
        """
        result = self.rollback("b", READY_MODE="fail")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("均未能通过就绪检查", result.stdout + result.stderr)
        self.assert_current_release("a")

    def test_rollback_rejects_target_that_is_already_current(self):
        """用途：目标与当前版本相同时拒绝无意义的切换。
        参数：无；target 等于 current。
        返回值：无；仍执行切换或重启时失败。
        """
        result = self.rollback("a")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("目标版本已经是当前版本", result.stdout + result.stderr)
        self.assert_current_release("a")
        self.assertEqual(self.commands_named("systemctl"), [])


class PruneBackupsBoundaryTest(OpsSandboxTestCase):
    """用途：回归清理历史备份的删除范围与参数边界。"""

    def setUp(self):
        """用途：准备过期备份、新鲜备份、非备份文件、上传根目录与目录外哨兵。
        参数：无。
        返回值：无。
        """
        super().setUp()
        # command_prune_backups 现在要求环境文件里存在非空的 FILE_STORAGE_ROOT，
        # 并与备份目录做重叠校验；canonical_path 依赖 GNU readlink -m。
        self.require_gnu()
        self.wrap("find")
        self.storage = self.sandbox / "storage"
        (self.storage / "tenant").mkdir(parents=True)
        (self.storage / "tenant" / "attachment.txt").write_text("fixture\n", encoding="utf-8")
        self.write_env(FILE_STORAGE_ROOT=str(self.storage))
        self.outside = self.sandbox / "outside"
        self.old_files = []
        self.manual_files = []
        self.protected_files = []
        self.make_prune_fixture()

    def make_prune_fixture(self):
        """用途：构造 mysql/uploads 子目录内的新旧备份、人工同扩展名文件及子目录外的保护文件。
        参数：无。
        返回值：无；夹具路径记录在 self.old_files、self.manual_files 与 self.protected_files。
        """
        mysql = self.backup / "mysql"
        uploads = self.backup / "uploads"
        keys = self.backup / "keys"
        for directory in (mysql, uploads, keys, self.outside):
            directory.mkdir(parents=True, exist_ok=True)
        # 脚本自身生成的命名：<库名>-<UTC 时间戳>.sql.gz / uploads-<UTC 时间戳>.tar.gz 及其 .sha256。
        expired = [
            mysql / "fixture_db-20240101T000000Z.sql.gz",
            mysql / "fixture_db-20240101T000000Z.sql.gz.sha256",
            uploads / "uploads-20240101T000000Z.tar.gz",
            uploads / "uploads-20240101T000000Z.tar.gz.sha256",
        ]
        # 人工放进备份子目录、与新命名模式不匹配的同扩展名文件：不按扩展名删除的反例。
        manual = [
            mysql / "manual-dump-2024-01-01.sql.gz",
            uploads / "manual-upload-2024-01-01.tar.gz",
        ]
        aged_protected = [
            mysql / "manual-notes.txt",
            keys / "api.key",
            self.backup / "stray-20240101T000000Z.sql.gz",
            self.outside / "uploads-20240101T000000Z.tar.gz",
        ]
        fresh_protected = [
            mysql / "fixture_db-20990101T000000Z.sql.gz",
            uploads / "uploads-20990101T000000Z.tar.gz",
            mysql / "manual-dump.sql",
        ]
        protected = fresh_protected + aged_protected + manual
        for path in expired + protected:
            path.write_text("fixture\n", encoding="utf-8")
        for path in expired + manual + aged_protected:
            age_file(path, 30)
        self.old_files = expired
        self.manual_files = manual
        self.protected_files = protected

    def prune(self, retain_days, **environment):
        """用途：执行真实的 command_prune_backups。
        参数：retain_days 为 --retain-days 值；environment 为额外环境变量。
        返回值：CompletedProcess。
        """
        overrides = {"RETAIN_DAYS": retain_days}
        overrides.update(environment)
        return self.run_functions(PRUNE_FUNCTIONS, "command_prune_backups\n",
                                  boundaries=ROOT_BOUNDARY, **overrides)

    def test_prune_deletes_only_expired_backup_files_inside_backup_dir(self):
        """用途：只删除两个固定子目录里过期且符合备份扩展名的文件。
        参数：无；retain-days=7，夹具中 30 天前的备份视为过期。
        返回值：无；删到目录外或手工文件时失败。
        """
        expired = {str(path.relative_to(self.sandbox)) for path in self.old_files}
        expected = self.tracked_files() - expired

        result = self.prune("7")

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("历史备份清理完成", result.stdout)
        for path in self.old_files:
            self.assertFalse(path.exists(), f"过期备份应被删除：{path}")
        for path in self.protected_files:
            self.assertTrue(path.exists(), f"受保护文件不得被删除：{path}")
        self.assertEqual(self.tracked_files(), expected,
                         "除过期备份外不得删除任何文件（含目录外、手工备份与密钥）")
        inspected = {operand_path(argument)
                     for call in self.commands_named("find")
                     for argument in call[1:]}
        self.assertEqual(inspected & {str(self.backup / "mysql"), str(self.backup / "uploads")},
                         {str(self.backup / "mysql"), str(self.backup / "uploads")},
                         "find 必须且只能扫描两个固定备份子目录")
        self.assert_only_sandbox_paths()

    def test_prune_refuses_invalid_retain_days_without_deleting_anything(self):
        """用途：非法或负数保留天数必须拒绝，且不删除任何文件。
        参数：无；覆盖空值、0、负数、非数字、小数、带空格。
        返回值：无；出现删除或错误不可见时失败。
        """
        snapshot = tree_snapshot(self.backup)
        for value in ("", "0", "-1", "abc", "1.5", " 7", "7 "):
            with self.subTest(retain_days=value):
                result = self.prune(value)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("必须提供正整数 --retain-days", result.stdout + result.stderr)
                self.assertEqual(tree_snapshot(self.backup), snapshot,
                                 "参数被拒绝时不得删除任何备份文件")
                self.assertEqual(self.commands_named("find"), [], "参数被拒绝时不得调用 find")

    def test_prune_rejects_system_directories_as_backup_dir(self):
        """用途：`--backup-dir` 指向系统目录时必须在初始化校验阶段拒绝。
        参数：无；覆盖 `/`、`/var`、`/etc`、`/tmp`。
        返回值：无；校验放行或错误不可见时失败。
        """
        self.require_linux_host()
        for value in ("/", "/var", "/etc", "/tmp"):
            with self.subTest(backup_dir=value):
                result = self.run_functions(
                    INITIALIZE_FUNCTIONS, "initialize_settings\n",
                    boundaries=ROOT_BOUNDARY + LINUX_BOUNDARY,
                    BACKUP_DIR=value, LOG_DIRECTORY=str(self.sandbox / "logs"))
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("不能直接指向系统目录", result.stdout + result.stderr)
                self.assertIn("/", result.stdout + result.stderr)

    def test_prune_only_inspects_fixed_backup_subdirectories(self):
        """用途：备份目录里混有普通附件与密钥时，只扫描 mysql/ 与 uploads/ 两个固定子目录。
        参数：无；备份目录内另有 tenant/photo.jpg 与 keys/api.key。
        返回值：无；普通附件或密钥被删除、扫描越界时失败。
        """
        storage = self.backup
        attachment = storage / "tenant" / "photo.jpg"
        attachment.parent.mkdir(parents=True, exist_ok=True)
        attachment.write_text("fixture\n", encoding="utf-8")
        age_file(attachment, 30)
        key_file = storage / "keys" / "api.key"

        result = self.prune("7", BACKUP_DIR=str(storage))

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(attachment.exists(), "普通附件不在固定备份子目录内，不得删除")
        self.assertTrue(key_file.exists(), "密钥文件不得被清理")
        for call in self.commands_named("find"):
            for argument in call[1:]:
                candidate = operand_path(argument)
                if not candidate.startswith(str(self.backup)):
                    continue
                self.assertIn(candidate, {str(storage / "mysql"), str(storage / "uploads")},
                              f"find 只能作用于固定备份子目录：{candidate}")

    def test_prune_should_reject_backup_dir_overlapping_file_storage_root(self):
        """用途：备份目录与 FILE_STORAGE_ROOT 相同或互为父子时，清理必须拒绝且零删除。
        参数：无；覆盖相同、备份在存储内、备份是存储父目录三种重叠。
        返回值：无；放行、删除任何文件或调用 find 时失败。
        """
        payload = self.storage / "payload"
        payload.mkdir(parents=True, exist_ok=True)
        (payload / "attachment.txt").write_text("fixture\n", encoding="utf-8")
        cases = [
            (str(self.storage), str(self.storage)),
            (str(self.storage), str(self.storage / "backup")),
            (str(payload), str(self.storage)),
        ]
        for storage_root, backup_dir in cases:
            with self.subTest(storage_root=storage_root, backup_dir=backup_dir):
                self.write_env(FILE_STORAGE_ROOT=storage_root)
                self.call_log.unlink(missing_ok=True)
                snapshot = tree_snapshot(Path(backup_dir))
                files_before = files_under(self.sandbox)

                result = self.prune("7", BACKUP_DIR=backup_dir)

                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("不能相同或互为父子目录", result.stdout + result.stderr)
                self.assertEqual(tree_snapshot(Path(backup_dir)), snapshot,
                                 "重叠被拒绝时不得删除或新建任何文件")
                self.assertEqual(files_under(self.sandbox), files_before,
                                 "重叠被拒绝时整个沙箱不得有任何文件变化")
                if snapshot == {}:
                    self.assertFalse(Path(backup_dir).exists(), "重叠被拒绝时不得创建备份目录")
                self.assertEqual(self.commands_named("find"), [], "重叠被拒绝时不得调用 find")

    def test_prune_should_only_delete_script_generated_backup_names(self):
        """用途：只删除本脚本生成的 <库名>-<UTC 时间戳>.sql.gz / uploads-<UTC 时间戳>.tar.gz
        及其同名 .sha256；人工放进 mysql/ 或 uploads/ 的同扩展名文件必须保留。
        参数：无；夹具里已放置 30 天前的人工 mysqldump 与人工 tar 归档。
        返回值：无；人工文件被删除、或脚本命名的过期备份未被删除时失败。
        """
        mysql_remaining = {path.name for path in (self.backup / "mysql").iterdir() if path.is_file()}
        uploads_remaining = {path.name for path in (self.backup / "uploads").iterdir() if path.is_file()}

        result = self.prune("7")

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("历史备份清理完成", result.stdout)
        for path in self.old_files:
            self.assertFalse(path.exists(), f"脚本生成的过期备份应被删除：{path}")
        for path in self.manual_files:
            self.assertTrue(path.exists(), f"非脚本命名的人工备份不得被删除：{path}")
        self.assertEqual({path.name for path in (self.backup / "mysql").iterdir() if path.is_file()},
                         {"fixture_db-20990101T000000Z.sql.gz", "manual-dump-2024-01-01.sql.gz",
                          "manual-notes.txt", "manual-dump.sql"},
                         f"mysql/ 内只应删除脚本命名的过期备份及其校验文件，清理前={sorted(mysql_remaining)}")
        self.assertEqual({path.name for path in (self.backup / "uploads").iterdir() if path.is_file()},
                         {"uploads-20990101T000000Z.tar.gz", "manual-upload-2024-01-01.tar.gz"},
                         f"uploads/ 内只应删除脚本命名的过期归档及其校验文件，清理前={sorted(uploads_remaining)}")

    def test_backup_uploads_rejects_storage_root_overlapping_backup_dir(self):
        """用途：上传备份必须拒绝 FILE_STORAGE_ROOT 与备份目录相同或互为父子，且拒绝时零副作用。
        参数：无；覆盖相同、备份在存储内、备份是存储父目录三种情况。
        返回值：无；放行、创建/改权限、生成归档或写出沙箱外时失败。
        """
        storage = self.storage
        self.wrap("tar", "sha256sum", "mv", "rm", "chmod")
        payload = storage / "payload"
        payload.mkdir(parents=True, exist_ok=True)
        (payload / "attachment.txt").write_text("fixture\n", encoding="utf-8")

        cases = [
            (str(storage), str(storage)),
            (str(storage), str(storage / "backup")),
            (str(payload), str(storage)),
        ]
        for storage_root, backup_dir in cases:
            with self.subTest(storage_root=storage_root, backup_dir=backup_dir):
                self.write_env(FILE_STORAGE_ROOT=storage_root)
                self.call_log.unlink(missing_ok=True)
                target = Path(backup_dir)
                existed = target.exists()
                mode_before = stat.S_IMODE(target.stat().st_mode) if existed else None
                snapshot = tree_snapshot(target)

                result = self.run_functions(BACKUP_UPLOADS_FUNCTIONS, "backup_uploads\n",
                                            BACKUP_DIR=backup_dir)

                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("不能相同或互为父子目录", result.stdout + result.stderr)
                self.assertEqual(tree_snapshot(target), snapshot,
                                 "拒绝时不得创建目录或改动其中任何条目")
                if existed:
                    self.assertEqual(stat.S_IMODE(target.stat().st_mode), mode_before,
                                     "拒绝时不得收紧已存在备份目录的权限")
                else:
                    self.assertFalse(target.exists(), "拒绝时不得创建备份目录")
                self.assertEqual(files_under(target / "uploads"), set(), "拒绝时不得生成归档")
                self.assertEqual(self.commands_named("tar"), [], "拒绝时不得调用 tar")
                self.assertEqual(self.commands_named("chmod"), [], "拒绝时不得调用 chmod")


class EnvFileReadTest(OpsSandboxTestCase):
    """用途：回归环境文件解析规则，并确保读单个键不会泄漏其他密钥。"""

    def read_key(self, key, text=None):
        """用途：调用真实的 read_env_value。
        参数：key 为待读取键；text 为环境文件内容。
        返回值：CompletedProcess。
        """
        if text is not None:
            self.env_file.write_text(text, encoding="utf-8")
        return self.run_functions(["read_env_value"], f'read_env_value {key}\n')

    def test_read_env_value_follows_key_value_contract(self):
        """用途：注释、引号、export、缺失键、重复键与 `=` 内嵌都按约定解析。
        参数：无；环境文件覆盖全部边界写法。
        返回值：无；任一写法解析错误时失败。
        """
        text = (
            "# MYSQL_PASSWORD=commented-out\n"
            "\n"
            "   \n"
            "export EXPORTED=exported-value\n"
            'DOUBLE_QUOTED="double quoted"\n'
            "SINGLE_QUOTED='single quoted'\n"
            'HASH_INSIDE="quoted # not a comment"\n'
            "  SPACED_KEY  =  spaced value  \n"
            "EQUAL_INSIDE=abc==\n"
            "EMPTY_VALUE=\n"
            "NO_EQUALS_LINE\n"
            "DUPLICATED=first\n"
            "DUPLICATED=second\n"
            'UNBALANCED="opened\n'
        )
        cases = {
            "EXPORTED": "exported-value\n",
            "DOUBLE_QUOTED": "double quoted\n",
            "SINGLE_QUOTED": "single quoted\n",
            "HASH_INSIDE": "quoted # not a comment\n",
            "SPACED_KEY": "spaced value\n",
            "EQUAL_INSIDE": "abc==\n",
            "EMPTY_VALUE": "\n",
            "DUPLICATED": "first\n",
            "UNBALANCED": '"opened\n',
            "NO_EQUALS_LINE": "",
            "MISSING_KEY": "",
            "MYSQL_PASSWORD": "",
        }
        for key, expected in cases.items():
            with self.subTest(key=key):
                result = self.read_key(key, text)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, expected,
                                 f"{key} 解析结果不符合脚本约定")

    def test_read_env_value_does_not_echo_adjacent_secrets(self):
        """用途：读取普通键时不得把相邻的 PASSWORD/SECRET 值带进输出。
        参数：无；环境文件同时包含普通键与两个密钥键。
        返回值：无；密钥值出现在 stdout/stderr 时失败。
        """
        text = (f"FILE_STORAGE_ROOT=/srv/fixture\n"
                f"MYSQL_PASSWORD={FIXTURE_PASSWORD}\n"
                f"API_SECRET={FIXTURE_SECRET}\n")
        for key in ("FILE_STORAGE_ROOT", "MISSING_KEY"):
            with self.subTest(key=key):
                result = self.read_key(key, text)
                combined = result.stdout + result.stderr
                self.assertNotIn(FIXTURE_PASSWORD, combined)
                self.assertNotIn(FIXTURE_SECRET, combined)
        result = self.read_key("FILE_STORAGE_ROOT", text)
        self.assertEqual(result.stdout, "/srv/fixture\n")

    def test_require_nonempty_env_value_rejects_blank_without_leaking(self):
        """用途：必需配置为空时立即失败，且错误信息不带其他密钥的值。
        参数：无；MYSQL_PASSWORD 为空、API_SECRET 有值。
        返回值：无；失败缺失或泄漏密钥时失败。
        """
        text = f"MYSQL_PASSWORD=\nAPI_SECRET={FIXTURE_SECRET}\n"
        self.env_file.write_text(text, encoding="utf-8")
        result = self.run_functions(
            ["log", "fatal", "read_env_value", "require_nonempty_env_value"],
            'require_nonempty_env_value MYSQL_PASSWORD\n')

        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("环境文件缺少非空配置 MYSQL_PASSWORD", result.stdout + result.stderr)
        self.assertNotIn(FIXTURE_SECRET, result.stdout + result.stderr)


class ArgumentValidationTest(OpsSandboxTestCase):
    """用途：回归参数校验必须“先拒绝、后动手”，被拒绝的参数不允许有文件副作用。"""

    def test_invalid_release_id_is_rejected_before_any_backup(self):
        """用途：非法 --release-id 在备份与发布之前就被拒绝。
        参数：无；覆盖斜杠、空格、分号、路径穿越、命令替换、换行。
        返回值：无；放行、创建备份目录或调用 mysqldump 时失败。
        """
        (self.sandbox / "bootstrap.sh").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        self.write_env(MYSQL_HOST="127.0.0.1", MYSQL_PORT="3306",
                       MYSQL_DATABASE="fixture_db", MYSQL_USERNAME="fixture_user",
                       MYSQL_PASSWORD=FIXTURE_PASSWORD)
        self.fake("mysqldump",
                  'printf \'mysqldump\\t%s\\n\' "$*" >>"${OPS_CALL_LOG}"\n'
                  'printf \'INSERT INTO fixture VALUES (1);\\n\'\n'
                  'exit 0\n')
        self.fake("systemctl", 'exit 0\n')
        snapshot = tree_snapshot(self.sandbox)

        for value in ("20260723/1", "20260723 1", "20260723;1", "../release",
                      "$(id)", "release\n20260724.1"):
            with self.subTest(release_id=value):
                self.call_log.unlink(missing_ok=True)
                result = self.run_functions(
                    ["log", "fatal", "require_env_file", "validate_release_id",
                     "command_deploy"],
                    "command_deploy\n",
                    boundaries=ROOT_BOUNDARY + READY_BOUNDARY, RELEASE_ID=value)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("发布版本号仅允许", result.stdout + result.stderr)
                self.assertEqual(tree_snapshot(self.sandbox), snapshot,
                                 "被拒绝的 --release-id 不得产生任何文件")
                self.assertEqual(self.commands_named("mysqldump"), [])
                self.assertFalse(self.backup.exists(), "校验失败前不得创建备份目录")

    def test_valid_release_id_passes_validation(self):
        """用途：合法版本号必须通过校验，避免过度收紧挡住正常发布。
        参数：无；覆盖时间戳、点、下划线、短横线。
        返回值：无；合法值被拒绝时失败。
        """
        for value in ("20260723.1", "release_2026-07-23", "v1.2.3-rc_1"):
            with self.subTest(release_id=value):
                result = self.run_functions(["log", "fatal", "validate_release_id"],
                                            f'validate_release_id {value}\n')
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_parser_rejects_malformed_options_without_side_effects(self):
        """用途：真实 parse_arguments 对缺值、非数字和未知选项立即失败。
        参数：无；覆盖 --lines 非正整数、未知选项与缺参。
        返回值：无；放行或产生文件副作用时失败。
        """
        snapshot = tree_snapshot(self.sandbox)
        cases = {
            "--lines 0": "--lines 必须是正整数",
            "--lines abc": "--lines 必须是正整数",
            "--unknown": "未知选项",
            "--env-file": "--env-file 缺少路径",
            "--retain-days": "--retain-days 缺少天数",
        }
        for arguments, expected in cases.items():
            with self.subTest(arguments=arguments):
                result = self.run_functions(["log", "fatal", "parse_arguments"],
                                            f"parse_arguments doctor {arguments}\n")
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(expected, result.stdout + result.stderr)
                self.assertEqual(tree_snapshot(self.sandbox), snapshot)

    def test_relative_production_paths_are_rejected_by_initialize_settings(self):
        """用途：相对路径的 --deploy-root/--backup-dir 必须在初始化阶段拒绝。
        参数：无；覆盖相对发布根目录与相对备份目录。
        返回值：无；放行或错误不可见时失败。
        """
        for overrides, expected in (
                ({"DEPLOY_ROOT": "relative/deploy"}, "生产路径必须是绝对路径"),
                ({"ENV_FILE": "relative.env"}, "生产路径必须是绝对路径"),
                ({"BACKUP_DIR": "relative/backup"}, "生产路径必须是绝对路径"),
                ({"HEALTH_URL": "http://127.0.0.1:8080/api"}, "--health-url 仅接受不带路径")):
            with self.subTest(**overrides):
                environment = {"LOG_DIRECTORY": str(self.sandbox / "logs"),
                               "BACKUP_DIR": str(self.backup)}
                environment.update(overrides)
                result = self.run_functions(INITIALIZE_FUNCTIONS, "initialize_settings\n",
                                            boundaries=ROOT_BOUNDARY + LINUX_BOUNDARY,
                                            **environment)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(expected, result.stdout + result.stderr)


class RootGateTest(OpsSandboxTestCase):
    """用途：确认破坏性命令本身仍带 root 门禁，避免隔离测试把身份校验一起绕过。"""

    def test_destructive_commands_still_require_root(self):
        """用途：普通用户直接执行清理命令时必须被 require_root 拒绝且不删除文件。
        参数：无；不替换 require_root。
        返回值：无；门禁消失时失败；以 root 运行时无法验证则跳过。
        """
        if os.geteuid() == 0:
            self.skipTest("当前以 root 运行（如容器内），require_root 必然放行，无法验证身份门禁")
        sentinel = self.backup / "mysql" / "fixture_db-20240101T000000Z.sql.gz"
        sentinel.parent.mkdir(parents=True)
        sentinel.write_text("fixture\n", encoding="utf-8")
        age_file(sentinel, 30)

        result = self.run_functions(["log", "fatal", "require_root", "command_prune_backups"],
                                    "command_prune_backups\n", RETAIN_DAYS="7")

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("命令需要 root 权限", result.stdout + result.stderr)
        self.assertTrue(sentinel.exists(), "被 root 门禁拒绝时不得删除任何备份")


if __name__ == "__main__":
    unittest.main()
