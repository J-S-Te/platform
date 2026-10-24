#!/usr/bin/env python3
"""V2 结构兼容：真实 TAR/OCI 字节夹具，签名及业务授权始终留给权威验证器。"""
import copy
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("delivery_legacy_fixtures", Path(__file__).with_name("test-delivery-inspect.py"))
FIXTURE = importlib.util.module_from_spec(spec)
spec.loader.exec_module(FIXTURE)
CHECK = FIXTURE.CHECK


def fixture_bundle(payload, *, dsse=False):
    # 只构造合法封装；该 dummy 签名未经任何受信密钥签发，检查结果必须保持验签 PENDING。
    signature = base64.b64encode(b"unverified-fixture-signature").decode()
    result = {"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}
    if dsse:
        result["dsseEnvelope"] = {"payloadType": "application/vnd.in-toto+json",
                                  "payload": base64.b64encode(payload).decode(),
                                  "signatures": [{"keyid": "untrusted-fixture", "sig": signature}]}
    else:
        result["messageSignature"] = {"messageDigest": {"algorithm": "SHA2_256", "digest": base64.b64encode(hashlib.sha256(payload).digest()).decode()},
                                      "signature": signature}
    return FIXTURE.encoded(result)


class V2DeliveryTests(unittest.TestCase):
    setUp = FIXTURE.DeliveryTests.setUp
    add_package = FIXTURE.DeliveryTests.add_package
    material = FIXTURE.DeliveryTests.material
    result = FIXTURE.DeliveryTests.result

    def finish(self):
        # finalize 在总 SHA 生成后增加证明文件，证明由 release manifest 自身绑定。
        proofs = {"release-manifest.json", "release-manifest.sigstore.json"}
        held = {}
        for path in list(self.media.iterdir()):
            if path.is_dir() or path.name in proofs:
                held[path.name] = path
        if held:
            # 复用既有资产构造器，不把新目录喂给其仅用于 V1 的 flat SHA 枚举。
            asset_name = "deployment-assets-" + FIXTURE.VERSION + ".tar.gz"
            (self.media / asset_name).write_bytes(FIXTURE.tar_bytes(list(self.assets.items()), True))
            compatibility = "FORMAT=1\nPLATFORM_ASSETS_INPUT_SHA256=" + "b" * 64 + "\n"
            for key, name in (("COMMON", self.names["common"]), ("PLATFORM", self.names["platform"]), ("FRONTEND", self.names["frontend"]), ("ASSETS", asset_name)):
                compatibility += key + "_PACKAGE=" + name + "\n" + key + "_PACKAGE_SHA256=" + FIXTURE.digest((self.media / name).read_bytes()) + "\n"
            (self.media / "PLATFORM_ASSETS_COMPATIBILITY.env").write_text(compatibility)
            for path in self.media.iterdir():
                if path.is_file() and path.name.endswith(".tar.gz"):
                    path.with_name(path.name + ".sha256").write_text(FIXTURE.digest(path.read_bytes()) + "  " + path.name + "\n")
            files = sorted(path for path in self.media.iterdir() if path.is_file() and path.name not in proofs | {"SHA256SUMS"})
            (self.media / "SHA256SUMS").write_text("".join(FIXTURE.digest(path.read_bytes()) + "  " + path.name + "\n" for path in files))
        else:
            FIXTURE.DeliveryTests.finish(self)

    def v2(self, *, protocols=None, materials=True):
        if materials:
            self.material()
            self.assets.pop("runtime-license-contract_management-prod.json", None)
            self.assets.pop("license-evidence.json", None)
        self.finish()
        protocols = protocols or {CHECK.APPS[component]: 2 for component in self.names if component in CHECK.APPS}
        for folder in ("profiles", "attestations"):
            (self.media / folder).mkdir(exist_ok=True)
        images, by_tag = [], {}
        for component, facts in self.facts.items():
            for index, (tag, fact) in enumerate(facts.items()):
                name = component.replace("-", "_") + "_" + str(index)
                by_tag[tag] = name
                statement = "attestations/" + name + ".intoto.json"
                bundle = "attestations/" + name + ".intoto.sigstore.json"
                statement_raw = FIXTURE.encoded({"_type": "https://in-toto.io/Statement/v1", "predicateType": "https://slsa.dev/provenance/v1",
                                                 "subject": [{"name": name, "digest": {"sha256": fact["config"][7:]}}],
                                                 "predicate": {"fixture": "unverified-build-provenance"}})
                (self.media / statement).write_bytes(statement_raw)
                (self.media / bundle).write_bytes(fixture_bundle(statement_raw, dsse=True))
                images.append({"name": name, "platform": "linux/amd64", "source": "built", "package": self.names[component],
                               "archive_member": "images.tar", "config_digest": fact["config"], "manifest_digest": fact["manifest"],
                               "provenance": statement, "provenance_bundle": bundle})
        profiles = []
        parser = CHECK.sibling("subsystem-runtime.py")
        applications = []
        for app, filename in FIXTURE.PROFILES.items():
            raw = self.assets["subsystems.d/" + filename]
            (self.media / "profiles" / filename).write_bytes(raw)
            profiles.append({"path": "profiles/" + filename, "sha256": FIXTURE.digest(raw), "normalized_sha256": FIXTURE.digest(raw), "policy_version": 1})
            if app not in protocols:
                continue
            compose, conditional, _ = parser.projection(raw.decode(), app)
            components = []
            component = CHECK.APP_COMPONENT[app]
            for service in compose["runtime_services"]:
                role = "infrastructure" if service == "data-analysis-metabase" else "api" if service.endswith("-api") else "worker"
                tag = "metabase/metabase:v0.53.7" if service == "data-analysis-metabase" else FIXTURE.image_tags(component)[
                    {"data-analysis-api": 0, "data-analysis-aggregation-worker": 1, "data-analysis-alert-worker": 2}.get(service, 0)]
                components.append({"service_id": service, "role": role, "conditional": service in conditional, "image": by_tag[tag],
                                   "registration_protocol": protocols[app], "catalog_version": 1, "replicas": 1, "command_policy": "locked"})
            applications.append({"code": app, "environment": "prod", "components": components})
        self.release = {"schema": "urn:uip:release-manifest:v1", "release_id": FIXTURE.VERSION, "channel": "production", "release_sequence": 1,
                        "target_platform": "linux/amd64", "build_inputs_sha256": "a" * 64,
                        "compatibility": {"verifier_policy": "vendor-key-offline-v1", "min_api_release_integrity": "release-integrity-v1",
                                          "min_agent_release_integrity": "release-integrity-v1", "catalog_version": 1,
                                          "registration_protocols": protocols, "shared_control_plane_sha256": "b" * 64},
                        "artifacts": [], "profiles": profiles, "applications": applications, "images": images}
        self.rebind_artifacts()
        self.write_release()
        return self.release

    def rebind_artifacts(self):
        self.release["artifacts"] = [{"path": path.relative_to(self.media).as_posix(), "sha256": FIXTURE.digest(path.read_bytes()), "size": path.stat().st_size,
                                      "type": "other"} for path in sorted(self.media.rglob("*")) if path.is_file() and path.name not in {"release-manifest.json", "release-manifest.sigstore.json"}]

    def write_release(self):
        raw = FIXTURE.encoded(self.release)
        (self.media / "release-manifest.json").write_bytes(raw)
        (self.media / "release-manifest.sigstore.json").write_bytes(fixture_bundle(raw))

    def test_corrupt_proof_json_or_wrong_statement_bundle_shape_is_refused(self):
        original = copy.deepcopy(self.v2())
        statement = self.media / original["images"][0]["provenance"]
        dsse = self.media / original["images"][0]["provenance_bundle"]
        release_signature = self.media / "release-manifest.sigstore.json"
        saved = {path: path.read_bytes() for path in (statement, dsse, release_signature)}
        mutations = [
            (path, value) for path in saved for value in
            (b"not-json", b"[]", b'{"mediaType":"a","mediaType":"b"}')
        ]
        mutations.extend([
            (statement, FIXTURE.encoded({"_type": "wrong", "predicateType": "https://slsa.dev/provenance/v1", "subject": []})),
            (dsse, FIXTURE.encoded({"mediaType": "invalid", "dsseEnvelope": {}})),
            (dsse, fixture_bundle(b"different statement", dsse=True)),
        ])
        missing_signature = json.loads(saved[dsse]);missing_signature["dsseEnvelope"]["signatures"] = []
        mutations.append((dsse, FIXTURE.encoded(missing_signature)))
        wrong_key_type = json.loads(saved[dsse]);wrong_key_type["dsseEnvelope"]["signatures"][0]["keyid"] = 3
        mutations.append((dsse, FIXTURE.encoded(wrong_key_type)))
        wrong_material = json.loads(saved[dsse]);wrong_material["verificationMaterial"] = {"tlogEntries": "wrong-type"}
        mutations.append((dsse, FIXTURE.encoded(wrong_material)))
        for path, value in mutations:
            with self.subTest(path=path.name, value=value[:20]):
                for item, raw in saved.items():item.write_bytes(raw)
                self.release = copy.deepcopy(original)
                path.write_bytes(value)
                self.rebind_artifacts()
                self.write_release()
                if path == release_signature:path.write_bytes(value)
                report, code = self.result("install")
                self.assertEqual(code, 2, report)
                self.assertNotIn("VERIFIED", json.dumps(report))
        for item, raw in saved.items():item.write_bytes(raw)
        self.release = copy.deepcopy(original)
        self.rebind_artifacts();self.write_release()
        self.assertEqual(self.result("install")[1], 0)

    def outer(self, extra=()):
        entries = [("candidate/" + path.relative_to(self.media).as_posix(), path.read_bytes()) for path in sorted(self.media.rglob("*")) if path.is_file()]
        path = self.root / "signed-delivery.tar"
        path.write_bytes(FIXTURE.tar_bytes(entries + list(extra)))
        path.with_name(path.name + ".sha256").write_text(FIXTURE.digest(path.read_bytes()) + "  " + path.name + "\n")
        return path

    def test_v2_target_uses_catalog_structure_without_requiring_v1_approval_or_target_evidence(self):
        self.v2()
        report, code = self.result("install")
        self.assertEqual((code, report["status"]), (0, "READY"), report)
        checks = {row["id"]: row for row in report["checks"]}
        self.assertEqual(checks["authorization.release.contract_management"]["status"], "PASS")
        self.assertNotIn("authorization.evidence", checks)
        self.assertEqual(checks["authority.platform"]["status"], "PENDING")
        self.assertTrue(any(row["scope"] == "VERIFIER" and row["status"] == "PENDING" for row in report["checks"]))
        self.assertNotIn("VERIFIED", json.dumps(report))
        self.assertNotIn(FIXTURE.SECRET, json.dumps(report))

    def test_v2_outer_tar_and_directory_have_identical_reports_without_extraction(self):
        self.v2()
        outer = self.outer()
        before = sorted(path.relative_to(self.root).as_posix() for path in self.root.rglob("*"))
        self.assertEqual(self.result("install", outer), self.result("install"))
        self.assertEqual(before, sorted(path.relative_to(self.root).as_posix() for path in self.root.rglob("*")))

    def test_v2_explicit_plan_still_requires_independent_old_installation_baseline(self):
        self.assets.pop("license-migration-evidence.json", None)
        self.v2()
        self.assets.pop("license-migration-evidence.json")
        self.finish()
        self.rebind_artifacts()
        self.write_release()
        report, code = self.result("install")
        self.assertEqual((report["status"], code), ("NEEDS_INPUT", 3), report)
        self.assertTrue(any(row["id"] == "authorization.baseline" and row["status"] == "PENDING" for row in report["checks"]))

    def test_v2_fresh_baseline_cannot_contain_existing_business(self):
        self.v2()
        self.assets["license-migration-evidence.json"] = FIXTURE.encoded({"version": 1, "project": "fixture-project", "installation_boundary": True,
            "services": [{"application": "contract_management", "environment": "prod", "name": "contract-api", "image_digest": "sha256:" + "a" * 64,
                          "version": FIXTURE.VERSION, "protocol": "1", "replicas": 1}], "excluded_services": []})
        self.finish()
        self.rebind_artifacts()
        self.write_release()
        self.assertEqual(self.result()[1], 2)

    def test_cross_application_v1_and_v2_use_their_own_material_requirements(self):
        self.add_package("project")
        self.material()
        self.v2(protocols={"contract_management": 1, "project_management": 2}, materials=False)
        report, code = self.result("install")
        self.assertEqual((report["status"], code), ("READY", 0), report)
        self.assertTrue(any(row["id"] == "authorization.evidence" and row["status"] == "PASS" for row in report["checks"]))
        self.assets.pop("runtime-license-contract_management-prod.json")
        self.finish()
        self.rebind_artifacts()
        self.write_release()
        report, code = self.result("install")
        self.assertEqual((report["status"], code), ("NEEDS_INPUT", 3), report)

    def test_manifest_protocol_and_component_contradictions_fail_closed(self):
        original = copy.deepcopy(self.v2())
        changes = [
            lambda value: value.update(schema="urn:uip:release-manifest:v999"),
            lambda value: value.update(unrecognized=FIXTURE.SECRET),
            lambda value: value["compatibility"]["registration_protocols"].update(contract_management=9),
            lambda value: value["compatibility"].update(catalog_version=2),
            lambda value: value["applications"][0]["components"][0].update(registration_protocol=1),
            lambda value: value["applications"][0]["components"][0].update(catalog_version=2),
            lambda value: value["applications"][0]["components"].pop(),
            lambda value: value["applications"][0]["components"].append(copy.deepcopy(value["applications"][0]["components"][0])),
            lambda value: value["applications"][0]["components"][0].update(service_id="contract-unregistered-worker"),
            lambda value: value["applications"][0]["components"][0].update(role="infrastructure"),
            lambda value: value["applications"][0]["components"][0].update(conditional=True),
            lambda value: value["applications"][0]["components"][0].update(replicas=2),
            lambda value: value["applications"][0]["components"][0].update(replicas=True),
            lambda value: value["applications"][0]["components"][0].update(image="platform_0"),
            lambda value: value["compatibility"].update(shared_control_plane_sha256="not-a-sha256"),
            lambda value: value.update(build_inputs_sha256="0" * 64),
            lambda value: value.update(release_id="invalid/release"),
            lambda value: value["artifacts"].pop(),
        ]
        for mutate in changes:
            self.release = copy.deepcopy(original)
            mutate(self.release)
            self.write_release()
            report, code = self.result()
            self.assertEqual(code, 2, report)
            self.assertNotIn(FIXTURE.SECRET, json.dumps(report))

    def test_duplicate_manifest_fields_and_legacy_compatibility_unknown_fields_are_refused(self):
        self.v2()
        (self.media / "release-manifest.json").write_bytes(b'{"schema":"urn:uip:release-manifest:v1","schema":"urn:uip:release-manifest:v1"}')
        self.assertEqual(self.result()[1], 2)
        self.release["legacy_compatibility"] = {"v1_approval_sha256": "a" * 64, "purpose": "bridge", "unknown": True}
        self.write_release()
        self.assertEqual(self.result()[1], 2)

    def test_six_raw_normalized_and_asset_profiles_are_all_bound(self):
        original = copy.deepcopy(self.v2())
        for field in ("sha256", "normalized_sha256", "policy_version"):
            self.release = copy.deepcopy(original)
            self.release["profiles"][0][field] = 2 if field == "policy_version" else "0" * 64
            self.write_release()
            self.assertEqual(self.result()[1], 2)
        self.release = copy.deepcopy(original)
        self.release["profiles"].pop()
        self.write_release()
        self.assertEqual(self.result()[1], 2)
        self.release = copy.deepcopy(original)
        path = self.media / self.release["profiles"][0]["path"]
        path.write_bytes(path.read_bytes() + b"\n# changed source profile\n")
        for field in ("sha256", "normalized_sha256"):
            self.release["profiles"][0][field] = FIXTURE.digest(path.read_bytes())
        self.rebind_artifacts()
        self.write_release()
        self.assertEqual(self.result()[1], 2)

    def test_image_typed_identity_and_artifact_sha_size_bind_real_oci_bytes(self):
        original = copy.deepcopy(self.v2())
        mutations = [lambda value: value["images"][-1].update(config_digest="sha256:" + "0" * 64),
                     lambda value: value["images"][-1].update(manifest_digest="sha256:" + "0" * 64),
                     lambda value: value["images"][-1].update(package=self.names["platform"]),
                     lambda value: value["images"][-1].update(archive_member="outside.tar"),
                     lambda value: value["artifacts"][0].update(sha256="0" * 64),
                     lambda value: value["artifacts"][0].update(size=1),
                     lambda value: value["artifacts"].append(copy.deepcopy(value["artifacts"][0]))]
        for mutate in mutations:
            self.release = copy.deepcopy(original)
            mutate(self.release)
            self.write_release()
            self.assertEqual(self.result()[1], 2)

    def test_conditional_portal_worker_and_metabase_infrastructure_are_not_dropped(self):
        for component in ("customer-opportunity", "customer-portal", "data-analysis"):
            self.add_package(component)
        original = copy.deepcopy(self.v2())
        report, code = self.result()
        self.assertEqual(code, 0, report)
        self.assertEqual(report["consumer_count"], 17)
        for app, service in (("customer_portal", "portal-invite-compensation-worker"), ("data_analysis", "data-analysis-metabase")):
            self.release = copy.deepcopy(original)
            application = next(value for value in self.release["applications"] if value["code"] == app)
            application["components"] = [value for value in application["components"] if value["service_id"] != service]
            self.write_release()
            self.assertEqual(self.result()[1], 2)

    def test_proof_paths_types_duplicates_and_snapshot_changes_remain_unsafe(self):
        self.v2()
        proof = "candidate/profiles/contract_management-prod.yaml"
        for name in ("candidate/profiles/../outside", "candidate/profiles/private.env", "candidate/other/file.json", "candidate/profiles/deeper/file.yaml"):
            self.assertEqual(self.result(path=self.outer([(name, b"unsafe")]))[1], 2)
        self.assertEqual(self.result(path=self.outer([(proof, b"duplicate")]))[1], 2)
        link = tarfile.TarInfo("candidate/attestations/link.intoto.json")
        link.type, link.linkname = tarfile.SYMTYPE, "/etc/passwd"
        self.assertEqual(self.result(path=self.outer([(link.name, link)]))[1], 2)
        media = CHECK.DirectoryMedia(self.media)
        self.addCleanup(media.close)
        path = self.media / "profiles/contract_management-prod.yaml"
        path.write_bytes(path.read_bytes() + b"\n")
        with self.assertRaises(CHECK.Invalid):
            media.verify()

    def test_v2_inspection_does_not_invoke_media_or_external_tools(self):
        self.v2()
        tools = self.root / "tools"
        tools.mkdir()
        marker = self.root / "unexpected-command"
        for name in ("go", "docker", "bash", "sh", "tar", "release-verifier"):
            path = tools / name
            path.write_text("#!/bin/sh\nexit 99\n")
            path.chmod(0o755)
        self.assets["bin/deploy.sh"] = ("#!/bin/sh\ntouch " + str(marker) + "\n").encode()
        self.finish()
        self.rebind_artifacts()
        self.write_release()
        completed = subprocess.run([sys.executable, str(FIXTURE.CHECKER), "--media", str(self.media), "--phase", "install", "--format", "json"],
                                   env=dict(os.environ, PATH=str(tools), PYTHONDONTWRITEBYTECODE="1"), capture_output=True, text=True)
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertEqual(json.loads(completed.stdout)["status"], "READY")
        self.assertNotIn(FIXTURE.SECRET, completed.stdout + completed.stderr)
        self.assertFalse(marker.exists())

    def test_v2_proof_metadata_limits_and_symlinked_directories_remain_rejected(self):
        self.v2()
        oversized = tarfile.TarInfo("candidate/attestations/oversized.intoto.json")
        oversized.size = 8 * 1024 * 1024 + 1
        self.assertEqual(self.result(path=self.outer([(oversized.name, b"0" * oversized.size)]))[1], 2)
        directory = self.media / "profiles"
        renamed = self.media / "old-profiles"
        directory.rename(renamed)
        directory.symlink_to(renamed, target_is_directory=True)
        self.assertEqual(self.result()[1], 2)

    def test_python_structure_matches_go_schema_and_authoritative_component_catalog(self):
        if shutil.which("go") is None:
            self.skipTest("Go is only a development oracle; production inspection does not require it")
        self.add_package("project")
        self.material()
        self.v2(protocols={"contract_management": 1, "project_management": 2}, materials=False)
        # 这两个身份/输入在 Go 契约中独立于软件版本与资产输入，不应自行强制相等。
        self.release["release_id"] = "independent-release-id"
        self.release["compatibility"]["shared_control_plane_sha256"] = "f" * 64
        self.release["legacy_compatibility"] = {"v1_approval_sha256": "", "purpose": "bridge prerequisites still require platform validation"}
        self.write_release()
        # 隐藏临时包留在 Go internal 的允许父目录内；构建产物和缓存不进入业务目录。
        platform = FIXTURE.PRODUCTION.parents[1]
        with tempfile.TemporaryDirectory(prefix=".delivery-inspect-oracle-", dir=platform) as source_dir:
            source = Path(source_dir) / "main.go"
            executable = self.root / "manifest-oracle"
            source.write_text('''package main
import (
 "encoding/json"
 "io"
 "os"
 "github.com/J-S-Te/Basic-Platform/internal/shared/releaseintegrity"
 "github.com/J-S-Te/license-core/catalog"
)
func main() {
 raw, _ := io.ReadAll(os.Stdin)
 _, err := releaseintegrity.ParseManifest(raw, releaseintegrity.DefaultValidationOptions())
 components := map[string][]catalog.Component{}
 for _, app := range []string{"contract_management","customer_and_opportunity","customer_portal","data_analysis","project_management","settlement"} {
  components[app], _ = catalog.Components(app)
 }
 json.NewEncoder(os.Stdout).Encode(map[string]any{"valid":err==nil,"catalog_version":catalog.Version,"components":components})
}
''')
            environment = dict(os.environ, GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local", GOCACHE="/tmp/delivery-inspect-v2-go-cache")
            built = subprocess.run(["go", "build", "-mod=vendor", "-o", str(executable), str(source)], cwd=platform, env=environment, capture_output=True, text=True, timeout=60)
            self.assertEqual(built.returncode, 0, built.stderr)
            output = subprocess.run([str(executable)], input=FIXTURE.encoded(self.release), capture_output=True, timeout=10)
            self.assertEqual(output.returncode, 0, output.stderr)
            oracle = json.loads(output.stdout)
            self.assertTrue(oracle["valid"])
            self.assertEqual(self.result("install")[1], 0)
            self.assertEqual(oracle["catalog_version"], 1)
            helper = CHECK.sibling("delivery-inspect-v2.py")
            self.assertEqual({app: set(services) for app, services in helper.CATALOG.items()},
                             {app: {item["ServiceID"] for item in services} for app, services in oracle["components"].items()})
            for services in oracle["components"].values():
                for item in services:
                    self.assertEqual(helper.role(item["ServiceID"]), item["Role"].lower())
                    self.assertEqual(item["Conditional"], item["ServiceID"] == "portal-invite-compensation-worker")


if __name__ == "__main__":
    unittest.main()
