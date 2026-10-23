"""启动预检：旧固定镜像缺失时必须在任何迁移之前失败，并一次列出完整清单。

只读替身：不调用 Docker、不执行迁移、不修改任何状态。函数体直接从 docker-local.sh 抽取，
保证测的是真实实现而不是复刻。
"""
from pathlib import Path
import hashlib
import json
import os
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "scripts/docker-local.sh").read_text()
DIGEST_A = "sha256:" + "a" * 64
DIGEST_B = "sha256:" + "b" * 64


def function(name):
    """按函数名从真实脚本抽取函数体，缺失即报错，避免测试与实现漂移。"""
    if name + "() {" not in SCRIPT:
        raise AssertionError("docker-local.sh 缺少函数 " + name)
    lines = SCRIPT.split(name + "() {", 1)[1].splitlines(keepends=True)
    body = []
    heredoc = None
    for line in lines:
        body.append(line)
        if heredoc:
            if line.strip() == heredoc:
                heredoc = None
        elif line == "}\n":
            return name + "() {" + "".join(body)
        else:
            match = re.search(r"<<'([A-Za-z_]+)'", line)
            if match:
                heredoc = match.group(1)
    raise AssertionError("未找到 shell 函数结束：" + name)


def approval(application, services, migration):
    return {
        "version": 1, "project": "basic-platform-local", "application": application,
        "environment": "dev", "operator": "tester", "reviewed": True,
        "compose_sha256": "sha256:" + "c" * 64, "source_digest": "sha256:" + "d" * 64,
        "services": [{"application": application, "environment": "dev", "name": name,
                      "image_digest": digest, "version": "local", "protocol": "1"}
                     for name, digest in services],
        "components": [],
        "migration": {"service": migration[0], "image_digest": migration[1],
                      "version": "local", "protocol": "1"},
    }


def builtin_state_path(values):
    parts = [values["COMMERCIAL_LICENSE_" + key] for key in
             ("INSTANCE_ID", "ENVIRONMENT", "CLIENT_ID", "SERVICE_ID", "CATALOG_VERSION")]
    parts.append("catalog-1")
    return "/var/lib/commercial-license/" + values["COMMERCIAL_LICENSE_SERVICE_ID"] + "-" + hashlib.sha256("\0".join(parts).encode()).hexdigest()[:32] + ".json"


class LocalLicensePreflightTest(unittest.TestCase):
    def run_preflight(self, override, approvals, present, docker_ok=True, mode=0o600,
                      env_files=None, env_mode=0o600, env_symlink=False):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "runtime").mkdir()
            (root / "runtime-approvals").mkdir()
            (root / "config").mkdir()
            (root / "config/local-license-components.json").write_bytes(
                (ROOT / "config/local-license-components.json").read_bytes())
            override_path = root / "runtime" / "local-license.compose.json"
            if override is not None:
                encoded = override if isinstance(override, str) else json.dumps(override)
                override_path.write_text(encoded.replace("@ROOT@", str(root)))
                os.chmod(override_path, mode)
            for name, values in (env_files or {}).items():
                env_path = root / "runtime" / ("license-" + name + ".env")
                body = values if isinstance(values, str) else "".join(
                    key + "=" + json.dumps(value) + "\n" for key, value in values.items())
                if env_symlink:
                    real_path = root / (name + ".env")
                    real_path.write_text(body)
                    real_path.chmod(env_mode)
                    env_path.symlink_to(real_path)
                else:
                    env_path.write_text(body)
                    env_path.chmod(env_mode)
            for name, body in approvals.items():
                (root / "runtime-approvals" / name).write_text(json.dumps(body))
            present_list = " ".join(sorted(present))
            harness = "".join([
                "set -Eeuo pipefail\n",
                "exec 3>&1\n",
                "project_root=" + str(root) + "\n",
                "log() { printf '[docker-local] %s\\n' \"$*\" >&3; }\n",
                "fail() { printf 'FAIL: %s\\n' \"$*\" >&3; exit 1; }\n",
                "compose() { printf 'COMPOSE-CALLED %s\\n' \"$*\" >&3; }\n",
                "compose_run() { printf 'COMPOSE-RUN-CALLED %s\\n' \"$*\" >&3; }\n",
                "docker_control_available() { return " + ("0" if docker_ok else "1") + "; }\n",
                "local_image_present() { case \" " + present_list + " \" in *\" $1 \"*) return 0 ;; *) return 1 ;; esac; }\n",
                function("preflight_local_license_images"),
                "preflight_local_license_images\n",
            ])
            return subprocess.run(["bash", "-c", harness], text=True, capture_output=True)

    def builtin_contract(self):
        services = {}
        envs = {}
        for name in ("contract-api", "contract-worker"):
            services[name] = {"image": DIGEST_A, "pull_policy": "never",
                              "env_file": ["@ROOT@/runtime/license-" + name + ".env"],
                              "volumes": ["license-state-" + name + ":/var/lib/commercial-license",
                                          "@ROOT@/data/keys:/app/data/keys:ro"]}
            envs[name] = {
                "COMMERCIAL_LICENSE_ENABLED": "true",
                "COMMERCIAL_LICENSE_INSTANCE_ID": "test-instance",
                "COMMERCIAL_LICENSE_ENVIRONMENT": "dev",
                "COMMERCIAL_LICENSE_SERVICE_ID": name,
                "COMMERCIAL_LICENSE_STATE_PATH": "/var/lib/commercial-license/" + name + ".json",
                "COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH": "/app/data/keys/jwt-ed25519-public.pem",
                "COMMERCIAL_LICENSE_PLATFORM_BASE_URL": "http://platform-api:8080",
                "COMMERCIAL_LICENSE_CLIENT_ID": "test-client-" + name,
                "COMMERCIAL_LICENSE_CLIENT_SECRET": 'test-secret-"$not-expanded\\value',
                "COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL": "2",
                "COMMERCIAL_LICENSE_CATALOG_VERSION": "1",
                "COMMERCIAL_LICENSE_ALLOW_HTTP": "true",
            }
            envs[name]["COMMERCIAL_LICENSE_STATE_PATH"] = builtin_state_path(envs[name])
        services["contract-migrate"] = {"image": DIGEST_B, "pull_policy": "never"}
        return {"services": services, "volumes": {
            "license-state-contract-api": {}, "license-state-contract-worker": {}}}, envs

    def test_builtin_v2_agent_pins_start_without_legacy_approval(self):
        pins, envs = self.builtin_contract()
        result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("test-secret", result.stdout + result.stderr)
        self.assertNotIn("COMPOSE-CALLED", result.stdout)

    def test_builtin_v2_checks_all_images_and_gives_v2_recovery(self):
        pins, envs = self.builtin_contract()
        result = self.run_preflight(pins, {}, (), env_files=envs)
        self.assertNotEqual(result.returncode, 0)
        for name in pins["services"]:
            self.assertIn("[镜像缺失] contract_management / " + name, result.stdout)
        self.assertIn("V2", result.stdout)
        self.assertNotIn("prepare-license --application", result.stdout)

    def test_builtin_v2_rejects_invalid_binding_and_unknown_versions(self):
        for key, value in (("COMMERCIAL_LICENSE_SERVICE_ID", "customer-api"),
                           ("COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL", "3"),
                           ("COMMERCIAL_LICENSE_CATALOG_VERSION", "999"),
                           ("COMMERCIAL_LICENSE_ENVIRONMENT", ""),
                           ("COMMERCIAL_LICENSE_ENABLED", "false"),
                           ("COMMERCIAL_LICENSE_CLIENT_SECRET", "")):
            with self.subTest(key=key):
                pins, envs = self.builtin_contract()
                envs["contract-api"][key] = value
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("test-secret", result.stdout + result.stderr)

    def test_builtin_v2_requires_private_regular_env_file(self):
        pins, envs = self.builtin_contract()
        for options in ({"env_mode": 0o644}, {"env_symlink": True}, {"env_files": {}}):
            with self.subTest(options=options):
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B),
                                            **({"env_files": envs} | options))
                self.assertNotEqual(result.returncode, 0, result.stdout)

    def test_builtin_v2_checks_required_worker_and_migration_pins(self):
        for missing in ("contract-worker", "contract-migrate"):
            with self.subTest(missing=missing):
                pins, envs = self.builtin_contract()
                del pins["services"][missing]
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(missing, result.stdout)

    def test_builtin_v2_does_not_accept_untrusted_env_path_or_pull(self):
        for field, value in (("env_file", ["@ROOT@/runtime/not-agent.env"]),
                             ("pull_policy", "always"), ("volumes", [])):
            with self.subTest(field=field):
                pins, envs = self.builtin_contract()
                pins["services"]["contract-api"][field] = value
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
                self.assertNotEqual(result.returncode, 0)

    def test_builtin_v2_supersedes_old_digest_only_for_v2_services(self):
        pins, envs = self.builtin_contract()
        old = approval("contract_management", [("contract-api", DIGEST_B),
                                              ("contract-worker", DIGEST_B)],
                       ("contract-migrate", DIGEST_A))
        result = self.run_preflight(pins, {"local-contract_management-dev.json": old},
                                    (DIGEST_A, DIGEST_B), env_files=envs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        del envs["contract-worker"]
        pins["services"]["contract-worker"].pop("env_file")
        old["services"][1]["image_digest"] = DIGEST_A
        result = self.run_preflight(pins, {"local-contract_management-dev.json": old},
                                    (DIGEST_A, DIGEST_B), env_files=envs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        pins["services"]["contract-worker"]["image"] = DIGEST_B
        result = self.run_preflight(pins, {"local-contract_management-dev.json": old},
                                    (DIGEST_A, DIGEST_B), env_files=envs)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("contract-worker", result.stdout)
        self.assertIn("固定记录不一致", result.stdout)

    def test_missing_approval_still_rejected_for_legacy_pins(self):
        pins = {"services": {"contract-api": {"image": DIGEST_A}}}
        result = self.run_preflight(pins, {}, (DIGEST_A,))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("缺少批准", result.stdout)

    def test_all_builtin_local_aliases_and_optional_portal_component(self):
        manifest = json.loads((ROOT / "config/local-license-components.json").read_text())
        aliases = {"presale-worker": "customer-presale-worker",
                   "contract-transfer-worker": "customer-contract-transfer-worker",
                   "sla-notifier": "project-sla-notifier",
                   "dashboard-api": "data-analysis-api",
                   "aggregation-worker": "data-analysis-aggregation-worker",
                   "alert-worker": "data-analysis-alert-worker"}
        for application, target in manifest["applications"].items():
            with self.subTest(application=application):
                template, credentials = self.builtin_contract()
                pins = {"services": {}, "volumes": {}}
                envs = {}
                for name in target["services"]:
                    if name == "portal-invite-compensation-worker":
                        continue
                    spec = json.loads(json.dumps(template["services"]["contract-api"]).replace("contract-api", name))
                    values = dict(credentials["contract-api"])
                    values["COMMERCIAL_LICENSE_SERVICE_ID"] = aliases.get(name, name)
                    values["COMMERCIAL_LICENSE_STATE_PATH"] = builtin_state_path(values)
                    pins["services"][name] = spec
                    pins["volumes"]["license-state-" + name] = {}
                    envs[name] = values
                pins["services"][target["migration"]["service"]] = {
                    "image": DIGEST_B, "pull_policy": "never"}
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_builtin_env_duplicate_or_wrong_alias_does_not_pass(self):
        pins, envs = self.builtin_contract()
        body = "".join(key + "=" + json.dumps(value) + "\n"
                       for key, value in envs["contract-api"].items())
        envs["contract-api"] = body + "COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL=2\n"
        result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("test-secret", result.stdout + result.stderr)

    def test_builtin_commercial_environment_can_differ_from_dev_application(self):
        pins, envs = self.builtin_contract()
        for values in envs.values():
            values["COMMERCIAL_LICENSE_ENVIRONMENT"] = "production"
            values["COMMERCIAL_LICENSE_STATE_PATH"] = builtin_state_path(values)
        result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_builtin_override_fields_cannot_replace_agent_binding(self):
        for field, value in (("environment", {"COMMERCIAL_LICENSE_SERVICE_ID": "customer-api"}),
                             ("entrypoint", ["/bin/sh"]), ("command", ["skip-license"])):
            with self.subTest(field=field):
                pins, envs = self.builtin_contract()
                pins["services"]["contract-api"][field] = value
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
                self.assertNotEqual(result.returncode, 0)

    def test_builtin_state_file_must_match_agent_credential_identity(self):
        for path in ("/var/lib/commercial-license/", "/var/lib/commercial-license/other.json"):
            with self.subTest(path=path):
                pins, envs = self.builtin_contract()
                envs["contract-api"]["COMMERCIAL_LICENSE_STATE_PATH"] = path
                result = self.run_preflight(pins, {}, (DIGEST_A, DIGEST_B), env_files=envs)
                self.assertNotEqual(result.returncode, 0)

    def test_missing_pinned_image_fails_before_any_migration(self):
        override = {"services": {"customer-api": {"image": DIGEST_A, "pull_policy": "never"},
                                 "customer-migrate": {"image": DIGEST_A, "pull_policy": "never"}}}
        approvals = {"local-customer_and_opportunity-dev.json": approval(
            "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A))}
        result = self.run_preflight(override, approvals, present=())
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("customer_and_opportunity", result.stdout)
        self.assertIn("customer-migrate", result.stdout)
        self.assertIn(DIGEST_A, result.stdout)
        self.assertNotIn("COMPOSE-RUN-CALLED", result.stdout)
        self.assertNotIn("COMPOSE-CALLED", result.stdout)

    def test_all_missing_applications_reported_in_one_run(self):
        override = {"services": {"customer-api": {"image": DIGEST_A, "pull_policy": "never"},
                                 "project-api": {"image": DIGEST_B, "pull_policy": "never"}}}
        approvals = {
            "local-customer_and_opportunity-dev.json": approval(
                "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A)),
            "local-project_management-dev.json": approval(
                "project_management", [("project-api", DIGEST_B)], ("project-migrate", DIGEST_B)),
        }
        result = self.run_preflight(override, approvals, present=())
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("customer_and_opportunity", result.stdout)
        self.assertIn("project_management", result.stdout)

    def test_complete_pinned_images_pass_without_compose_calls(self):
        override = {"services": {"customer-api": {"image": DIGEST_A, "pull_policy": "never"},
                                 "customer-migrate": {"image": DIGEST_A, "pull_policy": "never"}}}
        approvals = {"local-customer_and_opportunity-dev.json": approval(
            "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A))}
        result = self.run_preflight(override, approvals, present=(DIGEST_A,))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("COMPOSE-CALLED", result.stdout)

    def test_docker_unreachable_reported_as_dependency_not_missing_image(self):
        override = {"services": {"customer-api": {"image": DIGEST_A, "pull_policy": "never"}}}
        approvals = {"local-customer_and_opportunity-dev.json": approval(
            "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A))}
        result = self.run_preflight(override, approvals, present=(), docker_ok=False)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Docker", result.stdout)
        self.assertNotIn("镜像缺失", result.stdout)

    def test_unsafe_pin_file_rejected(self):
        override = {"services": {"customer-api": {"image": DIGEST_A, "pull_policy": "never"}}}
        approvals = {"local-customer_and_opportunity-dev.json": approval(
            "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A))}
        result = self.run_preflight(override, approvals, present=(DIGEST_A,), mode=0o644)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("权限", result.stdout)

    def test_corrupt_pin_file_rejected_without_falling_back(self):
        approvals = {"local-customer_and_opportunity-dev.json": approval(
            "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A))}
        result = self.run_preflight("{ not json", approvals, present=(DIGEST_A,))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("固定文件", result.stdout)

    def test_pin_mismatch_against_approval_is_reported(self):
        override = {"services": {"customer-api": {"image": DIGEST_B, "pull_policy": "never"}}}
        approvals = {"local-customer_and_opportunity-dev.json": approval(
            "customer_and_opportunity", [("customer-api", DIGEST_A)], ("customer-migrate", DIGEST_A))}
        result = self.run_preflight(override, approvals, present=(DIGEST_A, DIGEST_B))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("固定记录", result.stdout)

    def test_all_illegal_pins_include_application_service_and_current_image(self):
        override = {"services": {
            "contract-api": {"image": "contract-management/backend:local"},
            "customer-api": {"image": "customer-opportunity/backend:local"},
            "dashboard-migrate": {"image": "data-analysis/local-migrate:local"},
        }}
        result = self.run_preflight(override, {}, present=())
        self.assertNotEqual(result.returncode, 0, result.stdout)
        for app, service, image in (
            ("contract_management", "contract-api", "contract-management/backend:local"),
            ("customer_and_opportunity", "customer-api", "customer-opportunity/backend:local"),
            ("data_analysis", "dashboard-migrate", "data-analysis/local-migrate:local"),
        ):
            self.assertTrue(any(app in line and service in line and image in line
                                for line in result.stdout.splitlines()), result.stdout)
        self.assertNotIn("COMPOSE-CALLED", result.stdout)

    def test_invalid_pin_does_not_hide_other_missing_approved_images(self):
        override = {"services": {"customer-api": {"image": "mutable:local"},
                                 "project-api": {"image": DIGEST_B}}}
        approvals = {"local-project_management-dev.json": approval(
            "project_management", [("project-api", DIGEST_B)], ("project-migrate", DIGEST_B))}
        result = self.run_preflight(override, approvals, present=())
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("customer-api", result.stdout)
        self.assertIn("project-api", result.stdout)
        self.assertIn("project-migrate", result.stdout)
        self.assertIn(DIGEST_B, result.stdout)


if __name__ == "__main__":
    unittest.main()
