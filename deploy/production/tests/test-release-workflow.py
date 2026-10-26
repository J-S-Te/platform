#!/usr/bin/env python3
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve()
TOOL = HERE.parent.parent / "bin" / "release-workflow.py"
spec = importlib.util.spec_from_file_location("release_workflow", TOOL)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ReleaseWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_package_verified_directory_without_rebuild(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "BUILD_INFO.txt").write_text("RELEASE_VERSION=test-v1\n", encoding="utf-8")
        import hashlib
        lines = []
        for name in ("a.txt", "BUILD_INFO.txt"):
            lines.append(hashlib.sha256((output / name).read_bytes()).hexdigest() + "  " + name)
        (output / "SHA256SUMS").write_text("\n".join(lines) + "\n", encoding="utf-8")
        args = type("Args", (), {"output": str(output), "version": None, "allow_legacy_unsigned": True})()
        self.assertEqual(0, module.package(args))
        archive = self.root / "uip-offline-delivery-test-v1-linux-amd64.tar"
        self.assertTrue(archive.is_file())
        self.assertTrue(Path(str(archive) + ".sha256").is_file())
        self.assertEqual(2, module.package(args))

    def test_package_rejects_bad_manifest(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "SHA256SUMS").write_text("0" * 64 + "  a.txt\n", encoding="utf-8")
        args = type("Args", (), {"output": str(output), "version": "v1", "allow_legacy_unsigned": True})()
        self.assertEqual(2, module.package(args))
        self.assertFalse((self.root / "uip-offline-delivery-v1-linux-amd64.tar").exists())

    def test_diff_reports_added_removed_changed(self):
        before, after = self.root / "before", self.root / "after"
        before.mkdir(); after.mkdir()
        (before / "a").write_text("a", encoding="utf-8")
        (before / "b").write_text("b", encoding="utf-8")
        (after / "a").write_text("a2", encoding="utf-8")
        (after / "c").write_text("c", encoding="utf-8")
        args = type("Args", (), {"before": str(before), "after": str(after), "format": "json"})()
        import contextlib, io
        stream = io.StringIO()
        with contextlib.redirect_stdout(stream):
            self.assertEqual(0, module.diff(args))
        value = json.loads(stream.getvalue())
        self.assertEqual(["c"], value["added"])
        self.assertEqual(["b"], value["removed"])
        self.assertEqual(["a"], value["changed"])

    def test_review_writes_v2_schema(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "release-plan.json").write_text(json.dumps({"schema": "uip.release-materials/v1", "overall_status": "READY"}), encoding="utf-8")
        args = type("Args", (), {"output": str(output), "operator": "qa", "purpose": "release", "confirm": True, "before": None})()
        self.assertEqual(0, module.review(args))
        value = json.loads((output / "release-review.json").read_text(encoding="utf-8"))
        self.assertEqual("uip.release-review/v2", value["schema"])

    def test_review_binds_candidate_and_detects_change(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "release-plan.json").write_text(json.dumps({"schema": "uip.release-materials/v1", "overall_status": "READY"}), encoding="utf-8")
        args = type("Args", (), {"output": str(output), "operator": "qa", "purpose": "release", "confirm": True})()
        self.assertEqual(0, module.review(args))
        self.assertEqual("", module.validate_review(output))
        (output / "a.txt").write_text("changed\n", encoding="utf-8")
        self.assertIn("未绑定当前候选", module.validate_review(output))

    def test_review_binds_existing_formal_review_files(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "release-plan.json").write_text(json.dumps({"schema": "uip.release-materials/v1", "overall_status": "READY"}), encoding="utf-8")
        (output / "approval-review.json").write_text(json.dumps({"operator": "qa", "candidate": False, "created_at": "2026-10-25T00:00:00Z", "input_reviewed": True}), encoding="utf-8")
        upgrade = {"version": 1, "inventory_sha256": "a"*64, "mapping_sha256": "b"*64,
                   "registry_sha256": "c"*64, "target_facts_sha256": "d"*64, "expected_sha256": "e"*64}
        (output / "upgrade-review.json").write_text(json.dumps(upgrade), encoding="utf-8")
        args = type("Args", (), {"output": str(output), "operator": "qa", "purpose": "upgrade", "confirm": True, "before": None})()
        self.assertEqual(0, module.review(args))
        self.assertEqual("", module.validate_review(output))
        upgrade["mapping_sha256"] = "f"*64
        (output / "upgrade-review.json").write_text(json.dumps(upgrade), encoding="utf-8")
        self.assertIn("upgrade-review.json", module.validate_review(output))

    def test_status_marks_invalid_plan(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "release-plan.json").write_text('{"schema":"wrong"}', encoding="utf-8")
        args = type("Args", (), {"output": str(output), "version": None, "format": "json"})()
        import contextlib
        import io
        stream = io.StringIO()
        with contextlib.redirect_stdout(stream):
            self.assertEqual(0, module.status(args))
        self.assertEqual("INVALID", json.loads(stream.getvalue())["plan_status"])

    def test_package_rejects_blocked_plan(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "BUILD_INFO.txt").write_text("RELEASE_VERSION=v1\n", encoding="utf-8")
        import hashlib
        lines = [hashlib.sha256((output / name).read_bytes()).hexdigest() + "  " + name
                 for name in ("a.txt", "BUILD_INFO.txt")]
        (output / "SHA256SUMS").write_text("\n".join(lines) + "\n", encoding="utf-8")
        (output / "release-plan.json").write_text('{"schema":"uip.release-materials/v1","overall_status":"BLOCKED"}', encoding="utf-8")
        args = type("Args", (), {"output": str(output), "version": "v1", "allow_legacy_unsigned": True})()
        self.assertEqual(2, module.package(args))

    def test_package_accepts_formal_review_and_rejects_candidate(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "BUILD_INFO.txt").write_text("RELEASE_VERSION=v1\n", encoding="utf-8")
        import hashlib
        lines = [hashlib.sha256((output / name).read_bytes()).hexdigest() + "  " + name
                 for name in ("a.txt", "BUILD_INFO.txt")]
        (output / "SHA256SUMS").write_text("\n".join(lines) + "\n", encoding="utf-8")
        review = {"operator": "qa", "candidate": False, "created_at": "2026-10-25T00:00:00Z", "input_reviewed": True}
        (output / "approval-review.json").write_text(json.dumps(review), encoding="utf-8")
        args = type("Args", (), {"output": str(output), "version": "v1", "allow_legacy_unsigned": True})()
        self.assertEqual(0, module.package(args))
        archive = self.root / "uip-offline-delivery-v1-linux-amd64.tar"
        archive.unlink()
        Path(str(archive) + ".sha256").unlink()
        review["candidate"] = True
        (output / "approval-review.json").write_text(json.dumps(review), encoding="utf-8")
        self.assertEqual(2, module.package(args))

    def test_package_rejects_invalid_plan_schema(self):
        output = self.root / "pkg"
        output.mkdir()
        (output / "a.txt").write_text("hello\n", encoding="utf-8")
        (output / "BUILD_INFO.txt").write_text("RELEASE_VERSION=v1\n", encoding="utf-8")
        import hashlib
        lines = [hashlib.sha256((output / name).read_bytes()).hexdigest() + "  " + name
                 for name in ("a.txt", "BUILD_INFO.txt")]
        (output / "SHA256SUMS").write_text("\n".join(lines) + "\n", encoding="utf-8")
        (output / "release-plan.json").write_text('{"schema":"wrong","overall_status":"READY"}', encoding="utf-8")
        args = type("Args", (), {"output": str(output), "version": "v1", "allow_legacy_unsigned": True})()
        self.assertEqual(2, module.package(args))

    def test_finalize_requires_absolute_material(self):
        self.assertFalse(module.finalize_gate(["--media", str(self.root)]))

    def test_finalize_gate_accepts_complete_material_and_rejects_duplicate(self):
        media = self.root / "media"
        trust = self.root / "trust"
        media.mkdir()
        trust.mkdir()
        (media / "SHA256SUMS").write_text("", encoding="utf-8")
        paths = {}
        for name in ("spec.json", "build.key", "release.key", "verifier"):
            path = self.root / name
            path.write_text("x", encoding="utf-8")
            paths[name] = path
        os.chmod(paths["verifier"], 0o755)
        args = ["--media", str(media), "--spec", str(paths["spec.json"]),
                "--build-key", str(paths["build.key"]), "--release-key", str(paths["release.key"]),
                "--trust-dir", str(trust), "--verifier", str(paths["verifier"])]
        self.assertTrue(module.finalize_gate(args))
        (media / "release-manifest.json").write_text("{}", encoding="utf-8")
        self.assertFalse(module.finalize_gate(args))

    def test_gate_blocks_v1_without_approval(self):
        spec_path = self.root / "spec.json"
        spec_path.write_text(json.dumps({
            "schema": "urn:uip:release-spec:v1",
            "applications": [{"code": "contract_management", "components": [
                {"service_id": "contract-api", "registration_protocol": 1,
                 "catalog_version": 0, "replicas": 1, "command_policy": "locked"}]}],
        }), encoding="utf-8")
        self.assertFalse(module.gate_build(["--release-integrity-spec", str(spec_path),
                                            "--components", "contract"]))


if __name__ == "__main__":
    unittest.main()
