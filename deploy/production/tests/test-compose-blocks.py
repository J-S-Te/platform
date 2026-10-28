#!/usr/bin/env python3
"""验证所有可选子系统组合，不启动容器。"""
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


MYSQL_SERVER_IMAGE = "mysql:8.4"
LOG_BIN_TRUST_FLAG = "--log-bin-trust-function-creators=1"
# 该服务用 mysql 客户端执行初始化 SQL，不是 mysqld 服务端，不要求该参数。
MYSQL_CLIENT_SERVICES = {"data-analysis-metabase-init"}
EXPECTED_MYSQL_SERVERS = {
    "keycloak-db", "platform-mysql", "contract-mysql", "file-gateway-mysql",
    "project-mysql", "customer-mysql", "portal-mysql", "settlement-mysql",
    "data-analysis-mysql",
}


def assert_mysql_servers_trust_function_creators(services):
    """MySQL 8.4 默认 log_bin=1，官方镜像创建的 MYSQL_USER 只有本库权限、没有 SUPER；
    迁移 000130 的 CREATE TRIGGER 会报 ERROR 1419（project_management 000033 的存储过程
    为一致配置）。以 mysqld 参数集启动的 mysql:8.4 服务端必须显式信任函数创建者；
    data-analysis-metabase-init 是 mysql 客户端，不属于服务端，不要求该参数。"""
    servers = set()
    for name, service in services.items():
        if service.get("image") != MYSQL_SERVER_IMAGE:
            continue
        command = service.get("command")
        assert not isinstance(command, str), (name, "command must be an argument list")
        if command is None:
            # 未写 command 时按镜像默认 CMD 运行 mysqld，无法携带该参数，按缺失处理。
            raise AssertionError((name, "mysql:8.4 server must set an explicit command with " + LOG_BIN_TRUST_FLAG))
        assert command, (name, "empty command")
        if not (command[0] == "mysqld" or command[0].startswith("-")):
            # 非 mysqld 参数集的 mysql:8.4 服务只允许显式登记为客户端（如 metabase-init）。
            assert name in MYSQL_CLIENT_SERVICES, (name, command)
            continue
        servers.add(name)
        trusted = [argument for argument in command if argument.startswith("--log-bin-trust-function-creators")]
        assert trusted == [LOG_BIN_TRUST_FLAG], (name, command)
    return servers


def main():
    text = SOURCE.read_text()
    assert "profiles:" not in text
    assert "compose.https.yaml" not in text
    assert "compose.drain.yaml" not in text
    profile = subprocess.run(["ruby", "-rjson", "-ryaml", "-e",
                              "puts JSON.generate(YAML.safe_load(File.read(ARGV[0]), permitted_classes: [], aliases: false))",
                              str(SOURCE.parent / "subsystems.d/customer_and_opportunity-prod.yaml")],
                             capture_output=True, text=True, check=True)
    crm_profile = json.loads(profile.stdout)
    crm_dependencies = crm_profile["compose"]["dependency_services"]
    assert set(crm_dependencies) == {"customer-mysql", "contract-mysql", "temporal"}
    assert {"CONTRACT_MYSQL_PASSWORD", "CONTRACT_MYSQL_ROOT_PASSWORD"} <= set(crm_profile["runtime"]["required_infrastructure_keys"])
    with tempfile.TemporaryDirectory(prefix="uip-compose-blocks-") as directory:
        root = Path(directory)
        runtime = root / ".env"
        runtime.write_text("")
        env = {k: v for k, v in os.environ.items() if not re.match(r"^(COMPOSE_|PUBLIC_|.*_IMAGE$|.*_RUNTIME_ENV_FILE$)", k)}
        # 此夹具不存在可选运行文件或可选镜像。
        env.update({
            "BASIC_PLATFORM_RUNTIME_ENV_FILE": str(runtime),
            "PLATFORM_KEYS_DIR": str(root / "custom-keys"),
            "PLATFORM_IMAGE": "test/platform:fixture", "FRONTEND_IMAGE": "test/frontend:fixture",
            "FILE_GATEWAY_IMAGE": "test/gateway:fixture",
            "SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT": str(root / "custom-deploy"),
            "FILE_GATEWAY_HOST_ROOT": "",
            "KEYCLOAK_DB_PASSWORD": "fixture-not-a-secret", "KEYCLOAK_DB_ROOT_PASSWORD": "fixture-not-a-secret",
            "KEYCLOAK_ADMIN_PASSWORD": "fixture-not-a-secret", "KEYCLOAK_PUBLIC_URL": "http://localhost:18090",
            "FILE_GATEWAY_DB_PASSWORD": "fixture-not-a-secret", "FILE_GATEWAY_DB_ROOT_PASSWORD": "fixture-not-a-secret",
            "AUTH_JWT_ISSUER": "fixture", "AUTH_APPLICATION_JWT_AUDIENCE": "fixture",
            "MYSQL_DATABASE": "platform", "MYSQL_USER": "platform", "MYSQL_PASSWORD": "fixture-not-a-secret",
            "MYSQL_ROOT_PASSWORD": "fixture-not-a-secret", "CONTRACT_MYSQL_PASSWORD": "fixture-not-a-secret",
            "CONTRACT_MYSQL_ROOT_PASSWORD": "fixture-not-a-secret",
        })
        checked = 0
        mysql_servers_checked = set()
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
                mysql_servers_checked |= assert_mysql_servers_trust_function_creators(services)
                for name in MODULES:
                    assert (API[name] in services) == (name not in disabled), (name, disabled)
                assert "contract-mysql" in services and "temporal" in services
                assert services["temporal"]["environment"]["MYSQL_SEEDS"] == "contract-mysql"
                assert services["contract-mysql"]["volumes"][0]["source"] == "contract-mysql-data"
                if "customer" not in disabled:
                    # 仅购买 CRM 时全部依赖目标仍须可用；--no-deps 刻意不会隐式启动被省略的基础设施。
                    assert set(crm_dependencies) <= set(services)
                    for dependency in crm_dependencies:
                        assert set(services[dependency].get("depends_on", {})) <= set(crm_dependencies)
                    assert services["customer-presale-worker"]["environment"]["PRESALE_TEMPORAL_ENABLED"] == "true"
                    assert services["temporal"]["depends_on"]["contract-mysql"]["condition"] == "service_healthy"
                assert model["name"] == "basic-platform-production"
                init = services["subsystem-provisioner-socket-init"]
                assert init["user"] == "0:0" and init["network_mode"] == "none"
                assert len(init["volumes"]) == 1
                assert init["volumes"][0]["source"] == "subsystem-provisioner-socket"
                assert init["environment"]["SOCKET_OWNER"] == "0:10001"
                proxy = services["docker-socket-proxy"]
                assert proxy["image"] == "ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459"
                assert proxy["environment"]["LOG_LEVEL"] == "warning"
                assert proxy["healthcheck"]["test"] == ["CMD", "haproxy", "-c", "-f", "/tmp/haproxy.cfg"]
                api = services["platform-api"]
                worker = services["platform-worker"]
                for key in ["SUBSYSTEM_ONBOARDING_AUTOMATION_ENABLED", "SUBSYSTEM_ONBOARDING_MODE", "SUBSYSTEM_PROVISIONING_SOCKET_PATH", "SUBSYSTEM_PRODUCTION_DEPLOY_ROOT", "SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID", "SUBSYSTEM_PRODUCTION_PROFILES_DIR", "SUBSYSTEM_ONBOARDING_TIMEOUT", "LICENSE_RUNTIME_PLATFORM_BASE_URL", "LICENSE_RUNTIME_PUBLIC_KEY_PATH", "LICENSE_RUNTIME_STATE_DIRECTORY", "AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS"]:
                    assert worker["environment"].get(key) == api["environment"][key], (key, "access worker is not wired like API")
                assert worker["depends_on"]["subsystem-provisioner"]["condition"] == "service_healthy"
                assert any(v.get("source") == "subsystem-provisioner-socket" and v["target"] == "/run/basic-platform-provisioner" for v in worker["volumes"])
                assert any(v["target"] == str(root / "custom-deploy/subsystems.d") and v.get("read_only") for v in worker["volumes"])
                assert all(v["target"] != "/var/run/docker.sock" for v in worker["volumes"])
                provisioner = services["subsystem-provisioner"]
                gateway = services["file-gateway"]
                gateway_storage = next(v for v in gateway["volumes"] if v["target"] == "/app/data/file-gateway")
                assert gateway_storage["source"] == str(root / "custom-deploy/data/file-gateway")
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
                assert ("contract-worker" in services) == ("contract" not in disabled)
                if "contract" not in disabled:
                    for component in ("contract-api", "contract-worker"):
                        assert services[component]["environment"]["CONTRACT_PROCESS_MODE"] == "split"
                        assert services[component]["environment"]["CONTRACT_RUN_WORKER_WITH_API"] == "false"
                    assert services["contract-worker"]["command"] == ["./worker"]
                    assert "ports" not in services["contract-worker"]
                licensed = ("contract-api", "contract-worker", "project-api", "project-sla-notifier", "customer-api",
                            "customer-opportunity-alert-worker", "customer-owner-notification-worker",
                            "customer-presale-alert-worker", "customer-presale-assignment-notification-worker",
                            "customer-presale-progress-notification-worker", "customer-notification-delivery-worker",
                            "customer-presale-worker", "portal-api", "portal-invite-compensation-worker",
                            "settlement-api", "settlement-worker", "data-analysis-api",
                            "data-analysis-aggregation-worker", "data-analysis-alert-worker")
                for component in licensed:
                    if component not in services:
                        continue
                    mounts = services[component].get("volumes", [])
                    state = [v for v in mounts if v["target"] == "/var/lib/commercial-license"]
                    assert len(state) == 1 and state[0]["source"] == "license-state-" + component
                    keys = [v for v in mounts if v["target"] == "/app/data/keys/jwt-ed25519-public.pem"]
                    assert len(keys) == 1 and keys[0]["read_only"]
                    assert f"path: runtime/license-{component}.env" in text
                assert services["platform-api"]["environment"]["LICENSE_RUNTIME_PLATFORM_BASE_URL"] == "http://platform-api:8080"
                checked += 1
        # 每个可选子系统关闭组合都渲染后，必须覆盖全部预期 MySQL 服务端，防止新增服务漏加参数。
        assert mysql_servers_checked == EXPECTED_MYSQL_SERVERS, mysql_servers_checked
        env["FILE_GATEWAY_HOST_ROOT"] = str(root / "company-custom-files")
        explicit = subprocess.run(["docker", "compose", "--project-directory", directory,
                                   "--env-file", str(runtime), "-f", str(candidate), "config", "--format", "json"],
                                  env=env, capture_output=True, text=True)
        assert explicit.returncode == 0, explicit.stderr
        gateway = json.loads(explicit.stdout)["services"]["file-gateway"]
        gateway_storage = next(v for v in gateway["volumes"] if v["target"] == "/app/data/file-gateway")
        assert gateway_storage["source"] == str(root / "company-custom-files")
    print(f"PASS: {checked} subsystem combinations; missing optional runtime files; shared Temporal DB and names preserved")


if __name__ == "__main__":
    main()
