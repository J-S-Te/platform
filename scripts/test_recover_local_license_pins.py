"""真实临时文件恢复回归；只替换 Docker 查询，不接触业务环境。"""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


SCRIPT_DIR = Path(__file__).resolve().parent
PREP = load("prepare_fixture", SCRIPT_DIR / "prepare-local-runtime-license.py")
IMAGE = "sha256:" + "a" * 64


class RecoveryTest(unittest.TestCase):
    def setUp(self):
        self.tool = load("recover_pins", SCRIPT_DIR / "recover-local-license-pins.py")
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        for directory in ("platform/config", "platform/runtime", "platform/runtime-approvals", "app/internal"):
            (self.root / directory).mkdir(parents=True, mode=0o700)
        (self.root / "platform/compose.local.yaml").write_text("services: {}\n")
        (self.root / "app/go.mod").write_text("module fixture\n")
        (self.root / "app/Dockerfile").write_text("FROM scratch\n")
        (self.root / "app/internal/main.go").write_text("package fixture\n")
        manifest = {"version": 1, "applications": {"fixture": {
            "directory": "app", "services": {"fixture-api": "fixture:local", "fixture-worker": "fixture:local"},
            "migration": {"service": "fixture-migrate", "image": "fixture:local"}}}}
        (self.root / "platform/config/local-license-components.json").write_text(json.dumps(manifest))
        self.metadata = {"id": IMAGE, "version": "local", "protocol": "1", "architecture": "arm64", "os": "linux"}
        with mock.patch.object(PREP, "inspect_image", return_value=self.metadata):
            self.approval = PREP.build_document(self.root, "fixture", "tester", reviewed=True)
        self.approval_path = self.root / "platform/runtime-approvals/local-fixture-dev.json"
        self.write_approval()
        runtime = self.root / "platform/runtime"
        keys = str(self.root / "platform/data/keys") + ":/app/data/keys:ro"
        services = {}
        for name in ("fixture-api", "fixture-worker"):
            env = runtime / ("license-" + name + ".env")
            env.write_text("COMMERCIAL_LICENSE_SERVICE_ID=" + name + "\nCOMMERCIAL_LICENSE_CLIENT_SECRET=private-fixture\n")
            env.chmod(0o600)
            services[name] = {"image": "mutable:local", "pull_policy": "never", "env_file": [str(env)],
                              "volumes": ["license-state-" + name + ":/var/lib/commercial-license", keys]}
        services["fixture-migrate"] = {"image": "mutable:local", "pull_policy": "never"}
        self.pin = runtime / "local-license.compose.json"
        self.pin.write_text(json.dumps({"services": services, "volumes": {
            "license-state-fixture-api": {}, "license-state-fixture-worker": {}}}))
        self.pin.chmod(0o600)
        self.before = self.pin.read_bytes()
        self.env_before = {p: p.read_bytes() for p in runtime.glob("*.env")}
        self.inspect = mock.patch.object(self.tool.PREP, "inspect_image", return_value=self.metadata)
        self.inspect.start()
        self.addCleanup(self.inspect.stop)
        self.offline = mock.patch.object(self.tool, "assert_offline")
        self.offline.start()
        self.addCleanup(self.offline.stop)

    def write_approval(self):
        self.approval_path.write_text(json.dumps(self.approval))
        self.approval_path.chmod(0o600)

    def test_preview_changes_no_files(self):
        before = set(self.pin.parent.iterdir())
        result = self.tool.recover(self.root, apply=False)
        self.assertEqual(self.pin.read_bytes(), self.before)
        self.assertEqual(set(self.pin.parent.iterdir()), before)
        self.assertIsNone(result["backup"])
        self.assertEqual(len(result["services"]), 3)

    def test_apply_backs_up_exact_original_and_preserves_credentials_and_volumes(self):
        result = self.tool.recover(self.root, apply=True)
        backup = Path(result["backup"])
        self.assertEqual(backup.read_bytes(), self.before)
        self.assertEqual(backup.stat().st_mode & 0o777, 0o600)
        repaired = json.loads(self.pin.read_text())
        self.assertEqual(set(repaired["services"]), {"fixture-api", "fixture-worker", "fixture-migrate"})
        self.assertTrue(all(item["image"] == IMAGE for item in repaired["services"].values()))
        self.assertEqual(repaired["volumes"], json.loads(self.before)["volumes"])
        self.assertEqual({p: p.read_bytes() for p in self.env_before}, self.env_before)
        self.assertEqual(self.pin.stat().st_mode & 0o777, 0o600)

    def test_invalid_approval_is_rejected_without_changing_original(self):
        original = json.loads(json.dumps(self.approval))
        for fault in ("candidate", "missing-worker", "stale-source", "stale-compose", "wrong-app", "wrong-migration", "coverage"):
            with self.subTest(fault=fault):
                self.approval = json.loads(json.dumps(original))
                if fault == "candidate":
                    self.approval["reviewed"] = False
                elif fault == "missing-worker":
                    self.approval["services"].pop()
                    self.approval["components"].pop()
                elif fault == "stale-source":
                    self.approval["source_digest"] = "sha256:" + "b" * 64
                elif fault == "stale-compose":
                    self.approval["compose_sha256"] = "sha256:" + "b" * 64
                elif fault == "wrong-app":
                    self.approval["application"] = "other"
                elif fault == "wrong-migration":
                    self.approval["migration"]["service"] = "other-migrate"
                else:
                    self.approval["components"][0]["coverage_digest"] = "sha256:" + "b" * 64
                self.write_approval()
                with self.assertRaises(self.tool.PREP.PreparationError):
                    self.tool.recover(self.root, apply=True)
                self.assertEqual(self.pin.read_bytes(), self.before)
                self.assertEqual(list(self.pin.parent.glob("*.bak")), [])

    def test_backup_failure_leaves_original_untouched(self):
        with mock.patch.object(self.tool, "backup_original", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), self.before)

    def test_missing_or_wrong_immutable_image_is_rejected(self):
        self.metadata["id"] = "sha256:" + "b" * 64
        with self.assertRaises(self.tool.PREP.PreparationError):
            self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), self.before)

    def test_injected_runtime_mount_rejected(self):
        value = json.loads(self.before)
        value["services"]["fixture-api"]["volumes"].append("/:/host")
        self.pin.write_text(json.dumps(value))
        before = self.pin.read_bytes()
        with self.assertRaises(self.tool.PREP.PreparationError):
            self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), before)

    def test_concurrent_pin_change_after_backup_is_not_overwritten(self):
        save_backup = self.tool.backup_original
        changed = b'{"services": {}, "volumes": {}}'
        def concurrent(path, raw):
            result = save_backup(path, raw)
            path.write_bytes(changed)
            return result
        with mock.patch.object(self.tool, "backup_original", side_effect=concurrent):
            with self.assertRaises(self.tool.PREP.PreparationError):
                self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), changed)
        backups = list(self.pin.parent.glob("*.bak"))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_bytes(), self.before)

    def test_missing_approval_or_private_credential_blocks_all_recovery(self):
        self.approval_path.unlink()
        with self.assertRaises(self.tool.PREP.PreparationError):
            self.tool.recover(self.root, apply=True)
        self.write_approval()
        next(iter(self.env_before)).chmod(0o644)
        with self.assertRaises(self.tool.PREP.PreparationError):
            self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), self.before)

    def test_running_environment_prevents_backup_and_apply(self):
        with mock.patch.object(self.tool, "assert_offline", side_effect=self.tool.PREP.PreparationError("running")):
            with self.assertRaises(self.tool.PREP.PreparationError):
                self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), self.before)
        self.assertEqual(list(self.pin.parent.glob("*.bak")), [])

    def test_pin_change_during_final_offline_check_is_not_overwritten(self):
        calls = 0
        changed = b'{"services": {}, "volumes": {}}'
        def concurrent():
            nonlocal calls
            calls += 1
            if calls == 2:
                self.pin.write_bytes(changed)
        with mock.patch.object(self.tool, "assert_offline", side_effect=concurrent):
            with self.assertRaises(self.tool.PREP.PreparationError):
                self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), changed)

    def test_approval_change_during_final_check_blocks_write(self):
        calls = 0
        def concurrent():
            nonlocal calls
            calls += 1
            if calls == 2:
                self.approval_path.write_text('{}')
        with mock.patch.object(self.tool, "assert_offline", side_effect=concurrent):
            with self.assertRaises(self.tool.PREP.PreparationError):
                self.tool.recover(self.root, apply=True)
        self.assertEqual(self.pin.read_bytes(), self.before)


if __name__ == "__main__":
    unittest.main()
