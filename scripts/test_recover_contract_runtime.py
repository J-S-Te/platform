import base64
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("recovery", Path(__file__).with_name("recover_contract_runtime.py"))
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class RecoveryTests(unittest.TestCase):
    def fixture(self):
        values = {key: "safe-test-password" for key in recovery.KEYS}
        for key in recovery.KEYS[2:]:
            values[key] = base64.b64encode(b"x" * 32).decode()
        return values

    def test_validation_and_dsn_binding(self):
        values = self.fixture()
        env = [key + "=" + value for key, value in values.items()]
        env.append("MYSQL_DSN=contract:safe-test-password@tcp(contract-mysql:3306)/contract_management")
        container = {"Config": {"Env": env}}
        self.assertEqual(recovery.validated_values(container), values)
        container["Config"]["Env"][-1] = "MYSQL_DSN=contract:other@tcp(other:3306)/other"
        with self.assertRaises(RuntimeError):
            recovery.validated_values(container)

    def test_invalid_key_is_rejected(self):
        values = self.fixture()
        values[recovery.KEYS[3]] = "not-base64"
        with self.assertRaises(RuntimeError):
            recovery.validated_values({"Config": {"Env": [key + "=" + value for key, value in values.items()]}})

    def test_atomic_restore_preserves_managed_settings_and_backup(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / ".env.local"
            original = "# comment\nOIDC_CLIENT_ID=keep-managed\nCONTRACT_MYSQL_PASSWORD=bad\n"
            path.write_text(original)
            recovery.restore_file(path, self.fixture())
            text = path.read_text()
            self.assertIn("OIDC_CLIENT_ID=keep-managed", text)
            self.assertEqual(text.count("CONTRACT_MYSQL_PASSWORD="), 1)
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
            backups = list(Path(directory).glob(".contract-runtime-backup-*"))
            self.assertEqual(len(backups), 1)
            self.assertEqual(backups[0].read_text(), original)
            self.assertEqual(os.stat(backups[0]).st_mode & 0o777, 0o600)

    def test_shell_retained_volume_guard_never_generates_missing_config(self):
        source = Path(__file__).with_name("docker-local.sh").read_text()
        function = source.split("ensure_contract_env_file() {", 1)[1].split("\n}\n", 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "missing.env"
            script = '''set -Eeuo pipefail
fail() { echo "$1"; exit 1; }
docker() { echo basic-platform-local-contract-mysql-data; }
ensure_contract_env_file() {''' + function + '\n}\nensure_contract_env_file false\n'
            result = subprocess.run(["bash", "-c", 'contract_env_file="$1"; ' + script, "bash", str(path)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("配置丢失", result.stdout)
            self.assertFalse(path.exists())


if __name__ == "__main__":
    unittest.main()
