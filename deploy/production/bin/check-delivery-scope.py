#!/usr/bin/env python3
"""Fail closed when the packaged delivery scope disagrees with its images/spec.

The check is intentionally read-only and does not sign or install anything.
It exists because deployment-assets is one archive while image packages are
selected per component; a stale asset archive can otherwise declare a portal
or another subsystem whose image was never built.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import sys
import tarfile

COMPONENT_APP = {
    "customer-opportunity": "customer_and_opportunity",
    "customer-portal": "customer_portal",
    "contract": "contract_management",
    "project": "project_management",
    "settlement": "settlement",
    "data-analysis": "data_analysis",
}
BASE_COMPONENTS = {"common", "platform", "frontend"}
PORTAL_TEMPLATES = {
    "subsystem-templates/portal.env.example",
    "subsystem-templates/portal-report-worker.env.example",
}
ACTIVE_PORTAL_SERVICE = re.compile(r"(?m)^  portal-[a-z0-9-]+:")
ACTIVE_PORTAL_VOLUME = re.compile(r"(?m)^  portal-(?:mysql|reports)-data:")


class ScopeError(ValueError):
    pass


def read_json(path: Path) -> dict:
    if path.is_symlink() or not path.is_file():
        raise ScopeError(f"缺失构建证据：{path.name}")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, ValueError) as exc:
        raise ScopeError(f"构建证据不是合法 JSON：{path.name}") from exc
    if not isinstance(value, dict):
        raise ScopeError("构建证据顶层不是对象")
    return value


def image_components(media: Path) -> set[str]:
    evidence = read_json(media / "build-evidence.json")
    packages = evidence.get("packages")
    if not isinstance(packages, list):
        raise ScopeError("build-evidence.json 缺少 packages")
    result = set()
    for package in packages:
        if not isinstance(package, dict) or not isinstance(package.get("component"), str):
            raise ScopeError("build-evidence.json 包含非法 package")
        result.add(package["component"])
    return result


def selected_apps(selected: str) -> set[str]:
    components = {value for value in re.split(r"[\s,]+", selected.strip()) if value}
    apps = set()
    for component in components:
        if component in COMPONENT_APP:
            apps.add(COMPONENT_APP[component])
        elif component not in BASE_COMPONENTS:
            raise ScopeError(f"未知的 selected component：{component}")
    return apps


def spec_apps(media: Path, spec_path: str) -> set[str]:
    if not spec_path:
        return set()
    spec = read_json(Path(spec_path))
    applications = spec.get("applications")
    if not isinstance(applications, list):
        raise ScopeError("release spec 缺少 applications")
    result = set()
    for application in applications:
        if isinstance(application, dict):
            code = application.get("code")
        else:
            code = application
        if not isinstance(code, str) or not code:
            raise ScopeError("release spec 包含非法 application")
        result.add(code)
    return result


def asset_compose(media: Path) -> tuple[set[str], str]:
    matches = sorted(media.glob("deployment-assets-*.tar.gz"))
    if len(matches) != 1:
        raise ScopeError("交付目录必须恰好包含一个 deployment-assets 包")
    names: set[str] = set()
    compose = ""
    with tarfile.open(matches[0], "r:gz") as archive:
        for member in archive:
            name = member.name.removeprefix("./")
            if not member.isfile():
                continue
            names.add(name)
            if name == "docker-compose.yml":
                stream = archive.extractfile(member)
                if stream is None:
                    raise ScopeError("无法读取 docker-compose.yml")
                compose = stream.read(2 * 1024 * 1024).decode("utf-8", errors="strict")
    if not compose:
        raise ScopeError("deployment-assets 缺少 docker-compose.yml")
    return names, compose


def check(media: Path, selected: str, spec_path: str = "") -> None:
    components = image_components(media)
    business_images = {COMPONENT_APP[item] for item in components if item in COMPONENT_APP}
    selected_scope = selected_apps(selected)
    declared = spec_apps(media, spec_path)
    if declared and declared != business_images:
        raise ScopeError(
            "release spec applications 与实际镜像包不一致："
            f"spec={sorted(declared)} images={sorted(business_images)}"
        )
    if not declared and selected_scope != business_images:
        raise ScopeError(
            "selected components 与实际镜像包不一致："
            f"selected={sorted(selected_scope)} images={sorted(business_images)}"
        )
    names, compose = asset_compose(media)
    portal_image = "customer-portal" in components
    portal_assets = bool(PORTAL_TEMPLATES & names)
    portal_compose = bool(ACTIVE_PORTAL_SERVICE.search(compose) or ACTIVE_PORTAL_VOLUME.search(compose))
    if portal_image:
        if not portal_assets:
            raise ScopeError("已选择 customer-portal，但 deployment-assets 缺少 portal 环境模板")
        if not portal_compose:
            raise ScopeError("已选择 customer-portal，但 docker-compose.yml 缺少 portal 服务或卷")
    else:
        if portal_assets:
            raise ScopeError("未选择 customer-portal，但 deployment-assets 仍包含 portal 环境模板")
        if portal_compose:
            raise ScopeError("未选择 customer-portal，但 docker-compose.yml 仍含 portal 服务/卷")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--media", required=True)
    parser.add_argument("--selected-components", default="")
    parser.add_argument("--spec", default="")
    args = parser.parse_args()
    try:
        check(Path(args.media).resolve(), args.selected_components, args.spec)
    except (OSError, UnicodeError, ValueError, tarfile.TarError) as exc:
        print(f"delivery scope check failed: {exc}", file=sys.stderr)
        return 1
    print("delivery scope check passed: spec/images/assets are consistent")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
