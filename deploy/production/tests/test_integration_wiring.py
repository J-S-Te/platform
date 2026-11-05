#!/usr/bin/env python3
"""静态校验生产部署的跨系统接线：合同侧机器入站键、合同→结算开关、数析只读 DSN。

只用标准库解析 platform/deploy/production 下的模板、清单和 Compose 文件，
不读取 runtime/.env、不启动容器、不连接数据库。断言信息只输出键名，
不输出任何键值或凭据内容。

覆盖发现：BIZ-XEVT2-01（商机签单入站）、BIZ-XWIRE-01（合同摘要）、
BIZ-XWIRE-03（合同→结算开关与模板矛盾）、BIZ-XWIRE-05（数析只读 DSN 注入）、
BIZ-XWIRE-10a（清单 client_id 字面量必须能被规范命名规则推导）、
BIZ-XAPI-13（数析报表状态端点的期望 OAuth client 配置键）、
BIZ-XEVT2-05c（合同财务变更开关在模板/清单两端声明且默认关闭）。

模板漂移门禁：清单每个 runtime.files[] 条目的 values/bindings 键都必须写进它
自己的模板（customer_portal 的 portal.env / portal-report-worker.env /
customer.env 逐条目配对），并按模板=清单校验文件网关与负责人目录开关，
避免“按模板重建”比生产清单弱一档。

client_id 命名门禁（BIZ-XWIRE-10a）：平台受控接入按
`applicationCode-environment-suffix` 生成 OAuth Client，清单里出现的每个
`*_CLIENT_ID` 字面量都必须能被这套规则推导出来（或落进显式 allowlist），
否则接入后运行时拿不到同名 Client，只会在很晚的联调阶段以 401 暴露。
"""
from __future__ import annotations

import re
import unittest
from pathlib import Path


PRODUCTION = Path(__file__).resolve().parents[1]
TEMPLATE = PRODUCTION / "subsystem-templates" / "contract.env.example"
MANIFEST = PRODUCTION / "subsystems.d" / "contract_management-prod.yaml"
COMPOSE = PRODUCTION / "docker-compose.yml"
ROOT_ENV_EXAMPLE = PRODUCTION / ".env.example"
CRM_MANIFEST = PRODUCTION / "subsystems.d" / "customer_and_opportunity-prod.yaml"
CUSTOMER_TEMPLATE = PRODUCTION / "subsystem-templates" / "customer.env.example"
PROJECT_TEMPLATE = PRODUCTION / "subsystem-templates" / "project.env.example"
PROJECT_MANIFEST = PRODUCTION / "subsystems.d" / "project_management-prod.yaml"
SETTLEMENT_TEMPLATE = PRODUCTION / "subsystem-templates" / "settlement.env.example"
SETTLEMENT_MANIFEST = PRODUCTION / "subsystems.d" / "settlement-prod.yaml"
PORTAL_MANIFEST = PRODUCTION / "subsystems.d" / "customer_portal-prod.yaml"
DATA_ANALYSIS_MANIFEST = PRODUCTION / "subsystems.d" / "data-analysis-prod.yaml"
DATA_ANALYSIS_TEMPLATE = PRODUCTION / "subsystem-templates" / "data-analysis.env.example"
SUBSYSTEMS_DIR = PRODUCTION / "subsystems.d"

# 受控接入的用途后缀表（只读镜像，不重复实现 Go 逻辑）。
# 与 platform/internal/platform/applicationregistry/application/
# subsystem_update_credential_requirements.go 的 SubsystemUpdateServiceCredentialRequirements
# 一致；命名函数见同目录 subsystem_update_credentials.go 的
# SubsystemUpdateServiceClientName：applicationCode + "-" + environment + "-" + suffix，
# 不做大小写归一化。settlement_and_invoicing 是 settlement 的历史别名，Go 侧两个编码
# 返回同一组需求，因此这里也保留。
FILE_GATEWAY_SUFFIX = "file-gateway-writer"
UPDATE_CREDENTIAL_SUFFIXES = {
    "contract_management": (
        "analysis-import-report-status",
        "audit-publisher",
        "notification-publisher",
        "owner-directory",
        "crm-contract-reference",
        "crm-contract-link",
        "crm-customer-import-provision",
        "crm-opportunity-import-provision",
        "project-integration",
        "settlement-integration",
        FILE_GATEWAY_SUFFIX,
    ),
    "customer_and_opportunity": (
        "audit-publisher",
        "notification-publisher",
        "contract-summary",
        "opportunity-intake",
        "owner-directory",
        "contract-signed-count",
        FILE_GATEWAY_SUFFIX,
    ),
    "customer_portal": (
        "audit-publisher",
        "external-user-provision",
        "role-assign",
        "role-revoke",
        "portal-mapping-provision",
        "portal-mapping-disable",
        "portal-invite-verify",
        "portal-report-source",
        "portal-report-identity",
        FILE_GATEWAY_SUFFIX,
    ),
    "data_analysis": (
        "contract-import-report-read",
        "audit-publisher",
        "contract-dashboard",
        "project-dashboard",
        FILE_GATEWAY_SUFFIX,
    ),
    "project_management": (
        "audit-publisher",
        "owner-directory",
        "notification-ingest",
        "contract-approved-reader",
        FILE_GATEWAY_SUFFIX,
    ),
    "settlement": (
        "audit-publisher",
        "notification-publisher",
        "owner-directory",
        "project-reference",
        "crm-credit-ingest",
        FILE_GATEWAY_SUFFIX,
    ),
}
UPDATE_CREDENTIAL_SUFFIXES["settlement_and_invoicing"] = UPDATE_CREDENTIAL_SUFFIXES["settlement"]

# client_id 字面量的唯一显式例外。每条都必须写明理由，不允许“先加进来再说”：
# 例外会把命名规则从“可静态推导”降级为“人工记忆”，越少越好。
CLIENT_ID_ALLOWLIST = {
    "platform-handover": (
        "平台人员交接（handover）机器身份不是受控接入按 applicationCode-environment-suffix "
        "生成的子系统 Client，而是平台内置人员交接的固定身份，多个子系统清单复用同名客户端"
        "（contract/project/settlement 的 HANDOVER_*_CLIENT_ID）。改名会同时打断三份清单与"
        "平台内置交接客户端，因此只在此处登记，不参与后缀推导。"
    ),
}

# 占位绑定：值由受控接入/渲染写入，不是需要校验的字面量。
# 与任务口径一致排除 service.*.client_id、client_id、catalog_publisher_client_id，
# 同一类引用路径（related_service.*.client_id、*.oauth_client_id）按同一规则处理。
PLACEHOLDER_CLIENT_ID_VALUES = frozenset(
    {"client_id", "catalog_publisher_client_id", "oauth_client_id", ""}
)
PLACEHOLDER_CLIENT_ID_PREFIXES = ("service.", "related_service.")

# Go 侧后缀表的只读来源：仅用于“内嵌表是否过期”的自检，不修改 Go 代码。
UPDATE_CREDENTIAL_REQUIREMENTS_GO = (
    PRODUCTION.parents[1]
    / "internal"
    / "platform"
    / "applicationregistry"
    / "application"
    / "subsystem_update_credential_requirements.go"
)

_GO_CASE = re.compile(r"^\s*case\s+(.+):\s*$")
_GO_SUFFIX = re.compile(r'Suffix:\s*"([a-z0-9-]+)"')
_GO_DEFAULT = re.compile(r"^\s*default:\s*$")
_GO_SHARED_FILE_GATEWAY = re.compile(r"^\s*fileGateway,\s*$")
_CLIENT_ID_LITERAL = re.compile(r"^[ ]*([A-Z][A-Z0-9_]*_CLIENT_ID):[ \t]*(.*)$")

# 清单 → 模板的对应关系。清单里一个 runtime.files[] 条目就是一个运行配置文件，
# 各自声明 template_path/values/bindings；按模板重建或灾后恢复时，该条目声明的
# 每个 values/bindings 键都必须能在它自己的模板找到，否则重建会静默漏键。
# 必须逐条目配对：customer_portal 有 portal.env / portal-report-worker.env /
# customer.env 三个文件，把整个清单摊平后与单一模板比较会同时产生假缺失和假重复。
SUBSYSTEM_TEMPLATE_PAIRS = (
    ("contract_management", MANIFEST),
    ("customer_and_opportunity", CRM_MANIFEST),
    ("project_management", PROJECT_MANIFEST),
    ("settlement", SETTLEMENT_MANIFEST),
    ("customer_portal", PORTAL_MANIFEST),
    ("data_analysis", DATA_ANALYSIS_MANIFEST),
)

# 模板=清单且必须取预期值的键（参数化对账）。这里同时覆盖两类：
# 1. 开关类键：模板关闭而清单开启时，按模板重建会静默停用已交付的接线（文件网关、
#    负责人目录），因此把它们与清单绑成同一门禁；
# 2. 受控接入占位键：模板与清单都只写空占位，真实值由受控接入写入（XAPI-13 的
#    报表状态端点期望 OAuth client）。这时“预期值”就是空串——断言的是两处都必须
#    声明该键且都保持空占位，避免按模板重建或改清单时整键消失后端点静默不挂载。
TEMPLATE_MANIFEST_EXPECTATIONS = (
    ("SETTLEMENT_FILE_GATEWAY_MODE", "required", SETTLEMENT_TEMPLATE, SETTLEMENT_MANIFEST),
    ("OWNER_DIRECTORY_ENABLED", "true", CUSTOMER_TEMPLATE, CRM_MANIFEST),
    ("OWNER_DIRECTORY_ENABLED", "true", PROJECT_TEMPLATE, PROJECT_MANIFEST),
    (
        "REPORT_STATUS_MACHINE_OAUTH_CLIENT_ID",
        "",
        DATA_ANALYSIS_TEMPLATE,
        DATA_ANALYSIS_MANIFEST,
    ),
)

MACHINE_KEY_SUFFIXES = (
    "ENABLED",
    "REQUIRE_BEARER",
    "ISSUER",
    "AUDIENCE",
    "PUBLIC_KEY_PATH",
    "CLIENT_ID",
    "CALLER_APPLICATION_CODE",
    "CALLER_ENVIRONMENT_CODE",
    "REQUIRED_SCOPE",
)

PUBLIC_KEY_PATH = "/app/data/keys/jwt-ed25519-public.pem"

CRM_OPPORTUNITY_INTAKE_EXPECTED = {
    "ENABLED": "true",
    "REQUIRE_BEARER": "true",
    "ISSUER": "basic-platform",
    "AUDIENCE": "basic-platform-application",
    "PUBLIC_KEY_PATH": PUBLIC_KEY_PATH,
    "CLIENT_ID": "customer_and_opportunity-prod-opportunity-intake",
    "CALLER_APPLICATION_CODE": "customer_and_opportunity",
    "CALLER_ENVIRONMENT_CODE": "prod",
    "REQUIRED_SCOPE": "opportunity.signed.write",
}

CONTRACT_SUMMARY_EXPECTED = {
    "ENABLED": "true",
    "REQUIRE_BEARER": "true",
    "ISSUER": "basic-platform",
    "AUDIENCE": "basic-platform-application",
    "PUBLIC_KEY_PATH": PUBLIC_KEY_PATH,
    "CLIENT_ID": "customer_and_opportunity-prod-contract-summary",
    "CALLER_APPLICATION_CODE": "customer_and_opportunity",
    "CALLER_ENVIRONMENT_CODE": "prod",
    "REQUIRED_SCOPE": "contract.summary.read",
}

AGGREGATION_READ_DSNS = (
    "CONTRACT_READ_DSN",
    "CRM_READ_DSN",
    "PROJECT_READ_DSN",
    "SETTLEMENT_READ_DSN",
)

_ASSIGNMENT = re.compile(r"^(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)$")


def _unquote(raw: str) -> str:
    value = raw.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        return value[1:-1]
    return value


def parse_env_template(path: Path) -> dict[str, str]:
    """解析 KEY=VALUE 模板，跳过注释与空行。只返回键值映射，不写入日志。"""
    values: dict[str, str] = {}
    text = path.read_text(encoding="utf-8")
    for number, line in enumerate(text.splitlines(), 1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = _ASSIGNMENT.match(stripped)
        if not match:
            continue
        key = match.group(1)
        if key in values:
            raise AssertionError(f"{path.name}:{number} 模板出现重复键 {key}")
        values[key] = _unquote(match.group(2))
    return values


def parse_manifest_block(path: Path, block: str) -> dict[str, str]:
    """解析清单中一个扁平 YAML 映射块（values / bindings），不依赖 PyYAML。"""
    header = re.compile(r"^([ ]*)%s:[ \t]*$" % re.escape(block))
    lines = path.read_text(encoding="utf-8").splitlines()
    values: dict[str, str] = {}
    block_indent: int | None = None
    for number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        match = header.match(line)
        if match:
            block_indent = len(match.group(1))
            continue
        if block_indent is None:
            continue
        indent = len(line) - len(line.lstrip(" "))
        if indent <= block_indent:
            block_indent = None
            continue
        if indent != block_indent + 2:
            continue
        entry = re.match(r"^[ ]*([A-Za-z_][A-Za-z0-9_]*):[ \t]*(.*)$", line)
        if not entry:
            continue
        key = entry.group(1)
        if key in values:
            raise AssertionError(f"{path.name}:{number} {block} 出现重复键 {key}")
        values[key] = _unquote(entry.group(2))
    if not values:
        raise AssertionError(f"{path.name} 缺少 {block} 块或块为空")
    return values


def parse_manifest_runtime_files(path: Path) -> list[dict[str, object]]:
    """把清单 runtime.files[] 拆成逐条目的 {path, template_path, values, bindings}。

    每个条目是一个独立运行配置文件，因此重复键只在同一 (文件, values|bindings)
    块内违规；跨条目的同名键（例如 portal.env 与 portal-report-worker.env 都声明
    PLATFORM_APPLICATION_CODE）是两个不同文件的正常配置，不是 YAML 重复键。
    """
    lines = path.read_text(encoding="utf-8").splitlines()
    entries: list[dict[str, object]] = []
    current: dict[str, object] | None = None
    block: str | None = None
    for number, line in enumerate(lines, 1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        indent = len(line) - len(line.lstrip(" "))
        start = re.match(r"^ {4}- [A-Za-z_]+:[ \t]*(.*)$", line)
        if start:
            current = {"path": "", "template_path": "", "values": {}, "bindings": {}}
            entries.append(current)
            block = None
            field = re.match(r"^ {4}- ([A-Za-z_]+):[ \t]*(.*)$", line)
            if field and field.group(1) == "path":
                current["path"] = _unquote(field.group(2))
            continue
        if current is None:
            continue
        if indent < 4:
            current = None
            block = None
            continue
        if indent == 6:
            field = re.match(r"^ {6}([A-Za-z_]+):[ \t]*(.*)$", line)
            if not field:
                continue
            name, raw = field.group(1), field.group(2)
            if not raw.strip() and name in ("values", "bindings"):
                block = name
                continue
            block = None
            if name == "template_path":
                current["template_path"] = _unquote(raw)
            continue
        if indent == 8 and block:
            item = re.match(r"^ {8}([A-Za-z_][A-Za-z0-9_]*):[ \t]*(.*)$", line)
            if not item:
                continue
            key = item.group(1)
            values = current[block]
            assert isinstance(values, dict)
            if key in values:
                raise AssertionError(f"{path.name}:{number} {block} 出现重复键 {key}")
            values[key] = _unquote(item.group(2))
    if not entries:
        raise AssertionError(f"{path.name} 缺少 runtime.files[] 条目")
    for entry in entries:
        if not entry["template_path"]:
            raise AssertionError(f"{path.name} 的 {entry['path']} 缺少 template_path")
        for name in ("values", "bindings"):
            if not entry[name]:
                raise AssertionError(f"{path.name} 的 {entry['path']} 缺少 {name} 块或块为空")
    return entries


def manifest_values_for(manifest: Path, template: Path) -> dict[str, str]:
    """返回清单中指向该模板的条目所声明的 values+bindings（同名键 values 优先）。"""
    merged: dict[str, str] = {}
    matched = False
    for entry in parse_manifest_runtime_files(manifest):
        declared = (PRODUCTION / str(entry["template_path"])).resolve()
        if declared != template.resolve():
            continue
        matched = True
        for block in ("values", "bindings"):
            values = entry[block]
            assert isinstance(values, dict)
            for key, value in values.items():
                merged.setdefault(key, value)
    if not matched:
        raise AssertionError(f"{manifest.name} 没有任何 runtime.files[] 条目指向 {template.name}")
    return merged


def manifest_application_identity(path: Path) -> tuple[str, str]:
    """从清单 application 块读取 (应用编码, 环境)，用于生成规范 client_id 期望名。"""
    code: str | None = None
    environment: str | None = None
    for line in path.read_text(encoding="utf-8").splitlines():
        field = re.match(r"^  (code|environment):[ \t]*(\S+)[ \t]*$", line)
        if not field:
            continue
        name, raw = field.group(1), _unquote(field.group(2))
        if name == "code" and code is None:
            code = raw
        elif name == "environment" and environment is None:
            environment = raw
    if not code or not environment:
        raise AssertionError(f"{path.name} 的 application 块缺少 code 或 environment")
    return code, environment


def expected_client_ids(environment: str) -> set[str]:
    """返回该环境下按 `applicationCode-environment-suffix` 可推导的 client_id 集合。"""
    return {
        f"{code}-{environment}-{suffix}"
        for code, suffixes in UPDATE_CREDENTIAL_SUFFIXES.items()
        for suffix in suffixes
    }


def is_placeholder_client_id(value: str) -> bool:
    """判断取值是不是由受控接入写入的占位/引用路径（不参与命名校验）。"""
    text = value.strip()
    if text in PLACEHOLDER_CLIENT_ID_VALUES:
        return True
    return text.startswith(PLACEHOLDER_CLIENT_ID_PREFIXES)


def collect_client_id_literals(path: Path) -> list[tuple[int, str, str]]:
    """收集清单里 *_CLIENT_ID 的非占位字面量，返回 (行号, 键, 值)。"""
    literals: list[tuple[int, str, str]] = []
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        entry = _CLIENT_ID_LITERAL.match(line)
        if not entry:
            continue
        value = _unquote(entry.group(2))
        if is_placeholder_client_id(value):
            continue
        literals.append((number, entry.group(1), value))
    return literals


def parse_go_credential_suffixes(path: Path) -> dict[str, set[str]]:
    """从 Go 需求表源码提取每个应用编码的用途后缀（只读，用于内嵌表自检）。"""
    suffixes: dict[str, set[str]] = {}
    current: list[str] | None = None
    for line in path.read_text(encoding="utf-8").splitlines():
        case = _GO_CASE.match(line)
        if case:
            current = [item.strip().strip('"') for item in case.group(1).split(",")]
            for code in current:
                suffixes.setdefault(code, set())
            continue
        if _GO_DEFAULT.match(line):
            current = None
            continue
        if current is None:
            continue
        suffix = _GO_SUFFIX.search(line)
        if suffix:
            for code in current:
                suffixes[code].add(suffix.group(1))
            continue
        if _GO_SHARED_FILE_GATEWAY.match(line):
            for code in current:
                suffixes[code].add(FILE_GATEWAY_SUFFIX)
    return suffixes


def parse_compose_service_environment(path: Path, service: str) -> dict[str, str]:
    """解析 docker-compose.yml 中某个服务的 environment 映射键。"""
    lines = path.read_text(encoding="utf-8").splitlines()
    service_header = re.compile(r"^  %s:[ \t]*$" % re.escape(service))
    start = None
    for index, line in enumerate(lines):
        if service_header.match(line):
            start = index
            break
    if start is None:
        raise AssertionError(f"{path.name} 缺少服务 {service}")

    environment: dict[str, str] = {}
    collecting = False
    for line in lines[start + 1:]:
        if not line.strip():
            continue
        if re.match(r"^  [A-Za-z0-9_.-]+:[ \t]*$", line) and not line.startswith("    "):
            break  # 下一个顶层服务
        if re.match(r"^    environment:[ \t]*$", line):
            collecting = True
            continue
        if not collecting:
            continue
        if not line.startswith("      "):
            collecting = False
            continue
        entry = re.match(r"^      ([A-Za-z_][A-Za-z0-9_]*):[ \t]*(.*)$", line)
        if entry:
            environment[entry.group(1)] = _unquote(entry.group(2))
    return environment


class IntegrationWiringTest(unittest.TestCase):
    def assert_machine_group(self, values: dict[str, str], prefix: str, expected: dict[str, str]) -> None:
        for suffix, wanted in expected.items():
            key = f"{prefix}{suffix}"
            self.assertIn(key, values, f"缺少接线键 {key}")
            self.assertEqual(values[key], wanted, f"接线键 {key} 取值与约定不一致")

    def test_manifest_keys_are_all_declared_in_subsystem_templates(self) -> None:
        """通用不变量：清单每个 runtime.files[] 条目的键都必须在它自己的模板存在。

        清单是生产权威取值来源，模板是重建/灾后恢复的唯一入口；模板缺键时
        按模板重建会静默丢掉清单已启用的接线（机器入站、机器凭据、开关等）。
        模板允许有清单未用到的多余键，这里只禁止单向缺失。
        """
        for label, manifest in SUBSYSTEM_TEMPLATE_PAIRS:
            with self.subTest(subsystem=label):
                for entry in parse_manifest_runtime_files(manifest):
                    runtime_path = str(entry["path"])
                    template = (PRODUCTION / str(entry["template_path"])).resolve()
                    with self.subTest(subsystem=label, runtime_file=runtime_path):
                        self.assertTrue(
                            template.is_file(),
                            f"{manifest.name} 的 {runtime_path} 指向不存在的模板 {template.name}",
                        )
                        template_keys = parse_env_template(template)
                        for block in ("values", "bindings"):
                            declared = entry[block]
                            assert isinstance(declared, dict)
                            missing = sorted(set(declared) - set(template_keys))
                            self.assertEqual(
                                missing,
                                [],
                                f"{manifest.name} 的 {runtime_path} {block} 键未写入 {template.name}"
                                "（按模板重建会漏键）：" + ", ".join(missing),
                            )

    def test_declared_values_match_manifests(self) -> None:
        """参数化对账：模板=清单，且取清单已交付时的预期值。

        覆盖两类键（见 TEMPLATE_MANIFEST_EXPECTATIONS）：
        - 开关类键：模板写 false/disabled、清单写 true/required 时，按模板重建会静默把
          已交付的文件网关或负责人目录接线降级；
        - 受控接入占位键：预期值是空占位，断言的是模板与清单都声明了该键且都保持空占位，
          避免重建/改清单时整键消失（XAPI-13 报表状态端点的期望 OAuth client）。
        """
        for key, expected, template_path, manifest in TEMPLATE_MANIFEST_EXPECTATIONS:
            with self.subTest(key=key, template=template_path.name, manifest=manifest.name):
                template = parse_env_template(template_path)
                declared = manifest_values_for(manifest, template_path)
                self.assertIn(key, template, f"{template_path.name} 缺少 {key}")
                self.assertIn(key, declared, f"{manifest.name} 缺少 {key}")
                self.assertEqual(
                    template[key].strip().lower(),
                    expected,
                    f"{template_path.name} 的 {key} 必须与清单一致为 {expected!r}",
                )
                self.assertEqual(
                    template[key].strip().lower(),
                    declared[key].strip().lower(),
                    f"{template_path.name} 与 {manifest.name} 的 {key} 不一致",
                )

    def test_data_analysis_oidc_values_match_manifest(self) -> None:
        """数析运行配置的 OIDC 组必须与清单同值。

        模板缺这组键时按模板重建会让看板回退到无 IDP 提示/无 scope 的默认登录，
        会话 Cookie 名也会与清单不一致，导致既有会话全部失效。
        """
        keys = (
            "OIDC_AUTHORIZATION_REFRESH_INTERVAL",
            "OIDC_IDP_HINT",
            "OIDC_SCOPES",
            "OIDC_SESSION_COOKIE_NAME",
        )
        template = parse_env_template(DATA_ANALYSIS_TEMPLATE)
        declared = manifest_values_for(DATA_ANALYSIS_MANIFEST, DATA_ANALYSIS_TEMPLATE)
        for key in keys:
            with self.subTest(key=key):
                self.assertIn(key, template, f"{DATA_ANALYSIS_TEMPLATE.name} 缺少 {key}")
                self.assertIn(key, declared, f"{DATA_ANALYSIS_MANIFEST.name} 缺少 {key}")
                self.assertEqual(
                    template[key],
                    declared[key],
                    f"{DATA_ANALYSIS_TEMPLATE.name} 与清单的 {key} 不一致",
                )

    def test_contract_template_wires_machine_inbound_endpoints(self) -> None:
        template = parse_env_template(TEMPLATE)
        self.assert_machine_group(template, "CRM_OPPORTUNITY_INTAKE_MACHINE_", CRM_OPPORTUNITY_INTAKE_EXPECTED)
        self.assert_machine_group(template, "CONTRACT_SUMMARY_MACHINE_", CONTRACT_SUMMARY_EXPECTED)

    def test_contract_manifest_wires_machine_inbound_endpoints(self) -> None:
        values = parse_manifest_block(MANIFEST, "values")
        self.assert_machine_group(values, "CRM_OPPORTUNITY_INTAKE_MACHINE_", CRM_OPPORTUNITY_INTAKE_EXPECTED)
        self.assert_machine_group(values, "CONTRACT_SUMMARY_MACHINE_", CONTRACT_SUMMARY_EXPECTED)
        # 清单中的机器入站键必须都能在模板找到，避免模板/清单键集漂移再次断链。
        template = parse_env_template(TEMPLATE)
        for prefix in ("CRM_OPPORTUNITY_INTAKE_MACHINE_", "CONTRACT_SUMMARY_MACHINE_"):
            for suffix in MACHINE_KEY_SUFFIXES:
                key = f"{prefix}{suffix}"
                self.assertIn(key, template, f"模板缺少清单已声明的接线键 {key}")

    def test_contract_settlement_outbound_switch_matches_manifest(self) -> None:
        template = parse_env_template(TEMPLATE)
        values = parse_manifest_block(MANIFEST, "values")
        key = "SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED"
        self.assertIn(key, template, f"模板缺少 {key}")
        self.assertIn(key, values, f"清单 values 缺少 {key}")
        self.assertEqual(template[key].strip().lower(), "true", f"模板 {key} 必须为 true，否则按模板部署会静默停用合同→结算边")
        self.assertEqual(
            template[key].strip().lower(),
            values[key].strip().lower(),
            f"模板与清单的 {key} 不一致",
        )
        # 生产凭据只能由受控接入注入：模板不得内置任何可用凭据，缺失时由启动校验 fail-closed。
        for credential in ("SETTLEMENT_INTEGRATION_CLIENT_ID", "SETTLEMENT_INTEGRATION_CLIENT_SECRET"):
            self.assertIn(credential, template, f"模板缺少 {credential} 占位")
            self.assertEqual(template[credential], "", f"模板 {credential} 必须为空占位，凭据由受控接入注入")
        bindings = parse_manifest_block(MANIFEST, "bindings")
        self.assertEqual(
            bindings.get("SETTLEMENT_INTEGRATION_CLIENT_ID"),
            "service.settlement_contract_ingest.client_id",
            "清单缺少结算出站 client_id 受控绑定",
        )
        self.assertEqual(
            bindings.get("SETTLEMENT_INTEGRATION_CLIENT_SECRET"),
            "service.settlement_contract_ingest.client_secret",
            "清单缺少结算出站 client_secret 受控绑定",
        )

    def test_contract_financial_change_switch_is_declared_and_fail_closed(self) -> None:
        """受控“合同财务变更”开关必须在模板与清单两端都声明，且默认关闭。

        该键此前只存在于合同 config 解析里（默认 false），模板/清单缺键时受控更新
        无键可下发，运维只能手改 runtime env，等于绕开交付门禁。这里把
        “两端声明 + 默认关闭 + 两端同值”绑成同一门禁：开关涉及金额变更
        （XEVT2-05c 单人改额尚待产品拍板），要开启必须同时改模板与清单，
        并显式改本断言，不能只在一侧静默打开。
        """
        key = "CONTRACT_FINANCIAL_CHANGE_ENABLED"
        template = parse_env_template(TEMPLATE)
        values = parse_manifest_block(MANIFEST, "values")
        self.assertIn(key, template, f"{TEMPLATE.name} 缺少 {key}（按模板重建会静默丢键）")
        self.assertIn(key, values, f"{MANIFEST.name} 缺少 {key}（受控更新无键可下发）")
        self.assertEqual(
            template[key].strip().lower(),
            "false",
            f"{TEMPLATE.name} 的 {key} 默认必须关闭（fail-closed）",
        )
        self.assertEqual(
            template[key].strip().lower(),
            values[key].strip().lower(),
            f"{TEMPLATE.name} 与 {MANIFEST.name} 的 {key} 不一致",
        )

    def test_aggregation_worker_receives_all_read_only_dsns(self) -> None:
        environment = parse_compose_service_environment(COMPOSE, "data-analysis-aggregation-worker")
        for key in AGGREGATION_READ_DSNS:
            self.assertIn(key, environment, f"aggregation-worker 环境缺少 {key}")
            self.assertEqual(
                environment[key],
                "${%s:-}" % key,
                f"aggregation-worker 的 {key} 必须由 Compose 变量注入",
            )

    def test_root_env_example_declares_read_only_dsns(self) -> None:
        declared = parse_env_template(ROOT_ENV_EXAMPLE)
        for key in AGGREGATION_READ_DSNS:
            self.assertIn(key, declared, f".env.example 缺少 {key} 占位")

    def test_settlement_inbound_switch_matches_manifest(self) -> None:
        """合同→结算的另一半：结算入站开关不能模板 false / 清单 true。

        合同出站已开启时，结算入站若按模板关闭，财务/责任事件会 401→重试→dead。
        """
        template = parse_env_template(SETTLEMENT_TEMPLATE)
        values = parse_manifest_block(SETTLEMENT_MANIFEST, "values")
        key = "SETTLEMENT_INTEGRATION_ENABLED"
        self.assertIn(key, template, f"结算模板缺少 {key}")
        self.assertIn(key, values, f"结算清单 values 缺少 {key}")
        self.assertEqual(template[key].strip().lower(), "true", f"结算模板 {key} 必须与清单一致为 true")
        self.assertEqual(values[key].strip().lower(), "true", f"结算清单 {key} 必须为 true")

    def test_crm_contract_verification_is_enabled_and_bound(self) -> None:
        """CRM 侧必须真正开启合同摘要核验，并用受控绑定取专用凭据。

        只补合同侧端点/键位但 CRM 开关仍为 false，会继续以 verifier 为空
        返回 503；因此这里把“CRM 开关 + 端点 + scope + 绑定”作为同一门禁断言。
        """
        values = parse_manifest_block(CRM_MANIFEST, "values")
        self.assertEqual(
            values.get("CONTRACT_VERIFICATION_ENABLED", "").strip().lower(),
            "true",
            "生产 CRM 清单必须开启 CONTRACT_VERIFICATION_ENABLED",
        )
        self.assertEqual(values.get("CONTRACT_SUMMARY_SCOPE", "").strip(), "contract.summary.read")
        for key in ("CONTRACT_SUMMARY_URL", "CONTRACT_SUMMARY_TOKEN_URL"):
            self.assertIn(key, values, f"CRM 清单缺少 {key}")
        bindings = parse_manifest_block(CRM_MANIFEST, "bindings")
        self.assertEqual(
            bindings.get("CONTRACT_SUMMARY_CLIENT_ID"),
            "service.contract_summary_read.client_id",
            "CRM 清单缺少合同摘要 client_id 受控绑定",
        )
        self.assertEqual(
            bindings.get("CONTRACT_SUMMARY_CLIENT_SECRET"),
            "service.contract_summary_read.client_secret",
            "CRM 清单缺少合同摘要 client_secret 受控绑定",
        )

    def test_contract_import_crm_provision_scope_is_registered_and_bound(self) -> None:
        """历史导入开立 CRM 客户/商机的机器 scope 必须登记并绑定成专用凭据。

        平台按“一用途一 client 一 scope”授予：客户 scope 由
        crm_customer_import_provision 承载，商机 scope 由
        crm_opportunity_import_provision 承载。清单只补一对 client 却让合同侧
        同时请求两个 scope 时，平台 token 端点会以 invalid_scope 拒绝，故障点
        被推迟到导入联调阶段；因此这里把“用途后缀已登记 + 两个 scope 各自绑定
        到自己的用途 + 开关默认关闭且模板/清单同值”绑成同一门禁。
        """
        pairs = (
            ("crm-customer-import-provision", "crm_customer_import_provision"),
            ("crm-opportunity-import-provision", "crm_opportunity_import_provision"),
        )
        allowed_bindings = next(
            (
                line.strip()
                for line in MANIFEST.read_text(encoding="utf-8").splitlines()
                if line.strip().startswith("allowed_service_bindings:")
            ),
            "",
        )
        self.assertTrue(allowed_bindings, f"{MANIFEST.name} 缺少 allowed_service_bindings")
        for suffix, purpose in pairs:
            with self.subTest(purpose=purpose):
                self.assertIn(
                    suffix,
                    UPDATE_CREDENTIAL_SUFFIXES["contract_management"],
                    f"用途后缀 {suffix} 未登记：清单引用的 client_id 无法由受控接入推导",
                )
                self.assertIn(
                    purpose,
                    allowed_bindings,
                    f"{MANIFEST.name} 的 allowed_service_bindings 缺少 {purpose}："
                    "受控接入不会创建对应凭据客户端",
                )
        bindings = parse_manifest_block(MANIFEST, "bindings")
        values = parse_manifest_block(MANIFEST, "values")
        self.assertEqual(
            bindings.get("CRM_PROVISION_CLIENT_ID"),
            "service.crm_customer_import_provision.client_id",
            "清单缺少客户导入开立 client_id 受控绑定",
        )
        self.assertEqual(
            bindings.get("CRM_PROVISION_CLIENT_SECRET"),
            "service.crm_customer_import_provision.client_secret",
            "清单缺少客户导入开立 client_secret 受控绑定",
        )
        self.assertEqual(
            bindings.get("CRM_PROVISION_OPPORTUNITY_CLIENT_ID"),
            "service.crm_opportunity_import_provision.client_id",
            "清单缺少商机导入开立 client_id 受控绑定",
        )
        self.assertEqual(
            bindings.get("CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET"),
            "service.crm_opportunity_import_provision.client_secret",
            "清单缺少商机导入开立 client_secret 受控绑定",
        )
        self.assertEqual(values.get("CRM_PROVISION_SCOPE"), "customer.import_provision")
        self.assertEqual(values.get("CRM_PROVISION_OPPORTUNITY_SCOPE"), "opportunity.import_provision")
        for key in ("CRM_PROVISION_BASE_URL", "CRM_PROVISION_TOKEN_URL"):
            self.assertIn(key, values, f"{MANIFEST.name} 缺少 {key}")
        key = "CONTRACT_IMPORT_CRM_PROVISION_ENABLED"
        template = parse_env_template(TEMPLATE)
        self.assertIn(key, template, f"{TEMPLATE.name} 缺少 {key}（按模板重建会静默丢键）")
        self.assertIn(key, values, f"{MANIFEST.name} 缺少 {key}（受控更新无键可下发）")
        self.assertEqual(
            template[key].strip().lower(),
            "false",
            f"{TEMPLATE.name} 的 {key} 默认必须关闭（fail-closed）",
        )
        self.assertEqual(
            template[key].strip().lower(),
            values[key].strip().lower(),
            f"{TEMPLATE.name} 与 {MANIFEST.name} 的 {key} 不一致",
        )


class ClientIdNamingLintTest(unittest.TestCase):
    """BIZ-XWIRE-10a：清单里的 client_id 字面量必须能被规范命名规则推导出来。"""

    def test_manifest_client_ids_are_derivable_or_allowlisted(self) -> None:
        """离线 lint：subsystems.d/*.yaml 的 *_CLIENT_ID 字面量必须在期望名集合内。

        平台受控接入按 `applicationCode-environment-suffix` 生成 OAuth Client
        （SubsystemUpdateServiceClientName + 用途后缀表）。清单里写死一个推导不出来的
        字面量，接入时不会创建同名 Client，运行时只会 401/租户拒绝，且失败点在很晚的
        联调阶段才暴露。这里对部署资产做静态对账，失败信息给出文件、键、值、可用期望名。

        不参与比对（由受控接入/渲染写入的占位）：空占位、client_id、
        catalog_publisher_client_id，以及 service.* / related_service.* 引用路径。
        """
        manifests = sorted(SUBSYSTEMS_DIR.glob("*.yaml"))
        self.assertTrue(manifests, f"{SUBSYSTEMS_DIR} 下没有任何子系统清单，lint 失去检查对象")
        checked = 0
        for manifest in manifests:
            _, environment = manifest_application_identity(manifest)
            expected = expected_client_ids(environment)
            self.assertTrue(expected, f"环境 {environment} 的期望 client_id 集合为空，规则表失效")
            for number, key, value in collect_client_id_literals(manifest):
                checked += 1
                with self.subTest(manifest=manifest.name, key=key, value=value):
                    if value in CLIENT_ID_ALLOWLIST:
                        continue
                    # 用 assertTrue + 自述信息，避免 unittest 额外打印整个期望名集合，
                    # 让失败信息保持“文件、键、值、可用期望名”可读。
                    self.assertTrue(
                        value in expected,
                        f"{manifest.name}:{number} 的 {key} 字面量 {value!r} "
                        "既不是可按 SubsystemUpdateServiceClientName "
                        f"(applicationCode-environment-suffix) 推导的规范名，也不在显式 allowlist "
                        f"{sorted(CLIENT_ID_ALLOWLIST)} 内；受控接入不会创建同名 Client，"
                        f"运行时只会 401/租户拒绝。环境 {environment} 下可用的期望名："
                        + ", ".join(sorted(expected)),
                    )
        self.assertGreater(
            checked,
            0,
            f"{SUBSYSTEMS_DIR} 中没有任何 *_CLIENT_ID 字面量可校验："
            "清单可能被改成纯占位，lint 已失去检查对象",
        )

    def test_lint_distinguishes_allowlisted_and_placeholder_values(self) -> None:
        """守卫的守卫：规则本身要能区分规范名、allowlist 与占位。

        防止后继改动把期望集合放空、或把占位前缀写宽后 lint 变成恒真。
        """
        expected = expected_client_ids("prod")
        self.assertIn("data_analysis-prod-contract-dashboard", expected)
        self.assertIn("contract_management-prod-analysis-import-report-status", expected)
        self.assertNotIn("data_analysis-prod-legacy-dashboard", expected)
        self.assertNotIn("platform-handover", expected, "allowlist 不应混进可推导集合")
        self.assertIn("platform-handover", CLIENT_ID_ALLOWLIST)
        for placeholder in (
            "",
            "client_id",
            "catalog_publisher_client_id",
            "service.audit_ingest.client_id",
            "related_service.contract_management.settlement_contract_ingest.oauth_client_id",
        ):
            self.assertTrue(
                is_placeholder_client_id(placeholder),
                f"占位识别应覆盖 {placeholder!r}",
            )
        self.assertFalse(is_placeholder_client_id("data_analysis-prod-contract-dashboard"))

    @unittest.skipUnless(
        UPDATE_CREDENTIAL_REQUIREMENTS_GO.is_file(),
        "缺少 platform/internal 受控接入源码（离线资产包/单测只带 deploy 目录时跳过）",
    )
    def test_embedded_suffix_table_matches_controlled_onboarding_source(self) -> None:
        """内嵌后缀表必须与 Go 需求表一致，否则 lint 会按过期规则放行或误杀。

        只读 Go 源码，不修改它：这里比对的是“代码里的用途后缀”和本文件的内嵌镜像，
        任何一侧新增/改名后缀都会让本用例先失败，提醒同步 lint。
        """
        source = parse_go_credential_suffixes(UPDATE_CREDENTIAL_REQUIREMENTS_GO)
        self.assertTrue(source, f"{UPDATE_CREDENTIAL_REQUIREMENTS_GO.name} 未解析出任何用途后缀")
        self.assertEqual(
            sorted(source),
            sorted(UPDATE_CREDENTIAL_SUFFIXES),
            "Go 侧应用编码集合与内嵌表不一致：" f"仅 Go 有 {sorted(set(source) - set(UPDATE_CREDENTIAL_SUFFIXES))}，"
            f"仅内嵌表有 {sorted(set(UPDATE_CREDENTIAL_SUFFIXES) - set(source))}",
        )
        for code in sorted(source):
            with self.subTest(application=code):
                self.assertEqual(
                    sorted(source[code]),
                    sorted(UPDATE_CREDENTIAL_SUFFIXES[code]),
                    f"{code} 的用途后缀与 Go 需求表不一致："
                    f"仅 Go 有 {sorted(source[code] - set(UPDATE_CREDENTIAL_SUFFIXES[code]))}，"
                    f"仅内嵌表有 {sorted(set(UPDATE_CREDENTIAL_SUFFIXES[code]) - source[code])}",
                )
if __name__ == "__main__":
    unittest.main()
