#!/usr/bin/env python3
"""模板漂移门禁：比对本地模板与生产模板的键集差异。

背景
----
`platform/.env.example`（本地/开发）与 `platform/deploy/production/.env.example`
（生产）面向两种完全不同的运行环境，键集本来就不相等。此前没有任何门禁检查这件事，
后果是：任意一侧新增/删除变量，另一侧要到真正部署时才以“变量未设置 → 容器起不来”
或“静默使用代码默认值”的形式暴露，属于只能靠现场事故发现的漂移。

本脚本因此只做一件事：把“两侧键集不对称”这件事变成可评审的显式清单。
清单（下方 ONLY_ROOT / ONLY_PRODUCTION）必须逐条写明**为什么**这个键只在一侧出现；
清单之外出现的新差异一律 exit 1。删除键之后留下的清单条目会作为“陈旧项”提示，
避免并行修改模板时把门禁变成误报源。

判定口径
--------
1. 键集漂移（硬门禁）：`只在一侧出现的键` 必须出现在对应清单里，否则失败。
2. 值差异（不判定，仅 --verbose 展示）：同名键在两侧取不同的值是**设计如此**——
   本地是 `http://localhost:...`，生产是 `https://...`。值级别的约束（占位符、
   空值、危险默认）由各模板自身注释与 `deploy/production/tests/` 里的专项用例负责，
   不在这里重复实现，否则本脚本会因为“本地/生产的正常差异”长期红着而被跳过。
3. 重复键：同一文件内出现同名键直接失败（后面的值会静默覆盖前面的值）。

用法
----
    python3 -B scripts/check-env-template-drift.py            # 门禁模式
    python3 -B scripts/check-env-template-drift.py --verbose  # 附带值差异明细

退出码：0 = 无清单外漂移；1 = 存在清单外漂移或模板不可解析。

维护提示
--------
* 白名单基线以**当前工作树**的两个模板为准；模板改动后必须同步改这里的分组注释。
* 新增“只在一侧”的键时，先确认它是不是真的只该在一侧（多数情况应该两侧都写），
  只有确实环境专属才加入清单，并在分组注释里说明归属。
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

PLATFORM_ROOT = Path(__file__).resolve().parents[1]
ROOT_TEMPLATE = PLATFORM_ROOT / ".env.example"
PRODUCTION_TEMPLATE = PLATFORM_ROOT / "deploy" / "production" / ".env.example"

# `KEY=VALUE`、`export KEY=VALUE` 两种写法都要认；行内 `#` 不剥离（模板里没有行内注释，
# 强行剥离会把含 `#` 的口令截断，宁可让它在值差异里显示出来）。
_ASSIGNMENT = re.compile(r"^(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)$")


def _unquote(raw: str) -> str:
    """去掉成对的单/双引号，与 test_integration_wiring.py 的模板解析保持一致。"""
    value = raw.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        return value[1:-1]
    return value


def parse_env_template(path: Path) -> dict:
    """解析 KEY=VALUE 模板，跳过注释与空行；重复键直接报错，不静默覆盖。"""
    if not path.is_file():
        raise SystemExit(f"[drift] 模板不存在：{path}")
    values: dict = {}
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = _ASSIGNMENT.match(stripped)
        if not match:
            continue
        key = match.group(1)
        if key in values:
            raise SystemExit(f"[drift] {path.name}:{number} 出现重复键 {key}")
        values[key] = _unquote(match.group(2))
    if not values:
        raise SystemExit(f"[drift] {path.name} 没有解析出任何键")
    return values


# ---------------------------------------------------------------------------
# 本地模板专属键（只在 platform/.env.example 出现）
#
# 分组依据：这些键服务于“在本机用 compose.local.yaml 起一套开发环境”或
# “本地子系统接入/首次引导”这类只在开发机存在的流程；生产侧要么由部署脚本
# 从受控来源注入（runtime/*.env、密钥目录），要么根本不存在该流程。
# ---------------------------------------------------------------------------
ONLY_ROOT = {
    # 本地子系链接入/编排：由 scripts/subsystem*.sh 与本地 provisioner 使用，
    # 生产侧走 subsystem-provisioner + deploy/production/subsystems.d 清单，
    # 不读这些“宿主机路径/容器网络名”类键，因此不进生产模板。
    "本地子系统接入与编排（scripts/subsystem*.sh + 本地 provisioner）": {
        "SUBSYSTEM_DOCKER_BINARY",
        "SUBSYSTEM_GATEWAY_INCLUDE_PATH",
        "SUBSYSTEM_GATEWAY_SCRIPT_PATH",
        "SUBSYSTEM_ONBOARDING_AUTOMATION_ENABLED",
        "SUBSYSTEM_ONBOARDING_MODE",
        "SUBSYSTEM_ONBOARDING_TIMEOUT",
        "SUBSYSTEM_PLATFORM_COMPOSE_PROJECT",
        "SUBSYSTEM_PLATFORM_DOCKER_NETWORK",
        "SUBSYSTEM_PLATFORM_FRONTEND_SERVICE",
        "SUBSYSTEM_PRODUCTION_COMPOSE_FILE",
        "SUBSYSTEM_PRODUCTION_DEPLOY_ROOT",
        "SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH",
        "SUBSYSTEM_PRODUCTION_RUNTIME_ENV_PATH",
        "SUBSYSTEM_PROJECTS_ROOT",
        "SUBSYSTEM_PROVISIONING_SOCKET_PATH",
    },
    # 首次引导管理员：本地允许直接给初始账号名/显示名/口令，
    # 生产必须走 bootstrap-admin 受控流程（口令不落在模板与 Git 里），故不在生产模板出现。
    "本地首次引导管理员（生产走 bootstrap-admin 受控流程，不写模板）": {
        "IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME",
        "IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME",
        "IAM_BOOTSTRAP_ADMIN_PASSWORD",
    },
    # 本地 Keycloak 平台 OIDC 与管理客户端：本地 compose 里的 Keycloak 需要显式客户端
    # 凭据与 issuer/回调路径；生产由平台按 Application 清单与 runtime 许可文件下发，
    # 不入模板（关键凭据不放模板也是安全约束）。
    "本地 Keycloak 平台 OIDC/管理客户端（生产由 Application 清单下发）": {
        "KEYCLOAK_ADMIN_CLIENT_ID",
        "KEYCLOAK_ADMIN_CLIENT_SECRET",
        "KEYCLOAK_OUTAGE_LOGIN_POLICY",
        "KEYCLOAK_PLATFORM_OIDC_CLIENT_ID",
        "KEYCLOAK_PLATFORM_OIDC_CLIENT_SECRET",
        "KEYCLOAK_PLATFORM_OIDC_ENABLED",
        "KEYCLOAK_PLATFORM_OIDC_ISSUER",
        "KEYCLOAK_PLATFORM_OIDC_REDIRECT_PATH",
    },
    # 本地客户引用加密/鉴权上下文与 MYSQL_APPLICATION_ALLOWED_HOST：
    # 本地允许关闭（AUTHZ_CONTEXT_CUSTOMER_REF_ENABLED 等）并用本地回环做多应用联调；
    # 生产侧这些能力由运行期许可与部署环境决定，不在生产模板开默认值。
    "本地客户引用加密与联调放行（生产不开默认值）": {
        "AUTHZ_CONTEXT_CUSTOMER_REF_ENABLED",
        "AUTH_LOGIN_TARGET_ALLOWED_ORIGINS",
        "IAM_CUSTOMER_REF_DIGEST_KEY",
        "IAM_CUSTOMER_REF_ENCRYPTION_KEY",
        "MYSQL_APPLICATION_ALLOWED_HOST",
    },
    # File Gateway 本地存储与本地巡检：清理/对账周期只对本地常驻网关有意义，
    # 生产由 compose 的 FILE_GATEWAY_* 固定值（见 docker-compose.yml）与容量告警覆盖。
    "File Gateway 本地存储与巡检（生产由 compose 固定值覆盖）": {
        "FILE_GATEWAY_CLEANUP_BATCH_SIZE",
        "FILE_GATEWAY_CLEANUP_ENABLED",
        "FILE_GATEWAY_CLEANUP_INTERVAL",
        "FILE_GATEWAY_CLEANUP_MINIMUM_AGE",
        "FILE_GATEWAY_DATABASE_DSN",
        "FILE_GATEWAY_RECONCILIATION_BATCH_SIZE",
        "FILE_GATEWAY_RECONCILIATION_INTERVAL",
        "FILE_GATEWAY_RECONCILIATION_STALE_AFTER",
        # FILE_GATEWAY_S3_* 死键已从本地模板删除（cmd/file-gateway/main.go 只接受 local，
        # S3/OSS/MinIO 后端未实现），因此不再登记；若日后重新引入，门禁会以“未登记”报红。
    },
}

# ---------------------------------------------------------------------------
# 生产模板专属键（只在 deploy/production/.env.example 出现）
#
# 分组依据：这些键要么是生产编排/发布/运维专属（端口、镜像源、备份、TLS、告警），
# 要么是生产必需的强制凭据占位符（REPLACE_WITH_*），本地没有对应容器或流程。
# ---------------------------------------------------------------------------
ONLY_PRODUCTION = {
    # compose 项目名与各子系统端口：本地由 compose.local.yaml 固定/推导，
    # 生产必须在模板里显式声明，发布脚本据此渲染 nginx 与健康检查。
    "生产编排：项目名与各 API/前端端口": {
        "COMPOSE_PROJECT_NAME",
        "PLATFORM_API_PORT",
        "CONTRACT_API_PORT",
        "CUSTOMER_API_PORT",
        "DATA_ANALYSIS_API_PORT",
        "PORTAL_API_PORT",
        "PROJECT_API_PORT",
        "SETTLEMENT_API_PORT",
        "FRONTEND_PORT",
        "FRONTEND_BIND_ADDRESS",
        "FRONTEND_IPV4_ADDRESS",
    },
    # 对外暴露与 TLS：本地走回环 HTTP；生产要公网域名、HTTPS 端口、证书路径与排空宽限。
    "生产对外暴露与 TLS（域名/证书/排空）": {
        "PUBLIC_HTTPS_ENABLED",
        "PUBLIC_HTTPS_PORT",
        "PUBLIC_HTTP_PORT",
        "PUBLIC_HTTPS_DRAIN_GRACE",
        "PUBLIC_PLATFORM_HOST",
        "PUBLIC_SSO_HOST",
        "PUBLIC_SSO_HTTP_PORT",
        "PUBLIC_SSO_HTTPS_PORT",
        "PUBLIC_TLS_CERTIFICATE_PATH",
        "PUBLIC_TLS_PRIVATE_KEY_PATH",
        "SSO_TLS_CERTIFICATE_PATH",
        "SSO_TLS_PRIVATE_KEY_PATH",
    },
    # 各子系统独立库：生产每库一套口令/root 口令（本地用同一个共享 MySQL）。
    "生产分库凭据（每子系统独立 MySQL）": {
        "MYSQL_ROOT_PASSWORD",
        "MYSQL_USER",
        "CONTRACT_MYSQL_PASSWORD",
        "CONTRACT_MYSQL_ROOT_PASSWORD",
        "CUSTOMER_MYSQL_PASSWORD",
        "CUSTOMER_MYSQL_ROOT_PASSWORD",
        "DASHBOARD_MYSQL_PASSWORD",
        "DASHBOARD_MYSQL_ROOT_PASSWORD",
        "PORTAL_MYSQL_PASSWORD",
        "PORTAL_MYSQL_ROOT_PASSWORD",
        "PROJECT_MYSQL_PASSWORD",
        "PROJECT_MYSQL_ROOT_PASSWORD",
        # 报销独立库由生产Compose消费；本地根模板不承载其专属运行凭据。
        "REIMBURSEMENT_MYSQL_PASSWORD",
        "REIMBURSEMENT_MYSQL_ROOT_PASSWORD",
        "SETTLEMENT_MYSQL_PASSWORD",
        "SETTLEMENT_MYSQL_ROOT_PASSWORD",
        "DASHBOARD_TIMEZONE",
    },
    # 跨子系统只读 DSN 与内网地址：生产是容器网络内的服务名，
    # 本地对应能力由 compose.local.yaml 与 scripts 推导，不写进本地模板。
    "生产跨子系统只读 DSN 与内网地址": {
        "CONTRACT_READ_DSN",
        "CRM_READ_DSN",
        "PROJECT_READ_DSN",
        "SETTLEMENT_READ_DSN",
        "CONTRACT_INTERNAL_URL",
        "PROJECT_INTERNAL_URL",
        "DATA_ANALYSIS_PUBLIC_ORIGIN",
        "TEMPORAL_NAMESPACE",
        "TEMPORAL_TASK_QUEUE",
        "TEMPORAL_TLS",
    },
    # 生产告警收敛开关：本地不跑 alert-worker，开关默认关闭（ALERT_AUTO_RESOLVE=false）。
    "生产告警收敛开关": {
        "ALERT_AUTO_RESOLVE",
    },
    # Keycloak 生产备份：本地 Keycloak 容器是一次性开发实例，不做备份轮转。
    "生产 Keycloak 备份与保留": {
        "KEYCLOAK_BACKUP_DIR",
        "KEYCLOAK_BACKUP_PROM_FILE",
        "KEYCLOAK_BACKUP_RETENTION_DAYS",
    },
    # 生产会话/身份开关：这些开关在生产必须显式声明（默认 fail-closed 或强制严格）。
    # AUTH_CENTRAL_SESSION_ENABLED、IAM_APPLICATION_ACCESS_REVISION_STRICT、HANDOVER_*
    # 与 METRICS_ACCESS_TOKEN、PLATFORM_AUDIT_HASH_KEY、IAM_IMPORT_FILE_GATEWAY_*
    # 现在两侧模板都声明了（本地也要能跑同样的收敛/交接/抓取路径），因此不再是漂移项。
    "生产会话与身份严格性开关": {
        "AUTH_LEGACY_PLATFORM_ACCESS_TOKEN_ENABLED",
        "SUBSYSTEM_ALLOW_INSECURE_HTTP_SESSION",
        "SUBSYSTEM_OIDC_BACKCHANNEL_BASE_URL",
        "SUBSYSTEM_OIDC_ISSUER",
        "SUBSYSTEM_PRODUCTION_ALLOW_PLACEHOLDER_DATABASE_CREDENTIALS",
        "SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT",
    },
    # 生产机器令牌地址：本地机器认证走 compose.local 内的直连，不需要外部 token URL。
    "生产机器令牌地址": {
        "MACHINE_TOKEN_URL",
    },
    # 生产 File Gateway 容量与宿主根：本地的目录编排在 compose.local.yaml 里写死。
    "生产 File Gateway 容量与宿主根": {
        "FILE_GATEWAY_CAPACITY_REJECT_PERCENT",
        "FILE_GATEWAY_HOST_ROOT",
    },
    # 生产 SLA 扫描范围：本地只用默认租户。
    "生产 SLA 扫描范围": {
        "PROJECT_SLA_SCAN_TENANTS",
    },
}


def flatten(groups: dict) -> dict:
    """把 {分组说明: {键...}} 摊平成 {键: 分组说明}，并检查一个键只归属一个分组。"""
    flattened: dict = {}
    for description, keys in groups.items():
        for key in keys:
            if key in flattened:
                raise SystemExit(
                    f"[drift] 白名单分组冲突：{key} 同时属于 "
                    f"“{flattened[key]}”与“{description}”"
                )
            flattened[key] = description
    return flattened


def report(title: str, keys: list) -> None:
    if not keys:
        return
    print(f"  {title}（{len(keys)}）：")
    for key in keys:
        print(f"    - {key}")


def main(argv: list) -> int:
    parser = argparse.ArgumentParser(
        description="校验本地模板与生产模板的键集漂移（清单外差异 exit 1）"
    )
    parser.add_argument(
        "--verbose", action="store_true", help="列出值差异明细与陈旧清单条目"
    )
    parser.add_argument(
        "--root-template", default=str(ROOT_TEMPLATE), help="本地模板路径（默认 platform/.env.example）"
    )
    parser.add_argument(
        "--production-template",
        default=str(PRODUCTION_TEMPLATE),
        help="生产模板路径（默认 platform/deploy/production/.env.example）",
    )
    arguments = parser.parse_args(argv)

    root_values = parse_env_template(Path(arguments.root_template))
    production_values = parse_env_template(Path(arguments.production_template))

    allowed_root = flatten(ONLY_ROOT)
    allowed_production = flatten(ONLY_PRODUCTION)

    only_root = sorted(set(root_values) - set(production_values))
    only_production = sorted(set(production_values) - set(root_values))
    unexpected_root = [key for key in only_root if key not in allowed_root]
    unexpected_production = [key for key in only_production if key not in allowed_production]

    print(
        "[drift] 本地模板 {0} 键 / 生产模板 {1} 键 / 仅本地 {2} / 仅生产 {3}".format(
            len(root_values), len(production_values), len(only_root), len(only_production)
        )
    )

    failures = 0
    if unexpected_root:
        failures += len(unexpected_root)
        print("[drift][FAIL] 只出现在本地模板、且不在白名单里的键：")
        report("新增/未登记", unexpected_root)
        print(
            "  处理方式：确认它是否真的只该在本地——多数变量应在两侧模板都声明；"
            "确实环境专属时才加入 ONLY_ROOT 并写清归属分组。"
        )
    if unexpected_production:
        failures += len(unexpected_production)
        print("[drift][FAIL] 只出现在生产模板、且不在白名单里的键：")
        report("新增/未登记", unexpected_production)
        print(
            "  处理方式：同步补进本地模板（本地也需要能跑通），"
            "或加入 ONLY_PRODUCTION 并写清归属分组。"
        )

    # 白名单卫生检查（只提示，不失败）。两类都要可见，否则清单会随时间失真：
    #   MISSING    白名单登记了某个键，但该键已从模板里彻底删除 → 登记项已失效。
    #   REDUNDANT  白名单登记了“只在单侧出现”，但该键现在两侧模板都有了 → 已无漂移可免。
    # 之所以不做成硬门禁：模板与白名单可能由不同人/不同批次并行修改，把“清理清单”
    # 变成失败项会让并行的模板收敛工作互相卡死。
    missing_root = sorted(set(allowed_root) - set(root_values))
    missing_production = sorted(set(allowed_production) - set(production_values))
    shared_keys = set(root_values) & set(production_values)
    redundant_root = sorted(set(allowed_root) & shared_keys)
    redundant_production = sorted(set(allowed_production) & shared_keys)
    hygiene_total = (
        len(missing_root) + len(missing_production) + len(redundant_root) + len(redundant_production)
    )
    if hygiene_total:
        print(
            "[drift][WARN] 白名单需要清理 {0} 条：MISSING {1} / REDUNDANT {2}"
            "（不影响本次判定，仅提示清单与模板已不同步）".format(
                hygiene_total,
                len(missing_root) + len(missing_production),
                len(redundant_root) + len(redundant_production),
            )
        )
        if arguments.verbose:
            report("MISSING：仅本地白名单里已失效的键", missing_root)
            report("MISSING：仅生产白名单里已失效的键", missing_production)
            report("REDUNDANT：仅本地白名单里已两侧都有的键", redundant_root)
            report("REDUNDANT：仅生产白名单里已两侧都有的键", redundant_production)

    # 值差异：设计如此（本地 vs 生产），从不失败；--verbose 才展示，避免把正常差异当噪声。
    value_divergence = [
        key
        for key in sorted(set(root_values) & set(production_values))
        if root_values[key] != production_values[key]
    ]
    if arguments.verbose:
        print(
            "[drift] 同名键取值不同 {0} 个（本地与生产环境差异，不作为门禁判定）：".format(
                len(value_divergence)
            )
        )
        for key in value_divergence:
            print(f"    - {key}: 本地={root_values[key]!r} 生产={production_values[key]!r}")

    if failures:
        print(
            "[drift] 结论：失败——{0} 个键的漂移不在白名单内。".format(failures)
        )
        return 1

    print(
        "[drift] 结论：通过——两侧键集差异全部在已登记白名单内"
        "（本地专属 {0} / 生产专属 {1} 条登记）。".format(len(allowed_root), len(allowed_production))
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
