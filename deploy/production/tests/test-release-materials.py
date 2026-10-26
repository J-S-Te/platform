#!/usr/bin/env python3
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve()
TOOL = HERE.parent.parent / "bin" / "release-materials.py"
spec = importlib.util.spec_from_file_location("release_materials", TOOL)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def write(path, value):
    Path(path).write_text(json.dumps(value), encoding="utf-8")
    return str(path)


def spec_value(protocols):
    return {
        "schema": "urn:uip:release-spec:v1",
        "applications": [
            {"code": app, "components": [
                {"service_id": app + "-api", "registration_protocol": protocol,
                 "catalog_version": 1, "replicas": 1, "command_policy": "locked"}
            ]}
            for app, protocol in protocols.items()
        ],
    }


class ReleaseMaterialsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def plan(self, scenario, protocols, **kwargs):
        spec_path = kwargs.pop("spec_path", None)
        if spec_path is None:
            spec_path = write(self.root / "spec.json", spec_value(protocols))
        argv = ["--scenario", scenario, "--spec", spec_path, "--format", "json"]
        for key, value in kwargs.items():
            if value is True:
                argv.append("--" + key.replace("_", "-"))
            elif value is not None:
                argv.extend(["--" + key.replace("_", "-"), str(value)])
        document = module.decide(module.argparse.Namespace(
            scenario=scenario, spec=spec_path, component=kwargs.get("component", []),
            license=kwargs.get("license"), approval_dir=kwargs.get("approval_dir"),
            old_install_evidence=kwargs.get("old_install_evidence"),
            entitlement=kwargs.get("entitlement"), frozen_dir=kwargs.get("frozen_dir"),
            release_manifest=kwargs.get("release_manifest"),
            backup_evidence=kwargs.get("backup_evidence"),
            compatibility=kwargs.get("compatibility"),
            build_evidence=kwargs.get("build_evidence"),
            source_freeze=kwargs.get("source_freeze", False),
        ))
        return document

    def material(self, document, material_id):
        return next(item for item in document["materials"] if item["id"] == material_id)

    def test_plan_output_is_atomic_and_not_overwritten(self):
        spec_path = write(self.root / "spec.json", spec_value({"contract_management": 2}))
        plan_path = self.root / "plan.json"
        with tempfile.TemporaryDirectory() as output:
            result = module.main(["--scenario", "update", "--spec", spec_path,
                                  "--component", "contract", "--format", "json",
                                  "--output", str(Path(output) / "plan.json")])
            self.assertEqual(3, result)
            self.assertTrue((Path(output) / "plan.json").is_file())
            self.assertFalse((Path(output) / ".plan.json.tmp").exists())
        result = module.main(["--scenario", "update", "--spec", spec_path,
                              "--component", "contract", "--format", "json",
                              "--output", str(plan_path)])
        self.assertEqual(3, result)
        self.assertEqual(2, module.main(["--scenario", "update", "--spec", spec_path,
                                         "--component", "contract", "--format", "json",
                                         "--output", str(plan_path)]))

    def test_v2_update_does_not_require_v1_approval(self):
        document = self.plan("update", {"contract_management": 2}, source_freeze=True,
                             build_evidence=write(self.root / "evidence.json", {}),
                             release_manifest=write(self.root / "manifest.json", {}),
                             backup_evidence=write(self.root / "backup.json", {}),
                             compatibility=write(self.root / "compat.json", {}))
        self.assertEqual("READY", document["overall_status"])
        self.assertEqual("NOT_APPLICABLE", self.material(document, "runtime-approval:contract_management")["requirement"])
        self.assertEqual("NOT_APPLICABLE", self.material(document, "commercial-license")["requirement"])

    def test_v1_update_requires_matching_approval(self):
        document = self.plan("update", {"contract_management": 1}, source_freeze=True,
                             build_evidence=write(self.root / "evidence.json", {}),
                             release_manifest=write(self.root / "manifest.json", {}),
                             backup_evidence=write(self.root / "backup.json", {}),
                             compatibility=write(self.root / "compat.json", {}))
        self.assertEqual("NEEDS_ACTION", document["overall_status"])
        self.assertTrue(self.material(document, "runtime-approval:contract_management")["requirement"] == "REQUIRED")
        approval = self.root / "approval"
        approval.mkdir()
        write(approval / "runtime-license-contract_management-prod.json", {})
        document = self.plan("update", {"contract_management": 1}, source_freeze=True,
                             build_evidence=write(self.root / "evidence.json", {}),
                             release_manifest=write(self.root / "manifest.json", {}),
                             backup_evidence=write(self.root / "backup.json", {}),
                             compatibility=write(self.root / "compat.json", {}),
                             approval_dir=approval)
        self.assertEqual("READY", document["overall_status"])

    def test_mixed_protocols_are_independent(self):
        document = self.plan("update", {"contract_management": 1, "project_management": 2},
                             source_freeze=True,
                             build_evidence=write(self.root / "evidence.json", {}),
                             release_manifest=write(self.root / "manifest.json", {}),
                             backup_evidence=write(self.root / "backup.json", {}),
                             compatibility=write(self.root / "compat.json", {}))
        self.assertEqual("NEEDS_ACTION", document["overall_status"])
        self.assertEqual("REQUIRED", self.material(document, "runtime-approval:contract_management")["requirement"])
        self.assertEqual("NOT_APPLICABLE", self.material(document, "runtime-approval:project_management")["requirement"])

    def test_unknown_protocol_blocks(self):
        spec = self.root / "unknown.json"
        write(spec, {"schema": "urn:uip:release-spec:v1", "applications": [
            {"code": "contract_management", "components": [
                {"service_id": "contract-api", "registration_protocol": 9, "catalog_version": 1,
                 "replicas": 1, "command_policy": "locked"}]}
        ]})
        document = self.plan("update", {}, spec_path=str(spec))
        self.assertEqual("BLOCKED", document["overall_status"])

    def test_license_renew_does_not_build_or_require_approvals(self):
        license_path = write(self.root / "license.jws", {"fixture": True})
        document = self.plan("license-renew", {}, license=license_path)
        self.assertFalse(document["build_required"])
        self.assertEqual("READY", document["overall_status"])
        self.assertEqual([], document["applications"])

    def test_sign_only_requires_frozen_artifacts(self):
        document = self.plan("sign-only", {}, release_manifest=write(self.root / "manifest.json", {}))
        self.assertEqual("NEEDS_ACTION", document["overall_status"])
        self.assertFalse(document["build_required"])
        self.assertEqual("REQUIRED", self.material(document, "frozen-artifacts")["requirement"])
        document = self.plan("sign-only", {}, release_manifest=write(self.root / "manifest.json", {}),
                             frozen_dir=self.root)
        self.assertEqual("READY", document["overall_status"])

    def test_migrate_requires_old_evidence_and_license(self):
        document = self.plan("migrate", {"contract_management": 2})
        self.assertIn(document["overall_status"], ("BLOCKED", "NEEDS_ACTION"))
        self.assertEqual("REQUIRED", self.material(document, "old-install-evidence")["requirement"])
        self.assertEqual("REQUIRED", self.material(document, "commercial-license")["requirement"])


if __name__ == "__main__":
    unittest.main()
