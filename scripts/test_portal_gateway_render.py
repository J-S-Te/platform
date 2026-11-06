"""验证 scripts/portal-gateway.sh 与 scripts/lib/gateway_render.py 的渲染、解析、写锁与拒绝语义。

用途：在独立临时目录内驱动 portal-gateway.sh，对 nginx include 文件做精确块结构断言；
纯函数用例直接导入 gateway_render 模块做单元断言（不复制实现、不抽取 shell 文本）。
参数：无；可选环境变量 PORTAL_GATEWAY_TEST_KEEP=1 保留临时目录便于排查。
返回值：进程退出码 0 表示全部用例通过，非 0 表示存在失败（unittest 约定）。

覆盖场景：
  * gateway_render 的纯函数行为：render_location（含逐字节 golden 契约）、parse_entry、
    escape_curl_config、code/path_prefix/upstream_url/page_limit 校验、内置前端编码归一化、
    path_prefix 去重、sync 按 code 升序渲染；
  * add 渲染结果、幂等、替换、路径冲突检测、默认 python3 解析；
  * remove 的块级删除、不存在入口的幂等语义、清空后仅保留托管头；
  * list 的列输出；
  * sync 的全量重建（curl 桩）、空清单、分页空页守卫（total>0 但 items 为空）、
    全部 application 被跳过/无可用 environment 时写空清单、重复 path_prefix、失败退出码；
  * 分页健壮性：以实际返回条数为准（total 少报仍翻到真实末页）、恰好整页后探到空页不报错、
    application 与 environment 两处空页自相矛盾都必须拒绝写入 include；
  * 依赖面：sync 在 PATH 中没有 jq 时仍可工作（渲染与解析已迁到 Python，禁止回退到 jq）；
  * flock 锁路径与 macOS mkdir 目录锁回退路径的超时语义、并发串行化；
  * 非法 code / path_prefix / upstream_url / 超时参数必须拒绝且不写 include 文件；
  * reload 不再执行环境变量命令串（SEC-F10）并强制先 nginx -t。

隔离性：不接触真实 docker daemon、nginx、平台 API 或网络；所有命令来自
PATH 桩目录（桩把 argv 写入临时日志），TMPDIR 指向临时目录，不写仓库内文件。
"""
from __future__ import annotations

import atexit
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "portal-gateway.sh"
LIB_DIR = ROOT / "scripts" / "lib"
if str(LIB_DIR) not in sys.path:
    # 纯函数用例直接导入被测模块；插入顺序保证优先使用仓库内的实现。
    sys.path.insert(0, str(LIB_DIR))
import gateway_render  # noqa: E402  （需要先算出 ROOT/LIB_DIR 才能导入）

_FARM_CACHE = {}

# 脚本 render_location 的实际模板（严格按 nginx include 片段逐行断言）。
HEADER_ADD = (
    "# DO NOT EDIT: managed by scripts/portal-gateway.sh.\n"
    "# 任何手动修改会在下一次 add/remove/sync 时被覆盖。\n"
)
HEADER_SYNC = (
    "# DO NOT EDIT: managed by scripts/portal-gateway.sh sync.\n"
    "# 任何手动修改会在下一次 add/remove/sync 时被覆盖。\n"
)

# 渲染契约 golden：固定 code/prefix/upstream 的完整片段逐字节断言，防止模板漂移。
GOLDEN_RENDER = """
# redirect code=demo
location = /demo/api {
    return 308 /demo/api/;
}

# code=demo
location /demo/api/ {
    proxy_pass http://10.0.0.5:8080/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-Prefix /demo/api;
}
"""


def render_block(code: str, prefix: str, upstream: str) -> str:
    """返回 CLI 级用例期望的 include 片段（含前导空行）。

    这是测试侧独立维护的契约副本：纯函数侧另有 GOLDEN_RENDER 逐字节守护模板，
    两者同时失败才说明模板整体漂移；只改一侧不会互相掩盖。
    """
    prefix = prefix.rstrip("/")
    upstream = upstream.rstrip("/")
    return (
        "\n"
        f"# redirect code={code}\n"
        f"location = {prefix} {{\n"
        f"    return 308 {prefix}/;\n"
        "}\n"
        "\n"
        f"# code={code}\n"
        f"location {prefix}/ {{\n"
        f"    proxy_pass {upstream}/;\n"
        "    proxy_http_version 1.1;\n"
        "    proxy_set_header Host $host;\n"
        "    proxy_set_header X-Real-IP $remote_addr;\n"
        "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n"
        "    proxy_set_header X-Forwarded-Proto $scheme;\n"
        "    proxy_set_header X-Forwarded-Host $host;\n"
        f"    proxy_set_header X-Forwarded-Prefix {prefix};\n"
        "}\n"
    )


def bash_executable() -> str:
    """返回可用的 bash 路径。"""
    return shutil.which("bash") or "/bin/bash"


def write_stub(directory: Path, name: str, body: str, interpreter: str = None) -> Path:
    """在目录中写入可执行桩脚本并返回路径。

    参数：directory 桩目录；name 命令名；body 脚本正文；
    interpreter 显式 shebang 解释器（默认 /bin/sh）。
    说明：目录里可能已有同名系统命令软链（PATH 桩农场），必须先移除再写，
    否则会顺着软链改到系统二进制。
    """
    path = directory / name
    if path.is_symlink() or path.exists():
        path.unlink()
    path.write_text(f"#!{interpreter or '/bin/sh'}\n{body}\n", encoding="utf-8")
    path.chmod(0o755)
    return path


def shared_bin_farm(skipped_commands=("flock",)) -> Path:
    """返回进程级共享的系统命令软链目录（只读复用，避免每例重建）。

    参数：skipped_commands 不软链的命令名；默认排除 flock，用于稳定触发
    macOS mkdir 目录锁回退；sync 依赖面用例还会排除 jq。
    """
    key = tuple(sorted(skipped_commands))
    farm = _FARM_CACHE.get(key)
    if farm is None:
        farm = Path(tempfile.mkdtemp(prefix="portal-gateway-farm-"))
        atexit.register(shutil.rmtree, farm, ignore_errors=True)
        make_bin_farm(farm, skipped_commands=skipped_commands)
        _FARM_CACHE[key] = farm
    return farm


def make_bin_farm(destination: Path, skipped_commands=("flock",)) -> Path:
    """建立系统命令软链目录，用于稳定触发缺少 flock/jq 的运行环境。

    参数：destination 空目录；skipped_commands 不建立的命令名。
    返回值：该目录路径（可直接作为 PATH 的一部分）。
    """
    skipped = set(skipped_commands)
    for source_dir in ("/bin", "/usr/bin"):
        base = Path(source_dir)
        if not base.is_dir():
            continue
        for entry in base.iterdir():
            if entry.name in skipped or entry.name in {p.name for p in destination.iterdir()}:
                continue
            if entry.is_dir() or not os.access(entry, os.X_OK):
                continue
            try:
                (destination / entry.name).symlink_to(entry)
            except OSError:
                continue
    return destination


class GatewayHarness:
    """单个测试用的 portal-gateway.sh 运行环境：独立临时目录 + PATH 桩。"""

    def __init__(self, testcase: unittest.TestCase, path_mode: str = "fallback",
                 skipped_commands=("flock",)):
        """参数：testcase 用于注册清理；path_mode 为 "fallback"（无 flock）或 "host"（真实 PATH）；
        skipped_commands 为 fallback 农场里不软链的命令名。
        """
        self._testcase = testcase
        self.tmp = Path(tempfile.mkdtemp(prefix="portal-gateway-test-"))
        testcase.addCleanup(self._cleanup)
        self.include = self.tmp / "conf" / "portal-apps-locations.conf"
        self.lock = self.tmp / "conf" / "portal-apps-locations.conf.lock"
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        self.log = self.tmp / "stub.log"
        self.path_mode = path_mode
        self.skipped_commands = tuple(skipped_commands)
        self.path = self._build_path()

    def _cleanup(self):
        if os.environ.get("PORTAL_GATEWAY_TEST_KEEP") == "1":
            print(f"\n[保留临时目录] {self.tmp}", file=sys.stderr)
            return
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _build_path(self) -> str:
        if self.path_mode == "fallback":
            # 桩目录在前（可覆盖 curl/flock 等），共享农场在后提供系统命令。
            return str(self.bin) + os.pathsep + str(shared_bin_farm(self.skipped_commands))
        # host 模式保留系统命令，但把桩目录放在最前，确保被测脚本优先调用桩。
        return str(self.bin) + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin")

    def env(self, **overrides) -> dict:
        """构造子进程环境：清空所有 PORTAL_GATEWAY_* 泄漏值，固定 TMPDIR/PATH。"""
        env = {key: value for key, value in os.environ.items() if not key.startswith("PORTAL_GATEWAY_")}
        env["PATH"] = self.path
        env["TMPDIR"] = str(self.tmp / "tmpdir")
        env["PORTAL_GATEWAY_NGINX_INCLUDE"] = str(self.include)
        env["PORTAL_GATEWAY_LOCK_FILE"] = str(self.lock)
        env["PORTAL_GATEWAY_STUB_LOG"] = str(self.log)
        # 显式钉住渲染器解释器：用例不依赖 PATH 里是否存在 python3（Docker 与宿主机布局不同）。
        env["PORTAL_GATEWAY_PYTHON"] = sys.executable
        Path(env["TMPDIR"]).mkdir(parents=True, exist_ok=True)
        env.update({key: str(value) for key, value in overrides.items()})
        return env

    def run(self, *args: str, timeout: int = 90, **overrides) -> subprocess.CompletedProcess:
        """以被测脚本为入口执行一次命令。"""
        return subprocess.run(
            [bash_executable(), str(SCRIPT), *args],
            capture_output=True,
            text=True,
            errors="replace",
            env=self.env(**overrides),
            timeout=timeout,
        )

    def stub_records(self, name: str = "curl") -> list:
        """读取桩记录的 argv 列表（每行一个 JSON）。"""
        if not self.log.exists():
            return []
        records = []
        for line in self.log.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            record = json.loads(line)
            if record.get("cmd") == name:
                records.append(record)
        return records

    def write_curl_stub(self, spec: dict) -> Path:
        """写入 Python curl 桩：按 URL/页码返回预置 JSON，并记录 argv/--config 内容。

        spec["responses"][url] 为固定响应体；spec["pages"][url] 为 {页码: 响应体}，
        页码取自 --data-urlencode page=N（缺省按 "1" 处理，可用 "default" 兜底），
        用于验证分页循环内的空页语义。
        """
        spec_path = self.tmp / "curl-spec.json"
        spec_path.write_text(json.dumps(spec, ensure_ascii=False), encoding="utf-8")
        stub = f'''#!{sys.executable}
import json, os, sys
spec = json.load(open({str(spec_path)!r}, encoding="utf-8"))
argv = sys.argv[1:]
record = {{"cmd": "curl", "argv": argv, "cwd": os.getcwd()}}
url = ""
page = ""
config_path = None
for index, item in enumerate(argv):
    if item.startswith("http://") or item.startswith("https://"):
        url = item
    if item == "--data-urlencode" and index + 1 < len(argv):
        value = argv[index + 1]
        if value.startswith("page="):
            page = value[len("page="):]
    if item == "--config" and index + 1 < len(argv):
        config_path = argv[index + 1]
record["page"] = page
if config_path:
    record["config_path"] = config_path
    try:
        record["config_mode"] = oct(os.stat(config_path).st_mode & 0o777)
        record["config_body"] = open(config_path, encoding="utf-8").read()
    except OSError as exc:
        record["config_error"] = str(exc)
with open(os.environ["PORTAL_GATEWAY_STUB_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, ensure_ascii=False) + "\\n")
fail = spec.get("fail", {{}})
if url in fail:
    sys.stdout.write(fail[url].get("body", ""))
    sys.stderr.write(fail[url].get("stderr", ""))
    sys.exit(fail[url].get("status", 22))
body = spec.get("responses", {{}}).get(url)
if body is None:
    page_map = spec.get("pages", {{}}).get(url)
    if page_map is not None:
        body = page_map.get(page or "1", page_map.get("default"))
if body is None:
    for marker, payload in spec.get("responses", {{}}).items():
        if marker.endswith("*") and url.startswith(marker[:-1]):
            body = payload
            break
if body is None:
    body = spec.get("default", {{}})
sys.stdout.write(json.dumps(body, ensure_ascii=False))
'''
        return write_stub(self.bin, "curl", stub, interpreter=sys.executable)

    def write_flock_stub(self, exit_code: int = 0) -> Path:
        """写入 flock 桩：记录 argv 并以指定退出码返回。"""
        return self.write_recorder_stub("flock", {"exit": exit_code})

    def write_docker_stub(self, ps_output: str = "") -> Path:
        """写入 docker 桩：compose ps 输出预置服务名，其余子命令成功并记录 argv。"""
        return self.write_recorder_stub("docker", {"ps_output": ps_output})

    def write_recorder_stub(self, name: str, spec: dict) -> Path:
        """写入只记录 argv 的通用桩（flock/docker 用），退出码与 stdout 由 spec 决定。"""
        spec_path = self.tmp / f"{name}-spec.json"
        spec_path.write_text(json.dumps(spec, ensure_ascii=False), encoding="utf-8")
        stub = f'''#!{sys.executable}
import json, os, sys
spec = json.load(open({str(spec_path)!r}, encoding="utf-8"))
argv = sys.argv[1:]
record = {{"cmd": {name!r}, "argv": argv}}
with open(os.environ["PORTAL_GATEWAY_STUB_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, ensure_ascii=False) + "\\n")
if "ps" in argv:
    sys.stdout.write(spec.get("ps_output", ""))
sys.exit(int(spec.get("exit", 0)))
'''
        return write_stub(self.bin, name, stub, interpreter=sys.executable)

    def read_include(self) -> str:
        return self.include.read_text(encoding="utf-8")


class RenderContractTest(unittest.TestCase):
    """直接导入 gateway_render 的纯函数契约：渲染、解析、转义、校验与去重。"""

    def test_render_location_matches_golden_bytes(self):
        """固定 code/prefix/upstream 的完整片段必须逐字节等于 golden，防止模板漂移。"""
        rendered = gateway_render.render_location("demo", "/demo/api", "http://10.0.0.5:8080")
        self.assertEqual(GOLDEN_RENDER, rendered)
        self.assertEqual(GOLDEN_RENDER.encode("utf-8"), rendered.encode("utf-8"))

    def test_render_location_normalizes_trailing_slashes(self):
        """prefix 与 upstream 的结尾斜杠必须成对去掉，避免 nginx 拼出 //。"""
        rendered = gateway_render.render_location("demo", "/demo/api/", "http://h:1/")
        self.assertIn("location /demo/api/ {", rendered)
        self.assertIn("proxy_pass http://h:1/;", rendered)
        self.assertIn("location = /demo/api {", rendered)
        self.assertIn("return 308 /demo/api/;", rendered)
        self.assertNotIn("/demo/api//", rendered)
        self.assertNotIn("h:1//", rendered)

    def test_render_location_emits_no_extra_directives(self):
        """模板只生成转发所需指令，不出现脚本未实现的超时/缓冲设置。"""
        rendered = gateway_render.render_location("demo", "/demo/api", "http://10.0.0.5:8080")
        self.assertNotIn("proxy_buffering", rendered)
        self.assertNotIn("proxy_read_timeout", rendered)
        self.assertEqual(2, rendered.count("location "))

    def test_trim_trailing_slash_removes_single_slash(self):
        """trim_trailing_slash 只剥一个结尾斜杠（与原 shell 的 ${value%/} 等价）。"""
        self.assertEqual("/a", gateway_render.trim_trailing_slash("/a/"))
        self.assertEqual("/a/", gateway_render.trim_trailing_slash("/a//"))
        self.assertEqual("/a", gateway_render.trim_trailing_slash("/a"))

    def test_parse_entry_returns_code_prefix_upstream(self):
        """parse_entry 只识别 # code= 标记块，输出 (code, prefix, upstream) 三元组。"""
        with tempfile.TemporaryDirectory() as tmp:
            include = Path(tmp) / "inc.conf"
            include.write_text(
                HEADER_ADD
                + render_block("alpha", "/alpha/", "http://10.0.0.1:80/")
                + render_block("beta", "/beta/api/", "http://10.0.0.2:8080/"),
                encoding="utf-8",
            )
            entries = gateway_render.parse_entry(include.read_text(encoding="utf-8"))
        self.assertEqual(
            [
                ("alpha", "/alpha/", "http://10.0.0.1:80/"),
                ("beta", "/beta/api/", "http://10.0.0.2:8080/"),
            ],
            [(entry.code, entry.prefix, entry.upstream) for entry in entries],
        )

    def test_parse_entry_skips_blocks_without_proxy_pass(self):
        """缺少 proxy_pass 或 code 的残块不得被识别为有效入口。"""
        text = (
            "# code=broken\nlocation /broken/ {\n    proxy_http_version 1.1;\n}\n"
            "# code=ok\nlocation /ok/ {\n    proxy_pass http://10.0.0.9:81;\n}\n"
        )
        entries = gateway_render.parse_entry(text)
        self.assertEqual(
            [("ok", "/ok/", "http://10.0.0.9:81")],
            [(entry.code, entry.prefix, entry.upstream) for entry in entries],
        )

    def test_parse_entry_roundtrips_render_location(self):
        """render_location 的输出必须能被 parse_entry 原样解析回来（写读闭环）。"""
        text = render_block("gamma", "/gamma/api", "http://[::1]:9090")
        entries = gateway_render.parse_entry(text)
        self.assertEqual(1, len(entries), text)
        self.assertEqual("gamma", entries[0].code)
        self.assertEqual("http://[::1]:9090/", entries[0].upstream)
        self.assertEqual("/gamma/api/", entries[0].prefix)

    def test_parse_entry_ignores_text_outside_blocks(self):
        """块外的 location/proxy_pass 不属于托管条目，不得被解析。"""
        text = (
            HEADER_ADD
            + "location /handwritten/ {\n    proxy_pass http://10.0.0.99:1;\n}\n"
            + render_block("delta", "/delta", "http://10.0.0.4:4")
        )
        entries = gateway_render.parse_entry(text)
        self.assertEqual(
            [("delta", "/delta/", "http://10.0.0.4:4/")],
            [(entry.code, entry.prefix, entry.upstream) for entry in entries],
        )

    def test_escape_curl_config_escapes_quote_and_backslash(self):
        """escape_curl_config 必须转义反斜杠与双引号，避免破坏 curl --config。"""
        self.assertEqual('a\\"b\\\\c', gateway_render.escape_curl_config('a"b\\c'))
        self.assertEqual("", gateway_render.escape_curl_config(""))
        self.assertEqual("plain-token", gateway_render.escape_curl_config("plain-token\n"))

    def test_validate_code_accepts_and_rejects(self):
        """code 只接受 ^[a-z0-9][a-z0-9_.-]{0,79}$。"""
        for value in ("demo", "svc_a.b-c", "a" * 80):
            gateway_render.validate_code(value)
        for value in ("Bad_Code", "-lead", ".lead", "", "a" * 81):
            with self.subTest(code=value):
                with self.assertRaises(gateway_render.GatewayInputError) as caught:
                    gateway_render.validate_code(value)
                self.assertIn("code 必须是", str(caught.exception))

    def test_validate_path_prefix_accepts_and_rejects(self):
        """path_prefix 必须是非根绝对路径，且不含 // 与 . / .. 路径段。"""
        for value in ("/a", "/a_b.c~d!e+f/", "/" + "a" * 127):
            gateway_render.validate_path_prefix(value)
        cases = {
            "/": "path_prefix 必须是非根绝对路径",
            "/" + "a" * 129: "path_prefix 必须是非根绝对路径",
            "//evil": "path_prefix 不能包含 //",
            "/a/../b": "path_prefix 不能包含 . 或 ..",
            "/../../etc": "path_prefix 不能包含 . 或 ..",
            "/has space/": "path_prefix 必须是非根绝对路径",
        }
        for value, expected in cases.items():
            with self.subTest(prefix=value):
                with self.assertRaises(gateway_render.GatewayInputError) as caught:
                    gateway_render.validate_path_prefix(value)
                self.assertIn(expected, str(caught.exception))

    def test_validate_upstream_url_accepts_and_rejects(self):
        """upstream_url 必须是安全的 http(s) 绝对 URL，端口范围 1..65535。"""
        for value in ("http://h:1", "https://h", "http://[::1]:65535/base_path"):
            gateway_render.validate_upstream_url(value)
        cases = {
            "127.0.0.1:9001": "upstream_url 必须是安全的",
            "http://h/?a=1": "upstream_url 必须是安全的",
            "http://h/;}": "upstream_url 必须是安全的",
            "ftp://h:1": "upstream_url 必须是安全的",
            "http://h:70000": "端口必须在 1..65535",
            "http://h:0": "端口必须在 1..65535",
        }
        for value, expected in cases.items():
            with self.subTest(url=value):
                with self.assertRaises(gateway_render.GatewayInputError) as caught:
                    gateway_render.validate_upstream_url(value)
                self.assertIn(expected, str(caught.exception))

    def test_validate_page_limit_accepts_and_rejects(self):
        """PORTAL_GATEWAY_PAGE_LIMIT 必须是 1..100 的十进制整数。"""
        self.assertEqual(1, gateway_render.validate_page_limit("1"))
        self.assertEqual(100, gateway_render.validate_page_limit("100"))
        for value in ("0", "101", "abc", "-1", "1.5", ""):
            with self.subTest(page_limit=value):
                with self.assertRaises(gateway_render.GatewayInputError) as caught:
                    gateway_render.validate_page_limit(value)
                self.assertIn("PORTAL_GATEWAY_PAGE_LIMIT", str(caught.exception))

    def test_integrated_frontend_codes_normalize_separators(self):
        """内置前端清单支持空格/逗号/换行分隔，并忽略空项。"""
        self.assertTrue(gateway_render.is_integrated_frontend_code("contract_management"))
        self.assertTrue(
            gateway_render.is_integrated_frontend_code(
                "customer_and_opportunity", "contract_management,customer_and_opportunity"
            )
        )
        self.assertTrue(
            gateway_render.is_integrated_frontend_code(
                "contract_management", "contract_management\ncustomer_and_opportunity"
            )
        )
        self.assertFalse(gateway_render.is_integrated_frontend_code("demo", "contract_management"))

    def test_duplicate_prefixes_report_normalized_duplicates(self):
        """查重按去尾斜杠后的规范化路径比较，并只报告重复项。"""
        entries = [
            gateway_render.Entry("a-prod", "/dup/", "http://10.0.0.1:1"),
            gateway_render.Entry("b-prod", "/dup", "http://10.0.0.2:2"),
            gateway_render.Entry("c-prod", "/c/", "http://10.0.0.3:3"),
        ]
        self.assertEqual(["/dup"], gateway_render.duplicate_prefixes(entries))
        self.assertEqual([], gateway_render.duplicate_prefixes(entries[:1]))

    def test_render_sync_sorts_entries_by_code(self):
        """sync 渲染必须按 code 升序，且忽略字段不完整的条目。"""
        entries = [
            gateway_render.Entry("payment-prod", "/payment/", "http://10.0.0.12:9002"),
            gateway_render.Entry("demo-prod", "/demo/", "http://10.0.0.11:9001"),
            gateway_render.Entry("broken", "/broken/", ""),
        ]
        rendered = gateway_render.render_sync(entries)
        self.assertTrue(rendered.startswith(HEADER_SYNC), rendered)
        self.assertLess(rendered.index("# code=demo-prod"), rendered.index("# code=payment-prod"))
        self.assertNotIn("broken", rendered)
        self.assertEqual(2, rendered.count("# code="))


class AddRemoveListTest(unittest.TestCase):
    """add/remove/list 的文件级行为：块结构、幂等、冲突与清理语义。"""

    def setUp(self):
        self.harness = GatewayHarness(self)

    def test_add_writes_expected_include_and_mode(self):
        """add 生成托管头 + 完整 location 块，权限 0644 且不残留锁目录。"""
        result = self.harness.run("add", "demo", "/demo/api/", "http://127.0.0.1:9001/")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(HEADER_ADD + render_block("demo", "/demo/api", "http://127.0.0.1:9001"), self.harness.read_include())
        mode = stat.S_IMODE(self.harness.include.stat().st_mode)
        self.assertEqual(0o644, mode)
        self.assertFalse(Path(f"{self.harness.lock}.d").exists(), "目录锁未被清理")
        self.assertIn("已注册子系 demo", result.stderr)

    def test_add_is_idempotent(self):
        """同一入口连续 add 两次，文件内容必须完全一致且只含一个块。"""
        first = self.harness.run("add", "demo", "/demo/api/", "http://127.0.0.1:9001/")
        self.assertEqual(0, first.returncode, first.stderr)
        after_first = self.harness.read_include()
        second = self.harness.run("add", "demo", "/demo/api/", "http://127.0.0.1:9001/")
        self.assertEqual(0, second.returncode, second.stderr)
        self.assertEqual(after_first, self.harness.read_include())
        self.assertEqual(1, self.harness.read_include().count("# code=demo\n"))

    def test_add_replaces_upstream_for_same_code(self):
        """同 code 重新 add 视为替换：旧 upstream 消失，其他入口保持原样。"""
        self.assertEqual(0, self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001").returncode)
        self.assertEqual(0, self.harness.run("add", "other", "/other/", "http://10.0.0.2:9002").returncode)
        replaced = self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.3:9003")
        self.assertEqual(0, replaced.returncode, replaced.stderr)
        content = self.harness.read_include()
        self.assertNotIn("10.0.0.1", content)
        self.assertIn("proxy_pass http://10.0.0.3:9003/;", content)
        self.assertIn(render_block("other", "/other", "http://10.0.0.2:9002"), content)

    def test_add_uses_default_python3_when_override_is_unset(self):
        """未设置 PORTAL_GATEWAY_PYTHON 时按 PATH 解析 python3（发行版默认入口）。"""
        self.assertIsNotNone(shutil.which("python3"), "本机缺少 python3，无法验证默认解释器解析")
        harness = GatewayHarness(self, path_mode="host")
        result = harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001", PORTAL_GATEWAY_PYTHON="")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("proxy_pass http://10.0.0.1:9001/;", harness.read_include())

    def test_add_rejects_conflicting_path_prefix_without_touching_file(self):
        """同一 path_prefix 被其他子系统占用时必须以 2 退出并保留原文件。"""
        self.assertEqual(0, self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001").returncode)
        before = self.harness.read_include()
        result = self.harness.run("add", "intruder", "/demo/api", "http://10.0.0.9:9999")
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("已被子系 demo 占用", result.stderr)
        self.assertEqual(before, self.harness.read_include())

    def test_remove_keeps_other_subsystems(self):
        """remove 只删除目标块，其余子系统块逐字节保持不变。"""
        self.assertEqual(0, self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001").returncode)
        self.assertEqual(0, self.harness.run("add", "other", "/other/", "http://10.0.0.2:9002").returncode)
        removed = self.harness.run("remove", "demo")
        self.assertEqual(0, removed.returncode, removed.stderr)
        content = self.harness.read_include()
        self.assertNotIn("# code=demo", content)
        self.assertNotIn("/demo/api", content)
        self.assertEqual(HEADER_ADD + render_block("other", "/other", "http://10.0.0.2:9002"), content)
        self.assertIn("已移除子系 demo", removed.stderr)

    def test_remove_unknown_code_is_idempotent(self):
        """移除不存在的入口不报错（约定为幂等跳过），文件不变。"""
        self.assertEqual(0, self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001").returncode)
        before = self.harness.read_include()
        result = self.harness.run("remove", "ghost")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("子系 ghost 不存在，跳过", result.stderr)
        self.assertEqual(before, self.harness.read_include())

    def test_remove_without_include_file_is_noop(self):
        """include 文件不存在时 remove 直接成功返回，不创建任何文件。"""
        result = self.harness.run("remove", "demo")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("include 文件不存在", result.stderr)
        self.assertFalse(self.harness.include.exists())

    def test_remove_last_entry_leaves_managed_header_only(self):
        """移除最后一个入口后文件只剩托管头，不留空 location 块。"""
        self.assertEqual(0, self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001").returncode)
        result = self.harness.run("remove", "demo")
        self.assertEqual(0, result.returncode, result.stderr)
        content = self.harness.read_include()
        self.assertEqual(HEADER_ADD, content)
        self.assertNotIn("location", content)

    def test_list_prints_registered_entries(self):
        """list 输出表头与 code/path_prefix/upstream 三列，且不修改文件。"""
        self.assertEqual(0, self.harness.run("add", "demo", "/demo/api/", "http://10.0.0.1:9001").returncode)
        before = self.harness.read_include()
        result = self.harness.run("list")
        self.assertEqual(0, result.returncode, result.stderr)
        lines = [line for line in result.stdout.splitlines() if line.strip()]
        self.assertIn("code", lines[0])
        self.assertIn("demo", lines[1])
        self.assertIn("/demo/api/", lines[1])
        self.assertIn("http://10.0.0.1:9001", lines[1])
        self.assertEqual(before, self.harness.read_include())

    def test_unknown_command_exits_nonzero_with_usage(self):
        """未知子命令必须非零退出并打印用法。"""
        result = self.harness.run("frobnicate")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("未知命令", result.stderr)


class InvalidInputTest(unittest.TestCase):
    """非法参数必须在写文件前被拒绝：退出码 2 且 include 文件不存在。"""

    def setUp(self):
        self.harness = GatewayHarness(self)

    def test_rejects_unsafe_arguments_without_writing(self):
        """路径穿越/非法 code/缺 scheme/端口越界/超长前缀等均拒绝且不写文件。"""
        cases = [
            (("add", "Bad_Code", "/x/", "http://h:1"), "code 必须是"),
            (("add", "ok", "/a/../b", "http://h:1"), "不能包含 . 或 .."),
            (("add", "ok", "/../../etc", "http://h:1"), "不能包含 . 或 .."),
            (("add", "ok", "//evil", "http://h:1"), "不能包含 //"),
            (("add", "ok", "/", "http://h:1"), "path_prefix 必须是非根绝对路径"),
            (("add", "ok", "/x/", "127.0.0.1:9001"), "upstream_url 必须是安全的"),
            (("add", "ok", "/x/", "http://h/?a=1"), "upstream_url 必须是安全的"),
            (("add", "ok", "/x/", "http://h/;}"), "upstream_url 必须是安全的"),
            (("add", "ok", "/x/", "http://h:70000"), "端口必须在 1..65535"),
            (("add", "ok", "/x/", "ftp://h:1"), "upstream_url 必须是安全的"),
            (("add", "ok", "/" + "a" * 129, "http://h:1"), "path_prefix 必须是非根绝对路径"),
            (("add", "", "/x/", "http://h:1"), "缺少 <code>"),
            (("add", "ok"), "缺少 <path_prefix>"),
            (("remove", "BAD"), "code 必须是"),
        ]
        for argv, expected in cases:
            with self.subTest(argv=argv):
                result = self.harness.run(*argv)
                self.assertEqual(2, result.returncode, f"{argv} stderr={result.stderr}")
                self.assertIn(expected, result.stderr)
                self.assertFalse(self.harness.include.exists(), f"{argv} 不应写 include 文件")

    def test_add_accepts_documented_safe_characters(self):
        """合法边界值（IPv6、端口、下划线 code、子路径 upstream）必须被接受。"""
        result = self.harness.run("add", "svc_a.b-c", "/a_b.c~d!e+f/", "http://[::1]:65535/base_path")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("proxy_pass http://[::1]:65535/base_path/;", self.harness.read_include())


class SyncTest(unittest.TestCase):
    """sync 全量重建：与逐个 add 等价、空清单、重复前缀、失败退出码。"""

    def setUp(self):
        self.harness = GatewayHarness(self)

    def _write_curl(self, **spec):
        spec.setdefault("responses", {})
        self.harness.write_curl_stub(spec)

    def _apps(self, *items, total=None):
        return {"data": {"items": list(items), "total": len(items) if total is None else total}}

    def _envs(self, *items, total=None):
        return {"data": {"items": list(items), "total": len(items) if total is None else total}}

    def _preserve_fixture(self) -> bytes:
        """先写入一条旧 include 并返回字节内容，用于断言 sync 失败时逐字节不变。"""
        self.assertEqual(0, self.harness.run("add", "keep", "/keep/", "http://10.0.0.1:1").returncode)
        return self.harness.include.read_bytes()

    def _paged(self, url: str, pages: dict, total: int, kind: str = "apps") -> dict:
        """构造 pages 分页桩片段：{页码: 条目列表} → {url: {页码: 响应体}}。

        参数：url 接口匹配键（含 API base）；pages 以页码为键、条目 dict 列表为值；
        total 桩在每一页回报的 total；kind 为 "apps" 或 "envs" 决定响应形状。
        返回值：可直接并入 spec["pages"] 的字典。
        说明：count 驱动的分页会真实探测末页之后的那一页，因此这里总补一个
        「空 items + 同一 total」的兜底页，避免未知页码被当成响应格式错误。
        """
        builder = self._apps if kind == "apps" else self._envs
        page_map = {str(page): builder(*items, total=total) for page, items in pages.items()}
        page_map["default"] = builder(total=total)
        return {url: page_map}

    def test_sync_rebuilds_include_equivalent_to_add(self):
        """sync 生成的条目必须与逐个 add 一致（同一 code/prefix/upstream 集合）。"""
        apps = self._apps(
            {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"},
            {"application_id": "01APPBBBBBBBBBBBBBBBBBBBBB", "code": "payment"},
        )
        # 用脚本默认 API base（127.0.0.1:8080）作为桩匹配键，桩不会真的联网。
        base = "http://127.0.0.1:8080"
        self._write_curl(
            responses={
                f"{base}/api/v1/applications": apps,
                f"{base}/api/v1/applications/01APPAAAAAAAAAAAAAAAAAAAAA/environments": self._envs(
                    {"environment": "prod", "path_prefix": "/demo/api/", "upstream_url": "http://10.0.0.11:9001/"}
                ),
                f"{base}/api/v1/applications/01APPBBBBBBBBBBBBBBBBBBBBB/environments": self._envs(
                    {"environment": "prod", "path_prefix": "/payment/", "upstream_url": "http://10.0.0.12:9002"}
                ),
            }
        )
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="sync-token-value")
        self.assertEqual(0, result.returncode, result.stderr)
        synced = self.harness.read_include()
        self.assertTrue(synced.startswith(HEADER_SYNC), synced)

        # 逐个 add 到独立文件，比较 parse_entry 语义等价集合。
        other = GatewayHarness(self)
        self.assertEqual(0, other.run("add", "demo-prod", "/demo/api/", "http://10.0.0.11:9001/").returncode)
        self.assertEqual(0, other.run("add", "payment-prod", "/payment/", "http://10.0.0.12:9002").returncode)
        synced_entries = gateway_render.parse_include_file(str(self.harness.include))
        added_entries = gateway_render.parse_include_file(str(other.include))
        self.assertEqual(
            sorted((entry.code, entry.prefix, entry.upstream) for entry in added_entries),
            sorted((entry.code, entry.prefix, entry.upstream) for entry in synced_entries),
        )
        # 排序稳定：条目按 code 升序写入。
        self.assertLess(synced.index("# code=demo-prod"), synced.index("# code=payment-prod"))
        # 认证不使用 argv：token 只能出现在 0600 的 --config 文件里。
        records = self.harness.stub_records("curl")
        self.assertTrue(records, "curl 桩未被调用")
        self.assertNotIn("sync-token-value", result.stdout + result.stderr)
        for record in records:
            self.assertNotIn("sync-token-value", " ".join(record["argv"]))
            self.assertEqual("0o600", record.get("config_mode"), record)
            self.assertTrue(record["config_path"].startswith(str(self.harness.tmp)), record["config_path"])
        # 退出后临时凭据文件必须被清理。
        for record in records:
            self.assertFalse(Path(record["config_path"]).exists(), "临时凭据文件未清理")

    def test_sync_escapes_token_in_curl_config(self):
        """token 中的双引号/反斜杠必须转义后才能写入 --config（不能破坏文件）。"""
        base = "http://127.0.0.1:8080"
        token = 'tok"en\\value'
        self._write_curl(responses={f"{base}/api/v1/applications": self._apps()})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN=token)
        self.assertEqual(0, result.returncode, result.stderr)
        records = self.harness.stub_records("curl")
        self.assertTrue(records, "curl 桩未被调用")
        self.assertEqual(
            'header = "Authorization: Bearer tok\\"en\\\\value"\n',
            records[0]["config_body"],
        )
        self.assertNotIn(token, result.stdout + result.stderr)
        self.assertFalse(Path(records[0]["config_path"]).exists(), "临时凭据文件未清理")

    def test_sync_queries_pages_with_status_filter(self):
        """分页请求必须携带 page/page_size/status=ACTIVE 查询参数。"""
        base = "http://127.0.0.1:8080"
        self._write_curl(
            responses={
                f"{base}/api/v1/applications": self._apps(
                    {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}
                ),
                f"{base}/api/v1/applications/01APPAAAAAAAAAAAAAAAAAAAAA/environments": self._envs(
                    {"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://10.0.0.11:9001"}
                ),
            }
        )
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="t0ken")
        self.assertEqual(0, result.returncode, result.stderr)
        argv = self.harness.stub_records("curl")[0]["argv"]
        self.assertIn("page=1", argv)
        self.assertIn("page_size=100", argv)
        self.assertIn("status=ACTIVE", argv)
        self.assertIn("-G", argv)

    def test_sync_empty_application_list_resets_include(self):
        """清单为空时 sync 仍成功，并重写为仅含 sync 托管头的文件。"""
        base = "http://127.0.0.1:8080"
        self._write_curl(responses={f"{base}/api/v1/applications": self._apps()})
        self.assertEqual(0, self.harness.run("add", "stale", "/stale/", "http://10.0.0.1:1").returncode)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(0, result.returncode, result.stderr)
        content = self.harness.read_include()
        self.assertEqual(HEADER_SYNC, content)
        self.assertNotIn("stale", content)

    def test_sync_requires_token(self):
        """未提供 PORTAL_GATEWAY_API_TOKEN 时必须退出 2 且不发起请求。"""
        result = self.harness.run("sync")
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("PORTAL_GATEWAY_API_TOKEN", result.stderr)
        self.assertEqual([], self.harness.stub_records("curl"))

    def test_sync_rejects_out_of_range_page_limit(self):
        """PAGE_LIMIT 越界（0/101/非数字）必须退出 2。"""
        for value in ("0", "101", "abc"):
            with self.subTest(page_limit=value):
                result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="t", PORTAL_GATEWAY_PAGE_LIMIT=value)
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn("PORTAL_GATEWAY_PAGE_LIMIT", result.stderr)

    def test_sync_fails_and_preserves_include_on_http_error(self):
        """application 列表请求失败时以非零退出，旧 include 不被改写。"""
        base = "http://127.0.0.1:8080"
        self.assertEqual(0, self.harness.run("add", "keep", "/keep/", "http://10.0.0.1:1").returncode)
        before = self.harness.read_include()
        self._write_curl(fail={f"{base}/api/v1/applications": {"status": 22, "body": "boom"}})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("拉取 application 列表失败", result.stderr)
        self.assertEqual(before, self.harness.read_include())

    def test_sync_surfaces_curl_diagnostics_on_http_error(self):
        """curl 自身的 stderr（-fsS 的失败原因）必须透传，便于定位网关同步故障。"""
        base = "http://127.0.0.1:8080"
        self._write_curl(
            fail={f"{base}/api/v1/applications": {"status": 22, "stderr": "curl: (22) http 500\n"}}
        )
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("curl: (22) http 500", result.stderr)
        self.assertIn("拉取 application 列表失败", result.stderr)

    def test_sync_rejects_invalid_response_shape(self):
        """响应不是 .data.items 数组时以非零退出并保留旧 include。"""
        base = "http://127.0.0.1:8080"
        self.assertEqual(0, self.harness.run("add", "keep", "/keep/", "http://10.0.0.1:1").returncode)
        before = self.harness.read_include()
        self._write_curl(responses={f"{base}/api/v1/applications": {"data": {"items": "oops"}}})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("响应格式无效", result.stderr)
        self.assertEqual(before, self.harness.read_include())

    def test_sync_rejects_duplicate_path_prefix(self):
        """多个 ACTIVE environment 使用同一 path_prefix 时拒绝写入并保留旧文件。"""
        base = "http://127.0.0.1:8080"
        self.assertEqual(0, self.harness.run("add", "keep", "/keep/", "http://10.0.0.1:1").returncode)
        before = self.harness.read_include()
        self._write_curl(
            responses={
                f"{base}/api/v1/applications": self._apps(
                    {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"},
                    {"application_id": "01APPBBBBBBBBBBBBBBBBBBBBB", "code": "other"},
                ),
                f"{base}/api/v1/applications/01APPAAAAAAAAAAAAAAAAAAAAA/environments": self._envs(
                    {"environment": "prod", "path_prefix": "/dup/", "upstream_url": "http://10.0.0.11:9001"}
                ),
                f"{base}/api/v1/applications/01APPBBBBBBBBBBBBBBBBBBBBB/environments": self._envs(
                    {"environment": "prod", "path_prefix": "/dup/", "upstream_url": "http://10.0.0.12:9002"}
                ),
            }
        )
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("同一 path_prefix", result.stderr)
        self.assertEqual(before, self.harness.read_include())

    def test_sync_rejects_unsafe_upstream_from_api(self):
        """后端返回的非法 upstream 必须被拒绝（退出 2），不能写入 nginx 配置。"""
        base = "http://127.0.0.1:8080"
        self._write_curl(
            responses={
                f"{base}/api/v1/applications": self._apps(
                    {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}
                ),
                f"{base}/api/v1/applications/01APPAAAAAAAAAAAAAAAAAAAAA/environments": self._envs(
                    {"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://evil/;include"}
                ),
            }
        )
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(2, result.returncode, result.stderr)
        self.assertIn("upstream_url 必须是安全的", result.stderr)

    def test_sync_skips_entries_without_upstream_or_prefix(self):
        """缺少 upstream_url / path_prefix 的 environment 被过滤，不产生半残块。"""
        base = "http://127.0.0.1:8080"
        self._write_curl(
            responses={
                f"{base}/api/v1/applications": self._apps(
                    {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}
                ),
                f"{base}/api/v1/applications/01APPAAAAAAAAAAAAAAAAAAAAA/environments": self._envs(
                    {"environment": "no-upstream", "path_prefix": "/a/", "upstream_url": ""},
                    {"environment": "no-prefix", "path_prefix": "", "upstream_url": "http://10.0.0.1:1"},
                    {"environment": "ok", "path_prefix": "/ok/", "upstream_url": "http://10.0.0.2:2"},
                ),
            }
        )
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(0, result.returncode, result.stderr)
        content = self.harness.read_include()
        self.assertIn("# code=demo-ok", content)
        self.assertNotIn("no-upstream", content)
        self.assertNotIn("no-prefix", content)

    def test_sync_empty_page_with_positive_total_fails_without_creating_include(self):
        """items 为空但 total>0（接口异常/过滤不一致）时必须失败且不创建 include。

        回归缺陷：旧逻辑只在 count==0 时 break，循环后无条件全量重写，
        一次异常响应即把全部门户子系统路由静默清空。
        """
        base = "http://127.0.0.1:8080"
        self._write_curl(pages={f"{base}/api/v1/applications": {"1": self._apps(total=250)}})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("page=1", result.stderr)
        self.assertIn("total=250", result.stderr)
        self.assertIn("已收集 0 条", result.stderr)
        self.assertIn("拒绝重写 include", result.stderr)
        self.assertNotIn("sync 完成", result.stderr)
        self.assertFalse(self.harness.include.exists(), "异常空页不得创建 include 文件")
        self.assertEqual([], list(self.harness.tmp.glob("**/*.sync-input.*")), "sync_input 临时文件未清理")

    def test_sync_empty_page_with_positive_total_preserves_existing_include(self):
        """items 为空但 total>0 且有旧 include 时，文件必须逐字节不变。"""
        base = "http://127.0.0.1:8080"
        before = self._preserve_fixture()
        self._write_curl(pages={f"{base}/api/v1/applications": {"1": self._apps(total=250)}})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("total=250", result.stderr)
        self.assertNotIn("sync 完成", result.stderr)
        self.assertEqual(before, self.harness.include.read_bytes())

    def test_sync_allows_empty_list_when_total_is_zero(self):
        """合法空清单（items 为空且 total=0）保持既有语义：退出 0 并重写为仅托管头。"""
        base = "http://127.0.0.1:8080"
        self._preserve_fixture()
        self._write_curl(pages={f"{base}/api/v1/applications": {"1": self._apps(total=0)}})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(HEADER_SYNC, self.harness.read_include())
        self.assertIn("sync 完成", result.stderr)

    def test_sync_empty_list_creates_header_only_include_when_missing(self):
        """无旧文件 + 合法空清单：成功路径仍创建仅托管头的 include（0644）。

        sync 不再预先调用 ensure_include_file，这里锁定「成功时才落盘」的替代语义。
        """
        base = "http://127.0.0.1:8080"
        self._write_curl(pages={f"{base}/api/v1/applications": {"1": self._apps(total=0)}})
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertTrue(self.harness.include.exists())
        self.assertEqual(HEADER_SYNC, self.harness.read_include())
        self.assertEqual(0o644, stat.S_IMODE(self.harness.include.stat().st_mode))

    def test_sync_rejects_empty_second_page_and_preserves_include(self):
        """第 1 页正常、第 2 页 items 为空且 total 仍声称有第 2 页时必须失败且不改写 include。"""
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        before = self._preserve_fixture()
        pages = {
            apps_url: {
                "1": self._apps({"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}, total=2),
                "2": self._apps(total=2),
                "default": self._apps(total=2),
            },
            # environment 只做正常单页，确保失败点落在 application 第 2 页。
            f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments": {
                "1": self._envs({"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://10.0.0.11:9001"}),
                "default": self._envs(total=1),
            },
        }
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("page=2", result.stderr)
        self.assertIn("total=2", result.stderr)
        self.assertIn("已收集 1 条", result.stderr)
        self.assertIn("拒绝重写 include", result.stderr)
        self.assertNotIn("sync 完成", result.stderr)
        self.assertEqual(before, self.harness.include.read_bytes())

    def test_sync_succeeds_across_two_pages(self):
        """两页都有数据时正常分页，结果与逐个 add 等价（不误伤正常路径）。"""
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        pages = {}
        pages.update(
            self._paged(
                apps_url,
                {
                    1: [{"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}],
                    2: [{"application_id": "01APPBBBBBBBBBBBBBBBBBBBBB", "code": "payment"}],
                },
                total=2,
            )
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments",
                {1: [{"environment": "prod", "path_prefix": "/demo/api/", "upstream_url": "http://10.0.0.11:9001/"}]},
                total=1,
                kind="envs",
            )
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPBBBBBBBBBBBBBBBBBBBBB/environments",
                {1: [{"environment": "prod", "path_prefix": "/payment/", "upstream_url": "http://10.0.0.12:9002"}]},
                total=1,
                kind="envs",
            )
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(0, result.returncode, result.stderr)
        content = self.harness.read_include()
        self.assertTrue(content.startswith(HEADER_SYNC), content)
        self.assertIn(render_block("demo-prod", "/demo/api", "http://10.0.0.11:9001"), content)
        self.assertIn(render_block("payment-prod", "/payment", "http://10.0.0.12:9002"), content)
        # 条目按 code 升序写入，不受分页顺序影响。
        self.assertLess(content.index("# code=demo-prod"), content.index("# code=payment-prod"))
        self.assertEqual(2, content.count("# code="))

    def test_sync_follows_page_count_when_application_total_undercounts(self):
        """total 少报（报 1 条却实际两页各 1 条）时仍必须翻到真实末页，两条都收集。

        回归缺陷：旧逻辑用 page*PAGE_LIMIT >= total 判定结束，total=1、PAGE_LIMIT=1 时
        第 1 页之后就停，第 2 页的真实条目被静默丢弃。
        """
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        pages = {}
        pages.update(
            self._paged(
                apps_url,
                {
                    1: [{"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}],
                    2: [{"application_id": "01APPBBBBBBBBBBBBBBBBBBBBB", "code": "payment"}],
                },
                total=1,
            )
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments",
                {1: [{"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://10.0.0.11:9001"}]},
                total=1,
                kind="envs",
            )
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPBBBBBBBBBBBBBBBBBBBBB/environments",
                {1: [{"environment": "prod", "path_prefix": "/payment/", "upstream_url": "http://10.0.0.12:9002"}]},
                total=1,
                kind="envs",
            )
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(0, result.returncode, result.stderr)
        content = self.harness.read_include()
        self.assertIn(render_block("demo-prod", "/demo", "http://10.0.0.11:9001"), content)
        self.assertIn(render_block("payment-prod", "/payment", "http://10.0.0.12:9002"), content)
        self.assertEqual(2, content.count("# code="))
        # 第 2 页必须真的被请求过（旧逻辑在 total=1 时第 1 页就提前退出）。
        requested = [record["page"] for record in self.harness.stub_records("curl") if record["argv"][-1] == apps_url]
        self.assertIn("2", requested, requested)

    def test_sync_exact_full_page_probes_empty_tail_without_error(self):
        """恰好整页结束：探到第 2 页为空页且 total 不再声称有数据时，必须正常结束。

        第 2 页满足 total <= (page-1)*PAGE_LIMIT（2 <= 2），属于合法结束而非自相矛盾。
        """
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        pages = {}
        pages.update(
            self._paged(
                apps_url,
                {
                    1: [
                        {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"},
                        {"application_id": "01APPBBBBBBBBBBBBBBBBBBBBB", "code": "payment"},
                    ],
                },
                total=2,
            )
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments",
                {1: [{"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://10.0.0.11:9001"}]},
                total=1,
                kind="envs",
            )
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPBBBBBBBBBBBBBBBBBBBBB/environments",
                {1: [{"environment": "prod", "path_prefix": "/payment/", "upstream_url": "http://10.0.0.12:9002"}]},
                total=1,
                kind="envs",
            )
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=2)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertNotIn("拒绝重写 include", result.stderr)
        self.assertIn("sync 完成", result.stderr)
        content = self.harness.read_include()
        self.assertIn(render_block("demo-prod", "/demo", "http://10.0.0.11:9001"), content)
        self.assertIn(render_block("payment-prod", "/payment", "http://10.0.0.12:9002"), content)
        self.assertEqual(2, content.count("# code="))
        # 整页之后必然再探一页，确认空页不被误判为自相矛盾。
        requested = [record["page"] for record in self.harness.stub_records("curl") if record["argv"][-1] == apps_url]
        self.assertIn("2", requested, requested)

    def test_sync_rejects_empty_environment_page_with_positive_total(self):
        """environment 接口 total>0 却返回空页时必须失败、保留 include，并给出可诊断信息。

        回归缺陷：environment 内层分页没有空页守卫，total>0 但 items 为空时静默结束，
        该 application 的 environment 被少收集且不报错。
        """
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        before = self._preserve_fixture()
        pages = {}
        pages.update(
            self._paged(apps_url, {1: [{"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}]}, total=1)
        )
        env_url = f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments"
        pages.update(
            {
                env_url: {
                    "1": self._envs(total=2),
                    "default": self._envs(total=2),
                }
            }
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("environment 列表 page=1", result.stderr)
        self.assertIn("total=2", result.stderr)
        self.assertIn("拒绝重写 include", result.stderr)
        self.assertNotIn("sync 完成", result.stderr)
        self.assertEqual(before, self.harness.include.read_bytes())

    def test_sync_follows_page_count_when_environment_total_undercounts(self):
        """environment total 少报时仍要翻到真实末页，两页的 environment 都必须收集。"""
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        pages = {}
        pages.update(
            self._paged(apps_url, {1: [{"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}]}, total=1)
        )
        pages.update(
            self._paged(
                f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments",
                {
                    1: [{"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://10.0.0.11:9001"}],
                    2: [{"environment": "staging", "path_prefix": "/demo-staging/", "upstream_url": "http://10.0.0.12:9002"}],
                },
                total=1,
                kind="envs",
            )
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(0, result.returncode, result.stderr)
        content = self.harness.read_include()
        self.assertIn(render_block("demo-prod", "/demo", "http://10.0.0.11:9001"), content)
        self.assertIn(render_block("demo-staging", "/demo-staging", "http://10.0.0.12:9002"), content)
        self.assertEqual(2, content.count("# code="))
        env_url = f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments"
        requested = [record["page"] for record in self.harness.stub_records("curl") if record["argv"][-1] == env_url]
        self.assertIn("2", requested, requested)

    def test_sync_writes_empty_include_when_all_environments_filtered(self):
        """total>0 但所有 environment 缺 upstream/prefix 是合法结果：写空清单并给 INFO。"""
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        before = self._preserve_fixture()
        self.assertTrue(before)
        pages = {}
        pages.update(
            self._paged(apps_url, {1: [{"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}]}, total=1)
        )
        # 注意：items 非空但缺少 upstream_url，被聚合逻辑过滤；分页仍按原始条数推进。
        pages.update(
            self._paged(
                f"{apps_url}/01APPAAAAAAAAAAAAAAAAAAAAA/environments",
                {1: [{"environment": "prod", "path_prefix": "/demo/", "upstream_url": ""}]},
                total=1,
                kind="envs",
            )
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("空清单", result.stderr)
        self.assertIn("sync 完成", result.stderr)
        self.assertNotIn("拒绝重写 include", result.stderr)
        self.assertEqual(HEADER_SYNC, self.harness.read_include())

    def test_sync_writes_empty_include_when_all_applications_are_integrated_frontend(self):
        """total>0 且所有 application 都是内置统一前端模块：合法空清单，不报错。"""
        base = "http://127.0.0.1:8080"
        apps_url = f"{base}/api/v1/applications"
        before = self._preserve_fixture()
        self.assertTrue(before)
        pages = {}
        pages.update(
            self._paged(
                apps_url,
                {1: [{"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "contract_management"}]},
                total=1,
            )
        )
        self._write_curl(pages=pages)
        result = self.harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token", PORTAL_GATEWAY_PAGE_LIMIT=1)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("跳过统一前端内置模块的整站反代：contract_management", result.stderr)
        self.assertIn("空清单", result.stderr)
        self.assertIn("sync 完成", result.stderr)
        self.assertNotIn("拒绝重写 include", result.stderr)
        self.assertEqual(HEADER_SYNC, self.harness.read_include())
        # 内置模块不产生整站反代条目。
        self.assertNotIn("# code=", self.harness.read_include())

    def test_sync_works_without_jq_on_path(self):
        """sync 不再依赖 jq：PATH 中移除 jq 后仍能全量重建（解析已迁到 Python 标准库）。

        回归防线：一旦有人把 jq 调用接回 shell 或渲染器，这条用例会在缺 jq 的环境里失败。
        """
        harness = GatewayHarness(self, path_mode="fallback", skipped_commands=("flock", "jq"))
        self.assertIsNone(shutil.which("jq", path=harness.path), "PATH 中仍能解析到 jq")
        base = "http://127.0.0.1:8080"
        harness.write_curl_stub(
            {
                "responses": {
                    f"{base}/api/v1/applications": self._apps(
                        {"application_id": "01APPAAAAAAAAAAAAAAAAAAAAA", "code": "demo"}
                    ),
                    f"{base}/api/v1/applications/01APPAAAAAAAAAAAAAAAAAAAAA/environments": self._envs(
                        {"environment": "prod", "path_prefix": "/demo/", "upstream_url": "http://10.0.0.11:9001"}
                    ),
                }
            }
        )
        result = harness.run("sync", PORTAL_GATEWAY_API_TOKEN="token")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn(render_block("demo-prod", "/demo", "http://10.0.0.11:9001"), harness.read_include())


class IntegratedFrontendGuardTest(unittest.TestCase):
    """内置统一前端模块的整站反代保护（INTEGRATED_FRONTEND_CODES）。"""

    def setUp(self):
        self.harness = GatewayHarness(self)

    def test_newline_separated_list_skips_full_site_proxy(self):
        """换行分隔的清单能正确匹配：内置模块不生成整站 location。"""
        result = self.harness.run(
            "add",
            "contract_management",
            "/contract/",
            "http://10.0.0.1:9001",
            PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES="contract_management\ncustomer_and_opportunity",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("已内置于统一前端", result.stderr)
        self.assertFalse(self.harness.include.exists())

    def test_default_space_separated_list_skips_full_site_proxy(self):
        """默认空格分隔清单应命中内置模块保护，不生成整站 location。"""
        result = self.harness.run("add", "contract_management", "/contract/", "http://10.0.0.1:9001")
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("已内置于统一前端", result.stderr)
        self.assertFalse(self.harness.include.exists(), "内置模块不应生成整站 location")

    def test_comma_separated_list_is_normalized(self):
        """逗号分隔的历史写法应归一化后识别。"""
        result = self.harness.run(
            "add",
            "customer_and_opportunity",
            "/customer/",
            "http://10.0.0.1:9001",
            PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES="contract_management,customer_and_opportunity",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("已内置于统一前端", result.stderr)
        self.assertFalse(self.harness.include.exists())


class ShellDependencyTest(unittest.TestCase):
    """依赖面守卫：shell 只做锁/写文件/reload，不再自带渲染或 jq 调用。"""

    def test_shell_has_no_jq_invocation(self):
        """portal-gateway.sh 的可执行行不得再出现 jq（解析已迁到 Python 标准库）。"""
        for line in SCRIPT.read_text(encoding="utf-8").splitlines():
            stripped = line.strip()
            if stripped.startswith("#"):
                continue
            self.assertNotIn("jq", stripped, line)

    def test_shell_delegates_rendering_to_gateway_render(self):
        """shell 必须调用 gateway_render.py，且自身不保留 location 模板正文。"""
        source = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("scripts/lib/gateway_render.py", source)
        self.assertNotIn("proxy_set_header", source)
        self.assertNotIn("return 308", source)

    def test_gateway_render_uses_stdlib_only(self):
        """渲染器只用标准库：不得出现第三方 import 或 subprocess shell=True。"""
        source = (LIB_DIR / "gateway_render.py").read_text(encoding="utf-8")
        imported = set(re.findall(r"^(?:import|from)\s+([A-Za-z_][A-Za-z0-9_]*)", source, re.MULTILINE))
        allowed = {
            "__future__",
            "datetime",
            "json",
            "os",
            "re",
            "subprocess",
            "sys",
            "tempfile",
            "typing",
        }
        self.assertEqual(set(), imported - allowed, imported - allowed)
        self.assertNotIn("shell=True", source)


class LockTest(unittest.TestCase):
    """写锁：mkdir 回退路径、flock 路径、超时参数与并发串行化。"""

    def test_mkdir_fallback_times_out_without_writing(self):
        """目录锁已被持有时，第二个写者按约定超时失败且不写文件。"""
        harness = GatewayHarness(self, path_mode="fallback")
        self.assertIsNone(shutil.which("flock", path=harness.path), "回退路径不应找到 flock")
        lock_dir = Path(f"{harness.lock}.d")
        lock_dir.mkdir(parents=True)
        result = harness.run("add", "demo", "/demo/", "http://10.0.0.1:1", PORTAL_GATEWAY_LOCK_TIMEOUT=1)
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertFalse(harness.include.exists(), "未获得锁时不得写 include")
        self.assertTrue(lock_dir.exists(), "失败方不得删除他人持有的目录锁")
        self.assertNotIn("已注册子系", result.stderr)
        # bash 3.2 回归：全角括号紧跟变量名会让诊断退化成 unbound variable。
        self.assertIn("等待网关配置目录锁超时", result.stderr)
        self.assertIn("操作：add", result.stderr)
        self.assertNotIn("unbound variable", result.stderr)

    def test_mkdir_fallback_releases_lock_after_success(self):
        """正常完成后必须删除目录锁，允许后续写者进入。"""
        harness = GatewayHarness(self, path_mode="fallback")
        self.assertEqual(0, harness.run("add", "demo", "/demo/", "http://10.0.0.1:1").returncode)
        self.assertFalse(Path(f"{harness.lock}.d").exists())
        self.assertEqual(0, harness.run("add", "other", "/other/", "http://10.0.0.2:2").returncode)

    def test_concurrent_adds_are_serialized(self):
        """两个并发 add 必须串行化，最终文件同时包含两个入口且内容完整。"""
        harness = GatewayHarness(self, path_mode="fallback")
        env = harness.env(PORTAL_GATEWAY_LOCK_TIMEOUT="30")
        # 临时诊断（根因确认后移除）：让脚本把抢锁时间线写到 stderr，
        # 便于在 CI 日志里看到两个写者的锁路径、抢锁分支与是否发生等待。
        env["PORTAL_GATEWAY_LOCK_DEBUG"] = "1"
        commands = [
            ("demo", "/demo/", "http://10.0.0.1:1"),
            ("other", "/other/", "http://10.0.0.2:2"),
        ]
        processes = []
        started = []
        for argv in commands:
            started.append(time.monotonic())
            processes.append(
                subprocess.Popen(
                    [bash_executable(), str(SCRIPT), "add", *argv],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    errors="replace",
                    env=env,
                )
            )
        outputs = [process.communicate(timeout=120) for process in processes]
        elapsed = [round(time.monotonic() - start, 3) for start in started]
        diagnostics = [
            f"锁文件：{harness.lock}",
            f"include：{harness.include}",
            "PATH 解析："
            + repr(
                {
                    name: shutil.which(name, path=harness.path)
                    for name in ("flock", "mkdir", "rmdir", "sleep", "cat", "install", "mktemp")
                }
            ),
            f"耗时：{elapsed}",
            f"剩余锁目录：{Path(str(harness.lock) + '.d').exists()}",
        ]
        for argv, process, (stdout, stderr) in zip(commands, processes, outputs):
            diagnostics.append(f"=== {argv[0]} rc={process.returncode} ===\n{stderr}")
        detail = "\n".join(diagnostics)
        for process, (stdout, stderr) in zip(processes, outputs):
            self.assertEqual(0, process.returncode, detail)
        content = harness.read_include()
        self.assertIn(render_block("demo", "/demo", "http://10.0.0.1:1"), content, detail)
        self.assertIn(render_block("other", "/other", "http://10.0.0.2:2"), content, detail)
        # 抢锁先后决定写入顺序，因此断言「两条都在且各一次、无残块」，不锁定顺序。
        self.assertEqual(2, content.count("# code="), detail)
        self.assertEqual(2, content.count("location = "), detail)
        self.assertTrue(content.startswith(HEADER_ADD), detail)
        self.assertEqual(len(render_block("demo", "/demo", "http://10.0.0.1:1")) + len(
            render_block("other", "/other", "http://10.0.0.2:2")
        ) + len(HEADER_ADD), len(content), f"文件存在多余内容：{content}\n{detail}")

    def test_flock_path_passes_wait_timeout(self):
        """存在 flock 时必须走 flock 路径，并把 LOCK_TIMEOUT 传给 -w。"""
        harness = GatewayHarness(self, path_mode="host")
        harness.write_flock_stub(exit_code=0)
        result = harness.run("add", "demo", "/demo/", "http://10.0.0.1:1", PORTAL_GATEWAY_LOCK_TIMEOUT=7)
        self.assertEqual(0, result.returncode, result.stderr)
        records = harness.stub_records("flock")
        self.assertTrue(records, "flock 桩未被调用")
        self.assertEqual(["-w", "7", "9"], records[0]["argv"])
        self.assertIn("proxy_pass http://10.0.0.1:1/;", harness.read_include())

    def test_flock_path_timeout_fails_without_writing(self):
        """flock 拿不到锁时退出 1、不写文件，并给出可诊断信息。"""
        harness = GatewayHarness(self, path_mode="host")
        harness.write_flock_stub(exit_code=1)
        result = harness.run("add", "demo", "/demo/", "http://10.0.0.1:1", PORTAL_GATEWAY_LOCK_TIMEOUT=2)
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertFalse(harness.include.exists())
        self.assertTrue(harness.stub_records("flock"))
        # 必须真的输出超时诊断（bash 3.2 上也不得退化成 unbound variable）。
        self.assertIn("等待网关配置锁超时", result.stderr)
        self.assertIn("操作：add", result.stderr)
        self.assertNotIn("unbound variable", result.stderr)

    def test_lock_timeout_must_be_positive_integer(self):
        """LOCK_TIMEOUT 非正整数时必须退出 2；空值按脚本约定回退默认 60 秒。"""
        harness = GatewayHarness(self, path_mode="fallback")
        for value in ("0", "-1", "abc", "1.5"):
            with self.subTest(lock_timeout=value):
                result = harness.run("add", "demo", "/demo/", "http://10.0.0.1:1", PORTAL_GATEWAY_LOCK_TIMEOUT=value)
                self.assertEqual(2, result.returncode, result.stderr)
                self.assertIn("PORTAL_GATEWAY_LOCK_TIMEOUT", result.stderr)
                self.assertFalse(harness.include.exists())


class ReloadTest(unittest.TestCase):
    """reload 只走内置路径：忽略环境变量命令串，先 nginx -t 再 -s reload。"""

    def setUp(self):
        self.harness = GatewayHarness(self, path_mode="host")

    def test_reload_ignores_env_command_hook_and_checks_config(self):
        """PORTAL_GATEWAY_NGINX_RELOAD_CMD 只告警不执行；reload 前必须执行 nginx -t。"""
        compose = self.harness.tmp / "compose.yaml"
        compose.write_text("services: {}\n", encoding="utf-8")
        self.harness.write_docker_stub(ps_output="frontend")
        marker = self.harness.tmp / "pwned"
        result = self.harness.run(
            "reload",
            PORTAL_GATEWAY_COMPOSE_FILE=str(compose),
            PORTAL_GATEWAY_NGINX_RELOAD_CMD=f"touch {marker}",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("已忽略 NGINX_RELOAD_CMD", result.stderr)
        self.assertFalse(marker.exists(), "环境变量命令串不得被执行")
        argvs = [record["argv"] for record in self.harness.stub_records("docker")]
        self.assertEqual(
            [
                ["compose", "-f", str(compose), "exec", "-T", "frontend", "nginx", "-t"],
                ["compose", "-f", str(compose), "exec", "-T", "frontend", "nginx", "-s", "reload"],
            ],
            argvs,
        )

    def test_reload_requires_running_frontend(self):
        """compose.local.yaml 中没有运行中的 frontend 时 reload 以非零退出并提示。"""
        self.harness.write_docker_stub(ps_output="")
        result = self.harness.run("reload")
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("未找到 compose.local.yaml 中正在运行的 frontend 服务", result.stderr)

    def test_reload_rejects_missing_compose_file(self):
        """显式指定的 compose 文件不存在时拒绝 reload。"""
        missing = self.harness.tmp / "nope.yaml"
        result = self.harness.run("reload", PORTAL_GATEWAY_COMPOSE_FILE=str(missing))
        self.assertEqual(1, result.returncode, result.stderr)
        self.assertIn("不存在", result.stderr)
        self.assertEqual([], self.harness.stub_records("docker"))


class _CiLockProbe(unittest.TestCase):
    """临时诊断用例（根因确认后删除）：只打印证据，不做断言。

    目的：在 CI 机器上直接采集两类事实——
      1. mkdir 互斥是否原子：8 个进程同时 mkdir 同一路径，成功数必须为 1；
      2. 并发 add 的锁时间线：重复多轮，打印每个写者的 LOCKDBG 轨迹、
         耗时与最终 include 内容，用于捕捉偶发丢更新的那一轮。
    """

    def _probe_mkdir_atomicity(self):
        for round_index in range(4):
            tmp = Path(tempfile.mkdtemp(prefix="portal-gateway-probe-mkdir-"))
            self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
            target = tmp / "guard.d"
            body = 'if mkdir -- "$1" 2>/dev/null; then echo won; else echo lost; fi'
            processes = [
                subprocess.Popen(
                    [bash_executable(), "-c", body, "probe", str(target)],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                )
                for _ in range(8)
            ]
            outputs = [process.communicate() for process in processes]
            results = [stdout.strip() for stdout, _ in outputs]
            print(
                f"[PROBE-MKDIR round={round_index}] won={results.count('won')} "
                f"lost={results.count('lost')} raw={results} exists={target.exists()}",
                flush=True,
            )

    def _probe_concurrent_pair(self):
        for round_index in range(12):
            harness = GatewayHarness(self, path_mode="fallback")
            env = harness.env(
                PORTAL_GATEWAY_LOCK_TIMEOUT="30",
                PORTAL_GATEWAY_LOCK_DEBUG="1",
            )
            commands = [
                ("demo", "/demo/", "http://10.0.0.1:1"),
                ("other", "/other/", "http://10.0.0.2:2"),
            ]
            started = []
            processes = []
            for argv in commands:
                started.append(time.monotonic())
                processes.append(
                    subprocess.Popen(
                        [bash_executable(), str(SCRIPT), "add", *argv],
                        stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE,
                        text=True,
                        errors="replace",
                        env=env,
                    )
                )
            outputs = [process.communicate(timeout=120) for process in processes]
            elapsed = [round(time.monotonic() - start, 3) for start in started]
            content = harness.read_include() if harness.include.exists() else ""
            entries = [
                (entry.code, entry.prefix) for entry in gateway_render.parse_entry(content)
            ]
            print(
                f"[PROBE-PAIR round={round_index}] elapsed={elapsed} "
                f"rc={[process.returncode for process in processes]} entries={entries} "
                f"lockdir_left={Path(str(harness.lock) + '.d').exists()} "
                f"lock={harness.lock} include={harness.include}",
                flush=True,
            )
            for index, argv in enumerate(commands):
                for line in outputs[index][1].splitlines():
                    print(f"[PROBE-PAIR round={round_index} {argv[0]}] {line}", flush=True)

    def test_probe_lock_evidence(self):
        """打印 mkdir 原子性与并发 add 时间线证据。"""
        print(f"[PROBE] bash={bash_executable()} python={sys.executable}", flush=True)
        print(
            "[PROBE] which="
            + repr({name: shutil.which(name) for name in ("flock", "mkdir", "sleep", "cat")}),
            flush=True,
        )
        print(f"[PROBE] tmpdir={tempfile.gettempdir()}", flush=True)
        for name in ("mkdir", "install", "dirname", "rmdir", "flock"):
            resolved = shutil.which(name)
            if not resolved:
                print(f"[PROBE] {name}=MISSING", flush=True)
                continue
            real = os.path.realpath(resolved)
            version = subprocess.run(
                [resolved, "--version"], capture_output=True, text=True, timeout=30
            )
            first = (version.stdout or version.stderr).splitlines()[:1]
            print(
                f"[PROBE] {name}={resolved} real={real} rc={version.returncode} v={first}",
                flush=True,
            )
        self._probe_mkdir_atomicity()
        self._probe_concurrent_pair()


if __name__ == "__main__":
    unittest.main(verbosity=2)
