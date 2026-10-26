"""替换命令边界后验证真实刷新函数，不向 Docker 写入。"""
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "scripts/docker-local.sh").read_text()


def function(name):
    return name + "() {" + SCRIPT.split(name + "() {", 1)[1].split("\n}\n", 1)[0] + "\n}\n"


def source_lib(name):
    """加载 scripts/lib/<name>.sh 真实库文件（仅函数定义，加载零副作用）。"""
    return 'source "' + str(ROOT / "scripts/lib" / (name + ".sh")) + '"\n'


class LocalRuntimeTargetValidationTest(unittest.TestCase):
    def validate(self, application, environment, client_id, *, succeeds=True,
                 application_code="", environment_code=""):
        harness = """
set -Eeuo pipefail
fail() { echo "FAIL: $*" >&2; exit 7; }
""" + source_lib("runtime-validation") + """
env_value() {
    local key="$2"
    case "$key" in
        PLATFORM_APPLICATION_CODE) printf '%s' "$STUB_APPLICATION_CODE" ;;
        PLATFORM_ENVIRONMENT_CODE) printf '%s' "$STUB_ENVIRONMENT_CODE" ;;
        *) printf '%s' "$STUB_CLIENT_ID" ;;
    esac
}
"""
        harness += 'validate_local_runtime_target 描述 runtime.env "$APPLICATION" "$ENVIRONMENT" CLIENT_KEY\n'
        env = {"PATH": "/usr/bin:/bin", "STUB_APPLICATION_CODE": application_code or application,
               "STUB_ENVIRONMENT_CODE": environment_code or environment, "STUB_CLIENT_ID": client_id}
        result = subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                                env={**env, "APPLICATION": application, "ENVIRONMENT": environment})
        self.assertEqual(result.returncode == 0, succeeds, result.stdout + result.stderr)
        return result

    def test_portal_accepts_server_managed_broker_client(self):
        result = self.validate("customer_portal", "dev", "keycloak-customer-portal-broker")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_portal_accepts_canonical_onboarding_client(self):
        result = self.validate("customer_portal", "dev", "customer_portal-dev-web")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_portal_rejects_foreign_client(self):
        result = self.validate("customer_portal", "dev", "customer_and_opportunity-dev-web", succeeds=False)
        self.assertIn("不匹配", result.stderr)

    def test_other_applications_must_use_canonical_client(self):
        self.validate("contract", "dev", "contract-dev-web")
        result = self.validate("contract", "dev", "keycloak-customer-portal-broker", succeeds=False)
        self.assertIn("不匹配", result.stderr)

    def test_declared_environment_conflict_fails_before_client_check(self):
        result = self.validate("customer_portal", "dev", "keycloak-customer-portal-broker",
                               succeeds=False, environment_code="prod")
        self.assertIn("目标环境", result.stderr)

    def test_stale_metadata_still_skips_validation(self):
        self.validate("customer_portal", "dev", "", application_code="", environment_code="")


class CrmWorkerInventoryTest(unittest.TestCase):
    def test_up_starts_every_licensed_crm_worker(self):
        # start-presale-worker 的授权门禁要求 CRM 九个运行组件在窗口内全部 ACK：
        # customer-api、presale-worker、presale-alert-worker 由各自步骤启动，其余六个
        # worker 必须全部在 customer_notification_worker_services 中，否则 up 永远卡在门禁。
        block = SCRIPT.split("customer_notification_worker_services=(", 1)[1].split(")", 1)[0]
        services = {line.strip() for line in block.splitlines() if line.strip()}
        self.assertEqual(services, {
            "customer-opportunity-alert-worker",
            "customer-owner-notification-worker",
            "customer-presale-assignment-notification-worker",
            "customer-presale-progress-notification-worker",
            "customer-notification-delivery-worker",
            "contract-transfer-worker",
        })


class DiscoveryCandidateSafetyTest(unittest.TestCase):
    def candidates(self, running_services, *, compose_fails=False, succeeds=True):
        harness = """
set -Eeuo pipefail
compose_project=basic-platform-local
log() { printf '%s\\n' "$*"; }
subsystem_discovery_service() {
    case "$1" in
        settlement) printf 'settlement-api' ;;
        contract_management) printf 'contract-api' ;;
        *) return 1 ;;
    esac
}
compose_run() {
    if [[ "$SKIP_OVERRIDE" == "1" ]]; then
        printf 'candidate-config\\n'
        if [[ "$COMPOSE_FAILS" == "1" ]]; then return 1; fi
        return 0
    fi
    printf 'licensed-config\\n'
}
docker() {
    if [[ "$1" == "ps" ]]; then
        local service="" arg
        for arg in "$@"; do
            case "$arg" in
                label=com.docker.compose.service=*) service="${arg#label=com.docker.compose.service=}" ;;
            esac
        done
        case "$service" in
            settlement-api) [[ "$RUNNING_SETTLEMENT" == "1" ]] && printf 'container-id\\n' ;;
            contract-api) [[ "$RUNNING_CONTRACT" == "1" ]] && printf 'container-id\\n' ;;
        esac
        return 0
    fi
    return 0
}
""" + function("ensure_subsystem_discovery_candidates")
        harness += "ensure_subsystem_discovery_candidates\n"
        env = {"PATH": "/usr/bin:/bin", "SKIP_OVERRIDE": "1", "COMPOSE_FAILS": "1" if compose_fails else "0",
               "RUNNING_SETTLEMENT": "1" if "settlement-api" in running_services else "0",
               "RUNNING_CONTRACT": "1" if "contract-api" in running_services else "0"}
        result = subprocess.run(["bash", "-c", harness], text=True, capture_output=True, env=env)
        self.assertEqual(result.returncode == 0, succeeds, result.stdout + result.stderr)
        return result

    def test_running_service_container_is_not_recreated_as_candidate(self):
        result = self.candidates(["settlement-api"])
        self.assertIn("[运行中] settlement（settlement-api）", result.stdout)
        self.assertIn("已创建或已存在 1 个，跳过 1 个", result.stdout)

    def test_absent_service_still_gets_a_candidate(self):
        result = self.candidates(["settlement-api"])
        self.assertNotIn("[运行中] contract_management", result.stdout)
        self.assertNotIn("[跳过] contract_management", result.stdout)
        self.assertIn("已创建或已存在 1 个，跳过 1 个", result.stdout)

    def test_candidate_failure_is_reported_without_aborting(self):
        result = self.candidates([])
        self.assertIn("跳过", result.stdout)


class LocalControlDependenciesTest(unittest.TestCase):
    def run_refresh(self, target, fail_service=""):
        harness = """
set -Eeuo pipefail
exec 3>&1
""" + source_lib("compose") + """
force_build=false
log() { :; }
sleep() { :; }
prepare_operational_env() { :; }
ensure_catalog_publisher_credentials_consistent() { :; }
prepare_go_backend_base_images() { :; }
preflight_local_license_images() { :; }
refuse_pinned_rebuild() { :; }
report_candidate_drift() { :; }
ensure_platform_env_file() { :; }
ensure_contract_env_file() { :; }
ensure_customer_env_file() { :; }
ensure_project_env_file() { :; }
ensure_portal_env_file() { :; }
ensure_settlement_env_file() { :; }
validate_local_runtime_target() { :; }
runtime_target_environment() { printf dev; }
disable_contract_startup_catalog_sync() { :; }
sync_contract_authorization_catalog() { :; }
verify_gateway_routes() { :; }
contract_env_file=fixture
compose() { printf 'build %s\\n' "$*"; }
compose_run() {
    printf 'compose %s\\n' "$*" >&3
    if [[ -n "$FAIL_SERVICE" && " $* " == *" up "* && " $* " == *" $FAIL_SERVICE "* ]]; then
        return 1
    fi
}
"""
        # compose_up_wait 已拆分到 lib/compose.sh，由 source 提供真实实现；
        # 其余入口函数仍从 docker-local.sh 抽取。
        for name in ("verify_local_file_gateway",
                     "ensure_platform_control_dependencies", target):
            harness += function(name)
        harness += target + "\n"
        return subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                              env={"PATH": "/usr/bin:/bin", "FAIL_SERVICE": fail_service})

    def test_both_refresh_paths_restore_dependencies_before_agent(self):
        for target in ("refresh_platform_api", "refresh_contract_backend"):
            with self.subTest(target=target):
                result = self.run_refresh(target)
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = result.stdout.splitlines()
                dependencies = next(i for i, line in enumerate(calls)
                                    if "up -d --wait --no-deps mysql file-gateway-mysql docker-socket-proxy" in line)
                gateway = next(i for i, line in enumerate(calls)
                               if "up -d --wait --no-deps file-gateway" == line.removeprefix("compose "))
                agent = next(i for i, line in enumerate(calls) if "up " in line and "subsystem-provisioner" in line)
                self.assertLess(dependencies, gateway)
                self.assertLess(gateway, agent)
                self.assertNotIn("--volumes", result.stdout)
                self.assertNotIn("down", result.stdout)

    def test_missing_proxy_or_gateway_blocks_refresh_before_agent(self):
        for target in ("refresh_platform_api", "refresh_contract_backend"):
            for dependency in ("docker-socket-proxy", "file-gateway"):
                with self.subTest(target=target, dependency=dependency):
                    result = self.run_refresh(target, dependency)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(any("up " in line and "subsystem-provisioner" in line
                                         for line in result.stdout.splitlines()))

    def test_api_refresh_verifies_gateway_after_api(self):
        result = self.run_refresh("refresh_platform_api")
        self.assertLess(result.stdout.index("up -d --wait --no-deps api"),
                        result.stdout.index("exec -T api wget -qO- http://file-gateway:8086/readyz"))

    def test_helper_health_requires_socket_and_restricted_docker(self):
        helper = (ROOT / "compose.local.yaml").read_text().split("\n  subsystem-provisioner:\n", 1)[1].split("\n  worker:\n", 1)[0]
        self.assertIn("test -S /run/basic-platform-provisioner/provisioner.sock && exec docker info", helper)
        self.assertIn("DOCKER_HOST: tcp://docker-socket-proxy:2375", helper)
        self.assertNotIn("/var/run/docker.sock:", helper)
        self.assertIn("timeout: 3s", helper)


if __name__ == "__main__":
    unittest.main()
