#!/usr/bin/env python3
"""回归生产配置选择与所有权判断，不连接 Docker 守护进程。"""

import importlib.util
import json
import re
import shutil
from pathlib import Path
import subprocess
import tempfile
import unittest


BIN = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("subsystem_runtime", BIN / "subsystem-runtime.py")
RUNTIME = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNTIME)
PRODUCTION = BIN.parent


class RuntimePolicyTests(unittest.TestCase):
    def load(self, component):
        app, name = RUNTIME.PROFILES[component]
        return RUNTIME.projection((PRODUCTION / "subsystems.d" / name).read_text(), app)

    def test_all_six_shipped_profiles_parse(self):
        for component in RUNTIME.PROFILES:
            with self.subTest(component=component):
                compose, _, _ = self.load(component)
                self.assertTrue(compose["runtime_services"])

    def test_crm_includes_nine_consumers_and_transfer_worker(self):
        compose, conditional, files = self.load("customer-opportunity")
        self.assertEqual(len(compose["runtime_services"]), 9)
        self.assertIn("customer-contract-transfer-worker", compose["runtime_services"])
        self.assertEqual(RUNTIME.selected_services(PRODUCTION, compose, conditional, files, set(compose["runtime_services"])), compose["runtime_services"])

    def test_contract_ownership_includes_worker_but_not_shared_database(self):
        compose, _, _ = self.load("contract")
        owned = RUNTIME.owned_services("contract", compose)
        self.assertIn("contract-worker", owned)
        self.assertNotIn("contract-mysql", owned)
        self.assertNotIn("temporal", owned)

    def test_ownership_keeps_optional_worker_for_stopping_old_containers(self):
        compose, _, _ = self.load("customer-portal")
        self.assertIn("portal-invite-compensation-worker", RUNTIME.owned_services("customer-portal", compose))

    def test_required_runtime_service_missing_is_fatal(self):
        compose, conditional, files = self.load("contract")
        with self.assertRaisesRegex(ValueError, "disabled or missing"):
            RUNTIME.selected_services(PRODUCTION, compose, conditional, files, {"contract-api"})

    def test_conditional_worker_matches_agent_dependency_and_required_keys(self):
        compose, conditional, files = self.load("customer-portal")
        baseline = ["portal-api", "portal-report-worker"]
        enabled = set(compose["runtime_services"])
        required = files["runtime/customer.env"]["required"]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "runtime").mkdir()
            env = root / "runtime/customer.env"
            self.assertEqual(RUNTIME.selected_services(root, compose, conditional, files, enabled), baseline)
            env.write_text("\n".join(key + "=ready" for key in required))
            self.assertEqual(RUNTIME.selected_services(root, compose, conditional, files, enabled), baseline)
            enabled.add("customer-api")
            self.assertEqual(RUNTIME.selected_services(root, compose, conditional, files, enabled), compose["runtime_services"])
            for value in ("", "PENDING_ONBOARDING", "REPLACE_WITH_SECRET"):
                env.write_text("\n".join(key + "=" + (value if key == required[0] else "ready") for key in required))
                self.assertEqual(RUNTIME.selected_services(root, compose, conditional, files, enabled), baseline)
            env.unlink()
            env.symlink_to(root / "missing")
            self.assertEqual(RUNTIME.selected_services(root, compose, conditional, files, enabled), baseline)

    def test_conditional_environment_is_not_executed(self):
        values = RUNTIME.environment_values('export OIDC_CLIENT_ID="$(touch /do-not-run)"\nOIDC_CLIENT_SECRET=literal\n')
        self.assertEqual(values["OIDC_CLIENT_ID"], "$(touch /do-not-run)")
        self.assertEqual(values["OIDC_CLIENT_SECRET"], "literal")

    def test_mismatched_application_and_unknown_version_rejected(self):
        text = (PRODUCTION / "subsystems.d/contract_management-prod.yaml").read_text()
        for changed in (text.replace("version: 1", "version: 2", 1), text.replace("code: contract_management", "code: project_management", 1)):
            with self.assertRaises(ValueError):
                RUNTIME.projection(changed, "contract_management")

    def test_unsupported_policy_yaml_does_not_drop_workers(self):
        text = (PRODUCTION / "subsystems.d/contract_management-prod.yaml").read_text()
        for value in ("*runtime_alias", "[contract-api, contract-worker, contract-worker]", "[contract-api, ../worker]"):
            changed = text.replace("runtime_services: [contract-api, contract-worker]", "runtime_services: " + value)
            with self.assertRaises(ValueError):
                RUNTIME.projection(changed, "contract_management")

    def test_duplicate_and_undefined_conditional_policy_rejected(self):
        text = (PRODUCTION / "subsystems.d/customer_portal-prod.yaml").read_text()
        for changed in (
            text + "  runtime_services: [portal-api]\n",
            text.replace("portal-invite-compensation-worker: runtime/customer.env", "portal-invite-compensation-worker: runtime/unknown.env"),
        ):
            with self.assertRaises(ValueError):
                RUNTIME.projection(changed, "customer_portal")

    def test_cross_application_runtime_cannot_expand_stop_scope(self):
        text = (PRODUCTION / "subsystems.d/contract_management-prod.yaml").read_text()
        text = text.replace("runtime_services: [contract-api, contract-worker]", "runtime_services: [contract-api, platform-api]")
        with self.assertRaisesRegex(ValueError, "ownership"):
            RUNTIME.projection(text, "contract_management")

    def test_block_service_lists_supported(self):
        text = (PRODUCTION / "subsystems.d/contract_management-prod.yaml").read_text()
        text = text.replace("runtime_services: [contract-api, contract-worker]", "runtime_services:\n    - contract-api\n    - contract-worker")
        compose, _, _ = RUNTIME.projection(text, "contract_management")
        self.assertEqual(compose["runtime_services"], ["contract-api", "contract-worker"])

    def test_cli_rejects_symlink_profile_and_invalid_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            profiles = root / "subsystems.d"
            profiles.mkdir()
            target = profiles / "contract_management-prod.yaml"
            target.symlink_to(PRODUCTION / "subsystems.d/contract_management-prod.yaml")
            command = ["python3", "-B", str(BIN / "subsystem-runtime.py"), "runtime", "--target", str(root), "--component", "contract"]
            self.assertNotEqual(subprocess.run(command, input="contract-api\ncontract-worker\n", text=True, capture_output=True).returncode, 0)
            target.unlink()
            target.write_text((PRODUCTION / "subsystems.d/contract_management-prod.yaml").read_text())
            self.assertNotEqual(subprocess.run(command, input="../invalid\n", text=True, capture_output=True).returncode, 0)

    def test_transfer_worker_has_isolated_license_credentials_state_and_probe(self):
        for compose_name in ("docker-compose.yml", "compose.yaml"):
            text = (PRODUCTION / compose_name).read_text()
            block = text.split("  customer-contract-transfer-worker:\n", 1)[1].split("\n  customer-presale-alert-worker:", 1)[0]
            self.assertIn("runtime/license-customer-contract-transfer-worker.env", block)
            self.assertIn("license-state-customer-contract-transfer-worker:/var/lib/commercial-license", block)
            self.assertIn("jwt-ed25519-public.pem:ro", block)
            self.assertIn("/app/contract-transfer-worker", block)
            self.assertIn('command: ["./contract-transfer-worker"]', block)

    @unittest.skipUnless(shutil.which("docker"), "Docker CLI is required for native Compose parsing")
    def test_native_compose_resolves_worker_and_proxy_without_daemon_actions(self):
        check = subprocess.run(["docker", "compose", "version"], capture_output=True, text=True)
        if check.returncode:
            self.skipTest("Docker Compose plugin is unavailable")
        with tempfile.TemporaryDirectory(prefix="runtime-compose-") as directory:
            root = Path(directory).resolve()
            env_file = root / "fixture.env"
            env_file.write_text("")
            fields = {
                key: "fixture" for filename in ("docker-compose.yml", "compose.yaml")
                for key in re.findall(r"\$\{([A-Z0-9_]+):\?", (PRODUCTION / filename).read_text())
            }
            fields.update(
                PLATFORM_IMAGE="127.0.0.1:5000/uip/platform@sha256:" + "a" * 64,
                FILE_GATEWAY_IMAGE="127.0.0.1:5000/uip/gateway@sha256:" + "b" * 64,
                FRONTEND_IMAGE="127.0.0.1:5000/uip/frontend@sha256:" + "c" * 64,
                CUSTOMER_CRM_IMAGE="127.0.0.1:5000/uip/crm@sha256:" + "d" * 64,
                PUBLIC_SSO_ORIGIN="http://sso.fixture.invalid", KEYCLOAK_PUBLIC_URL="http://sso.fixture.invalid",
            )
            for key in ("BASIC_PLATFORM_RUNTIME_ENV_FILE", "CUSTOMER_RUNTIME_ENV_FILE", "CONTRACT_RUNTIME_ENV_FILE", "PROJECT_RUNTIME_ENV_FILE", "PORTAL_RUNTIME_ENV_FILE", "SETTLEMENT_RUNTIME_ENV_FILE", "DATA_ANALYSIS_RUNTIME_ENV_FILE", "PORTAL_REPORT_WORKER_RUNTIME_ENV_FILE"):
                fields[key] = str(env_file)
            settings = root / "compose.env"
            for filename in ("docker-compose.yml", "compose.yaml"):
                for proxy in ("", "127.0.0.1:5000/uip/proxy@sha256:" + "e" * 64):
                    settings.write_text("\n".join(key + "=" + value for key, value in dict(fields, DOCKER_SOCKET_PROXY_IMAGE=proxy).items()) + "\n")
                    result = subprocess.run(["docker", "compose", "--project-directory", str(PRODUCTION), "--env-file", str(settings), "-f", str(PRODUCTION / filename), "--profile", "*", "config", "--format", "json"], capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    services = json.loads(result.stdout)["services"]
                    worker = services["customer-contract-transfer-worker"]
                    self.assertEqual(worker["image"], fields["CUSTOMER_CRM_IMAGE"])
                    self.assertEqual(worker["command"], ["./contract-transfer-worker"])
                    self.assertTrue(any(volume["target"] == "/var/lib/commercial-license" and volume["source"].endswith("license-state-customer-contract-transfer-worker") for volume in worker["volumes"]))
                    self.assertEqual(services["docker-socket-proxy"]["pull_policy"], "never")
                    if proxy:
                        self.assertEqual(services["docker-socket-proxy"]["image"], proxy)
                    else:
                        self.assertTrue(services["docker-socket-proxy"]["image"].startswith("ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:"))


if __name__ == "__main__":
    unittest.main()
