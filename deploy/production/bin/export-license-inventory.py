#!/usr/bin/env python3
"""导出完整 Compose 清单供审核，不包含运行时凭据。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\Z")
CONTAINER_ID = re.compile(r"[a-f0-9]{64}\Z")
DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")
CONTAINER_FORMAT = ('{"container_id":{{json .Id}},"service":{{json (index .Config.Labels "com.docker.compose.service")}},'
                    '"project":{{json (index .Config.Labels "com.docker.compose.project")}},'
                    '"image_digest":{{json .Image}},"running":{{json .State.Running}},'
                    '"one_off":{{json (index .Config.Labels "com.docker.compose.oneoff")}},'
                    '"started_at":{{json .State.StartedAt}}}')
IMAGE_FORMAT = ('{{$labels := index .Config "Labels"}}{"id":{{json .Id}},'
                '"version":{{if $labels}}{{json (index $labels "org.opencontainers.image.version")}}{{else}}""{{end}},'
                '"protocol":{{if $labels}}{{json (index $labels "com.basic-platform.license.protocol")}}{{else}}""{{end}},'
                '"architecture":{{json .Architecture}},"os":{{json .Os}}}')


def run_docker(args):
    try:
        result = subprocess.run(["docker", *args], stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=10, check=True)
    except (OSError, subprocess.SubprocessError) as exc:
        raise ValueError("Docker 只读检查失败；请检查 Docker 权限和运行状态") from exc
    if len(result.stdout) > 262144:
        raise ValueError("Docker 检查输出超过上限")
    return result.stdout.decode("utf-8")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Docker 输出含重复 JSON 字段")
        result[key] = value
    return result


def docker_object(raw):
    result = json.loads(raw, object_pairs_hook=unique_object)
    if not isinstance(result, dict):
        raise ValueError("Docker 检查必须返回 JSON 对象")
    return result


def safe_file(path, limit):
    if path.is_symlink() or not path.is_file() or path.resolve() != path:
        raise ValueError("部署输入须为无符号链接的普通文件")
    raw = path.read_bytes()
    if len(raw) > limit:
        raise ValueError("部署输入超过大小上限")
    return raw


def project_name(raw):
    # 只解析这个非敏感字段，绝不执行 .env 或导出其内容。
    found = []
    for line in raw.decode("utf-8").splitlines():
        match = re.match(r"\s*(?:export\s+)?COMPOSE_PROJECT_NAME\s*=\s*(.*?)\s*$", line)
        if not match:
            continue
        value = match[1]
        if value[:1] in ("'", '"') and value[-1:] == value[:1]:
            value = value[1:-1]
        if not IDENTIFIER.fullmatch(value):
            raise ValueError("COMPOSE_PROJECT_NAME 格式无效；不执行变量替换")
        found.append(value)
    if len(found) != 1:
        raise ValueError(".env 必须唯一明确声明 COMPOSE_PROJECT_NAME")
    return found[0]


def collect(target, runner=run_docker):
    target = Path(os.path.abspath(target))
    if target == Path("/") or target.resolve() != target or not target.is_dir():
        raise ValueError("部署目录须为无符号链接的非根绝对目录")
    compose = safe_file(target / "docker-compose.yml", 2097152)
    release = safe_file(target / ".release.env", 65536)
    env = safe_file(target / ".env", 1048576)
    project = project_name(env)
    query = ["ps", "--all", "--no-trunc", "--filter",
             "label=com.docker.compose.project=" + project, "--format", "{{.ID}}"]
    ids = runner(query).split()
    if not ids or len(ids) > 128 or len(ids) != len(set(ids)) or any(not CONTAINER_ID.fullmatch(i) for i in ids):
        raise ValueError("项目容器范围为空、重复或超过上限，无法导出完整安装")
    containers, images = [], {}
    for cid in sorted(ids):
        item = docker_object(runner(["container", "inspect", "--format", CONTAINER_FORMAT, cid]))
        if item.get("container_id") != cid or item.pop("project", None) != project:
            raise ValueError("容器归属或 ID 在采集期间发生变化")
        service = item.get("service")
        digest = item.get("image_digest")
        if not isinstance(service, str) or not IDENTIFIER.fullmatch(service) or not isinstance(digest, str) or not DIGEST.fullmatch(digest):
            raise ValueError("容器缺少有效服务归属或实际镜像 ID")
        if not isinstance(item.get("running"), bool) or not isinstance(item.get("started_at"), str):
            raise ValueError("容器运行事实格式无效")
        one_off = item.get("one_off")
        if one_off not in (None, "", "False", "false", "True", "true"):
            raise ValueError("容器 one-off 标记无效")
        item["one_off"] = one_off in ("True", "true")
        if digest not in images:
            image = docker_object(runner(["image", "inspect", "--format", IMAGE_FORMAT, digest]))
            if image.pop("id", None) != digest or image.get("architecture") != "amd64" or image.get("os") != "linux":
                raise ValueError("镜像 ID 或目标平台不匹配 linux/amd64")
            for field in ("version", "protocol"):
                image[field] = "" if image.get(field) is None else image[field]
                if not isinstance(image[field], str):
                    raise ValueError("镜像标签格式无效")
            images[digest] = image
        item = {key: item[key] for key in ("container_id", "service", "image_digest", "running", "one_off", "started_at")}
        item.update({key: images[digest][key] for key in ("version", "protocol", "architecture", "os")})
        containers.append(item)
    # 同时重读投影事实和成员关系，避免并发部署产生由不同安装状态拼装的清单。
    refreshed_ids = runner(query).split()
    if len(refreshed_ids) != len(ids) or len(refreshed_ids) != len(set(refreshed_ids)) or set(refreshed_ids) != set(ids):
        raise ValueError("项目容器集合在采集期间发生变化，请停止并发升级后重试")
    for before in containers:
        after = docker_object(runner(["container", "inspect", "--format", CONTAINER_FORMAT, before["container_id"]]))
        if not isinstance(after.get("running"), bool) or after.get("one_off") not in (None, "", "False", "false", "True", "true"):
            raise ValueError("容器状态格式在采集期间发生变化")
        if any(after.get(key) != before[key] for key in ("container_id", "service", "image_digest", "running", "started_at")) or after.get("project") != project or (after.get("one_off") in ("True", "true")) != before["one_off"]:
            raise ValueError("容器状态在采集期间发生变化，请停止并发升级后重试")
    if compose != safe_file(target / "docker-compose.yml", 2097152) or release != safe_file(target / ".release.env", 65536) or env != safe_file(target / ".env", 1048576):
        raise ValueError("部署配置在采集期间发生变化，请重试")
    return {"version": 1, "project": project, "deploy_root": str(target),
            "collected_at": datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z"),
            "compose_digest": "sha256:" + hashlib.sha256(compose).hexdigest(),
            "release_digest": "sha256:" + hashlib.sha256(release).hexdigest(),
            "containers": containers}


def write_inventory(output, inventory):
    output = Path(os.path.abspath(output))
    if output.parent.resolve() != output.parent or not output.parent.is_dir():
        raise ValueError("输出父目录必须已存在且无符号链接")
    raw = (json.dumps(inventory, ensure_ascii=False, indent=2) + "\n").encode()
    if len(raw) > 262144:
        raise ValueError("采集结果超过上限")
    fd, temporary = tempfile.mkstemp(prefix=".license-inventory-", dir=output.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        # 通过排他硬链接发布，防止最终产物只有部分内容，并拒绝已有文件或符号链接。
        os.link(temporary, output, follow_symlinks=False)
    finally:
        Path(temporary).unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        write_inventory(args.output, collect(args.target))
    except (ValueError, OSError, UnicodeError, json.JSONDecodeError) as exc:
        print("[错误] " + str(exc), file=sys.stderr)
        return 1
    print("旧安装完整运行信息已导出：" + args.output)
    print("文件只包含运行元数据；尚未批准或冻结迁移，也未修改容器。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
