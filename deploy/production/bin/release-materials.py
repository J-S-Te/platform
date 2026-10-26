#!/usr/bin/env python3
"""Scenario-driven release material decision core.

This is a read-only planner. It never builds, signs, installs or mutates a
delivery. Registration protocols come from the inspected release spec, never
from a filename, a manual switch or an untrusted JSON field.
"""

import argparse
import json
import os
import re
import sys
from pathlib import Path

SCHEMA = "uip.release-materials/v1"
SCENARIOS = ("fresh", "update", "add", "migrate", "sign-only", "package", "license-renew", "repair")
BUILD_SCENARIOS = {"fresh", "update", "add", "migrate"}
APP_BY_COMPONENT = {
    "contract": "contract_management",
    "customer-opportunity": "customer_and_opportunity",
    "customer-portal": "customer_portal",
    "project": "project_management",
    "settlement": "settlement",
    "data-analysis": "data_analysis",
}
COMPONENTS = tuple(APP_BY_COMPONENT) + ("platform", "frontend")
PROTOCOLS = (1, 2)
OWNER = {
    "commercial-license": "vendor",
    "runtime-approval": "vendor",
    "release-manifest": "vendor",
    "build-evidence": "vendor",
    "old-install-evidence": "customer",
    "entitlement": "vendor",
    "backup": "customer",
    "compatibility": "vendor",
    "frozen-artifacts": "vendor",
    "source-freeze": "vendor",
}


class Invalid(ValueError):
    pass


def regular_file(path):
    if not path:
        return False
    try:
        info = os.lstat(path)
    except OSError:
        return False
    return os.path.isfile(path) and not os.path.islink(path) and info.st_size > 0


def regular_dir(path):
    if not path:
        return False
    try:
        info = os.lstat(path)
    except OSError:
        return False
    return os.path.isdir(path) and not os.path.islink(path)


def load_spec(path):
    if not path:
        return None, "缺少 release spec，无法从权威清单读取应用和登记协议"
    file = Path(path)
    if not regular_file(file):
        return None, "release spec 不是安全普通文件"
    try:
        value = json.loads(file.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None, "release spec 不是合法 JSON"
    if not isinstance(value, dict) or value.get("schema") != "urn:uip:release-spec:v1":
        return None, "release spec schema 不受支持"
    return value, ""


def spec_protocols(spec):
    protocols = {}
    apps = spec.get("applications") if spec else None
    if not isinstance(apps, list) or not apps:
        return protocols, ["release spec 没有 applications"]
    problems = []
    for app in apps:
        if not isinstance(app, dict):
            problems.append("release spec application 不是对象")
            continue
        code = app.get("code")
        if not isinstance(code, str) or not code:
            problems.append("release spec application 缺少 code")
            continue
        if code in protocols:
            problems.append("release spec 重复 application: " + code)
            continue
        components = app.get("components")
        if not isinstance(components, list) or not components:
            problems.append("application 缺少 components: " + code)
            continue
        values = set()
        for component in components:
            if not isinstance(component, dict) or component.get("registration_protocol") not in PROTOCOLS:
                problems.append("application 存在未知登记协议: " + code)
                continue
            values.add(component["registration_protocol"])
        if len(values) != 1:
            problems.append("application 内部登记协议矛盾: " + code)
            continue
        protocols[code] = values.pop()
    return protocols, problems


def selected_apps(spec, components):
    protocols, problems = spec_protocols(spec)
    selected = []
    raw = components or []
    if not raw:
        selected = sorted(protocols)
    else:
        for component in raw:
            if component in ("platform", "frontend"):
                continue
            app = APP_BY_COMPONENT.get(component)
            if not app:
                problems.append("未知组件: " + component)
                continue
            if app not in protocols:
                problems.append("release spec 缺少选中应用: " + app)
                continue
            if app not in selected:
                selected.append(app)
    selected.sort()
    return selected, protocols, problems


def material(materials, material_id, title, scope, requirement, satisfied, reason, action):
    materials.append({
        "id": material_id,
        "title": title,
        "scope": scope,
        "requirement": requirement,
        "satisfied": bool(satisfied),
        "owner": OWNER.get(material_id, "vendor"),
        "reason": reason,
        "next_action": action,
    })


def decide(args):
    scenario = args.scenario
    if scenario not in SCENARIOS:
        raise Invalid("不支持的场景: " + scenario)
    if args.component and any(item not in COMPONENTS for item in args.component):
        raise Invalid("存在不支持的组件")
    spec_required = scenario in BUILD_SCENARIOS
    if not spec_required:
        apps, protocols, problems = [], {}, []
    else:
        spec, spec_error = load_spec(args.spec)
        apps, protocols, problems = selected_apps(spec, args.component)
        if spec is None:
            problems.append(spec_error)
    materials = []
    build_required = scenario in BUILD_SCENARIOS
    license_required = scenario in ("add", "migrate", "license-renew")
    license_optional = scenario in ("fresh", "repair")
    approval_required = scenario in ("fresh", "update", "add", "migrate")
    manifest_required = scenario in ("fresh", "update", "add", "migrate", "sign-only", "repair")
    backup_required = scenario in ("update", "add", "migrate")
    compatibility_required = scenario in ("update", "migrate", "repair")
    frozen_required = scenario in ("sign-only", "package", "repair")

    if build_required:
        material(materials, "source-freeze", "冻结源码与构建输入", "release",
                 "REQUIRED", bool(args.source_freeze), "构建场景必须先冻结源码", "先完成各仓库提交/版本冻结")
        material(materials, "build-evidence", "构建事实与证据", "release",
                 "REQUIRED", regular_file(args.build_evidence), "镜像和源码事实必须可验证", "运行构建并生成 build evidence")
    else:
        material(materials, "build-evidence", "构建事实与证据", "release",
                 "NOT_APPLICABLE", True, "本场景不重新构建镜像", "")

    if license_required:
        material(materials, "commercial-license", "商业授权 JWS", "customer",
                 "REQUIRED", regular_file(args.license), "该场景需要有效商业授权", "导入与目标实例匹配的 JWS")
    elif license_optional:
        material(materials, "commercial-license", "商业授权 JWS", "customer",
                 "OPTIONAL", regular_file(args.license), "可先安装后按真实实例补签", "浏览器/平台授权页或 license-activate.sh")
    else:
        material(materials, "commercial-license", "商业授权 JWS", "customer",
                 "NOT_APPLICABLE", True, "该场景不改变商业授权", "")

    for app in apps:
        protocol = protocols.get(app)
        if protocol not in PROTOCOLS:
            material(materials, "runtime-approval:" + app, "运行批准 " + app, "application",
                     "UNKNOWN", False, "应用登记协议未知", "重新生成 release spec 并从权威目录确认协议")
            continue
        if protocol == 2 or not approval_required:
            material(materials, "runtime-approval:" + app, "运行批准 " + app, "application",
                     "NOT_APPLICABLE", True, "V2 内置目录不要求逐镜像批准" if protocol == 2 else "本场景不引入新的 V1 运行批准", "")
        else:
            path = Path(args.approval_dir) / ("runtime-license-" + app + "-prod.json") if args.approval_dir else None
            material(materials, "runtime-approval:" + app, "运行批准 " + app, "application",
                     "REQUIRED", bool(path and regular_file(path)), "V1 应用继续要求对应运行批准", "完成厂商正式审核并交付 runtime-license JSON")

    if app_needs_entitlement(scenario, args, apps):
        material(materials, "entitlement", "新增应用购买/接入资格", "customer",
                 "REQUIRED", regular_file(args.entitlement), "增加系统不能扩大已有购买范围", "提供购买范围或授权签发材料")

    if scenario == "migrate":
        material(materials, "old-install-evidence", "旧安装边界与运行证据", "customer",
                 "REQUIRED", regular_file(args.old_install_evidence), "迁移必须基于真实旧安装事实", "导出旧安装清单并审核")
    else:
        material(materials, "old-install-evidence", "旧安装边界与运行证据", "customer",
                 "NOT_APPLICABLE", True, "非迁移场景不要求旧安装证据", "")

    material(materials, "release-manifest", "签名发布清单与证明", "release",
             "REQUIRED" if manifest_required else "NOT_APPLICABLE",
             regular_file(args.release_manifest) if manifest_required else True,
             "发布事实、镜像和证明必须随介质交付" if manifest_required else "本场景不重新签名发布",
             "完成冻结、审核和发行签名" if manifest_required else "")

    material(materials, "frozen-artifacts", "已冻结的发行产物", "release",
             "REQUIRED" if frozen_required else "NOT_APPLICABLE",
             regular_dir(args.frozen_dir) if frozen_required else True,
             "补签/包装/修复只能复用已冻结产物" if frozen_required else "构建场景重新生成产物",
             "提供已冻结发行目录" if frozen_required else "")

    material(materials, "backup", "升级前完整备份", "customer",
             "REQUIRED" if backup_required else "NOT_APPLICABLE",
             regular_file(args.backup_evidence) if backup_required else True,
             "迁移或替换程序前必须可恢复" if backup_required else "本场景不执行升级",
             "执行完整备份并交付校验证据" if backup_required else "")

    material(materials, "compatibility", "升级/恢复兼容性声明", "release",
             "REQUIRED" if compatibility_required else "NOT_APPLICABLE",
             regular_file(args.compatibility) if compatibility_required else True,
             "无法证明旧程序可运行时不得普通升级" if compatibility_required else "非升级场景不要求",
             "提供经验证的兼容性声明或显式前向恢复材料" if compatibility_required else "")

    required_missing = [item for item in materials if item["requirement"] == "REQUIRED" and not item["satisfied"]]
    unknown = [item for item in materials if item["requirement"] == "UNKNOWN"]
    if problems or unknown:
        overall = "BLOCKED"
    elif required_missing:
        overall = "NEEDS_ACTION"
    else:
        overall = "READY"

    next_steps = []
    for item in materials:
        if item["requirement"] in ("REQUIRED", "UNKNOWN") and not item["satisfied"]:
            next_steps.append({"material": item["id"], "owner": item["owner"], "action": item["next_action"]})
    if problems:
        next_steps.insert(0, {"material": "release-spec", "owner": "vendor", "action": "修复 release spec 后重新 plan"})

    return {
        "schema": SCHEMA,
        "scenario": scenario,
        "build_required": build_required,
        "components": args.component or [],
        "applications": apps,
        "registration_protocols": protocols,
        "materials": materials,
        "problems": problems,
        "overall_status": overall,
        "next_steps": next_steps,
    }


def app_needs_entitlement(scenario, args, apps):
    if scenario != "add":
        return False
    return bool(apps)


def human(document):
    print("场景：%s" % document["scenario"])
    print("结论：%s" % document["overall_status"])
    print("应用：%s" % (", ".join(document["applications"]) or "无"))
    print("构建：%s" % ("是" if document["build_required"] else "否"))
    for item in document["materials"]:
        mark = "PASS" if item["satisfied"] else "MISSING"
        if item["requirement"] in ("NOT_APPLICABLE",):
            mark = "N/A"
        print("[%s] %-28s %-13s %s" % (mark, item["id"], item["requirement"], item["title"]))
    for item in document["problems"]:
        print("[BLOCK] " + item)
    if document["next_steps"]:
        print("下一步：")
        for step in document["next_steps"]:
            print("  - %s [%s] %s" % (step["material"], step["owner"], step["action"]))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=SCENARIOS)
    parser.add_argument("--v2-only", action="store_true")
    parser.add_argument("--spec")
    parser.add_argument("--component", action="append", default=[])
    parser.add_argument("--license")
    parser.add_argument("--approval-dir")
    parser.add_argument("--old-install-evidence")
    parser.add_argument("--entitlement")
    parser.add_argument("--frozen-dir")
    parser.add_argument("--release-manifest")
    parser.add_argument("--backup-evidence")
    parser.add_argument("--compatibility")
    parser.add_argument("--build-evidence")
    parser.add_argument("--source-freeze", action="store_true")
    parser.add_argument("--format", choices=("human", "json"), default="human")
    parser.add_argument("--output")
    args = parser.parse_args(argv)
    if args.v2_only:
        spec, error = load_spec(args.spec)
        apps, protocols, problems = selected_apps(spec, args.component)
        if error:
            problems.append(error)
        if not apps:
            problems.append("没有选中应用")
        if any(protocol != 2 for protocol in protocols.values()):
            problems.append("存在非 V2 应用")
        document = {"schema": "uip.release-protocol-gate/v1", "applications": apps,
                    "registration_protocols": protocols, "v2_only": not problems,
                    "problems": problems}
        if args.format == "json":
            print(json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
        else:
            print("V2_ONLY" if not problems else "NOT_V2_ONLY")
            for item in problems:
                print("[BLOCK] " + item)
        return 0 if not problems else 2
    if not args.scenario:
        parser.error("--scenario is required unless --v2-only is used")
    try:
        document = decide(args)
    except Invalid as error:
        print(str(error), file=sys.stderr)
        return 64
    if args.output:
        target = Path(args.output)
        if not target.is_absolute():
            print("材料计划输出必须是绝对路径", file=sys.stderr)
            return 64
        if target.parent.exists() and target.parent.is_symlink():
            print("材料计划输出父目录是符号链接", file=sys.stderr)
            return 2
        if target.exists() or target.is_symlink():
            print("材料计划输出已存在，拒绝覆盖", file=sys.stderr)
            return 2
        target.parent.mkdir(parents=True, exist_ok=True)
        temporary = target.with_name("." + target.name + ".tmp")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(document, stream, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
                stream.write("\n")
            os.replace(temporary, target)
        except Exception:
            try:
                temporary.unlink()
            except OSError:
                pass
            raise
    if args.format == "json":
        print(json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    else:
        human(document)
    return {"READY": 0, "NEEDS_ACTION": 3, "BLOCKED": 2}[document["overall_status"]]


if __name__ == "__main__":
    raise SystemExit(main())
