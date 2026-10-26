#!/usr/bin/env python3
"""Read-only deployment plan for an installed or about-to-be-installed target.

The plan is intentionally conservative: it never starts/stops containers, writes
configuration, or treats a previous READY result as authorization.  It emits a
versioned stable-check report used by deploy.sh, the operations tool and CI.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
from datetime import datetime, timezone

SCHEMA = "uip.deploy-plan/v1"
EXIT_READY, EXIT_BLOCKED, EXIT_ACTION, EXIT_USAGE = 0, 2, 3, 64
COMPONENTS = {
    "platform": ["PLATFORM_IMAGE"],
    "frontend": ["FRONTEND_IMAGE"],
    "customer-opportunity": ["CUSTOMER_CRM_IMAGE"],
    "customer-portal": ["CUSTOMER_PORTAL_IMAGE"],
    "contract": ["CONTRACT_IMAGE"],
    "project": ["PROJECT_IMAGE"],
    "settlement": ["SETTLEMENT_IMAGE"],
    "data-analysis": ["DATA_ANALYSIS_IMAGE", "DATA_ANALYSIS_DASHBOARD_API_IMAGE",
                      "DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE", "DATA_ANALYSIS_ALERT_WORKER_IMAGE",
                      "DATA_ANALYSIS_MIGRATE_IMAGE"],
}
ALL_COMPONENTS = list(COMPONENTS)
BUSINESS_COMPONENTS = [c for c in ALL_COMPONENTS if c not in ("platform", "frontend")]
ACTIONS = ("install", "upgrade", "add", "repair", "resume")
TENANT = re.compile(r"^[0-9A-HJKMNP-TV-Z]{26}$")
DIGEST = re.compile(r"@sha256:[0-9a-f]{64}$")
PLACEHOLDER = re.compile(r"(?:REPLACE_WITH|CHANGE_ME|PENDING_ONBOARDING|example\.com|:main$|:latest$)", re.I)

REQUIRED_ENV = (
    "COMPOSE_PROJECT_NAME",
    "SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT",
    "SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID",
    "MYSQL_HOST",
    "MYSQL_DATABASE",
    "OIDC_ISSUER",
    "APP_PUBLIC_BASE_URL",
)


class Check:
    def __init__(self, check_id, title, status, severity, evidence="", remediation=""):
        self.check_id = check_id
        self.title = title
        self.status = status
        self.severity = severity
        self.evidence = evidence
        self.remediation = remediation

    def as_dict(self):
        return {
            "id": self.check_id,
            "title": self.title,
            "status": self.status,
            "severity": self.severity,
            "evidence": self.evidence,
            "remediation": self.remediation,
        }


def utcnow():
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def read_env(path):
    """Parse a simple KEY=VALUE file without executing it. Values never leave this module."""
    result = {}
    try:
        raw = Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return result
    for line in raw.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key or ""):
            result[key] = value.strip()
    return result


def duplicate_env_keys(path, keys):
    seen = set()
    duplicates = set()
    try:
        raw = Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return []
    for line in raw.splitlines():
        text = line.strip()
        if not text or text.startswith("#") or "=" not in text:
            continue
        key = text.split("=", 1)[0].strip()
        if key in keys:
            if key in seen:
                duplicates.add(key)
            seen.add(key)
    return sorted(duplicates)


def sha256_file(path):
    try:
        h = hashlib.sha256()
        with open(path, "rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                h.update(chunk)
        return "sha256:" + h.hexdigest()
    except OSError:
        return ""


def stat_regular(path):
    try:
        info = os.lstat(path)
    except OSError:
        return None
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        return None
    return info


def mode_ok(path):
    info = stat_regular(path)
    return bool(info and stat.S_IMODE(info.st_mode) == 0o600)


def is_placeholder(value):
    return not value or bool(PLACEHOLDER.search(value))


def run(cmd, cwd, timeout=15):
    try:
        proc = subprocess.run(cmd, cwd=str(cwd), text=True, capture_output=True, timeout=timeout)
        return proc.returncode, (proc.stdout or "").strip(), (proc.stderr or "").strip()
    except (OSError, subprocess.TimeoutExpired) as error:
        return 127, "", str(error)


def safe_detail(text, limit=180):
    text = " ".join((text or "").split())
    text = re.sub(r"(?i)(password|secret|token|client_secret)\s*[:=]\s*\S+", r"\1=<redacted>", text)
    return text[:limit]


def add(checks, check_id, title, status, severity, evidence="", remediation=""):
    checks.append(Check(check_id, title, status, severity, evidence, remediation))


def selected_components(raw):
    values = []
    for item in raw or ["all"]:
        values.extend(part.strip() for part in item.split(",") if part.strip())
    if not values or "all" in values:
        return list(ALL_COMPONENTS)
    result = []
    for value in values:
        if value not in COMPONENTS:
            raise ValueError("不支持的组件：%s" % value)
        if value not in result:
            result.append(value)
    return result


PORT_KEYS = (
    "PUBLIC_HTTP_PORT", "PUBLIC_HTTPS_PORT", "FRONTEND_PORT", "PLATFORM_API_PORT",
    "KEYCLOAK_HTTP_PORT", "CONTRACT_API_PORT", "CUSTOMER_API_PORT", "PORTAL_API_PORT",
    "PROJECT_API_PORT", "SETTLEMENT_API_PORT",
)


def configured_ports(env):
    result = {}
    for key in PORT_KEYS:
        value = (env.get(key) or "").strip()
        if value.isdigit() and 1 <= int(value) <= 65535:
            result[int(value)] = key
    return result


def listening_ports(runner, target):
    tool = shutil.which("ss") or shutil.which("lsof")
    if not tool:
        return None
    if tool.endswith("/ss") or tool == "ss":
        rc, out, err = runner(["ss", "-H", "-ltn"], target, 10)
    else:
        rc, out, err = runner(["lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "n"], target, 10)
    if rc not in (0, 1) or (rc == 1 and out.strip()):
        return None
    ports = set()
    for line in out.splitlines():
        text = line.strip()
        if not text:
            continue
        if text.startswith("n"):
            text = text[1:]
        candidate = text.rsplit(":", 1)[-1] if ":" in text else ""
        if candidate.isdigit():
            ports.add(int(candidate))
            continue
        match = re.search(r":(\d+)(?:\s|$)", text)
        if match:
            ports.add(int(match.group(1)))
    return ports


def docker_published_ports(runner, target):
    rc, out, _ = runner(["docker", "ps", "--format", "{{.Ports}}"], target, 10)
    if rc != 0:
        return set()
    return {int(value) for value in re.findall(r":(\d+)->", out)}


def imported_image(target, component):
    records = sorted((target / "manifests").glob("*.imported"),
                     key=lambda item: item.stat().st_mtime, reverse=True) if (target / "manifests").is_dir() else []
    for record in records:
        value = read_env(record)
        if value.get("COMPONENT") == component and value.get("IMAGE_REF"):
            return value["IMAGE_REF"]
    return ""


def check_compatibility(target, action, forward_recovery, evidence_path, compatibility_path=""):
    path = Path(compatibility_path) if compatibility_path else target / "deployment-compatibility.json"
    if not path.exists():
        if action == "upgrade":
            return "BLOCKED", "BLOCKER", "缺少 deployment-compatibility.json；无法证明上一程序在迁移后仍可运行", \
                   "由发布批次提供经验证的兼容性声明，或走显式前向恢复审批"
        return "NEEDS_ACTION", "WARNING", "本目标尚无部署兼容性声明", \
               "由发布批次提供 deployment-compatibility.json 并纳入资产校验"
    info = stat_regular(path)
    if info is None or info.st_size > 128 * 1024:
        return "BLOCKED", "BLOCKER", "兼容性声明不是安全普通文件或超过大小限制", "恢复受控兼容性声明"
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return "BLOCKED", "BLOCKER", "兼容性声明不是合法 JSON", "恢复受控兼容性声明"
    required = {"schema", "release", "assets_contract", "config_contract", "database", "recovery"}
    if not isinstance(value, dict) or set(value) != required or value.get("schema") != "uip.deployment-compatibility/v1":
        return "BLOCKED", "BLOCKER", "兼容性声明字段或版本无效", "按当前契约重新生成声明"
    database = value.get("database")
    recovery = value.get("recovery")
    if not isinstance(database, dict) or not isinstance(recovery, dict):
        return "BLOCKED", "BLOCKER", "兼容性声明缺少数据库或恢复字段", "按当前契约重新生成声明"
    if not isinstance(database.get("migrate_to"), int) or database.get("migrate_to") < 0:
        return "BLOCKED", "BLOCKER", "迁移目标版本无效", "按迁移清单重新生成声明"
    previous = database.get("previous_program_after_migration")
    recovery_type = recovery.get("type")
    if previous not in ("compatible", "incompatible", "unknown") or recovery_type not in ("backward", "forward-only", "unknown"):
        return "BLOCKED", "BLOCKER", "恢复类型或上一程序兼容性取值无效", "按当前契约重新生成声明"
    if previous == "compatible":
        return "PASS", "INFO", "上一程序在本次迁移后可继续运行", ""
    if action == "upgrade":
        if previous == "incompatible" and not forward_recovery:
            return "BLOCKED", "BLOCKER", "上一程序不能运行迁移后的数据库；普通升级禁止", \
                   "使用明确的前向恢复发行和完整备份；不得回退旧程序"
        if not forward_recovery:
            return "BLOCKED", "BLOCKER", "恢复兼容性未知；默认阻断升级", \
                   "提供经验证的兼容性声明，或显式选择前向恢复并提交恢复材料"
        if not evidence_path:
            return "NEEDS_ACTION", "BLOCKER", "已选择前向恢复但缺少恢复材料", "提供恢复证据文件"
        evidence = Path(evidence_path)
        info = stat_regular(evidence)
        if info is None or info.st_size > 64 * 1024:
            return "BLOCKED", "BLOCKER", "前向恢复材料不是安全普通文件或超过大小限制", "提供受控恢复证据"
        try:
            proof = json.loads(evidence.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return "BLOCKED", "BLOCKER", "前向恢复材料不是合法 JSON", "提供受控恢复证据"
        required_proof = {"backup_sha256", "migration_target", "operator", "approved_at", "recovery_steps"}
        if not isinstance(proof, dict) or set(proof) != required_proof:
            return "BLOCKED", "BLOCKER", "前向恢复材料字段不完整", "按恢复模板补齐材料"
        if not isinstance(proof.get("backup_sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", proof["backup_sha256"]):
            return "BLOCKED", "BLOCKER", "恢复材料缺少完整备份摘要", "提供完整备份的 SHA256"
        if proof.get("migration_target") != database.get("migrate_to"):
            return "BLOCKED", "BLOCKER", "恢复材料的迁移目标与声明不一致", "核对迁移目标"
        if not isinstance(proof.get("recovery_steps"), list) or not proof["recovery_steps"] or \
                not all(isinstance(item, str) and item.strip() and len(item) <= 500 for item in proof["recovery_steps"]):
            return "BLOCKED", "BLOCKER", "恢复步骤为空或格式无效", "补充可执行的前向修复步骤"
        if not isinstance(proof.get("operator"), str) or not proof["operator"].strip() or len(proof["operator"]) > 128:
            return "BLOCKED", "BLOCKER", "恢复材料缺少审核人", "填写审核人"
        return "PASS", "WARNING", "已选择显式前向恢复；材料结构完整，仍需在部署锁内复核", \
               "执行阶段必须重新核验备份、迁移目标和恢复步骤；不得回退旧程序"
    return "NEEDS_ACTION", "WARNING", "恢复兼容性未证明；非升级动作仅做提示", \
           "升级前补齐兼容性声明或前向恢复材料"


def check_session(target, action, script_dir, runner):
    state_dir = Path(str(target) + ".installation-state")
    session_file = state_dir / "session.json"
    if not session_file.exists():
        if action == "resume":
            return "BLOCKED", "BLOCKER", "没有可续跑的安装会话", "使用原介质和原参数创建/继续会话"
        return "PASS", "INFO", "没有安装会话（非续跑动作）", ""
    tool = Path(script_dir) / "deployment-session.py"
    if not tool.exists():
        return "SKIPPED", "WARNING", "安装会话存在但缺少 deployment-session.py，无法解析", "使用完整部署资产"
    rc, out, err = runner([sys.executable or "python3", str(tool), "status", "--state-dir", str(state_dir), "--format", "json"], target, 15)
    if rc != 0:
        return "BLOCKED", "BLOCKER", "安装会话无法读取：%s" % safe_detail(err or out), "保留会话目录并联系部署负责人"
    try:
        value = json.loads(out)
    except ValueError:
        return "BLOCKED", "BLOCKER", "安装会话状态不是合法 JSON", "保留会话目录并联系部署负责人"
    overall = value.get("overall_status", "UNKNOWN")
    if action == "resume":
        if overall == "COMPLETED":
            return "NEEDS_ACTION", "BLOCKER", "上一会话已完成；续跑必须使用 --new-session", "确认验收后显式开启新批次"
        if overall not in ("FAILED", "WAITING", "RUNNING"):
            return "BLOCKED", "BLOCKER", "会话状态 %s 不允许续跑" % overall, "查看 --status 后按原入口处理"
        return "PASS", "INFO", "续跑会话状态：%s" % overall, ""
    if overall in ("FAILED", "WAITING", "RUNNING"):
        return "BLOCKED", "BLOCKER", "存在未完成的安装会话（%s）" % overall, "先按原参数 --resume 处理，不要开新批次"
    return "PASS", "INFO", "安装会话状态：%s" % overall, ""


def baseline_check(target, action, components, fingerprints, images, baseline_path, compare_fingerprints=None):
    if not baseline_path:
        return None
    path = Path(baseline_path)
    info = stat_regular(path)
    if info is None or info.st_size > 2 * 1024 * 1024:
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "计划基线不是安全普通文件或超过大小限制", "重新生成并保存计划 JSON")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "计划基线不是合法 JSON", "重新生成并保存计划 JSON")
    if not isinstance(value, dict) or value.get("schema") != SCHEMA or value.get("action") != action or value.get("target") != str(target):
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "计划基线的 schema/操作/目标与本次不一致", "使用同一目标、同一操作的当前计划")
    if value.get("summary") != "READY" or value.get("exit_code") != 0:
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "原计划未通过全部检查", "处理阻断/待办后重新生成 READY 计划")
    if compare_fingerprints is None and value.get("components") != components:
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "计划组件范围与本次不一致", "按实际组件范围重新生成计划")
    old_fingerprints = value.get("fingerprints")
    old_images = value.get("images")
    if not isinstance(old_fingerprints, dict) or not isinstance(old_images, dict):
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "计划基线缺少指纹或镜像字段", "重新生成并保存计划 JSON")
    changed = []
    keys = compare_fingerprints if compare_fingerprints is not None else fingerprints.keys()
    for key in keys:
        current = fingerprints.get(key, "")
        if old_fingerprints.get(key) != current:
            changed.append("fingerprints." + key)
    if images:
        for component in components:
            old = old_images.get(component) or {}
            current = images.get(component) or {}
            if old.get("current") != current.get("current") or old.get("target") != current.get("target"):
                changed.append("images." + component)
    if changed:
        return Check("PLAN-001", "计划基线", "BLOCKED", "BLOCKER", "计划已过期，现场变化：" + "、".join(changed), "重新执行 plan 并把新报告交给执行入口")
    return Check("PLAN-001", "计划基线", "PASS", "INFO", "现场指纹与基线一致", "")


def build_plan(target, action, components, *, script_dir=None, runner=run, docker_available=None,
               forward_recovery=False, recovery_evidence="", compatibility_path="", baseline_report="", now=None):
    target = Path(target).resolve()
    checks = []
    if script_dir is None:
        script_dir = Path(__file__).resolve().parent

    # CFG-001: target and required files
    if not target.is_dir():
        add(checks, "CFG-001", "目标目录存在", "BLOCKED", "BLOCKER", "目标目录不存在：%s" % target, "先安装部署资产或指定正确 --target")
    else:
        required_files = ("docker-compose.yml", ".env", ".release.env", "subsystems.d")
        missing = [name for name in required_files if not (target / name).exists()]
        config_missing = [name for name in missing if name in (".env", ".release.env")]
        hard_missing = [name for name in missing if name not in config_missing]
        if hard_missing:
            add(checks, "CFG-001", "目标目录存在且包含必需文件", "BLOCKED", "BLOCKER", "缺少：" + "、".join(hard_missing), "先执行资产安装")
        elif config_missing and action == "install":
            add(checks, "CFG-001", "目标目录存在且包含必需文件", "NEEDS_ACTION", "BLOCKER", "首装配置尚未生成：" + "、".join(config_missing), "执行 configure 生成运行配置")
        elif config_missing:
            add(checks, "CFG-001", "目标目录存在且包含必需文件", "BLOCKED", "BLOCKER", "缺少：" + "、".join(config_missing), "恢复运行配置后再规划")
        else:
            add(checks, "CFG-001", "目标目录存在且包含必需文件", "PASS", "INFO", "必需文件齐全", "")

    # CFG-002: permissions and symlinks
    bad = []
    missing_config = []
    for name in (".env", ".release.env"):
        path = target / name
        if not path.exists():
            missing_config.append(name)
        elif not mode_ok(path):
            bad.append(name)
    if bad:
        add(checks, "CFG-002", "运行配置文件权限", "BLOCKED", "BLOCKER", "权限不是 0600 或不是普通文件：" + "、".join(bad), "按部署资产要求修复文件权限，不使用 chmod 777")
    elif missing_config and action == "install":
        add(checks, "CFG-002", "运行配置文件权限", "SKIPPED", "WARNING", "首装配置尚未生成：" + "、".join(missing_config), "由 configure 生成后重新规划")
    elif missing_config:
        add(checks, "CFG-002", "运行配置文件权限", "BLOCKED", "BLOCKER", "运行配置缺失：" + "、".join(missing_config), "恢复运行配置")
    else:
        add(checks, "CFG-002", "运行配置文件权限", "PASS", "INFO", ".env/.release.env 为 0600 普通文件", "")
    compose = target / "docker-compose.yml"
    if not compose.exists():
        add(checks, "CFG-002b", "编排文件安全类型", "SKIPPED", "WARNING", "缺少 docker-compose.yml", "先安装部署资产")
    elif stat_regular(compose) is None:
        add(checks, "CFG-002b", "编排文件安全类型", "BLOCKED", "BLOCKER", "docker-compose.yml 不是普通文件", "恢复受控编排文件")
    else:
        add(checks, "CFG-002b", "编排文件安全类型", "PASS", "INFO", "docker-compose.yml 为普通文件", "")

    env = read_env(target / ".env")
    release = read_env(target / ".release.env")

    # CFG-003: required configuration and tenant
    missing_keys = [key for key in REQUIRED_ENV if not env.get(key)]
    placeholders = [key for key in REQUIRED_ENV if key in env and is_placeholder(env.get(key))]
    if missing_keys:
        status = "NEEDS_ACTION" if action == "install" else "BLOCKED"
        add(checks, "CFG-003", "运行配置必填项", status, "BLOCKER", "缺少：" + "、".join(missing_keys), "执行 configure 或受控补齐配置")
    elif placeholders:
        add(checks, "CFG-003", "运行配置必填项", "NEEDS_ACTION", "BLOCKER", "仍为占位值：" + "、".join(placeholders), "由配置生成器写入真实值，不静默使用默认值")
    else:
        add(checks, "CFG-003", "运行配置必填项", "PASS", "INFO", "必填配置非空且非占位值", "")
    if not (target / ".env").is_file():
        add(checks, "CFG-003b", "运行配置重复键", "SKIPPED", "WARNING", "运行配置尚未生成，无法检查重复键", "configure 后重新规划")
    else:
        duplicates = duplicate_env_keys(target / ".env", set(REQUIRED_ENV))
        if duplicates:
            add(checks, "CFG-003b", "运行配置重复键", "BLOCKED", "BLOCKER", "重复键：" + "、".join(duplicates), "删除重复配置并重新生成，不能依赖后值覆盖")
        else:
            add(checks, "CFG-003b", "运行配置重复键", "PASS", "INFO", "未发现重复的必填键", "")
    tenant = (env.get("SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID") or "").strip()
    if not TENANT.fullmatch(tenant):
        status = "NEEDS_ACTION" if action == "install" else "BLOCKED"
        add(checks, "INIT-001", "租户 ID", status, "BLOCKER", "SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID 不是有效 ULID", "在配置中写入真实租户 ID")
    else:
        add(checks, "INIT-001", "租户 ID", "PASS", "INFO", "租户 ID 格式有效（%s…）" % tenant[:4], "")
    root = (env.get("SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT") or "").strip()
    if root and Path(root).resolve() != target:
        add(checks, "CFG-004", "部署根目录一致性", "BLOCKED", "BLOCKER", "SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT 与实际目标不一致", "修正配置或使用正确 --target")
    else:
        add(checks, "CFG-004", "部署根目录一致性", "PASS", "INFO", "部署根目录与目标一致或未设置", "")

    # STATE: asset transaction / control plane marker
    if (target / "runtime/.assets-install-transaction").exists() or (target / "runtime/.assets-install-transaction").is_symlink():
        add(checks, "STATE-001", "部署资产安装事务", "BLOCKED", "BLOCKER", "检测到未完成的资产安装事务", "使用同包 install-assets.sh --recover 恢复")
    else:
        add(checks, "STATE-001", "部署资产安装事务", "PASS", "INFO", "无未完成资产事务", "")
    if (target / "runtime/.control-plane-reload-required").exists() or (target / "runtime/.control-plane-reload-required").is_symlink():
        add(checks, "STATE-002", "控制面重载标记", "BLOCKED", "BLOCKER", "资产已更新但控制面尚未成对重载", "先执行 deploy.sh reload-control-plane")
    else:
        add(checks, "STATE-002", "控制面重载标记", "PASS", "INFO", "无待重载标记", "")
    status, severity, evidence, remediation = check_session(target, action, script_dir, runner)
    add(checks, "STATE-003", "安装会话状态", status, severity, evidence, remediation)

    # MEDIA: packages/manifests/release references
    package_dir = target / "packages"
    archives = sorted(package_dir.glob("*-linux-amd64.tar.gz")) if package_dir.is_dir() else []
    if not archives:
        add(checks, "MEDIA-001", "镜像包与伴随校验", "NEEDS_ACTION", "BLOCKER", "packages/ 没有镜像包", "把本批镜像包及 .sha256 放入 packages/")
    else:
        broken = [p.name for p in archives if not (p.with_name(p.name + ".sha256")).is_file()]
        if broken:
            add(checks, "MEDIA-001", "镜像包与伴随校验", "BLOCKED", "BLOCKER", "缺少伴随校验：" + "、".join(broken[:5]), "补齐同名 .sha256 后重试")
        else:
            add(checks, "MEDIA-001", "镜像包与伴随校验", "PASS", "INFO", "找到 %d 个镜像包及伴随校验" % len(archives), "")
    records = sorted((target / "manifests").glob("*.imported")) if (target / "manifests").is_dir() else []
    if not records:
        add(checks, "MEDIA-002", "导入记录", "NEEDS_ACTION", "BLOCKER", "manifests/ 没有导入记录", "import 本批镜像包")
    else:
        add(checks, "MEDIA-002", "导入记录", "PASS", "INFO", "找到 %d 条导入记录" % len(records), "")
    for component in components:
        keys = COMPONENTS[component]
        values = [(key, (release.get(key) or "").strip()) for key in keys]
        bad = [key for key, value in values if not DIGEST.search(value)]
        missing = [key for key, value in values if not value]
        if missing:
            status = "NEEDS_ACTION" if action in ("install", "add") else "BLOCKED"
            add(checks, "MEDIA-003-%s" % component, "组件镜像引用：%s" % component, status, "BLOCKER",
                "缺少 release 键：" + "、".join(missing), "import/prepare 该组件后再规划")
        elif bad:
            add(checks, "MEDIA-003-%s" % component, "组件镜像引用：%s" % component, "NEEDS_ACTION", "BLOCKER",
                "不是不可变 @sha256 引用：" + "、".join(bad), "重新 import 并 prepare 该组件")
        else:
            imported = imported_image(target, component)
            imported_digest = DIGEST.search(imported or "")
            release_digests = {DIGEST.search(value).group(0) for _, value in values if DIGEST.search(value)}
            if not imported:
                status = "NEEDS_ACTION" if action in ("install", "add") else "BLOCKED"
                add(checks, "MEDIA-003-%s" % component, "组件镜像引用：%s" % component, status, "BLOCKER",
                    "没有匹配的导入记录", "先 import 该组件")
            elif imported_digest and imported_digest.group(0) not in release_digests:
                status = "NEEDS_ACTION" if action in ("install", "add") else "BLOCKED"
                add(checks, "MEDIA-003-%s" % component, "组件镜像引用：%s" % component, status, "BLOCKER",
                    "release 指针与最新导入记录不一致", "重新 import/prepare 该组件")
            else:
                add(checks, "MEDIA-003-%s" % component, "组件镜像引用：%s" % component, "PASS", "INFO",
                    "引用均为不可变 digest，且与最新导入记录一致", "")

    # DOCKER: availability and compose parse
    if docker_available is None:
        docker_available = shutil.which("docker") is not None
    if not docker_available:
        add(checks, "DOCKER-001", "Docker/Compose 可用性", "BLOCKED", "BLOCKER", "未找到 docker 命令", "安装/启动 Docker 后重试")
        add(checks, "DOCKER-002", "Compose 配置解析", "SKIPPED", "WARNING", "Docker 不可用，跳过", "")
    else:
        rc, out, err = runner(["docker", "compose", "version"], target, 10)
        if rc != 0:
            add(checks, "DOCKER-001", "Docker/Compose 可用性", "BLOCKED", "BLOCKER", "docker compose 不可用：%s" % safe_detail(err or out), "安装 Compose 插件")
            add(checks, "DOCKER-002", "Compose 配置解析", "SKIPPED", "WARNING", "Compose 不可用，跳过", "")
        else:
            add(checks, "DOCKER-001", "Docker/Compose 可用性", "PASS", "INFO", "docker compose 可用", "")
            if not (target / "docker-compose.yml").is_file():
                add(checks, "DOCKER-002", "Compose 配置解析", "SKIPPED", "WARNING", "缺少编排文件", "")
            elif not (target / ".env").is_file() or not (target / ".release.env").is_file():
                add(checks, "DOCKER-002", "Compose 配置解析", "SKIPPED", "WARNING", "运行配置尚未生成，无法解析 Compose", "configure 后重新规划")
            else:
                rc, out, err = runner(["docker", "compose", "--project-directory", str(target), "--file", str(target / "docker-compose.yml"),
                                       "--env-file", str(target / ".env"), "--env-file", str(target / ".release.env"), "config", "--quiet"], target, 20)
                if rc != 0:
                    add(checks, "DOCKER-002", "Compose 配置解析", "BLOCKED", "BLOCKER", "compose config 失败（退出码 %d）；未输出原始错误以避免泄露配置" % rc, "在本机查看 compose 错误并修正配置/编排后重试")
                else:
                    add(checks, "DOCKER-002", "Compose 配置解析", "PASS", "INFO", "compose config 通过", "")

    # DB connectivity for non-install actions
    db_connected = False
    if action != "install":
        rc, out, err = runner(["docker", "ps", "-q", "--filter", "label=com.docker.compose.service=platform-mysql"], target, 10) if docker_available else (127, "", "docker unavailable")
        container = out.splitlines()[0].strip() if rc == 0 and out.strip() else ""
        if not container:
            add(checks, "DB-001", "平台数据库连接", "BLOCKED", "BLOCKER", "未找到运行中的 platform-mysql 容器", "先启动/恢复平台数据库")
        else:
            rc2, out2, err2 = runner(["docker", "exec", container, "sh", "-c",
                                      'mysqladmin ping --silent -h 127.0.0.1 -u"$MYSQL_USER" -p"$MYSQL_PASSWORD"'], target, 15)
            if rc2 != 0:
                add(checks, "DB-001", "平台数据库连接", "BLOCKED", "BLOCKER", "platform-mysql 连接失败", "检查数据库容器、凭据和网络")
            else:
                db_connected = True
                add(checks, "DB-001", "平台数据库连接", "PASS", "INFO", "platform-mysql 连接成功", "")
    else:
        add(checks, "DB-001", "平台数据库连接", "SKIPPED", "INFO", "首装前数据库尚不应运行", "")

    # Personnel import machine client: precheck only; never create it here.
    client_id = (env.get("IAM_IMPORT_FILE_GATEWAY_CLIENT_ID") or "").strip()
    client_secret = (env.get("IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET") or "").strip()
    configured = bool(client_id and client_id != "REPLACE_WITH_PLATFORM_IAM_IMPORT_FILE_GATEWAY_CLIENT_ID"
                      and client_secret and not client_secret.startswith("REPLACE_WITH_"))
    if configured:
        add(checks, "INIT-002", "人员导入机器客户端", "PASS", "INFO", "客户端 ID/密钥已配置（不输出值）", "")
    elif action == "install":
        add(checks, "INIT-002", "人员导入机器客户端", "PASS", "INFO", "尚未配置；平台发布阶段将按租户初始化，预检不创建", "")
    elif not db_connected:
        add(checks, "INIT-002", "人员导入机器客户端", "BLOCKED", "BLOCKER", "数据库不可连接，无法在发布阶段初始化客户端", "先恢复数据库连接")
    elif not os.access(target / ".env", os.W_OK):
        add(checks, "INIT-002", "人员导入机器客户端", "BLOCKED", "BLOCKER", ".env 不可写，无法写入一次性客户端密钥", "以实际部署账号修复 .env 写入权限")
    else:
        add(checks, "INIT-002", "人员导入机器客户端", "PASS", "INFO", "尚未配置；平台发布阶段将初始化，预检不创建", "")

    # RES: disk/inode
    try:
        usage = shutil.disk_usage(target)
        free_gib = usage.free / (1024 ** 3)
        st = os.statvfs(target)
        inode_total = st.f_files or 1
        inode_free_pct = (st.f_favail / inode_total) * 100
        if free_gib < 5:
            add(checks, "RES-001", "磁盘与 inode", "BLOCKED", "BLOCKER", "可用磁盘仅 %.1f GiB" % free_gib, "释放空间或迁移数据盘")
        elif inode_free_pct < 5:
            add(checks, "RES-001", "磁盘与 inode", "BLOCKED", "BLOCKER", "可用 inode 仅 %.1f%%" % inode_free_pct, "清理小文件或迁移数据盘")
        else:
            add(checks, "RES-001", "磁盘与 inode", "PASS", "INFO", "可用 %.1f GiB / inode %.1f%%" % (free_gib, inode_free_pct), "")
    except (OSError, ValueError):
        add(checks, "RES-001", "磁盘与 inode", "NEEDS_ACTION", "WARNING", "无法读取目标文件系统资源", "检查挂载和权限")

    ports = configured_ports(env)
    if not ports:
        add(checks, "PORT-001", "端口冲突", "SKIPPED", "WARNING", "未配置可检查的宿主端口", "configure 后重新规划")
    elif not docker_available:
        add(checks, "PORT-001", "端口冲突", "SKIPPED", "WARNING", "Docker 不可用，无法区分本部署端口", "")
    else:
        listening = listening_ports(runner, target)
        if listening is None:
            add(checks, "PORT-001", "端口冲突", "SKIPPED", "WARNING", "缺少 ss/lsof，无法检查监听端口", "安装 iproute2 或 lsof 后复跑")
        else:
            own = docker_published_ports(runner, target)
            conflicts = sorted(port for port in ports if port in listening and port not in own)
            if conflicts:
                add(checks, "PORT-001", "端口冲突", "BLOCKED", "BLOCKER",
                    "已被其他进程占用：" + "、".join(str(port) for port in conflicts), "停止占用进程或修改配置端口")
            else:
                add(checks, "PORT-001", "端口冲突", "PASS", "INFO", "配置端口未被其他进程占用", "")

    # Docker/containerd data root must be on a filesystem with room to import.
    if docker_available:
        rc, out, err = runner(["docker", "info", "--format", "{{.DockerRootDir}}"], target, 10)
        docker_root = out.strip() if rc == 0 and out.strip() else ""
        if not docker_root:
            add(checks, "RES-002", "Docker 数据根分区", "SKIPPED", "WARNING", "无法读取 DockerRootDir", "检查 Docker 权限/状态")
        else:
            try:
                docker_free = shutil.disk_usage(docker_root).free / (1024 ** 3)
            except OSError:
                add(checks, "RES-002", "Docker 数据根分区", "SKIPPED", "WARNING", "DockerRootDir 不可读：%s" % docker_root, "检查挂载和权限")
            else:
                if docker_free < 5:
                    add(checks, "RES-002", "Docker 数据根分区", "BLOCKED", "BLOCKER", "Docker 数据盘仅 %.1f GiB" % docker_free, "释放 Docker 数据盘或迁移数据根")
                else:
                    add(checks, "RES-002", "Docker 数据根分区", "PASS", "INFO", "Docker 数据盘可用 %.1f GiB" % docker_free, "")
    else:
        add(checks, "RES-002", "Docker 数据根分区", "SKIPPED", "WARNING", "Docker 不可用，跳过", "")

    # Backup must fit on its own filesystem, not just the target filesystem.
    backup_probe = target / "backups/system"
    if not backup_probe.exists():
        backup_probe = target / "backups" if (target / "backups").exists() else target
    try:
        backup_free = shutil.disk_usage(backup_probe).free / (1024 ** 3)
    except OSError:
        add(checks, "RES-003", "备份分区容量", "SKIPPED", "WARNING", "无法读取备份分区", "检查备份挂载和权限")
    else:
        if backup_free < 5:
            add(checks, "RES-003", "备份分区容量", "BLOCKED", "BLOCKER", "备份分区仅 %.1f GiB" % backup_free, "释放备份空间或迁移备份目录")
        else:
            add(checks, "RES-003", "备份分区容量", "PASS", "INFO", "备份分区可用 %.1f GiB" % backup_free, "")

    # PERM: backup target/sources
    backup_parent = target / "backups"
    backup_system = backup_parent / "system"
    writable = backup_system if backup_system.exists() else backup_parent if backup_parent.exists() else target
    if not os.access(writable, os.W_OK):
        add(checks, "PERM-001", "备份目标可写", "BLOCKED", "BLOCKER", "备份目录不可写：%s" % writable, "以实际部署账号修复目录权限")
    else:
        add(checks, "PERM-001", "备份目标可写", "PASS", "INFO", "备份目录可写", "")
    runtime = target / "runtime"
    if not runtime.exists():
        status = "SKIPPED" if action == "install" else "BLOCKED"
        add(checks, "PERM-002", "运行配置可读", status, "BLOCKER" if status == "BLOCKED" else "WARNING", "runtime/ 不存在", "首装由部署资产创建；已安装环境应恢复 runtime/")
    elif not os.access(runtime, os.R_OK | os.X_OK):
        add(checks, "PERM-002", "运行配置可读", "BLOCKED", "BLOCKER", "runtime/ 不可读或不可穿越", "以实际部署账号修复权限")
    else:
        add(checks, "PERM-002", "运行配置可读", "PASS", "INFO", "runtime/ 可读", "")
    gateway_value = (env.get("FILE_GATEWAY_HOST_ROOT") or "data/file-gateway").strip()
    gateway = Path(gateway_value)
    if not gateway.is_absolute():
        gateway = target / gateway
    gateway = gateway.resolve()
    if gateway.exists() and not os.access(gateway, os.R_OK | os.X_OK):
        add(checks, "PERM-003", "文件网关源可读", "BLOCKED", "BLOCKER", "文件网关目录不可读：%s" % gateway, "以实际部署账号修复权限，不使用 777")
    else:
        add(checks, "PERM-003", "文件网关源可读", "PASS", "INFO", "文件网关目录可读或尚未创建", "")

    # COMPAT
    status, severity, evidence, remediation = check_compatibility(target, action, forward_recovery, recovery_evidence, compatibility_path)
    add(checks, "COMPAT-001", "升级/恢复兼容性", status, severity, evidence, remediation)

    # SCOPE/action preconditions
    if action == "install":
        if (target / ".env").exists() and (target / ".release.env").exists() and (target / "docker-compose.yml").exists():
            add(checks, "SCOPE-001", "首装目标状态", "BLOCKED", "BLOCKER", "目标已有部署文件；首装不允许覆盖", "改用 upgrade 或指定空目标")
        else:
            add(checks, "SCOPE-001", "首装目标状态", "PASS", "INFO", "目标尚未形成完整部署", "")
    elif action in ("upgrade", "repair", "add", "resume"):
        if not (target / ".env").is_file() or not (target / ".release.env").is_file():
            add(checks, "SCOPE-001", "已安装目标", "BLOCKED", "BLOCKER", "目标不是完整已安装环境", "先完成首装或指定正确 --target")
        else:
            add(checks, "SCOPE-001", "已安装目标", "PASS", "INFO", "目标具备已安装环境文件", "")

    images = {}
    for component in components:
        current = (release.get(COMPONENTS[component][0]) or "").strip()
        images[component] = {"current": current, "target": imported_image(target, component) or current}
    # Fingerprints (never include values)
    fingerprints = {
        "compose_sha256": sha256_file(target / "docker-compose.yml"),
        "runtime_env_sha256": sha256_file(target / ".env"),
        "release_env_sha256": sha256_file(target / ".release.env"),
        "compatibility_sha256": sha256_file(Path(compatibility_path) if compatibility_path else target / "deployment-compatibility.json"),
    }
    baseline = baseline_check(target, action, components, fingerprints, images, baseline_report)
    if baseline is not None:
        checks.append(baseline)
    return build_document(target, action, components, checks, fingerprints, images, now)


def compatibility_document(target, action, components, *, compatibility_path="", forward_recovery=False,
                          recovery_evidence="", baseline_report="", now=None):
    target = Path(target).resolve()
    checks = []
    status, severity, evidence, remediation = check_compatibility(
        Path(target), action, forward_recovery, recovery_evidence, compatibility_path)
    add(checks, "COMPAT-001", "升级/恢复兼容性", status, severity, evidence, remediation)
    fingerprints = {
        "compatibility_sha256": sha256_file(Path(compatibility_path) if compatibility_path else Path(target) / "deployment-compatibility.json"),
        "runtime_env_sha256": sha256_file(Path(target) / ".env"),
    }
    baseline = baseline_check(target, action, components, fingerprints, {}, baseline_report,
                              compare_fingerprints={"runtime_env_sha256", "compatibility_sha256"})
    if baseline is not None:
        checks.append(baseline)
    return build_document(target, action, components, checks, fingerprints, {}, now)


def build_document(target, action, components, checks, fingerprints, images, now=None):
    blockers = [c for c in checks if c.status == "BLOCKED"]
    actions = [c for c in checks if c.status == "NEEDS_ACTION"]
    if blockers:
        summary, code = "BLOCKED", EXIT_BLOCKED
    elif actions:
        summary, code = "NEEDS_ACTION", EXIT_ACTION
    else:
        summary, code = "READY", EXIT_READY
    stages = {
        "install": ["preflight", "assets", "packages", "configure", "platform", "frontend", "subsystems", "verify"],
        "upgrade": ["preflight", "backup", "assets", "compose", "packages", "platform", "frontend", "subsystems", "verify"],
        "add": ["preflight", "packages", "prepare", "platform-adoption", "continue", "verify"],
        "repair": ["preflight", "backup", "recreate", "verify"],
        "resume": ["preflight", "session-reverify", "current-stage", "verify"],
    }[action]
    impact = {
        "install": ["平台与前端启动", "按交付范围接入子系统"],
        "upgrade": ["平台 API/控制面重启", "前端重启", "所选子系统受控更新"],
        "add": ["新增子系统候选/接入", "不改动未选择系统"],
        "repair": ["按当前不可变镜像重建容器", "不升级、不删数据"],
        "resume": ["重验原会话目标状态", "按当前阶段继续"],
    }[action]
    recovery = {
        "requires_backup": action in ("upgrade", "repair", "resume"),
        "point_of_no_return": "数据库迁移开始后，镜像回退不能撤销迁移；必须记录已迁移事实并选择经批准的恢复路径。",
        "strategy": "普通升级在恢复兼容性未证明时阻断；显式前向恢复必须提供完整备份、迁移目标和经验证的修复步骤。",
    }
    return {
        "schema": SCHEMA,
        "generated_at": now or utcnow(),
        "action": action,
        "target": str(target),
        "operator": os.environ.get("SUDO_USER") or os.environ.get("USER") or "unknown",
        "components": components,
        "summary": summary,
        "exit_code": code,
        "checks": [c.as_dict() for c in checks],
        "fingerprints": fingerprints,
        "images": images,
        "stages": stages,
        "service_impact": impact,
        "recovery": recovery,
    }


def human(document):
    labels = {"PASS": "通过", "BLOCKED": "阻断", "NEEDS_ACTION": "需要处理", "SKIPPED": "跳过"}
    lines = [
        "部署计划（只读，不启动/停止容器，不修改配置）",
        "目标：%s" % document["target"],
        "操作：%s" % document["action"],
        "执行账号：%s" % document["operator"],
        "组件：%s" % "、".join(document["components"]),
        "结论：%s（退出码 %s）" % (document["summary"], document["exit_code"]),
        "",
    ]
    for item in document["checks"]:
        lines.append("[%s] %s %s" % (labels.get(item["status"], item["status"]), item["id"], item["title"]))
        if item["evidence"]:
            lines.append("       证据：%s" % item["evidence"])
        if item["remediation"] and item["status"] != "PASS":
            lines.append("       下一步：%s" % item["remediation"])
    for component, value in document.get("images", {}).items():
        lines.append("镜像[%s]：%s → %s" % (component, value.get("current") or "未登记", value.get("target") or "未登记"))
    lines.extend(["", "预计阶段：" + " → ".join(document["stages"]),
                  "服务影响：" + "；".join(document["service_impact"]),
                  "恢复策略：" + document["recovery"]["strategy"]])
    if document["summary"] != "READY":
        lines.append("当前不可执行；先处理上面的阻断/待办，再重新生成计划。")
    else:
        lines.append("计划可执行；执行入口仍会在部署锁内重新核验镜像、配置、数据库和状态指纹。")
    return "\n".join(lines)


class Parser(argparse.ArgumentParser):
    def error(self, message):
        self.print_usage(sys.stderr)
        sys.stderr.write("参数错误：%s\n" % message)
        raise SystemExit(EXIT_USAGE)


def main(argv=None):
    parser = Parser(description="只读部署计划：生成稳定检查项、执行阶段和恢复边界。")
    parser.add_argument("--target", required=True, help="部署目录")
    parser.add_argument("--action", required=True, choices=ACTIONS)
    parser.add_argument("--component", action="append", default=[], help="组件，可重复或用逗号分隔；默认 all")
    parser.add_argument("--format", choices=("human", "json"), default="human")
    parser.add_argument("--forward-recovery", action="store_true", help="显式选择前向恢复")
    parser.add_argument("--recovery-evidence", default="", help="前向恢复材料 JSON")
    parser.add_argument("--compatibility", default="", help="兼容性声明路径；默认使用目标目录内声明")
    parser.add_argument("--compatibility-only", action="store_true", help="只做执行前兼容性门禁")
    parser.add_argument("--baseline-report", default="", help="此前 plan JSON；用于检测现场指纹变化")
    parser.add_argument("--now", default="", help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    try:
        components = selected_components(args.component)
    except ValueError as error:
        sys.stderr.write(str(error) + "\n")
        return EXIT_USAGE
    if args.compatibility_only:
        document = compatibility_document(Path(args.target), args.action, components,
                                          compatibility_path=args.compatibility,
                                          forward_recovery=args.forward_recovery,
                                          recovery_evidence=args.recovery_evidence,
                                          baseline_report=args.baseline_report,
                                          now=args.now or None)
    else:
        document = build_plan(Path(args.target), args.action, components,
                              forward_recovery=args.forward_recovery,
                              recovery_evidence=args.recovery_evidence,
                              compatibility_path=args.compatibility,
                              baseline_report=args.baseline_report,
                              now=args.now or None)
    if args.format == "json":
        json.dump(document, sys.stdout, ensure_ascii=False, sort_keys=True, indent=2)
        sys.stdout.write("\n")
    else:
        print(human(document))
    return document["exit_code"]


if __name__ == "__main__":
    sys.exit(main())
