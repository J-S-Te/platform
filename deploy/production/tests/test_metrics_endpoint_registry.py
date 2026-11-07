#!/usr/bin/env python3
"""指标端点注册表门禁：README 的“已采集/未采集”清单、prometheus.yml 与 compose 三方对齐。

背景
----
`deploy/production/README.md` 的「监控抓取与指标端点边界」与「新增抓取 job 与指标清单」
是采集边界的唯一说明：声明为“已采集”的端点必须真的在
`monitoring/prometheus/prometheus.yml` 里有 job，声明为“未采集”的端点绝不能被悄悄接进
抓取（README 明确写了本轮不新增 job，接入顺序要先改 prometheus 再同步文档与门禁）。
两侧任一侧漂移都会产生“以为有样本、其实永远静默”的告警，或者“文档说没采集、实际在抓”
配置泄漏。因此这里把三者做成硬断言：

1. README「已采集」表里每个 job/端点，必须在 prometheus.yml 里存在同名 job 且 target
   与表格端点一致；
2. README「未采集」表里的端点，不得出现在 prometheus.yml 的 job/target 里；
3. prometheus.yml 每个 target 的服务名必须是 docker-compose.yml `services:` 里已声明的
   服务（跨主机地址、宿主机端口、写错服务名都会在这里断掉）；
4. 「监控抓取与指标端点边界」章节列出的 job 与 target 同样是权威清单，必须与
   prometheus.yml 一致；
5. 指标目录归属（D3）：每个 job 必须有 `# access: token|loopback-only|network-only`
   访问控制声明，且声明必须与 docker-compose.yml 的 `ports` 对得上（network-only 不允许
   发布宿主机端口，loopback-only 只允许 127.0.0.1 绑定），token 必须配只读凭据文件；
6. 「未采集」表的每一行必须显式登记主责人（`owner=`）和“未采集”状态，本轮 scope-out
   不允许静默缺省；项目管理 `:9092` 还必须写明无应用层鉴权、仅内网；
7. `monitoring/prometheus/platform-alerts.yml` 的告警表达式里，带 status 标签的指标
   必须用等值 `status="5xx"`；`status=~"5.."` 这类正则/数字区间写法永远匹配不到序列，
   会让规则静默永不触发（注释里的反例不算表达式）。

依赖与执行
----------
只用标准库做逐行解析（不引入 pyyaml/ruby），不发请求、不启动容器、不读 runtime 文件。
默认读本仓库的 `deploy/production`；负例验证时可设置 `DEPLOY_PRODUCTION_ROOT` 指向
/tmp 副本（CI 不设置该变量）。
"""
from __future__ import annotations

import os
import re
import unittest
from pathlib import Path


PRODUCTION = Path(
    os.environ.get("DEPLOY_PRODUCTION_ROOT", Path(__file__).resolve().parents[1])
).resolve()
README = PRODUCTION / "README.md"
PROMETHEUS = PRODUCTION / "monitoring" / "prometheus" / "prometheus.yml"
PLATFORM_ALERTS = PRODUCTION / "monitoring" / "prometheus" / "platform-alerts.yml"
COMPOSE = PRODUCTION / "docker-compose.yml"

# 指标目录归属（D3）：job 注释里的访问控制声明取值。
ACCESS_VALUES = ("token", "loopback-only", "network-only")
ACCESS_LINE = re.compile(r"#\s*access:\s*([A-Za-z-]+)\s*$")
# status 标签是分桶枚举字符串，正则/区间写法永远匹配不到；只允许这些等值字面量。
STATUS_BUCKETS = ("1xx", "2xx", "3xx", "4xx", "5xx", "other")
STATUS_REGEX_SELECTOR = re.compile(r'status\s*(?:=~|!~)\s*"([^"]*)"')
STATUS_NUMERIC_SELECTOR = re.compile(r"status\s*(?:>=|<=|>|<)\s*\d")
STATUS_5XX_EQUALITY = re.compile(r'status\s*=\s*"5xx"')
# 只允许回环绑定的两种写法（与 test-compose-orchestration-consistency.py 的既有约定一致）。
LOOPBACK_PREFIXES = ("127.0.0.1:", "${KEYCLOAK_BIND_ADDRESS:-127.0.0.1}")
# 带 {status} 分桶标签的 HTTP 指标：表达式里的 status 选择器必须是等值桶名。
HTTP_STATUS_METRICS = ("platform_http_errors_total", "platform_http_requests_total")

# README 的章节标题（不含 # 前缀，标题级别由出现顺序决定）。
BOUNDARY_SECTION = "监控抓取与指标端点边界"
CAPTURE_SECTION = "新增抓取 job 与指标清单"
COLLECTED_TABLE_FIRST_CELL = "抓取 job"
UNCOLLECTED_TABLE_FIRST_CELL = "服务（无抓取 job）"
# 「未采集」行必须出现的状态标记（D3：scope-out 也要显式登记，不能静默缺省）。
UNCOLLECTED_MARKER = "未采集"

BACKTICK_TOKEN = re.compile(r"`([^`]+)`")
ENDPOINT = re.compile(r"https?://([A-Za-z0-9._-]+:\d{1,5})/metrics")
JOB_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")
TARGET = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*:\d{1,5}")
SAME_AS_ABOVE = ("同上", "同上。")


def read(path: Path) -> str:
    if not path.is_file():
        raise AssertionError("缺少部署资产：%s" % path)
    return path.read_text(encoding="utf-8")


def section_lines(text: str, title: str) -> list:
    """按标题文本取章节正文；标题级别用 # 个数判定，遇到同级或更高级标题截断。"""
    lines = text.splitlines()
    start = None
    for index, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith("#"):
            continue
        # 标题可能带编号（如「#### 1. 新增抓取 job 与指标清单」），编号不参与匹配。
        heading = re.sub(r"^\d+[.、]\s*", "", stripped.lstrip("#").strip())
        if heading == title:
            start = index
            break
    if start is None:
        raise AssertionError("README 缺少章节「%s」（本门禁依赖该章节，不得删改标题）" % title)
    level = len(lines[start].strip()) - len(lines[start].strip().lstrip("#"))
    body = []
    for line in lines[start + 1:]:
        stripped = line.strip()
        if stripped.startswith("#"):
            current = len(stripped) - len(stripped.lstrip("#"))
            if current <= level:
                break
        body.append(line)
    return body


def tables(lines):
    """逐行提取 markdown 表格：返回 (表头单元格, 数据行单元格列表)。"""
    result = []
    index = 0
    while index < len(lines):
        if not lines[index].strip().startswith("|"):
            index += 1
            continue
        block = []
        while index < len(lines) and lines[index].strip().startswith("|"):
            block.append(lines[index].strip())
            index += 1
        rows = [[cell.strip() for cell in line.strip("|").split("|")] for line in block]
        if len(rows) < 2:
            continue
        header = rows[0]
        body = [row for row in rows[2:] if any(cell for cell in row)]
        result.append((header, body))
    return result


def table_with_first_cell(lines, first_cell: str):
    for header, rows in tables(lines):
        if header and header[0] == first_cell:
            return header, rows
    raise AssertionError(
        "README 缺少表头首列为「%s」的指标表（本门禁依赖该表，不得删改表头）" % first_cell
    )


def tokens(cell: str) -> list:
    return BACKTICK_TOKEN.findall(cell)


def endpoint_of(row_description: str, cell: str):
    """表“端点”列 → host:port；`同上` 返回 None 由调用方继承。"""
    if cell.strip() in SAME_AS_ABOVE:
        return None
    match = ENDPOINT.search(cell)
    if match:
        return match.group(1)
    raise AssertionError(
        "无法解析端点「%s」：%s（本门禁只认得 http(s)://host:port/metrics 或「同上」）"
        % (cell, row_description)
    )


def collected_endpoints(readme: str):
    """README「已采集」表 → {job: {host:port, ...}}。"""
    _, rows = table_with_first_cell(section_lines(readme, CAPTURE_SECTION), COLLECTED_TABLE_FIRST_CELL)
    endpoints = {}
    last_by_job = {}
    previous = None
    for row in rows:
        if len(row) < 2:
            raise AssertionError("「已采集」表行缺少 job/端点列：%s" % row)
        job_tokens = tokens(row[0])
        if not job_tokens:
            raise AssertionError("「已采集」表首列没有 job 名（需要 `job` 形式）：%s" % row[0])
        job = job_tokens[0]
        if not JOB_NAME.fullmatch(job):
            raise AssertionError("「已采集」表首列的 job 名不合法：%s" % job)
        endpoint = endpoint_of("job=%s" % job, row[1])
        if endpoint is None:
            endpoint = last_by_job.get(job) or previous
            if endpoint is None:
                raise AssertionError("「已采集」表首行不能用「同上」：job=%s" % job)
        endpoints.setdefault(job, set()).add(endpoint)
        last_by_job[job] = endpoint
        previous = endpoint
    if not endpoints:
        raise AssertionError("README「已采集」表没有数据行")
    return endpoints


def uncollected_endpoints(readme: str):
    """README「未采集」表 → [(服务名, host:port), ...]。"""
    _, rows = table_with_first_cell(
        section_lines(readme, CAPTURE_SECTION), UNCOLLECTED_TABLE_FIRST_CELL
    )
    endpoints = []
    for row in rows:
        if len(row) < 2:
            raise AssertionError("「未采集」表行缺少服务/端点列：%s" % row)
        service_tokens = tokens(row[0])
        if not service_tokens:
            raise AssertionError("「未采集」表首列没有服务名（需要 `service` 形式）：%s" % row[0])
        service = service_tokens[0]
        endpoint = endpoint_of("未采集服务=%s" % service, row[1])
        if endpoint is None:
            raise AssertionError("「未采集」表不允许「同上」：%s" % row[0])
        endpoints.append((service, endpoint))
    if not endpoints:
        raise AssertionError("README「未采集」表没有数据行")
    return endpoints


def boundary_registry(readme: str):
    """「监控抓取与指标端点边界」章节 → (job 名集合, target 集合)。"""
    lines = section_lines(readme, BOUNDARY_SECTION)
    bullet = next(
        (line for line in lines if "抓取" in line and "`" in line), None
    )
    if bullet is None:
        raise AssertionError(
            "README「%s」章节缺少“抓取 …”清单条目（本门禁依赖该条目）" % BOUNDARY_SECTION
        )
    # 只取“抓取 …。”这一段：后面的正文（如“只走 `application` 内网”）不是抓取清单。
    scope = bullet.split("抓取", 1)[1].split("。", 1)[0] if "抓取" in bullet else ""
    if not scope:
        raise AssertionError(
            "README「%s」章节的抓取清单无法解析：%s" % (BOUNDARY_SECTION, bullet)
        )
    listed = tokens(scope)
    jobs = {token for token in listed if JOB_NAME.fullmatch(token)}
    targets = {token for token in listed if TARGET.fullmatch(token)}
    if not jobs or not targets:
        raise AssertionError(
            "README「%s」章节的抓取清单无法解析出 job/target：%s" % (BOUNDARY_SECTION, bullet)
        )
    return jobs, targets


def parse_prometheus(text: str):
    """prometheus.yml → {job: [target, ...]}；只解析 job_name 与 targets。"""
    jobs = {}
    markers = list(
        re.finditer(r"(?m)^\s*-\s*job_name:\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*$", text)
    )
    if not markers:
        raise AssertionError("prometheus.yml 未解析出任何 job_name（本门禁依赖该结构）")
    for index, marker in enumerate(markers):
        end = markers[index + 1].start() if index + 1 < len(markers) else len(text)
        block = text[marker.end():end]
        jobs[marker.group(1)] = parse_targets(block)
    return jobs


def parse_targets(block: str):
    targets = []
    lines = block.splitlines()
    index = 0
    while index < len(lines):
        line = lines[index]
        index += 1
        match = re.match(r"\s*-?\s*targets:\s*(.*)$", line)
        if not match:
            continue
        rest = match.group(1).strip()
        if rest:
            targets.extend(re.findall(r'"([^"]+)"', rest))
            continue
        # 块状列表：`targets:` 之后的 `- "host:port"` 行。
        while index < len(lines) and lines[index].strip().startswith("-"):
            targets.extend(re.findall(r'"([^"]+)"', lines[index]))
            index += 1
    return targets


def parse_compose_services(text: str):
    """docker-compose.yml → services 段的键集合（两空格缩进的服务名）。"""
    services = set()
    in_services = False
    for line in text.splitlines():
        if not in_services:
            if re.match(r"^services:\s*(?:#.*)?$", line):
                in_services = True
            continue
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if not line[:1].isspace():
            break  # 下一个顶层键（networks/volumes/...）
        match = re.match(r"^ {2}([A-Za-z0-9][A-Za-z0-9._-]*):\s*(?:#.*)?$", line)
        if match:
            services.add(match.group(1))
    if not services:
        raise AssertionError("docker-compose.yml 未解析出 services 段的服务名")
    return services


def host_of(target: str) -> str:
    return target.rsplit(":", 1)[0]


def parse_compose_service_ports(text: str):
    """docker-compose.yml → {service: [已发布端口条目原文, ...]}；未发布端口为空列表。"""
    ports = {}
    current = None
    in_services = False
    in_ports = False
    for line in text.splitlines():
        if not in_services:
            if re.match(r"^services:\s*(?:#.*)?$", line):
                in_services = True
            continue
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if not line[:1].isspace():
            break  # 下一个顶层键
        service = re.match(r"^ {2}([A-Za-z0-9][A-Za-z0-9._-]*):\s*(?:#.*)?$", line)
        if service:
            current = service.group(1)
            ports.setdefault(current, [])
            in_ports = False
            continue
        if current is None:
            continue
        if re.match(r"^ {4}ports:\s*(?:#.*)?$", line):
            in_ports = True
            continue
        if not in_ports:
            continue
        entry = re.match(r"^\s+-\s*(.+?)\s*$", line)
        if entry and not entry.group(1).startswith("#"):
            ports[current].append(entry.group(1).strip().strip('"').strip("'"))
            continue
        if len(line) - len(line.lstrip()) <= 4:
            in_ports = False
    return ports


def parse_jobs_with_access(text: str):
    """prometheus.yml → {job: {"targets": [...], "access": [声明值], "block": [行]}}。

    `# access:` 只认 job 上方的缩进注释（与 `- job_name` 同级或更浅），job 块内部注释
    也归档到 comments 以便排查，但允许声明出现在紧邻 job 的注释块里。
    """
    jobs = {}
    current = None
    pending = []
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("#"):
            indent = len(line) - len(line.lstrip())
            if current is None or indent <= 2:
                pending.append(stripped)
            else:
                current["comments"].append(stripped)
            continue
        if not stripped:
            continue
        marker = re.match(r"^\s*-\s*job_name:\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*$", line)
        if marker:
            job = marker.group(1)
            jobs[job] = {"targets": [], "access": [], "block": [line], "comments": list(pending)}
            current = jobs[job]
            pending = []
            continue
        if current is not None:
            current["block"].append(line)
            continue
        pending = []
    for data in jobs.values():
        data["targets"] = parse_targets("\n".join(data["block"]))
        data["access"] = [
            match.group(1)
            for comment in data["comments"]
            for match in [ACCESS_LINE.search(comment)]
            if match
        ]
    return jobs


def alert_expressions(text: str):
    """platform-alerts.yml → [expr 文本, ...]。

    只取 `expr:` 的值（含 `>-` 折叠块），不扫注释和 `description` 文本：反例写法经常出现
    在说明/注释里，扫全文会把“文档写对了、表达式写错了”判反。
    """
    lines = text.splitlines()
    expressions = []
    index = 0
    while index < len(lines):
        line = lines[index]
        index += 1
        match = re.match(r"^(\s*)expr:\s*(.*)$", line)
        if not match:
            continue
        indent = len(match.group(1))
        value = match.group(2).strip()
        if value and value not in (">", ">-", "|", "|-"):
            expressions.append(value)
            continue
        block = []
        while index < len(lines):
            candidate = lines[index]
            stripped = candidate.strip()
            if not stripped:
                index += 1
                continue
            if len(candidate) - len(candidate.lstrip()) <= indent:
                break
            index += 1
            if stripped.startswith("#"):
                continue
            block.append(stripped)
        expressions.append(" ".join(block))
    return expressions


class MetricsEndpointRegistryTest(unittest.TestCase):
    """README 声明、prometheus 抓取配置与 compose 服务名必须三方一致。"""

    @classmethod
    def setUpClass(cls):
        cls.readme = read(README)
        cls.prometheus_jobs = parse_prometheus(read(PROMETHEUS))
        cls.services = parse_compose_services(read(COMPOSE))
        cls.service_ports = parse_compose_service_ports(read(COMPOSE))
        cls.jobs_with_access = parse_jobs_with_access(read(PROMETHEUS))

    def test_collected_endpoints_have_scrape_jobs(self):
        """README 声明“已采集”的每个 job/端点都必须在 prometheus.yml 里真实存在。"""
        declared = collected_endpoints(self.readme)
        missing_jobs = sorted(set(declared) - set(self.prometheus_jobs))
        missing_targets = sorted(
            "%s -> %s" % (job, endpoint)
            for job, endpoints in declared.items()
            if job in self.prometheus_jobs
            for endpoint in endpoints
            if endpoint not in self.prometheus_jobs[job]
        )
        self.assertEqual(
            missing_jobs,
            [],
            "README 声明已采集但 prometheus.yml 缺少 job：%s（现有 job：%s）"
            % (missing_jobs, sorted(self.prometheus_jobs)),
        )
        self.assertEqual(
            missing_targets,
            [],
            "README 已采集端点与 prometheus.yml 的 job target 不一致：%s（现有 target：%s）"
            % (
                missing_targets,
                {job: targets for job, targets in sorted(self.prometheus_jobs.items())},
            ),
        )

    def test_uncollected_endpoints_stay_out_of_scrape_config(self):
        """README 声明“未采集”的端点不得出现在 prometheus.yml 的 job 或 target 里。"""
        declared_uncollected = uncollected_endpoints(self.readme)
        all_targets = {
            target for targets in self.prometheus_jobs.values() for target in targets
        }
        leaked_jobs = sorted(
            "%s（服务 %s）" % (service, endpoint)
            for service, endpoint in declared_uncollected
            if service in self.prometheus_jobs
        )
        leaked_targets = sorted(
            "%s（服务 %s）" % (endpoint, service)
            for service, endpoint in declared_uncollected
            if endpoint in all_targets
        )
        self.assertEqual(
            leaked_jobs,
            [],
            "README 声明未采集，但 prometheus.yml 出现同名 job：%s（现有 job：%s）"
            % (leaked_jobs, sorted(self.prometheus_jobs)),
        )
        self.assertEqual(
            leaked_targets,
            [],
            "README 声明未采集，但 prometheus.yml 出现该 target：%s（现有 target：%s）"
            % (leaked_targets, sorted(all_targets)),
        )

    def test_prometheus_targets_are_declared_compose_services(self):
        """每个抓取 target 的服务名都必须是 docker-compose.yml 已声明的服务。"""
        missing = sorted(
            "%s（job %s）" % (host_of(target), job)
            for job, targets in self.prometheus_jobs.items()
            for target in targets
            if host_of(target) not in self.services
        )
        self.assertEqual(
            missing,
            [],
            "prometheus.yml 的 target 指向未声明的 compose 服务：%s"
            % missing,
        )

    def test_boundary_section_matches_prometheus(self):
        """「监控抓取与指标端点边界」章节列出的 job/target 必须与 prometheus.yml 一致。"""
        jobs, targets = boundary_registry(self.readme)
        all_targets = {
            target for target_list in self.prometheus_jobs.values() for target in target_list
        }
        self.assertEqual(
            sorted(jobs - set(self.prometheus_jobs)),
            [],
            "README 边界章节声明的 job 不在 prometheus.yml：%s（现有 job：%s）"
            % (sorted(jobs - set(self.prometheus_jobs)), sorted(self.prometheus_jobs)),
        )
        self.assertEqual(
            sorted(targets - all_targets),
            [],
            "README 边界章节声明的 target 不在 prometheus.yml：%s（现有 target：%s）"
            % (sorted(targets - all_targets), sorted(all_targets)),
        )
        # 反向：prometheus 里出现的每个 job 都必须写进 README 边界章节，否则新增抓取
        # 会绕过文档口径。这里只反向核对 job 名，不核对 target 字面量：README 该条目
        # 对 keycloak-backup 只写 job 名（target 是 keycloak-backup-metrics:9100，与
        # job 名不同），target 与 compose 服务的一致性由上面的用例覆盖。
        self.assertEqual(
            sorted(set(self.prometheus_jobs) - jobs),
            [],
            "prometheus.yml 的 job 未写入 README 边界章节：%s（章节声明：%s）"
            % (sorted(set(self.prometheus_jobs) - jobs), sorted(jobs)),
        )

    # ---------- D3：指标目录归属（访问控制声明 + 未采集显式登记） ----------

    def test_every_scrape_job_declares_access_control(self):
        """每个抓取 job 必须且只能声明一次 `# access: token|loopback-only|network-only`。"""
        self.assertEqual(
            sorted(set(self.jobs_with_access)),
            sorted(self.prometheus_jobs),
            "访问控制声明的解析结果与 job 清单不一致：%s vs %s"
            % (sorted(self.jobs_with_access), sorted(self.prometheus_jobs)),
        )
        problems = []
        for job, data in sorted(self.jobs_with_access.items()):
            declared = data["access"]
            if len(declared) != 1:
                problems.append(
                    "%s 声明了 %d 次访问控制（%s），必须恰好一次"
                    % (job, len(declared), declared)
                )
            elif declared[0] not in ACCESS_VALUES:
                problems.append(
                    "%s 的访问控制取值非法：%s（只允许 %s）"
                    % (job, declared[0], list(ACCESS_VALUES))
                )
        self.assertEqual(
            problems,
            [],
            "prometheus.yml 的 job 缺少或写错 `# access:` 声明（新增 job 必须同时登记访问边界）：%s"
            % problems,
        )

    def test_token_jobs_use_read_only_credentials_file(self):
        """`access: token` 的 job 必须用 authorization + 只读 credentials_file，不得内联凭据。"""
        token_jobs = sorted(
            job for job, data in self.jobs_with_access.items() if data["access"] == ["token"]
        )
        self.assertIn(
            "platform-api",
            token_jobs,
            "平台 job 必须声明 access: token（RequireMetricsAccess 的 Bearer 凭据），实际 %s"
            % token_jobs,
        )
        for job in token_jobs:
            block = "\n".join(self.jobs_with_access[job]["block"])
            with self.subTest(job=job):
                self.assertRegex(
                    block,
                    r"(?m)^\s*authorization:\s*$",
                    "job %s 声明 access: token，但缺少 authorization 段" % job,
                )
                self.assertRegex(
                    block,
                    r"(?m)^\s*credentials_file:\s*\S+",
                    "job %s 声明 access: token，但缺少 credentials_file（凭据必须来自只读挂载文件）"
                    % job,
                )
                self.assertNotRegex(
                    block,
                    r"(?m)^\s*credentials:\s*\S+",
                    "job %s 内联了 credentials，仓库内不得出现真实 token" % job,
                )
                self.assertNotIn(
                    "bearer_token",
                    block,
                    "job %s 使用了 bearer_token，必须改用 credentials_file" % job,
                )

    def test_network_only_jobs_publish_no_host_ports(self):
        """`access: network-only` 的 job，其 target 服务不得发布任何宿主机端口。"""
        network_jobs = sorted(
            job
            for job, data in self.jobs_with_access.items()
            if data["access"] == ["network-only"]
        )
        self.assertTrue(
            network_jobs,
            "prometheus.yml 没有任何 network-only job（file-gateway/keycloak-backup 应为例）",
        )
        problems = []
        for job in network_jobs:
            for target in self.jobs_with_access[job]["targets"]:
                service = host_of(target)
                if service not in self.service_ports:
                    problems.append("%s: target %s 不是 docker-compose.yml 已声明服务" % (job, target))
                    continue
                published = self.service_ports[service]
                if published:
                    problems.append(
                        "%s: 声明 network-only，但 %s 发布了宿主机端口 %s"
                        % (job, service, published)
                    )
        self.assertEqual(
            problems,
            [],
            "network-only 与 compose 端口矛盾（要么改访问声明，要么撤掉 ports）：%s" % problems,
        )

    def test_loopback_only_jobs_publish_only_loopback_ports(self):
        """`access: loopback-only` 的 job，其 target 服务的端口只能绑定 127.0.0.1。"""
        loopback_jobs = sorted(
            job
            for job, data in self.jobs_with_access.items()
            if data["access"] == ["loopback-only"]
        )
        self.assertTrue(
            loopback_jobs,
            "prometheus.yml 没有任何 loopback-only job（platform/contract/settlement 应为例）",
        )
        problems = []
        for job in loopback_jobs:
            for target in self.jobs_with_access[job]["targets"]:
                service = host_of(target)
                published = self.service_ports.get(service)
                if not published:
                    problems.append(
                        "%s: 声明 loopback-only，但 %s 没有任何宿主机端口发布；"
                        "若本就不发布端口应改为 network-only" % (job, service)
                    )
                    continue
                for entry in published:
                    if not entry.startswith(LOOPBACK_PREFIXES):
                        problems.append(
                            "%s: %s 的端口 %s 不是回环绑定（只允许 127.0.0.1:/"
                            "${KEYCLOAK_BIND_ADDRESS:-127.0.0.1}）"
                            % (job, service, entry)
                        )
        self.assertEqual(
            problems,
            [],
            "loopback-only 与 compose 端口绑定矛盾：%s" % problems,
        )

    def test_uncollected_endpoints_are_explicitly_registered(self):
        """「未采集」表的每一行必须显式登记 owner；项目 :9092 必须写明无鉴权、仅内网。"""
        _, rows = table_with_first_cell(
            section_lines(self.readme, CAPTURE_SECTION), UNCOLLECTED_TABLE_FIRST_CELL
        )
        problems = []
        for row in rows:
            service = tokens(row[0])[0] if tokens(row[0]) else row[0]
            status = row[-1]
            if UNCOLLECTED_MARKER not in status:
                problems.append("%s: 状态列没有声明“%s”：%s" % (service, UNCOLLECTED_MARKER, status))
            if "owner=" not in status:
                problems.append(
                    "%s: 缺少主责人登记（需在该行写明 owner=...，本轮 scope-out 不得静默缺省）"
                    % service
                )
            if service == "project-worker":
                joined = " ".join(row)
                for required in ("无应用层鉴权", "内网"):
                    if required not in joined:
                        problems.append(
                            "project-worker: 端点/状态列必须写明“%s”（:9092 无应用层鉴权、仅内网）"
                            % required
                        )
        self.assertEqual(
            problems,
            [],
            "「未采集」表的显式登记不完整：%s" % problems,
        )

    def test_alert_expressions_use_literal_5xx_status(self):
        """platform_http_* 的 status 选择器必须写等值 `status="5xx"`，不能用正则/区间。"""
        expressions = alert_expressions(read(PLATFORM_ALERTS))
        self.assertTrue(expressions, "platform-alerts.yml 未解析出任何 expr（本门禁依赖该结构）")
        problems = []
        for expression in expressions:
            if not any(metric in expression for metric in HTTP_STATUS_METRICS):
                continue
            for value in STATUS_REGEX_SELECTOR.findall(expression):
                if value not in STATUS_BUCKETS:
                    problems.append(
                        "status=~\"%s\"（status 是分桶枚举字符串，正则/区间写法永远匹配不到；"
                        "只能用等值 status=\"5xx\"）：%s" % (value, expression)
                    )
            if STATUS_NUMERIC_SELECTOR.search(expression):
                problems.append(
                    "status 使用了数字比较（标签值是 1xx/2xx/3xx/4xx/5xx 字符串）：%s"
                    % expression
                )
        self.assertEqual(
            problems,
            [],
            "platform-alerts.yml 的 5xx 选择器写错（规则会静默永不触发）：%s" % problems,
        )
        self.assertTrue(
            [
                expression
                for expression in expressions
                if "platform_http_errors_total" in expression
                and STATUS_5XX_EQUALITY.search(expression)
            ],
            "缺少使用 platform_http_errors_total{status=\"5xx\"} 的错误率规则；"
            "删除 5xx 规则或改成 =~\"5..\" 都会让 5xx 告警静默",
        )


if __name__ == "__main__":
    unittest.main()
