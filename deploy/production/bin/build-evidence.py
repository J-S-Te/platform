#!/usr/bin/env python3
"""从离线包输出目录采集实际构建证据。

本工具不签名、不批准，也不虚构构建者身份。记录离线构建器实际生成的
包归档、镜像配置摘要、Dockerfile、基础镜像引用及源码输入指纹。
后续发布清单或签名步骤仅在 formal_signing_allowed 为 true 时
才可使用这些证据。

退出状态：
  0  证据已写入
  1  构建输入不安全或不一致
  2  用法错误
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tarfile
import tempfile

BUILD_TYPE = "https://uip.dev/buildtypes/container-v1"
SCHEMA = "urn:uip:build-evidence:v1"
VERSION_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
BUILDER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$")
FROM_RE = re.compile(r"^\s*FROM\s+([^\s]+)", re.IGNORECASE)

DOCKERFILES = {
    "common": ("platform/deploy/production/bin/offline-base.Dockerfile", "platform/Dockerfile"),
    "platform": ("platform/Dockerfile",),
    "frontend": ("frontend/Dockerfile",),
    "customer-opportunity": ("customer_and_opportunity/Dockerfile",),
    "customer-portal": ("customer_and_opportunity/Dockerfile",),
    "contract": ("contract_management/Dockerfile",),
    "project": ("project_management/Dockerfile",),
    "settlement": ("Settlement/Dockerfile",),
    "data-analysis": ("data_analysis/Dockerfile",),
}


class EvidenceError(Exception):
    pass


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def read_package_env(archive: Path) -> dict[str, str]:
    with tarfile.open(archive, "r:gz") as bundle:
        members = [item for item in bundle.getmembers() if item.name in ("package.env", "./package.env")]
        if len(members) != 1 or not members[0].isfile():
            raise EvidenceError(f"镜像包 package.env 必须恰好一个：{archive.name}")
        stream = bundle.extractfile(members[0])
        if stream is None:
            raise EvidenceError(f"无法读取镜像包 package.env：{archive.name}")
        raw = stream.read(64 * 1024 + 1)
    if len(raw) > 64 * 1024:
        raise EvidenceError(f"镜像包 package.env 过大：{archive.name}")
    result: dict[str, str] = {}
    for line in raw.decode("utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            raise EvidenceError(f"镜像包 package.env 行格式错误：{archive.name}")
        key, value = line.split("=", 1)
        if not key or key in result:
            raise EvidenceError(f"镜像包 package.env 键重复或为空：{archive.name}")
        result[key] = value
    return result


def verify_sidecar(archive: Path, expected: str) -> None:
    sidecar = archive.with_name(archive.name + ".sha256")
    if sidecar.is_symlink() or not sidecar.is_file():
        raise EvidenceError(f"缺少镜像包 sidecar：{archive.name}")
    raw = sidecar.read_text(encoding="utf-8").splitlines()
    if len(raw) != 1:
        raise EvidenceError(f"镜像包 sidecar 必须恰好一行：{archive.name}")
    parts = raw[0].split()
    if len(parts) != 2 or parts[1] != archive.name or parts[0].lower() != expected:
        raise EvidenceError(f"镜像包 sidecar 与归档不一致：{archive.name}")


def collect_packages(output_root: Path, version: str, platform: str) -> list[dict]:
    packages: list[dict] = []
    seen_components: set[str] = set()
    for archive in sorted(output_root.glob("*-linux-amd64.tar.gz")):
        if archive.name.startswith("host-dependencies-"):
            continue
        if archive.is_symlink() or not archive.is_file():
            raise EvidenceError(f"镜像包不是安全普通文件：{archive.name}")
        metadata = read_package_env(archive)
        if metadata.get("PACKAGE_FORMAT") != "2":
            raise EvidenceError(f"镜像包格式不是 2：{archive.name}")
        component = metadata.get("COMPONENT", "")
        if component in seen_components or component not in DOCKERFILES:
            raise EvidenceError(f"镜像包组件未知或重复：{component or archive.name}")
        if metadata.get("VERSION") != version:
            raise EvidenceError(f"镜像包版本与构建版本不一致：{archive.name}")
        if metadata.get("PLATFORM") != platform:
            raise EvidenceError(f"镜像包平台与构建平台不一致：{archive.name}")
        images = [item for item in metadata.get("IMAGES", "").split(",") if item]
        digests = [item for item in metadata.get("IMAGE_CONFIG_DIGESTS", "").split(",") if item]
        if not images or len(images) != len(digests):
            raise EvidenceError(f"镜像标签与配置摘要数量不一致：{archive.name}")
        for digest in digests:
            if not DIGEST_RE.match(digest):
                raise EvidenceError(f"镜像配置摘要格式错误：{archive.name}")
        archive_digest = sha256_file(archive)
        verify_sidecar(archive, archive_digest)
        seen_components.add(component)
        packages.append({
            "component": component,
            "archive": archive.name,
            "sha256": archive_digest,
            "size": archive.stat().st_size,
            "images": [
                {"tag": images[index], "config_digest": digests[index]}
                for index in range(len(images))
            ],
        })
    if not packages:
        raise EvidenceError("输出目录没有可采集的镜像包")
    return packages


def read_base_image_evidence(path: str) -> dict[str, list[str]]:
    if not path:
        return {}
    source = Path(os.path.abspath(path))
    if source.is_symlink() or not source.is_file():
        raise EvidenceError("基础镜像证据不是安全普通文件")
    mapping: dict[str, list[str]] = {}
    for line in source.read_text(encoding="utf-8").splitlines():
        parts = line.split("\t")
        if len(parts) != 2 or not parts[0] or parts[0].startswith("/") or ".." in Path(parts[0]).parts:
            raise EvidenceError("基础镜像证据行格式错误")
        if not parts[1] or any(char.isspace() for char in parts[1]):
            raise EvidenceError("基础镜像引用非法")
        mapping.setdefault(parts[0], []).append(parts[1])
    return mapping


def collect_dockerfiles(workspace: Path, packages: list[dict], base_evidence: dict[str, list[str]] | None = None) -> tuple[list[dict], bool]:
    base_evidence = base_evidence or {}
    evidence: list[dict] = []
    all_pinned = True
    seen_paths: set[str] = set()
    for package in packages:
        for relative in DOCKERFILES[package["component"]]:
            if relative in seen_paths:
                continue
            seen_paths.add(relative)
            path = workspace / relative
            if path.is_symlink() or not path.is_file():
                raise EvidenceError(f"缺少构建 Dockerfile：{relative}")
            raw = path.read_text(encoding="utf-8")
            base_refs: list[str] = []
            aliases: set[str] = set()
            for line in raw.splitlines():
                match = FROM_RE.match(line)
                if not match:
                    continue
                ref = match.group(1)
                if ref.lower() == "scratch":
                    continue
                if ref.lower() in aliases:
                    # 之前阶段的别名表示内部构建阶段，不是外部仓库基础镜像。
                    alias_match = re.search(r"\s+AS\s+(\S+)", line, re.IGNORECASE)
                    if alias_match:
                        aliases.add(alias_match.group(1).lower())
                    continue
                if ref.startswith("$"):
                    resolved = base_evidence.get(relative, [])
                    if not resolved:
                        base_refs.append(ref)
                        all_pinned = False
                    else:
                        for resolved_ref in resolved:
                            base_refs.append(resolved_ref)
                            if "@sha256:" not in resolved_ref:
                                all_pinned = False
                else:
                    base_refs.append(ref)
                    if "@sha256:" not in ref:
                        all_pinned = False
                alias_match = re.search(r"\s+AS\s+(\S+)", line, re.IGNORECASE)
                if alias_match:
                    aliases.add(alias_match.group(1).lower())
            evidence.append({
                "path": relative,
                "sha256": sha256_file(path),
                "from": base_refs,
                "all_base_refs_pinned": all("@sha256:" in ref for ref in base_refs),
            })
    return evidence, all_pinned and bool(evidence)


def collect_source_index(output_root: Path, workspace: Path, configured: str) -> dict:
    if not configured:
        return {"path": "", "sha256": "", "entries": 0}
    path = Path(os.path.abspath(configured))
    if output_root not in path.parents:
        raise EvidenceError("源码索引必须位于构建输出目录内")
    if path.is_symlink() or not path.is_file():
        raise EvidenceError("源码索引不是安全普通文件")
    digest = sha256_file(path)
    seen: set[str] = set()
    entries = 0
    for line in path.read_text(encoding="utf-8").splitlines():
        parts = line.split("\t")
        if len(parts) != 3 or not SHA256_RE.match(parts[0]) or not parts[1].isdigit():
            raise EvidenceError("源码索引行格式错误")
        relative = parts[2]
        if not relative or relative.startswith("/") or ".." in Path(relative).parts or relative in seen:
            raise EvidenceError("源码索引路径不安全或重复")
        seen.add(relative)
        source = workspace / relative
        if source.is_symlink() or not source.is_file():
            raise EvidenceError(f"源码索引引用的文件缺失：{relative}")
        if source.stat().st_size != int(parts[1]) or sha256_file(source) != parts[0]:
            raise EvidenceError(f"源码索引与实际工作区不一致：{relative}")
        entries += 1
    return {"path": path.name, "sha256": digest, "entries": entries}


def read_build_info(output_root: Path) -> dict[str, str]:
    path = output_root / "BUILD_INFO.txt"
    if path.is_symlink() or not path.is_file():
        raise EvidenceError("缺少 BUILD_INFO.txt")
    result: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line or "=" not in line:
            raise EvidenceError("BUILD_INFO.txt 行格式错误")
        key, value = line.split("=", 1)
        if not key or key in result:
            raise EvidenceError("BUILD_INFO.txt 键重复")
        result[key] = value
    return result


def write_atomic(path: Path, payload: dict) -> None:
    if path.is_symlink():
        raise EvidenceError("build-evidence.json 不能是符号链接")
    if path.exists() and not path.is_file():
        raise EvidenceError("build-evidence.json 已存在且不是普通文件")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".build-evidence.", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-root", required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--platform", default="linux/amd64")
    parser.add_argument("--source-sha256", required=True)
    parser.add_argument("--source-index", default="", help="source-input-index.tsv")
    parser.add_argument("--base-image-evidence", default="", help="base-image-evidence.tsv for ARG-based FROM refs")
    parser.add_argument("--builder-id", default="")
    parser.add_argument("--invocation-id", default="")
    parser.add_argument("--output", default="")
    args = parser.parse_args()
    if not VERSION_RE.match(args.version):
        print("版本格式不正确", file=sys.stderr)
        return 2
    if args.platform != "linux/amd64":
        print("首版只支持 linux/amd64", file=sys.stderr)
        return 2
    if not SHA256_RE.match(args.source_sha256):
        print("source-input SHA-256 格式不正确", file=sys.stderr)
        return 2
    builder_id = args.builder_id.strip()
    if builder_id and not BUILDER_RE.match(builder_id):
        print("builder-id 格式不正确", file=sys.stderr)
        return 2
    output_root = Path(os.path.abspath(args.output_root))
    workspace = Path(os.path.abspath(args.workspace))
    if output_root.is_symlink() or not output_root.is_dir():
        print("输出目录不安全", file=sys.stderr)
        return 1
    try:
        packages = collect_packages(output_root, args.version, args.platform)
        base_evidence = read_base_image_evidence(args.base_image_evidence)
        dockerfiles, all_pinned = collect_dockerfiles(workspace, packages, base_evidence)
        build_info = read_build_info(output_root)
        source_index = collect_source_index(output_root, workspace, args.source_index)
        output = Path(os.path.abspath(args.output)) if args.output else output_root / "build-evidence.json"
        if output_root not in output.parents:
            raise EvidenceError("build-evidence 输出必须位于构建输出目录内")
        payload = {
            "schema": SCHEMA,
            "created_at": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
            "version": args.version,
            "target_platform": args.platform,
            "build_type": BUILD_TYPE,
            "source_input_sha256": args.source_sha256,
            "source_index": source_index,
            "builder": {
                "id": builder_id,
                "invocation_id": args.invocation_id.strip(),
            },
            "tool_versions": {
                "docker_client": build_info.get("DOCKER_CLIENT_VERSION", ""),
                "docker_server": build_info.get("DOCKER_SERVER_VERSION", ""),
                "buildx": build_info.get("BUILDX_VERSION", ""),
            },
            "packages": packages,
            "dockerfiles": dockerfiles,
            # 正式发布签名要求明确的构建器身份和不可变基础镜像引用。二者均是外部事实，此辅助工具绝不伪造。
            "formal_signing_allowed": bool(builder_id and all_pinned and args.invocation_id.strip() and source_index["entries"] > 0),
            "unsigned_candidate": True,
        }
        write_atomic(output, payload)
    except (EvidenceError, OSError, UnicodeError, tarfile.TarError) as exc:
        print(f"build evidence failed: {exc}", file=sys.stderr)
        return 1
    print(f"build evidence written: {output}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
