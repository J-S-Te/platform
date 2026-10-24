#!/usr/bin/env python3
"""离线单元测试，不使用 Docker 守护进程、数据库或工作区已有数据。"""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("prepare_local_runtime_license", Path(__file__).with_name("prepare-local-runtime-license.py"))
TOOL = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TOOL)


class LocalRuntimePreparationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        (self.root / "platform/config").mkdir(parents=True)
        (self.root / "platform/compose.local.yaml").write_text("services:\n  customer-api: {}\n", encoding="utf-8")
        self.services = {"customer-api": "crm/backend:local", "customer-worker": "crm/backend:local", "presale-worker": "crm/presale:local"}
        self.manifest = {"version": 1, "applications": {"customer_and_opportunity": {"directory": "customer_and_opportunity", "services": self.services, "migration": {"service": "customer-migrate", "image": "crm/backend:local"}}}}
        self.write_manifest()
        app = self.root / "customer_and_opportunity"
        (app / "internal").mkdir(parents=True)
        (app / "internal/main.go").write_text("package internal\n", encoding="utf-8")
        (app / "go.mod").write_text("module test\n", encoding="utf-8")
        (app / "Dockerfile").write_text("FROM scratch\n", encoding="utf-8")
        self.metadata = {"id": "sha256:" + "a" * 64, "version": "local-20261015", "protocol": "1", "architecture": "arm64", "os": "linux"}
        self.runner = mock.patch.object(TOOL.subprocess, "run", side_effect=self.inspect)
        self.mock_run = self.runner.start()
        self.addCleanup(self.runner.stop)

    def write_manifest(self):
        (self.root / "platform/config/local-license-components.json").write_text(json.dumps(self.manifest), encoding="utf-8")

    def inspect(self, command, **kwargs):
        self.assertEqual(command[:3], ["docker", "image", "inspect"])
        self.assertEqual(command[3], "--format")
        self.assertNotIn(".Config.Env", command[4])
        self.assertEqual(kwargs["timeout"], 30)
        self.assertEqual(kwargs["stderr"], subprocess.DEVNULL)
        metadata = dict(self.metadata)
        if command[-1] == "crm/presale:local":
            metadata["id"] = "sha256:" + "b" * 64
        kwargs["stdout"].write(json.dumps(metadata).encode("utf-8"))
        return subprocess.CompletedProcess(command, 0)

    def document(self):
        return TOOL.build_document(self.root, "customer_and_opportunity", "yglf")

    def test_complete_components_and_real_image_projection(self):
        document = self.document()
        self.assertEqual([item["service_id"] for item in document["components"]], sorted(self.services))
        self.assertEqual(document["components"][-1]["image_digest"], "sha256:" + "b" * 64)
        self.assertEqual(self.mock_run.call_count, 2)
        self.assertEqual(document["services"][0]["version"], "local-20261015")
        self.assertFalse(document["reviewed"])
        self.assertEqual(document["migration"], {"service": "customer-migrate", "image_digest": self.metadata["id"], "version": self.metadata["version"], "protocol": "1"})
        TOOL.validate_document(document)

    def test_separate_migration_image_is_inspected_and_not_a_business_consumer(self):
        self.manifest["applications"]["customer_and_opportunity"]["migration"]["image"] = "crm/migrate:local"
        self.write_manifest()
        def distinct(command, **kwargs):
            metadata = dict(self.metadata)
            if command[-1] == "crm/migrate:local":
                metadata["id"] = "sha256:" + "d" * 64
            kwargs["stdout"].write(json.dumps(metadata).encode())
            return subprocess.CompletedProcess(command, 0)
        with mock.patch.object(TOOL.subprocess, "run", side_effect=distinct) as inspector:
            document = self.document()
        self.assertEqual(inspector.call_count, 3)
        self.assertEqual(document["migration"]["image_digest"], "sha256:" + "d" * 64)
        self.assertEqual(len(document["components"]), len(self.services))
        self.assertNotIn("customer-migrate", [item["name"] for item in document["services"]])

    def test_invalid_migration_metadata_is_rejected(self):
        self.manifest["applications"]["customer_and_opportunity"]["migration"]["image"] = "crm/migrate:local"
        self.write_manifest()
        for field, value in (("id", "mutable:tag"), ("protocol", ""), ("version", None), ("architecture", "unknown")):
            with self.subTest(field=field):
                def invalid(command, **kwargs):
                    metadata = dict(self.metadata)
                    if command[-1] == "crm/migrate:local":
                        metadata[field] = value
                    kwargs["stdout"].write(json.dumps(metadata).encode())
                    return subprocess.CompletedProcess(command, 0)
                with mock.patch.object(TOOL.subprocess, "run", side_effect=invalid), self.assertRaises(TOOL.PreparationError):
                    self.document()

    def test_migration_schema_is_required_and_strict(self):
        target = self.manifest["applications"]["customer_and_opportunity"]
        for migration in (None, {}, {"service": "customer-migrate", "image": "crm/backend:local", "unknown": True}, {"service": "customer-api", "image": "crm/backend:local"}):
            with self.subTest(migration=migration):
                target["migration"] = migration
                self.write_manifest()
                with self.assertRaises(TOOL.PreparationError):
                    self.document()
        target["migration"] = {"service": "customer-migrate", "image": "crm/backend:local"}
        self.write_manifest()
        document = self.document()
        for mutation in ("missing", "unknown", "digest", "protocol"):
            changed = json.loads(json.dumps(document))
            if mutation == "missing":
                del changed["migration"]
            elif mutation == "unknown":
                changed["migration"]["extra"] = True
            elif mutation == "digest":
                changed["migration"]["image_digest"] = "mutable:tag"
            else:
                changed["migration"]["protocol"] = "2"
            with self.subTest(mutation=mutation), self.assertRaises(TOOL.PreparationError):
                TOOL.validate_document(changed)

    def test_missing_or_invalid_image_labels_and_identity_fail(self):
        for field, value in [("version", ""), ("version", None), ("version", "bad\nversion"), ("protocol", None), ("protocol", "2"), ("id", "backend:local"), ("id", "sha256:" + "A" * 64), ("os", "darwin"), ("architecture", "unknown")]:
            with self.subTest(field=field, value=value):
                original = self.metadata[field]
                self.metadata[field] = value
                with self.assertRaises(TOOL.PreparationError):
                    self.document()
                self.metadata[field] = original

    def test_output_limit_timeout_and_unknown_projection_fail(self):
        for behavior in ("oversized", "unknown", "timeout", "failed"):
            with self.subTest(behavior=behavior):
                def bad(command, **kwargs):
                    if behavior == "timeout":
                        raise subprocess.TimeoutExpired(command, 30, stderr="secret must not appear")
                    if behavior == "oversized":
                        kwargs["stdout"].write(b"x" * (TOOL.LIMIT + 1))
                    else:
                        kwargs["stdout"].write(json.dumps(dict(self.metadata, extra="unexpected")).encode())
                    return subprocess.CompletedProcess(command, 1 if behavior == "failed" else 0)
                with mock.patch.object(TOOL.subprocess, "run", side_effect=bad):
                    with self.assertRaises(TOOL.PreparationError) as raised:
                        self.document()
                    self.assertNotIn("secret", str(raised.exception))

    def test_source_and_compose_hashes_are_deterministic_and_sensitive(self):
        original = self.document()
        self.assertEqual(original, self.document())
        source = self.root / "customer_and_opportunity/internal/main.go"
        source.write_text("package changed\n")
        changed = self.document()
        self.assertNotEqual(original["source_digest"], changed["source_digest"])
        self.assertNotEqual(original["components"][0]["coverage_digest"], changed["components"][0]["coverage_digest"])
        self.assertEqual(original["compose_sha256"], changed["compose_sha256"])
        (self.root / "platform/compose.local.yaml").write_text("services: {}\n")
        changed_compose = self.document()
        self.assertNotEqual(changed_compose["compose_sha256"], changed["compose_sha256"])
        self.assertEqual(changed_compose["source_digest"], changed["source_digest"])

    def test_source_hash_framing(self):
        app = self.root / "customer_and_opportunity"
        raw = b""
        for name in ("Dockerfile", "go.mod", "internal/main.go"):
            raw += (name + "\0" + TOOL.digest((app / name).read_bytes()) + "\n").encode()
        expected = TOOL.digest(raw)
        self.assertEqual(self.document()["source_digest"], expected)
        self.assertEqual(self.document()["components"][0]["coverage_digest"], TOOL.digest(("dev-source-fingerprint\0customer_and_opportunity\0" + expected).encode()))

    def test_candidate_and_approval_are_separate_private_files(self):
        document = self.document()
        candidate = TOOL.publish(self.root, document)
        self.assertEqual(candidate.name, "local-customer_and_opportunity-dev.candidate.json")
        self.assertFalse(json.loads(candidate.read_text())["reviewed"])
        approved = TOOL.publish(self.root, document, approve=True)
        self.assertTrue(json.loads(approved.read_text())["reviewed"])
        self.assertNotEqual(candidate, approved)
        self.assertEqual(approved.stat().st_mode & 0o777, 0o600)
        self.assertEqual(approved.parent.stat().st_mode & 0o777, 0o700)
        self.assertEqual(TOOL.publish(self.root, document, approve=True), approved)

    def test_default_refuses_approved_changes_explicit_replace_preserves_state(self):
        approved = TOOL.publish(self.root, self.document(), approve=True)
        state = self.root / "platform/runtime"
        state.mkdir()
        snapshot = state / "old-snapshot.json"
        snapshot.write_text("existing state")
        self.metadata["id"] = "sha256:" + "c" * 64
        changed = self.document()
        before = approved.read_bytes()
        with self.assertRaises(TOOL.PreparationError):
            TOOL.publish(self.root, changed, approve=True)
        self.assertEqual(before, approved.read_bytes())
        TOOL.publish(self.root, changed, approve=True, replace=True)
        self.assertNotEqual(before, approved.read_bytes())
        self.assertEqual(snapshot.read_text(), "existing state")
        with self.assertRaises(TOOL.PreparationError):
            TOOL.publish(self.root, changed, replace=True)

    def test_concurrent_approval_publication_cannot_clobber(self):
        document = self.document()
        with mock.patch.object(TOOL.os, "link", side_effect=FileExistsError):
            with self.assertRaises(TOOL.PreparationError):
                TOOL.publish(self.root, document, approve=True)
        self.assertEqual(list((self.root / "platform/runtime-approvals").iterdir()), [])

    def test_rejects_source_symlink_including_dangling(self):
        for destination in ("main.go", "absent.go"):
            link = self.root / "customer_and_opportunity/internal/link.go"
            link.symlink_to(destination)
            with self.assertRaises(TOOL.PreparationError):
                self.document()
            link.unlink()

    def test_rejects_symlink_source_directory_and_approval_parent(self):
        internal = self.root / "customer_and_opportunity/internal"
        internal.rename(internal.with_name("actual"))
        internal.symlink_to("actual", target_is_directory=True)
        with self.assertRaises(TOOL.PreparationError):
            self.document()
        internal.unlink()
        internal.with_name("actual").rename(internal)
        document = self.document()
        outside = self.root / "outside"
        outside.mkdir(mode=0o700)
        (self.root / "platform/runtime-approvals").symlink_to(outside, target_is_directory=True)
        with self.assertRaises(TOOL.PreparationError):
            TOOL.publish(self.root, document)
        self.assertEqual(list(outside.iterdir()), [])

    def test_rejects_existing_symlink_and_unprivate_directory(self):
        directory = self.root / "platform/runtime-approvals"
        directory.mkdir(mode=0o755)
        with self.assertRaises(TOOL.PreparationError):
            TOOL.publish(self.root, self.document())
        directory.chmod(0o700)
        path = directory / "local-customer_and_opportunity-dev.json"
        path.symlink_to(self.root / "absent")
        with self.assertRaises(TOOL.PreparationError):
            TOOL.publish(self.root, self.document(), approve=True)

    def test_rejects_malformed_unknown_or_large_existing_document(self):
        path = TOOL.publish(self.root, self.document(), approve=True)
        for payload in (b"broken JSON", json.dumps(dict(self.document(), unknown=True)).encode(), b"x" * (TOOL.LIMIT + 1), b'{"version":1,"version":1}'):
            path.write_bytes(payload)
            with self.assertRaises(TOOL.PreparationError):
                TOOL.publish(self.root, self.document(), approve=True, replace=True)

    def test_rejects_world_readable_existing_approval(self):
        path = TOOL.publish(self.root, self.document(), approve=True)
        path.chmod(0o644)
        with self.assertRaises(TOOL.PreparationError):
            TOOL.publish(self.root, self.document(), approve=True, replace=True)

    def test_rejects_invalid_manifest_paths_and_unknown_fields(self):
        target = self.manifest["applications"]["customer_and_opportunity"]
        for directory in ("../outside", "/tmp/outside", "./customer_and_opportunity", "customer_and_opportunity/../other"):
            target["directory"] = directory
            self.write_manifest()
            with self.assertRaises(TOOL.PreparationError):
                self.document()
        target["directory"] = "customer_and_opportunity"
        target["unknown"] = True
        self.write_manifest()
        with self.assertRaises(TOOL.PreparationError):
            self.document()

    def test_rejects_duplicate_service_and_bad_image(self):
        self.manifest["applications"]["other"] = {"directory": "other", "services": {"customer-api": "other:local"}, "migration": {"service": "other-migrate", "image": "other:local"}}
        self.write_manifest()
        with self.assertRaises(TOOL.PreparationError):
            self.document()
        del self.manifest["applications"]["other"]
        self.services["customer-api"] = "$(credential-leak)"
        self.write_manifest()
        with self.assertRaises(TOOL.PreparationError):
            self.document()

    def test_rejects_incomplete_source_and_unknown_application(self):
        with self.assertRaises(TOOL.PreparationError):
            TOOL.build_document(self.root, "untrusted_app", "yglf")
        (self.root / "customer_and_opportunity/Dockerfile").unlink()
        with self.assertRaises(TOOL.PreparationError):
            self.document()

    def test_cli_prints_review_only_no_secrets_and_rejects_nondev(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            result = TOOL.main(["--workspace", str(self.root), "--application", "customer_and_opportunity", "--operator", "yglf"])
        self.assertEqual(result, 0)
        self.assertIn("Unapproved candidate:", output.getvalue())
        self.assertIn("not a production coverage approval", output.getvalue())
        self.assertNotIn("CLIENT_SECRET", output.getvalue())
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            TOOL.main(["--application", "customer_and_opportunity", "--operator", "yglf", "--environment", "prod"])


if __name__ == "__main__":
    unittest.main()
