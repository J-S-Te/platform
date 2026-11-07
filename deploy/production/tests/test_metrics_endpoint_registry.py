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
   prometheus.yml 一致。

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
COMPOSE = PRODUCTION / "docker-compose.yml"

# README 的章节标题（不含 # 前缀，标题级别由出现顺序决定）。
BOUNDARY_SECTION = "监控抓取与指标端点边界"
CAPTURE_SECTION = "新增抓取 job 与指标清单"
COLLECTED_TABLE_FIRST_CELL = "抓取 job"
UNCOLLECTED_TABLE_FIRST_CELL = "服务（无抓取 job）"

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
    level = len(lines[start]) - len(lines[start].lstrip())
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


class MetricsEndpointRegistryTest(unittest.TestCase):
    """README 声明、prometheus 抓取配置与 compose 服务名必须三方一致。"""

    @classmethod
    def setUpClass(cls):
        cls.readme = read(README)
        cls.prometheus_jobs = parse_prometheus(read(PROMETHEUS))
        cls.services = parse_compose_services(read(COMPOSE))

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


if __name__ == "__main__":
    unittest.main()
