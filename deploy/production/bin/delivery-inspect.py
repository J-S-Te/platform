#!/usr/bin/env python3
"""检查离线交付字节及声明，不使用 Docker 或提取归档。

READY 仅表示所提供的结构相互一致。签名、安装身份、运行范围
及当前确认状态仍以平台校验为准。
"""
import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import sys
import tarfile

sys.dont_write_bytecode = True


def sibling(name):
    path = Path(__file__).resolve().parent / name
    spec = importlib.util.spec_from_file_location("uip_"+name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


IMAGES = sibling("delivery-inspect-images.py")
Invalid = IMAGES.Invalid
DIGEST = IMAGES.DIGEST
HEX = IMAGES.HEX
VERSION = re.compile(r"[A-Za-z0-9._-]{1,128}\Z")
IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
SERVICE = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
SAFE_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,220}\Z")
CHINESE_NAMES = {"服务器一键离线部署.sh", "系统运维工具.sh", "清理首次部署产物.sh", "交付检查.sh", "离线交付检查.sh", "离线镜像构建脚本.sh", "服务器前置检查与安装脚本.sh"}
COMPONENTS = {
    "common": "common-infrastructure", "platform": "platform-backend", "frontend": "frontend",
    "customer-opportunity": "customer-opportunity-backend", "customer-portal": "customer-portal-backend",
    "contract": "contract-backend", "project": "project-backend", "settlement": "settlement-backend", "data-analysis": "data-analysis-backend",
}
APPS = {"customer-opportunity": "customer_and_opportunity", "customer-portal": "customer_portal", "contract": "contract_management",
        "project": "project_management", "settlement": "settlement", "data-analysis": "data_analysis"}
APP_COMPONENT = {app: component for component, app in APPS.items()}
PROFILES = {"customer_and_opportunity": "customer_and_opportunity-prod.yaml", "customer_portal": "customer_portal-prod.yaml",
            "contract_management": "contract_management-prod.yaml", "project_management": "project_management-prod.yaml",
            "settlement": "settlement-prod.yaml", "data_analysis": "data-analysis-prod.yaml"}
PROXY = "ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459"
CORE = {"keycloak-db", "keycloak", "platform-mysql", "platform-migrate", "platform-api", "platform-worker", "docker-socket-proxy",
        "subsystem-provisioner", "platform-key-init", "subsystem-provisioner-socket-init", "frontend", "file-gateway-mysql", "file-gateway",
        "prometheus", "keycloak-backup-metrics"}
ASSET_DIRS = {"bin", "subsystems.d", "subsystem-templates", "mysql-init", "nginx", "monitoring", "tests"}
ASSET_FILES = {"docker-compose.yml", ".env.example", ".release.env.example", ".gitignore", "ACCEPTANCE_CHECKLIST.md", "README.md",
               "OFFLINE_DEPLOYMENT.md", "OFFLINE_RUNBOOK.md", "DELIVERY_WORKFLOW.md", "DEPLOYMENT_COMPATIBILITY.md", "BACKUP_RECOVERY.md", "CONTAINER_TIMEZONE.md",
               "license-installation.json", "license-evidence.json", "license-migration-evidence.json"}
APPROVAL_NAMES = {"runtime-license-"+app+"-prod.json" for app in PROFILES}
RECOVERY_TOOLS = {"deployment-workflow.sh", "deployment-session.py", "delivery-runtime.sh", "delivery-runtime.py",
                  "delivery-inspect.py", "delivery-inspect-images.py", "delivery-inspect-v2.py", "delivery-files.py", "subsystem-runtime.py", "project-background.sh"}


def safe_basename(name):
    if (not (SAFE_NAME.fullmatch(name) or name in CHINESE_NAMES) or name.endswith((".pem", ".key", ".p12", ".jks")) or name.startswith(".env")
            or name.endswith(".env") and name != "PLATFORM_ASSETS_COMPATIBILITY.env"):
        raise Invalid("unsafe delivery filename")
    return name


def regular(path, limit=IMAGES.ARCHIVE_LIMIT):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or path.is_symlink() or info.st_size > limit:
        raise Invalid("unsafe or oversized delivery file")
    return info


def read_bytes(source, limit=2*1024*1024):
    raw = source.read(limit+1)
    if len(raw) > limit:
        raise Invalid("delivery metadata exceeds size limit")
    return raw


def sha(source):
    result = hashlib.sha256()
    total = 0
    while True:
        block = source.read(1024*1024)
        if not block:
            break
        total += len(block)
        if total > IMAGES.ARCHIVE_LIMIT:
            raise Invalid("delivery file exceeds byte budget")
        result.update(block)
    return result.hexdigest()


def checksums(raw):
    try:
        lines = raw.decode("utf-8").splitlines()
    except UnicodeError as error:
        raise Invalid("invalid checksum manifest encoding") from error
    if not lines or len(lines) > 4096:
        raise Invalid("empty or excessive checksum manifest")
    result = {}
    for line in lines:
        match = re.fullmatch(r"([A-Fa-f0-9]{64}) [ *](.+)", line)
        if not match:
            raise Invalid("invalid checksum manifest row")
        name = safe_basename(match[2])
        if name in result or name == "SHA256SUMS":
            raise Invalid("duplicate or recursive checksum manifest entry")
        result[name] = match[1].lower()
    return result


def env_document(raw, allowed=None):
    try:
        text = raw.decode("utf-8")
    except UnicodeError as error:
        raise Invalid("invalid metadata encoding") from error
    result = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if (not separator or not re.fullmatch(r"[A-Z0-9_]+", key) or key in result or not value
                or len(value) > 4096 or any(ord(ch) < 32 or ord(ch) > 126 for ch in value) or (allowed is not None and key not in allowed)):
            raise Invalid("invalid or duplicate metadata field")
        result[key] = value
    return result


RELEASE_INTEGRITY_TOP_LEVEL = {
    "release-manifest.json",
    "release-manifest.sigstore.json",
}
RELEASE_INTEGRITY_DIRECTORIES = {"attestations", "profiles"}


def integrity_name(name):
    if name in RELEASE_INTEGRITY_TOP_LEVEL:
        return name
    parts = name.split("/")
    if len(parts) != 2 or parts[0] not in RELEASE_INTEGRITY_DIRECTORIES:
        raise Invalid("unsupported release proof path")
    safe_basename(parts[1])
    if (parts[0] == "profiles" and not parts[1].endswith(".yaml")
            or parts[0] == "attestations" and not re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,127}\.intoto(?:\.sigstore)?\.json", parts[1])):
        raise Invalid("unsupported release proof filename")
    return name


def snapshot(info):
    return info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns


class DirectoryMedia:
    def __init__(self, path):
        self.path = path
        if path.is_symlink() or not path.is_dir():
            raise Invalid("delivery directory is unsafe")
        self.root_snapshot = snapshot(path.lstat())
        self.files, self.integrity_files, self.snapshots, self.directory_snapshots = self._scan()
        self.integrity_dirs = set(self.directory_snapshots)
        self.root_fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_NOFOLLOW", 0))
        try:
            if snapshot(os.fstat(self.root_fd)) != self.root_snapshot or snapshot(path.lstat()) != self.root_snapshot:
                raise Invalid("delivery directory changed during inspection")
        except Exception:
            self.close()
            raise

    def _scan(self):
        files, proofs, snapshots, directories = {}, {}, {}, {}
        proof_bytes = 0
        for item in self.path.iterdir():
            if item.name in RELEASE_INTEGRITY_DIRECTORIES:
                info = item.lstat()
                if not stat.S_ISDIR(info.st_mode) or item.is_symlink():
                    raise Invalid("release proof directory is unsafe")
                directories[item.name] = snapshot(info)
                children = list(item.iterdir())
                if len(children) > 4096:
                    raise Invalid("excessive release proof files")
                for child in children:
                    name = integrity_name(item.name + "/" + child.name)
                    child_info = regular(child, 8 * 1024 * 1024)
                    proofs[name], snapshots[name] = child_info.st_size, snapshot(child_info)
                    proof_bytes += child_info.st_size
                continue
            name = safe_basename(item.name)
            info = regular(item, 8 * 1024 * 1024 if name in RELEASE_INTEGRITY_TOP_LEVEL else IMAGES.ARCHIVE_LIMIT)
            (proofs if name in RELEASE_INTEGRITY_TOP_LEVEL else files)[name] = info.st_size
            snapshots[name] = snapshot(info)
            if name in RELEASE_INTEGRITY_TOP_LEVEL:
                proof_bytes += info.st_size
        if len(snapshots) > 4097 or proof_bytes > 64 * 1024 * 1024:
            raise Invalid("excessive delivery files or release proof bytes")
        return files, proofs, snapshots, directories

    def open(self, name):
        if name not in self.snapshots:
            raise Invalid("missing delivery member")
        parent = os.dup(self.root_fd)
        try:
            parts = name.split("/")
            if len(parts) == 2:
                nested = os.open(parts[0], os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_NOFOLLOW", 0), dir_fd=parent)
                os.close(parent)
                parent = nested
                if snapshot(os.fstat(parent)) != self.directory_snapshots[parts[0]]:
                    raise Invalid("release proof directory changed during open")
            descriptor = os.open(parts[-1], os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0), dir_fd=parent)
            if snapshot(os.fstat(descriptor)) != self.snapshots[name]:
                os.close(descriptor)
                raise Invalid("delivery file changed during open")
        finally:
            os.close(parent)
        return os.fdopen(descriptor, "rb")

    def close(self):
        if getattr(self, "root_fd", None) is not None:
            os.close(self.root_fd)
            self.root_fd = None

    def verify(self):
        if snapshot(self.path.lstat()) != self.root_snapshot:
            raise Invalid("delivery directory changed during inspection")
        files, proofs, snapshots, directories = self._scan()
        if files != self.files or proofs != self.integrity_files or directories != self.directory_snapshots:
            raise Invalid("delivery member set changed during inspection")
        if snapshots != self.snapshots:
            raise Invalid("delivery bytes changed during inspection")


class ArchiveMedia:
    def __init__(self, path):
        regular(path)
        sidecar = path.with_name(path.name+".sha256")
        regular(sidecar, 8192)
        with sidecar.open("rb") as data:
            values = checksums(read_bytes(data, 8192))
        if set(values) != {safe_basename(path.name)}:
            raise Invalid("outer archive sidecar does not bind exactly this file")
        self.path = path
        self.source = os.fdopen(os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)), "rb")
        info = os.fstat(self.source.fileno())
        self.snapshot = (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
        self.archive = None
        try:
            if sha(self.source) != values[path.name]:
                raise Invalid("outer archive checksum mismatch")
            self.source.seek(0)
            self.archive = tarfile.open(fileobj=self.source, mode="r:", tarinfo=IMAGES.SafeTarInfo, ignore_zeros=True)
            self._index()
        except Exception:
            self.close()
            raise

    def _index(self):
        self.files, self.members, self.integrity_files, self.integrity_dirs = {}, {}, {}, set()
        top = None
        seen = set()
        proof_bytes = 0
        for member in self.archive:
            name = IMAGES.member_name(member)
            if not name:
                continue
            if name in seen or len(seen) > 8192:
                raise Invalid("duplicate or excessive outer archive members")
            seen.add(name)
            parts = name.split("/")
            if top is None:
                top = parts[0]
                if len(top.encode("utf-8")) > 255:
                    raise Invalid("outer delivery directory name exceeds limit")
            if parts[0] != top or (len(parts) == 1 and not member.isdir()) or len(parts) > 3:
                raise Invalid("outer archive must contain one bounded delivery directory")
            if len(parts) == 1:
                continue
            relative = "/".join(parts[1:])
            if len(parts) == 2 and member.isdir():
                if relative not in RELEASE_INTEGRITY_DIRECTORIES:
                    raise Invalid("unsupported outer delivery subdirectory")
                self.integrity_dirs.add(relative)
                continue
            if not member.isfile() or relative in RELEASE_INTEGRITY_DIRECTORIES:
                raise Invalid("unsupported outer delivery member type")
            if len(parts) == 3 or relative in RELEASE_INTEGRITY_TOP_LEVEL:
                integrity_name(relative)
                if member.size > 8 * 1024 * 1024:
                    raise Invalid("release proof exceeds size limit")
                proof_bytes += member.size
                if proof_bytes > 64 * 1024 * 1024:
                    raise Invalid("release proofs exceed byte budget")
                self.integrity_files[relative] = member.size
                if len(parts) == 3:
                    self.integrity_dirs.add(parts[1])
            else:
                safe_basename(relative)
                self.files[relative] = member.size
            self.members[relative] = member
        if len(self.files) + len(self.integrity_files) > 4097:
            raise Invalid("excessive delivery files")

    def open(self, name):
        if name not in self.members:
            raise Invalid("missing outer delivery member")
        return self.archive.extractfile(self.members[name])

    def close(self):
        if self.archive is not None:
            self.archive.close()
        self.source.close()

    def verify(self):
        info = regular(self.path)
        if self.snapshot != (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns):
            raise Invalid("outer delivery changed during inspection")


def expected_tags(component, version):
    if component == "common":
        return ["mysql:8.4", "quay.io/keycloak/keycloak:26.2", "temporalio/auto-setup:1.29.7", "metabase/metabase:v0.53.7",
                "prom/prometheus:v3.5.0", "prom/node-exporter:v1.9.1", "registry:2.8.3", "tecnativa/docker-socket-proxy:v0.5.0", "uip-package/file-gateway:"+version]
    if component == "data-analysis":
        return ["uip-package/data-analysis-"+role+":"+version for role in ("dashboard-api", "aggregation-worker", "alert-worker", "production-migrate")]
    return ["uip-package/"+COMPONENTS[component]+":"+version]


def component_package(source):
    # Gzip 外层是包围大 images.tar 的小型目录表；文件及外层 TAR 流支持 TarFile 定位，无需临时文件。
    with tarfile.open(fileobj=source, mode="r:gz", tarinfo=IMAGES.SafeTarInfo, ignore_zeros=True) as archive:
        entries = {}
        seen = set()
        for member in archive:
            name = IMAGES.member_name(member)
            if name in seen or not member.isfile():
                raise Invalid("duplicate or unsafe image package member")
            seen.add(name)
            if name in {"._package.env", "._SHA256SUMS", "._images.tar"}:
                if member.size > IMAGES.META_LIMIT:
                    raise Invalid("legacy image envelope metadata exceeds limit")
                continue
            if name not in {"package.env", "SHA256SUMS", "images.tar"}:
                raise Invalid("image package must contain exactly three safe files")
            entries[name] = member
        if set(entries) != {"package.env", "SHA256SUMS", "images.tar"}:
            raise Invalid("incomplete image package")
        metadata = env_document(read_bytes(archive.extractfile(entries["package.env"]), 65536),
                                {"PACKAGE_FORMAT", "COMPONENT", "VERSION", "PLATFORM", "IMAGES", "IMAGE_IDS", "IMAGE_CONFIG_DIGESTS",
                                 "DOCKER_SOCKET_PROXY_UPSTREAM_REF", "DOCKER_SOCKET_PROXY_CONFIG_DIGEST"})
        component, version = metadata.get("COMPONENT"), metadata.get("VERSION", "")
        if component not in COMPONENTS or not VERSION.fullmatch(version) or metadata.get("PLATFORM") != "linux/amd64" or metadata.get("PACKAGE_FORMAT") not in ("1", "2"):
            raise Invalid("invalid image package component, version or platform")
        tags = metadata.get("IMAGES", "").split(",")
        if tags != expected_tags(component, version):
            raise Invalid("image names differ from the registered component")
        if "IMAGE_CONFIG_DIGESTS" in metadata and "IMAGE_IDS" in metadata and metadata["IMAGE_CONFIG_DIGESTS"] != metadata["IMAGE_IDS"]:
            raise Invalid("conflicting image config metadata")
        configs = metadata.get("IMAGE_CONFIG_DIGESTS", metadata.get("IMAGE_IDS", "")).split(",")
        inner = checksums(read_bytes(archive.extractfile(entries["SHA256SUMS"]), 8192))
        if set(inner) != {"images.tar"}:
            raise Invalid("invalid inner image checksum manifest")
        images = IMAGES.image_archive(archive.extractfile(entries["images.tar"]), tags, configs, inner["images.tar"], entries["images.tar"].size)
        if component in APPS:
            for fact in images.values():
                if fact["protocol"] != "1" or not IDENTIFIER.fullmatch(fact["version"]):
                    raise Invalid("business image lacks valid runtime protocol/version metadata")
                if component != "customer-opportunity" and fact["version"] != version:
                    raise Invalid("built business image version differs from package version")
        proxy_keys = {"DOCKER_SOCKET_PROXY_UPSTREAM_REF", "DOCKER_SOCKET_PROXY_CONFIG_DIGEST"}
        if proxy_keys & metadata.keys():
            if component != "common" or not proxy_keys <= metadata.keys() or metadata["DOCKER_SOCKET_PROXY_UPSTREAM_REF"] != PROXY or metadata["DOCKER_SOCKET_PROXY_CONFIG_DIGEST"] != configs[7]:
                raise Invalid("fixed proxy provenance metadata is incomplete or inconsistent")
        return {"component": component, "version": version, "platform": "linux/amd64", "images": images, "metadata": metadata}


def read_component_package(path):
    """供运行时证明工具使用的可信模块接口，镜像保持包内顺序。"""
    path = Path(path)
    before = regular(path)
    with os.fdopen(os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)), "rb") as data:
        digest = sha(data)
        data.seek(0)
        result = component_package(data)
        after = regular(path)
        if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns):
            raise Invalid("image package changed during inspection")
    result["sha256"] = digest
    return result


def asset_contents(source):
    captured, seen, size = {}, set(), 0
    with tarfile.open(fileobj=source, mode="r:gz", tarinfo=IMAGES.SafeTarInfo, ignore_zeros=True) as archive:
        for member in archive:
            name = IMAGES.member_name(member)
            if not name:
                continue
            if name in seen or len(seen) >= 8192:
                raise Invalid("duplicate or excessive deployment asset members")
            seen.add(name)
            top = name.split("/", 1)[0]
            valid = top in ASSET_DIRS or name in ASSET_FILES or name in APPROVAL_NAMES or name in ("license", "license/commercial-license.jws")
            if not valid or (top in ASSET_FILES | APPROVAL_NAMES and "/" in name):
                raise Invalid("deployment asset is outside the installer allowlist")
            if (name.endswith((".pem", ".key", ".p12", ".jks")) or (name.rsplit("/", 1)[-1].startswith(".env") and not name.endswith(".env.example"))):
                raise Invalid("deployment assets contain a private runtime file")
            size += member.size
            if size > 128*1024*1024:
                raise Invalid("deployment asset archive exceeds byte budget")
            if name in {"bin/"+tool for tool in RECOVERY_TOOLS} and member.isfile():
                if member.size == 0 or member.size > 2*1024*1024:
                    raise Invalid("deployment recovery tool is empty or oversized")
                # 存在性仅是静态能力声明，绝不授权执行或导入被检查归档中的代码。
                captured[name] = b""
            wanted = (name in ASSET_FILES | APPROVAL_NAMES or name == "license/commercial-license.jws"
                      or name.startswith("subsystems.d/") and name.endswith("-prod.yaml"))
            if member.isfile() and wanted:
                captured[name] = read_bytes(archive.extractfile(member), 2*1024*1024)
        if not {"docker-compose.yml", "bin/deploy.sh"} <= seen:
            raise Invalid("deployment assets lack Compose or deployment entry")
    return captured


def host_dependencies(source):
    metadata, expected, actual, seen, total = None, None, {}, set(), 0
    with tarfile.open(fileobj=source, mode="r|gz", tarinfo=IMAGES.SafeTarInfo, ignore_zeros=True) as archive:
        for member in archive:
            name = IMAGES.member_name(member)
            if not name:
                continue
            if name in seen or len(seen) >= 4096:
                raise Invalid("duplicate or excessive host dependency members")
            seen.add(name)
            if not (name in {"host-dependencies.env", "package-manifest.tsv", "Packages", "Packages.gz", "SHA256SUMS", "debs"}
                    or re.fullmatch(r"debs/[A-Za-z0-9][A-Za-z0-9._+%~-]*\.deb", name)):
                raise Invalid("unknown host dependency archive member")
            total += member.size
            if total > 2*1024*1024*1024:
                raise Invalid("host dependency byte budget exceeded")
            if not member.isfile():
                if name != "debs":
                    raise Invalid("invalid host dependency member type")
                continue
            with archive.extractfile(member) as data:
                if name in {"host-dependencies.env", "SHA256SUMS"}:
                    raw = read_bytes(data)
                    actual[name] = hashlib.sha256(raw).hexdigest()
                    if name == "host-dependencies.env":
                        metadata = env_document(raw)
                    else:
                        expected = {}
                        for line in raw.decode("utf-8").splitlines():
                            match = re.fullmatch(r"([a-fA-F0-9]{64}) [ *]([A-Za-z0-9][A-Za-z0-9._+%/~-]*)", line)
                            if not match or ".." in match[2].split("/") or match[2] in expected or match[2] == "SHA256SUMS":
                                raise Invalid("invalid host dependency checksum manifest")
                            expected[match[2]] = match[1].lower()
                else:
                    actual[name] = sha(data)
    if (metadata is None or metadata.get("PACKAGE_FORMAT") != "1" or metadata.get("ARCHITECTURE") != "amd64" or metadata.get("OS_ID") != "ubuntu"
            or expected is None or not expected or set(actual)-{"SHA256SUMS"} != set(expected) or any(actual[name] != value for name, value in expected.items())):
        raise Invalid("host dependency platform or inner checksums disagree")


def json_object(raw, allowed):
    value = IMAGES.document(raw)
    if not isinstance(value, dict) or set(value)-allowed or type(value.get("version")) is not int or value["version"] != 1:
        raise Invalid("invalid declaration fields or version")
    return value


class Inspection:
    def __init__(self, media, phase):
        self.media, self.phase = media, phase
        self.report = {"version": 1, "status": "INVALID", "delivery_id": "", "release_version": "", "target_platform": "linux/amd64",
                       "components": [], "applications": [], "consumer_count": 0, "checks": [], "next_actions": []}
        self.manifest, self.actual, self.packages, self.assets, self.projections, self.build_info = {}, {}, {}, {}, {}, {}
        self.registration_protocols = {}
        self.material_pending = False
        self.required_missing = False
        self.declared = False
        self.current_id, self.current_scope = "media.structure", "MEDIA"

    def check(self, identifier, status, scope, message, required=True):
        self.report["checks"].append({"id": identifier, "status": status, "scope": scope, "message": message, "required_for_install": required})

    def pending(self, identifier, scope, message, required):
        self.check(identifier, "PENDING", scope, message, required)
        self.material_pending = True
        self.required_missing = self.required_missing or required

    def action(self, location, action, message):
        value = {"location": location, "action": action, "message": message}
        if value not in self.report["next_actions"]:
            self.report["next_actions"].append(value)

    def raw(self, name, limit=2*1024*1024):
        with self.media.open(name) as source:
            return read_bytes(source, limit)

    def structure(self):
        self.current_id = "media.checksums"
        manifest_raw = self.raw("SHA256SUMS")
        self.report["delivery_id"] = hashlib.sha256(manifest_raw).hexdigest()
        self.manifest = checksums(manifest_raw)
        for name, digest in self.manifest.items():
            if name not in self.media.files:
                raise Invalid("manifest references a missing delivery file")
            with self.media.open(name) as data:
                self.actual[name] = sha(data)
            if self.actual[name] != digest:
                raise Invalid("delivery payload checksum mismatch")
        for name in self.media.files:
            if name == "SHA256SUMS":
                continue
            if name not in self.manifest and not name.endswith(".sha256"):
                raise Invalid("delivery contains an unbound file")
            if name.endswith(".sha256"):
                values = checksums(self.raw(name, 8192))
                payload = name[:-7]
                if set(values) != {payload} or payload not in self.manifest or values[payload] != self.actual[payload]:
                    raise Invalid("sidecar does not bind the checksummed payload")
        self.check("media.checksums", "PASS", "MEDIA", "交付文件、单包校验和总清单一致")
        asset_names = []
        for name in sorted(self.manifest):
            if name.startswith("deployment-assets-") and name.endswith(".tar.gz"):
                asset_names.append(name)
            elif name.endswith("-linux-amd64.tar.gz") and not name.startswith("host-dependencies-"):
                self.current_id = "package.content"
                with self.media.open(name) as data:
                    package = component_package(data)
                component = package["component"]
                expected = COMPONENTS[component]+("" if component == "common" else "-"+package["version"])+"-linux-amd64.tar.gz"
                if name != expected or component in self.packages:
                    raise Invalid("package filename/component/version mismatch or duplicate component")
                package["archive"] = name
                self.packages[component] = package
                self.check("package."+component, "PASS", component, "镜像包身份图、配置摘要、平台与内容一致")
            elif name.startswith("host-dependencies-") and name.endswith("-linux-amd64.tar.gz"):
                self.current_id = "package.host-dependencies"
                with self.media.open(name) as data:
                    host_dependencies(data)
                self.check("package.host-dependencies", "PASS", "MEDIA", "宿主依赖平台与内部校验清单一致")
            elif name.endswith(".tar.gz"):
                raise Invalid("unknown delivery archive type")
        if not {"common", "platform", "frontend"} <= self.packages.keys() or len(asset_names) != 1:
            raise Invalid("delivery lacks a unique complete platform/assets baseline")
        self.report["components"] = sorted(self.packages)
        self.report["applications"] = sorted(APPS[component] for component in self.packages if component in APPS)
        if "BUILD_INFO.txt" in self.manifest:
            info = env_document(self.raw("BUILD_INFO.txt"))
            if not VERSION.fullmatch(info.get("RELEASE_VERSION", "")) or info.get("TARGET_PLATFORM") != "linux/amd64":
                raise Invalid("build information has no supported release/platform")
            if any(key.endswith("_SHA256") and not HEX.fullmatch(value) for key, value in info.items()):
                raise Invalid("build information contains an invalid input digest")
            self.build_info = info
            self.report["release_version"] = info["RELEASE_VERSION"]
        else:
            versions = {package["version"] for component, package in self.packages.items() if component in APPS}
            if len(versions) > 1:
                raise Invalid("business packages mix release versions")
            self.report["release_version"] = next(iter(versions), self.packages["platform"]["version"])
        if any(package["version"] != self.report["release_version"] for component, package in self.packages.items() if component in APPS):
            raise Invalid("business package version differs from delivery release")
        self.current_id = "media.compatibility"
        compatibility = env_document(self.raw("PLATFORM_ASSETS_COMPATIBILITY.env"),
            {"FORMAT", "PLATFORM_ASSETS_INPUT_SHA256"} | {prefix+suffix for prefix in ("COMMON", "PLATFORM", "FRONTEND", "ASSETS") for suffix in ("_PACKAGE", "_PACKAGE_SHA256")})
        if len(compatibility) != 10 or compatibility.get("FORMAT") != "1" or not HEX.fullmatch(compatibility.get("PLATFORM_ASSETS_INPUT_SHA256", "")):
            raise Invalid("unsupported platform/assets compatibility lock")
        for key, component in (("COMMON", "common"), ("PLATFORM", "platform"), ("FRONTEND", "frontend"), ("ASSETS", None)):
            name = compatibility.get(key+"_PACKAGE", "")
            if name not in self.manifest or self.actual[name] != compatibility.get(key+"_PACKAGE_SHA256"):
                raise Invalid("platform/assets lock does not bind the actual package")
            if component is None:
                if name != asset_names[0]:
                    raise Invalid("assets lock selects a different asset package")
            elif self.packages[component]["version"] not in name and component != "common":
                raise Invalid("platform/assets lock selects a different component version")
        self.check("media.compatibility", "PASS", "MEDIA", "基础平台、前端、公共包与资产兼容锁绑定一致")
        self.current_id = "assets.profiles"
        with self.media.open(asset_names[0]) as data:
            self.assets = asset_contents(data)
        parser = sibling("subsystem-runtime.py")
        for app, filename in PROFILES.items():
            path = "subsystems.d/"+filename
            if path not in self.assets:
                raise Invalid("deployment assets lack a required production profile")
            self.projections[app] = parser.projection(self.assets[path].decode("utf-8"), app)
        self.report["consumer_count"] = sum(len(self.projections[app][0]["runtime_services"])-int(app == "data_analysis") for app in self.report["applications"])
        self.check("assets.profiles", "PASS", "MEDIA", "六份生产清单可解析，运行组件数量来自实际清单")
        self.current_id = "release.structure"
        release = sibling("delivery-inspect-v2.py")
        try:
            self.registration_protocols = release.validate(self, IMAGES, PROFILES, APP_COMPONENT)
        except release.Invalid as error:
            raise Invalid(str(error)) from error
        if self.registration_protocols:
            self.check("release.structure", "PASS", "MEDIA", "发布清单、应用组件、真实镜像、六份清单与总校验字节一致；尚未验签")
            self.check("authority.release-signature", "PENDING", "VERIFIER", "发布签名和构建证明须由介质外受信验证器核验，结构通过不能建立发布信任", False)
            self.action("VERIFIER", "VERIFY_RELEASE_INTEGRITY", "使用独立受信验证器与信任根核验原始介质签名和构建证明")
        self.current_id = "assets.recovery-tools"
        if all("bin/"+name in self.assets for name in RECOVERY_TOOLS):
            self.check("assets.recovery-tools", "PASS", "MEDIA", "统一检查与分阶段恢复工具已完整进入资产包")
        else:
            self.pending("assets.recovery-tools", "MEDIA", "旧资产缺少新版分阶段恢复工具，不能套用新版安装入口", True)
            self.action("MAC", "REBUILD_MATCHING_BASELINE", "使用新批次重新制作匹配平台与资产；原介质继续配套原安装入口，不修改原包摘要")
        if "customer_portal" in self.report["applications"] and "customer_and_opportunity" not in self.report["applications"]:
            # 已有 CRM 仅在声明扩容时可满足此条件，下方 authorization() 检查该确切场景。
            self.portal_needs_existing = True
        else:
            self.portal_needs_existing = False
        if self.raw("SHA256SUMS") != manifest_raw:
            raise Invalid("delivery manifest changed during inspection")

    def image(self, app, service):
        package = self.packages.get(APP_COMPONENT[app])
        if package is None:
            return None
        images = list(package["images"].values())
        if app == "data_analysis":
            if service == "data-analysis-metabase":
                return self.packages["common"]["images"]["metabase/metabase:v0.53.7"]
            index = {"data-analysis-api": 0, "data-analysis-aggregation-worker": 1, "data-analysis-alert-worker": 2}.get(service)
            if index is None:
                raise Invalid("unrecognized analytics runtime service")
            return images[index]
        return images[0]

    def release(self, app, raw):
        release = json_object(raw, {"version", "application", "environment", "components", "release_generation", "retire_service_ids"})
        if release.get("application") != app or release.get("environment") != "prod" or not isinstance(release.get("components"), list):
            raise Invalid("runtime release application/environment mismatch")
        expected = set(self.projections[app][0]["runtime_services"])-{"data-analysis-metabase"}
        components = {}
        coverage = set()
        for item in release["components"]:
            if (not isinstance(item, dict) or set(item) != {"service_id", "protocol", "coverage_digest", "image_digest"}
                    or item.get("service_id") not in expected or item["service_id"] in components or type(item.get("protocol")) is not int or item["protocol"] != 1
                    or not isinstance(item.get("coverage_digest"), str) or not DIGEST.fullmatch(item["coverage_digest"])
                    or not isinstance(item.get("image_digest"), str) or not DIGEST.fullmatch(item["image_digest"])):
                raise Invalid("runtime release has unknown, duplicate or invalid components")
            fact = self.image(app, item["service_id"])
            if fact is not None and item["image_digest"] not in fact["identities"]:
                raise Invalid("runtime release image is not proved by this delivery")
            coverage.add(item["coverage_digest"])
            components[item["service_id"]] = item
        if set(components) != expected or len(coverage) != 1:
            raise Invalid("runtime release is missing a required worker or mixes coverage generations")
        generation = release.get("release_generation", "")
        retire = release.get("retire_service_ids", [])
        if (not isinstance(generation, str) or generation and not SERVICE.fullmatch(generation) or not isinstance(retire, list)
                or len(retire) > 32 or any(not isinstance(item, str) or not SERVICE.fullmatch(item) for item in retire) or len(set(retire)) != len(retire)):
            raise Invalid("invalid runtime release generation or retirement list")
        prefix = {"customer_and_opportunity": "customer-", "customer_portal": "portal-", "contract_management": "contract-", "project_management": "project-"}.get(app, app.replace("_", "-")+"-")
        conditional = self.projections[app][1]
        if any(not item.startswith(prefix) or item in expected and item not in conditional or item == "data-analysis-metabase" for item in retire):
            raise Invalid("runtime retirement crosses application or required service boundary")
        return components

    def evidence(self, raw, releases, *, migration=False, required_apps=None):
        evidence = json_object(raw, {"version", "project", "installation_boundary", "services", "infrastructure", "excluded_services"})
        if (not isinstance(evidence.get("project"), str) or not IDENTIFIER.fullmatch(evidence["project"]) or evidence.get("installation_boundary") is not True
                or not isinstance(evidence.get("services"), list) or not isinstance(evidence.get("excluded_services"), list)
                or not isinstance(evidence.get("infrastructure", []), list) or len(evidence["services"]) > 64):
            raise Invalid("invalid approved installation evidence")
        observed, provided_apps = set(), set()
        for item in evidence["services"]:
            if not isinstance(item, dict) or set(item) != {"application", "environment", "name", "image_digest", "version", "protocol", "replicas"}:
                raise Invalid("invalid approved business evidence fields")
            app, service = item["application"], item["name"]
            if (app not in self.projections or service not in self.projections[app][0]["runtime_services"] or service == "data-analysis-metabase"
                    or item["environment"] != "prod" or service in observed or not isinstance(item["image_digest"], str) or not DIGEST.fullmatch(item["image_digest"])
                    or type(item["replicas"]) is not int or not 1 <= item["replicas"] <= 64 or not isinstance(item["version"], str) or not isinstance(item["protocol"], str)):
                raise Invalid("invalid approved business evidence identity")
            if not migration:
                component = releases.get(app, {}).get(service)
                fact = self.image(app, service)
                if (app not in set(self.report["applications"]) | releases.keys() or component is not None and component["image_digest"] != item["image_digest"]
                        or item["protocol"] != "1" or not IDENTIFIER.fullmatch(item["version"])):
                    raise Invalid("target evidence and approved release disagree")
                if fact is not None and (item["version"] != fact["version"] or item["image_digest"] not in fact["identities"]):
                    raise Invalid("target evidence version differs from actual image metadata")
            elif item["version"] and not IDENTIFIER.fullmatch(item["version"]) or migration and item["protocol"] not in ("", "1"):
                raise Invalid("invalid old-image version or protocol declaration")
            observed.add(service)
            provided_apps.add(app)
        target_apps = set(self.report["applications"]) | releases.keys()
        evidence_apps = target_apps if required_apps is None else set(required_apps) | provided_apps
        if not migration and observed != {service for app in evidence_apps for service in self.projections[app][0]["runtime_services"] if service != "data-analysis-metabase"}:
            raise Invalid("target evidence does not cover the complete approved release")
        infra_seen = set()
        for item in evidence.get("infrastructure", []):
            if (not isinstance(item, dict) or set(item) != {"application", "environment", "name", "image_digest", "replicas", "guarded_by"}
                    or item.get("application") != "data_analysis" or item.get("environment") != "prod" or item.get("name") != "data-analysis-metabase"
                    or item.get("guarded_by") != "data-analysis-api" or item["name"] in infra_seen or not isinstance(item.get("image_digest"), str)
                    or not DIGEST.fullmatch(item["image_digest"]) or type(item.get("replicas")) is not int or not 1 <= item["replicas"] <= 64):
                raise Invalid("invalid controlled infrastructure evidence")
            if "data-analysis-api" not in observed:
                raise Invalid("controlled analytics infrastructure lacks its gateway evidence")
            fact = self.image("data_analysis", "data-analysis-metabase")
            if not migration and fact is not None and item["image_digest"] not in fact["identities"]:
                raise Invalid("Metabase evidence does not match the delivered image")
            infra_seen.add(item["name"])
        if not migration and "data_analysis" in evidence_apps and infra_seen != {"data-analysis-metabase"}:
            raise Invalid("target evidence lacks controlled Metabase infrastructure")
        excluded = evidence["excluded_services"]
        nonbusiness = set(CORE)
        for app, (compose, _, _) in self.projections.items():
            nonbusiness.update(compose.get("initialization_services", []))
            nonbusiness.update(service for field in ("migrate_service", "database_service") if (service := compose.get(field)))
            content = self.assets["subsystems.d/"+PROFILES[app]].decode("utf-8")
            match = re.search(r"^  dependency_services: \[([^\]]*)\]", content, re.MULTILINE)
            if match:
                nonbusiness.update(value.strip() for value in match[1].split(","))
        business = {service for compose, _, _ in self.projections.values() for service in compose["runtime_services"]}
        if (any(not isinstance(item, str) or item not in nonbusiness or item in business for item in excluded)
                or len(set(excluded)) != len(excluded) or len(excluded) > 128):
            raise Invalid("installation exclusions contain unknown or business services")
        return evidence

    def license_claims(self, raw, applications, customer):
        if len(raw) > 256*1024:
            raise Invalid("license document exceeds limit")
        try:
            token = raw.decode("ascii").strip()
            parts = token.split(".")
            if len(parts) != 3 or any(not re.fullmatch(r"[A-Za-z0-9_-]+", part) for part in parts):
                raise Invalid("invalid JWS structure")
            decoded = [base64.urlsafe_b64decode(part+"="*((-len(part)) % 4)) for part in parts]
            header, claims = IMAGES.document(decoded[0]), IMAGES.document(decoded[1])
            if (not isinstance(header, dict) or set(header) != {"alg", "typ", "kid"} or header.get("alg") != "EdDSA"
                    or header.get("typ") != "commercial-license+jws" or not isinstance(header.get("kid"), str) or not IDENTIFIER.fullmatch(header["kid"]) or len(decoded[2]) != 64):
                raise Invalid("unsupported license header")
            allowed = {"protocol_version", "issuer", "license_id", "version", "customer_id", "product_id", "environment", "instance_id", "issued_at", "not_before", "applications"}
            if (not isinstance(claims, dict) or set(claims) != allowed or type(claims.get("protocol_version")) is not int or claims["protocol_version"] != 1
                    or claims.get("issuer") != "unified-business-suite-vendor" or claims.get("product_id") != "unified-business-suite"
                    or claims.get("environment") != "production" or any(not isinstance(claims.get(key), str) or not IDENTIFIER.fullmatch(claims[key]) for key in ("license_id", "customer_id", "instance_id"))
                    or type(claims.get("version")) is not int or not 1 <= claims["version"] <= 18446744073709551615
                    or type(claims.get("issued_at")) is not int or type(claims.get("not_before")) is not int or not isinstance(claims.get("applications"), list)
                    or not 0 < claims.get("issued_at", 0) <= claims.get("not_before", 0) <= 253402300799):
                raise Invalid("invalid public license claims")
            if customer is not None and customer != claims["customer_id"]:
                raise Invalid("license customer differs from the installation plan")
            licensed = set()
            for item in claims["applications"]:
                if (not isinstance(item, dict) or set(item) != {"code", "kind", "not_before", "expires_at"} or item.get("code") not in PROFILES
                        or item["code"] in licensed or item.get("kind") not in ("FULL", "TRIAL") or type(item.get("not_before")) is not int or type(item.get("expires_at")) is not int
                        or not 0 < item["not_before"] < item["expires_at"] <= 253402300799):
                    raise Invalid("invalid public application entitlement claims")
                licensed.add(item["code"])
            if not licensed or not licensed <= applications:
                raise Invalid("license declares an application without approved delivery scope")
        except (ValueError, TypeError, UnicodeError, OverflowError) as error:
            raise Invalid("license declaration structure is invalid") from error

    def authorization(self):
        self.current_id, self.current_scope = "authorization.plan", "AUTHORIZATION"
        selected = set(self.report["applications"])
        builtin = {app for app, protocol in self.registration_protocols.items() if protocol == 2}
        legacy_selected = selected - builtin
        plan = None
        for key, path in (("LICENSE_SHA256", "license/commercial-license.jws"), ("INSTALLATION_PLAN_SHA256", "license-installation.json")):
            if key in self.build_info and path in self.assets and hashlib.sha256(self.assets[path]).hexdigest() != self.build_info[key]:
                raise Invalid("authorization file differs from its immutable build binding")
        if "license-installation.json" in self.assets:
            plan = json_object(self.assets["license-installation.json"], {"version", "scenario", "customer_id"})
            if plan.get("scenario") not in {"fresh", "migrate", "platform-only", "expand"} or not isinstance(plan.get("customer_id"), str) or not IDENTIFIER.fullmatch(plan["customer_id"]):
                raise Invalid("invalid explicit installation scenario")
            self.check("authorization.plan", "PASS", "AUTHORIZATION", "安装场景与客户声明格式有效")
        self.declared = plan is not None or "license/commercial-license.jws" in self.assets or bool({"LICENSE_SHA256", "RUNTIME_APPROVAL_SHA256", "INSTALLATION_PLAN_SHA256"} & self.build_info.keys())
        if plan is None and "INSTALLATION_PLAN_SHA256" in self.build_info:
            self.pending("authorization.plan", "AUTHORIZATION", "构建声明的安装计划尚未随资产提供", True)
        existing_allowed = plan is not None and plan["scenario"] in {"migrate", "platform-only", "expand"}
        if self.portal_needs_existing and not existing_allowed:
            raise Invalid("Portal package lacks its required CRM dependency")
        releases = {}
        for app in PROFILES:
            name = "runtime-license-"+app+"-prod.json"
            if name in self.assets:
                self.current_id = "authorization.release."+app
                if app not in selected and not existing_allowed:
                    raise Invalid("unrequested business release is mixed into the delivery")
                releases[app] = self.release(app, self.assets[name])
                self.check("authorization."+("legacy-release." if app in builtin else "release.")+app, "PASS", app, "运行批准、完整组件、代次和镜像身份一致")
        for app in sorted(builtin):
            self.check("authorization.release."+app, "PASS", app, "V2目录组件、完整生产清单与实际镜像一致；无需重制目标V1运行批准")
        for app in sorted(legacy_selected-releases.keys()):
            self.pending("authorization.release."+app, app, "缺少本系统已审核的完整运行批准", True)
        if existing_allowed and releases.keys()-selected:
            self.check("authority.existing-images", "PENDING", "SERVER", "既有系统镜像未随本包交付，须在目标服务器核验", False)
        if selected or releases or self.declared:
            if "license-evidence.json" in self.assets:
                self.current_id = "authorization.evidence"
                evidence = self.evidence(self.assets["license-evidence.json"], releases, required_apps=legacy_selected | releases.keys())
                self.check("authorization.evidence", "PASS", "AUTHORIZATION", "目标安装证据覆盖完整批准，业务排除边界一致")
            else:
                evidence = None
                if legacy_selected or releases:
                    self.pending("authorization.evidence", "AUTHORIZATION", "缺少目标安装证据", True)
            if plan is not None:
                if "license-migration-evidence.json" not in self.assets:
                    self.pending("authorization.baseline", "AUTHORIZATION", "显式安装场景缺少独立旧安装边界材料", True)
                else:
                    self.current_id = "authorization.baseline"
                    baseline = self.evidence(self.assets["license-migration-evidence.json"], {}, migration=True)
                    if evidence is not None and baseline["project"] != evidence["project"]:
                        raise Invalid("old and target evidence refer to different Compose projects")
                    if plan["scenario"] in {"fresh", "platform-only"} and (baseline["services"] or baseline.get("infrastructure")):
                        raise Invalid("empty-baseline scenario contains existing business claims")
                    self.check("authorization.baseline", "PASS", "AUTHORIZATION", "旧安装材料与所声明场景一致，实际运行事实仍待服务器核验")
            elif "license-migration-evidence.json" in self.assets:
                self.evidence(self.assets["license-migration-evidence.json"], {}, migration=True)
            if "license/commercial-license.jws" in self.assets:
                self.current_id = "authorization.license"
                self.license_claims(self.assets["license/commercial-license.jws"], selected | releases.keys(), plan["customer_id"] if plan else None)
                self.check("authorization.license", "PASS", "AUTHORIZATION", "JWS公开声明结构与交付范围一致；尚未验证签名")
            else:
                self.pending("authorization.license", "AUTHORIZATION", "尚未提供实例绑定许可证，可先安装基础平台并导出真实申请", "LICENSE_SHA256" in self.build_info)
            self.check("authority.platform", "PENDING", "PLATFORM", "签名、真实实例、目录与迁移资格、运行条件和当前全体ACK仍需平台权威核验", False)
            if legacy_selected or releases:
                self.action("MAC", "REVIEW_RUNTIME_APPROVALS", "按本次实际镜像与生产清单准备并审核运行批准")
            self.action("SERVER", "PREPARE_BASELINE", "核对真实部署事实，按已声明场景冻结安装边界")
            self.action("PLATFORM", "VERIFY_LICENSE_AND_ACK", "导入匹配真实实例的许可证，完成受控接入及所有当前确认")
        else:
            self.check("authorization.scope", "PASS", "AUTHORIZATION", "仅交付基础平台，未声明业务授权或存量资格", False)

    def run(self):
        try:
            self.structure()
            self.authorization()
            self.media.verify()
            self.report["status"] = "NEEDS_INPUT" if self.material_pending else "READY"
            return self.report, 3 if self.phase == "install" and self.declared and self.required_missing else 0
        except (Invalid, OSError, ValueError, TypeError, KeyError, AttributeError, tarfile.TarError, UnicodeError, RecursionError) as error:
            detail = str(error) if isinstance(error, Invalid) else "unsupported or inconsistent delivery structure"
            self.check(self.current_id, "FAIL", self.current_scope, "配套检查未通过："+detail, True)
            self.report["status"] = "INVALID"
            self.action("MAC", "REBUILD_OR_RESTORE_DELIVERY", "恢复原始介质或使用新版本完整构建，禁止改写既有摘要绕过检查")
            return self.report, 2


def inspect(media, phase="prepare"):
    instance = None
    try:
        path = Path(os.path.abspath(media))
        instance = DirectoryMedia(path) if path.is_dir() else ArchiveMedia(path)
        return Inspection(instance, phase).run()
    except (Invalid, OSError, ValueError, TypeError, tarfile.TarError, UnicodeError):
        report = Inspection(None, phase).report
        report["checks"] = [{"id": "media.open", "status": "FAIL", "scope": "MEDIA", "message": "交付路径、外层归档或伴随校验不安全或无效", "required_for_install": True}]
        report["next_actions"] = [{"location": "MAC", "action": "RESTORE_ORIGINAL_MEDIA", "message": "核对交付文件及对应校验，使用原始完整介质"}]
        return report, 2
    finally:
        if instance is not None:
            instance.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--media", required=True)
    parser.add_argument("--format", choices=("human", "json"), default="human")
    parser.add_argument("--phase", choices=("prepare", "install"), default="prepare")
    args = parser.parse_args()
    report, code = inspect(args.media, args.phase)
    if args.format == "json":
        print(json.dumps(report, ensure_ascii=False, separators=(",", ":")))
    else:
        print("交付状态："+report["status"]+"（READY仅表示结构配套通过）")
        print("交付标识："+(report["delivery_id"] or "未确认"))
        print("发布版本："+(report["release_version"] or "未确认"))
        print("应用："+(", ".join(report["applications"]) or "基础平台"))
        print("清单运行消费者："+str(report["consumer_count"]))
        for check in report["checks"]:
            print("["+check["status"]+"] "+check["id"]+"："+check["message"])
        for action in report["next_actions"]:
            print(action["location"]+"："+action["message"])
    return code


if __name__ == "__main__":
    sys.exit(main())
