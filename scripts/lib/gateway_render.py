"""portal-gateway 的渲染、解析与 sync 聚合核心（仅标准库，兼容 Python 3.9）。

由 scripts/portal-gateway.sh 以子进程方式调用。shell 侧继续负责最终的文件写入
（用 cat 覆盖以保留 bind mount inode）以及 nginx -t / nginx -s reload；本模块负责：

  * 跨进程写锁：在 shell 已打开的锁文件描述符上取内核 flock（lock-fd 子命令），
    不再依赖外部 flock(1) 命令，也不依赖 mkdir 的目录原子性；
  * nginx 托管头与 location 片段渲染（render_location）；
  * include 文件条目解析（parse_entry）；
  * curl --config 值转义（escape_curl_config）；
  * code / path_prefix / upstream_url / page_limit / lock_timeout 校验；
  * sync 的分页拉取、聚合、空页守卫与 path_prefix 去重判定。

进程约定（与 shell 封装严格对应）：
  stdin   不使用。
  stdout  成功时为「完整 include 内容」（add/remove/sync）或「列表输出」（list）。
          shell 只在这个输出上做一次 cat 覆盖写，避免渲染与写入交错；
          lock-fd 子命令不使用 stdout，只把锁结果写进退出码。
  stderr  与 shell 版逐字一致的日志行 `[YYYY-mm-dd HH:MM:SS] [LEVEL] 消息`。
  退出码  0  stdout 有内容，需要写入/输出；
          1  运行时失败（curl 失败、分页自相矛盾等），禁止写文件；
          2  输入非法（参数或后端返回值不可渲染），禁止写文件；
          3  幂等跳过，无需写文件（include 不存在或目标条目不存在）。

环境变量与 shell 侧一致：PORTAL_GATEWAY_NGINX_INCLUDE、
PORTAL_GATEWAY_API_BASE_URL、PORTAL_GATEWAY_API_TOKEN、PORTAL_GATEWAY_PAGE_LIMIT、
PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES。
"""
from __future__ import annotations

import datetime
import fcntl
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from typing import List, Optional, Sequence, Tuple

# nginx include 文件的托管头：add/remove/list 与 sync 使用不同首行，但落盘内容逐字节固定。
HEADER_ADD = (
    "# DO NOT EDIT: managed by scripts/portal-gateway.sh.\n"
    "# 任何手动修改会在下一次 add/remove/sync 时被覆盖。\n"
)
HEADER_SYNC = (
    "# DO NOT EDIT: managed by scripts/portal-gateway.sh sync.\n"
    "# 任何手动修改会在下一次 add/remove/sync 时被覆盖。\n"
)

# 分页死循环保护（原 shell 的 readonly SYNC_MAX_PAGES）：application 与 environment
# 各自最多翻这么多页。超过上限说明服务端分页自相矛盾，必须放弃写入而不是继续请求。
SYNC_MAX_PAGES = 1000
# 后端列表接口的最大页大小；PORTAL_GATEWAY_PAGE_LIMIT 的合法区间是 1..100。
MAX_PAGE_LIMIT = 100

DEFAULT_API_BASE_URL = "http://127.0.0.1:8080"
DEFAULT_PAGE_LIMIT = "100"
DEFAULT_INTEGRATED_FRONTEND_CODES = (
    "contract_management customer_and_opportunity customer-opportunity"
)

EXIT_OK = 0
EXIT_RUNTIME = 1
EXIT_INPUT = 2
EXIT_NO_WRITE = 3

# POSIX [[:space:]] 的等价字符集：awk 的 trim 与 bash 的 [[:space:]] 只认这些字符，
# 不能用 Python 的 \s（还会匹配 NBSP 等 Unicode 空白），否则解析结果会偏离旧实现。
_SPACE = "[ \t\n\r\f\v]"
_AWK_SPACE = " \t\n\r\f\v"

_LOCATION_LEAD_RE = re.compile(r"^" + _SPACE + r"*location" + _SPACE + "+")
_LOCATION_BRACE_TAIL_RE = re.compile(r"\{.*$")
_PROXY_PASS_RE = re.compile(r"proxy_pass" + _SPACE + "+")
_PROXY_PASS_LEAD_RE = re.compile(r"^" + _SPACE + r"*proxy_pass" + _SPACE + "+")
_PROXY_PASS_SEMI_TAIL_RE = re.compile(r";" + _SPACE + r"*$")
_BLOCK_CLOSE_RE = re.compile(r"^" + _SPACE + r"*\}" + _SPACE + r"*$")

_CODE_RE = re.compile(r"[a-z0-9][a-z0-9_.-]{0,79}")
_PATH_PREFIX_RE = re.compile(r"/[A-Za-z0-9._~!+/\-]*")
_UPSTREAM_URL_RE = re.compile(
    r"https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~-]+)(:([0-9]{1,5}))?(/[A-Za-z0-9._~!%+/@-]*)?"
)
_PAGE_LIMIT_RE = re.compile(r"[0-9]+")

# 抢锁失败后的重试间隔（秒）。锁本身由内核 flock 提供，这里只是轮询的粒度；
# 50ms 既不会明显放大延迟，也不会像 1s 轮询那样让第二个写者白等整秒。
LOCK_POLL_INTERVAL = 0.05

_MAX_PATH_PREFIX_LENGTH = 128
_MAX_UPSTREAM_URL_LENGTH = 512
_MAX_PORT = 65535

# TSV 字段转义：与 jq 的 @tsv 一致，保证字段里的反斜杠/制表/换行不会破坏按行按列的解析。
_TSV_ESCAPE_TABLE = str.maketrans({
    "\\": "\\\\",
    "\t": "\\t",
    "\n": "\\n",
    "\r": "\\r",
})


class GatewayInputError(Exception):
    """输入非法（参数或后端返回值）：shell 侧按退出码 2 处理。"""


class GatewayRuntimeError(Exception):
    """运行时失败（网络/分页异常）：shell 侧按退出码 1 处理。"""


class GatewayLockTimeout(Exception):
    """抢锁超时：shell 侧按退出码 1 处理，且不得继续写 include。"""


class _CurlFailure(Exception):
    """内部信号：curl 非零退出；具体文案由调用方补上下文。"""


class Entry:
    """include 文件中的一条托管条目：code -> path_prefix -> upstream_url。"""

    __slots__ = ("code", "prefix", "upstream")

    def __init__(self, code: str, prefix: str, upstream: str) -> None:
        self.code = code
        self.prefix = prefix
        self.upstream = upstream

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, Entry):
            return NotImplemented
        return (
            self.code == other.code
            and self.prefix == other.prefix
            and self.upstream == other.upstream
        )

    def __repr__(self) -> str:  # pragma: no cover - 仅调试/失败信息使用
        return "Entry(%r, %r, %r)" % (self.code, self.prefix, self.upstream)


def log(level: str, message: str) -> None:
    """按旧 shell 的格式写 stderr：`[YYYY-mm-dd HH:MM:SS] [LEVEL] 消息`。"""
    stamp = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    sys.stderr.write("[%s] [%s] %s\n" % (stamp, level, message))
    sys.stderr.flush()


def trim_trailing_slash(value: str) -> str:
    """去掉一个结尾斜杠（原 shell 的 ${value%/}），仅一个，不做循环剥离。"""
    if value.endswith("/"):
        return value[:-1]
    return value


def render_location(code: str, path_prefix: str, upstream_url: str) -> str:
    """渲染单条 location 片段（含前导空行），逐字节对应旧脚本的 heredoc 模板。

    上游结尾斜杠与 location 成对规范化，使 Nginx 去掉门户前缀后再转发，
    并把原前缀通过 X-Forwarded-Prefix 传给子系统。
    """
    prefix = trim_trailing_slash(path_prefix)
    upstream = trim_trailing_slash(upstream_url)
    return (
        "\n"
        "# redirect code=%s\n"
        "location = %s {\n"
        "    return 308 %s/;\n"
        "}\n"
        "\n"
        "# code=%s\n"
        "location %s/ {\n"
        "    proxy_pass %s/;\n"
        "    proxy_http_version 1.1;\n"
        "    proxy_set_header Host $host;\n"
        "    proxy_set_header X-Real-IP $remote_addr;\n"
        "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n"
        "    proxy_set_header X-Forwarded-Proto $scheme;\n"
        "    proxy_set_header X-Forwarded-Host $host;\n"
        "    proxy_set_header X-Forwarded-Prefix %s;\n"
        "}\n"
    ) % (code, prefix, prefix, code, prefix, upstream, prefix)


def render_entries(entries: Sequence[Entry], header: str) -> str:
    """按给定顺序渲染托管头 + 每条 location 片段。"""
    return header + "".join(
        render_location(entry.code, entry.prefix, entry.upstream) for entry in entries
    )


def parse_entry(text: str) -> List[Entry]:
    """解析 include 文件中由本脚本写入的 entry（用注释行 # code=xxx 标识）。

    逐行等价于旧实现的 awk 脚本：只认 `# code=` 标记块，块内第一条 location 提供
    prefix，第一条 proxy_pass 提供 upstream，遇到单独一行的 `}` 收块；三者缺一不产出。
    """
    entries: List[Entry] = []
    current_code = ""
    in_block = False
    prefix = ""
    upstream = ""
    for line in text.split("\n"):
        if line.startswith("# code="):
            current_code = line[len("# code=") :]
            in_block = True
            prefix = ""
            upstream = ""
            continue
        if not in_block:
            continue
        if _LOCATION_LEAD_RE.match(line):
            # location 后面第一个非空白 token 就是前缀；忽略尾部 { 及之后内容。
            value = _LOCATION_LEAD_RE.sub("", line, count=1)
            value = _LOCATION_BRACE_TAIL_RE.sub("", value, count=1)
            prefix = value.strip(_AWK_SPACE)
        elif _PROXY_PASS_RE.search(line):
            value = _PROXY_PASS_LEAD_RE.sub("", line, count=1)
            value = _PROXY_PASS_SEMI_TAIL_RE.sub("", value, count=1)
            upstream = value.strip(_AWK_SPACE)
        elif _BLOCK_CLOSE_RE.match(line):
            if current_code and prefix and upstream:
                entries.append(Entry(current_code, prefix, upstream))
            current_code = ""
            in_block = False
    return entries


def parse_include_file(path: str) -> List[Entry]:
    """读取并解析 include 文件；文件不存在或为空时返回空列表。"""
    if not os.path.isfile(path):
        return []
    try:
        if os.path.getsize(path) == 0:
            return []
    except OSError:
        return []
    return parse_entry(read_text_file(path))


def escape_curl_config(value: str) -> str:
    """转义要写入 curl --config 的双引号字符串值。

    等价于旧实现的 `sed 's/\\\\/\\\\\\\\/g; s/"/\\\\"/g'`：先转义反斜杠再转义双引号；
    命令替换会吃掉结尾换行，因此这里同样剥离结尾换行。
    """
    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
    return escaped.rstrip("\n")


def is_integrated_frontend_code(code: str, codes: Optional[str] = None) -> bool:
    """判断编码是否属于已编译进统一 Vue 前端的模块。

    INTEGRATED_FRONTEND_CODES 默认空格分隔，历史配置可能写成逗号分隔：
    先把逗号归一化为空格，再按空白切分并忽略空项。
    """
    raw = DEFAULT_INTEGRATED_FRONTEND_CODES if codes is None else codes
    normalized = raw.replace(",", " ")
    return code in normalized.split()


def validate_code(code: str) -> None:
    """code 必须是 ^[a-z0-9][a-z0-9_.-]{0,79}$；否则按退出码 2 拒绝。"""
    if not _CODE_RE.fullmatch(code):
        raise GatewayInputError(
            "code 必须是 ^[a-z0-9][a-z0-9_.-]{0,79}$：%s" % code
        )


def validate_path_prefix(path_prefix: str) -> None:
    """校验 path_prefix：非根绝对路径、不含重复斜杠与点段、字符集受限。"""
    if (
        len(path_prefix) > _MAX_PATH_PREFIX_LENGTH
        or path_prefix == "/"
        or not _PATH_PREFIX_RE.fullmatch(path_prefix)
    ):
        raise GatewayInputError(
            "path_prefix 必须是非根绝对路径（最长 128，字母数字 . _ ~ ! - + /）：%s"
            % path_prefix
        )
    if "//" in path_prefix:
        raise GatewayInputError("path_prefix 不能包含 //：%s" % path_prefix)
    for segment in path_prefix.split("/"):
        if segment in (".", ".."):
            raise GatewayInputError(
                "path_prefix 不能包含 . 或 .. 路径段：%s" % path_prefix
            )


def validate_upstream_url(upstream_url: str) -> None:
    """校验 upstream_url：http(s) 绝对 URL，支持 [IPv6]:port，端口 1..65535。"""
    if len(upstream_url) > _MAX_UPSTREAM_URL_LENGTH:
        raise GatewayInputError(
            "upstream_url 必须是安全的 http(s) 绝对 URL（支持 [IPv6]:port）：%s"
            % upstream_url
        )
    matched = _UPSTREAM_URL_RE.fullmatch(upstream_url)
    if matched is None:
        raise GatewayInputError(
            "upstream_url 必须是安全的 http(s) 绝对 URL（支持 [IPv6]:port）：%s"
            % upstream_url
        )
    port = matched.group(3) or ""
    if port and not (1 <= int(port) <= _MAX_PORT):
        raise GatewayInputError(
            "upstream_url 端口必须在 1..65535：%s" % upstream_url
        )


def validate_page_limit(value: str) -> int:
    """校验 PORTAL_GATEWAY_PAGE_LIMIT 必须是 1..100 的十进制整数，返回其数值。"""
    if not _PAGE_LIMIT_RE.fullmatch(value) or not (1 <= int(value) <= MAX_PAGE_LIMIT):
        raise GatewayInputError(
            "PORTAL_GATEWAY_PAGE_LIMIT 必须在 1..100（后端最大页大小为 100）：%s" % value
        )
    return int(value)


def validate_lock_timeout(value: str) -> int:
    """校验 PORTAL_GATEWAY_LOCK_TIMEOUT 必须是正整数秒，返回其数值。"""
    if not re.fullmatch(r"[1-9][0-9]*", value or ""):
        raise GatewayInputError(
            "PORTAL_GATEWAY_LOCK_TIMEOUT 必须是正整数：%s" % value
        )
    return int(value)


def command_lock_fd(args: Sequence[str]) -> None:
    """在 shell 传入的文件描述符上取排他 flock（非阻塞重试到超时）。

    参数：<timeout> <lock_path> <operation> <fd>。锁挂在打开的 file description 上，
    因此本进程退出后锁仍然由持有该 fd 的 shell 保持，直到 shell 关闭该 fd——
    与 `flock -w TIMEOUT FD` 的内核语义完全一致，但不再依赖外部 flock(1) 命令。
    """
    if len(args) != 4:
        raise GatewayInputError("lock-fd 需要 <timeout> <lock_path> <operation> <fd>")
    timeout_text, lock_path, operation, fd_text = args
    timeout_seconds = validate_lock_timeout(timeout_text)
    try:
        descriptor = int(fd_text)
    except ValueError:
        raise GatewayInputError("lock-fd 的 fd 必须是整数：%s" % fd_text)
    deadline = time.monotonic() + timeout_seconds
    while True:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return
        except OSError:
            if time.monotonic() >= deadline:
                # 文案必须保留「等待网关配置锁超时」与「操作：」两段，运维脚本按此匹配。
                log(
                    "ERROR",
                    "等待网关配置锁超时：%s（操作：%s）" % (lock_path, operation),
                )
                raise GatewayLockTimeout()
            time.sleep(LOCK_POLL_INTERVAL)


def duplicate_prefixes(entries: Sequence[Entry]) -> List[str]:
    """返回被多个条目占用的 path_prefix（按规范化路径去尾斜杠后比较，升序去重）。"""
    normalized = [entry.prefix.rstrip("/") for entry in entries]
    counts = {}
    for prefix in normalized:
        counts[prefix] = counts.get(prefix, 0) + 1
    return sorted(prefix for prefix, count in counts.items() if count > 1)


def aggregate_sync(
    api_base_url: str,
    api_token: str,
    page_limit: int,
    integrated_codes: Optional[str],
) -> List[Entry]:
    """分页拉取 ACTIVE application/environment，返回待渲染条目（未排序）。

    分页语义与旧 shell 完全一致：以「本页实际返回条数」驱动翻页（count < PAGE_LIMIT
    才是真实末页），空页只在 total 不再声称有后续数据时才合法结束，否则视为接口
    自相矛盾并拒绝写入；两侧都受 SYNC_MAX_PAGES 保护。
    """
    entries: List[Entry] = []
    seen_applications = 0
    total = 0
    page = 1
    auth_config = _write_auth_config(api_token)
    try:
        while True:
            if page > SYNC_MAX_PAGES:
                raise GatewayRuntimeError(
                    "application 列表分页超过上限 %d 页（page=%d, total=%d，已收集 %d 条）；"
                    "疑似服务端分页异常，拒绝重写 include"
                    % (SYNC_MAX_PAGES, page, total, len(entries))
                )
            items, total = _fetch_page(
                url="%s/api/v1/applications" % api_base_url,
                page=page,
                page_limit=page_limit,
                auth_config=auth_config,
                failure_message="拉取 application 列表失败（page=%d）" % page,
                invalid_message="application 列表响应格式无效（page=%d）" % page,
            )
            seen_applications += len(items)
            if not items:
                # 空页有两种语义：服务端不再声称有后续数据时是正常结束（保留「重写为
                # 空清单」的既有语义）；仍声称总条数超过已翻页覆盖范围却给空页，只可能
                # 是接口瞬时异常或过滤口径不一致，必须拒绝写入。
                if total > (page - 1) * page_limit:
                    raise GatewayRuntimeError(
                        "application 列表 page=%d 的 items 为空但 total=%d（已收集 %d 条）；"
                        "疑似接口瞬时异常或过滤不一致，拒绝重写 include"
                        % (page, total, len(entries))
                    )
                break

            for item in items:
                if not isinstance(item, dict):
                    # jq 在非对象条目上会报错并停止输出该页剩余行，等价于跳过。
                    continue
                application_id = _tsv_field(item.get("application_id"))
                application_code = _tsv_field(item.get("code"))
                if not application_id or not application_code:
                    continue
                if is_integrated_frontend_code(application_code, integrated_codes):
                    log(
                        "INFO",
                        "跳过统一前端内置模块的整站反代：%s" % application_code,
                    )
                    continue
                _collect_environments(
                    entries=entries,
                    api_base_url=api_base_url,
                    auth_config=auth_config,
                    page_limit=page_limit,
                    application_id=application_id,
                    application_code=application_code,
                )

            # 以实际返回条数驱动翻页：只有「本页不足一页」才说明到了真实末页；
            # 不用 page*PAGE_LIMIT >= total，避免服务端 total 少报时漏页。
            if len(items) < page_limit:
                break
            page += 1
    finally:
        _remove_auth_config(auth_config)

    # 兜底：应用总数非零，但跨页一条 application 都没读到（接口声称有数据却全空），
    # 属于接口自相矛盾，不能把 include 重写成仅托管头。
    if total > 0 and seen_applications == 0:
        raise GatewayRuntimeError(
            "application total=%d 但未读取到任何 application（已收集 %d 条）；拒绝重写 include"
            % (total, len(entries))
        )
    # 读到 application 但 0 条可代理 environment 是合法结果（全部被内置统一前端跳过，
    # 或全部缺 upstream/prefix）：保持旧语义重写为空清单，同时给出可审计的 INFO。
    if total > 0 and not entries:
        log(
            "INFO",
            "全部 application 均被跳过或无可用 environment（total=%d），写入空清单" % total,
        )
    duplicates = duplicate_prefixes(entries)
    if duplicates:
        raise GatewayRuntimeError(
            "多个 ACTIVE environment 使用了同一 path_prefix：%s" % "\n".join(duplicates)
        )
    return entries


def render_sync(entries: Sequence[Entry]) -> str:
    """按 code 升序渲染 sync 的完整 include 内容（排序不受分页顺序影响）。"""
    ordered = sorted(entries, key=lambda entry: entry.code)
    filtered = [
        entry for entry in ordered if entry.code and entry.prefix and entry.upstream
    ]
    return render_entries(filtered, HEADER_SYNC)


def _collect_environments(
    entries: List[Entry],
    api_base_url: str,
    auth_config: str,
    page_limit: int,
    application_id: str,
    application_code: str,
) -> None:
    """拉取单个 application 的 environment 分页并追加条目（与旧内层循环同构）。"""
    environment_page = 1
    environment_total = 0
    while True:
        if environment_page > SYNC_MAX_PAGES:
            raise GatewayRuntimeError(
                "environment 列表分页超过上限 %d 页（application=%s, page=%d, total=%d，"
                "已收集 %d 条）；疑似服务端分页异常，拒绝重写 include"
                % (
                    SYNC_MAX_PAGES,
                    application_id,
                    environment_page,
                    environment_total,
                    len(entries),
                )
            )
        items, environment_total = _fetch_page(
            url="%s/api/v1/applications/%s/environments" % (api_base_url, application_id),
            page=environment_page,
            page_limit=page_limit,
            auth_config=auth_config,
            failure_message="拉取 environment 列表失败（application=%s, page=%d）"
            % (application_id, environment_page),
            invalid_message="environment 列表响应格式无效（application=%s, page=%d）"
            % (application_id, environment_page),
        )
        if not items:
            # 空页守卫：服务端声称还有 environment 却没返回任何条目时，静默结束会漏
            # 收集路由，与 application 空页一样必须拒绝写入 include。
            if environment_total > (environment_page - 1) * page_limit:
                raise GatewayRuntimeError(
                    "environment 列表 page=%d 的 items 为空但 total=%d（application=%s，"
                    "已收集 %d 条）；疑似接口瞬时异常或过滤不一致，拒绝重写 include"
                    % (
                        environment_page,
                        environment_total,
                        application_id,
                        len(entries),
                    )
                )
            break

        for item in items:
            if not isinstance(item, dict):
                continue
            # 与旧 jq select 等价：缺少 upstream_url 或 path_prefix 的条目直接过滤。
            upstream = _tsv_field(item.get("upstream_url"))
            prefix = _tsv_field(item.get("path_prefix"))
            if not upstream or not prefix:
                continue
            environment_code = _tsv_field(item.get("environment"))
            if not environment_code:
                continue
            entry_code = "%s-%s" % (application_code, environment_code)
            validate_code(entry_code)
            validate_path_prefix(prefix)
            validate_upstream_url(upstream)
            entries.append(Entry(entry_code, prefix, upstream))

        # 与 application 分页同构：environment 的页码每个 application 独立计数。
        if len(items) < page_limit:
            break
        environment_page += 1


def _fetch_page(
    url: str,
    page: int,
    page_limit: int,
    auth_config: str,
    failure_message: str,
    invalid_message: str,
) -> Tuple[List[object], int]:
    """请求一页列表并解析 {data:{items,total}}；失败抛对应 GatewayRuntimeError。"""
    try:
        payload = _curl_list(url, page, page_limit, auth_config)
    except _CurlFailure:
        raise GatewayRuntimeError(failure_message)
    try:
        document = json.loads(payload)
    except ValueError:
        raise GatewayRuntimeError(invalid_message)
    data = document.get("data") if isinstance(document, dict) else None
    if not isinstance(data, dict) or not isinstance(data.get("items"), list):
        raise GatewayRuntimeError(invalid_message)
    return data["items"], _coerce_total(data.get("total"))


def _curl_list(url: str, page: int, page_limit: int, auth_config: str) -> str:
    """执行与旧 shell 等价的 curl 调用；Bearer Token 只经 0600 --config 传入 argv 之外。"""
    argv = [
        "curl",
        "-fsS",
        "-G",
        "--data-urlencode",
        "page=%d" % page,
        "--data-urlencode",
        "page_size=%d" % page_limit,
        "--data-urlencode",
        "status=ACTIVE",
        "--config",
        auth_config,
        url,
    ]
    try:
        completed = subprocess.run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            universal_newlines=True,
            errors="replace",
        )
    except OSError:
        raise _CurlFailure()
    if completed.returncode != 0:
        # 旧实现用 $(curl ...) 捕获 stdout，curl 自身的 stderr 直接透传到脚本 stderr
        # （-fsS 会在 HTTP 失败时打印原因）；这里保持同样的诊断可见性。
        if completed.stderr:
            sys.stderr.write(completed.stderr)
            sys.stderr.flush()
        raise _CurlFailure()
    return completed.stdout


def _coerce_total(value: object) -> int:
    """把 .data.total 归一化为整数；旧实现是 jq '// 0' 后交给 bash 算术。"""
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            return 0
        try:
            return int(stripped)
        except ValueError:
            return 0
    return 0


def _tsv_field(value: object) -> str:
    """把 jq @tsv 渲染后的单个字段还原为字符串（含转义）。

    缺失/None 渲染为空串；对象与数组在 jq 里会报错，这里按「跳过该行」处理（注册表
    接口不会返回这类值）。转义后字段内不再包含裸的制表/换行，等价于 bash 逐行读取。
    """
    if value is None:
        return ""
    if isinstance(value, str):
        text = value
    elif isinstance(value, bool):
        text = "true" if value else "false"
    elif isinstance(value, (int, float)):
        text = json.dumps(value)
    else:
        return ""
    return text.translate(_TSV_ESCAPE_TABLE)


def _write_auth_config(api_token: str) -> str:
    """创建 0600 的临时 curl 配置文件，Bearer Token 不进入 argv（ps 可见）。"""
    handle, path = tempfile.mkstemp(prefix="portal-gateway-auth.")
    try:
        os.chmod(path, 0o600)
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            stream.write(
                'header = "Authorization: Bearer %s"\n' % escape_curl_config(api_token)
            )
    except BaseException:
        _remove_auth_config(path)
        raise
    return path


def _remove_auth_config(path: Optional[str]) -> None:
    """删除临时凭据文件，已删除或不存在时忽略。"""
    if not path:
        return
    try:
        os.unlink(path)
    except OSError:
        pass


def read_text_file(path: str) -> str:
    """读取文件；非法 UTF-8 用 surrogateescape 保留，保证解析/回写可往返。"""
    with open(path, "r", encoding="utf-8", errors="surrogateescape") as stream:
        return stream.read()


def write_stdout(text: str) -> None:
    """把内容按 UTF-8 原样写到 stdout（不做换行转换，保持逐字节一致）。"""
    buffer = getattr(sys.stdout, "buffer", None)
    if buffer is None:  # pragma: no cover - 仅在 stdout 被替换为文本流时使用
        sys.stdout.write(text)
        sys.stdout.flush()
        return
    buffer.write(text.encode("utf-8", "surrogateescape"))
    buffer.flush()


def command_add(args: Sequence[str], include_path: str) -> Optional[str]:
    """add：校验 → 冲突检测 → 渲染完整 include 内容（返回 None 表示无需写入）。"""
    if len(args) != 3:
        raise GatewayInputError("add 需要 <code> <path_prefix> <upstream_url>")
    code, path_prefix, upstream_url = args
    validate_code(code)
    validate_path_prefix(path_prefix)
    validate_upstream_url(upstream_url)

    integrated_codes = os.environ.get("PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES")
    if is_integrated_frontend_code(code, integrated_codes):
        # 已编译进统一前端：先清掉历史整站代理，再跳过登记。
        _remove_entries(code, include_path)
        log("INFO", "%s 已内置于统一前端，跳过整站反向代理登记" % code)
        return None

    entries = parse_include_file(include_path)
    incoming_prefix = trim_trailing_slash(path_prefix)
    for entry in entries:
        if entry.code != code and trim_trailing_slash(entry.prefix) == incoming_prefix:
            # 同一路径只能有一个 ACTIVE 上游，避免 Nginx 匹配顺序把请求送错子系统。
            raise GatewayInputError(
                "path_prefix 已被子系 %s 占用：%s" % (entry.code, path_prefix)
            )
    kept = [entry for entry in entries if entry.code != code]
    kept.append(Entry(code, path_prefix, upstream_url))
    return render_entries(kept, HEADER_ADD)


def command_remove(args: Sequence[str], include_path: str) -> Optional[str]:
    """remove：删除目标条目并渲染剩余内容（返回 None 表示无需写入）。"""
    if len(args) != 1:
        raise GatewayInputError("remove 需要 <code>")
    validate_code(args[0])
    return _remove_entries(args[0], include_path)


def command_list(include_path: str) -> Optional[str]:
    """list：输出 code/path_prefix/upstream_url 三列（按文件内顺序）。"""
    if not os.path.isfile(include_path):
        log("INFO", "include 文件不存在")
        return None
    entries = parse_entry(read_text_file(include_path))
    lines = ["%-32s %-40s %s" % ("code", "path_prefix", "upstream_url")]
    for entry in entries:
        lines.append("%-32s %-40s %s" % (entry.code, entry.prefix, entry.upstream))
    return "\n".join(lines) + "\n"


def command_sync(include_path: str) -> Optional[str]:
    """sync：从受控认证适配层全量重建 include 内容（返回 None 表示无需写入）。"""
    api_token = os.environ.get("PORTAL_GATEWAY_API_TOKEN", "")
    if not api_token:
        raise GatewayInputError("sync 需要 PORTAL_GATEWAY_API_TOKEN")
    page_limit = validate_page_limit(
        os.environ.get("PORTAL_GATEWAY_PAGE_LIMIT", DEFAULT_PAGE_LIMIT)
    )
    api_base_url = os.environ.get("PORTAL_GATEWAY_API_BASE_URL") or DEFAULT_API_BASE_URL
    integrated_codes = os.environ.get("PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES")
    # 不预先落盘托管头：sync 拉取过程中任何异常都不应留下「空清单已生效」的假象。
    # 这里只保证目录存在，供临时凭据文件与最终写入使用。
    directory = os.path.dirname(os.path.abspath(include_path))
    try:
        os.makedirs(directory, mode=0o700, exist_ok=True)
    except OSError as error:
        raise GatewayRuntimeError("无法创建 include 目录：%s（%s）" % (directory, error))
    entries = aggregate_sync(api_base_url, api_token, page_limit, integrated_codes)
    return render_sync(entries)


def _remove_entries(code: str, include_path: str) -> Optional[str]:
    """remove 的共享实现：返回剩余条目渲染结果，None 表示无需写文件。"""
    if not os.path.isfile(include_path):
        log("INFO", "include 文件不存在，无需清理")
        return None
    entries = parse_entry(read_text_file(include_path))
    kept = [entry for entry in entries if entry.code != code]
    if len(kept) == len(entries):
        log("INFO", "子系 %s 不存在，跳过" % code)
        return None
    return render_entries(kept, HEADER_ADD)


def main(argv: Sequence[str]) -> int:
    """CLI 入口：分发 add/remove/list/sync/lock-fd，把异常映射为稳定的退出码。"""
    command = argv[0] if argv else ""
    include_path = os.environ.get("PORTAL_GATEWAY_NGINX_INCLUDE", "")
    if command in ("add", "remove", "list", "sync") and not include_path:
        log("ERROR", "缺少 PORTAL_GATEWAY_NGINX_INCLUDE（由 portal-gateway.sh 注入）")
        return EXIT_INPUT
    try:
        if command == "lock-fd":
            command_lock_fd(argv[1:])
            return EXIT_OK
        if command == "add":
            rendered = command_add(argv[1:], include_path)
        elif command == "remove":
            rendered = command_remove(argv[1:], include_path)
        elif command == "list":
            rendered = command_list(include_path)
        elif command == "sync":
            rendered = command_sync(include_path)
        else:
            log("ERROR", "未知渲染命令：%s" % command)
            return EXIT_INPUT
    except GatewayInputError as error:
        log("ERROR", str(error))
        return EXIT_INPUT
    except GatewayRuntimeError as error:
        log("ERROR", str(error))
        return EXIT_RUNTIME
    except GatewayLockTimeout:
        # 文案已由 command_lock_fd 输出；退出码 1 让 shell 放弃本次写入。
        return EXIT_RUNTIME
    if rendered is None:
        return EXIT_NO_WRITE
    write_stdout(rendered)
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
