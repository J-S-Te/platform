"""uipctl 公共基础设施：日志、退出码、参数模型、终端交互与临时工作目录。

对应 Shell 的 lib/subsystem-common.sh 与 scripts/subsystem.sh 的全局变量区；
日志格式、退出码与交互语义逐字保持一致，避免脚本化调用方感知差异。
"""
from __future__ import annotations

import datetime
import os
import shutil
import subprocess
import sys
import tempfile
from typing import NoReturn


class CliExit(Exception):
    """受控退出：携带 Shell 时代的退出码（0 = 成功，1 = 执行失败，2 = 参数/前置校验失败）。"""

    def __init__(self, code: int) -> None:
        super().__init__("exit %d" % code)
        self.code = code


def log(level: str, message: str) -> None:
    """与 Shell log() 逐字一致：`[时间] [级别] 内容` 写 stderr。"""
    timestamp = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    sys.stderr.write("[%s] [%s] %s\n" % (timestamp, level, message))
    sys.stderr.flush()


def fail(message: str, code: int = 2) -> NoReturn:
    """`log ERROR` + 以指定退出码结束（等价 Shell 的 log ERROR 后 exit）。"""
    log("ERROR", message)
    raise CliExit(code)


def require_command(name: str) -> None:
    """等价 Shell require_command：缺少命令时退出 2。"""
    if shutil.which(name) is None:
        fail("缺少命令：%s" % name, 2)


def can_interact() -> bool:
    """等价 Shell can_interact：标准输入与标准输出都是终端。"""
    return sys.stdin.isatty() and sys.stdout.isatty()


def read_line(prompt: str) -> str:
    """读取一行输入并去掉行尾换行。

    Bash 的 `read -p` 只在标准输入是终端时显示提示，这里保持同样的行为，
    避免管道/CI 场景平白多出提示文本。
    """
    if prompt and sys.stdin.isatty():
        sys.stderr.write(prompt)
        sys.stderr.flush()
    line = sys.stdin.readline()
    if not line:
        return ""
    return line[:-1] if line.endswith("\n") else line


def sleep_seconds(seconds: object) -> None:
    """等待指定秒数。

    与 Shell 实现一致地调用 PATH 中的 `sleep`：轮询节奏（间隔 5 秒、先查询后等待）
    对外保持完全同构，测试也可以用替身 sleep 观察等待行为；sleep 失败时按原样退出。
    """
    try:
        completed = subprocess.run(["sleep", str(seconds)])
    except OSError:
        fail("缺少命令：sleep", 2)
    if completed.returncode != 0:
        raise CliExit(completed.returncode)


class Config:
    """一次调用的全部参数，等价于 Shell 入口脚本的全局变量区。"""

    def __init__(self) -> None:
        # 平台 API 与传输保护默认值
        self.api_base_url = os.environ.get("BASIC_PLATFORM_API_BASE_URL") or "http://localhost:8081/api/v1"
        self.platform_origin = os.environ.get("BASIC_PLATFORM_ORIGIN") or ""
        self.allow_insecure_http_api = os.environ.get("BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API", "false")
        self.insecure_http_api_allowed_hosts = os.environ.get(
            "BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS", ""
        )
        # 通用认证参数
        self.account = ""
        self.password_stdin = False
        self.replace_existing_session = False
        self.cookie_file = ""
        # 通用目标参数
        self.application_code = ""
        self.environment = ""
        self.issuer_alias = ""
        # 通用控制
        self.assume_yes = False
        # offboard 专用
        self.confirmation_code = ""
        self.shallow = False
        self.skip_teardown = False
        self.delete_application = False
        # connect 专用
        self.connect_public_origin = ""
        self.connect_initial_admin = ""
        self.connect_idempotency_key = ""
        self.connect_resume_id = ""
        self.connect_watch = True
        self.connect_watch_interval = "5"
        self.connect_watch_max = "60"


class Workspace:
    """临时工作目录（权限 0700），退出时删除；等价于 Shell 的 `trap cleanup EXIT`。"""

    def __init__(self, prefix: str) -> None:
        self.path = tempfile.mkdtemp(prefix=prefix)
        self.session = None

    def __enter__(self) -> "Workspace":
        return self

    def __exit__(self, exc_type, exc_value, traceback) -> bool:
        self.close()
        return False

    def close(self) -> None:
        """先退出本次创建的会话，再删除临时目录；不掩盖原有退出码。"""
        session = self.session
        if session is not None:
            try:
                session.logout_owned_session()
            except Exception:
                pass
        shutil.rmtree(self.path, ignore_errors=True)


def write_private(path: str, data: bytes) -> None:
    """以 0600 权限写入文件：口令、Cookie、封套等敏感内容不落到宽权限文件。"""
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "wb") as handle:
        handle.write(data)


def wipe(path: str) -> None:
    """尽力清除敏感临时文件（Shell 版本用 `: > file` + rm）。"""
    try:
        write_private(path, b"")
    except OSError:
        pass
    try:
        os.unlink(path)
    except OSError:
        pass


def parse_positive_int(raw: str) -> int:
    """按 Bash 算术的宽松语义把轮询参数转成整数（非法值视为 0）。"""
    value = raw.strip()
    digits = value[1:] if value[:1] in {"+", "-"} else value
    if not digits.isdigit():
        return 0
    return int(value)
