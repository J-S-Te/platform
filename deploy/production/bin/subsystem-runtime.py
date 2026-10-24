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
    return compose, conditional, files


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


def selected_services(root, compose, conditional, files, enabled):
    root = root.resolve()
    runtime = compose["runtime_services"]
    if any(service not in enabled for service in runtime):
        raise ValueError("required production runtime service is disabled or missing")
    selected_files = set()
    for path, file in files.items():
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("runtime", "owned"))
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
        services = selected_services(root, compose, conditional, files, set(enabled))
    print("\n".join(services))


if __name__ == "__main__":
    try:
        main()
    except (OSError, UnicodeError, ValueError) as error:
        print("生产运行组件清单读取失败：" + str(error), file=sys.stderr)
        sys.exit(1)
