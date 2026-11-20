#!/usr/bin/env python3
"""把已接入子系统的公开认证配置收敛到已提交的公开入口。"""

import argparse
import os
import re
import shutil
import sys
import tempfile
from pathlib import Path
from urllib.parse import urlsplit

RUNTIME_FILES = {
    "CONTRACT_RUNTIME_ENV_FILE": "contract.env",
    "CUSTOMER_RUNTIME_ENV_FILE": "customer.env",
    "DATA_ANALYSIS_RUNTIME_ENV_FILE": "data-analysis.env",
    "PORTAL_RUNTIME_ENV_FILE": "portal.env",
    "PROJECT_RUNTIME_ENV_FILE": "project.env",
    "REIMBURSEMENT_RUNTIME_ENV_FILE": "reimbursement.env",
    "SETTLEMENT_RUNTIME_ENV_FILE": "settlement.env",
}
URL_KEYS = frozenset("""
APP_PUBLIC_ORIGIN APP_PUBLIC_URL CENTRAL_SESSION_PUBLIC_BASE_URL COOKIE_PUBLIC_URL
OIDC_ISSUER OIDC_POST_LOGOUT_REDIRECT_URI OIDC_REDIRECT_URI
PORTAL_ACCOUNT_SECURITY_CENTER_URL PORTAL_CENTRAL_SESSION_PUBLIC_BASE_URL
PORTAL_OIDC_ISSUER PORTAL_OIDC_REDIRECT_URI PORTAL_PUBLIC_ORIGIN PORTAL_PUBLIC_URL
PUBLIC_BASE_URL SETTLEMENT_PUBLIC_ORIGIN
""".split())
COOKIE_ORIGIN_KEYS = frozenset("""
APP_PUBLIC_ORIGIN APP_PUBLIC_URL OIDC_REDIRECT_URI PORTAL_OIDC_REDIRECT_URI
PORTAL_PUBLIC_ORIGIN PUBLIC_BASE_URL SETTLEMENT_PUBLIC_ORIGIN
""".split())
SECURE_KEYS = frozenset("""
COOKIE_SECURE OIDC_SESSION_COOKIE_SECURE PORTAL_OIDC_COOKIE_SECURE
PORTAL_SESSION_COOKIE_SECURE
""".split())
INSECURE_KEYS = frozenset("""
OIDC_ALLOW_INSECURE_HTTP_SESSION PORTAL_ALLOW_INSECURE_HTTP_SESSION
SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN
""".split())
SETTING = re.compile(r"^([A-Z][A-Z0-9_]*=)(.*?)(\r?\n)?$")


def origin(value):
    """用途：校验受管入口是否为绝对 HTTP 或 HTTPS Origin。
    参数：value 为公开入口字符串。
    返回值：规范化 Origin，格式错误时终止解析。
    """
    parsed = urlsplit(value)
    if (
        parsed.scheme not in ("http", "https") or not parsed.hostname
        or parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment
    ):
        raise argparse.ArgumentTypeError("expected an HTTP or HTTPS origin without a path")
    return value


def update_file(text, old_platform, old_sso, platform, sso):
    """用途：生成一个运行文件的公开 TLS 设置，保留其他原始内容。
    参数：text 为现有配置，剩余参数为旧的和目标的平台、SSO Origin。
    返回值：变更后的文件和变更键，重复受管键时报错。
    """
    output = []
    changed = []
    seen = set()
    secure = platform.startswith("https://")
    cookie_origins = {old_platform, platform}
    managed_cookie = False
    for line in text.splitlines():
        key, separator, raw = line.partition("=")
        if separator and key in COOKIE_ORIGIN_KEYS:
            value = raw.strip("'\"")
            if any(value == root or value.startswith(root + "/") for root in cookie_origins):
                managed_cookie = True
                break
    for line in text.splitlines(keepends=True):
        match = SETTING.match(line)
        if not match:
            output.append(line)
            continue
        prefix, raw, ending = match.groups()
        key = prefix[:-1]
        if key not in URL_KEYS | SECURE_KEYS | INSECURE_KEYS:
            output.append(line)
            continue
        if key in seen:
            raise ValueError(f"duplicate public transport key: {key}")
        seen.add(key)
        quote = raw[0] if len(raw) >= 2 and raw[0] in "'\"" and raw[-1] == raw[0] else ""
        value = raw[1:-1] if quote else raw
        target = value
        if managed_cookie and key in SECURE_KEYS and value in ("true", "false"):
            target = "true" if secure else "false"
        elif managed_cookie and key in INSECURE_KEYS and value in ("true", "false"):
            target = "false" if secure else "true"
        elif key == "PORTAL_ACCOUNT_SECURITY_CENTER_URL" and value.startswith(old_sso + "/realms/") and value.endswith("/settings/security"):
            target = platform + "/settings/security"
        elif key in URL_KEYS:
            for previous, replacement in ((old_platform, platform), (old_sso, sso)):
                if value == previous or value.startswith((previous + "/", previous + "?")):
                    target = replacement + value[len(previous):]
                    break
        if value != target:
            changed.append(key)
            raw = f"{quote}{target}{quote}"
        output.append(f"{prefix}{raw}{ending or ''}")
    return "".join(output), changed


def runtime_paths(deploy_root, bindings):
    """用途：解析 Compose 已采用的运行配置绑定，合并重复路径。
    参数：deploy_root 为部署目录，bindings 为 Compose 的运行配置路径声明。
    返回值：各已知子系统的配置路径列表。
    """
    paths = (
        deploy_root / (bindings.get(name) or f"runtime/{filename}")
        for name, filename in RUNTIME_FILES.items()
    )
    return list(dict.fromkeys(paths))


def main():
    """用途：校验、备份并写入切换涉及的现有子系统配置。
    参数：无，部署目录和四个 Origin 由命令行提供。
    返回值：无，非法配置、备份或写入失败时退出非零。
    """
    parser = argparse.ArgumentParser()
    parser.add_argument("deploy_root", type=Path)
    for arg in ("old_platform", "old_sso", "platform", "sso"):
        parser.add_argument(arg, type=origin)
    args = parser.parse_args()
    if urlsplit(args.platform).scheme != urlsplit(args.sso).scheme:
        parser.error("platform and SSO must use the same transport mode")
    bindings = {}
    for line in sys.stdin:
        key, separator, value = line.rstrip("\n").partition("=")
        if separator and key in RUNTIME_FILES:
            bindings[key] = value
    runtime = args.deploy_root / "runtime"
    if runtime.is_symlink():
        parser.error("runtime must be a real directory")
    plan = []
    for path in runtime_paths(args.deploy_root, bindings):
        if path.is_symlink():
            parser.error(f"refusing a symlink for {path.name}")
        if not path.exists():
            continue
        raw = path.read_bytes().decode("utf-8")
        result, keys = update_file(raw, args.old_platform, args.old_sso, args.platform, args.sso)
        if keys:
            plan.append((path, result, keys))
    if not plan:
        return
    backup = Path(tempfile.mkdtemp(prefix="public-transport-runtime-backup.", dir=runtime))
    for index, (path, _, _) in enumerate(plan):
        shutil.copy2(path, backup / f"{index}-{path.name}")
    candidates = []
    try:
        for path, value, _ in plan:
            with tempfile.NamedTemporaryFile(prefix=".public-transport.", dir=path.parent, delete=False) as file:
                candidate = Path(file.name)
                candidates.append(candidate)
                file.write(value.encode("utf-8"))
            shutil.copymode(path, candidate)
        for candidate, (path, _, keys) in zip(candidates, plan):
            os.replace(candidate, path)
            print(f"{path.name}: {', '.join(keys)}")
    finally:
        for candidate in candidates:
            candidate.unlink(missing_ok=True)
    print(f"Backup: {backup}")


if __name__ == "__main__":
    main()
