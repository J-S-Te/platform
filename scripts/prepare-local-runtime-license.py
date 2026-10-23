#!/usr/bin/env python3
"""审核本地开发源码指纹与确切的 Docker 镜像，不授予授权。

此处的覆盖摘要仅标识开发源码字节，不证明生产入口分类门禁已完整覆盖，
也不替代生产运行批准。
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile


LIMIT = 65536
IDENTIFIER = re.compile(r"^[a-z][a-z0-9_-]{0,63}$")
VERSION = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
IMAGE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$")
DIGEST = re.compile(r"^sha256:[a-f0-9]{64}$")
DOCUMENT_KEYS = {"version", "project", "application", "environment", "operator", "reviewed", "compose_sha256", "source_digest", "components", "services", "migration"}


class PreparationError(Exception):
    pass


def fail(message):
    raise PreparationError(message)


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def safe_chain(path):
    path = Path(os.path.abspath(path))
    for part in [*reversed(path.parents), path]:
        try:
            info = part.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode):
            fail("symbolic links are not permitted in trusted paths")
        if part != path and not stat.S_ISDIR(info.st_mode):
            fail("trusted parent path is not a directory")
    return path


def read_regular(path, limit=LIMIT, private=False):
    path = safe_chain(path)
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        fail("required trusted file is unavailable")
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            fail("trusted file is not regular or exceeds its size limit")
        if private and info.st_mode & 0o077:
            fail("existing approval file must have private permissions")
        data = stream.read(limit + 1)
        if len(data) > limit:
            fail("trusted file exceeds its size limit")
        return data


def parse_json(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                fail("duplicate JSON fields are not permitted")
            result[key] = value
        return result
    try:
        return json.loads(raw, object_pairs_hook=unique)
    except (ValueError, UnicodeError):
        fail("trusted JSON file could not be parsed")


def load_manifest(workspace):
    manifest = parse_json(read_regular(workspace / "platform/config/local-license-components.json"))
    if not isinstance(manifest, dict) or set(manifest) != {"version", "applications"} or type(manifest["version"]) is not int or manifest["version"] != 1:
        fail("local component manifest schema is invalid")
    apps = manifest["applications"]
    if not isinstance(apps, dict) or not apps or len(apps) > 6:
        fail("local component manifest applications are invalid")
    all_services = set()
    for app, target in apps.items():
        if not IDENTIFIER.fullmatch(app) or not isinstance(target, dict) or set(target) != {"directory", "services", "migration"}:
            fail("local component manifest application is invalid")
        directory, services = target["directory"], target["services"]
        if not isinstance(directory, str) or not directory or Path(directory).is_absolute() or any(p in (".", "..", "") for p in directory.split("/")) or "\\" in directory:
            fail("application directory must remain inside the workspace")
        if not isinstance(services, dict) or not services or len(services) > 32:
            fail("local component manifest service set is invalid")
        for name, image in services.items():
            if not IDENTIFIER.fullmatch(name) or name in all_services or not isinstance(image, str) or not IMAGE.fullmatch(image):
                fail("local component manifest service or image is invalid")
            all_services.add(name)
        migration = target["migration"]
        if not isinstance(migration, dict) or set(migration) != {"service", "image"} or not isinstance(migration["service"], str) or not IDENTIFIER.fullmatch(migration["service"]) or migration["service"] in services or not isinstance(migration["image"], str) or not IMAGE.fullmatch(migration["image"]):
            fail("local migration image manifest is invalid")
    return apps


def source_digest(workspace, directory):
    root = safe_chain(workspace / directory)
    if not root.is_dir():
        fail("application source directory is unavailable")
    files = []
    for name in ("cmd", "internal", "migrations", "authz", "third_party/license-core", "go.mod", "go.sum", "Dockerfile", "docker-entrypoint.sh"):
        target = safe_chain(root / name)
        if not target.exists():
            continue
        if target.is_file():
            files.append(target)
        elif target.is_dir():
            for directory_path, directories, names in os.walk(target, followlinks=False):
                for entry in directories + names:
                    candidate = safe_chain(Path(directory_path) / entry)
                    if not candidate.is_dir():
                        if not candidate.is_file():
                            fail("source contains a non-regular entry")
                        files.append(candidate)
        else:
            fail("source contains a non-regular entry")
    if not files or not (root / "go.mod").is_file() or not (root / "Dockerfile").is_file():
        fail("application source fingerprint is incomplete")
    combined = hashlib.sha256()
    for path in sorted(set(files), key=lambda p: p.relative_to(root).as_posix()):
        relative = path.relative_to(root).as_posix()
        # 流式读取源文件，不将其全部载入内存；此分帧格式与隔离 Go Agent 的批准复核共用。
        safe_chain(path)
        try:
            with os.fdopen(os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)), "rb") as stream:
                if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                    fail("source contains a non-regular entry")
                item = hashlib.sha256()
                while chunk := stream.read(1024 * 1024):
                    item.update(chunk)
        except OSError:
            fail("application source file could not be read")
        combined.update((relative + "\0sha256:" + item.hexdigest() + "\n").encode("utf-8"))
    return "sha256:" + combined.hexdigest()


def inspect_image(image):
    projection = '{"id":{{json .Id}},"version":{{json (index .Config.Labels "org.opencontainers.image.version")}},"protocol":{{json (index .Config.Labels "com.basic-platform.license.protocol")}},"architecture":{{json .Architecture}},"os":{{json .Os}}}'
    # 绝不请求 Config.Env 或显示 Docker 标准错误，两者都可能包含凭据。用临时输出文件限制内存占用，并始终移除。
    try:
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(["docker", "image", "inspect", "--format", projection, image], stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.DEVNULL, timeout=30, check=False)
            if result.returncode != 0 or output.tell() > LIMIT:
                fail("Docker image inspection failed or exceeded its output limit")
            output.seek(0)
            raw = output.read(LIMIT + 1)
    except (OSError, subprocess.TimeoutExpired):
        fail("Docker image inspection is unavailable or timed out")
    value = parse_json(raw)
    if not isinstance(value, dict) or set(value) != {"id", "version", "protocol", "architecture", "os"}:
        fail("Docker image inspection projection is invalid")
    if not isinstance(value["id"], str) or not DIGEST.fullmatch(value["id"]) or not isinstance(value["version"], str) or not VERSION.fullmatch(value["version"]) or value["protocol"] != "1" or value["os"] != "linux" or value["architecture"] not in {"amd64", "arm64", "arm", "386", "ppc64le", "s390x", "riscv64"}:
        fail("Docker image must have a valid immutable ID, version and license protocol 1")
    return value


def build_document(workspace, app, operator, reviewed=False):
    apps = load_manifest(workspace)
    if app not in apps:
        fail("application is not in the trusted local component manifest")
    if not isinstance(operator, str) or not operator.strip() or len(operator) > 128 or any(ord(c) < 32 or ord(c) == 127 for c in operator):
        fail("a nonempty operator name without control characters is required")
    target = apps[app]
    source = source_digest(workspace, target["directory"])
    coverage = digest(("dev-source-fingerprint\0" + app + "\0" + source).encode("utf-8"))
    document = {"version": 1, "project": "basic-platform-local", "application": app, "environment": "dev", "operator": operator, "reviewed": reviewed, "compose_sha256": digest(read_regular(workspace / "platform/compose.local.yaml", 2 * 1024 * 1024)), "source_digest": source, "components": [], "services": []}
    images = {}
    for name, image in sorted(target["services"].items()):
        if image not in images:
            images[image] = inspect_image(image)
        metadata = images[image]
        document["components"].append({"service_id": name, "protocol": 1, "coverage_digest": coverage, "image_digest": metadata["id"]})
        document["services"].append({"application": app, "environment": "dev", "name": name, "image_digest": metadata["id"], "version": metadata["version"], "protocol": "1", "replicas": 1})
    migration = target["migration"]
    if migration["image"] not in images:
        images[migration["image"]] = inspect_image(migration["image"])
    metadata = images[migration["image"]]
    document["migration"] = {"service": migration["service"], "image_digest": metadata["id"], "version": metadata["version"], "protocol": "1"}
    return document


def validate_document(value):
    if not isinstance(value, dict) or set(value) != DOCUMENT_KEYS or type(value["version"]) is not int or value["version"] != 1 or value["project"] != "basic-platform-local" or value["environment"] != "dev" or type(value["reviewed"]) is not bool:
        fail("existing local approval has an invalid schema")
    if not isinstance(value["application"], str) or not IDENTIFIER.fullmatch(value["application"]) or not isinstance(value["operator"], str) or not value["operator"].strip():
        fail("existing local approval has invalid identity")
    if any(not isinstance(value[k], str) or not DIGEST.fullmatch(value[k]) for k in ("compose_sha256", "source_digest")):
        fail("existing local approval has invalid digests")
    components, services = value["components"], value["services"]
    if not isinstance(components, list) or not 1 <= len(components) <= 32 or not isinstance(services, list) or len(services) != len(components):
        fail("existing local approval has invalid component sets")
    expected = {}
    for item in components:
        if not isinstance(item, dict) or set(item) != {"service_id", "protocol", "coverage_digest", "image_digest"} or not isinstance(item["service_id"], str) or not IDENTIFIER.fullmatch(item["service_id"]) or type(item["protocol"]) is not int or item["protocol"] != 1 or item["service_id"] in expected or any(not isinstance(item[k], str) or not DIGEST.fullmatch(item[k]) for k in ("coverage_digest", "image_digest")):
            fail("existing local approval has invalid component bindings")
        expected[item["service_id"]] = item["image_digest"]
    for item in services:
        if not isinstance(item, dict) or set(item) != {"application", "environment", "name", "image_digest", "version", "protocol", "replicas"} or item["application"] != value["application"] or item["environment"] != "dev" or not isinstance(item["name"], str) or expected.pop(item["name"], None) != item["image_digest"] or not isinstance(item["version"], str) or not VERSION.fullmatch(item["version"]) or item["protocol"] != "1" or type(item["replicas"]) is not int or item["replicas"] != 1:
            fail("existing local approval has invalid service bindings")
    migration = value["migration"]
    if not isinstance(migration, dict) or set(migration) != {"service", "image_digest", "version", "protocol"} or not isinstance(migration["service"], str) or not IDENTIFIER.fullmatch(migration["service"]) or migration["service"] in {c["service_id"] for c in components} or not isinstance(migration["image_digest"], str) or not DIGEST.fullmatch(migration["image_digest"]) or not isinstance(migration["version"], str) or not VERSION.fullmatch(migration["version"]) or migration["protocol"] != "1":
        fail("existing local approval has invalid migration image binding")


def publish(workspace, document, approve=False, replace=False):
    if replace and not approve:
        fail("--replace requires --approve")
    directory = safe_chain(workspace / "platform/runtime-approvals")
    if not directory.exists():
        directory.mkdir(mode=0o700)
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077:
        fail("local approval directory must be private (0700)")
    filename = "local-" + document["application"] + "-dev" + ("" if approve else ".candidate") + ".json"
    path = safe_chain(directory / filename)
    document = dict(document, reviewed=bool(approve))
    validate_document(document)
    if path.exists():
        previous = parse_json(read_regular(path, private=True))
        validate_document(previous)
        if approve and previous != document and not replace:
            fail("a different approval already exists; review it and use --approve --replace for a controlled update")
        if previous == document:
            return path
    raw = (json.dumps(document, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    fd, temporary = tempfile.mkstemp(prefix=".local-license-", dir=directory)
    try:
        with os.fdopen(fd, "wb") as output:
            os.fchmod(output.fileno(), 0o600)
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
        if approve and not replace:
            # 发布时不覆盖并发创建的批准文件；硬链接为原子操作，并保留临时文件的 0600 权限。
            try:
                os.link(temporary, path)
            except FileExistsError:
                fail("an approval appeared concurrently; review it before retrying")
        else:
            os.replace(temporary, path)
        fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return path


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workspace", type=Path, default=Path(__file__).absolute().parents[2])
    parser.add_argument("--application", required=True)
    parser.add_argument("--operator", required=True)
    parser.add_argument("--environment", choices=["dev"], default="dev")
    parser.add_argument("--project", choices=["basic-platform-local"], default="basic-platform-local")
    parser.add_argument("--approve", action="store_true", help="confirm the operator reviewed the displayed source and exact images")
    parser.add_argument("--replace", action="store_true", help="explicitly replace an existing approval; requires controlled deployment, preserves runtime state")
    args = parser.parse_args(argv)
    try:
        workspace = safe_chain(args.workspace)
        document = build_document(workspace, args.application, args.operator)
        print("DEV source fingerprint only; this is not a production coverage approval.")
        print("Application: " + document["application"] + " / dev; operator: " + document["operator"])
        print("Compose: " + document["compose_sha256"])
        print("Source: " + document["source_digest"])
        print("SERVICE | IMAGE ID | VERSION | PROTOCOL")
        for item in document["services"]:
            print(" | ".join([item["name"], item["image_digest"], item["version"], item["protocol"]]))
        migration = document["migration"]
        print("MIGRATION | " + " | ".join([migration["service"], migration["image_digest"], migration["version"], migration["protocol"]]))
        path = publish(workspace, document, args.approve, args.replace)
        print(("Reviewed approval: " if args.approve else "Unapproved candidate: ") + str(path))
        if args.replace:
            print("Replacement requires a controlled deployment update. No runtime state was changed.")
        return 0
    except (PreparationError, OSError) as error:
        print("ERROR: " + (str(error) if isinstance(error, PreparationError) else "local preparation filesystem operation failed"), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
