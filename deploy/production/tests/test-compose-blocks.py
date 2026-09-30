#!/usr/bin/env python3
"""Validate every optional-subsystem combination without starting containers."""
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


SOURCE = Path(__file__).resolve().parents[1] / "docker-compose.yml"
MODULES = ("contract", "project", "customer", "portal", "settlement", "data-analysis")
API = {name: name + "-api" for name in MODULES}


def trim_blocks(text, disabled):
    for name in disabled:
        pattern = rf"(?ms)^  # BEGIN SUBSYSTEM: {re.escape(name)} .*?^  # END SUBSYSTEM: {re.escape(name)} [^\n]*"
        text, count = re.subn(pattern, lambda m: "\n".join("# " + line for line in m[0].splitlines()), text)
        assert count == 1, f"missing or duplicate block: {name}"
    return text


def main():
    text = SOURCE.read_text()
    assert "profiles:" not in text
    assert "compose.https.yaml" not in text
    assert "compose.drain.yaml" not in text
    with tempfile.TemporaryDirectory(prefix="uip-compose-blocks-") as directory:
        root = Path(directory)
        runtime = root / ".env"
        runtime.write_text("")
        env = {k: v for k, v in os.environ.items() if not re.match(r"^(COMPOSE_|PUBLIC_|.*_IMAGE$|.*_RUNTIME_ENV_FILE$)", k)}
        # No optional runtime files or optional images exist in this fixture.
        env.update({
            "BASIC_PLATFORM_RUNTIME_ENV_FILE": str(runtime),
            "PLATFORM_KEYS_DIR": str(root / "custom-keys"),
            "PLATFORM_IMAGE": "test/platform:fixture", "FRONTEND_IMAGE": "test/frontend:fixture",
            "FILE_GATEWAY_IMAGE": "test/gateway:fixture",
            "KEYCLOAK_DB_PASSWORD": "fixture-not-a-secret", "KEYCLOAK_DB_ROOT_PASSWORD": "fixture-not-a-secret",
            "KEYCLOAK_ADMIN_PASSWORD": "fixture-not-a-secret", "KEYCLOAK_PUBLIC_URL": "http://localhost:18090",
            "FILE_GATEWAY_DB_PASSWORD": "fixture-not-a-secret", "FILE_GATEWAY_DB_ROOT_PASSWORD": "fixture-not-a-secret",
            "AUTH_JWT_ISSUER": "fixture", "AUTH_APPLICATION_JWT_AUDIENCE": "fixture",
            "MYSQL_DATABASE": "platform", "MYSQL_USER": "platform", "MYSQL_PASSWORD": "fixture-not-a-secret",
            "MYSQL_ROOT_PASSWORD": "fixture-not-a-secret", "CONTRACT_MYSQL_PASSWORD": "fixture-not-a-secret",
            "CONTRACT_MYSQL_ROOT_PASSWORD": "fixture-not-a-secret",
        })
        checked = 0
        for count in range(len(MODULES) + 1):
            for disabled in itertools.combinations(MODULES, count):
                candidate = root / "docker-compose.yml"
                candidate.write_text(trim_blocks(text, disabled))
                result = subprocess.run(["docker", "compose", "--project-directory", directory,
                                         "--env-file", str(runtime), "-f", str(candidate), "config", "--format", "json"],
                                        env=env, capture_output=True, text=True)
                assert result.returncode == 0, f"disabled={disabled}: {result.stderr}"
                model = json.loads(result.stdout)
                services = model["services"]
                for name in MODULES:
                    assert (API[name] in services) == (name not in disabled), (name, disabled)
                assert "contract-mysql" in services and "temporal" in services
                assert services["temporal"]["environment"]["MYSQL_SEEDS"] == "contract-mysql"
                assert services["contract-mysql"]["volumes"][0]["source"] == "contract-mysql-data"
                assert model["name"] == "basic-platform-production"
                init = services["subsystem-provisioner-socket-init"]
                assert init["user"] == "0:0" and init["network_mode"] == "none"
                assert len(init["volumes"]) == 1
                assert init["volumes"][0]["source"] == "subsystem-provisioner-socket"
                assert init["environment"]["SOCKET_OWNER"] == "0:10001"
                proxy = services["docker-socket-proxy"]
                assert proxy["environment"]["LOG_LEVEL"] == "warning"
                assert proxy["healthcheck"]["test"] == ["CMD", "haproxy", "-c", "-f", "/tmp/haproxy.cfg"]
                provisioner = services["subsystem-provisioner"]
                assert provisioner["user"] == "0:10001"
                assert provisioner["depends_on"]["docker-socket-proxy"]["condition"] == "service_healthy"
                assert set(provisioner["cap_add"]) == {"CHOWN", "DAC_OVERRIDE", "FOWNER"}
                assert provisioner["cap_drop"] == ["ALL"]
                assert "no-new-privileges:true" in provisioner["security_opt"]
                assert any(v.get("source") == "subsystem-provisioner-socket" for v in provisioner["volumes"])
                for service in services.values():
                    for mount in service.get("volumes", []):
                        if mount["target"].startswith("/app/data/keys"):
                            assert mount["source"].startswith(str(root / "custom-keys")), mount
                if "data-analysis" not in disabled:
                    metabase_init = services["data-analysis-metabase-init"]
                    assert "MYSQL_PWD" in metabase_init["environment"]
                    assert "-p" not in " ".join(metabase_init["command"]).replace("--protocol=TCP", "")
                    assert metabase_init["volumes"][0]["read_only"]
                for service in services.values():
                    for dependency in service.get("depends_on", {}):
                        assert dependency in services, dependency
                assert "profiles" not in json.dumps(services)
                checked += 1
    print(f"PASS: {checked} subsystem combinations; missing optional runtime files; shared Temporal DB and names preserved")


if __name__ == "__main__":
    main()
