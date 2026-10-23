#!/usr/bin/env python3
"""从已验证的离线归档中解析已加载镜像的身份。

检查 containerd 镜像可能得到索引或清单摘要；仅在精确的标签描述符
能证明已审核的 amd64 配置时才接受。通过未压缩 tar 的定位读取小型元数据，
不复制镜像层内容。此功能仅为可选导入优化；不支持的归档回退到 docker save。
"""

import argparse
import hashlib
import json
import re
import sys
import tarfile

MAX_METADATA_BYTES = 16 * 1024 * 1024
MAX_MEMBERS = 100000
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
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


class InvalidArchive(ValueError):
    pass


class UnsupportedArchive(ValueError):
    pass


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidArchive("duplicate JSON property")
        result[key] = value
    return result


def reject_constant(_):
    raise InvalidArchive("non-finite JSON number")


def canonical_reference(value):
    if not isinstance(value, str) or not value or value.strip() != value:
        raise InvalidArchive("invalid image reference")
    # 仅有 OCI reference-name（例如 latest）不能证明仓库归属。
    if ":" not in value or "@" in value or any(c.isspace() for c in value):
        raise InvalidArchive("full tagged image reference required")
    if "/" not in value:
        return "docker.io/library/" + value
    first = value.split("/", 1)[0]
    if "." not in first and ":" not in first and first != "localhost":
        return "docker.io/" + value
    return value


class Archive:
    def __init__(self, path):
        # Docker save 是未压缩 tar。只跳过一次大体积层载荷，读取元数据时不得重新扫描或解压整个归档。
        self.tar = tarfile.open(path, "r:")
        self.members = {}
        try:
            for member in self.tar:
                name = member.name
                if name.startswith("./"):
                    name = name[2:]
                if name in ("", ".") and member.isdir():
                    continue
                if name.startswith("/") or "\\" in name or ".." in name.split("/"):
                    raise InvalidArchive("unsafe archive path")
                if not member.isfile() and not member.isdir():
                    raise InvalidArchive("linked or special archive member")
                if name in self.members:
                    raise InvalidArchive("duplicate archive member")
                self.members[name] = member
                if len(self.members) > MAX_MEMBERS:
                    raise InvalidArchive("too many archive members")
        except Exception:
            self.tar.close()
            raise

    def close(self):
        self.tar.close()

    def raw(self, name):
        member = self.members.get(name)
        if member is None or not member.isfile() or not 0 < member.size <= MAX_METADATA_BYTES:
            raise InvalidArchive("missing or oversized metadata")
        with self.tar.extractfile(member) as stream:
            value = stream.read(MAX_METADATA_BYTES + 1)
        if len(value) != member.size:
            raise InvalidArchive("incomplete metadata")
        return value

    def document(self, raw):
        value = json.loads(raw, object_pairs_hook=unique_object, parse_constant=reject_constant)
        if not isinstance(value, dict):
            raise InvalidArchive("metadata must be an object")
        return value

    def descriptor(self, descriptor, allowed_types):
        if not isinstance(descriptor, dict):
            raise InvalidArchive("invalid descriptor")
        digest = descriptor.get("digest")
        size = descriptor.get("size")
        media = descriptor.get("mediaType")
        if not isinstance(digest, str) or not DIGEST.fullmatch(digest) or not isinstance(media, str) or media not in allowed_types:
            raise InvalidArchive("invalid descriptor digest or media type")
        if type(size) is not int or not 0 < size <= MAX_METADATA_BYTES:
            raise InvalidArchive("invalid descriptor size")
        raw = self.raw("blobs/sha256/" + digest[7:])
        if len(raw) != size or "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
            raise InvalidArchive("descriptor content differs from digest or size")
        doc = self.document(raw)
        # OCI 内容的 mediaType 可省略，父描述符始终提供该值；若内容声明了该值，必须与内容寻址描述符一致。
        if allowed_types != CONFIG_TYPES and (type(doc.get("schemaVersion")) is not int or doc["schemaVersion"] != 2 or ("mediaType" in doc and doc["mediaType"] != media)):
            raise InvalidArchive("descriptor schema or media type mismatch")
        return doc

    def identities(self, reference, expected):
        index = self.document(self.raw("index.json"))
        if type(index.get("schemaVersion")) is not int or index["schemaVersion"] != 2 or not isinstance(index.get("manifests"), list):
            raise InvalidArchive("invalid top-level OCI index")
        wanted = canonical_reference(reference)
        candidates = []
        for item in index["manifests"]:
            if not isinstance(item, dict) or not isinstance(item.get("annotations", {}), dict):
                raise InvalidArchive("invalid tagged descriptor")
            annotations = item.get("annotations", {})
            names = []
            for key in ("org.opencontainers.image.ref.name", "io.containerd.image.name"):
                name = annotations.get(key)
                if name is not None:
                    if not isinstance(name, str):
                        raise InvalidArchive("invalid tag annotation")
                    if ":" in name:
                        names.append(canonical_reference(name))
            if wanted in names:
                if any(name != wanted for name in names):
                    raise InvalidArchive("conflicting tag annotations")
                candidates.append(item)
        if not candidates:
            raise UnsupportedArchive("archive lacks an exact tagged identity")
        if len(candidates) != 1:
            raise InvalidArchive("image reference must have one exact descriptor")
        selected = self.amd64_manifests(candidates[0], [], 0, [0])
        if len(selected) != 1 or selected[0][0] != expected:
            raise InvalidArchive("amd64 manifest or config selection must be unique and match")
        return {"tag": reference, "config_digest": expected, "identities": selected[0][1]}

    def amd64_manifests(self, current, ancestors, depth, visited):
        if depth >= 8:
            raise InvalidArchive("excessive nested index depth")
        visited[0] += 1
        if visited[0] > 1024:
            raise InvalidArchive("excessive descriptor graph")
        declared = current.get("platform") if isinstance(current, dict) else None
        if declared is not None and not amd64_platform(declared):
            return []
        doc = self.descriptor(current, INDEX_TYPES | MANIFEST_TYPES)
        media = current["mediaType"]
        path = ancestors + [{"kind": "index" if media in INDEX_TYPES else "manifest", "digest": current["digest"]}]
        if media in MANIFEST_TYPES:
            config = self.descriptor(doc.get("config"), CONFIG_TYPES)
            if not amd64_platform(config):
                return []
            return [(doc["config"]["digest"], path)]
        manifests = doc.get("manifests")
        if not isinstance(manifests, list):
            raise InvalidArchive("invalid image index")
        selected = []
        for child in manifests:
            if not isinstance(child, dict):
                raise InvalidArchive("invalid child descriptor")
            # 嵌套 OCI 索引可能省略平台信息。必须解析其已验证子项，不能推测平台或按数组顺序选择。
            selected.extend(self.amd64_manifests(child, path, depth + 1, visited))
            if len(selected) > 1:
                raise InvalidArchive("ambiguous amd64 manifests")
        return selected


def amd64_platform(value):
    return isinstance(value, dict) and value.get("os") == "linux" and value.get("architecture") == "amd64" and value.get("variant", "") == ""


def resolve(path, images, configs):
    if not images or len(images) != len(configs) or len(set(images)) != len(images):
        raise InvalidArchive("image and config lists are not unique and paired")
    if len({canonical_reference(tag) for tag in images}) != len(images):
        raise InvalidArchive("image references must also be canonically unique")
    if any(not DIGEST.fullmatch(x) for x in configs):
        raise InvalidArchive("invalid expected config digest")
    archive = Archive(path)
    try:
        if "index.json" not in archive.members or "oci-layout" not in archive.members:
            raise UnsupportedArchive("archive has no OCI identity graph")
        layout = archive.document(archive.raw("oci-layout"))
        if layout.get("imageLayoutVersion") != "1.0.0":
            raise UnsupportedArchive("unsupported OCI layout version")
        return {"images": [archive.identities(tag, config) for tag, config in zip(images, configs)]}
    finally:
        archive.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", required=True)
    parser.add_argument("--images", required=True)
    parser.add_argument("--config-digests", required=True)
    args = parser.parse_args()
    try:
        result = resolve(args.archive, args.images.split(","), args.config_digests.split(","))
    except UnsupportedArchive:
        return 2
    except (InvalidArchive, OSError, ValueError, RecursionError, tarfile.TarError):
        # 此可选路径绝不打印任意归档数据或堆栈。
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
