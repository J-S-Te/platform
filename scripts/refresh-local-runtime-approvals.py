#!/usr/bin/env python3
"""为已登记应用批量生成本地授权运行批准候选，并在操作员确认后统一发布。

设计约束（与单应用工具一致，不放松任何门禁）：
  * 候选生成本身只读：不检查旧运行镜像、不执行迁移、不启动服务、不改写运行固定记录。
  * 不自动批准：必须显式 --approve，且核对单会在批准前打印。
  * 失败关闭：任一应用的镜缺失或指纹不一致时，整体不发布批准。
  * 逐应用仍走 prepare-local-runtime-license.py，保持单一实现来源。
"""
from pathlib import Path
import argparse
import json
import subprocess
import sys

SCRIPT_DIR = Path(__file__).absolute().parent
SINGLE = SCRIPT_DIR / "prepare-local-runtime-license.py"
DOCKER = "docker"


def fail(message):
    print("[refresh-approvals] ERROR: " + message, file=sys.stderr)
    raise SystemExit(1)


def digest_file(path):
    import hashlib
    return "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()


def registered_applications(workspace):
    """以组件清单为准：只要清单里登记了该应用，就必须有运行批准。

    只看已有批准文件会漏掉"从未批准过"的应用（例如客户门户），
    直到接入时才以 LICENSE_RUNTIME_APPROVAL_UNAVAILABLE 暴露出来。
    """
    manifest = workspace / "platform/config/local-license-components.json"
    if manifest.is_file():
        try:
            document = json.loads(manifest.read_text(encoding="utf-8"))
            applications = document.get("applications", document)
            if isinstance(applications, dict) and applications:
                return sorted(applications)
        except (ValueError, OSError):
            pass
    # 清单不可读时退回已有批准文件，至少不放大问题。
    directory = workspace / "platform/runtime-approvals"
    applications = []
    for path in sorted(directory.glob("local-*-dev.json")):
        applications.append(path.name[len("local-"):-len("-dev.json")])
    return applications


def run_prepare(workspace, application, operator, approve, replace):
    command = [sys.executable, str(SINGLE), "--workspace", str(workspace),
               "--application", application, "--operator", operator]
    if approve:
        command.append("--approve")
    if replace:
        command.append("--replace")
    return subprocess.run(command, text=True, capture_output=True)


def image_present(digest):
    return subprocess.run([DOCKER, "image", "inspect", digest],
                          capture_output=True).returncode == 0


def candidate_path(workspace, application):
    return workspace / ("platform/runtime-approvals/local-" + application + "-dev.candidate.json")


def inspect_candidate(workspace, application):
    """核对候选：编排指纹是否与当前一致、全部固定镜像是否存在。"""
    path = candidate_path(workspace, application)
    if not path.is_file():
        return {"ok": False, "problems": ["候选文件未生成"], "services": [], "source": "-", "compose": "-"}
    document = json.loads(path.read_text(encoding="utf-8"))
    problems = []
    current_compose = digest_file(workspace / "platform/compose.local.yaml")
    if document.get("compose_sha256") != current_compose:
        problems.append("编排指纹与当前 compose.local.yaml 不一致")
    services = []
    entries = [(item.get("name"), item.get("image_digest")) for item in document.get("services") or []]
    migration = document.get("migration") or {}
    entries.append((migration.get("service"), migration.get("image_digest")))
    for name, digest in entries:
        if not isinstance(name, str) or not isinstance(digest, str):
            problems.append("候选条目格式异常")
            continue
        present = image_present(digest)
        services.append((name, digest, present))
        if not present:
            problems.append(name + " 固定镜像缺失 " + digest)
    return {"ok": not problems, "problems": problems, "services": services,
            "source": document.get("source_digest", "-"), "compose": document.get("compose_sha256", "-")}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--workspace", type=Path, default=Path(__file__).absolute().parents[2])
    parser.add_argument("--operator", required=True)
    parser.add_argument("--applications", default="", help="逗号分隔；默认取全部已登记应用")
    parser.add_argument("--approve", action="store_true", help="核对后统一发布批准（内容不同自动加 --replace）")
    args = parser.parse_args(argv)
    workspace = args.workspace.absolute()
    if args.applications.strip():
        applications = [item.strip() for item in args.applications.split(",") if item.strip()]
    else:
        applications = registered_applications(workspace)
    if not applications:
        fail("没有已登记应用；先用 prepare-license 为单个应用生成首份批准")
    print("[refresh-approvals] 应用：" + ", ".join(applications))
    results = {}
    failed = False
    for application in applications:
        result = run_prepare(workspace, application, args.operator, approve=False, replace=False)
        if result.returncode != 0:
            print("[refresh-approvals] " + application + " 候选生成失败：")
            print((result.stdout + result.stderr).rstrip())
            failed = True
            continue
        results[application] = inspect_candidate(workspace, application)
    print()
    print("[refresh-approvals] 核对单（批准前请逐项确认）：")
    for application, report in results.items():
        state = "可以发布" if report["ok"] else "不可发布"
        print("  " + application + "  [" + state + "]")
        print("    源码指纹 " + str(report["source"]))
        print("    编排指纹 " + str(report["compose"]))
        for name, digest, present in report["services"]:
            print("    " + ("OK  " if present else "缺失") + " " + name.ljust(38) + " " + digest)
        for problem in report["problems"]:
            print("    问题：" + problem)
    blocked = [application for application, report in results.items() if not report["ok"]]
    if failed or blocked:
        print()
        fail("存在未通过核对的候选，拒绝发布批准：" + ", ".join(sorted(set(blocked))))
    if not args.approve:
        print()
        print("[refresh-approvals] 候选已生成，尚未发布批准。核对无误后执行：")
        print("  bash scripts/docker-local.sh refresh-approvals --operator " + args.operator + " --approve")
        return 0
    print()
    for application in results:
        result = run_prepare(workspace, application, args.operator, approve=True, replace=True)
        if result.returncode != 0:
            print(result.stdout.rstrip())
            fail("发布批准失败：" + application)
        print("[refresh-approvals] 已发布 " + application)
    print()
    print("[refresh-approvals] 全部批准已发布。下一步：在应用接入页或 scripts/subsystem.sh retry 走受控更新。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
