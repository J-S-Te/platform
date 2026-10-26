#!/usr/bin/env python3
"""Contract tests for the read-only deployment plan core."""
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

MODULE_PATH = Path(__file__).with_name("deployment-plan.py")
SPEC = importlib.util.spec_from_file_location("deployment_plan", MODULE_PATH)
PLAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PLAN)


class Runner:
    def __init__(self, db_ok=True):
        self.db_ok = db_ok

    def __call__(self, cmd, cwd, timeout=15):
        if cmd[:2] == ["docker", "compose"] and cmd[2:3] == ["version"]:
            return 0, "Docker Compose version v2", ""
        if cmd[:2] == ["docker", "compose"] and "config" in cmd:
            return 0, "", ""
        if cmd[:2] == ["docker", "ps"]:
            return 0, "container123", ""
        if cmd[:3] == ["docker", "exec", "container123"]:
            return (0, "mysqld is alive", "") if self.db_ok else (1, "", "connection refused")
        return 0, "", ""


def write(path, content, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    path.chmod(mode)


def fixture(root, *, tenant="01J00000000000000000000000", compatibility=None, config=True):
    target = Path(root) / "deploy"
    for directory in ("subsystems.d", "packages", "manifests", "runtime", "backups", "data/file-gateway"):
        (target / directory).mkdir(parents=True, exist_ok=True)
    write(target / "docker-compose.yml", "services: {}\n", 0o644)
    if config:
        write(target / ".env", "\n".join([
            "COMPOSE_PROJECT_NAME=test",
            "SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=%s" % target,
            "SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID=%s" % tenant,
            "MYSQL_HOST=platform-mysql",
            "MYSQL_DATABASE=basic_platform",
            "MYSQL_PASSWORD=supersecret-do-not-print",
            "OIDC_ISSUER=https://platform.local",
            "APP_PUBLIC_BASE_URL=https://platform.local",
        ]) + "\n")
        write(target / ".release.env", "PLATFORM_IMAGE=127.0.0.1:5000/uip/platform@sha256:%064d\n" % 1)
    write(target / "packages/platform-linux-amd64.tar.gz", "archive")
    write(target / "packages/platform-linux-amd64.tar.gz.sha256", "digest  platform-linux-amd64.tar.gz\n")
    write(target / "manifests/platform.imported", "COMPONENT=platform\nIMAGE_REF=127.0.0.1:5000/uip/platform@sha256:%064d\n" % 1)
    if compatibility is None:
        compatibility = {
            "schema": "uip.deployment-compatibility/v1",
            "release": "test",
            "assets_contract": "1",
            "config_contract": "1",
            "database": {"current_min": 0, "migrate_to": 127, "previous_program_after_migration": "compatible"},
            "recovery": {"type": "backward", "requires_backup": True},
        }
    if compatibility is not False:
        write(target / "deployment-compatibility.json", json.dumps(compatibility), 0o600)
    return target


class DeploymentPlanTest(unittest.TestCase):
    def build(self, target, action="upgrade", components=None, **kwargs):
        return PLAN.build_plan(target, action, components or ["platform"],
                               runner=kwargs.pop("runner", Runner()),
                               docker_available=kwargs.pop("docker_available", True),
                               now="2026-10-25T00:00:00Z", **kwargs)

    def test_ready_upgrade_with_compatible_declaration(self):
        with tempfile.TemporaryDirectory() as root:
            doc = self.build(fixture(root))
            self.assertEqual(doc["summary"], "READY")
            self.assertEqual(doc["exit_code"], 0)
            self.assertIn("COMPAT-001", {item["id"] for item in doc["checks"]})
            self.assertEqual(doc["schema"], "uip.deploy-plan/v1")
            self.assertIn("platform", doc["images"])
            self.assertTrue(doc["images"]["platform"]["target"].startswith("127.0.0.1:5000/"))
            ids = [item["id"] for item in doc["checks"]]
            self.assertEqual(len(ids), len(set(ids)))
            self.assertTrue(all(item["status"] in ("PASS", "BLOCKED", "NEEDS_ACTION", "SKIPPED") for item in doc["checks"]))
            self.assertEqual(doc["exit_code"], {"READY": 0, "BLOCKED": 2, "NEEDS_ACTION": 3}[doc["summary"]])

    def test_media_compatibility_can_be_used_before_asset_install(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root, compatibility=False)
            media = Path(root) / "media-declaration.json"
            media.write_text(json.dumps({
                "schema": "uip.deployment-compatibility/v1",
                "release": "test",
                "assets_contract": "1",
                "config_contract": "1",
                "database": {"current_min": 0, "migrate_to": 127, "previous_program_after_migration": "compatible"},
                "recovery": {"type": "backward", "requires_backup": True},
            }), encoding="utf-8")
            doc = self.build(target, compatibility_path=str(media))
            self.assertEqual(doc["summary"], "READY")
            check = next(item for item in doc["checks"] if item["id"] == "COMPAT-001")
            self.assertEqual(check["status"], "PASS")

    def test_port_conflict_blocks(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            with (target / ".env").open("a", encoding="utf-8") as stream:
                stream.write("PUBLIC_HTTP_PORT=8081\n")
            runner = Runner()
            def with_ss(cmd, cwd, timeout=15):
                if cmd[:1] == ["ss"]:
                    return 0, "LISTEN 0 128 *:8081 *:*", ""
                return runner(cmd, cwd, timeout)
            with mock.patch.object(PLAN.shutil, "which", return_value="/usr/bin/ss"):
                doc = self.build(target, runner=with_ss)
            self.assertEqual(doc["summary"], "BLOCKED")
            check = next(item for item in doc["checks"] if item["id"] == "PORT-001")
            self.assertEqual(check["status"], "BLOCKED")

    def test_personnel_client_blocks_without_database(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            doc = self.build(target, runner=Runner(db_ok=False))
            self.assertEqual(doc["summary"], "BLOCKED")
            ids = {item["id"] for item in doc["checks"] if item["status"] == "BLOCKED"}
            self.assertIn("DB-001", ids)
            self.assertIn("INIT-002", ids)

    def test_unwritable_backup_target_blocks(self):
        if PLAN.os.geteuid() == 0:
            self.skipTest("root ignores directory write bits")
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            (target / "backups").chmod(0o555)
            doc = self.build(target)
            self.assertEqual(doc["summary"], "BLOCKED")
            check = next(item for item in doc["checks"] if item["id"] == "PERM-001")
            self.assertEqual(check["status"], "BLOCKED")

    def test_asset_transaction_blocks_and_is_reported(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            write(target / "runtime/.assets-install-transaction", "state\n", 0o600)
            doc = self.build(target)
            self.assertEqual(doc["summary"], "BLOCKED")
            check = next(item for item in doc["checks"] if item["id"] == "STATE-001")
            self.assertEqual(check["status"], "BLOCKED")

    def test_control_plane_marker_blocks(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            write(target / "runtime/.control-plane-reload-required", "pending\n", 0o600)
            doc = self.build(target)
            self.assertEqual(doc["summary"], "BLOCKED")
            check = next(item for item in doc["checks"] if item["id"] == "STATE-002")
            self.assertEqual(check["status"], "BLOCKED")

    def test_missing_config_blocks(self):
        with tempfile.TemporaryDirectory() as root:
            doc = self.build(fixture(root, config=False))
            self.assertEqual(doc["summary"], "BLOCKED")
            self.assertEqual(doc["exit_code"], 2)
            self.assertIn("CFG-001", {item["id"] for item in doc["checks"] if item["status"] == "BLOCKED"})

    def test_duplicate_tenant_blocks_upgrade(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            with (target / ".env").open("a", encoding="utf-8") as stream:
                stream.write("SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID=01J00000000000000000000001\n")
            doc = self.build(target)
            self.assertEqual(doc["summary"], "BLOCKED")
            check = next(item for item in doc["checks"] if item["id"] == "CFG-003b")
            self.assertEqual(check["status"], "BLOCKED")

    def test_placeholder_tenant_blocks_upgrade(self):
        with tempfile.TemporaryDirectory() as root:
            doc = self.build(fixture(root, tenant="REPLACE_WITH_TENANT"))
            self.assertEqual(doc["summary"], "BLOCKED")
            self.assertIn("INIT-001", {item["id"] for item in doc["checks"] if item["status"] == "BLOCKED"})

    def test_unknown_compatibility_requires_action(self):
        with tempfile.TemporaryDirectory() as root:
            compatibility = {
                "schema": "uip.deployment-compatibility/v1",
                "release": "test",
                "assets_contract": "1",
                "config_contract": "1",
                "database": {"current_min": 0, "migrate_to": 127, "previous_program_after_migration": "unknown"},
                "recovery": {"type": "forward-only", "requires_backup": True},
            }
            doc = self.build(fixture(root, compatibility=compatibility))
            self.assertEqual(doc["summary"], "BLOCKED")
            self.assertEqual(doc["exit_code"], 2)
            check = next(item for item in doc["checks"] if item["id"] == "COMPAT-001")
            self.assertEqual(check["status"], "BLOCKED")

    def test_incompatible_requires_explicit_forward_recovery(self):
        with tempfile.TemporaryDirectory() as root:
            compatibility = {
                "schema": "uip.deployment-compatibility/v1",
                "release": "test",
                "assets_contract": "1",
                "config_contract": "1",
                "database": {"current_min": 0, "migrate_to": 127, "previous_program_after_migration": "incompatible"},
                "recovery": {"type": "forward-only", "requires_backup": True},
            }
            target = fixture(root, compatibility=compatibility)
            doc = self.build(target)
            self.assertEqual(doc["summary"], "BLOCKED")
            proof = Path(root) / "proof.json"
            proof.write_text(json.dumps({
                "backup_sha256": "b" * 64,
                "migration_target": 127,
                "operator": "release-manager",
                "approved_at": "2026-10-25T00:00:00Z",
                "recovery_steps": ["restore verified backup", "apply forward fix"],
            }), encoding="utf-8")
            doc = self.build(target, forward_recovery=True, recovery_evidence=str(proof))
            self.assertEqual(doc["summary"], "READY")

    def test_forward_recovery_requires_evidence(self):
        with tempfile.TemporaryDirectory() as root:
            compatibility = {
                "schema": "uip.deployment-compatibility/v1",
                "release": "test",
                "assets_contract": "1",
                "config_contract": "1",
                "database": {"current_min": 0, "migrate_to": 127, "previous_program_after_migration": "unknown"},
                "recovery": {"type": "forward-only", "requires_backup": True},
            }
            target = fixture(root, compatibility=compatibility)
            doc = self.build(target, forward_recovery=True)
            self.assertEqual(doc["summary"], "NEEDS_ACTION")
            proof = Path(root) / "proof.json"
            proof.write_text(json.dumps({
                "backup_sha256": "a" * 64,
                "migration_target": 127,
                "operator": "release-manager",
                "approved_at": "2026-10-25T00:00:00Z",
                "recovery_steps": ["restore verified backup", "run forward fix"],
            }), encoding="utf-8")
            doc = self.build(target, forward_recovery=True, recovery_evidence=str(proof))
            self.assertEqual(doc["summary"], "READY")
            check = next(item for item in doc["checks"] if item["id"] == "COMPAT-001")
            self.assertEqual(check["status"], "PASS")
            self.assertEqual(check["severity"], "WARNING")

    def test_baseline_rejects_nonready_or_different_component_scope(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            baseline = Path(root) / "baseline.json"
            original = self.build(target)
            for changes in ({"summary": "BLOCKED", "exit_code": 2}, {"components": ["frontend"]}):
                first = dict(original, **changes)
                baseline.write_text(json.dumps(first), encoding="utf-8")
                current = self.build(target, baseline_report=str(baseline))
                check = next(item for item in current["checks"] if item["id"] == "PLAN-001")
                self.assertEqual("BLOCKED", check["status"])

    def test_compatibility_baseline_rejects_changed_declaration(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            baseline = Path(root) / "baseline.json"
            baseline.write_text(json.dumps(self.build(target)), encoding="utf-8")
            declaration = target / "deployment-compatibility.json"
            value = json.loads(declaration.read_text())
            value["release"] = "different-release"
            declaration.write_text(json.dumps(value))
            current = PLAN.compatibility_document(target, "upgrade", ["platform"], baseline_report=str(baseline))
            check = next(item for item in current["checks"] if item["id"] == "PLAN-001")
            self.assertEqual("BLOCKED", check["status"])

    def test_baseline_report_detects_config_change(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            baseline = Path(root) / "baseline.json"
            first = self.build(target)
            baseline.write_text(json.dumps(first), encoding="utf-8")
            same = self.build(target, baseline_report=str(baseline))
            self.assertEqual(same["summary"], "READY")
            self.assertIn("PLAN-001", {item["id"] for item in same["checks"]})
            with (target / ".env").open("a", encoding="utf-8") as stream:
                stream.write("APP_TIMEZONE=UTC\n")
            changed = self.build(target, baseline_report=str(baseline))
            self.assertEqual(changed["summary"], "BLOCKED")
            check = next(item for item in changed["checks"] if item["id"] == "PLAN-001")
            self.assertEqual(check["status"], "BLOCKED")

    def test_compatibility_only_consumes_config_baseline(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            baseline = Path(root) / "baseline.json"
            first = self.build(target)
            baseline.write_text(json.dumps(first), encoding="utf-8")
            same = PLAN.compatibility_document(target, "upgrade", ["platform"],
                                               compatibility_path=str(target / "deployment-compatibility.json"),
                                               baseline_report=str(baseline))
            self.assertEqual(same["summary"], "READY")
            with (target / ".env").open("a", encoding="utf-8") as stream:
                stream.write("APP_TIMEZONE=UTC\n")
            changed = PLAN.compatibility_document(target, "upgrade", ["platform"],
                                                  compatibility_path=str(target / "deployment-compatibility.json"),
                                                  baseline_report=str(baseline))
            self.assertEqual(changed["summary"], "BLOCKED")
            check = next(item for item in changed["checks"] if item["id"] == "PLAN-001")
            self.assertEqual(check["status"], "BLOCKED")

    def test_compatibility_only_document_blocks_unknown(self):
        with tempfile.TemporaryDirectory() as root:
            compatibility = {
                "schema": "uip.deployment-compatibility/v1",
                "release": "test",
                "assets_contract": "1",
                "config_contract": "1",
                "database": {"current_min": 0, "migrate_to": 127, "previous_program_after_migration": "unknown"},
                "recovery": {"type": "forward-only", "requires_backup": True},
            }
            target = fixture(root, compatibility=compatibility)
            doc = PLAN.compatibility_document(target, "upgrade", ["platform"],
                                              compatibility_path=str(target / "deployment-compatibility.json"))
            self.assertEqual(doc["summary"], "BLOCKED")
            self.assertEqual(doc["exit_code"], 2)
            self.assertEqual(doc["checks"][0]["id"], "COMPAT-001")

    def test_compatibility_only_document_accepts_compatible(self):
        with tempfile.TemporaryDirectory() as root:
            target = fixture(root)
            doc = PLAN.compatibility_document(target, "upgrade", ["platform"],
                                              compatibility_path=str(target / "deployment-compatibility.json"))
            self.assertEqual(doc["summary"], "READY")
            self.assertEqual(doc["exit_code"], 0)

    def test_resume_without_session_blocks(self):
        with tempfile.TemporaryDirectory() as root:
            doc = self.build(fixture(root), action="resume")
            self.assertEqual(doc["summary"], "BLOCKED")
            self.assertIn("STATE-003", {item["id"] for item in doc["checks"] if item["status"] == "BLOCKED"})

    def test_install_without_generated_config_needs_action(self):
        with tempfile.TemporaryDirectory() as root:
            doc = self.build(fixture(root, config=False), action="install")
            self.assertEqual(doc["summary"], "NEEDS_ACTION")
            self.assertEqual(doc["exit_code"], 3)

    def test_json_report_does_not_leak_secret(self):
        with tempfile.TemporaryDirectory() as root:
            doc = self.build(fixture(root))
            raw = json.dumps(doc, ensure_ascii=False)
            self.assertNotIn("supersecret-do-not-print", raw)
            self.assertNotIn("MYSQL_PASSWORD", raw)

    def test_invalid_component_is_usage_error(self):
        with self.assertRaises(ValueError):
            PLAN.selected_components(["not-a-component"])

    def test_invalid_action_exits_64(self):
        with self.assertRaises(SystemExit) as raised:
            PLAN.main(["--target", "/tmp", "--action", "not-an-action"])
        self.assertEqual(raised.exception.code, 64)


if __name__ == "__main__":
    unittest.main()
