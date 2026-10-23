#!/usr/bin/env python3
"""停机恢复本地授权固定文件：默认只读，--apply 先备份再原子写回。

只接受全部应用的已审核批准、当前源码/编排和完整组件清单；不从标签推导镜像，
不修改批准、运行凭据或状态卷，不执行构建、迁移或启动。恢复后仍须 Agent 受控更新。
停机恢复期间不得并发启动 Agent 或运行其他发布/凭据写入入口。
"""
import argparse
import datetime
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

SPEC = importlib.util.spec_from_file_location(
    "prepare_local_runtime_license", Path(__file__).with_name("prepare-local-runtime-license.py"))
PREP = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PREP)


def assert_offline():
    try:
        result = subprocess.run(
            ["docker", "ps", "--filter", "label=com.docker.compose.project=basic-platform-local", "--format", "{{.ID}}"],
            capture_output=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired):
        PREP.fail("无法核对本地停机状态")
    if result.returncode != 0 or result.stdout.strip():
        PREP.fail("固定文件恢复要求 basic-platform-local 全部容器已停止，且 Docker 可达")


def fsync_directory(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def backup_original(path, raw):
    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    fd, name = tempfile.mkstemp(prefix=path.name + "." + timestamp + ".", suffix=".bak", dir=path.parent)
    with os.fdopen(fd, "wb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    backup = Path(name)
    fsync_directory(path.parent)
    if PREP.read_regular(backup, private=True) != raw:
        PREP.fail("固定文件备份校验失败，保留原文件")
    return backup


def recovery_plan(workspace):
    workspace = PREP.safe_chain(workspace)
    manifest_path = workspace / "platform/config/local-license-components.json"
    manifest_raw = PREP.read_regular(manifest_path)
    apps = PREP.load_manifest(workspace)
    compose_path = workspace / "platform/compose.local.yaml"
    compose_raw = PREP.read_regular(compose_path, 2 * 1024 * 1024)
    snapshots = {manifest_path: manifest_raw, compose_path: compose_raw}
    services, volumes, rows = {}, {}, []
    for app, target in sorted(apps.items()):
        approval_path = workspace / "platform/runtime-approvals" / ("local-" + app + "-dev.json")
        approval_raw = PREP.read_regular(approval_path, private=True)
        snapshots[approval_path] = approval_raw
        document = PREP.parse_json(approval_raw)
        PREP.validate_document(document)
        source = PREP.source_digest(workspace, target["directory"])
        if document["application"] != app or document["reviewed"] is not True:
            PREP.fail(app + ": 需要已审核的正式本地批准，候选不能用于恢复")
        if document["compose_sha256"] != PREP.digest(compose_raw) or document["source_digest"] != source:
            PREP.fail(app + ": 批准的源码或编排指纹已过期")
        expected = set(target["services"])
        if {item["name"] for item in document["services"]} != expected:
            PREP.fail(app + ": 批准未覆盖完整 API/Worker 组件")
        coverage = PREP.digest(("dev-source-fingerprint\0" + app + "\0" + source).encode())
        if any(item["coverage_digest"] != coverage for item in document["components"]):
            PREP.fail(app + ": 组件源码指纹绑定不一致")
        migration = document["migration"]
        if migration["service"] != target["migration"]["service"]:
            PREP.fail(app + ": 迁移服务与完整组件清单不一致")
        entries = [(item["name"], item) for item in document["services"]]
        entries.append((migration["service"], migration))
        metadata = {}
        for name, item in entries:
            if name in services:
                PREP.fail("组件清单存在跨应用重复服务：" + name)
            image = item["image_digest"]
            if image not in metadata:
                # 仅检查批准中的不可变 ID，绝不解析 :local 作为恢复依据。
                try:
                    metadata[image] = PREP.inspect_image(image)
                except PREP.PreparationError:
                    PREP.fail(app + " / " + name + " / " + image + ": 批准镜像不可用或元数据非法")
            actual = metadata[image]
            if actual["id"] != image or actual["version"] != item["version"] or actual["protocol"] != item["protocol"]:
                PREP.fail(app + " / " + name + ": 批准镜像元数据不一致")
            spec = {"image": image, "pull_policy": "never"}
            if name in expected:
                env_path = workspace / "platform/runtime" / ("license-" + name + ".env")
                # 不生成/猜测凭据；只保留已有私有文件，后续由 Agent 正规更新。
                snapshots[env_path] = PREP.read_regular(env_path, private=True)
                spec["env_file"] = [str(env_path)]
                spec["volumes"] = ["license-state-" + name + ":/var/lib/commercial-license",
                                   str(workspace / "platform/data/keys") + ":/app/data/keys:ro"]
                volumes["license-state-" + name] = {}
            services[name] = spec
            rows.append((app, name, image))
    pin = workspace / "platform/runtime/local-license.compose.json"
    original = PREP.read_regular(pin, private=True)
    existing = PREP.parse_json(original)
    if not isinstance(existing, dict) or set(existing) != {"services", "volumes"} or not isinstance(existing["services"], dict) or not isinstance(existing["volumes"], dict):
        PREP.fail("固定文件结构非法，不能安全保留挂载和状态身份")
    for name, spec in existing["services"].items():
        if name not in services or not isinstance(spec, dict):
            PREP.fail("固定文件包含未知服务或非法服务结构：" + name)
        expected_spec = dict(services[name])
        expected_spec.pop("image")
        old_spec = {key: value for key, value in spec.items() if key != "image"}
        # Agent 历史文件允许省略 pull_policy；恢复统一设置 never。
        old_spec.setdefault("pull_policy", "never")
        if old_spec != expected_spec:
            PREP.fail("固定文件存在非标准挂载/配置，拒绝自动恢复：" + name)
    if any(name not in volumes or value != {} for name, value in existing["volumes"].items()):
        PREP.fail("固定文件包含非标准状态卷，拒绝自动恢复")
    return pin, original, {"services": services, "volumes": volumes}, rows, snapshots


def recover(workspace, apply=False):
    pin, original, target, rows, snapshots = recovery_plan(workspace)
    result = {"services": rows, "backup": None}
    if not apply:
        return result
    assert_offline()
    # 同一恢复入口互斥；Agent 已停机，并在备份后重新核对全部输入防止并发改写。
    import fcntl
    lock_fd = os.open(PREP.safe_chain(pin.parent / ".license-pin-recovery.lock"),
                      os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if PREP.read_regular(pin, private=True) != original:
            PREP.fail("固定文件在核对后发生变化，请重新预览")
        backup = backup_original(pin, original)
        result["backup"] = str(backup)
        _, current, new_target, _, new_snapshots = recovery_plan(workspace)
        if current != original or new_target != target or new_snapshots != snapshots:
            PREP.fail("批准、源码、凭据或固定文件在备份后发生变化，拒绝恢复")
        assert_offline()
        fd, temporary = tempfile.mkstemp(prefix=".local-license-recovery-", dir=pin.parent)
        try:
            with os.fdopen(fd, "wb") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write((json.dumps(target, ensure_ascii=False, indent=2) + "\n").encode())
                stream.flush()
                os.fsync(stream.fileno())
            # 停机协议隔离 Agent；恢复锁隔离同入口。替换前最后核对外部输入，
            # 防止在备份/停机查询/临时写入期间出现的人工改动被覆盖。
            for path, raw in snapshots.items():
                limit = 2 * 1024 * 1024 if path.name == "compose.local.yaml" else PREP.LIMIT
                if PREP.read_regular(path, limit) != raw:
                    PREP.fail("批准、编排、组件清单或凭据在最终核对前发生变化，拒绝恢复")
            if PREP.read_regular(pin, private=True) != original:
                PREP.fail("固定文件在最终核对前发生变化，拒绝覆盖")
            os.replace(temporary, pin)
            fsync_directory(pin.parent)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    finally:
        os.close(lock_fd)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workspace", type=Path, default=Path(__file__).absolute().parents[2])
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args(argv)
    try:
        result = recover(args.workspace, args.apply)
        for app, service, image in result["services"]:
            print(app + " / " + service + " / " + image)
        if result["backup"]:
            print("已恢复固定文件；原文件备份：" + result["backup"])
            print("运行凭据未变更。恢复控制面后，逐应用执行 Agent 受控更新并确认成功，再运行普通 up。")
        else:
            print("只读预览通过；--apply 将先备份再恢复。要求本地应用及 Agent 全部停止。")
        return 0
    except (PREP.PreparationError, OSError) as exc:
        print("ERROR: " + (str(exc) if isinstance(exc, PREP.PreparationError) else "恢复文件操作失败，未完成恢复"), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
