"""验证 catalog 同步脚本（contract / settlement / project）的入参校验、请求契约与凭据安全。

用途：用 PATH 桩提供 docker（代替容器内 mysql 客户端）、curl 与 jq 的调用面，
在独立临时目录内驱动三个 sync-*-catalog.sh，断言 PUT URL/方法/鉴权头/JSON payload，
以及失败退出码与凭据不泄漏。
参数：无；可选环境变量 CATALOG_TEST_KEEP=1 保留临时目录，CATALOG_TEST_BASH 指定 bash。
返回值：进程退出码 0 表示全部用例通过，非 0 表示存在失败（unittest 约定）。

隔离性：不连接真实 MySQL（docker 桩只把预置 TSV 打到 stdout）、不访问网络（curl 桩直接
返回预置 JSON）、不接触真实 Docker daemon；所有临时文件写在 TMPDIR 指向的临时目录内。

已知环境依赖：
  * sync-contract-catalog.sh 只使用 bash 3.2/5 均支持的语法（不使用关联数组等
    bash 4 特性），因此完整流程用例在 macOS 自带 bash 3.2 与 Linux bash 5 下都执行，
    不再按 bash 版本 skip。可用 CATALOG_TEST_BASH 显式指定解释器。
  * jq 是脚本自身的前置依赖（脚本内有 command -v jq 校验），因此这里使用系统 jq；
    缺少 jq 时相关用例 skip 而不是伪造通过。
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = ROOT / "scripts"
CONTRACT = SCRIPTS / "sync-contract-catalog.sh"
SETTLEMENT = SCRIPTS / "sync-settlement-catalog.sh"
PROJECT = SCRIPTS / "sync-project-catalog.sh"

# ULID 形态（Crockford base32，排除 I/L/O/U）。
APP_ID = "01J2ABCDEFGHJKMNPQRSTVWXYZ"
ISSUER = "http://127.0.0.1:1"
CLIENT_ID = "catalog-publisher"
CLIENT_SECRET = 'publisher-"secret"-\\value'
ACCESS_TOKEN = 'tok-"en"-\\value'
MANIFEST_PATH_LITERAL = '/catalog/permission-manifest.json'


def curl_escape(value: str) -> str:
    """返回 curl --config 双引号值里的转义形式，用于构造期望值。

    转义规则本身由 CurlEscapeFunctionTest 用字面量期望单独验证，这里只做期望值换算。
    """
    return value.replace("\\", "\\\\").replace('"', '\\"')


def selected_bash() -> str:
    """返回驱动被测脚本的 bash：CATALOG_TEST_BASH 覆盖优先，否则 PATH 中的 bash。

    被测脚本已兼容 bash 3.2（macOS 自带）到 bash 5，因此不再按版本挑选解释器或跳过用例。
    """
    override = os.environ.get("CATALOG_TEST_BASH")
    if override:
        resolved = shutil.which(override) if not os.path.isabs(override) else override
        if not resolved or not os.access(resolved, os.X_OK):
            raise RuntimeError(f"CATALOG_TEST_BASH={override} 不是可执行的 bash")
        return resolved
    return shutil.which("bash") or "/bin/bash"


def python_executable() -> str:
    """返回当前解释器路径，供显式 shebang 使用。"""
    return sys.executable


BASH = selected_bash()
BASH_VERSION = subprocess.run(
    [BASH, "-c", "echo $BASH_VERSION"], capture_output=True, text=True, timeout=30
).stdout.strip()
HAS_JQ = shutil.which("jq") is not None

requires_jq = unittest.skipUnless(HAS_JQ, "被测脚本以 jq 为前置依赖，本机缺少 jq")


def extract_function(script: Path, name: str) -> str:
    """从 sh 脚本中截取指定函数源码（名称+参数+返回值见 docstring 约定）。

    参数：script 脚本路径；name 函数名。
    返回值：函数定义源码。用 bash -nc 逐个校验「列首 }」候选，避免误判多行结构。
    """
    lines = script.read_text(encoding="utf-8").splitlines()
    start = None
    for index, line in enumerate(lines):
        if line.startswith(f"{name}() {{"):
            start = index
            break
    if start is None:
        raise AssertionError(f"{script} 中未找到函数 {name}")
    # 单行函数（例如 escape() { ...; }）在起始行就已配平。
    if lines[start].count("{") - lines[start].count("}") <= 0:
        return lines[start]
    for end in range(start + 1, len(lines)):
        if lines[end] != "}":
            continue
        snippet = "\n".join(lines[start : end + 1])
        if subprocess.run([BASH, "-nc", snippet], capture_output=True).returncode == 0:
            return snippet
    raise AssertionError(f"函数 {name} 缺少结束括号")


def write_stub(directory: Path, name: str, body: str) -> Path:
    """写入 Python 桩脚本（记录 argv 并按 spec 回应），返回路径。"""
    path = directory / name
    if path.is_symlink() or path.exists():
        path.unlink()
    path.write_text(f"#!{python_executable()}\n{body}\n", encoding="utf-8")
    path.chmod(0o755)
    return path


CURL_STUB = r'''
import json, os, sys
spec = json.load(open(os.environ["STUB_SPEC"], encoding="utf-8"))
argv = sys.argv[1:]
record = {"tool": "curl", "argv": argv}
config_path = None
body_path = None
output_path = None
write_out = None
data_urlencode = []
for index, item in enumerate(argv):
    if item == "--config" and index + 1 < len(argv):
        config_path = argv[index + 1]
    if item.startswith("--data-binary") and index + 1 < len(argv):
        body_path = argv[index + 1]
    if item == "--data-urlencode" and index + 1 < len(argv):
        data_urlencode.append(argv[index + 1])
    if item in ("--output", "-o") and index + 1 < len(argv):
        output_path = argv[index + 1]
    if item == "--write-out" and index + 1 < len(argv):
        write_out = argv[index + 1]
if output_path:
    record["output_path"] = output_path
    record["output_outside_repo"] = not os.path.abspath(output_path).startswith(os.environ["REPO_ROOT"])
if config_path:
    record["config_path"] = config_path
    record["config_mode"] = oct(os.stat(config_path).st_mode & 0o777)
    record["config_body"] = open(config_path, encoding="utf-8").read()
    record["config_outside_repo"] = not os.path.abspath(config_path).startswith(os.environ["REPO_ROOT"])
if body_path and body_path.startswith("@"):
    payload = body_path[1:]
    record["payload_path"] = payload
    record["payload_outside_repo"] = not os.path.abspath(payload).startswith(os.environ["REPO_ROOT"])
    try:
        record["payload_mode"] = oct(os.stat(payload).st_mode & 0o777)
        record["payload_body"] = open(payload, encoding="utf-8").read()
    except OSError as exc:
        record["payload_error"] = str(exc)
if data_urlencode:
    record["data_urlencode"] = data_urlencode
urls = [item for item in argv if item.startswith("http://") or item.startswith("https://")]
record["url"] = urls[-1] if urls else ""
record["method"] = "PUT" if "PUT" in argv else "GET"
phase = "put" if record["method"] == "PUT" else "token"
plan = spec.get(phase, {})
status = str(int(plan.get("status", 200)))
write_out_text = (write_out or "").replace("%{http_code}", status)
if plan.get("fail"):
    body_text = plan.get("body", "")
    exit_code = int(plan.get("exit", 22))
    sys.stderr.write(plan.get("stderr", ""))
else:
    body_text = json.dumps(plan.get("response", {}), ensure_ascii=False)
    exit_code = 0
if output_path:
    try:
        with open(output_path, "w", encoding="utf-8") as handle:
            handle.write(body_text)
        record["output_body"] = body_text
        record["output_mode"] = oct(os.stat(output_path).st_mode & 0o777)
    except OSError as exc:
        record["output_error"] = str(exc)
with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, ensure_ascii=False) + "\n")
if output_path:
    if write_out_text:
        sys.stdout.write(write_out_text)
else:
    sys.stdout.write(body_text + write_out_text)
sys.exit(exit_code)
'''


DOCKER_STUB = r'''
import json, os, sys
spec = json.load(open(os.environ["STUB_SPEC"], encoding="utf-8"))
argv = sys.argv[1:]
sql = ""
for index, item in enumerate(argv):
    if item == "-e" and index + 1 < len(argv):
        candidate = argv[index + 1]
        if "SELECT" in candidate.upper():
            sql = candidate
record = {
    "tool": "docker",
    "argv": argv,
    "sql": sql,
    "mysql_pwd_env": os.environ.get("MYSQL_PWD", ""),
    "mysql_pwd_in_argv": "MYSQL_PWD" in " ".join(argv) and (spec.get("password", "") in " ".join(argv)),
}
with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, ensure_ascii=False) + "\n")
if spec.get("fail") or (spec.get("fail_sql_contains") and spec["fail_sql_contains"] in sql):
    sys.stderr.write("ERROR 1045 (28000): Access denied\n")
    sys.exit(int(spec.get("exit", 1)))
if "authz_role_permission" in sql:
    sys.stdout.write(spec.get("role_permissions", ""))
elif "JOIN authz_resource" in sql:
    sys.stdout.write(spec.get("permissions", ""))
elif "FROM authz_resource" in sql:
    sys.stdout.write(spec.get("resources", ""))
elif "FROM authz_role" in sql:
    sys.stdout.write(spec.get("roles", ""))
'''


MKTEMP_STUB = r'''
import json, os, sys, tempfile
fd, path = tempfile.mkstemp(prefix="catalog-sync-mktemp-")
os.close(fd)
with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps({"tool": "mktemp", "path": path}, ensure_ascii=False) + "\n")
sys.stdout.write(path + "\n")
'''


class CatalogHarness:
    """一个测试用的 sync-*-catalog.sh 运行环境：独立临时目录 + docker/curl 桩。"""

    def __init__(self, testcase: unittest.TestCase):
        """参数：testcase 用于注册清理回调。"""
        self._testcase = testcase
        self.tmp = Path(tempfile.mkdtemp(prefix="catalog-sync-test-"))
        testcase.addCleanup(self._cleanup)
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        self.tmpdir = self.tmp / "tmp"
        self.tmpdir.mkdir()
        self.log = self.tmp / "stub.jsonl"
        self.spec = self.tmp / "spec.json"
        self.manifest = self.tmp / "permission-manifest.json"
        write_stub(self.bin, "curl", CURL_STUB)
        write_stub(self.bin, "docker", DOCKER_STUB)
        write_stub(self.bin, "mktemp", MKTEMP_STUB)
        self.set_spec()

    def _cleanup(self):
        if os.environ.get("CATALOG_TEST_KEEP") == "1":
            print(f"\n[保留临时目录] {self.tmp}", file=sys.stderr)
            return
        shutil.rmtree(self.tmp, ignore_errors=True)

    def set_spec(self, **spec):
        """写入两个桩共享的行为规格（curl 的 token/put 响应、docker 的预置 TSV）。"""
        self.spec.write_text(json.dumps(spec, ensure_ascii=False), encoding="utf-8")
        return spec

    def env(self, **overrides) -> dict:
        """构造子进程环境：清空 PLATFORM_*/MYSQL 泄漏值，PATH 前置桩目录，TMPDIR 指向临时目录。"""
        env = {
            key: value
            for key, value in os.environ.items()
            if not key.startswith(("PLATFORM_", "CLAIMS_", "MYSQL_"))
        }
        env["PATH"] = str(self.bin) + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin")
        env["TMPDIR"] = str(self.tmpdir)
        # macOS 的 mktemp 走 confstr 临时目录（忽略 TMPDIR），因此断言「不落仓库」而不是「落在 TMPDIR」。
        env["REPO_ROOT"] = str(ROOT)
        env["STUB_SPEC"] = str(self.spec)
        env["STUB_LOG"] = str(self.log)
        env.update({key: str(value) for key, value in overrides.items()})
        return env

    def run(self, script: Path, args=(), timeout: int = 120, **overrides):
        """用已选定的 bash（默认 PATH 中的 bash，可被 CATALOG_TEST_BASH 覆盖）执行一次被测脚本。"""
        return subprocess.run(
            [BASH, str(script), *args],
            capture_output=True,
            text=True,
            errors="replace",
            env=self.env(**overrides),
            timeout=timeout,
        )

    def records(self, tool: str) -> list:
        """按工具有序返回桩记录。"""
        if not self.log.exists():
            return []
        result = []
        for line in self.log.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            record = json.loads(line)
            if record.get("tool") == tool:
                result.append(record)
        return result

    def token_records(self) -> list:
        return [record for record in self.records("curl") if record["method"] == "GET"]

    def put_records(self) -> list:
        return [record for record in self.records("curl") if record["method"] == "PUT"]

    def put_payload(self) -> dict:
        """解析 PUT 请求 body（--data-binary @file 的内容）。"""
        puts = self.put_records()
        if not puts:
            raise AssertionError("未记录到 PUT 请求")
        return json.loads(puts[0]["payload_body"])

    def temp_files_outside_repo(self) -> bool:
        """所有 curl --config / payload 文件都不在仓库目录内（凭据不落仓库文件）。"""
        for record in self.records("curl"):
            if record.get("config_path") and not record.get("config_outside_repo"):
                return False
            if record.get("payload_path") and not record.get("payload_outside_repo"):
                return False
        return True


class ManifestScriptMixin:
    """把脚本文本中的 /catalog/permission-manifest.json 字面量替换为临时文件路径的辅助。

    settlement / project 脚本把 manifest 固定在容器内绝对路径 /catalog/permission-manifest.json，
    测试环境无法创建该路径（也不应写根文件系统）。为覆盖 token/PUT 逻辑，这里只在临时副本里
    替换这一个字面量；生产脚本本身不做任何修改。若脚本改为参数化/环境变量化，替换会失败并
    skip，提示需要更新测试。
    """

    def script_with_manifest(self, harness: CatalogHarness, source: Path, manifest: Path) -> Path:
        text = source.read_text(encoding="utf-8")
        if MANIFEST_PATH_LITERAL not in text:
            self.skipTest(f"{source.name} 中已不存在 {MANIFEST_PATH_LITERAL} 字面量，请更新测试的副本替换逻辑")
        target = harness.tmp / f"{source.stem}-undertest.sh"
        target.write_text(text.replace(MANIFEST_PATH_LITERAL, str(manifest)), encoding="utf-8")
        return target

    def write_manifest(self, harness: CatalogHarness, payload: dict) -> Path:
        harness.manifest.write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")
        return harness.manifest


class CurlEscapeFunctionTest(unittest.TestCase):
    """三个脚本各自的 curl 配置转义函数边界（提取源码执行，不复制实现）。"""

    def _escape(self, script: Path, name: str, value: str) -> str:
        snippet = extract_function(script, name)
        return subprocess.run(
            [BASH, "-c", snippet + '\n' + name + ' "$1"', "snippet", value],
            capture_output=True,
            text=True,
            errors="replace",
            timeout=60,
        ).stdout

    def test_contract_escape_handles_quote_and_backslash(self):
        """curl_config_escape 必须转义反斜杠与双引号；空值原样输出。"""
        self.assertEqual('a\\"b\\\\c', self._escape(CONTRACT, "curl_config_escape", 'a"b\\c'))
        self.assertEqual("", self._escape(CONTRACT, "curl_config_escape", ""))
        self.assertEqual("plain-secret", self._escape(CONTRACT, "curl_config_escape", "plain-secret"))

    def test_settlement_escape_handles_quote_and_backslash(self):
        """sync-settlement-catalog.sh 的 escape 与 contract 实现保持一致。"""
        self.assertEqual('x\\"y\\\\z', self._escape(SETTLEMENT, "escape", 'x"y\\z'))

    def test_project_escape_handles_quote_and_backslash(self):
        """sync-project-catalog.sh 的 escape 同样必须转义引号与反斜杠。"""
        self.assertEqual('x\\"y\\\\z', self._escape(PROJECT, "escape", 'x"y\\z'))

    def test_escaped_values_stay_valid_curl_config_syntax(self):
        """转义后的 user/header 行必须仍是单行 curl --config 项（引号成对）。"""
        escaped = self._escape(CONTRACT, "curl_config_escape", 'id"with\\quotes')
        line = f'user = "{escaped}:{escaped}"\n'
        self.assertEqual(1, line.count("\n"))
        self.assertEqual(4, line.count('"'))
        self.assertNotIn('id"with', line)


@requires_jq
class ContractCatalogTest(unittest.TestCase):
    """sync-contract-catalog.sh：参数校验、token、MySQL 读取、payload 与退出码。"""

    def setUp(self):
        self.harness = CatalogHarness(self)
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "SYNCED", "roles": [{"code": "admin"}, {"code": "viewer"}]}}},
            password="db-password-value",
            resources="doc\t文档\ndoc-extra\t文档扩展\n",
            permissions=(
                "doc.read\t读取文档\tread\tLOW\tdoc\n"
                "doc.write\t写入文档\twrite\tMID\tdoc-extra\n"
                "ext.act\t外部动作\tact\tHIGH\tmissing-resource\n"
            ),
            role_permissions="admin\tdoc.read\nadmin\tdoc.write\nviewer\tdoc.read\n",
            roles="admin\t管理员\t全部权限\nviewer\t访客\t\n",
        )

    def _base_env(self, **overrides):
        env = {
            "PLATFORM_APPLICATION_ID": APP_ID,
            "PLATFORM_BASE_URL": ISSUER,
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID": CLIENT_ID,
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET": CLIENT_SECRET,
            "PLATFORM_MYSQL_PASSWORD": "db-password-value",
            "PLATFORM_AUTHORIZATION_CATALOG_VERSION": "v9-test",
            "CLAIMS_ROLE_CONFIG_HASH": "hash-abc",
        }
        env.update(overrides)
        return env

    def test_rejects_cli_arguments(self):
        """脚本拒绝任何命令行参数（避免敏感值进入 argv/history）。"""
        result = self.harness.run(CONTRACT, args=["--token", "leak"])
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("command-line arguments are disabled", result.stderr)
        self.assertEqual([], self.harness.records("curl"))

    def test_rejects_missing_required_env(self):
        """缺少应用/凭据/数据库口令时退出 2，且不发任何外部请求。"""
        for missing in (
            "PLATFORM_APPLICATION_ID",
            "PLATFORM_BASE_URL",
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID",
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
            "PLATFORM_MYSQL_PASSWORD",
        ):
            with self.subTest(missing=missing):
                env = self._base_env()
                env.pop(missing)
                result = self.harness.run(CONTRACT, **env)
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn("ERROR", result.stderr)
                self.assertEqual([], self.harness.records("curl"))
                self.assertEqual([], self.harness.records("docker"))

    def test_rejects_non_ulid_application_id(self):
        """应用 ID 必须是 26 位大写 ULID（含排除字符校验）。"""
        for value in ("not-a-ulid", "01j2abcdefghjkmnpqrstvwxyz", "01J2ABCDEFGHJKMNPQRSTVWXYI"):
            with self.subTest(app_id=value):
                result = self.harness.run(CONTRACT, **self._base_env(PLATFORM_APPLICATION_ID=value))
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn("uppercase ULID", result.stderr)

    def test_rejects_base_url_with_path(self):
        """PLATFORM_BASE_URL 带路径/查询时退出 2（原样拼接会产生错误端点）。"""
        for value in ("http://h/api", "http://h?x=1", "h:8080"):
            with self.subTest(base_url=value):
                result = self.harness.run(CONTRACT, **self._base_env(PLATFORM_BASE_URL=value))
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn("absolute HTTP(S) URL", result.stderr)
                self.assertEqual([], self.harness.records("curl"))

    def test_token_endpoint_failure_exits_three(self):
        """token 端点不可达（curl 失败）时退出 3，不读库、不发 PUT。"""
        self.harness.set_spec(token={"fail": True, "exit": 22, "body": ""})
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("token endpoint unreachable", result.stderr)
        self.assertEqual([], self.harness.records("docker"))
        self.assertEqual([], self.harness.put_records())

    def test_token_response_without_access_token_reports_error(self):
        """token 响应缺少 access_token 时退出 3 并透出 error/error_description。"""
        self.harness.set_spec(token={"response": {"error": "invalid_client", "error_description": "bad secret"}})
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("token response missing access_token", result.stderr)
        self.assertIn("invalid_client: bad secret", result.stderr)

    def test_token_request_parameters_and_private_config(self):
        """token 请求使用 client_credentials + 指定 scope，凭据只出现在 0600 的 --config 文件。"""
        result = self.harness.run(CONTRACT, **self._base_env())
        tokens = self.harness.token_records()
        self.assertTrue(tokens, f"未记录到 token 请求；stderr={result.stderr}")
        record = tokens[0]
        self.assertEqual(f"{ISSUER}/oauth2/token", record["url"])
        self.assertIn("grant_type=client_credentials", record["data_urlencode"])
        self.assertIn("scope=authorization.catalog.sync", record["data_urlencode"])
        self.assertEqual("0o600", record["config_mode"])
        self.assertIn("silent", record["config_body"])
        self.assertIn("show-error", record["config_body"])
        self.assertIn("fail-with-body", record["config_body"])
        self.assertIn(f'user = "{CLIENT_ID}:{curl_escape(CLIENT_SECRET)}"', record["config_body"])
        # 凭据不得进入 argv 或输出流。
        self.assertNotIn(CLIENT_SECRET, " ".join(record["argv"]))
        self.assertNotIn(CLIENT_SECRET, result.stdout + result.stderr)
        self.assertTrue(self.harness.temp_files_outside_repo())

    def test_contract_script_stays_bash32_compatible(self):
        """脚本必须保持 bash 3.2 兼容：镜像与 macOS 自带 bash 都可能只有 3.2。"""
        text = CONTRACT.read_text(encoding="utf-8")
        for construct in ("declare -A", "local -A", "typeset -A", "mapfile", "readarray", "coproc", "&>>"):
            self.assertNotIn(construct, text, f"{CONTRACT.name} 含 bash 4 专有语法：{construct}")
        self.assertNotRegex(text, r"\$\{[A-Za-z_][A-Za-z0-9_]*(\[[^\]]*\])?(\^\^|,,)", "不得使用 bash 4 的大小写转换展开")
        self.assertNotIn("|&", text, "|& 是 bash 4 语法")

    def test_requires_jq_curl_and_awk(self):
        """脚本自带前置依赖校验：缺 jq/curl/awk 时退出 2 并明确报错（不静默降级）。"""
        empty_bin = self.harness.tmp / "empty-bin"
        empty_bin.mkdir()
        result = self.harness.run(CONTRACT, **self._base_env(PATH=str(empty_bin)))
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("jq is required", result.stderr)

        if not HAS_JQ:
            return
        jq_only = self.harness.tmp / "jq-only-bin"
        jq_only.mkdir()
        (jq_only / "jq").symlink_to(shutil.which("jq"))
        result = self.harness.run(CONTRACT, **self._base_env(PATH=str(jq_only)))
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("curl is required", result.stderr)

        jq_curl = self.harness.tmp / "jq-curl-bin"
        jq_curl.mkdir()
        (jq_curl / "jq").symlink_to(shutil.which("jq"))
        (jq_curl / "curl").symlink_to(shutil.which("curl"))
        result = self.harness.run(CONTRACT, **self._base_env(PATH=str(jq_curl)))
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("awk is required", result.stderr)

    def test_happy_path_publishes_catalog(self):
        """正常路径：读库 -> 组装 payload -> PUT 200 + SYNCED -> 退出 0 并打印角色数。"""
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn(f"[contract-catalog-sync] OK: app={APP_ID} roles=2 version=v9-test", result.stdout)

        puts = self.harness.put_records()
        self.assertEqual(1, len(puts), "应只发一次 PUT")
        record = puts[0]
        self.assertEqual(f"{ISSUER}/api/v1/applications/{APP_ID}/authorization-catalog", record["url"])
        self.assertEqual("PUT", record["method"])
        self.assertIn("Content-Type: application/json", record["argv"])
        self.assertIn(f"Origin: {ISSUER}", record["argv"])
        self.assertIn("--max-time", record["argv"])
        self.assertEqual("0o600", record["payload_mode"])
        self.assertEqual("0o600", record["config_mode"])
        self.assertIn('header = "Authorization: Bearer ' + curl_escape(ACCESS_TOKEN) + '"', record["config_body"])
        self.assertNotIn(ACCESS_TOKEN, " ".join(record["argv"]))
        self.assertNotIn(ACCESS_TOKEN, result.stdout + result.stderr)
        self.assertTrue(self.harness.temp_files_outside_repo())

        payload = json.loads(record["payload_body"])
        self.assertEqual("v9-test", payload["catalog_version"])
        self.assertEqual("hash-abc", payload["claims_role_config_hash"])
        self.assertEqual({"max_effective_roles": 8}, payload["policy"])
        self.assertEqual(
            [
                {
                    "code": "doc.read",
                    "name": "读取文档",
                    "action": "read",
                    "resource_code": "doc",
                    "resource_name": "文档",
                    "risk_level": "LOW",
                },
                {
                    "code": "doc.write",
                    "name": "写入文档",
                    "action": "write",
                    "resource_code": "doc-extra",
                    "resource_name": "文档扩展",
                    "risk_level": "MID",
                },
                {
                    "code": "ext.act",
                    "name": "外部动作",
                    "action": "act",
                    "resource_code": "missing-resource",
                    # 资源表没有该 code 时用资源 code 自身兜底，避免空名字。
                    "resource_name": "missing-resource",
                    "risk_level": "HIGH",
                },
            ],
            payload["permissions"],
        )
        self.assertEqual(
            [
                {"code": "admin", "name": "管理员", "description": "全部权限", "permissions": ["doc.read", "doc.write"]},
                {"code": "viewer", "name": "访客", "description": "", "permissions": ["doc.read"]},
            ],
            payload["roles"],
        )

    def test_payload_bytes_match_pre_fix_output(self):
        """payload 必须与修复前（关联数组实现）逐字节一致：字段顺序/紧凑格式/UTF-8/结尾换行。

        期望值取自修复前的 sync-contract-catalog.sh（git HEAD）在 bash 5 + jq 1.7 下对同一
        fixture 生成的真实 PUT body；bash 3.2 的修复实现必须复现同样的字节。
        """
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(0, result.returncode, result.stderr)
        expected = (
            '{"catalog_version":"v9-test","permissions":['
            '{"code":"doc.read","name":"读取文档","action":"read","resource_code":"doc","resource_name":"文档","risk_level":"LOW"},'
            '{"code":"doc.write","name":"写入文档","action":"write","resource_code":"doc-extra","resource_name":"文档扩展","risk_level":"MID"},'
            '{"code":"ext.act","name":"外部动作","action":"act","resource_code":"missing-resource","resource_name":"missing-resource","risk_level":"HIGH"}'
            '],"roles":['
            '{"code":"admin","name":"管理员","description":"全部权限","permissions":["doc.read","doc.write"]},'
            '{"code":"viewer","name":"访客","description":"","permissions":["doc.read"]}'
            '],"policy":{"max_effective_roles":8},"claims_role_config_hash":"hash-abc"}\n'
        )
        puts = self.harness.put_records()
        self.assertEqual(1, len(puts))
        self.assertEqual(expected, puts[0]["payload_body"])

    def test_mysql_queries_scope_by_application_and_hide_password(self):
        """四条查询都必须带 application_id 过滤；口令通过 MYSQL_PWD 环境转发而非 argv。"""
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(0, result.returncode, result.stderr)
        queries = self.harness.records("docker")
        self.assertEqual(4, len(queries), [record["sql"] for record in queries])
        for record in queries:
            self.assertIn(f"application_id='{APP_ID}'", record["sql"])
            self.assertIn("MYSQL_PWD", record["argv"])
            self.assertNotIn("db-password-value", " ".join(record["argv"]))
            self.assertEqual("db-password-value", record["mysql_pwd_env"])

    def test_catalog_version_auto_uses_utc_timestamp(self):
        """未指定版本时使用 v1-<UTC 14 位时间戳>，保证每次同步产生新版本号。"""
        result = self.harness.run(
            CONTRACT, **self._base_env(PLATFORM_AUTHORIZATION_CATALOG_VERSION="auto")
        )
        self.assertEqual(0, result.returncode, result.stderr)
        payload = self.harness.put_payload()
        self.assertRegex(payload["catalog_version"], r"^v1-\d{14}$")

    def test_empty_database_result_publishes_empty_arrays(self):
        """库表为空时约定为「发布空目录」而不是报错：permissions/roles 均为 []。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "SYNCED", "roles": []}}},
            password="db-password-value",
            resources="",
            permissions="",
            role_permissions="",
            roles="",
        )
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(0, result.returncode, result.stderr)
        payload = self.harness.put_payload()
        self.assertEqual([], payload["permissions"])
        self.assertEqual([], payload["roles"])
        self.assertIn("roles=0", result.stdout)

    def test_put_failure_exits_four_with_response_body(self):
        """PUT 非 2xx 时退出 4，并把响应体留在 stderr 便于诊断（fail-with-body 语义）。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"fail": True, "exit": 22, "body": '{"code":"AUTH_FORBIDDEN","message":"denied"}'},
            password="db-password-value",
        )
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(4, result.returncode, result.stderr)
        self.assertIn("PUT failed", result.stderr)
        self.assertIn("AUTH_FORBIDDEN", result.stderr)

    def test_unsynced_status_exits_five(self):
        """平台未确认 SYNCED 时退出 5 并输出 sync_status 与响应体。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "PENDING"}}},
            password="db-password-value",
            resources="",
            permissions="",
            role_permissions="",
            roles="",
        )
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(5, result.returncode, result.stderr)
        self.assertIn("WARN: sync_status=PENDING", result.stderr)
        self.assertIn("PENDING", result.stderr)

    def test_mysql_failure_aborts_without_partial_publish(self):
        """docker exec/mysql 失败时：退出码 6、stderr 可诊断、不发 PUT、不伪造成功输出。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            fail=True,
            exit=1,
        )
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(6, result.returncode, result.stderr)
        self.assertIn("[contract-catalog-sync] ERROR: step 2 (resources)", result.stderr)
        self.assertIn("MySQL container basic-platform-local-mysql-1", result.stderr)
        self.assertIn("failed (exit 1)", result.stderr)
        self.assertIn("Access denied", result.stderr)
        self.assertNotIn("OK:", result.stdout)
        self.assertNotIn("db-password-value", result.stdout + result.stderr)
        self.assertEqual([], self.harness.put_records(), "读库失败后不得发布半残目录")

    def test_mysql_failure_in_later_query_does_not_publish_stale_reads(self):
        """第 4 条查询（roles）失败时同样中止：不得用前三条的旧结果拼 payload 发 PUT。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            fail_sql_contains="FROM authz_role WHERE",
            exit=2,
        )
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(6, result.returncode, result.stderr)
        self.assertIn("step 2 (roles)", result.stderr)
        self.assertIn("failed (exit 2)", result.stderr)
        self.assertNotIn("OK:", result.stdout)
        self.assertEqual(4, len(self.harness.records("docker")), "前三条查询成功、第四条失败")
        self.assertEqual([], self.harness.put_records(), "不得用部分读取结果发布")

    def test_temp_credential_files_are_cleaned_up(self):
        """退出时删除全部 0600 临时文件（curl 配置、payload、DB 映射、mysql stderr）。"""
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(0, result.returncode, result.stderr)
        seen = 0
        for record in self.harness.records("curl"):
            for key in ("config_path", "payload_path"):
                path = record.get(key)
                if path:
                    seen += 1
                    self.assertFalse(Path(path).exists(), f"临时文件未清理：{path}")
        self.assertGreater(seen, 0, "没有记录到任何临时文件路径")
        mktemps = self.harness.records("mktemp")
        self.assertEqual(
            5,
            len(mktemps),
            f"预期 5 个临时文件（config/mysql stderr/resources/role perms/payload），实际 {len(mktemps)}",
        )
        for record in mktemps:
            self.assertFalse(Path(record["path"]).exists(), f"临时文件未清理：{record['path']}")

    def test_temp_files_are_cleaned_up_after_mysql_failure(self):
        """读库失败退出时同样清理已创建的临时文件（含 mysql stderr 文件）。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            fail=True,
            exit=1,
        )
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(6, result.returncode, result.stderr)
        mktemps = self.harness.records("mktemp")
        self.assertEqual(2, len(mktemps), "失败发生在映射文件创建前，应只有 config 与 mysql stderr 两个临时文件")
        for record in mktemps:
            self.assertFalse(Path(record["path"]).exists(), f"临时文件未清理：{record['path']}")

    def test_temp_config_cleaned_up_after_token_failure(self):
        """token 阶段失败时也清理 0600 的 curl 配置临时文件。"""
        self.harness.set_spec(token={"fail": True, "exit": 22})
        result = self.harness.run(CONTRACT, **self._base_env())
        self.assertEqual(3, result.returncode, result.stderr)
        mktemps = self.harness.records("mktemp")
        self.assertEqual(1, len(mktemps))
        self.assertFalse(Path(mktemps[0]["path"]).exists(), "token 失败后 curl 配置临时文件未清理")


@requires_jq
class SettlementCatalogTest(ManifestScriptMixin, unittest.TestCase):
    """sync-settlement-catalog.sh：固定 manifest 路径、token、PUT 与退出码。"""

    MANIFEST = {"catalog_version": "v1-settlement", "permissions": [], "roles": [{"code": "settlement"}]}

    def setUp(self):
        self.harness = CatalogHarness(self)
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "SYNCED"}}},
        )
        self.manifest = self.write_manifest(self.harness, self.MANIFEST)
        self.script = self.script_with_manifest(self.harness, SETTLEMENT, self.manifest)

    def _env(self, **overrides):
        env = {
            "PLATFORM_APPLICATION_ID": APP_ID,
            "PLATFORM_BASE_URL": ISSUER,
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID": CLIENT_ID,
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET": CLIENT_SECRET,
        }
        env.update(overrides)
        return env

    def test_missing_env_names_each_missing_variable(self):
        """缺必需环境变量时退出 2，文案逐个指名缺失变量，且不发 token/PUT 请求。"""
        expected = (
            "PLATFORM_APPLICATION_ID",
            "PLATFORM_BASE_URL",
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID",
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
        )
        # 全部缺失：一条文案列出四个变量名，且不混淆成 manifest 问题。
        result = self.harness.run(SETTLEMENT, **{})
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("required environment variables not set", result.stderr)
        for name in expected:
            self.assertIn(name, result.stderr)
        self.assertNotIn("manifest unavailable", result.stderr)
        self.assertEqual([], self.harness.records("curl"))
        self.assertEqual([], self.harness.put_records())

        # 逐个缺失：只指名真正缺失的那个变量，不牵连其他变量。
        for missing in expected:
            with self.subTest(missing=missing):
                env = self._env()
                env.pop(missing)
                result = self.harness.run(SETTLEMENT, **env)
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn(f"required environment variables not set: {missing}", result.stderr)
                for other in expected:
                    if other == missing:
                        continue
                    self.assertNotIn(other, result.stderr, f"未缺失的 {other} 不应出现在文案里")
                self.assertEqual([], self.harness.records("curl"), "缺 env 时不得发起任何请求")
                self.assertEqual([], self.harness.put_records())

    def test_unavailable_manifest_reports_path_and_differs_from_env_error(self):
        """manifest 不可读时退出 2，文案含具体路径，且与「缺 env」文案可区分。"""
        if Path(MANIFEST_PATH_LITERAL).exists():
            self.skipTest("本机存在 /catalog/permission-manifest.json，无法验证缺失分支")
        result = self.harness.run(SETTLEMENT, **self._env())
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("manifest unavailable", result.stderr)
        self.assertIn(MANIFEST_PATH_LITERAL, result.stderr)
        self.assertNotIn("required environment variables", result.stderr)
        self.assertEqual([], self.harness.records("curl"))
        self.assertEqual([], self.harness.put_records())

    def test_manifest_path_that_is_not_a_regular_file(self):
        """路径存在但不是常规文件（例如挂载出错变成目录）：退出 2 并打印该路径。"""
        directory = self.harness.tmp / "manifest-as-directory"
        directory.mkdir()
        script = self.script_with_manifest(self.harness, SETTLEMENT, directory)
        result = self.harness.run(script, **self._env())
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("manifest unavailable", result.stderr)
        self.assertIn(str(directory), result.stderr)
        self.assertNotIn("required environment variables", result.stderr)
        self.assertEqual([], self.harness.records("curl"))

    def test_invalid_manifest_json_is_reported_separately(self):
        """manifest 存在且可读但内容不是合法 JSON：第三类文案 + 非零退出，不发请求。"""
        broken = self.harness.tmp / "invalid-manifest.json"
        broken.write_text('{"catalog_version": ', encoding="utf-8")
        script = self.script_with_manifest(self.harness, SETTLEMENT, broken)
        result = self.harness.run(script, **self._env())
        self.assertNotEqual(0, result.returncode, result.stderr)
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("not valid JSON", result.stderr)
        self.assertIn(str(broken), result.stderr)
        self.assertNotIn("manifest unavailable", result.stderr)
        self.assertNotIn("required environment variables", result.stderr)
        self.assertEqual([], self.harness.records("curl"))

    def test_publishes_manifest_as_put_body(self):
        """正常路径：manifest 原样作为 PUT body，URL/方法/鉴权头符合契约。"""
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("[settlement-catalog-sync] OK: catalog published", result.stdout)
        puts = self.harness.put_records()
        self.assertEqual(1, len(puts))
        record = puts[0]
        self.assertEqual(f"{ISSUER}/api/v1/applications/{APP_ID}/authorization-catalog", record["url"])
        self.assertEqual("PUT", record["method"])
        self.assertIn("Content-Type: application/json", record["argv"])
        self.assertEqual(str(self.manifest), record["payload_path"], "body 应为 manifest 文件本身")
        self.assertEqual(json.dumps(self.MANIFEST, ensure_ascii=False), record["payload_body"])
        self.assertIn(f'header = "Authorization: Bearer {curl_escape(ACCESS_TOKEN)}"', record["config_body"])
        self.assertEqual("0o600", record["config_mode"])
        self.assertNotIn(ACCESS_TOKEN, " ".join(record["argv"]))
        self.assertNotIn(CLIENT_SECRET, result.stdout + result.stderr)
        self.assertTrue(self.harness.temp_files_outside_repo())

    def test_token_failure_exits_three(self):
        """token 请求失败时退出 3 且不发 PUT。"""
        self.harness.set_spec(token={"fail": True, "exit": 22})
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("catalog publisher token request failed", result.stderr)
        self.assertEqual([], self.harness.put_records())

    def test_missing_access_token_exits_three(self):
        """token 响应没有 access_token 时退出 3 并说明原因。"""
        self.harness.set_spec(token={"response": {"error": "invalid_client"}})
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("token response did not include access_token", result.stderr)

    def test_put_failure_exits_four(self):
        """PUT 非 2xx 时退出 4。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"fail": True, "exit": 22, "body": "{}"},
        )
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(4, result.returncode, result.stderr)
        self.assertIn("authorization catalog publication failed", result.stderr)

    def test_unsynced_status_exits_five(self):
        """平台未确认 SYNCED 时退出 5。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "PENDING"}}},
        )
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(5, result.returncode, result.stderr)
        self.assertIn("platform did not confirm catalog synchronization", result.stderr)


@requires_jq
class ProjectCatalogTest(ManifestScriptMixin, unittest.TestCase):
    """sync-project-catalog.sh：最小实现的校验、PUT 与失败传播。"""

    MANIFEST = {"catalog_version": "v1-project", "permissions": [{"code": "project.read"}], "roles": []}

    def setUp(self):
        self.harness = CatalogHarness(self)
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "SYNCED"}}},
        )
        self.manifest = self.write_manifest(self.harness, self.MANIFEST)
        self.script = self.script_with_manifest(self.harness, PROJECT, self.manifest)

    def _env(self, **overrides):
        env = {
            "PLATFORM_APPLICATION_ID": APP_ID,
            "PLATFORM_BASE_URL": ISSUER,
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID": CLIENT_ID,
            "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET": CLIENT_SECRET,
        }
        env.update(overrides)
        return env

    REQUIRED_ENV = (
        "PLATFORM_APPLICATION_ID",
        "PLATFORM_BASE_URL",
        "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID",
        "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
    )

    def assert_no_requests_or_tempfiles(self, result):
        """缺 env 时必须零请求、零临时文件：预检发生在任何 curl/mktemp 之前。"""
        self.assertEqual([], self.harness.records("curl"), "缺 env 时不得发起任何请求")
        self.assertEqual([], self.harness.put_records(), "缺 env 时不得发起 PUT")
        self.assertEqual([], self.harness.records("mktemp"), "缺 env 时不得创建临时文件")
        self.assertEqual(
            [],
            sorted(path.name for path in self.harness.tmpdir.iterdir()),
            "缺 env 时临时目录应保持为空（无残留）",
        )

    def test_missing_env_names_each_missing_variable(self):
        """缺必需环境变量时退出 2，文案逐个指名缺失变量，且不发 token/PUT、不留临时文件。"""
        # 全部缺失：一条文案列出四个变量名，且不混淆成 manifest 问题。
        result = self.harness.run(PROJECT, **{})
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("[project-catalog-sync] ERROR: required environment variables not set", result.stderr)
        for name in self.REQUIRED_ENV:
            self.assertIn(name, result.stderr)
        self.assertNotIn("manifest unavailable", result.stderr, "缺 env 与 manifest 不可用必须是不同文案")
        self.assertNotIn("not valid JSON", result.stderr)
        self.assertNotIn("OK: catalog published", result.stdout)
        self.assert_no_requests_or_tempfiles(result)

        # 逐个缺失：只指名真正缺失的那个变量，不牵连其他变量。
        for missing in self.REQUIRED_ENV:
            with self.subTest(missing=missing):
                env = self._env()
                env.pop(missing)
                result = self.harness.run(PROJECT, **env)
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn(f"required environment variables not set: {missing}", result.stderr)
                for other in self.REQUIRED_ENV:
                    if other == missing:
                        continue
                    self.assertNotIn(other, result.stderr, f"未缺失的 {other} 不应出现在文案里")
                self.assertNotIn("manifest unavailable", result.stderr)
                self.assert_no_requests_or_tempfiles(result)

    def test_missing_multiple_env_variables_are_all_named(self):
        """同时缺多个变量时按声明顺序一并列出（不再只报第一个，也不重复改名）。"""
        env = self._env()
        env.pop("PLATFORM_APPLICATION_ID")
        env.pop("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET")
        result = self.harness.run(PROJECT, **env)
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn(
            "required environment variables not set: "
            "PLATFORM_APPLICATION_ID, PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
            result.stderr,
        )
        self.assertNotIn("PLATFORM_BASE_URL", result.stderr)
        self.assertNotIn("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID", result.stderr)
        self.assert_no_requests_or_tempfiles(result)

    def test_success_path_request_shape_is_unchanged(self):
        """成功路径的 curl 参数序列（开关/顺序/取值）与改动前逐项一致，payload 仍是 manifest 本身。"""
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual("[project-catalog-sync] OK: catalog published\n", result.stdout)
        tokens = self.harness.token_records()
        puts = self.harness.put_records()
        self.assertEqual(1, len(tokens), "应只有一次 token 请求")
        self.assertEqual(1, len(puts), "应只有一次 PUT")
        config_path = tokens[0]["config_path"]
        self.assertEqual(config_path, puts[0]["config_path"], "token 与 PUT 复用同一个 0600 配置临时文件")
        self.assertEqual(
            [
                "--config",
                config_path,
                "--data-urlencode",
                "grant_type=client_credentials",
                "--data-urlencode",
                "scope=authorization.catalog.sync",
                "-H",
                "Content-Type: application/x-www-form-urlencoded",
                "--max-time",
                "15",
                f"{ISSUER}/oauth2/token",
            ],
            tokens[0]["argv"],
        )
        self.assertEqual(
            [
                "--config",
                config_path,
                "-H",
                "Content-Type: application/json",
                "--data-binary",
                f"@{self.manifest}",
                "--max-time",
                "20",
                "-X",
                "PUT",
                "--output",
                puts[0]["output_path"],
                "--write-out",
                "%{http_code}",
                f"{ISSUER}/api/v1/applications/{APP_ID}/authorization-catalog",
            ],
            puts[0]["argv"],
        )
        self.assertEqual(json.dumps(self.MANIFEST, ensure_ascii=False), puts[0]["payload_body"])

    def test_original_script_requires_manifest(self):
        """manifest 不可读时退出 2，文案带具体路径，且与缺 env 的报错区分开。"""
        if Path(MANIFEST_PATH_LITERAL).exists():
            self.skipTest("本机存在 /catalog/permission-manifest.json，无法验证缺失分支")
        result = self.harness.run(PROJECT, **self._env())
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("manifest unavailable", result.stderr)
        self.assertIn(MANIFEST_PATH_LITERAL, result.stderr)
        self.assertEqual([], self.harness.records("curl"))
        self.assertEqual([], self.harness.put_records())

    def test_manifest_path_that_is_not_a_regular_file(self):
        """路径存在但不是常规文件：退出 2 并回显该路径（区分挂载问题与清单损坏）。"""
        directory = self.harness.tmp / "manifest-as-directory"
        directory.mkdir()
        script = self.script_with_manifest(self.harness, PROJECT, directory)
        result = self.harness.run(script, **self._env())
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("manifest unavailable", result.stderr)
        self.assertIn(str(directory), result.stderr)
        self.assertNotIn("not valid JSON", result.stderr)
        self.assertEqual([], self.harness.records("curl"))

    def test_invalid_manifest_json_exits_two(self):
        """manifest 可读但不是合法 JSON：退出 2、单独文案，且不发起 token/PUT。"""
        broken = self.harness.tmp / "invalid-manifest.json"
        broken.write_text('{"catalog_version": ', encoding="utf-8")
        script = self.script_with_manifest(self.harness, PROJECT, broken)
        result = self.harness.run(script, **self._env())
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("not valid JSON", result.stderr)
        self.assertIn(str(broken), result.stderr)
        self.assertNotIn("manifest unavailable", result.stderr)
        self.assertEqual([], self.harness.records("curl"))

    def test_publishes_manifest_with_put(self):
        """正常路径：token -> PUT manifest -> jq 校验 SYNCED -> 退出 0。"""
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("[project-catalog-sync] OK: catalog published", result.stdout)
        puts = self.harness.put_records()
        self.assertEqual(1, len(puts))
        record = puts[0]
        self.assertEqual(f"{ISSUER}/api/v1/applications/{APP_ID}/authorization-catalog", record["url"])
        self.assertEqual("PUT", record["method"])
        self.assertEqual(str(self.manifest), record["payload_path"], "body 应为 manifest 文件本身")
        self.assertEqual(json.dumps(self.MANIFEST, ensure_ascii=False), record["payload_body"])
        self.assertIn(f'header = "Authorization: Bearer {curl_escape(ACCESS_TOKEN)}"', record["config_body"])
        self.assertNotIn(ACCESS_TOKEN, " ".join(record["argv"]))
        self.assertNotIn(ACCESS_TOKEN, result.stdout + result.stderr)
        self.assertNotIn(CLIENT_SECRET, result.stdout + result.stderr)
        self.assertTrue(self.harness.temp_files_outside_repo())
        # PUT 响应落在 0600 临时文件（仓库外），且退出后清理。
        self.assertEqual("0o600", record["output_mode"])
        self.assertTrue(record["output_outside_repo"])
        self.assertFalse(Path(record["output_path"]).exists(), "PUT 响应临时文件未清理")

    def test_unsynced_status_fails(self):
        """平台返回非 SYNCED 时脚本非零退出（沿用原退出码 1）、不打印 OK 且说明原因。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"response": {"data": {"sync_status": "PENDING"}}},
        )
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertNotIn("OK: catalog published", result.stdout)
        self.assertIn("platform did not confirm catalog synchronization", result.stderr)
        self.assertIn("PENDING", result.stderr)

    def test_token_request_failure_exits_three(self):
        """token 请求非 2xx/不可达：明确文案 + 退出 3，不泄漏凭据、不发 PUT。"""
        self.harness.set_spec(token={"fail": True, "exit": 22, "status": 502, "body": '{"error":"bad_gateway"}'})
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("catalog publisher token request failed", result.stderr)
        self.assertEqual(1, len(self.harness.token_records()))
        self.assertEqual([], self.harness.put_records())
        self.assertNotIn(CLIENT_SECRET, result.stdout + result.stderr)
        self.assertNotIn(ACCESS_TOKEN, result.stdout + result.stderr)

    def test_missing_access_token_exits_three(self):
        """token 响应没有 access_token 时退出 3，指出响应缺字段。"""
        self.harness.set_spec(token={"response": {"error": "invalid_client"}})
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(3, result.returncode, result.stderr)
        self.assertIn("token response did not include access_token", result.stderr)
        self.assertNotIn("OK: catalog published", result.stdout)
        self.assertEqual([], self.harness.put_records())

    def test_put_failure_reports_status_and_body(self):
        """PUT 非 2xx：退出 4，stderr 含状态码与服务端响应体片段，且不泄漏 token/密码。"""
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={
                "fail": True,
                "exit": 22,
                "status": 403,
                "body": '{"code":"AUTH_FORBIDDEN","message":"denied"}',
            },
        )
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(4, result.returncode, result.stderr)
        self.assertIn("PUT failed", result.stderr)
        self.assertIn("403", result.stderr)
        self.assertIn("AUTH_FORBIDDEN", result.stderr)
        self.assertIn("denied", result.stderr)
        self.assertNotIn("OK: catalog published", result.stdout)
        self.assertNotIn(ACCESS_TOKEN, result.stdout + result.stderr)
        self.assertNotIn(CLIENT_SECRET, result.stdout + result.stderr)

    def test_put_failure_body_is_truncated(self):
        """PUT 失败回显的响应体截断到 500 字符，避免把大响应灌进日志。"""
        body = '{"code":"TOO_LONG","message":"' + "A" * 1200 + '"}'
        self.harness.set_spec(
            token={"response": {"access_token": ACCESS_TOKEN}},
            put={"fail": True, "exit": 22, "status": 500, "body": body},
        )
        result = self.harness.run(self.script, **self._env())
        self.assertEqual(4, result.returncode, result.stderr)
        self.assertIn("500", result.stderr)
        self.assertIn("TOO_LONG", result.stderr)
        echoed = result.stderr.count("A")
        self.assertLessEqual(echoed, 500, f"响应体回显超过 500 字符：{echoed}")
        self.assertGreater(echoed, 400, "应回显合理的响应体片段，而不是空")


if __name__ == "__main__":
    unittest.main(verbosity=2)
