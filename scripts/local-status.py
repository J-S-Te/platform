#!/usr/bin/env python3
"""本地启动状态总览：一眼看清卡在哪、下一步做什么。

只读：不构建、不启动、不迁移、不改写固定记录或批准，也不写任何文件。
"""
from pathlib import Path
import json
import re
import subprocess
import sys

WORKSPACE = Path(__file__).absolute().parents[2]
PLATFORM = WORKSPACE / "platform"
PROJECT = "basic-platform-local"
PIN = PLATFORM / "runtime" / "local-license.compose.json"
APPROVALS = PLATFORM / "runtime-approvals"
ENV_FILE = PLATFORM / "docker" / ".env.local"


def run(command):
    return subprocess.run(command, capture_output=True, text=True)


def docker(args):
    return run(["docker"] + args)


def image_present(digest):
    return docker(["image", "inspect", digest]).returncode == 0


def compose_env(name, default=""):
    if not ENV_FILE.is_file():
        return default
    pattern = re.compile("^" + re.escape(name) + "=(.*)$", re.M)
    match = pattern.search(ENV_FILE.read_text(encoding="utf-8"))
    return match.group(1).strip() if match else default


def query(sql):
    user = compose_env("MYSQL_USER", "basic_platform")
    password = compose_env("MYSQL_PASSWORD")
    database = compose_env("MYSQL_DATABASE", "basic_platform")
    if not password:
        return None
    result = run(["docker", "exec", "basic-platform-local-mysql-1", "mysql",
                  "-u" + user, "-p" + password, database, "-N", "-e", sql])
    if result.returncode != 0:
        return None
    return result.stdout.strip()


def scalar(sql):
    value = query(sql)
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def load_pins():
    if not PIN.is_file():
        return None
    try:
        return json.loads(PIN.read_text(encoding="utf-8")).get("services", {})
    except (ValueError, OSError):
        return None


def main():
    steps = []
    blockers = []

    if docker(["info"]).returncode != 0:
        print("[status] ❌ Docker 不可用")
        print("         下一步：确认 OrbStack 已启动，并执行 docker context use orbstack")
        return 1
    context = docker(["context", "show"]).stdout.strip()
    steps.append(("Docker 可用", context or "unknown", True))

    running = docker(["ps", "--filter", "label=com.docker.compose.project=" + PROJECT,
                      "--format", "{{.Names}}"]).stdout.split()
    steps.append(("控制面运行容器", str(len(running)) + " 个", len(running) > 0))

    pending_pin_missing = []
    pins = load_pins()
    if pins is None:
        steps.append(("授权固定记录", "缺失或不可读", False))
        blockers.append("固定记录缺失：执行 build-images 后由受控更新写入")
    else:
        missing = [name for name, item in pins.items() if not image_present(item.get("image", ""))]
        steps.append(("授权固定记录", "%d 个服务，%d 个镜像失效" % (len(pins), len(missing)), not missing))
        pending_pin_missing = missing

    approvals = sorted(APPROVALS.glob("local-*-dev.json"))
    stale = []
    for path in approvals:
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except (ValueError, OSError):
            continue
        digests = [item.get("image_digest", "") for item in document.get("services", [])]
        migration = (document.get("migration") or {}).get("image_digest", "")
        if migration:
            digests.append(migration)
        if any(d and not image_present(d) for d in digests):
            stale.append(path.name[len("local-"):-len("-dev.json")])
    steps.append(("已发布运行批准", "%d 个" % len(approvals), bool(approvals)))
    if stale:
        blockers.append("这些批准指向已回收镜像，需重新生成并发布：%s" % ", ".join(stale))
    elif pending_pin_missing:
        # 批准有效但固定记录仍指向旧镜像：固定记录由部署 Agent 在受控更新时改写，
        # 重复发布批准解决不了，必须走受控更新。
        shown = "、".join(pending_pin_missing[:8])
        more = "" if len(pending_pin_missing) <= 8 else " 等 %d 个" % len(pending_pin_missing)
        blockers.append(
            "固定记录待受控更新刷新（批准已有效，无需重新发布）：%d 个服务的运行镜像已在本机回收（%s%s）。"
            "下一步：在「基础平台 → 应用接入」对该应用执行受控更新，或运行 "
            "bash scripts/subsystem.sh retry --application-code <应用编码>；完成后执行 bash scripts/docker-local.sh up。"
            % (len(pending_pin_missing), shown, more)
        )

    candidates = []
    for container in docker(["ps", "-aq"]).stdout.split():
        found = docker(["inspect", "--format",
                        '{{index .Config.Labels "com.basic-platform.discovery"}}', container])
        label = found.stdout.strip()
        if label and label != "<no value>":
            app = docker(["inspect", "--format",
                          '{{index .Config.Labels "com.basic-platform.application_code"}}', container]).stdout.strip()
            candidates.append(app)
    steps.append(("子系统探测候选", "%d 个" % len(candidates), bool(candidates)))
    if not candidates:
        blockers.append("探测候选为空：执行 prepare-discovery 或 up --core-only")

    instances = scalar("SELECT COUNT(*) FROM license_deployment;")
    artifacts = scalar("SELECT COUNT(*) FROM license_artifact;")
    accounts = scalar("SELECT COUNT(*) FROM iam_account;")
    if instances is None:
        steps.append(("平台数据库", "不可查询（控制面未就绪）", False))
    else:
        steps.append(("授权实例", "%d 个实例 / %d 个产物" % (instances, artifacts), instances > 0))
        steps.append(("平台账号", "%d 个" % (accounts or 0), (accounts or 0) > 0))
        if instances == 0:
            blockers.append("授权实例未初始化：在「商业授权」初始化并导入签发产物（否则接入必然失败，固定记录也不会刷新）")
        if not accounts:
            blockers.append("没有平台账号：执行 bootstrap-admin 创建首个超级管理员")

    print("[status] 本地启动状态")
    for name, value, ok in steps:
        print("  %s %-14s %s" % ("✅" if ok else "⬜", name, value))
    print()
    if blockers:
        print("[status] 下一步（按顺序）：")
        for index, item in enumerate(blockers, 1):
            print("  %d. %s" % (index, item))
        return 1
    print("[status] 控制面就绪。可继续：界面接入子系统，然后执行普通 up 启动全栈。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
