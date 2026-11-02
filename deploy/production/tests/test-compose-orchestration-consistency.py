#!/usr/bin/env python3
"""生产编排一致性门禁（BIZ-XWIRE-08）：权威编排 vs 参考副本、监控抓取、告警接线。

背景与权威关系
--------------
生产唯一权威编排是 docker-compose.yml（离线资产包只发布它；deploy-service.sh、
lifecycle.sh、publish-runtime-approvals.py 与 provisioner 的 SUBSYSTEM_PRODUCTION_COMPOSE_FILE
都指向它）。同目录 compose.yaml 是“不得用于部署”的评审参考副本，
compose.observability.yaml 是被权威编排 monitoring 子系统取代的历史覆盖示例。

本用例把“不要再按错文件部署 / 不要再漂移”固化成回归守卫：

1. 三个编排文件的权威标记必须存在；
2. 参考副本不得出现已废弃的部署根 /opt/basic-platform，也不得把 provisioner 的
   SUBSYSTEM_PRODUCTION_COMPOSE_FILE 指回 compose.yaml（历史事故：脚本按错文件/错根目录操作）；
3. 两份文件的服务集合与卷集合必须完全相同；
4. 同名服务的 image / 已发布端口 / 网络 / restart / environment 键集不得矛盾，
   且商业许可接线（runtime/license-*.env、license-state-*:/var/lib/commercial-license、
   LICENSE_RUNTIME_STATE_DIRECTORY）与健康检查**禁止少写**：权威编排声明了什么，
   参考副本就必须声明同样内容（参考副本只允许省略注释/日志/资源上限这类非关键键）；
4b. 健康检查的覆盖规则（O6）：常驻服务（restart 不是 "no"）必须有 healthcheck，
   一次性任务（restart: "no"）必须没有；无 HTTP 端点的 Worker 用 PID1 可执行文件探针，
   探针里的二进制必须与容器实际命令/镜像 ENTRYPOINT 一致（见 PID1_EXPECTED）；
5. Prometheus 抓取目标必须都是已声明的服务，且 contract / platform / settlement 的
   /metrics 已被抓取；平台 job 的鉴权必须按 RequireMetricsAccess 实现配置为
   Bearer + credentials_file，仓库与 prometheus.yml 内不得出现真实 token；
6. 指标端口不得发布到宿主机，前端 nginx 不得出现 /metrics 路由（网络层即边界）；
7. 平台/结算的告警规则文件必须存在、被 rule_files 引用、被只读挂载进容器，
   且告警表达式里的指标名必须在“代码实际输出”的白名单内（禁止臆造指标名）；
8. alert-worker 必须同时具备租户隔离（来自 runtime 文件）与默认关闭的
   ALERT_AUTO_RESOLVE 开关；
9. 历史 observability 覆盖文件的关键键必须与权威编排 monitoring 块一致。

解析方式沿用仓库既有做法：本机与 CI 的 python3 都没有 pyyaml，因此调用
ruby -ryaml 解析成 JSON（见 tests/test-compose-one-shot-parity.py）。

本用例**不得**加进 bin/build-offline-packages.sh 的静态资产清单：compose.yaml 与
compose.observability.yaml 不随资产包发布，加了会让离线包内测试必然失败。
（prometheus 规则文件本身在那份清单里，见 bin/build-offline-packages.sh 的
monitoring/prometheus/*.yml 条目。）
"""
from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

PRODUCTION = Path(__file__).resolve().parents[1]
# 测试目录 → production → deploy → platform → 工作区根（兄弟仓库评审时才存在）。
WORKSPACE = PRODUCTION.parents[2]
CANONICAL = "docker-compose.yml"
REFERENCE = "compose.yaml"
OVERLAY = "compose.observability.yaml"
COMPOSE_FILES = (CANONICAL, REFERENCE)

PROMETHEUS_CONFIG = PRODUCTION / "monitoring" / "prometheus" / "prometheus.yml"
PROMETHEUS_DIR = PROMETHEUS_CONFIG.parent
DATA_ANALYSIS_TEMPLATE = PRODUCTION / "subsystem-templates" / "data-analysis.env.example"
ROOT_ENV_EXAMPLE = PRODUCTION / ".env.example"
DATA_ANALYSIS_COMPOSE = WORKSPACE / "data_analysis" / "compose.yaml"
FRONTEND_NGINX = WORKSPACE / "frontend" / "nginx"

# 本轮新增的告警规则文件：平台自身与跨系统集成（XEVT2-07 / XAPI-10）。
NEW_RULE_FILES = ("platform-alerts.yml", "integration-alerts.yml")
# 平台 /metrics 的抓取令牌容器内路径（RequireMetricsAccess 的 Bearer 凭据文件）。
METRICS_CREDENTIAL_FILE = "/etc/prometheus/secrets/platform-metrics-token"
# 凭据目录的宿主来源必须是 runtime/（被 .gitignore 忽略、不随资产包发布）。
METRICS_CREDENTIAL_VOLUME = "./runtime/prometheus-secrets:/etc/prometheus/secrets:ro"
# 新抓取 job → 期望 target；bool 表示该端点是否要求 Bearer 凭据。
REQUIRED_SCRAPE_JOBS = {
    "platform-api": {"target": "platform-api:8080", "authorized": True},
    "settlement-api": {"target": "settlement-api:8085", "authorized": False},
}

# 告警表达式允许出现的指标名：全部来自代码实际输出，新增指标必须先有代码与文档。
# 白名单只对 NEW_RULE_FILES 生效；keycloak / file-gateway 的历史规则不在本轮范围内。
KNOWN_METRICS = {
    # platform/internal/platform/oidc/backchannel/observability.go:16,18
    "platform_oauth_backchannel_unregistered_rp",
    "platform_oauth_backchannel_logout_no_endpoint",
    # platform/internal/transport/http/metrics.go:113-119（每次抓取无条件输出）
    "platform_http_requests_total",
    "platform_http_errors_total",
    "platform_http_request_duration_seconds_sum",
    "platform_http_request_duration_seconds_count",
    "platform_authz_failures_total",
    "platform_oidc_login_failures_total",
    "platform_metric_collection_errors_total",
    # platform/internal/transport/http/metrics.go:73-79（队列/可靠性计数）
    "platform_keycloak_outbox_backlog",
    "platform_keycloak_outbox_dead",
    "platform_async_job_backlog",
    "platform_async_job_dead",
    "platform_oidc_backchannel_logout_backlog",
    "platform_oidc_backchannel_logout_failed",
    "platform_oidc_backchannel_logout_delivered",
    # platform/internal/transport/http/metrics.go:135-137,159-160
    "platform_database_open_connections",
    "platform_database_in_use_connections",
    "platform_database_wait_count_total",
    "platform_async_job_retries_total",
    "platform_async_job_last_success_timestamp_seconds",
    # Settlement/internal/httpapi/integration_status.go:101-130（BIZ-XAPI-10）
    "settlement_outbox_pending",
    "settlement_integration_destination_configured",
    "settlement_integration_audit_required",
    "settlement_outbox_unconfigured_pending",
    "settlement_outbox_pending_query_error",
    # contract_management/internal/transport/httpapi/metrics.go:54-66
    "contract_integration_outbox_dead",
    "contract_integration_outbox_dead_total",
    # platform/internal/transport/http/metrics.go（RUNTIME-05 Secret 退役面常量块）：
    # 单客户端 ACTIVE 数量、去重客户端总数、最老 ACTIVE 龄期、超过 90 天的 ACTIVE 总数。
    "platform_oauth_client_secret_active",
    "platform_oauth_client_secret_active_client",
    "platform_oauth_client_secret_active_clients",
    "platform_oauth_client_secret_oldest_active_seconds",
    "platform_oauth_client_secret_active_over_90d",
    "platform_oauth_client_secret_revoked_total",
    # Prometheus 自身抓取健康度（job 目标进程不可达时为 0）
    "up",
}
# 表达式里的函数/聚合/标签名：不是指标，不能算臆造指标。
EXPR_KEYWORDS = {
    "absent",
    "absent_over_time",
    "and",
    "avg",
    "bool",
    "bottomk",
    "by",
    "ceil",
    "changes",
    "clamp_max",
    "clamp_min",
    "count",
    "count_values",
    "day_of_month",
    "day_of_week",
    "delta",
    "deriv",
    "destination",
    "floor",
    "group_left",
    "group_right",
    "histogram_quantile",
    "hour",
    "idelta",
    "ignoring",
    "increase",
    "instance",
    "irate",
    "job",
    "kind",
    "label_join",
    "label_replace",
    "max",
    "min",
    "month",
    "offset",
    "on",
    "or",
    "predict_linear",
    "quantile",
    "rate",
    "resets",
    "round",
    "scalar",
    "severity",
    "sort",
    "sort_desc",
    "stddev",
    "sum",
    "time",
    "topk",
    "unless",
    "vector",
    "without",
    "year",
}

# 指标端点所在端口：只能在内网被抓取。业务服务的指标端口允许“仅回环”发布
# （127.0.0.1 绑定，供本机受控验收），绝不允许绑定 0.0.0.0/容器外网卡。
METRICS_PORTS = {
    "platform-api": 8080,       # /metrics 带 METRICS_ACCESS_TOKEN（RequireMetricsAccess）
    "contract-api": 8081,       # /metrics 为集成 Outbox 聚合计数
    "settlement-api": 8085,     # /metrics 为 XAPI-10 集成目的地与 outbox 积压
    "file-gateway": 8086,       # /metrics 为容量/连接数
    "keycloak": 9000,           # 管理端点
    "prometheus": 9090,
    "keycloak-backup-metrics": 9100,
}
# 这些服务是纯监控/指标端点，必须完全不发布端口。
NO_PUBLISHED_PORTS = ("file-gateway", "prometheus", "keycloak-backup-metrics")
# 告警 worker 的租户隔离必须来自 runtime 文件，Compose 不得给 OIDC_TENANT_ID 设默认值。
ALERT_WORKER = "data-analysis-alert-worker"
ALERT_SWITCH = "ALERT_AUTO_RESOLVE"
ALERT_SWITCH_DEFAULT = "${ALERT_AUTO_RESOLVE:-false}"

# 商业许可接线的三个可检查面（BIZ-XWIRE-08）。许可运行时把“缺接线”当成未授权，
# 所以参考副本少写任何一项，被评审照抄进真实部署后该消费者会直接 fail-closed。
LICENSE_ENV_PREFIX = "runtime/license-"
LICENSE_STATE_MOUNT_SUFFIX = ":/var/lib/commercial-license"
LICENSE_STATE_VOLUME_PREFIX = "license-state-"
LICENSE_STATE_ENV = "LICENSE_RUNTIME_STATE_DIRECTORY"
# O5：许可运行态键。COMMERCIAL_LICENSE_ENABLED 及其同组键由平台写入
# runtime/license-<service>.env；COMMERCIAL_LICENSE_COMPATIBILITY 是兼容放行开关，
# 只有归一后等于 true 才生效，生产必须缺省（缺省即 fail-closed）。
LICENSE_RUNTIME_KEYS = ("COMMERCIAL_LICENSE_ENABLED", "COMMERCIAL_LICENSE_COMPATIBILITY")
SUBSYSTEM_TEMPLATES_DIR = PRODUCTION / "subsystem-templates"
SUBSYSTEMS_DIR = PRODUCTION / "subsystems.d"
# 健康检查按权威逐键比对；start_period 只影响启动窗口，不参与一致性判定。
HEALTHCHECK_KEYS = ("test", "interval", "timeout", "retries")
# 一次性任务的重启策略：迁移/初始化/巡检跑完就退出，不能有 healthcheck。
# 容器退出后 healthcheck 只会停在 unhealthy，`compose up --wait` 会把它当成启动失败，
# 于是“给短命任务补探针”不是加固而是把发布门禁改坏。常驻服务则相反，必须可判定。
ONE_SHOT_RESTART = "no"
# 无 HTTP 端点的常驻 Worker 用 PID1 可执行文件探针（与 platform-worker 同一判据）：
# `test "$(readlink /proc/1/exe)" = /app/<binary>`。探针二进制 = 镜像 WORKDIR(/app) +
# command 声明的可执行文件；未覆盖 command 的服务由镜像 ENTRYPOINT 决定。
PID1_PROBE_MARKER = "/proc/1/exe"
PID1_EXPECTED = {
    # 权威编排 command: ["./<binary>"]，镜像 WORKDIR 为 /app。
    "platform-worker": "/app/worker",
    "customer-contract-transfer-worker": "/app/contract-transfer-worker",
    "customer-opportunity-alert-worker": "/app/opportunity-alert-worker",
    "customer-owner-notification-worker": "/app/opportunity-owner-notification-worker",
    "customer-presale-alert-worker": "/app/presale-alert-worker",
    "customer-presale-assignment-notification-worker": "/app/presale-assignment-notification-worker",
    "customer-presale-progress-notification-worker": "/app/presale-progress-notification-worker",
    "customer-notification-delivery-worker": "/app/notification-delivery-worker",
    "customer-presale-worker": "/app/presale-worker",
    "portal-invite-compensation-worker": "/app/portal-invite-compensation-worker",
    "portal-report-worker": "/app/portal-report-worker",
    # 这两个服务不覆盖 command，PID1 来自镜像 ENTRYPOINT：
    # data_analysis/Dockerfile 的 aggregation-worker → /app/aggregation-worker，
    # alert-worker → /app/alert-worker。
    "data-analysis-aggregation-worker": "/app/aggregation-worker",
    "data-analysis-alert-worker": "/app/alert-worker",
}
# 探针里出现的二进制路径（从 healthcheck.test 抽取），用于核对“探针指向真实 PID1”。
# CMD-SHELL 全文形如：test "$$(readlink /proc/1/exe)" = /app/<binary>（路径在行尾）。
PID1_BINARY_PATTERN = re.compile(r'=\s*(/app/\S+?)\s*$')

RUBY_LOADER = (
    'require "json"; require "yaml"; '
    "doc = YAML.safe_load(File.read(ARGV[0]), permitted_classes: [Symbol], aliases: true); "
    "puts JSON.generate(doc)"
)


def load_compose(filename: str) -> dict:
    """用 ruby -ryaml 加载一份 Compose 文件（还原 anchor/merge），失败即中文断言错误。"""
    path = PRODUCTION / filename
    result = subprocess.run(
        ["ruby", "-e", RUBY_LOADER, str(path)], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        raise AssertionError(
            f"无法解析 {filename}：ruby -ryaml 返回 {result.returncode}，stderr={result.stderr.strip()!r}"
        )
    return json.loads(result.stdout)


def load_yaml(path: Path) -> dict:
    """用 ruby -ryaml 加载任意 YAML（prometheus.yml / 告警规则），失败即中文断言错误。"""
    result = subprocess.run(
        ["ruby", "-e", RUBY_LOADER, str(path)], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        raise AssertionError(
            f"无法解析 {path}：ruby -ryaml 返回 {result.returncode}，stderr={result.stderr.strip()!r}"
        )
    return json.loads(result.stdout)


def scrape_targets(job: dict) -> list[str]:
    """返回一个抓取 job 的全部 static_configs target（保持声明顺序）。"""
    targets: list[str] = []
    for config in job.get("static_configs") or []:
        for target in config.get("targets") or []:
            targets.append(str(target))
    return targets


def load_alert_rules(filename: str) -> list[dict]:
    """读取告警规则文件，返回带上所属 group 名称的规则列表。"""
    document = load_yaml(PROMETHEUS_DIR / filename)
    rules: list[dict] = []
    for group in document.get("groups") or []:
        self_group = str(group.get("name"))
        for rule in group.get("rules") or []:
            rules.append({**rule, "group": self_group, "file": filename})
    return rules


def metric_tokens(expr: str) -> set[str]:
    """从 PromQL 表达式里提取“疑似指标名”的标识符。

    先删除带引号的字符串（标签匹配值不是指标名）与区间选择器（如 [10m]，其单位 m 不是指标），
    再取标识符，最后剔除函数/聚合/标签名。
    这样 `settlement_outbox_pending{destination="PLATFORM_AUDIT"} > 200` 只会留下
    `settlement_outbox_pending`，而 `sum(contract_integration_outbox_dead)` 不会把
    `sum` 当成指标。
    """
    without_literals = re.sub(r'"[^"]*"', '""', expr)
    without_ranges = re.sub(r"\[[^\]]*\]", "[]", without_literals)
    tokens = set(re.findall(r"[a-z_][a-z0-9_]*", without_ranges))
    return {token for token in tokens if token not in EXPR_KEYWORDS}


def env_file_paths(service: dict) -> list[str]:
    """归一 env_file 的字符串/映射两种写法，返回有效引用路径（保持声明顺序）。"""
    entries = service.get("env_file") or []
    if isinstance(entries, str):
        entries = [entries]
    paths: list[str] = []
    for entry in entries:
        path = entry.get("path") if isinstance(entry, dict) else entry
        if isinstance(path, str) and path.strip():
            paths.append(path)
    return paths


def environment_of(service: dict) -> dict[str, str]:
    """返回服务的 environment 映射；值统一转成可比较的字符串。"""
    return {str(key): str(value) for key, value in (service.get("environment") or {}).items()}


def license_env_files(service: dict) -> list[str]:
    """返回服务声明的 runtime/license-*.env（保持声明顺序，去重后排序）。"""
    return sorted(
        {path for path in env_file_paths(service) if path.startswith(LICENSE_ENV_PREFIX)}
    )


def license_state_mounts(service: dict) -> list[str]:
    """返回商业许可状态卷挂载（归一成 "卷名:/var/lib/commercial-license" 形态）。"""
    mounts: set[str] = set()
    for entry in service.get("volumes") or []:
        if isinstance(entry, dict):
            text = f"{entry.get('source', '')}:{entry.get('target', '')}"
        else:
            text = str(entry)
        if text.startswith(LICENSE_STATE_VOLUME_PREFIX) and text.endswith(
            LICENSE_STATE_MOUNT_SUFFIX
        ):
            mounts.add(text)
    return sorted(mounts)


def healthcheck_of(service: dict, keys: tuple[str, ...] = HEALTHCHECK_KEYS) -> dict[str, object]:
    """返回健康检查中被门禁比较的键；服务没有健康检查时返回空映射。"""
    healthcheck = service.get("healthcheck") or {}
    return {key: healthcheck.get(key) for key in keys if key in healthcheck}


def pid1_probe_binary(service: dict) -> str | None:
    """返回 PID1 探针里断言的二进制路径；该服务没有 PID1 探针时返回 None。

    返回空字符串表示“声明了 /proc/1/exe 探针，但没有可识别的 `= /app/...` 断言”，
    调用方应把它当成失败（否则探针退化成恒真校验）。
    """
    test = (service.get("healthcheck") or {}).get("test") or []
    if not isinstance(test, list):
        return None
    text = " ".join(str(part) for part in test)
    if PID1_PROBE_MARKER not in text:
        return None
    match = PID1_BINARY_PATTERN.search(text)
    return match.group(1) if match else ""


def parse_scrape_jobs(path: Path) -> list[dict]:
    """极简解析 prometheus.yml 的 scrape_configs（文件结构扁平，无需完整 YAML 解析）。"""
    jobs: list[dict] = []
    current: dict | None = None
    in_scrape = False
    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if re.match(r"^scrape_configs:\s*$", line):
            in_scrape = True
            continue
        if not in_scrape:
            continue
        job = re.match(r"^\s*-\s+job_name:\s*(\S+)\s*$", line)
        if job:
            current = {"job": job.group(1), "metrics_path": "/metrics", "targets": []}
            jobs.append(current)
            continue
        if current is None:
            continue
        metric = re.match(r"^\s*metrics_path:\s*(\S+)\s*$", line)
        if metric:
            current["metrics_path"] = metric.group(1)
            continue
        targets = re.match(r"^\s*-\s+targets:\s*\[(.*)\]\s*$", line)
        if targets:
            for item in targets.group(1).split(","):
                target = item.strip().strip("\"'")
                if target:
                    current["targets"].append(target)
    return jobs


class OrchestrationConsistencyTest(unittest.TestCase):
    """两份生产编排 + 监控抓取 + 告警接线的一致性门禁。"""

    @classmethod
    def setUpClass(cls) -> None:
        cls.canonical = load_compose(CANONICAL)
        cls.reference = load_compose(REFERENCE)
        cls.overlay = load_compose(OVERLAY)

    def services(self, document: dict, filename: str) -> dict:
        services = document.get("services")
        self.assertIsInstance(services, dict, f"{filename} 缺少顶层 services 映射")
        return services

    def test_platform_central_session_uses_independent_runtime_env_file(self) -> None:
        """用途：用真实Compose解析保护独立runtime文件的中央会话开关。
        参数：无；输入来自两份真实编排的API和Worker环境块。
        返回值：无；缺Compose时明确跳过，覆盖runtime或解析失败时断言失败。
        """
        docker = shutil.which("docker")
        if docker is None:
            self.skipTest("缺少Docker CLI，无法执行Compose配置解析")
        # 只保留查找CLI及插件所需的环境，任何shell业务键均不能掩盖覆盖问题。
        clean_env = {key: os.environ[key] for key in ("PATH", "HOME") if key in os.environ}
        available = subprocess.run(
            [docker, "compose", "version"], capture_output=True, text=True,
            env=clean_env, check=False, timeout=30,
        )
        if available.returncode != 0:
            self.skipTest("Docker Compose不可用，未执行配置解析")
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            services = self.services(document, filename)
            for name in ("platform-api", "platform-worker"):
                service = services[name]
                for runtime_value in ("true", "false", None):
                    with self.subTest(filename=filename, service=name, runtime=runtime_value):
                        effective = self.render_runtime_environment(
                            docker, clean_env, service, runtime_value,
                        )
                        self.assertEqual(
                            effective.get("AUTH_CENTRAL_SESSION_ENABLED"), runtime_value,
                            f"{filename}/{name}覆盖独立runtime的中央会话配置；"
                            "缺省键必须留给应用默认策略处理",
                        )
                        self.assertEqual(effective.get("RUNTIME_SOURCE_MARKER"), "loaded")

    def render_runtime_environment(
        self, docker: str, clean_env: dict, service: dict, runtime_value: str | None,
    ) -> dict:
        """用途：以实际服务环境块构造最小Compose并解析最终容器环境，不访问daemon。
        参数：docker为CLI路径，clean_env为隔离进程环境，service为服务配置，runtime_value为开关输入。
        返回值：真实Compose合并后的环境；配置解析失败立即断言，不降级为静态检查。
        """
        with tempfile.TemporaryDirectory(prefix="platform-central-runtime-env-") as temporary:
            root = Path(temporary)
            runtime = root / "platform.env"
            runtime.write_text(
                "RUNTIME_SOURCE_MARKER=loaded\n" + (
                    f"AUTH_CENTRAL_SESSION_ENABLED={runtime_value}\n"
                    if runtime_value is not None else ""
                ), encoding="utf-8",
            )
            interpolation = root / ".env"
            interpolation.write_text(
                f"BASIC_PLATFORM_RUNTIME_ENV_FILE={runtime}\n", encoding="utf-8",
            )
            candidate = root / "compose.json"
            candidate.write_text(json.dumps({"services": {"probe": {
                "image": "scratch",
                "env_file": service.get("env_file"),
                "environment": service.get("environment"),
            }}}), encoding="utf-8")
            rendered = subprocess.run(
                [docker, "compose", "--project-name", "central-session-env-regression",
                 "--project-directory", temporary, "--env-file", str(interpolation),
                 "-f", str(candidate), "config", "--format", "json"],
                env=clean_env, capture_output=True, text=True, check=False, timeout=30,
            )
            self.assertEqual(rendered.returncode, 0, "真实Compose配置解析失败")
            return json.loads(rendered.stdout)["services"]["probe"].get("environment", {})

    # ---------- 1. 权威标记 ----------
    def test_authority_markers_name_the_single_deployable_file(self) -> None:
        """三份文件都必须自述权威关系，避免运维按参考副本部署。"""
        canonical_text = (PRODUCTION / CANONICAL).read_text(encoding="utf-8")
        reference_text = (PRODUCTION / REFERENCE).read_text(encoding="utf-8")
        overlay_text = (PRODUCTION / OVERLAY).read_text(encoding="utf-8")

        self.assertIn("生产唯一编排入口", canonical_text,
                      f"{CANONICAL} 必须声明自己是唯一编排入口")
        self.assertIn("唯一权威编排", canonical_text,
                      f"{CANONICAL} 必须声明自己是权威编排（BIZ-XWIRE-08）")
        self.assertIn(REFERENCE, canonical_text,
                      f"{CANONICAL} 必须点名 {REFERENCE} 是参考副本，而不是部署入口")

        self.assertIn("本文件不是部署入口", reference_text,
                      f"{REFERENCE} 必须显式声明不得用于部署")
        self.assertIn(CANONICAL, reference_text,
                      f"{REFERENCE} 必须指向权威编排 {CANONICAL}")
        self.assertIn("非关键键", reference_text,
                      f"{REFERENCE} 必须声明自身只省略注释/日志类非关键键，"
                      "避免被当成等价副本或免责样本")
        self.assertIn("与权威一致", reference_text,
                      f"{REFERENCE} 必须声明许可接线与健康检查与 {CANONICAL} 一致"
                      "（BIZ-XWIRE-08：禁止少写）")

        self.assertIn("历史覆盖示例", overlay_text,
                      f"{OVERLAY} 必须声明已被权威编排 monitoring 子系统取代")
        self.assertIn(CANONICAL, overlay_text,
                      f"{OVERLAY} 必须指向权威编排 {CANONICAL}")

    def test_reference_has_no_stale_deploy_root_or_self_referential_compose(self) -> None:
        """参考副本不得残留旧部署根 /opt/basic-platform 或把 provisioner 指回 compose.yaml。"""
        reference_text = (PRODUCTION / REFERENCE).read_text(encoding="utf-8")
        self.assertNotIn("/opt/basic-platform", reference_text,
                         f"{REFERENCE} 残留已废弃部署根 /opt/basic-platform；"
                         "provisioner 会把 runtime 文件写到错误目录")
        services = self.services(self.reference, REFERENCE)
        provisioner_env = environment_of(services["subsystem-provisioner"])
        compose_file = provisioner_env.get("SUBSYSTEM_PRODUCTION_COMPOSE_FILE", "")
        self.assertTrue(compose_file.endswith("/" + CANONICAL),
                        f"{REFERENCE} 的 provisioner SUBSYSTEM_PRODUCTION_COMPOSE_FILE "
                        f"必须指向 {CANONICAL}：实际 {compose_file!r}")

    # ---------- 2. 结构一致性 ----------
    def test_service_and_volume_sets_are_identical(self) -> None:
        """服务集合与卷集合必须完全相同：少一个服务就意味着“按错文件部署会少一层能力”。"""
        canonical_services = self.services(self.canonical, CANONICAL)
        reference_services = self.services(self.reference, REFERENCE)
        self.assertEqual(
            sorted(canonical_services),
            sorted(reference_services),
            f"{REFERENCE} 与 {CANONICAL} 的服务集合已漂移："
            f"仅权威有 {sorted(set(canonical_services) - set(reference_services))}，"
            f"仅参考有 {sorted(set(reference_services) - set(canonical_services))}",
        )
        canonical_volumes = sorted((self.canonical.get("volumes") or {}))
        reference_volumes = sorted((self.reference.get("volumes") or {}))
        self.assertEqual(
            canonical_volumes,
            reference_volumes,
            f"{REFERENCE} 与 {CANONICAL} 的卷集合已漂移："
            f"仅权威有 {sorted(set(canonical_volumes) - set(reference_volumes))}，"
            f"仅参考有 {sorted(set(reference_volumes) - set(canonical_volumes))}",
        )

    def test_shared_services_never_contradict_the_authority(self) -> None:
        """同名服务的镜像/端口/网络/重启策略/environment 不得与权威编排矛盾。

        参考副本允许“少写”（例如省略 runtime/license-*.env、缺 logging、stop_grace_period），
        但不允许“写错”：写错的键会被运维当成等价配置照抄。
        """
        canonical_services = self.services(self.canonical, CANONICAL)
        reference_services = self.services(self.reference, REFERENCE)
        for name in sorted(set(canonical_services) & set(reference_services)):
            authoritative = canonical_services[name]
            copied = reference_services[name]
            with self.subTest(service=name):
                for key in ("image", "ports", "networks", "restart"):
                    self.assertEqual(
                        copied.get(key),
                        authoritative.get(key),
                        f"{REFERENCE} 的 {name} 键 {key} 与 {CANONICAL} 矛盾："
                        f"权威 {authoritative.get(key)!r}，参考 {copied.get(key)!r}",
                    )
                authoritative_env = environment_of(authoritative)
                copied_env = environment_of(copied)
                self.assertEqual(
                    sorted(authoritative_env),
                    sorted(copied_env),
                    f"{REFERENCE} 的 {name} environment 键集与 {CANONICAL} 不一致："
                    f"仅权威有 {sorted(set(authoritative_env) - set(copied_env))}，"
                    f"仅参考有 {sorted(set(copied_env) - set(authoritative_env))}",
                )
                for key in sorted(set(authoritative_env) & set(copied_env)):
                    self.assertEqual(
                        copied_env[key],
                        authoritative_env[key],
                        f"{REFERENCE} 的 {name} 环境变量 {key} 与 {CANONICAL} 矛盾："
                        f"权威 {authoritative_env[key]!r}，参考 {copied_env[key]!r}",
                    )

    def test_reference_never_invents_extra_runtime_env_files(self) -> None:
        """参考副本不得新增权威编排没有的 env_file（会让“照抄参考”读到不同运行配置）。"""
        canonical_services = self.services(self.canonical, CANONICAL)
        reference_services = self.services(self.reference, REFERENCE)
        for name in sorted(set(canonical_services) & set(reference_services)):
            authoritative = set(env_file_paths(canonical_services[name]))
            copied = set(env_file_paths(reference_services[name]))
            with self.subTest(service=name):
                self.assertEqual(
                    sorted(copied - authoritative),
                    [],
                    f"{REFERENCE} 的 {name} 声明了 {CANONICAL} 没有的 env_file："
                    f"{sorted(copied - authoritative)}",
                )

    def test_reference_never_omits_license_wiring(self) -> None:
        """许可接线“禁止少写”（BIZ-XWIRE-08）：env_file、状态卷、状态目录键逐服务对齐。

        商业许可运行时把“没有接线”当成未授权运行：少写 runtime/license-*.env 就没有
        许可证运行态凭据，少写 license-state-*:/var/lib/commercial-license 就没有可写
        状态目录，少写 LICENSE_RUNTIME_STATE_DIRECTORY 就会退回镜像内默认路径。参考副本
        被评审当作等价样本，因此这些键一律不许比权威编排少一个。

        方向刻意是“参考副本 ⊇ 权威”：哪些服务需要许可接线由权威编排决定（platform-api /
        platform-worker 这类不走 license-*.env 的消费者只声明状态目录），本用例不替权威
        发明新要求，只禁止参考副本少写。
        """
        canonical_services = self.services(self.canonical, CANONICAL)
        reference_services = self.services(self.reference, REFERENCE)
        consumers = sorted(
            name
            for name, service in canonical_services.items()
            if license_env_files(service)
        )
        self.assertTrue(
            consumers,
            f"{CANONICAL} 没有声明任何 {LICENSE_ENV_PREFIX}*.env 消费者："
            "许可接线守卫已失去检查对象，先修复权威编排再谈一致性",
        )
        for name in consumers:
            with self.subTest(service=name):
                self.assertIn(name, reference_services, f"{REFERENCE} 缺少服务 {name}")
                authoritative = canonical_services[name]
                copied = reference_services[name]
                self.assertEqual(
                    sorted(set(license_env_files(authoritative)) - set(license_env_files(copied))),
                    [],
                    f"{REFERENCE} 的 {name} 少写了权威编排声明的许可运行配置文件："
                    f"缺少 {sorted(set(license_env_files(authoritative)) - set(license_env_files(copied)))}；"
                    "缺少 license-*.env 的消费者会按未授权 fail-closed",
                )
                self.assertEqual(
                    license_state_mounts(copied),
                    license_state_mounts(authoritative),
                    f"{REFERENCE} 的 {name} 商业许可状态卷与 {CANONICAL} 不一致："
                    f"权威 {license_state_mounts(authoritative)}，参考 {license_state_mounts(copied)}",
                )
                if LICENSE_STATE_ENV in environment_of(authoritative):
                    self.assertIn(
                        LICENSE_STATE_ENV,
                        environment_of(copied),
                        f"{REFERENCE} 的 {name} 少写了 {LICENSE_STATE_ENV}："
                        "许可状态目录会退回镜像内默认路径，与权威编排的状态卷不一致",
                    )

    def test_license_compatibility_switch_is_never_wired_in_production(self) -> None:
        """兼容放行开关不得出现在编排/模板/清单里（O5）：缺省即 fail-closed。

        `COMMERCIAL_LICENSE_COMPATIBILITY` 在 license-core 里是“允许以未授权方式启动”的
        兼容放行开关（只有归一后等于 `true` 才生效，且仅适用于从未登记的组件）；
        `COMMERCIAL_LICENSE_ENABLED` 及其同组运行态键由平台按受控接入写入
        `runtime/license-<service>.env`，`runtime/` 不进版本库也不进资产包。

        因此生产部署资产里任何一处默认值都必须是“不声明”：写进模板/清单会被“按模板重建”
        覆盖平台签发的凭据，写进 Compose `environment` 会盖掉 fail-closed 判定。这条守卫
        同时锁住 O5 的核对结论——缺说明时先补 README/ACCEPTANCE 的解释，而不是补默认值。
        """
        # 服务 environment 里这两类键一个都不许出现（env_file 引用 runtime/license-*.env 不在范围）。
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            services = self.services(document, filename)
            for name, service in sorted(services.items()):
                env = environment_of(service)
                for key in LICENSE_RUNTIME_KEYS:
                    with self.subTest(filename=filename, service=name, key=key):
                        self.assertNotIn(
                            key,
                            env,
                            f"{filename} 的 {name} 在 Compose environment 里声明了 {key}："
                            "该键只能由平台写入 runtime/license-<service>.env，"
                            "Compose 默认值会盖掉运行态凭据或 fail-closed 判定",
                        )

        # 模板与清单同样只允许出现解释性注释，不允许声明这些键。
        assets = sorted(SUBSYSTEM_TEMPLATES_DIR.glob("*.env.example")) + sorted(
            SUBSYSTEMS_DIR.glob("*.yaml")
        )
        self.assertGreater(len(assets), 0, "没有找到任何子系统模板或清单：O5 守卫失去检查对象")
        declaration = re.compile(
            r"^\s*(?:export\s+)?(" + "|".join(LICENSE_RUNTIME_KEYS) + r")\s*[=:]"
        )
        for path in assets:
            with self.subTest(asset=path.name):
                for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
                    if line.strip().startswith("#"):
                        continue  # 解释性注释是允许的（模板/清单里写明了为什么不声明）。
                    match = declaration.match(line)
                    if match is None:
                        continue
                    self.fail(
                        f"{path.name}:{number} 声明了 {match.group(1)}："
                        "许可运行态键由平台写入 runtime/license-<service>.env，"
                        f"{match.group(1)} 生产必须缺省（兼容开关缺省即 fail-closed）"
                    )

    def test_reference_healthchecks_match_the_authority(self) -> None:
        """健康检查“禁止少写”（BIZ-XWIRE-08）：test/interval/timeout/retries 与权威一致。
        参考副本缺健康检查时，照抄它的部署会失去 compose 的 service_healthy 语义
        （depends_on: condition: service_healthy 直接失效），也会失去发布脚本判断就绪
        的依据。参考副本同样不得发明权威编排没有的健康检查，否则两处对“就绪”的定义分叉。
        """
        canonical_services = self.services(self.canonical, CANONICAL)
        reference_services = self.services(self.reference, REFERENCE)
        compared = 0
        for name in sorted(set(canonical_services) & set(reference_services)):
            authoritative = canonical_services[name]
            copied = reference_services[name]
            authoritative_check = healthcheck_of(authoritative)
            copied_check = healthcheck_of(copied)
            with self.subTest(service=name):
                self.assertEqual(
                    sorted(copied_check),
                    sorted(authoritative_check),
                    f"{REFERENCE} 的 {name} healthcheck 键集与 {CANONICAL} 不一致："
                    f"权威 {sorted(authoritative_check)}，参考 {sorted(copied_check)}",
                )
                for key in sorted(authoritative_check):
                    self.assertEqual(
                        copied_check[key],
                        authoritative_check[key],
                        f"{REFERENCE} 的 {name} healthcheck.{key} 与 {CANONICAL} 矛盾："
                        f"权威 {authoritative_check[key]!r}，参考 {copied_check[key]!r}",
                    )
                if authoritative_check:
                    compared += 1
        self.assertGreater(
            compared,
            0,
            f"{CANONICAL} 与 {REFERENCE} 的同名服务里没有任何健康检查可比较："
            "守卫已失去检查对象，先修复权威编排再谈一致性",
        )

    def test_every_long_running_service_declares_a_healthcheck(self) -> None:
        """常驻服务必须可判定就绪（O6）：restart 不是 "no" 的服务都要有 healthcheck。

        常驻服务没有 healthcheck 时，`depends_on: condition: service_healthy` 与发布脚本
        的 `compose up --wait` 只能退化成“容器在运行”，重启循环/启动即退出都无法被发现。
        两份编排都必须满足，因为参考副本会被评审当作等价样本照抄。
        """
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            services = self.services(document, filename)
            long_running = sorted(
                name
                for name, service in services.items()
                if str((service or {}).get("restart") or "") != ONE_SHOT_RESTART
            )
            self.assertTrue(long_running, f"{filename} 没有解析出任何常驻服务")
            missing = [
                name for name in long_running if not healthcheck_of(services[name])
            ]
            with self.subTest(filename=filename):
                self.assertEqual(
                    missing,
                    [],
                    f"{filename} 的常驻服务缺少 healthcheck：{missing}；"
                    "常驻服务没有探针时发布脚本只能看到“容器在运行”",
                )

    def test_one_shot_services_declare_no_healthcheck(self) -> None:
        """一次性任务不得有 healthcheck（O6 判定：语义上不需要，补了会改坏发布门禁）。

        `restart: "no"` 的迁移/初始化/巡检容器跑完就退出；退出后 healthcheck 只会停在
        `unhealthy`，于是 `compose up --wait` 把“成功完成”误判为启动失败。这条断言同时
        锁住“短命任务不补探针”的结论，避免下一次评审把它当成缺口又补一遍。
        """
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            services = self.services(document, filename)
            for name in sorted(services):
                service = services[name]
                if str((service or {}).get("restart") or "") != ONE_SHOT_RESTART:
                    continue
                with self.subTest(filename=filename, service=name):
                    self.assertEqual(
                        healthcheck_of(service),
                        {},
                        f"{filename} 的 {name} 是一次性任务（restart: \"no\"），"
                        "不得声明 healthcheck：退出后探针只会停在 unhealthy，"
                        "`compose up --wait` 会把成功完成误判为失败",
                    )

    def test_services_depended_on_by_service_healthy_declare_a_healthcheck(self) -> None:
        """被 service_healthy 依赖的服务必须有 healthcheck，否则等待条件形同虚设。"""
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            services = self.services(document, filename)
            for name, service in sorted(services.items()):
                dependencies = (service or {}).get("depends_on") or {}
                if not isinstance(dependencies, dict):
                    continue
                for target, options in sorted(dependencies.items()):
                    condition = (options or {}).get("condition") if isinstance(options, dict) else None
                    if condition != "service_healthy":
                        continue
                    with self.subTest(filename=filename, service=name, dependency=target):
                        self.assertIn(
                            target,
                            services,
                            f"{filename} 的 {name} 依赖了不存在的服务 {target}",
                        )
                        self.assertTrue(
                            healthcheck_of(services[target]),
                            f"{filename} 的 {name} 用 service_healthy 等待 {target}，"
                            f"但 {target} 没有 healthcheck：等待条件会退化成“容器在运行”",
                        )

    def test_pid1_probes_point_at_the_real_worker_binary(self) -> None:
        """PID1 探针必须断言真实的 Worker 二进制，不能写错路径或退化成恒真。

        探针期望路径 = `/app/` + 容器实际运行的二进制：服务覆盖 `command` 时取该命令的
        可执行文件名，未覆盖时取镜像 ENTRYPOINT（权威与参考副本必须一致，键集已由
        `test_reference_healthchecks_match_the_authority` 逐键比对）。新增 PID1 探针必须
        在此登记期望路径，否则“探针通过”只能证明容器里还有个进程。
        """
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            services = self.services(document, filename)
            probed = {
                name: pid1_probe_binary(service)
                for name, service in sorted(services.items())
                if pid1_probe_binary(service) is not None
            }
            with self.subTest(filename=filename, check="registry"):
                self.assertEqual(
                    sorted(probed),
                    sorted(PID1_EXPECTED),
                    f"{filename} 的 PID1 探针服务集合与门禁登记表不一致："
                    f"仅编排有 {sorted(set(probed) - set(PID1_EXPECTED))}，"
                    f"仅登记表有 {sorted(set(PID1_EXPECTED) - set(probed))}",
                )
            for name, binary in sorted(probed.items()):
                with self.subTest(filename=filename, service=name):
                    self.assertEqual(
                        binary,
                        PID1_EXPECTED[name],
                        f"{filename} 的 {name} PID1 探针断言了 {binary!r}，"
                        f"与登记的 Worker 二进制 {PID1_EXPECTED[name]!r} 不一致",
                    )
                    command = (services[name].get("command") or [])
                    if not command:
                        continue  # 未覆盖 command：由镜像 ENTRYPOINT 决定，登记表即证据。
                    expected = "/app/" + str(command[0]).split("/")[-1]
                    self.assertEqual(
                        binary,
                        expected,
                        f"{filename} 的 {name} command 是 {command[0]!r}，"
                        f"PID1 探针必须断言 {expected!r}：实际 {binary!r}",
                    )

    # ---------- 3. 监控抓取 ----------
    def test_prometheus_scrapes_contract_metrics_inside_the_cluster(self) -> None:
        """必须抓取 contract-api 的 /metrics，且抓取目标只能是已声明的服务名。"""
        jobs = {job["job"]: job for job in parse_scrape_jobs(PROMETHEUS_CONFIG)}
        self.assertIn("contract", jobs,
                      "prometheus.yml 缺少 contract 抓取任务（合同集成 Outbox 死信指标不可见）")
        contract_job = jobs["contract"]
        self.assertEqual(contract_job["metrics_path"], "/metrics",
                         "contract 抓取任务的 metrics_path 必须是 /metrics")
        self.assertEqual(contract_job["targets"], ["contract-api:8081"],
                         f"contract 抓取目标必须是内网 contract-api:8081：实际 {contract_job['targets']!r}")

        services = self.services(self.canonical, CANONICAL)
        for job_name, job in sorted(jobs.items()):
            for target in job["targets"]:
                host = target.rsplit(":", 1)[0]
                with self.subTest(job=job_name, target=target):
                    self.assertIn(
                        host, services,
                        f"prometheus.yml 的 {job_name} 抓取目标 {target} 不是 {CANONICAL} 声明的服务；"
                        "跨出 Compose 网络就无法用网络层保证 /metrics 仅集群内可见",
                    )

    def test_metrics_ports_are_never_published_and_never_proxied(self) -> None:
        """指标端口不得发布到宿主机；前端 nginx 不得出现 /metrics 路由。"""
        services = self.services(self.canonical, CANONICAL)
        for name in NO_PUBLISHED_PORTS:
            with self.subTest(service=name):
                self.assertIn(name, services, f"{CANONICAL} 缺少服务 {name}")
                ports = services[name].get("ports") or []
                self.assertEqual(
                    ports, [],
                    f"{CANONICAL} 的 {name} 不得发布宿主机端口（/metrics 仅限内网抓取）："
                    f"实际 {ports!r}",
                )
        for name, metrics_port in METRICS_PORTS.items():
            if name not in services:
                continue
            for entry in services[name].get("ports") or []:
                text = entry if isinstance(entry, str) else str(entry)
                container_port = text.rsplit(":", 1)[-1].split("/")[0]
                if container_port != str(metrics_port):
                    continue
                with self.subTest(service=name, port=metrics_port):
                    # 只允许 127.0.0.1 绑定或显式回环变量；0.0.0.0/公网网卡一律拒绝。
                    self.assertTrue(
                        text.startswith("127.0.0.1:")
                        or text.startswith("${KEYCLOAK_BIND_ADDRESS:-127.0.0.1}"),
                        f"{CANONICAL} 的 {name} 把指标端口 {metrics_port} 发布为 {text!r}；"
                        "只能绑定回环地址，供本机受控验收（见 ACCEPTANCE_CHECKLIST.md）",
                    )

        if not FRONTEND_NGINX.is_dir():
            return  # 平台仓库单仓 CI 没有 frontend 兄弟仓库，跳过公网路由检查。
        for config in sorted(FRONTEND_NGINX.glob("*.conf*")):
            text = config.read_text(encoding="utf-8")
            with self.subTest(config=config.name):
                self.assertNotRegex(
                    text, r"location[^\n]*metrics",
                    f"{config.name} 出现了 /metrics 公网路由；指标端点不得经过前端网关",
                )

    def test_authoritative_monitoring_subsystem_is_complete(self) -> None:
        """权威编排必须自带 monitoring 子系统，且规则文件真实存在。"""
        text = (PRODUCTION / CANONICAL).read_text(encoding="utf-8")
        self.assertIn("BEGIN SUBSYSTEM: monitoring", text,
                      f"{CANONICAL} 缺少 monitoring 子系统块")
        services = self.services(self.canonical, CANONICAL)
        for name in ("prometheus", "keycloak-backup-metrics"):
            self.assertIn(name, services, f"{CANONICAL} 缺少监控服务 {name}")
        for rule in ("keycloak-alerts.yml", "file-gateway-alerts.yml"):
            self.assertTrue(
                (PRODUCTION / "monitoring" / "prometheus" / rule).is_file(),
                f"prometheus.yml 引用的规则文件 monitoring/prometheus/{rule} 不存在",
            )
        self.assertIn("contract-api:8081", PROMETHEUS_CONFIG.read_text(encoding="utf-8"),
                      "prometheus.yml 未把 contract-api 纳入抓取目标")

    # ---------- 3b. 平台 / 结算新增观测面 ----------
    def test_prometheus_scrapes_platform_and_settlement_metrics(self) -> None:
        """平台与结算的 /metrics 必须被内网抓取，平台 job 按 RequireMetricsAccess 鉴权。"""
        document = load_yaml(PROMETHEUS_CONFIG)
        jobs = {str(job.get("job_name")): job for job in document.get("scrape_configs") or []}
        services = self.services(self.canonical, CANONICAL)
        for job_name, expectation in sorted(REQUIRED_SCRAPE_JOBS.items()):
            target = expectation["target"]
            with self.subTest(job=job_name):
                self.assertIn(
                    job_name,
                    jobs,
                    f"prometheus.yml 缺少 {job_name} 抓取任务："
                    "新增的 backchannel/集成 outbox 指标将永远不可见",
                )
                job = jobs[job_name]
                self.assertEqual(
                    job.get("metrics_path"), "/metrics",
                    f"{job_name} 抓取任务的 metrics_path 必须是 /metrics",
                )
                self.assertEqual(
                    scrape_targets(job), [target],
                    f"{job_name} 抓取目标必须是内网 {target}：实际 {scrape_targets(job)!r}",
                )
                self.assertIn(
                    target.rsplit(":", 1)[0], services,
                    f"{job_name} 的抓取目标 {target} 不是 {CANONICAL} 声明的服务",
                )
                authorization = job.get("authorization") or {}
                if expectation["authorized"]:
                    # RequireMetricsAccess：配置了 METRICS_ACCESS_TOKEN 就只接受 Bearer token，
                    # 跨容器抓取只能靠 credentials_file，不得内联任何凭据。
                    self.assertEqual(
                        authorization.get("type"), "Bearer",
                        f"{job_name} job 必须按 RequireMetricsAccess 使用 Bearer 鉴权："
                        f"实际 {authorization!r}",
                    )
                    self.assertEqual(
                        authorization.get("credentials_file"), METRICS_CREDENTIAL_FILE,
                        f"{job_name} job 的凭据必须来自只读挂载的 credentials_file，"
                        f"期望 {METRICS_CREDENTIAL_FILE}："
                        f"实际 {authorization.get('credentials_file')!r}",
                    )
                    self.assertNotIn(
                        "credentials", authorization,
                        "prometheus.yml 不得内联抓取凭据（credentials）：仓库里不允许出现真实"
                        " token，改用部署方提供的 credentials_file",
                    )
                    self.assertNotIn(
                        "bearer_token", job,
                        "prometheus.yml 不得内联 bearer_token：改用部署方提供的 credentials_file",
                    )
                else:
                    # Settlement/compat 端点与 /healthz、/readyz 同级注册，实现里没有应用层
                    # 鉴权；凭空加鉴权会让抓取静默 401，边界只能是 Compose 网络。
                    self.assertNotIn(
                        "authorization", job,
                        f"{job_name} 的 /metrics 在实现里没有鉴权"
                        "（Settlement/internal/httpapi/api.go 与 /healthz、/readyz 同级注册），"
                        "不要凭空加鉴权：边界是 Compose 网络",
                    )

    def test_metrics_credential_is_mounted_readonly_and_never_committed(self) -> None:
        """抓取令牌只能来自部署方提供的 runtime/ 文件，仓库内不得出现 token 或内联凭据。"""
        services = self.services(self.canonical, CANONICAL)
        volumes = [str(item) for item in services["prometheus"].get("volumes") or []]
        self.assertIn(
            METRICS_CREDENTIAL_VOLUME,
            volumes,
            f"{CANONICAL} 的 prometheus 必须把部署方令牌目录只读挂载为 "
            f"{METRICS_CREDENTIAL_VOLUME}；缺失时平台 job 无法鉴权（或静默 401）",
        )
        for entry in volumes:
            if not entry.endswith(":/etc/prometheus/secrets:ro"):
                continue
            source = entry.split(":", 1)[0]
            with self.subTest(volume=entry):
                self.assertTrue(
                    source.startswith("./runtime/"),
                    f"令牌目录宿主来源 {source!r} 必须在 runtime/ 下："
                    "它被 .gitignore 忽略，且不随离线资产包发布；放进 monitoring/ 会被"
                    "install-assets.sh 覆盖或在资产包里泄漏凭据",
                )
        gitignore = (PRODUCTION / ".gitignore").read_text(encoding="utf-8")
        self.assertRegex(
            gitignore, r"(?m)^runtime/$",
            ".gitignore 必须忽略 runtime/（抓取令牌文件所在目录不得进版本库）",
        )

        leaked = sorted(
            path.name for path in PROMETHEUS_DIR.rglob("*")
            if path.is_file() and "token" in path.name.lower()
        )
        self.assertEqual(
            leaked, [],
            f"monitoring/prometheus 下不得提交令牌文件：发现 {leaked}；"
            "prometheus.yml 只允许引用 credentials_file 路径",
        )
        text = PROMETHEUS_CONFIG.read_text(encoding="utf-8")
        self.assertNotRegex(
            text, r"(?m)^\s+credentials:\s*\S+",
            "prometheus.yml 出现内联 credentials 字面量：仓库里不得出现真实 token",
        )
        self.assertNotRegex(
            text, r"\b[0-9a-fA-F]{32,}\b",
            "prometheus.yml 出现疑似真实令牌的十六进制字面量：改用 credentials_file",
        )

    def test_alert_rule_files_are_referenced_and_mounted_readonly(self) -> None:
        """新增告警规则文件必须存在、被 rule_files 引用、并被只读挂载进 prometheus。"""
        document = load_yaml(PROMETHEUS_CONFIG)
        rule_files = [str(item) for item in document.get("rule_files") or []]
        self.assertTrue(rule_files, "prometheus.yml 的 rule_files 不得为空")
        services = self.services(self.canonical, CANONICAL)
        volumes = [str(item) for item in services["prometheus"].get("volumes") or []]
        for rule_file in NEW_RULE_FILES:
            container_path = "/etc/prometheus/" + rule_file
            with self.subTest(rule=rule_file):
                self.assertTrue(
                    (PROMETHEUS_DIR / rule_file).is_file(),
                    f"缺少告警规则文件 monitoring/prometheus/{rule_file}",
                )
                self.assertIn(
                    container_path, rule_files,
                    f"prometheus.yml 的 rule_files 必须引用 {container_path}："
                    "否则规则文件存在也不会被加载",
                )
                self.assertIn(
                    f"./monitoring/prometheus/{rule_file}:{container_path}:ro",
                    volumes,
                    f"{CANONICAL} 的 prometheus 必须只读挂载 {rule_file} 到 {container_path}",
                )
        # rule_files 里的每一项都必须真被挂载，否则 prometheus 启动时加载失败。
        for container_path in rule_files:
            with self.subTest(rule_file=container_path):
                self.assertTrue(
                    container_path.startswith("/etc/prometheus/"),
                    f"rule_files 条目 {container_path!r} 必须位于 /etc/prometheus/ 下",
                )
                mounted = [v for v in volumes if v.endswith(":" + container_path + ":ro")]
                self.assertTrue(
                    mounted,
                    f"rule_files 引用的 {container_path} 没有对应的只读挂载："
                    "容器内找不到文件时 prometheus 直接启动失败",
                )

    def test_alert_rules_cover_the_new_observability_signals(self) -> None:
        """新增告警必须覆盖 backchannel 缺口、Settlement/合同 outbox 与抓取失败。"""
        rules: dict[str, dict] = {}
        for rule_file in NEW_RULE_FILES:
            for rule in load_alert_rules(rule_file):
                name = str(rule.get("alert") or "")
                self.assertTrue(name, f"{rule_file} 存在没有 alert 名称的规则")
                self.assertNotIn(name, rules, f"告警 {name} 在多个规则文件里重复定义")
                rules[name] = rule

        def matching(substring: str) -> list[dict]:
            return [rule for rule in rules.values() if substring in str(rule.get("expr") or "")]

        expectations = {
            # backchannel：未登记 RP 15 分钟即需要登记修复。
            "platform_oauth_backchannel_unregistered_rp > 0": ("15m", "warning"),
            # NO_ENDPOINT 积压 30 分钟为 warning（新缺口），6 小时升级 critical（缺口未清零）。
            "platform_oauth_backchannel_logout_no_endpoint > 0": ("30m", "warning"),
            # 采集失败 / 指标样本缺失。
            'up{job="platform-api"} == 0': ("5m", "critical"),
            'up{job="settlement-api"} == 0': ("5m", "critical"),
            'up{job="contract"} == 0': ("5m", "critical"),
            # Settlement 按 destination 的积压阈值。
            'settlement_outbox_pending{destination="PLATFORM_AUDIT"} > 200': ("30m", "warning"),
            'settlement_outbox_pending{destination="PLATFORM_NOTIFICATION"} > 500': ("30m", "warning"),
            'settlement_outbox_pending{destination="TAX_INVOICE_COMMAND"} > 200': ("30m", "warning"),
            # 未配置目的地积压（XAPI-10 核心缺口）。
            "settlement_outbox_unconfigured_pending > 0": ("15m", "warning"),
            # 可指标化的 readyz 降级：审计必开但凭据缺失 → /readyz 503。
            "settlement_integration_audit_required == 1": ("5m", "critical"),
            # 合同 integration outbox 死信。
            "contract_integration_outbox_dead_total > 0": ("10m", "warning"),
            "contract_integration_outbox_dead_total > 100": ("1h", "critical"),
        }
        for expr_fragment, (expected_for, expected_severity) in sorted(expectations.items()):
            with self.subTest(expr=expr_fragment):
                candidates = matching(expr_fragment)
                self.assertTrue(
                    candidates,
                    f"新增告警规则里没有任何表达式包含 {expr_fragment!r}",
                )
                self.assertTrue(
                    any(
                        str(rule.get("for")) == expected_for
                        and str((rule.get("labels") or {}).get("severity")) == expected_severity
                        for rule in candidates
                    ),
                    f"{expr_fragment!r} 需要 for={expected_for} / severity={expected_severity}："
                    f"实际 {[(rule.get('for'), (rule.get('labels') or {}).get('severity')) for rule in candidates]!r}",
                )

        for name, rule in sorted(rules.items()):
            annotations = rule.get("annotations") or {}
            with self.subTest(alert=name):
                self.assertTrue(
                    str(annotations.get("summary") or "").strip(),
                    f"{name} 缺少 annotations.summary",
                )
                runbook = str(annotations.get("runbook") or "")
                self.assertRegex(
                    runbook,
                    r"^(platform/deploy/production/README\.md|platform/docs/)\S*",
                    f"{name} 的 runbook 必须指向 platform/deploy/production/README.md "
                    f"或既有运维文档：实际 {runbook!r}",
                )
                self.assertIn(
                    str((rule.get("labels") or {}).get("severity")),
                    {"warning", "critical", "info"},
                    f"{name} 的 severity 必须是 warning/critical/info",
                )

    def test_alert_expressions_only_reference_implemented_metrics(self) -> None:
        """告警表达式里的指标名必须来自代码实际输出，禁止臆造。

        白名单见 KNOWN_METRICS（逐条标注 file:line 来源）。历史规则文件
        （keycloak/file-gateway）不在本用例范围：它们的指标来自其它服务的实现。
        """
        unknown: list[str] = []
        for rule_file in NEW_RULE_FILES:
            for rule in load_alert_rules(rule_file):
                expr = str(rule.get("expr") or "")
                for token in sorted(metric_tokens(expr)):
                    if token not in KNOWN_METRICS:
                        unknown.append(f"{rule.get('alert')}: {token}")
        self.assertEqual(
            unknown, [],
            "新增告警表达式里出现未在代码中实现的指标名（先实现指标或补白名单并注明来源）："
            f"{unknown}",
        )

    def test_legacy_observability_overlay_matches_authoritative_monitoring(self) -> None:
        """历史覆盖文件的 prometheus/node-exporter 关键键必须与权威编排一致。"""
        canonical_services = self.services(self.canonical, CANONICAL)
        overlay_services = self.services(self.overlay, OVERLAY)
        for name in ("prometheus", "keycloak-backup-metrics"):
            with self.subTest(service=name):
                self.assertIn(name, overlay_services, f"{OVERLAY} 缺少服务 {name}")
                authoritative = canonical_services[name]
                copied = overlay_services[name]
                self.assertEqual(copied.get("image"), authoritative.get("image"),
                                 f"{OVERLAY} 的 {name} 镜像与 {CANONICAL} 不一致")
                self.assertEqual(copied.get("networks"), authoritative.get("networks"),
                                 f"{OVERLAY} 的 {name} 网络与 {CANONICAL} 不一致")
                self.assertEqual(
                    sorted(str(item) for item in copied.get("volumes") or []),
                    sorted(str(item) for item in authoritative.get("volumes") or []),
                    f"{OVERLAY} 的 {name} 挂载与 {CANONICAL} 不一致"
                    "（配置挂载不同会导致抓取规则失效）",
                )
        overlay_volumes = self.overlay.get("volumes") or {}
        canonical_volumes = self.canonical.get("volumes") or {}
        for name in sorted(overlay_volumes):
            with self.subTest(volume=name):
                self.assertIn(name, canonical_volumes,
                              f"{OVERLAY} 声明了 {CANONICAL} 没有的卷 {name}")

    # ---------- 4. alert-worker 接线 ----------
    def test_alert_worker_switch_is_wired_and_defaults_off(self) -> None:
        """alert-worker 必须拿到租户来源说明与默认关闭的收敛开关，两份编排一致。"""
        for filename, document in ((CANONICAL, self.canonical), (REFERENCE, self.reference)):
            with self.subTest(filename=filename):
                services = self.services(document, filename)
                self.assertIn(ALERT_WORKER, services, f"{filename} 缺少 {ALERT_WORKER}")
                worker = services[ALERT_WORKER]
                env = environment_of(worker)
                self.assertEqual(
                    env.get(ALERT_SWITCH),
                    ALERT_SWITCH_DEFAULT,
                    f"{filename} 的 {ALERT_WORKER} 必须把 {ALERT_SWITCH} 固定为默认 false："
                    f"实际 {env.get(ALERT_SWITCH)!r}",
                )
                self.assertNotIn(
                    "OIDC_TENANT_ID", env,
                    f"{filename} 的 {ALERT_WORKER} 不得在 Compose 层给 OIDC_TENANT_ID 设值："
                    "那会覆盖 runtime/data-analysis.env，把占位租户带进告警写路径",
                )
                self.assertIn(
                    "${DATA_ANALYSIS_RUNTIME_ENV_FILE:-runtime/data-analysis.env}",
                    env_file_paths(worker),
                    f"{filename} 的 {ALERT_WORKER} 必须从 data-analysis runtime 文件读取 OIDC 租户",
                )

    def test_alert_worker_switch_is_declared_in_production_runtime_template(self) -> None:
        """runtime 模板与 .env.example 要能追溯默认值，避免按模板重建后开关消失。"""
        template = DATA_ANALYSIS_TEMPLATE.read_text(encoding="utf-8")
        self.assertRegex(template, r"(?m)^OIDC_TENANT_ID=\S+",
                         f"{DATA_ANALYSIS_TEMPLATE.name} 缺少 OIDC_TENANT_ID 声明")
        self.assertRegex(template, r"(?m)^ALERT_AUTO_RESOLVE=false$",
                         f"{DATA_ANALYSIS_TEMPLATE.name} 必须显式声明 ALERT_AUTO_RESOLVE=false")
        root_env = ROOT_ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertRegex(root_env, r"(?m)^ALERT_AUTO_RESOLVE=false$",
                         f".env.example 必须显式声明 ALERT_AUTO_RESOLVE=false（Compose 插值来源）")

    @unittest.skipUnless(DATA_ANALYSIS_COMPOSE.is_file(), "缺少 data_analysis 兄弟仓库")
    def test_data_analysis_compose_wires_alert_worker_env(self) -> None:
        """数析仓库的本地编排也必须给 alert-worker 租户与开关，否则本机栈起不来。"""
        document = load_compose_path(DATA_ANALYSIS_COMPOSE)
        services = document.get("services") or {}
        self.assertIn("alert-worker", services, "data_analysis/compose.yaml 缺少 alert-worker")
        env = environment_of(services["alert-worker"])
        self.assertEqual(
            env.get("OIDC_TENANT_ID"), "${OIDC_TENANT_ID:?required}",
            "data_analysis/compose.yaml 的 alert-worker 必须要求 OIDC_TENANT_ID 必填"
            "（与 aggregation-worker 一致，缺失时 fail-closed）",
        )
        self.assertEqual(
            env.get(ALERT_SWITCH), ALERT_SWITCH_DEFAULT,
            f"data_analysis/compose.yaml 的 alert-worker 必须声明 "
            f"{ALERT_SWITCH}: {ALERT_SWITCH_DEFAULT}",
        )


def load_compose_path(path: Path) -> dict:
    """解析任意路径的 Compose 文件（跨仓库检查用），失败即中文断言错误。"""
    result = subprocess.run(
        ["ruby", "-e", RUBY_LOADER, str(path)], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        raise AssertionError(
            f"无法解析 {path}：ruby -ryaml 返回 {result.returncode}，stderr={result.stderr.strip()!r}"
        )
    return json.loads(result.stdout)


if __name__ == "__main__":
    unittest.main()
