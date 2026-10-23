#!/usr/bin/env python3
"""使用确切的离线包与本地不可变对象，证明一次离线导入。

仅导入已安装且已审核的包检查器，不从交付介质加载代码。
外部操作仅限 Docker 检查和本机回环 Registry 读取；
此工具不能拉取、标记、创建或启动任何对象。
"""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys


sys.dont_write_bytecode = True
MAX_METADATA = 16 * 1024 * 1024
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
VERSION = re.compile(r"[A-Za-z0-9._-]+\Z")
COMPONENTS = {
    "common", "platform", "frontend", "customer-opportunity", "customer-portal",
    "contract", "project", "settlement", "data-analysis",
}
INDEX_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
MANIFEST_TYPES = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
}
CONFIG_TYPES = {
    "application/vnd.oci.image.config.v1+json",
    "application/vnd.docker.container.image.v1+json",
}
BI_KEYS = (
    "DATA_ANALYSIS_DASHBOARD_API_IMAGE", "DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE",
    "DATA_ANALYSIS_ALERT_WORKER_IMAGE", "DATA_ANALYSIS_MIGRATE_IMAGE",
)


class ProofError(ValueError):
    pass


def reject(message):
    raise ProofError(message)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            reject("JSON contains duplicate properties")
        result[key] = value
    return result


def document(raw):
    def invalid_constant(_):
        reject("JSON contains non-finite numbers")
    result = json.loads(raw, object_pairs_hook=unique_object, parse_constant=invalid_constant)
    if not isinstance(result, dict):
        reject("Registry metadata is not an object")
    return result


def regular(path, maximum=256 * 1024):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_size > maximum:
        reject("unsafe or oversized evidence file")
    return path.read_bytes()


def environment_file(path):
    result = {}
    for line in regular(path).decode("utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Z_][A-Z0-9_]*", key) or key in result or "\x00" in value:
            reject("invalid or duplicate import-record field")
        result[key] = value
    return result


def checked_output(arguments, maximum=MAX_METADATA):
    try:
        completed = subprocess.run(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, timeout=20, check=False)
    except (OSError, subprocess.TimeoutExpired):
        reject("local evidence command is unavailable or timed out")
    if completed.returncode or len(completed.stdout) > maximum:
        reject("local evidence is missing, unreadable or oversized")
    return completed.stdout


def native_identity(reference):
    output = checked_output([
        "docker", "image", "inspect", reference, "--format", "{{.Architecture}}/{{.Os}} {{.Id}}",
    ], 1024).decode("ascii").strip().split()
    if len(output) != 2 or output[0] != "amd64/linux" or not DIGEST.fullmatch(output[1]):
        reject("local image platform or native identity differs")
    return output[1]


class Registry:
    def __init__(self, address):
        match = re.fullmatch(r"127\.0\.0\.1:([0-9]{1,5})", address)
        if not match or not 1 <= int(match[1]) <= 65535:
            reject("only the explicit loopback offline Registry is supported")
        self.address = address
        self.metadata = {}
        self.layers = set()

    def request(self, repository, kind, digest, head=False):
        if not DIGEST.fullmatch(digest):
            reject("invalid Registry content digest")
        arguments = [
            "curl", "--noproxy", "*", "--proto", "=http", "--max-redirs", "0",
            "--fail", "--silent", "--show-error", "--connect-timeout", "2", "--max-time", "10",
        ]
        if head:
            arguments += ["--head", "--output", "/dev/null"]
        else:
            arguments += ["--max-filesize", str(MAX_METADATA)]
        if kind == "manifests":
            arguments += ["--header", "Accept: " + ", ".join(sorted(INDEX_TYPES | MANIFEST_TYPES))]
        arguments.append("http://" + self.address + "/v2/" + repository + "/" + kind + "/" + digest)
        return checked_output(arguments)

    def read(self, repository, kind, digest, size=None):
        key = repository, kind, digest
        if key not in self.metadata:
            raw = self.request(repository, kind, digest)
            if "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
                reject("Registry metadata differs from its immutable digest")
            self.metadata[key] = raw
        raw = self.metadata[key]
        if size is not None and (type(size) is not int or not 0 < size <= MAX_METADATA or len(raw) != size):
            reject("Registry descriptor size differs")
        return document(raw)

    def descriptor(self, value, allowed):
        if not isinstance(value, dict) or value.get("mediaType") not in allowed or not isinstance(value.get("digest"), str) or not DIGEST.fullmatch(value["digest"]):
            reject("invalid Registry descriptor")
        return value["digest"], value.get("size"), value["mediaType"]

    def walk(self, repository, digest, size=None, media=None, depth=0, visited=None):
        visited = [0] if visited is None else visited
        visited[0] += 1
        if depth >= 8 or visited[0] > 128:
            reject("Registry identity graph exceeds limits")
        value = self.read(repository, "manifests", digest, size)
        if type(value.get("schemaVersion")) is not int or value["schemaVersion"] != 2:
            reject("unsupported Registry manifest schema")
        actual_media = value.get("mediaType", media)
        if actual_media is None:
            actual_media = next(iter(INDEX_TYPES)) if "manifests" in value else next(iter(MANIFEST_TYPES))
        if actual_media not in INDEX_TYPES | MANIFEST_TYPES or (media is not None and actual_media != media):
            reject("Registry manifest media type differs")
        if actual_media in INDEX_TYPES:
            children = value.get("manifests")
            if not isinstance(children, list):
                reject("invalid Registry image index")
            result = []
            for child in children:
                child_digest, child_size, child_media = self.descriptor(child, INDEX_TYPES | MANIFEST_TYPES)
                platform = child.get("platform")
                if platform is not None:
                    if not isinstance(platform, dict):
                        reject("invalid Registry platform descriptor")
                    if platform.get("os") != "linux" or platform.get("architecture") != "amd64" or platform.get("variant", ""):
                        continue
                for config, identities in self.walk(repository, child_digest, child_size, child_media, depth + 1, visited):
                    result.append((config, identities | {digest}))
                    if len(result) > 1:
                        reject("Registry has multiple amd64 manifests")
            return result
        config_digest, config_size, _ = self.descriptor(value.get("config"), CONFIG_TYPES)
        config = self.read(repository, "blobs", config_digest, config_size)
        if config.get("os") != "linux" or config.get("architecture") != "amd64" or config.get("variant", ""):
            return []
        layers = value.get("layers")
        if not isinstance(layers, list):
            reject("Registry image has no layer descriptors")
        for layer in layers:
            if not isinstance(layer, dict) or not isinstance(layer.get("digest"), str) or not DIGEST.fullmatch(layer["digest"]) or type(layer.get("size")) is not int or layer["size"] < 0:
                reject("invalid Registry layer descriptor")
            layer_key = repository, layer["digest"]
            if layer_key not in self.layers:
                self.request(repository, "blobs", layer["digest"], head=True)
                self.layers.add(layer_key)
        return [(config_digest, {digest, config_digest})]

    def prove(self, reference, repository, expected, source_identities):
        prefix = self.address + "/" + repository + "@"
        if not reference.startswith(prefix) or not DIGEST.fullmatch(reference[len(prefix):]):
            reject("imported reference is outside its exact loopback repository")
        selected = self.walk(repository, reference[len(prefix):])
        if len(selected) != 1 or selected[0][0] != expected:
            reject("Registry image config differs from the reviewed package")
        identities = selected[0][1] | set(source_identities) | {expected}
        native = native_identity(reference)
        if native not in identities:
            reject("local immutable image is not proved by the reviewed identity graph")
        return native, sorted(identities)


def inspect_package(archive):
    inspector = Path(__file__).resolve().with_name("delivery-inspect.py")
    if not inspector.is_file() or inspector.is_symlink():
        reject("reviewed delivery inspector is missing or unsafe")
    specification = importlib.util.spec_from_file_location("uip_reviewed_delivery_inspector", inspector)
    module = importlib.util.module_from_spec(specification)
    sys.modules[specification.name] = module
    specification.loader.exec_module(module)
    result = module.read_component_package(archive)
    if not isinstance(result, dict) or result.get("component") not in COMPONENTS or not VERSION.fullmatch(result.get("version", "")) or result.get("platform") != "linux/amd64" or not re.fullmatch(r"[0-9a-f]{64}", result.get("sha256", "")):
        reject("reviewed package facts are invalid")
    sidecar = regular(Path(str(archive) + ".sha256"), 4096).decode("ascii").splitlines()
    fields = sidecar[0].split() if len(sidecar) == 1 else []
    if len(fields) != 2 or fields[0].lower() != result["sha256"] or fields[1] != archive.name:
        reject("package sidecar does not bind the exact archive")
    images = result.get("images")
    if not isinstance(images, dict) or not images:
        reject("package has no paired image facts")
    for image in images.values():
        identities = image.get("identities") if isinstance(image, dict) else None
        if not isinstance(identities, list) or not identities or any(not isinstance(value, str) or not DIGEST.fullmatch(value) for value in identities) or not DIGEST.fullmatch(image.get("config_digest", "")):
            reject("package image identity graph is invalid")
    return result


def prove_import(target, component, archive, address):
    if target.is_symlink() or not target.is_dir() or archive.is_symlink() or not archive.is_file():
        reject("deployment target or package is unsafe")
    target = target.resolve()
    package = inspect_package(archive)
    if package["component"] != component:
        reject("package belongs to another component")
    directory = target / "manifests"
    if directory.is_symlink() or not directory.is_dir():
        reject("import-record directory is unsafe or missing")
    filename = component + "-" + package["version"] + ".imported"
    pointer = regular(directory / (component + ".latest"), 4096).decode("ascii")
    if pointer != filename + "\n":
        reject("latest pointer is not the exact reviewed package")
    record = environment_file(directory / filename)
    images = list(package["images"].items())
    configs = [item[1]["config_digest"] for item in images]
    for key, expected in {
        "COMPONENT": component, "VERSION": package["version"], "PACKAGE_SHA256": package["sha256"],
        "SOURCE_IMAGE_IDS": ",".join(configs),
    }.items():
        if record.get(key) != expected:
            reject("import record does not match the reviewed package: " + key)
    registry = Registry(address)
    result = {
        "component": component, "version": package["version"], "package_sha256": package["sha256"],
        "record": filename, "image_ref": record.get("IMAGE_REF", ""), "native_ids": {}, "acceptable_ids": {},
    }

    def prove(reference, repository, image):
        native, identities = registry.prove(reference, repository, image["config_digest"], image["identities"])
        result["native_ids"][reference] = native
        result["acceptable_ids"][reference] = identities

    if component == "common":
        if record.get("IMAGE_REF") != ",".join(tag for tag, _ in images) or record.get("IMAGE_DIGEST") != "-":
            reject("common import image list differs")
        for tag, image in images:
            if native_identity(tag) not in set(image["identities"]) | {image["config_digest"]}:
                reject("common local image does not match the reviewed archive")
        gateway = package["images"].get("uip-package/file-gateway:" + package["version"])
        if gateway is None:
            reject("common package has no File Gateway image")
        gateway_ref = record.get("FILE_GATEWAY_IMAGE_REF", "")
        prove(gateway_ref, "uip/file-gateway", gateway)
        result["file_gateway_image_ref"] = gateway_ref
        metadata = package.get("metadata")
        if not isinstance(metadata, dict):
            reject("package provenance metadata is missing")
        has_proxy = bool(metadata.get("DOCKER_SOCKET_PROXY_UPSTREAM_REF") or metadata.get("DOCKER_SOCKET_PROXY_CONFIG_DIGEST"))
        proxy_ref = record.get("DOCKER_SOCKET_PROXY_IMAGE_REF", "")
        if has_proxy or proxy_ref:
            proxy = package["images"].get("tecnativa/docker-socket-proxy:v0.5.0")
            if proxy is None or (has_proxy and not proxy_ref):
                reject("common proxy import is incomplete")
            prove(proxy_ref, "uip/docker-socket-proxy", proxy)
            result["docker_socket_proxy_image_ref"] = proxy_ref
    elif component == "data-analysis":
        if len(images) != 4:
            reject("data-analysis import is missing one of its four images")
        result["release_bindings"] = {}
        for key, (tag, image) in zip(BI_KEYS, images):
            reference = record.get(key, "")
            prove(reference, "uip/" + tag.rsplit("/", 1)[1].split(":", 1)[0], image)
            result["release_bindings"][key] = reference
        if result["image_ref"] != result["release_bindings"][BI_KEYS[0]]:
            reject("data-analysis main reference differs from its API")
    else:
        if len(images) != 1:
            reject("application package has multiple image facts")
        prove(result["image_ref"], "uip/" + component, images[0][1])
    if component != "common" and record.get("IMAGE_DIGEST") != result["image_ref"].rsplit("@sha256:", 1)[-1]:
        reject("import-record digest differs from its immutable reference")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True)
    parser.add_argument("--component", choices=sorted(COMPONENTS), required=True)
    parser.add_argument("--archive", required=True)
    parser.add_argument("--registry", default="127.0.0.1:5000")
    arguments = parser.parse_args()
    try:
        result = prove_import(Path(os.path.abspath(arguments.target)), arguments.component,
                              Path(os.path.abspath(arguments.archive)), arguments.registry)
    except ProofError as error:
        print("交付运行证明不匹配：" + str(error), file=sys.stderr)
        return 1
    except Exception:
        # 介质无效、本地依赖不可用或检查器失败都会拒绝检查点，且不暴露任意归档内容。
        print("交付运行证明不匹配：包、精确导入记录、本地镜像或离线 Registry 证据缺失/不一致。", file=sys.stderr)
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
