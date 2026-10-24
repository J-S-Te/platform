#!/usr/bin/env python3
"""使用独立交付夹具中的真实 tar、SHA 及 OCI 字节，不使用 Docker 或敏感信息。"""
import base64
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
PRODUCTION = Path(__file__).resolve().parents[1]
CHECKER = PRODUCTION / "bin/delivery-inspect.py"
spec = importlib.util.spec_from_file_location("delivery_checker_fixture", CHECKER)
CHECK = importlib.util.module_from_spec(spec)
spec.loader.exec_module(CHECK)
VERSION = "inspection-v1"
SECRET = "DO_NOT_LOG_CUSTOMER_OR_INSTANCE_8d220731"
APP = "contract_management"
PROFILES = {"customer_and_opportunity": "customer_and_opportunity-prod.yaml", "customer_portal": "customer_portal-prod.yaml",
            "contract_management": "contract_management-prod.yaml", "project_management": "project_management-prod.yaml",
            "settlement": "settlement-prod.yaml", "data_analysis": "data-analysis-prod.yaml"}
COMPONENT_PREFIXES = {"common": "common-infrastructure", "platform": "platform-backend", "frontend": "frontend",
                      "contract": "contract-backend", "project": "project-backend", "customer-opportunity": "customer-opportunity-backend",
                      "customer-portal": "customer-portal-backend", "data-analysis": "data-analysis-backend"}


def encoded(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def tar_bytes(entries, compressed=False):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz" if compressed else "w") as archive:
        for name, raw in entries:
            if isinstance(raw, tarfile.TarInfo):
                archive.addfile(raw)
            else:
                info = tarfile.TarInfo(name)
                info.size = len(raw)
                info.mode = 0o600
                archive.addfile(info, io.BytesIO(raw))
    return output.getvalue()


def image_tags(component):
    if component == "common":
        return ["mysql:8.4", "quay.io/keycloak/keycloak:26.2", "temporalio/auto-setup:1.29.7", "metabase/metabase:v0.53.7",
                "prom/prometheus:v3.5.0", "prom/node-exporter:v1.9.1", "registry:2.8.3", "tecnativa/docker-socket-proxy:v0.5.0", "uip-package/file-gateway:"+VERSION]
    if component == "data-analysis":
        return ["uip-package/data-analysis-"+role+":"+VERSION for role in ("dashboard-api", "aggregation-worker", "alert-worker", "production-migrate")]
    return ["uip-package/"+COMPONENT_PREFIXES[component]+":"+VERSION]


def image_package(component, *, image_version=VERSION, classic=False, mutate=None):
    tags = image_tags(component)
    blobs, manifests, configs, facts = {}, [], [], {}
    for tag in tags:
        config = encoded({"architecture": "amd64", "os": "linux", "rootfs": {"type": "layers", "diff_ids": []},
                          "config": {"Cmd": [tag], "Labels": {"org.opencontainers.image.version": image_version, "com.basic-platform.license.protocol": "1",
                                                               "fixture-sensitive-label": SECRET}}})
        config_hash = digest(config)
        config_id = "sha256:"+config_hash
        configs.append(config_id)
        if classic:
            blobs[config_hash+".json"] = config
            manifests.append({"Config": config_hash+".json", "RepoTags": [tag], "Layers": []})
            facts[tag] = {"config": config_id, "manifest": config_id}
        else:
            blobs["blobs/sha256/"+config_hash] = config
            manifest = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                                "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config_id, "size": len(config)}, "layers": []})
            manifest_id = "sha256:"+digest(manifest)
            blobs["blobs/sha256/"+manifest_id[7:]] = manifest
            manifests.append({"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifest_id, "size": len(manifest),
                              "annotations": {"org.opencontainers.image.ref.name": tag}})
            facts[tag] = {"config": config_id, "manifest": manifest_id}
    entries = [("manifest.json", encoded(manifests))] if classic else [("index.json", encoded({"schemaVersion": 2, "manifests": manifests})), ("oci-layout", b'{"imageLayoutVersion":"1.0.0"}')]
    entries += list(blobs.items())
    if mutate:
        entries = mutate(entries)
    images = tar_bytes(entries)
    metadata = "PACKAGE_FORMAT=2\nCOMPONENT="+component+"\nVERSION="+VERSION+"\nPLATFORM=linux/amd64\nIMAGES="+",".join(tags)+"\nIMAGE_CONFIG_DIGESTS="+",".join(configs)+"\n"
    return tar_bytes([("package.env", metadata.encode()), ("SHA256SUMS", (digest(images)+"  images.tar\n").encode()), ("images.tar", images)], True), facts


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="delivery-inspector-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.media = self.root / "candidate"
        self.media.mkdir()
        self.assets = {"docker-compose.yml": b"services: {}\n", "bin/deploy.sh": b"#!/bin/sh\nexit 99\n"}
        for tool in CHECK.RECOVERY_TOOLS:
            self.assets['bin/'+tool] = (PRODUCTION/'bin'/tool).read_bytes()
        for filename in PROFILES.values():
            self.assets["subsystems.d/"+filename] = (PRODUCTION / "subsystems.d" / filename).read_bytes()
        self.names, self.facts = {}, {}
        for component in ("common", "platform", "frontend", "contract"):
            self.add_package(component)
        (self.media / "BUILD_INFO.txt").write_text("RELEASE_VERSION="+VERSION+"\nTARGET_PLATFORM=linux/amd64\nSOURCE_INPUT_SHA256="+"a"*64+"\n")
        self.finish()

    def add_package(self, component, **kwargs):
        raw, facts = image_package(component, **kwargs)
        name = COMPONENT_PREFIXES[component]+("" if component == "common" else "-"+VERSION)+"-linux-amd64.tar.gz"
        (self.media / name).write_bytes(raw)
        self.names[component] = name
        self.facts[component] = facts

    def finish(self):
        asset_name = "deployment-assets-"+VERSION+".tar.gz"
        (self.media / asset_name).write_bytes(tar_bytes(list(self.assets.items()), True))
        compatibility = "FORMAT=1\nPLATFORM_ASSETS_INPUT_SHA256="+"b"*64+"\n"
        for key, name in (("COMMON", self.names["common"]), ("PLATFORM", self.names["platform"]), ("FRONTEND", self.names["frontend"]), ("ASSETS", asset_name)):
            compatibility += key+"_PACKAGE="+name+"\n"+key+"_PACKAGE_SHA256="+digest((self.media / name).read_bytes())+"\n"
        (self.media / "PLATFORM_ASSETS_COMPATIBILITY.env").write_text(compatibility)
        for path in list(self.media.iterdir()):
            if path.name.endswith(".tar.gz"):
                path.with_name(path.name+".sha256").write_text(digest(path.read_bytes())+"  "+path.name+"\n")
        files = sorted(path for path in self.media.iterdir() if path.name != "SHA256SUMS")
        (self.media / "SHA256SUMS").write_text("".join(digest(path.read_bytes())+"  "+path.name+"\n" for path in files))

    def material(self, *, license=True):
        config = self.facts["contract"][image_tags("contract")[0]]["config"]
        components = [{"service_id": service, "protocol": 1, "coverage_digest": "sha256:"+"c"*64, "image_digest": config}
                      for service in ("contract-api", "contract-worker")]
        self.assets["runtime-license-"+APP+"-prod.json"] = encoded({"version": 1, "application": APP, "environment": "prod", "components": components,
                                                                 "release_generation": "contract-r1", "retire_service_ids": ["contract-old-worker"]})
        self.assets["license-evidence.json"] = encoded({"version": 1, "project": "fixture-project", "installation_boundary": True,
            "services": [{"application": APP, "environment": "prod", "name": service, "image_digest": config, "version": VERSION, "protocol": "1", "replicas": 1}
                         for service in ("contract-api", "contract-worker")], "excluded_services": []})
        self.assets["license-migration-evidence.json"] = encoded({"version": 1, "project": "fixture-project", "installation_boundary": True, "services": [], "excluded_services": []})
        self.assets["license-installation.json"] = encoded({"version": 1, "scenario": "fresh", "customer_id": SECRET})
        if license:
            header = {"alg": "EdDSA", "typ": "commercial-license+jws", "kid": "vendor-v1"}
            claims = {"protocol_version": 1, "issuer": "unified-business-suite-vendor", "product_id": "unified-business-suite", "license_id": "fixture-license",
                      "customer_id": SECRET, "instance_id": SECRET, "environment": "production", "version": 1, "issued_at": 1700000000, "not_before": 1700000000,
                      "applications": [{"code": APP, "kind": "FULL", "not_before": 1700000000, "expires_at": 2000000000}]}
            def b64(raw):
                return base64.urlsafe_b64encode(raw).rstrip(b"=")
            self.assets["license/commercial-license.jws"] = b".".join((b64(encoded(header)), b64(encoded(claims)), b64(bytes(64))))
        self.finish()

    def result(self, phase="prepare", path=None):
        return CHECK.inspect(path or self.media, phase)

    def test_candidate_can_prepare_and_install_base_without_fabricated_authorization(self):
        before = {path.name: path.read_bytes() for path in self.media.iterdir()}
        for phase in ("prepare", "install"):
            report, code = self.result(phase)
            self.assertEqual(code, 0, report)
            self.assertEqual(report["status"], "NEEDS_INPUT")
            self.assertEqual(report["consumer_count"], 2)
            self.assertEqual(report["applications"], [APP])
            self.assertEqual(report["delivery_id"], digest((self.media / "SHA256SUMS").read_bytes()))
        self.assertEqual(before, {path.name: path.read_bytes() for path in self.media.iterdir()})
        self.assertFalse(list(self.root.rglob("__pycache__")))

    def test_complete_material_ready_only_for_structure_and_does_not_validate_fake_signature(self):
        self.material()
        report, code = self.result("install")
        self.assertEqual((code, report["status"]), (0, "READY"), report)
        authority = next(row for row in report["checks"] if row["id"] == "authority.platform")
        self.assertEqual(authority["status"], "PENDING")
        self.assertFalse(authority["required_for_install"])
        self.assertNotIn(SECRET, json.dumps(report))
        self.assertNotIn(self.assets["license/commercial-license.jws"].decode(), json.dumps(report))

    def test_missing_release_is_pending_not_corruption_but_explicit_install_requires_it(self):
        self.material()
        del self.assets["runtime-license-"+APP+"-prod.json"]
        self.finish()
        self.assertEqual(self.result("prepare")[1], 0)
        report, code = self.result("install")
        self.assertEqual((report["status"], code), ("NEEDS_INPUT", 3), report)

    def test_plan_without_jws_still_supports_instance_request_bootstrap(self):
        self.material(license=False)
        report, code = self.result("install")
        self.assertEqual((report["status"], code), ("NEEDS_INPUT", 0), report)

    def test_promised_build_license_is_required_and_its_digest_cannot_change(self):
        self.material(license=False)
        info = self.media / "BUILD_INFO.txt"
        info.write_text(info.read_text()+"LICENSE_SHA256="+"1"*64+"\n")
        self.finish()
        self.assertEqual(self.result("prepare")[1], 0)
        self.assertEqual(self.result("install")[1], 3)
        self.material()
        self.assertEqual(self.result("install")[1], 2)
        lines = [line for line in info.read_text().splitlines() if not line.startswith("LICENSE_SHA256=")]
        info.write_text("\n".join(lines)+"\nLICENSE_SHA256="+digest(self.assets["license/commercial-license.jws"])+"\n")
        self.finish()
        self.assertEqual(self.result("install")[1], 0)

    def test_missing_worker_fails_even_after_all_outer_hashes_are_rebound(self):
        self.material()
        path = "runtime-license-"+APP+"-prod.json"
        release = json.loads(self.assets[path])
        release["components"] = release["components"][:1]
        self.assets[path] = encoded(release)
        self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_image_version_or_actual_identity_mix_fails(self):
        self.add_package("contract", image_version="other-release")
        self.finish()
        self.assertEqual(self.result()[1], 2)
        self.add_package("contract")
        self.material()
        path = "runtime-license-"+APP+"-prod.json"
        release = json.loads(self.assets[path])
        release["components"][0]["image_digest"] = "sha256:"+"d"*64
        self.assets[path] = encoded(release)
        self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_oci_native_manifest_identity_is_accepted_only_when_proved(self):
        self.material()
        manifest = self.facts["contract"][image_tags("contract")[0]]["manifest"]
        for path in ("runtime-license-"+APP+"-prod.json", "license-evidence.json"):
            self.assets[path] = self.assets[path].replace(self.facts["contract"][image_tags("contract")[0]]["config"].encode(), manifest.encode())
        self.finish()
        self.assertEqual(self.result("install")[1], 0)

    def test_generation_change_must_match_immutable_manifest_and_valid_schema(self):
        self.material()
        asset = self.media / ("deployment-assets-"+VERSION+".tar.gz")
        asset.write_bytes(asset.read_bytes()+b"altered-generation")
        self.assertEqual(self.result()[1], 2)
        self.material()
        path = "runtime-license-"+APP+"-prod.json"
        value = json.loads(self.assets[path]); value["release_generation"] = "invalid/unchecked"
        self.assets[path] = encoded(value); self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_duplicate_json_and_secret_fields_fail_without_logging_values(self):
        self.material()
        for raw in (b'{"version":1,"version":1}', encoded({"version": 1, "client_secret": SECRET})):
            self.assets["runtime-license-"+APP+"-prod.json"] = raw
            self.finish()
            report, code = self.result()
            self.assertEqual(code, 2)
            self.assertNotIn(SECRET, json.dumps(report))

    def test_fresh_baseline_cannot_claim_existing_business(self):
        self.material()
        baseline = json.loads(self.assets["license-evidence.json"])
        self.assets["license-migration-evidence.json"] = encoded(baseline)
        self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_business_cannot_be_excluded_from_installation_boundary(self):
        self.material()
        value = json.loads(self.assets["license-evidence.json"])
        value["excluded_services"] = ["contract-worker"]
        self.assets["license-evidence.json"] = encoded(value); self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_classic_archives_and_old_unbound_redundant_sidecar_are_supported(self):
        self.add_package("contract", classic=True)
        self.finish()
        sidecar = self.names["contract"]+".sha256"
        manifest = self.media / "SHA256SUMS"
        manifest.write_text("".join(line for line in manifest.read_text().splitlines(True) if not line.rstrip().endswith("  "+sidecar)))
        self.assertEqual(self.result()[1], 0)

    def test_sidecar_bound_to_other_file_is_not_accepted(self):
        sidecar = self.media / (self.names["contract"]+".sha256")
        sidecar.write_text(digest((self.media / self.names["contract"]).read_bytes())+"  another.tar.gz\n")
        manifest = self.media / "SHA256SUMS"
        manifest.write_text("".join(line for line in manifest.read_text().splitlines(True) if not line.rstrip().endswith("  "+sidecar.name)))
        self.assertEqual(self.result()[1], 2)

    def test_corrupt_blob_is_rejected_even_with_new_package_and_media_checksums(self):
        def corrupt(entries):
            return [(name, raw+b"corruption" if name.startswith("blobs/") else raw) for name, raw in entries]
        self.add_package("contract", mutate=corrupt)
        self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_directory_link_manifest_traversal_duplicate_and_sensitive_file_are_refused(self):
        manifest = self.media / "SHA256SUMS"
        original = manifest.read_bytes()
        for bad in (original+original.splitlines(True)[0], b"0"*64+b"  ../outside\n", b"0"*64+b"  .env\n"):
            manifest.write_bytes(bad)
            self.assertEqual(self.result()[1], 2)
        manifest.write_bytes(original)
        (self.media / "linked-file").symlink_to("/etc/passwd")
        self.assertEqual(self.result()[1], 2)

    def test_private_env_is_rejected_before_opening_any_payload(self):
        (self.media / "private.env").write_text(SECRET)
        with mock.patch.object(CHECK.os, "open", side_effect=AssertionError("no payload should be opened")) as opened:
            self.assertEqual(self.result()[1], 2)
            opened.assert_not_called()

    def test_assets_cannot_escape_allowlist_or_include_symlinks(self):
        for name, value in (("../outside", b"do not write"), ("bin/.env", SECRET.encode())):
            self.assets[name] = value; self.finish()
            self.assertEqual(self.result()[1], 2)
            del self.assets[name]
        link = tarfile.TarInfo("bin/link"); link.type = tarfile.SYMTYPE; link.linkname = "/etc/passwd"
        self.assets["bin/link"] = link; self.finish()
        self.assertEqual(self.result()[1], 2)

    def outer(self, entries=None):
        path = self.root / "uip-delivery.tar"
        if entries is None:
            entries = [("candidate/"+item.name, item.read_bytes()) for item in sorted(self.media.iterdir())]
        path.write_bytes(tar_bytes(entries))
        path.with_name(path.name+".sha256").write_text(digest(path.read_bytes())+"  "+path.name+"\n")
        return path

    def test_outer_tar_and_directory_share_identity_without_extraction(self):
        outer = self.outer()
        before = sorted(path.name for path in self.root.iterdir())
        directory, code = self.result()
        archived, archived_code = self.result(path=outer)
        self.assertEqual((archived_code, archived), (code, directory))
        self.assertEqual(sorted(path.name for path in self.root.iterdir()), before)

    def test_outer_delivery_directory_may_use_a_safe_chinese_name(self):
        outer = self.outer([("离线交付包/"+path.name, path.read_bytes()) for path in sorted(self.media.iterdir())])
        self.assertEqual(self.result(path=outer), self.result())

    def test_outer_archive_duplicate_mixed_roots_and_links_are_refused(self):
        link = tarfile.TarInfo("candidate/link"); link.type = tarfile.SYMTYPE; link.linkname = "/etc/passwd"
        for entries in ([ ("candidate/a", b"a"), ("other/b", b"b") ], [("candidate/a", b"a"), ("candidate/a", b"b")], [("candidate/link", link)], [("candidate/../../outside", b"bad")]):
            self.assertEqual(self.result(path=self.outer(entries))[1], 2)

    def test_public_package_api_preserves_order_metadata_and_actual_digest(self):
        path = self.media / self.names["common"]
        result = CHECK.read_component_package(path)
        self.assertEqual(result["sha256"], digest(path.read_bytes()))
        self.assertEqual(list(result["images"]), image_tags("common"))
        self.assertEqual(result["metadata"]["COMPONENT"], "common")
        self.assertNotIn(SECRET, json.dumps(result))

    def test_current_four_system_candidate_projects_sixteen_consumers(self):
        for component in ("customer-opportunity", "project", "data-analysis"):
            self.add_package(component)
        self.finish()
        report, code = self.result()
        self.assertEqual(code, 0, report)
        self.assertEqual(report["consumer_count"], 16)
        self.assertEqual(set(report["applications"]), {APP, "customer_and_opportunity", "project_management", "data_analysis"})

    def test_portal_conditional_worker_remains_in_complete_approval_contract(self):
        self.add_package("customer-opportunity")
        self.add_package("customer-portal")
        identity = self.facts["customer-portal"][image_tags("customer-portal")[0]]["config"]
        self.assets["runtime-license-customer_portal-prod.json"] = encoded({"version": 1, "application": "customer_portal", "environment": "prod",
            "components": [{"service_id": name, "protocol": 1, "coverage_digest": "sha256:"+"e"*64, "image_digest": identity}
                           for name in ("portal-api", "portal-report-worker")]})
        self.finish()
        self.assertEqual(self.result()[1], 2)
        value = json.loads(self.assets["runtime-license-customer_portal-prod.json"])
        value["components"].append({"service_id": "portal-invite-compensation-worker", "protocol": 1, "coverage_digest": "sha256:"+"e"*64, "image_digest": identity})
        self.assets["runtime-license-customer_portal-prod.json"] = encoded(value)
        self.finish()
        self.assertEqual(self.result()[1], 0)

    def test_analytics_metabase_is_controlled_infrastructure_with_a_gateway(self):
        self.add_package("data-analysis")
        self.material()
        services = ("data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker")
        identities = [self.facts["data-analysis"][tag]["config"] for tag in image_tags("data-analysis")[:3]]
        self.assets["runtime-license-data_analysis-prod.json"] = encoded({"version": 1, "application": "data_analysis", "environment": "prod",
            "components": [{"service_id": name, "protocol": 1, "coverage_digest": "sha256:"+"f"*64, "image_digest": identity} for name, identity in zip(services, identities)]})
        evidence = json.loads(self.assets["license-evidence.json"])
        evidence["services"] += [{"application": "data_analysis", "environment": "prod", "name": name, "image_digest": identity, "version": VERSION, "protocol": "1", "replicas": 1}
                                 for name, identity in zip(services, identities)]
        metabase = {"application": "data_analysis", "environment": "prod", "name": "data-analysis-metabase", "image_digest": self.facts["common"]["metabase/metabase:v0.53.7"]["config"], "replicas": 1, "guarded_by": "data-analysis-api"}
        evidence["infrastructure"] = [metabase]
        self.assets["license-evidence.json"] = encoded(evidence); self.finish()
        report, code = self.result("install")
        self.assertEqual((report["status"], code), ("READY", 0), report)
        self.assertEqual(report["consumer_count"], 5)
        for changed in ({**evidence, "infrastructure": []}, {**evidence, "excluded_services": ["data-analysis-metabase"]},
                        {**evidence, "infrastructure": [{**metabase, "guarded_by": "contract-api"}]}):
            self.assets["license-evidence.json"] = encoded(changed); self.finish()
            self.assertEqual(self.result()[1], 2)

    def test_current_proxy_metadata_is_paired_and_matches_eighth_real_config(self):
        path = self.media / self.names["common"]
        with tarfile.open(path, "r:gz") as archive:
            entries = {item.name: archive.extractfile(item).read() for item in archive}
        correct = ("DOCKER_SOCKET_PROXY_UPSTREAM_REF=ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459\n"
                   "DOCKER_SOCKET_PROXY_CONFIG_DIGEST="+self.facts["common"][image_tags("common")[7]]["config"]+"\n").encode()
        for suffix, expected in ((correct, 0), (correct.splitlines(True)[0], 2), (correct.replace(b"sha256:"+b"1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459", b"sha256:"+b"0"*64), 2)):
            changed = dict(entries); changed["package.env"] += suffix
            path.write_bytes(tar_bytes(list(changed.items()), True)); self.finish()
            self.assertEqual(self.result()[1], expected)

    def test_explicitly_rebound_valid_release_generation_is_not_falsely_rejected(self):
        self.material()
        path = "runtime-license-"+APP+"-prod.json"
        value = json.loads(self.assets[path]); value["release_generation"] = "contract-r2"
        self.assets[path] = encoded(value); self.finish()
        self.assertEqual(self.result("install")[1], 0)

    def test_host_dependencies_are_read_without_installing_and_inner_hashes_are_required(self):
        entries = {"host-dependencies.env": b"PACKAGE_FORMAT=1\nOS_ID=ubuntu\nOS_VERSION_ID=24.04\nARCHITECTURE=amd64\n",
                   "debs/python3_3.12_amd64.deb": b"fixture-package-not-installed"}
        manifest = "".join(digest(raw)+"  "+name+"\n" for name, raw in entries.items()).encode()
        path = self.media / "host-dependencies-ubuntu-24.04-linux-amd64.tar.gz"
        path.write_bytes(tar_bytes([*entries.items(), ("SHA256SUMS", manifest)], True)); self.finish()
        report, code = self.result()
        self.assertEqual(code, 0, report)
        self.assertTrue(any(row["id"] == "package.host-dependencies" and row["status"] == "PASS" for row in report["checks"]))
        entries["debs/python3_3.12_amd64.deb"] += b"corrupt"
        path.write_bytes(tar_bytes([*entries.items(), ("SHA256SUMS", manifest)], True)); self.finish()
        self.assertEqual(self.result()[1], 2)

    def test_documented_workflow_is_an_installable_public_asset(self):
        self.assets["DELIVERY_WORKFLOW.md"] = b"Structural READY still requires platform authority.\n"
        self.finish()
        self.assertEqual(self.result()[1], 0)

    def test_legacy_assets_report_missing_recovery_tools_before_installation(self):
        self.material()
        del self.assets['bin/deployment-session.py']
        self.finish()
        report, code = self.result('prepare')
        self.assertEqual((report['status'], code), ('NEEDS_INPUT', 0))
        self.assertTrue(any(row['id']=='assets.recovery-tools' and row['status']=='PENDING' for row in report['checks']))
        self.assertTrue(any(row['action']=='REBUILD_MATCHING_BASELINE' and row['location']=='MAC' for row in report['next_actions']))
        self.assertEqual(self.result('install')[1], 3)

    def test_cumulative_pax_extensions_have_a_bounded_budget(self):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode='w', format=tarfile.PAX_FORMAT) as archive:
            for name in ('first', 'second'):
                member = tarfile.TarInfo(name)
                member.size = 1
                member.pax_headers = {'comment': 'x' * 100}
                archive.addfile(member, io.BytesIO(b'x'))
        raw = output.getvalue()
        with tarfile.open(fileobj=io.BytesIO(raw), mode='r:', tarinfo=CHECK.IMAGES.SafeTarInfo) as archive:
            self.assertEqual([member.name for member in archive], ['first', 'second'])
        with mock.patch.object(CHECK.IMAGES, 'PAX_TOTAL_LIMIT', 150):
            with self.assertRaisesRegex(CHECK.Invalid, 'cumulative extension'):
                with tarfile.open(fileobj=io.BytesIO(raw), mode='r:', tarinfo=CHECK.IMAGES.SafeTarInfo) as archive:
                    list(archive)

    def test_cli_json_and_human_do_not_execute_any_external_command_or_echo_private_claims(self):
        self.material()
        tools = self.root / "tools"; tools.mkdir()
        marker = self.root / "executed-command"
        for name in ("docker", "sh", "bash", "go", "systemctl", "containerd", "tar", "sha256sum"):
            path = tools / name
            path.write_text("#!/bin/sh\nprintf called > "+shlex.quote(str(marker))+"\nexit 99\n")
            path.chmod(0o755)
        env = dict(os.environ, PATH=str(tools), PYTHONDONTWRITEBYTECODE="1")
        for format in ("json", "human"):
            result = subprocess.run([sys.executable, str(CHECKER), "--media", str(self.media), "--phase", "install", "--format", format], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            self.assertNotIn(SECRET, result.stdout+result.stderr)
            self.assertNotIn(self.assets["license/commercial-license.jws"].decode(), result.stdout+result.stderr)
            if format == "json":
                self.assertEqual(json.loads(result.stdout)["status"], "READY")
        self.assertFalse(marker.exists())

    def test_invalid_release_integrity_cannot_be_ignored_as_legacy(self):
        self.finish()
        (self.media / "release-manifest.json").write_text("{}\n", encoding="utf-8")
        (self.media / "release-manifest.sigstore.json").write_text("{}\n", encoding="utf-8")
        for directory, name in (("attestations", "contract.intoto.json"), ("profiles", "contract.yaml")):
            (self.media / directory).mkdir()
            (self.media / directory / name).write_text("proof\n", encoding="utf-8")
        report, code = self.result("prepare")
        self.assertEqual(code, 2, report)
        self.assertEqual(report["status"], "INVALID")
        self.assertTrue(any(row["id"] == "release.structure" and row["status"] == "FAIL" for row in report["checks"]))


if __name__ == "__main__":
    unittest.main()
