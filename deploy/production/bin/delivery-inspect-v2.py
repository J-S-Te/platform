#!/usr/bin/env python3
"""发布清单的有界结构交叉核对；不验证签名、不授予实例或迁移资格。"""
import hashlib
import base64
import binascii
import re


class Invalid(ValueError):
    pass


# 目录 1 的有限运行集合。测试用权威 Go catalog 检查漂移；运行时不调用 Go 或介质代码。
CATALOG = {
    "contract_management": ("contract-api", "contract-worker"),
    "customer_and_opportunity": ("customer-api", "customer-opportunity-alert-worker", "customer-owner-notification-worker",
        "customer-contract-transfer-worker", "customer-presale-alert-worker", "customer-presale-assignment-notification-worker",
        "customer-presale-progress-notification-worker", "customer-notification-delivery-worker", "customer-presale-worker"),
    "customer_portal": ("portal-api", "portal-report-worker", "portal-invite-compensation-worker"),
    "data_analysis": ("data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker", "data-analysis-metabase"),
    "project_management": ("project-api", "project-sla-notifier"),
    "settlement": ("settlement-api", "settlement-worker"),
}
HEX = re.compile(r"[0-9a-f]{64}\Z")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
IDENTITY = re.compile(r"[a-z0-9][a-z0-9._-]{0,127}\Z")
RELEASE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")
ARTIFACT_TYPES = {"image-package", "asset", "script", "host-dependencies", "template", "checksum", "provenance", "other"}


def fields(value, allowed, required=None):
    if not isinstance(value, dict) or set(value) - set(allowed) or not set(required if required is not None else allowed) <= value.keys():
        raise Invalid("release declaration has unknown or missing fields")
    return value


def text(value, pattern=None):
    if not isinstance(value, str) or not value or len(value) > 4096 or any(ord(ch) < 32 or ord(ch) == 127 for ch in value) or pattern and not pattern.fullmatch(value):
        raise Invalid("release declaration contains an invalid string")
    return value


def integer(value, minimum=1, maximum=2147483647):
    if type(value) is not int or not minimum <= value <= maximum:
        raise Invalid("release declaration contains an invalid integer")
    return value


def sequence(value, maximum=4096):
    if not isinstance(value, list) or not value or len(value) > maximum:
        raise Invalid("release declaration has an empty or excessive collection")
    return value


def role(service):
    return "infrastructure" if service == "data-analysis-metabase" else "api" if service.endswith("-api") else "worker"


def binary(value):
    if not isinstance(value, str):
        raise Invalid("release proof has an invalid base64 field")
    try:
        decoded = base64.b64decode(value.replace("\r", "").replace("\n", ""), validate=True)
    except (ValueError, binascii.Error):
        raise Invalid("release proof has an invalid base64 field") from None
    if not decoded:
        raise Invalid("release proof has empty signature or payload material")
    return decoded


def proof_object(inspection, image_module, path):
    raw = inspection.raw(path, 8 * 1024 * 1024)
    try:
        value = image_module.document(raw)
    except image_module.Invalid:
        raise Invalid("release proof is not strict JSON") from None
    if not isinstance(value, dict):
        raise Invalid("release proof must be a JSON object")
    return raw, value


def bundle_shape(value, payload, *, dsse=False):
    # 仅核对 Go bundle.go 已定义的格式和原始载荷绑定，不验证密钥/签名或建立发行信任。
    media_type = value.get("mediaType")
    if not isinstance(media_type, str) or not media_type.startswith("application/vnd.dev.sigstore.bundle"):
        raise Invalid("unsupported release proof bundle media type")
    if value.get("verificationMaterial") is not None and not isinstance(value["verificationMaterial"], dict):
        raise Invalid("invalid release proof verification material structure")
    material = value.get("verificationMaterial") or {}
    if material.get("tlogEntries") is not None and not isinstance(material["tlogEntries"], list):
        raise Invalid("invalid release proof transparency-log collection structure")
    if dsse:
        envelope = value.get("dsseEnvelope")
        if not isinstance(envelope, dict) or value.get("messageSignature") is not None or envelope.get("payloadType") != "application/vnd.in-toto+json":
            raise Invalid("build proof must be a DSSE in-toto envelope")
        if binary(envelope.get("payload")) != payload:
            raise Invalid("build proof DSSE payload differs from the readable statement")
        signatures = sequence(envelope.get("signatures"))
        valid_signature = False
        for signature in signatures:
            if (not isinstance(signature, dict) or any(signature.get(key) is not None and not isinstance(signature[key], str)
                                                       for key in ("keyid", "sig"))):
                raise Invalid("invalid DSSE signature structure")
            try:
                binary(signature.get("sig"))
                valid_signature = True
            except Invalid:
                continue
        if not valid_signature:
            raise Invalid("build proof has no nonempty signature material")
    else:
        message = value.get("messageSignature")
        if not isinstance(message, dict) or value.get("dsseEnvelope") is not None or not isinstance(message.get("messageDigest"), dict):
            raise Invalid("release proof must be a blob signature envelope")
        digest = message["messageDigest"]
        algorithm = text(digest.get("algorithm")).upper().replace("-", "").replace("_", "")
        if algorithm not in {"SHA2256", "SHA256"} or binary(digest.get("digest")) != hashlib.sha256(payload).digest():
            raise Invalid("release proof message digest differs from the release manifest")
        binary(message.get("signature"))


def statement_shape(value):
    fields(value, {"_type", "predicateType", "subject", "predicate"}, {"_type", "predicateType", "subject"})
    if value["_type"] != "https://in-toto.io/Statement/v1" or value["predicateType"] != "https://slsa.dev/provenance/v1":
        raise Invalid("unsupported in-toto statement or provenance type")
    for subject in sequence(value["subject"]):
        fields(subject, {"name", "digest"})
        text(subject["name"])
        if not isinstance(subject["digest"], dict) or set(subject["digest"]) != {"sha256"}:
            raise Invalid("in-toto subject must contain a SHA256 digest")
        text(subject["digest"]["sha256"], HEX)


def validate(inspection, image_module, profiles, app_components):
    proof_files = inspection.media.integrity_files
    if not proof_files and not inspection.media.integrity_dirs:
        return {}
    if not {"release-manifest.json", "release-manifest.sigstore.json"} <= proof_files.keys():
        raise Invalid("release manifest/signature pair is incomplete")
    if not proof_files["release-manifest.sigstore.json"]:
        raise Invalid("release signature material is empty")
    manifest_raw = inspection.raw("release-manifest.json", 4 * 1024 * 1024)
    value = image_module.document(manifest_raw)
    _, release_bundle = proof_object(inspection, image_module, "release-manifest.sigstore.json")
    bundle_shape(release_bundle, manifest_raw)
    fields(value, {"schema", "release_id", "channel", "release_sequence", "target_platform", "build_inputs_sha256", "compatibility",
                   "artifacts", "profiles", "applications", "images", "legacy_compatibility"},
           {"schema", "release_id", "channel", "release_sequence", "target_platform", "build_inputs_sha256", "compatibility", "artifacts", "profiles", "applications", "images"})
    if value["schema"] != "urn:uip:release-manifest:v1" or value["channel"] != "production" or value["target_platform"] != "linux/amd64":
        raise Invalid("unsupported release schema, channel or platform")
    text(value["release_id"], RELEASE)
    integer(value["release_sequence"], maximum=9223372036854775807)
    text(value["build_inputs_sha256"], HEX)
    if value["build_inputs_sha256"] != inspection.build_info.get("SOURCE_INPUT_SHA256"):
        raise Invalid("release source input differs from the delivered build")
    policy = fields(value["compatibility"], {"verifier_policy", "min_api_release_integrity", "min_agent_release_integrity", "catalog_version",
        "registration_protocols", "shared_control_plane_sha256", "migration_preconditions"},
        {"verifier_policy", "min_api_release_integrity", "min_agent_release_integrity", "catalog_version", "registration_protocols", "shared_control_plane_sha256"})
    for key in ("verifier_policy", "min_api_release_integrity", "min_agent_release_integrity"):
        text(policy[key])
    if integer(policy["catalog_version"]) != 1:
        raise Invalid("unsupported release catalog version")
    text(policy["shared_control_plane_sha256"], HEX)
    if "migration_preconditions" in policy:
        if not isinstance(policy["migration_preconditions"], list) or len(policy["migration_preconditions"]) > 64:
            raise Invalid("invalid migration preconditions")
        for condition in policy["migration_preconditions"]:
            text(condition)
    protocols = policy["registration_protocols"]
    selected = set(inspection.report["applications"])
    if not isinstance(protocols, dict) or set(protocols) != selected:
        raise Invalid("release application protocols differ from delivered application scope")
    for protocol in protocols.values():
        integer(protocol, maximum=2)
    if value.get("legacy_compatibility") is not None:
        legacy = fields(value["legacy_compatibility"], {"v1_approval_sha256", "purpose"}, set())
        # 与 Go schema 一致：这里只约束已知字符串字段，不从可选说明推导迁移授权。
        if any(item is not None and not isinstance(item, str) for item in legacy.values()):
            raise Invalid("invalid legacy compatibility declaration")

    available = dict(inspection.media.files)
    available.update(proof_files)
    expected_artifacts = set(available) - {"release-manifest.json", "release-manifest.sigstore.json"}
    artifacts = {}
    total = 0
    for artifact in sequence(value["artifacts"]):
        fields(artifact, {"path", "sha256", "size", "type"})
        path = text(artifact["path"])
        if path not in expected_artifacts or path in artifacts or artifact["type"] not in ARTIFACT_TYPES:
            raise Invalid("release artifact path/type is unknown or duplicated")
        text(artifact["sha256"], HEX)
        size = integer(artifact["size"], maximum=64 * 1024 ** 3)
        total += size
        if total > 64 * 1024 ** 3 or available[path] != size:
            raise Invalid("release artifact size differs from actual delivery bytes")
        actual = inspection.actual.get(path)
        if path == "SHA256SUMS":
            actual = inspection.report["delivery_id"]
        if actual is None:
            with inspection.media.open(path) as source:
                actual = _sha(source, size)
        if artifact["sha256"] != actual:
            raise Invalid("release artifact checksum differs from actual delivery bytes")
        artifacts[path] = artifact
    if set(artifacts) != expected_artifacts:
        raise Invalid("release artifacts do not bind the complete delivery and checksum manifest")

    profile_paths = {"profiles/" + filename: app for app, filename in profiles.items()}
    observed_profiles = set()
    for profile in sequence(value["profiles"], 6):
        fields(profile, {"path", "sha256", "normalized_sha256", "policy_version"})
        path = profile["path"]
        if not isinstance(path, str) or path not in profile_paths or path in observed_profiles or integer(profile["policy_version"]) != 1:
            raise Invalid("release profile set or policy version is unsupported")
        raw = inspection.raw(path)
        digest = hashlib.sha256(raw).hexdigest()
        # 当前 Go producer 的 policy 1 对原始字节取 SHA，normalized_sha256 使用同一值。
        if profile["sha256"] != digest or profile["normalized_sha256"] != digest or raw != inspection.assets["subsystems.d/" + profiles[profile_paths[path]]]:
            raise Invalid("raw/normalized release profile differs from the deployment asset profile")
        observed_profiles.add(path)
    if observed_profiles != set(profile_paths):
        raise Invalid("release must bind exactly six production profiles")

    packages = {package["archive"]: package for package in inspection.packages.values()}
    images = {}
    checked_proofs = set()
    for image in sequence(value["images"], 128):
        fields(image, {"name", "platform", "source", "package", "archive_member", "config_digest", "manifest_digest", "index_digest", "provenance", "provenance_bundle"},
               {"name", "platform", "source", "package", "archive_member", "provenance", "provenance_bundle"})
        name = text(image["name"], IDENTITY)
        text(image["source"])
        if name in images or image["platform"] != "linux/amd64" or image["package"] not in packages or image["archive_member"] != "images.tar":
            raise Invalid("release image package/member or platform is not proved by the delivery")
        if any(key in image and not isinstance(image[key], str) for key in ("config_digest", "manifest_digest", "index_digest")):
            raise Invalid("release image typed digest has an invalid type")
        digests = [text(image[key], DIGEST) for key in ("config_digest", "manifest_digest", "index_digest") if image.get(key)]
        if not digests:
            raise Invalid("release image has no typed digest")
        facts = [fact for fact in packages[image["package"]]["images"].values()
                 if (not image.get("config_digest") or image["config_digest"] == fact["config_digest"]) and set(digests) <= set(fact["identities"])]
        if not facts:
            raise Invalid("release image identities are not present in the actual image graph")
        for key in ("provenance", "provenance_bundle"):
            if not isinstance(image[key], str) or not image[key].startswith("attestations/") or image[key] not in artifacts:
                raise Invalid("release image build proof is absent or unbound")
        proof_pair = (image["provenance"], image["provenance_bundle"])
        if proof_pair not in checked_proofs:
            statement_raw, statement = proof_object(inspection, image_module, image["provenance"])
            statement_shape(statement)
            _, envelope = proof_object(inspection, image_module, image["provenance_bundle"])
            bundle_shape(envelope, statement_raw, dsse=True)
            checked_proofs.add(proof_pair)
        images[name] = image

    applications = set()
    for application in sequence(value["applications"], 6):
        fields(application, {"code", "environment", "components"})
        app = application["code"]
        if not isinstance(app, str) or app not in protocols or app in applications or application["environment"] != "prod":
            raise Invalid("release application/environment is unknown or duplicated")
        compose, conditional, _ = inspection.projections[app]
        expected = set(compose["runtime_services"])
        if expected != set(CATALOG[app]):
            raise Invalid("production profile runtime services differ from the supported catalog")
        services = set()
        for component in sequence(application["components"], 64):
            fields(component, {"service_id", "role", "conditional", "image", "registration_protocol", "catalog_version", "replicas", "command_policy"})
            service = component["service_id"]
            if not isinstance(service, str) or service not in expected or service in services or component["role"] != role(service):
                raise Invalid("release has an unknown, duplicate or misclassified runtime component")
            if (type(component["conditional"]) is not bool or component["conditional"] != (service in conditional)
                    or component["conditional"] != (service == "portal-invite-compensation-worker")
                    or integer(component["registration_protocol"], maximum=2) != protocols[app]
                    or integer(component["catalog_version"]) != policy["catalog_version"] or integer(component["replicas"], maximum=64) != 1
                    or not isinstance(component["command_policy"], str)):
                raise Invalid("release component conditions, protocol, catalog or replicas contradict the production profile")
            image = images.get(component["image"]) if isinstance(component["image"], str) else None
            package = inspection.packages["common" if service == "data-analysis-metabase" else app_components[app]]
            fact = inspection.image(app, service)
            if image is None or image["package"] != package["archive"] or any(image.get(key) and image[key] not in fact["identities"] for key in ("config_digest", "manifest_digest", "index_digest")):
                raise Invalid("release component image differs from its actual production image")
            services.add(service)
        if services != expected:
            raise Invalid("release omits a required or conditional runtime component")
        applications.add(app)
    if applications != selected:
        raise Invalid("release application set differs from the delivered application set")
    return protocols


def _sha(source, expected_size):
    digest = hashlib.sha256()
    size = 0
    for chunk in iter(lambda: source.read(1024 * 1024), b""):
        size += len(chunk)
        if size > expected_size:
            raise Invalid("release artifact changed during hashing")
        digest.update(chunk)
    if size != expected_size:
        raise Invalid("release artifact was truncated during hashing")
    return digest.hexdigest()
