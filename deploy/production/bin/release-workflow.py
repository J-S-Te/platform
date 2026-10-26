#!/usr/bin/env python3
"""Scenario facade for the root offline-package builder.

The existing root script remains the only implementation of build/package/
finalize. This facade adds read-only planning and status, and preserves old
flags by forwarding build/resume to the legacy path.
"""

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve()
DEPLOY = HERE.parent.parent
PLATFORM = DEPLOY.parent.parent
ROOT = PLATFORM.parent / "一键制作离线镜像包.sh"
MATERIALS = HERE.with_name("release-materials.py")
FINALIZE = HERE.with_name("finalize-release-integrity.sh")


def csv(values):
    result = []
    for value in values or []:
        if not value:
            continue
        result.extend(item.strip() for item in value.replace(",", " ").split() if item.strip())
    return result


def plan(args):
    scenario = args.scenario or args.installation_scenario or "fresh"
    command = [sys.executable or "python3", str(MATERIALS),
               "--scenario", scenario, "--format", args.format]
    if args.spec:
        command += ["--spec", args.spec]
    for component in csv(args.components):
        command += ["--component", component]
    mapping = {
        "license_file": "license",
        "runtime_approval_dir": "approval-dir",
        "old_install_evidence": "old-install-evidence",
        "entitlement": "entitlement",
        "frozen_dir": "frozen-dir",
        "release_manifest": "release-manifest",
        "backup_evidence": "backup-evidence",
        "compatibility": "compatibility",
        "build_evidence": "build-evidence",
    }
    for attr, flag in mapping.items():
        value = getattr(args, attr, None)
        if value:
            command += ["--" + flag, value]
    if args.source_freeze:
        command.append("--source-freeze")
    if args.write_plan:
        command += ["--output", args.write_plan]
    return subprocess.run(command).returncode


def file_digest(path):
    import hashlib
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def output_lock(function):
    import fcntl
    import functools

    @functools.wraps(function)
    def wrapped(args):
        output = Path(args.output).resolve()
        lock_path = output.parent / (".release-workflow-" + output.name + ".lock")
        descriptor = os.open(lock_path, os.O_WRONLY | os.O_CREAT, 0o600)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(descriptor)
            print("同一输出目录已有审核/打包任务运行", file=sys.stderr)
            return 2
        try:
            return function(args)
        finally:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
            os.close(descriptor)
    return wrapped


def read_state(output):
    path = output / "release-state.json"
    if not path.is_file() or path.is_symlink():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {"stage": "INVALID"}


def write_state(output, stage, **extra):
    path = output / "release-state.json"
    value = {"schema": "uip.release-state/v1", "stage": stage, **extra}
    temporary = path.with_name("." + path.name + ".tmp")
    if temporary.exists() or temporary.is_symlink():
        raise OSError("temporary state exists")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, sort_keys=True)
        stream.write("\n")
    os.replace(temporary, path)


def read_plan(output):
    path = output / "release-plan.json"
    if not path.is_file() or path.is_symlink():
        return None
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {"overall_status": "INVALID", "problems": ["release-plan.json 不可解析"]}
    if not isinstance(value, dict) or value.get("schema") != "uip.release-materials/v1":
        return {"overall_status": "INVALID"}
    return value


def verify_manifest(output):
    manifest = output / "SHA256SUMS"
    if not manifest.is_file() or manifest.is_symlink():
        return "缺少安全普通文件 SHA256SUMS"
    try:
        lines = manifest.read_text(encoding="utf-8").splitlines()
    except OSError as error:
        return "无法读取 SHA256SUMS：%s" % error
    if not lines:
        return "SHA256SUMS 为空"
    seen = set()
    for line in lines:
        parts = line.split("  ", 1)
        if len(parts) != 2 or not re.fullmatch(r"[0-9a-fA-F]{64}", parts[0]):
            return "SHA256SUMS 行格式无效"
        digest, name = parts
        if not name or "/" in name or name in (".", ".."):
            return "SHA256SUMS 包含不安全路径"
        if name in seen:
            return "SHA256SUMS 包含重复文件：%s" % name
        seen.add(name)
        path = output / name
        if not path.is_file() or path.is_symlink():
            return "SHA256SUMS 引用的文件缺失或不安全：%s" % name
        actual = file_digest(path)
        if actual.lower() != digest.lower():
            return "SHA256SUMS 摘要不一致：%s" % name
    return ""



def signature_error(output, args):
    """Use the independently installed verifier, never one supplied by the media."""
    verifier_arg = getattr(args, "verifier", None)
    trust_arg = getattr(args, "trust_dir", None)
    if not verifier_arg or not trust_arg:
        return "发布验签需要 --verifier 和 --trust-dir 绝对路径"
    verifier, trust = Path(verifier_arg), Path(trust_arg)
    if not verifier.is_absolute() or not trust.is_absolute():
        return "verifier 和 trust-dir 必须是绝对路径"
    if verifier.is_symlink() or not verifier.is_file() or not os.access(verifier, os.X_OK):
        return "独立 verifier 不存在、不安全或不可执行"
    if trust.is_symlink() or not trust.is_dir():
        return "独立信任目录不存在或不安全"
    for path in (verifier.resolve(), trust.resolve()):
        if path == output or output in path.parents:
            return "verifier 和信任目录不能来自待验证介质"
    try:
        completed = subprocess.run([str(verifier), "verify", "--media", str(output),
                                    "--trust-dir", str(trust), "--output", "json"],
                                   text=True, capture_output=True, timeout=300)
        result = json.loads(completed.stdout)
    except (OSError, ValueError, subprocess.TimeoutExpired):
        return "发布验签执行失败或返回无效结果"
    if completed.returncode != 0 or not isinstance(result, dict) or result.get("status") != "VERIFIED":
        return "发布验签未通过，拒绝打包"
    return ""


@output_lock
def package(args):
    output = Path(args.output).resolve()
    if not output.is_dir() or output.is_symlink():
        print("输出目录不存在或不安全：%s" % output, file=sys.stderr)
        return 2
    if not args.allow_legacy_unsigned:
        if not (output / "release-manifest.json").is_file():
            print("冻结目录缺少 release-manifest.json；旧介质请显式使用 --allow-legacy-unsigned", file=sys.stderr)
            return 2
        if not (output / "release-manifest.sigstore.json").is_file():
            print("冻结目录缺少发布签名；拒绝打包未签名发行", file=sys.stderr)
            return 2
    state = read_state(output)
    if state:
        if state.get("schema") != "uip.release-state/v1" or state.get("stage") not in ("reviewed", "packaged"):
            print("release-state.json 状态无效，拒绝打包", file=sys.stderr)
            return 2
        if state.get("stage") == "reviewed" and state.get("candidate_sha256") != candidate_digest(output):
            print("release-state.json 候选摘要不匹配，拒绝打包", file=sys.stderr)
            return 2
    frozen_plan = read_plan(output)
    if frozen_plan:
        if frozen_plan.get("schema") != "uip.release-materials/v1":
            print("冻结材料计划 schema 无效，拒绝打包", file=sys.stderr)
            return 2
        if frozen_plan.get("overall_status") not in ("READY", "NEEDS_ACTION"):
            print("冻结材料计划状态 %s，拒绝打包" % frozen_plan.get("overall_status"), file=sys.stderr)
            return 2
    formal_error = formal_review_error(output)
    if formal_error:
        print(formal_error + "，拒绝打包", file=sys.stderr)
        return 2
    review_path = output / "approval-review.json"
    if review_path.exists() or review_path.is_symlink():
        if not review_path.is_file() or review_path.is_symlink():
            print("approval-review.json 不安全，拒绝打包", file=sys.stderr)
            return 2
        try:
            review = json.loads(review_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            print("approval-review.json 不可解析，拒绝打包", file=sys.stderr)
            return 2
        if not isinstance(review, dict) or review.get("candidate") is not False or review.get("input_reviewed") is not True:
            print("approval-review.json 不是已审核正式记录，拒绝打包", file=sys.stderr)
            return 2
    review_error = validate_review(output)
    if review_error:
        print(review_error + "，拒绝打包", file=sys.stderr)
        return 2
    error = verify_manifest(output)
    if error:
        print(error, file=sys.stderr)
        return 2
    version = args.version
    if not version:
        build_info = output / "BUILD_INFO.txt"
        if build_info.is_file() and not build_info.is_symlink():
            for line in build_info.read_text(encoding="utf-8", errors="replace").splitlines():
                if line.startswith("RELEASE_VERSION="):
                    version = line.split("=", 1)[1]
                    break
    if not version:
        print("无法确定版本号；请提供 --version", file=sys.stderr)
        return 64
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", version):
        print("版本号格式无效，拒绝打包", file=sys.stderr)
        return 2
    if not args.allow_legacy_unsigned:
        error = signature_error(output, args)
        if error:
            print(error, file=sys.stderr)
            return 2
    archive = output.parent / ("uip-offline-delivery-" + version + "-linux-amd64.tar")
    sidecar = archive.with_name(archive.name + ".sha256")
    if archive.exists() or archive.is_symlink() or sidecar.exists() or sidecar.is_symlink():
        print("总归档或 sidecar 已存在，拒绝覆盖：%s" % archive, file=sys.stderr)
        return 2
    temporary = archive.with_name("." + archive.name + ".tmp")
    if temporary.exists() or temporary.is_symlink():
        print("临时归档已存在，拒绝覆盖：%s" % temporary, file=sys.stderr)
        return 2
    env = dict(os.environ)
    env["COPYFILE_DISABLE"] = "1"
    try:
        subprocess.run(["tar", "--no-xattrs", "-cf", str(temporary), "-C", str(output.parent), output.name],
                       check=True, env=env)
        listing = subprocess.run(["tar", "-tf", str(temporary)], check=True, text=True, capture_output=True).stdout
        if any(part.endswith("/._") or "/._" in part or part.endswith("/.DS_Store") for part in listing.splitlines()):
            raise RuntimeError("归档包含 Apple 元数据")
        digest = file_digest(temporary)
        os.chmod(temporary, 0o600)
        os.replace(temporary, archive)
        sidecar.write_text(digest + "  " + archive.name + "\n", encoding="utf-8")
        os.chmod(sidecar, 0o644)
    except (OSError, subprocess.CalledProcessError, RuntimeError) as error:
        try:
            temporary.unlink()
        except OSError:
            pass
        print("打包失败：%s" % error, file=sys.stderr)
        return 2
    if state or (output / "release-review.json").is_file():
        write_state(output, "packaged", archive=archive.name, archive_sha256=file_digest(archive))
    print("总归档：%s" % archive)
    print("校验：%s" % sidecar)
    return 0


HEX64 = re.compile(r"^[0-9a-f]{64}$")


def formal_review_error(output):
    approval = output / "approval-review.json"
    if approval.exists() or approval.is_symlink():
        if not approval.is_file() or approval.is_symlink():
            return "approval-review.json 不安全"
        try:
            value = json.loads(approval.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return "approval-review.json 不可解析"
        if not isinstance(value, dict) or value.get("candidate") is not False or value.get("input_reviewed") is not True:
            return "approval-review.json 不是已审核正式记录"
        if not isinstance(value.get("operator"), str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}", value["operator"]):
            return "approval-review.json operator 无效"
    upgrade = output / "upgrade-review.json"
    if upgrade.exists() or upgrade.is_symlink():
        if not upgrade.is_file() or upgrade.is_symlink():
            return "upgrade-review.json 不安全"
        try:
            value = json.loads(upgrade.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return "upgrade-review.json 不可解析"
        required = {"version", "inventory_sha256", "mapping_sha256", "registry_sha256", "target_facts_sha256", "expected_sha256"}
        if not isinstance(value, dict) or set(value) != required or value.get("version") != 1:
            return "upgrade-review.json 字段无效"
        if any(not isinstance(value[key], str) or not HEX64.fullmatch(value[key]) for key in required - {"version"}):
            return "upgrade-review.json 摘要无效"
    return ""


def candidate_digest(output):
    entries = []
    for path in sorted(output.rglob("*")):
        if not path.is_file() or path.is_symlink():
            continue
        relative = path.relative_to(output).as_posix()
        if relative in ("release-review.json", "release-state.json", "release-manifest.sigstore.json", "SHA256SUMS"):
            continue
        entries.append((relative, file_digest(path)))
    import hashlib
    digest = hashlib.sha256()
    for name, value in entries:
        digest.update(name.encode("utf-8") + b"\0" + value.encode("ascii") + b"\n")
    return digest.hexdigest()


def directory_manifest(output):
    result = {}
    for path in sorted(output.rglob("*")):
        if path.is_file() and not path.is_symlink():
            result[path.relative_to(output).as_posix()] = file_digest(path)
    return result


def diff(args):
    before = Path(args.before).resolve()
    after = Path(args.after).resolve()
    if not before.is_dir() or not after.is_dir() or before.is_symlink() or after.is_symlink():
        print("diff 目录不存在或不安全", file=sys.stderr)
        return 2
    old = directory_manifest(before)
    new = directory_manifest(after)
    document = {
        "schema": "uip.release-diff/v1",
        "added": sorted(set(new) - set(old)),
        "removed": sorted(set(old) - set(new)),
        "changed": sorted(name for name in set(old) & set(new) if old[name] != new[name]),
    }
    if args.format == "json":
        print(json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    else:
        print("新增：%d" % len(document["added"]))
        for name in document["added"]:
            print("  + " + name)
        print("删除：%d" % len(document["removed"]))
        for name in document["removed"]:
            print("  - " + name)
        print("变化：%d" % len(document["changed"]))
        for name in document["changed"]:
            print("  ~ " + name)
    return 0


@output_lock
def review(args):
    output = Path(args.output).resolve()
    if not output.is_dir() or output.is_symlink():
        print("审核目录不存在或不安全", file=sys.stderr)
        return 2
    if not args.confirm:
        print("审核必须显式提供 --confirm", file=sys.stderr)
        return 64
    import re
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}", args.operator or ""):
        print("审核人标识无效", file=sys.stderr)
        return 64
    formal_error = formal_review_error(output)
    if formal_error:
        print(formal_error + "，拒绝形成审核记录", file=sys.stderr)
        return 2
    if args.purpose == "upgrade" and not (output / "upgrade-review.json").is_file():
        print("upgrade 审核必须提供 upgrade-review.json", file=sys.stderr)
        return 2
    plan = read_plan(output)
    if not plan or plan.get("overall_status") != "READY":
        print("材料计划不是 READY，拒绝形成正式审核记录", file=sys.stderr)
        return 2
    target = output / "release-review.json"
    if target.exists() or target.is_symlink():
        print("审核记录已存在，拒绝覆盖", file=sys.stderr)
        return 2
    before_digest = ""
    if getattr(args, "before", None):
        before = Path(args.before).resolve()
        if not before.is_dir() or before.is_symlink():
            print("before 审核基线不存在或不安全", file=sys.stderr)
            return 2
        before_digest = candidate_digest(before)
        old, new = directory_manifest(before), directory_manifest(output)
        added = sorted(set(new) - set(old))
        removed = sorted(set(old) - set(new))
        changed = sorted(name for name in set(old) & set(new) if old[name] != new[name])
        print("审核差异：新增 %d，删除 %d，变化 %d" % (len(added), len(removed), len(changed)))
        for name in added:
            print("  + " + name)
        for name in removed:
            print("  - " + name)
        for name in changed:
            print("  ~ " + name)
    from datetime import datetime, timezone
    value = {
        "schema": "uip.release-review/v2",
        "operator": args.operator,
        "created_at": datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z"),
        "purpose": args.purpose,
        "candidate_sha256": candidate_digest(output),
        "release_plan_sha256": file_digest(output / "release-plan.json"),
        "release_manifest_sha256": file_digest(output / "release-manifest.json") if (output / "release-manifest.json").is_file() else "",
        "before_candidate_sha256": before_digest,
        "approval_review_sha256": file_digest(output / "approval-review.json") if (output / "approval-review.json").is_file() else "",
        "upgrade_review_sha256": file_digest(output / "upgrade-review.json") if (output / "upgrade-review.json").is_file() else "",
        "target_evidence_sha256": file_digest(output / "license-evidence.json") if (output / "license-evidence.json").is_file() else "",
        "old_install_evidence_sha256": file_digest(output / "upgrade-before.json") if (output / "upgrade-before.json").is_file() else "",
    }
    descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, sort_keys=True, indent=2)
        stream.write("\n")
    write_state(output, "reviewed", candidate_sha256=value["candidate_sha256"], review_sha256=file_digest(target))
    print("审核记录：%s" % target)
    return 0


def validate_review(output):
    path = output / "release-review.json"
    if not path.exists() and not path.is_symlink():
        return ""
    if not path.is_file() or path.is_symlink():
        return "release-review.json 不安全"
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return "release-review.json 不可解析"
    if not isinstance(value, dict) or value.get("schema") not in ("uip.release-review/v1", "uip.release-review/v2"):
        return "release-review.json schema 无效"
    for name, key in (("approval-review.json", "approval_review_sha256"), ("upgrade-review.json", "upgrade_review_sha256"),
                      ("license-evidence.json", "target_evidence_sha256"), ("upgrade-before.json", "old_install_evidence_sha256")):
        path = output / name
        if path.is_file() and value.get(key) != file_digest(path):
            return "release-review.json 未绑定 %s" % name
    if not isinstance(value.get("operator"), str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}", value["operator"]):
        return "release-review.json 审核人无效"
    if value.get("candidate_sha256") != candidate_digest(output):
        return "release-review.json 未绑定当前候选"
    return ""


def status(args):
    output = Path(args.output).resolve()
    files = []
    if output.is_dir():
        files = sorted(item.name for item in output.iterdir() if item.is_file())
    document = {
        "schema": "uip.release-workflow-status/v1",
        "output": str(output),
        "exists": output.is_dir(),
        "files": files,
        "build_info": str(output / "BUILD_INFO.txt") if (output / "BUILD_INFO.txt").is_file() else None,
        "has_manifest": (output / "release-manifest.json").is_file(),
        "has_signature": (output / "release-manifest.sigstore.json").is_file(),
        "has_attestations": (output / "attestations").is_dir(),
        "has_profiles": (output / "profiles").is_dir(),
        "plan_status": (read_plan(output) or {}).get("overall_status"),
        "has_archive": (output.parent / ("uip-offline-delivery-" + args.version + "-linux-amd64.tar")).is_file() if args.version else False,
    }
    if args.format == "json":
        print(json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    else:
        print("输出目录：%s" % output)
        print("存在：%s" % ("是" if document["exists"] else "否"))
        print("文件：%d" % len(files))
        print("材料计划：%s" % (document["plan_status"] or "无"))
        print("发布清单：%s" % ("有" if document["has_manifest"] else "无"))
        print("发布签名：%s" % ("有" if document["has_signature"] else "无"))
        print("证明目录：%s" % ("有" if document["has_attestations"] else "无"))
        print("Profile：%s" % ("有" if document["has_profiles"] else "无"))
        print("总归档：%s" % ("有" if document["has_archive"] else "无"))
    return 0 if document["exists"] else 2


def value(args, flag):
    for index, item in enumerate(args):
        if item == flag and index + 1 < len(args):
            return args[index + 1]
    return None


def gate_build(args):
    if "--help" in args or "-h" in args:
        return True
    spec = value(args, "--release-integrity-spec") or value(args, "--spec")
    if not spec:
        return True
    command = [sys.executable or "python3", str(MATERIALS), "--scenario", "fresh",
               "--spec", spec, "--format", "json"]
    for component in csv([value(args, "--components")]):
        command += ["--component", component]
    approval = value(args, "--runtime-approval-dir")
    if approval:
        command += ["--approval-dir", approval]
    completed = subprocess.run(command, text=True, capture_output=True)
    if completed.returncode not in (0, 3):
        print(completed.stdout.strip() or completed.stderr.strip(), file=sys.stderr)
        return False
    try:
        document = json.loads(completed.stdout)
    except ValueError:
        print("构建前材料门禁返回无效 JSON，拒绝继续", file=sys.stderr)
        return False
    if not isinstance(document, dict) or document.get("schema") != "uip.release-materials/v1" or not isinstance(document.get("materials"), list):
        print("构建前材料门禁返回无效结构，拒绝继续", file=sys.stderr)
        return False
    if document.get("overall_status") not in ("READY", "NEEDS_ACTION"):
        return False
    for item in document["materials"]:
        if not isinstance(item, dict):
            return False
        if item.get("id", "").startswith("runtime-approval:") and item.get("requirement") == "REQUIRED" and not item.get("satisfied"):
            print("构建前材料门禁阻断：%s 缺少正式运行批准" % item["id"], file=sys.stderr)
            return False
    return True


def finalize_gate(args):
    required = {
        "--media": "介质目录", "--spec": "release spec", "--build-key": "构建私钥",
        "--release-key": "发布私钥", "--trust-dir": "信任目录", "--verifier": "verifier",
    }
    paths = {}
    for flag, label in required.items():
        item = value(args, flag)
        if not item or not item.startswith("/"):
            print("finalize 缺少绝对路径参数：%s（%s）" % (flag, label), file=sys.stderr)
            return False
        paths[flag] = Path(item)
    if not paths["--media"].is_dir() or paths["--media"].is_symlink():
        print("finalize 介质目录不存在或不安全", file=sys.stderr)
        return False
    if not (paths["--media"] / "SHA256SUMS").is_file():
        print("finalize 必须在 SHA256SUMS 生成后执行", file=sys.stderr)
        return False
    for flag in ("--spec", "--build-key", "--release-key"):
        if not paths[flag].is_file() or paths[flag].is_symlink():
            print("finalize 参数不是安全普通文件：%s" % flag, file=sys.stderr)
            return False
    if not paths["--trust-dir"].is_dir() or paths["--trust-dir"].is_symlink():
        print("finalize 信任目录不存在或不安全", file=sys.stderr)
        return False
    if not paths["--verifier"].is_file() or paths["--verifier"].is_symlink() or not os.access(paths["--verifier"], os.X_OK):
        print("finalize verifier 不存在或不可执行", file=sys.stderr)
        return False
    for name in ("release-manifest.json", "release-manifest.sigstore.json", "attestations", "profiles"):
        if (paths["--media"] / name).exists():
            print("finalize 拒绝重复签名：介质已包含 %s" % name, file=sys.stderr)
            return False
    return True


def resume_gate(args):
    output = value(args, "--output")
    if not output:
        return True
    state = read_state(Path(output).resolve())
    if not state:
        return True
    if state.get("stage") == "packaged":
        print("输出已完成打包，resume 必须使用新的输出目录/版本", file=sys.stderr)
        return False
    if state.get("stage") not in ("reviewed", "packaged"):
        print("release-state.json 状态不允许 resume", file=sys.stderr)
        return False
    return True


def forward(command, args):
    if command == "resume" and not resume_gate(args):
        return 2
    if command in ("build", "resume") and not gate_build(args):
        return 2
    if command == "finalize" and not finalize_gate(args):
        return 2
    if command in ("build", "resume"):
        target = [str(ROOT), "--legacy-build"] + list(args)
    elif command == "finalize":
        target = [str(FINALIZE)] + list(args)
    else:
        raise SystemExit("unsupported scenario command: " + command)
    os.execv("/bin/bash", ["/bin/bash"] + target)


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] in ("-h", "--help"):
        print("用法：一键制作离线镜像包.sh <plan|build|finalize|package|status|resume> [参数]")
        return 0
    command, rest = argv[0], argv[1:]
    if command == "plan":
        parser = argparse.ArgumentParser()
        parser.add_argument("--scenario")
        parser.add_argument("--installation-scenario")
        parser.add_argument("--spec")
        parser.add_argument("--components", action="append", default=[])
        parser.add_argument("--license-file")
        parser.add_argument("--runtime-approval-dir")
        parser.add_argument("--old-install-evidence")
        parser.add_argument("--entitlement")
        parser.add_argument("--frozen-dir")
        parser.add_argument("--release-manifest")
        parser.add_argument("--backup-evidence")
        parser.add_argument("--compatibility")
        parser.add_argument("--build-evidence")
        parser.add_argument("--source-freeze", action="store_true")
        parser.add_argument("--write-plan")
        parser.add_argument("--format", choices=("human", "json"), default="human")
        return plan(parser.parse_args(rest))
    if command == "status":
        parser = argparse.ArgumentParser()
        parser.add_argument("--output", required=True)
        parser.add_argument("--version")
        parser.add_argument("--format", choices=("human", "json"), default="human")
        return status(parser.parse_args(rest))
    if command == "review":
        parser = argparse.ArgumentParser()
        parser.add_argument("--output", required=True)
        parser.add_argument("--operator", required=True)
        parser.add_argument("--purpose", choices=("release", "upgrade", "migration"), default="release")
        parser.add_argument("--before")
        parser.add_argument("--confirm", action="store_true")
        return review(parser.parse_args(rest))
    if command == "diff":
        parser = argparse.ArgumentParser()
        parser.add_argument("--before", required=True)
        parser.add_argument("--after", required=True)
        parser.add_argument("--format", choices=("human", "json"), default="human")
        return diff(parser.parse_args(rest))
    if command == "package":
        # Packaging an already frozen directory must never rebuild it.
        parser = argparse.ArgumentParser()
        parser.add_argument("--output", required=True)
        parser.add_argument("--version")
        parser.add_argument("--allow-legacy-unsigned", action="store_true")
        parser.add_argument("--verifier", help="介质外受信 release-verifier 绝对路径")
        parser.add_argument("--trust-dir", help="介质外固定信任目录绝对路径")
        return package(parser.parse_args(rest))
    if command in ("build", "resume", "finalize"):
        return forward(command, rest)
    print("未知子命令：%s" % command, file=sys.stderr)
    return 64


if __name__ == "__main__":
    raise SystemExit(main())
