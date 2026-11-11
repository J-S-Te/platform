#!/usr/bin/env python3
"""读取已审核生产配置的服务投影，不依赖 YAML 库。

部署策略仅接受随包配置中使用的受限标量及列表语法。
不支持的策略语法会拒绝放行，避免静默遗漏工作进程。
Agent 仍是完整清单校验的权威来源。
"""

import argparse
import os
from pathlib import Path
import re
import stat
import sys


PROFILES = {
    "contract": ("contract_management", "contract_management-prod.yaml"),
    "project": ("project_management", "project_management-prod.yaml"),
    "customer-opportunity": ("customer_and_opportunity", "customer_and_opportunity-prod.yaml"),
    "customer-portal": ("customer_portal", "customer_portal-prod.yaml"),
    "reimbursement": ("reimbursement", "reimbursement-prod.yaml"),
    "settlement": ("settlement", "settlement-prod.yaml"),
    "data-analysis": ("data_analysis", "data-analysis-prod.yaml"),
}
SERVICE = re.compile(r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*\Z")
ENV_KEY = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")
RUNTIME_PATH = re.compile(r"runtime/[A-Za-z0-9_.-]+\.env\Z")
LIST_FIELDS = {"runtime_services", "teardown_services", "initialization_services"}


def read_regular(path, limit=1024 * 1024):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or path.resolve() != path or info.st_size > limit:
        raise ValueError("unsafe or oversized deployment policy file")
    with path.open("r", encoding="utf-8") as source:
        content = source.read(limit + 1)
    if len(content) > limit or "\x00" in content:
        raise ValueError("invalid deployment policy file")
    return content


def scalar(value, pattern):
    value = value.split(" #", 1)[0].strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        value = value[1:-1]
    if not pattern.fullmatch(value):
        raise ValueError("unsupported deployment policy scalar")
    return value


def list_value(lines, index, indent, value, pattern):
    value = value.split(" #", 1)[0].strip()
    if value.startswith("[") and value.endswith("]"):
        values = [] if not value[1:-1].strip() else [scalar(item, pattern) for item in value[1:-1].split(",")]
    elif not value:
        values = []
        for line in lines[index + 1:]:
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            if len(line) - len(line.lstrip()) <= indent:
                break
            match = re.fullmatch(r" {" + str(indent + 2) + r"}- (.+)", line)
            if not match:
                raise ValueError("unsupported deployment policy list")
            values.append(scalar(match[1], pattern))
    else:
        raise ValueError("unsupported deployment policy list")
    if len(set(values)) != len(values):
        raise ValueError("duplicate deployment policy list member")
    return values


# 用途：读取生产应用的服务归属、运行文件及有限可选能力策略。
# 参数：content为审核清单文本，application为目标应用编码。
# 返回值：服务、条件、运行文件投影；非法策略失败关闭。
def projection(content, application):
    lines = content.splitlines()
    if any("\t" in line[:len(line) - len(line.lstrip())] for line in lines):
        raise ValueError("tabs are not supported in deployment policy")
    roots = {}
    for index, line in enumerate(lines):
        if re.fullmatch(r"[a-z_]+:.*", line):
            key, value = line.split(":", 1)
            if key in roots:
                raise ValueError("duplicate deployment profile section")
            roots[key] = (index, value.strip())
    if roots.get("version", (None, None))[1] != "1" or not {"application", "runtime", "compose"} <= roots.keys():
        raise ValueError("unsupported deployment profile version or sections")
    app = {}
    compose = {}
    conditional = {}
    files = {}
    section = ""
    file = None
    conditional_section = False
    database_section = False
    for index, line in enumerate(lines):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith(" "):
            section = line.split(":", 1)[0]
            file = None
            continue
        if section == "application":
            match = re.fullmatch(r"  (code|environment): (.+)", line)
            if match:
                if match[1] in app:
                    raise ValueError("duplicate application identity")
                app[match[1]] = scalar(match[2], re.compile(r"[a-z][a-z0-9_]*\Z"))
        elif section == "runtime":
            match = re.fullmatch(r"    - path: (.+)", line)
            if match:
                path = scalar(match[1], RUNTIME_PATH)
                if path in files:
                    raise ValueError("duplicate runtime policy file")
                file = {"path": path, "required": [], "seen": set()}
                files[path] = file
                continue
            match = re.fullmatch(r"      (when_service|required_existing_keys):(?: (.*))?", line)
            if match and file is not None:
                if match[1] in file["seen"]:
                    raise ValueError("duplicate runtime condition")
                file["seen"].add(match[1])
                if match[1] == "when_service":
                    file["when"] = scalar(match[2] or "", SERVICE)
                else:
                    file["required"] = list_value(lines, index, 6, match[2] or "", ENV_KEY)
        elif section == "compose":
            match = re.fullmatch(r"  ([a-z_]+):(?: (.*))?", line)
            if match:
                key, value = match[1], match[2] or ""
                conditional_section = key == "conditional_runtime_services"
                database_section = key == "database"
                if key in LIST_FIELDS or key in {"migrate_service", "catalog_sync_service", "conditional_runtime_services", "database"}:
                    if key in compose:
                        raise ValueError("duplicate Compose service policy")
                    if key in LIST_FIELDS:
                        compose[key] = list_value(lines, index, 2, value, SERVICE)
                    elif key in {"migrate_service", "catalog_sync_service"}:
                        compose[key] = scalar(value, SERVICE)
                    else:
                        if value not in {"", "{}"}:
                            raise ValueError("unsupported Compose mapping policy")
                        compose[key] = True
                continue
            if conditional_section:
                match = re.fullmatch(r"    ([a-z][a-z0-9-]*): (.+)", line)
                if not match or match[1] in conditional:
                    raise ValueError("invalid conditional runtime service policy")
                conditional[scalar(match[1], SERVICE)] = scalar(match[2], RUNTIME_PATH)
            elif database_section:
                match = re.fullmatch(r"    service: (.+)", line)
                if match:
                    if "database_service" in compose:
                        raise ValueError("duplicate database service policy")
                    compose["database_service"] = scalar(match[1], SERVICE)
    if app != {"code": application, "environment": "prod"}:
        raise ValueError("deployment profile application/environment mismatch")
    runtime = compose.get("runtime_services", [])
    if not runtime:
        raise ValueError("deployment profile has no runtime services")
    prefix = {
        "customer_and_opportunity": "customer-", "customer_portal": "portal-",
        "contract_management": "contract-", "project_management": "project-",
    }.get(application, application.replace("_", "-") + "-")
    owned = runtime + compose.get("teardown_services", []) + compose.get("initialization_services", [])
    owned += [compose.get(field) for field in ("migrate_service", "catalog_sync_service", "database_service")]
    if any(service and not service.startswith(prefix) for service in owned):
        raise ValueError("deployment service is outside application ownership")
    for service, path in conditional.items():
        if service not in runtime or path not in files or not files[path].get("when"):
            raise ValueError("conditional service requires a reviewed conditional runtime file")
    compose["optional_features"] = optional_features(lines, application, compose, files)
    return compose, conditional, files


# 用途：解析审核过的嵌入基础设施策略，与Agent有限能力校验保持一致。
# 参数：lines为清单行，application为应用编码，compose/files为已校验服务和运行文件。
# 返回值：可选策略；未审核字段、服务或配置边界返回错误。
def optional_features(lines, application, compose, files):
    features = []
    active = False
    feature = None
    list_fields = {"runtime_services", "initialization_services", "required_existing_keys"}
    key = ""
    for index, line in enumerate(lines):
        if line == "optional_features:":
            active = True
            continue
        if not active or not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith(" "):
            break
        match = re.fullmatch(r"  - name: (.+)", line)
        if match:
            feature = {"name": scalar(match[1], ENV_KEY)}
            features.append(feature)
            continue
        match = re.fullmatch(r"    ([a-z_]+):(?: (.*))?", line)
        if match and feature is not None:
            key, value = match[1], match[2] or ""
            if key in feature:
                raise ValueError("duplicate optional feature field")
            if key in list_fields:
                feature[key] = list_value(lines, index, 4, value,
                                          ENV_KEY if key == "required_existing_keys" else SERVICE)
            elif key in {"runtime_file", "enabled_key", "default_enabled"}:
                pattern = {"runtime_file": RUNTIME_PATH, "enabled_key": ENV_KEY,
                           "default_enabled": re.compile(r"(?:true|false)\Z")}[key]
                feature[key] = scalar(value, pattern)
            else:
                raise ValueError("unreviewed optional feature field")
        elif not (key in list_fields and re.fullmatch(r"      - .+", line)):
            raise ValueError("unsupported optional feature policy")
    if len(features) > 1:
        raise ValueError("duplicate or unsupported optional feature")
    for feature in features:
        if (application != "data_analysis" or feature["name"] != "dashboard_embed" or
                feature.get("enabled_key") != "DASHBOARD_EMBED_ENABLED" or
                feature.get("runtime_file") not in files or
                feature.get("runtime_services") != ["data-analysis-metabase"] or
                any(k != "METABASE_EMBEDDING_SECRET" for k in feature.get("required_existing_keys", []))):
            raise ValueError("optional feature is outside reviewed capability policy")
        for kind in ("runtime_services", "initialization_services"):
            if any(service not in compose.get(kind, []) for service in feature.get(kind, [])):
                raise ValueError("optional service is not declared by the application")
    return features


# 用途：只移除显式关闭的可选基础设施，启用模式继续检查必要材料。
# 参数：root为私有部署根，compose为受控策略，kind为进程或初始化集合。
# 返回值：保持原顺序的集合；非法开关、缺材料或不安全路径返回错误。
def feature_services(root, compose, kind):
    disabled = set()
    for feature in compose.get("optional_features", []):
        path = root / feature["runtime_file"]
        if path.exists() and stat.S_IMODE(path.lstat().st_mode) & 0o077:
            raise ValueError("optional feature runtime file must be private")
        values = environment_values(read_regular(path)) if path.exists() else {}
        enabled = values.get(feature["enabled_key"], "").strip() or feature.get("default_enabled", "false")
        if enabled not in {"true", "false"}:
            raise ValueError("optional feature flag must be true or false")
        if enabled == "false":
            disabled.update(feature.get(kind, []))
        elif any(not values.get(key, "").strip() or values[key].startswith(("PENDING_", "REPLACE_WITH_"))
                 for key in feature.get("required_existing_keys", [])):
            raise ValueError("enabled optional feature configuration is incomplete")
    return [service for service in compose.get(kind, []) if service not in disabled]


def environment_values(content):
    result = {}
    for line in content.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].strip()
        key, delimiter, value = line.partition("=")
        if delimiter and ENV_KEY.fullmatch(key.strip()):
            # 匹配 Agent 的 parseEnvironmentValues，绝不执行变量展开。
            result[key.strip()] = value.strip().strip("\"'")
    return result


# 用途：读取门户唯一客户来源，防止本地模式启动旧CRM邀请链。
# 参数：root为部署根，compose及files为当前已审核清单。
# 返回值：是否为本地来源；文件不安全或模式非法时拒绝投影。
def portal_local_directory(root, compose, files):
    if "portal-api" not in compose.get("runtime_services", []) or "runtime/portal.env" not in files:
        return False
    path = root / "runtime/portal.env"
    if not path.exists() and not path.is_symlink():
        return False
    content = read_regular(path)
    if path.stat().st_mode & 0o077:
        raise ValueError("portal directory runtime file is not private")
    mode = environment_values(content).get("PORTAL_CUSTOMER_DIRECTORY_MODE", "").strip()
    if mode not in ("", "crm", "local"):
        raise ValueError("portal customer directory mode is invalid")
    return mode == "local"


# 用途：选取本轮已安装且配置就绪的应用进程。
# 参数：root为部署根，其余参数为已校验策略与实际Compose服务集合。
# 返回值：当前启动集合；必需服务缺失或能力材料非法时返回错误。
def selected_services(root, compose, conditional, files, enabled):
    root = root.resolve()
    runtime = feature_services(root, compose, "runtime_services")
    if any(service not in enabled for service in runtime):
        raise ValueError("required production runtime service is disabled or missing")
    selected_files = set()
    local_portal = portal_local_directory(root, compose, files)
    for path, file in files.items():
        if local_portal and path == "runtime/customer.env":
            continue
        if file.get("when"):
            if file["when"] not in enabled:
                continue
            try:
                values = environment_values(read_regular(root / path))
            except (OSError, ValueError):
                continue
            if any(not values.get(key, "").strip() or values[key].strip().startswith(("PENDING_", "REPLACE_WITH_")) for key in file["required"]):
                continue
        selected_files.add(path)
    return [service for service in runtime if service not in conditional or conditional[service] in selected_files]


def owned_services(component, compose):
    result = []
    for field in ("runtime_services", "teardown_services", "initialization_services"):
        result.extend(compose.get(field, []))
    result.extend(compose.get(field) for field in ("migrate_service", "catalog_sync_service"))
    # 合同数据库也存储共享 Temporal 数据库，不能作为可选合同业务卸载的一部分停止。
    if component != "contract":
        result.append(compose.get("database_service"))
    return list(dict.fromkeys(service for service in result if service))


# 用途：向受控发布入口输出运行、初始化或归属服务投影。
# 参数：无；参数来自命令行和仅含服务名的标准输入。
# 返回值：正常输出集合；非法策略由入口转换为非零退出码。
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("runtime", "owned", "initialization"))
    parser.add_argument("--target", required=True)
    parser.add_argument("--profiles-dir")
    parser.add_argument("--component", choices=PROFILES, required=True)
    args = parser.parse_args()
    root = Path(os.path.abspath(args.target))
    if root.is_symlink() or not root.is_dir():
        raise ValueError("deployment root is unsafe")
    root = root.resolve()
    directory = Path(os.path.abspath(args.profiles_dir)) if args.profiles_dir else root / "subsystems.d"
    if directory.is_symlink() or not directory.is_dir():
        raise ValueError("deployment profile directory is unsafe")
    directory = directory.resolve()
    application, filename = PROFILES[args.component]
    compose, conditional, files = projection(read_regular(directory / filename), application)
    if args.action == "owned":
        services = owned_services(args.component, compose)
    else:
        enabled_text = sys.stdin.read(1024 * 1024 + 1)
        enabled = enabled_text.split()
        if len(enabled_text) > 1024 * 1024 or any(not SERVICE.fullmatch(service) for service in enabled):
            raise ValueError("enabled Compose service inventory is invalid")
        if args.action == "initialization":
            services = feature_services(root, compose, "initialization_services")
            if any(service not in enabled for service in services):
                raise ValueError("required initialization service is disabled or missing")
        else:
            services = selected_services(root, compose, conditional, files, set(enabled))
    print("\n".join(services))


if __name__ == "__main__":
    try:
        main()
    except (OSError, UnicodeError, ValueError) as error:
        print("生产运行组件清单读取失败：" + str(error), file=sys.stderr)
        sys.exit(1)
