#!/usr/bin/env python3
"""以有界流式读取获取镜像归档事实，不导入镜像或提取文件。"""
import hashlib
import json
import re
import tarfile

META_LIMIT = 16 * 1024 * 1024
CACHE_LIMIT = 64 * 1024 * 1024
PAX_TOTAL_LIMIT = 64 * 1024 * 1024
MEMBER_LIMIT = 100000
ARCHIVE_LIMIT = 256 * 1024 * 1024 * 1024
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
HEX = re.compile(r"[0-9a-f]{64}\Z")
INDEX_TYPES = {"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"}
MANIFEST_TYPES = {"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"}
CONFIG_TYPES = {"application/vnd.oci.image.config.v1+json", "application/vnd.docker.container.image.v1+json"}


class Invalid(ValueError):
    pass


class SafeTarInfo(tarfile.TarInfo):
    def _proc_pax(self, archive):
        if self.size > META_LIMIT:
            raise Invalid("archive extension metadata exceeds limit")
        consumed = getattr(archive, "_uip_pax_bytes", 0) + self.size
        if consumed > PAX_TOTAL_LIMIT:
            raise Invalid("archive cumulative extension metadata exceeds limit")
        archive._uip_pax_bytes = consumed
        return super()._proc_pax(archive)

    def _proc_gnulong(self, archive):
        if self.size > 16384:
            raise Invalid("archive name metadata exceeds limit")
        return super()._proc_gnulong(archive)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Invalid("duplicate JSON property")
        result[key] = value
    return result


def document(raw):
    def invalid_constant(_):
        raise Invalid("non-finite JSON number")
    try:
        return json.loads(raw, object_pairs_hook=unique_object, parse_constant=invalid_constant)
    except (ValueError, UnicodeError, RecursionError) as error:
        raise Invalid("invalid JSON document") from error


def member_name(member):
    name = member.name
    if name.startswith("./"):
        name = name[2:]
    if name in ("", ".") and member.isdir():
        return ""
    if (not name or len(name) > 4096 or name.startswith("/") or "\\" in name or any(ord(ch) < 32 or ord(ch) == 127 for ch in name)
            or any(part in ("", ".", "..") for part in name.rstrip("/").split("/"))):
        raise Invalid("unsafe archive member path")
    if not (member.isfile() or member.isdir()) or member.sparse is not None:
        raise Invalid("linked, sparse or special archive member")
    if member.size < 0 or member.size > ARCHIVE_LIMIT:
        raise Invalid("archive member exceeds limit")
    return name.rstrip("/")


def tagged(value):
    if not isinstance(value, str) or "@" in value or ":" not in value or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/-]{0,511}", value):
        raise Invalid("full image tag required")
    first = value.split("/", 1)[0]
    if "/" not in value:
        return "docker.io/library/"+value
    return value if "." in first or ":" in first or first == "localhost" else "docker.io/"+value


class HashReader:
    def __init__(self, source, limit=ARCHIVE_LIMIT):
        self.source = source
        self.digest = hashlib.sha256()
        self.size = 0
        self.limit = limit

    def read(self, size=-1):
        if size < 0:
            size = 1024 * 1024
        data = self.source.read(size)
        self.size += len(data)
        if self.size > self.limit:
            raise Invalid("archive byte budget exceeded")
        self.digest.update(data)
        return data


def amd64(value):
    return isinstance(value, dict) and value.get("os") == "linux" and value.get("architecture") == "amd64" and value.get("variant", "") == ""


def image_archive(source, references, configs, expected_sha, expected_size):
    if (not references or len(references) != len(configs) or len(references) > 64
            or len({tagged(ref) for ref in references}) != len(references)
            or any(not isinstance(item, str) or not DIGEST.fullmatch(item) for item in configs)):
        raise Invalid("image names and config identities must be unique and paired")
    reader = HashReader(source, expected_size)
    metadata, members, layers, cached = {}, {}, {}, 0
    with tarfile.open(fileobj=reader, mode="r|", tarinfo=SafeTarInfo, ignore_zeros=True) as archive:
        for member in archive:
            name = member_name(member)
            if not name:
                continue
            if name in members or len(members) >= MEMBER_LIMIT:
                raise Invalid("duplicate or excessive image archive members")
            members[name] = member.size
            if not member.isfile():
                continue
            blob = re.fullmatch(r"blobs/sha256/([0-9a-f]{64})", name)
            classic_config = re.fullmatch(r"[0-9a-f]{64}\.json", name)
            metadata_candidate = name in ("manifest.json", "index.json", "oci-layout") or classic_config or (blob and member.size <= META_LIMIT)
            digest = hashlib.sha256()
            chunks = []
            with archive.extractfile(member) as data:
                prefix = data.read(min(64, member.size))
                digest.update(prefix)
                keep = bool(metadata_candidate and (name in ("manifest.json", "index.json", "oci-layout") or prefix.lstrip().startswith((b"{", b"["))))
                if keep:
                    if member.size > META_LIMIT or cached+member.size > CACHE_LIMIT:
                        raise Invalid("image metadata exceeds bounded cache")
                    chunks.append(prefix)
                count = len(prefix)
                while True:
                    chunk = data.read(1024 * 1024)
                    if not chunk:
                        break
                    count += len(chunk)
                    digest.update(chunk)
                    if keep:
                        chunks.append(chunk)
                if count != member.size:
                    raise Invalid("truncated image archive member")
            actual = digest.hexdigest()
            if blob and blob[1] != actual:
                raise Invalid("image blob content does not match its digest")
            if classic_config and name[:-5] != actual:
                raise Invalid("image config content does not match its digest")
            if name.endswith("/layer.tar"):
                layers[name] = "sha256:"+actual
            if keep:
                metadata[name] = b"".join(chunks)
                cached += member.size
        while reader.read(1024 * 1024):
            pass
    if reader.size != expected_size or reader.digest.hexdigest() != expected_sha:
        raise Invalid("images.tar checksum or length mismatch")

    def read_json(name):
        if name not in metadata:
            raise Invalid("required image metadata is missing")
        return document(metadata[name])

    def config_fact(config, digest, identities):
        if not amd64(config):
            raise Invalid("image config is not linux/amd64")
        runtime_config = config.get("config") or {}
        if not isinstance(runtime_config, dict):
            raise Invalid("invalid image runtime config")
        labels = runtime_config.get("Labels") or {}
        if not isinstance(labels, dict):
            raise Invalid("invalid image labels")
        version = labels.get("org.opencontainers.image.version", "")
        protocol = labels.get("com.basic-platform.license.protocol", "")
        if not isinstance(version, str) or not isinstance(protocol, str) or len(version) > 128 or len(protocol) > 16:
            raise Invalid("invalid image version or protocol label")
        return {"config_digest": digest, "identities": sorted(set([digest]+identities)),
                "version": version, "protocol": protocol, "architecture": "amd64", "os": "linux"}

    def descriptor(item, types):
        if (not isinstance(item, dict) or item.get("mediaType") not in types or not isinstance(item.get("digest"), str)
                or not DIGEST.fullmatch(item["digest"]) or type(item.get("size")) is not int):
            raise Invalid("invalid OCI descriptor")
        path = "blobs/sha256/"+item["digest"][7:]
        if members.get(path) != item["size"] or path not in metadata:
            raise Invalid("OCI metadata descriptor length mismatch")
        value = read_json(path)
        if not isinstance(value, dict) or (types != CONFIG_TYPES and (value.get("schemaVersion") != 2 or value.get("mediaType", item["mediaType"]) != item["mediaType"])):
            raise Invalid("invalid OCI metadata schema")
        return value

    def resolve(item, path, depth, visited):
        if depth > 8 or visited[0] >= 1024:
            raise Invalid("OCI descriptor graph exceeds bounds")
        visited[0] += 1
        if "platform" in item and not amd64(item["platform"]):
            return []
        value = descriptor(item, INDEX_TYPES | MANIFEST_TYPES)
        chain = path+[item["digest"]]
        if item["mediaType"] in INDEX_TYPES:
            if not isinstance(value.get("manifests"), list):
                raise Invalid("invalid OCI index")
            results = []
            for child in value["manifests"]:
                if not isinstance(child, dict):
                    raise Invalid("invalid OCI child")
                results.extend(resolve(child, chain, depth+1, visited))
            return results
        config = descriptor(value.get("config"), CONFIG_TYPES)
        if not amd64(config):
            return []
        if not isinstance(value.get("layers"), list):
            raise Invalid("invalid OCI layers")
        for layer in value["layers"]:
            if (not isinstance(layer, dict) or not isinstance(layer.get("digest"), str) or not DIGEST.fullmatch(layer["digest"])
                    or type(layer.get("size")) is not int or members.get("blobs/sha256/"+layer["digest"][7:]) != layer["size"]):
                raise Invalid("missing OCI layer or descriptor size mismatch")
        return [(config, value["config"]["digest"], chain)]

    result = {}
    if "index.json" in metadata and "oci-layout" in metadata:
        layout, index = read_json("oci-layout"), read_json("index.json")
        if not isinstance(layout, dict) or layout.get("imageLayoutVersion") != "1.0.0" or not isinstance(index, dict) or index.get("schemaVersion") != 2 or not isinstance(index.get("manifests"), list):
            raise Invalid("unsupported OCI archive")
        for reference, expected in zip(references, configs):
            candidates = []
            for item in index["manifests"]:
                if not isinstance(item, dict) or not isinstance(item.get("annotations", {}), dict):
                    raise Invalid("invalid tagged OCI descriptor")
                annotations = item.get("annotations", {})
                names = []
                for key in ("org.opencontainers.image.ref.name", "io.containerd.image.name"):
                    if key in annotations:
                        if not isinstance(annotations[key], str):
                            raise Invalid("invalid OCI image reference annotation")
                        if ":" in annotations[key]:
                            names.append(tagged(annotations[key]))
                if tagged(reference) in names:
                    if any(name != tagged(reference) for name in names):
                        raise Invalid("conflicting OCI reference annotations")
                    candidates.append(item)
            if len(candidates) != 1:
                raise Invalid("image tag must have one exact OCI descriptor")
            selected = resolve(candidates[0], [], 0, [0])
            if len(selected) != 1 or selected[0][1] != expected:
                raise Invalid("image config does not match the unique amd64 descriptor")
            result[reference] = config_fact(*selected[0])
    elif "manifest.json" in metadata:
        manifest = read_json("manifest.json")
        if not isinstance(manifest, list):
            raise Invalid("invalid Docker archive manifest")
        for reference, expected in zip(references, configs):
            selected = [entry for entry in manifest if isinstance(entry, dict) and isinstance(entry.get("RepoTags"), list)
                        and tagged(reference) in [tagged(tag) for tag in entry["RepoTags"]]]
            if len(selected) != 1:
                raise Invalid("image tag must have one exact Docker manifest")
            entry = selected[0]
            config_path = entry.get("Config")
            if config_path not in (expected[7:]+".json", "blobs/sha256/"+expected[7:]):
                raise Invalid("Docker config identity differs from package metadata")
            config = read_json(config_path)
            if not isinstance(config, dict) or not isinstance(config.get("rootfs", {}), dict):
                raise Invalid("invalid Docker config document")
            diff_ids = config.get("rootfs", {}).get("diff_ids", [])
            if not isinstance(diff_ids, list):
                raise Invalid("invalid Docker rootfs diff IDs")
            layer_paths = entry.get("Layers")
            if not isinstance(layer_paths, list) or len(layer_paths) != len(diff_ids):
                raise Invalid("Docker layer list differs from config")
            for layer, expected_layer in zip(layer_paths, diff_ids):
                if layers.get(layer) != expected_layer:
                    raise Invalid("Docker layer bytes differ from config diff ID")
            result[reference] = config_fact(config, expected, [])
    else:
        raise Invalid("image archive has no supported identity graph")
    return result
