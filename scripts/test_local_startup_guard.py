"""启动流程守护：子命令解析、候选构建入口、普通启动复用已登记镜像、控制面单独恢复。

只读替身：不调用 Docker、不构建、不启动容器。函数体从 docker-local.sh 抽取，
保证测的是真实实现。
"""
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "scripts/docker-local.sh").read_text()


def function(name):
    if name + "() {" not in SCRIPT:
        raise AssertionError("docker-local.sh 缺少函数 " + name)
    body = []
    heredoc = None
    for line in SCRIPT.split(name + "() {", 1)[1].splitlines(keepends=True):
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


def block(name):
    return function(name).split(name + "() {", 1)[1].rsplit("\n}", 1)[0]


HEADER = "".join([
    "set -Eeuo pipefail\nexec 3>&1\n",
    "log() { printf '[docker-local] %s\\n' \"$*\" >&3; }\n",
    "fail() { printf 'FAIL: %s\\n' \"$*\" >&3; exit 1; }\n",
])

ALL_SERVICES = ["api", "file-gateway", "contract-api", "contract-worker", "settlement-api",
                "customer-api", "customer-presale-alert-worker", "portal-api", "project-api",
                "dashboard-migrate", "dashboard-api", "aggregation-worker", "alert-worker",
                "frontend", "presale-worker", "presale-integration-mock"]


class ComposeSubcommandTest(unittest.TestCase):
    def run_compose(self, args, override=True, skip_override=False):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "runtime").mkdir()
            if override:
                (root / "runtime" / "local-license.compose.json").write_text('{"services": {}, "volumes": {}}')
            harness = HEADER + "".join([
                "project_root=" + str(root) + "\n",
                "compose_project=basic-platform-local\n",
                "compose_file=/tmp/compose.local.yaml\n",
                "env_file=/dev/null\ncontract_env_file=/dev/null\ncustomer_env_file=/dev/null\n",
                "portal_env_file=/dev/null\nproject_env_file=/dev/null\ndata_analysis_env_file=/dev/null\n",
                "settlement_env_file=/dev/null\n",
                "portal_configured() { return 0; }\nproject_configured() { return 1; }\n",
                "data_analysis_configured() { return 1; }\n",
                "docker() { printf 'DOCKER %s\\n' \"$*\" >&3; }\n",
                function("compose"),
                "compose " + " ".join(args) + "\n",
            ])
            env = {"PATH": "/usr/bin:/bin"}
            if skip_override:
                env["COMPOSE_SKIP_LICENSE_OVERRIDE"] = "1"
            return subprocess.run(["bash", "-c", harness], text=True, capture_output=True, env=env)

    def test_build_does_not_mount_license_override(self):
        result = self.run_compose(["build", "api"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("local-license.compose.json", result.stdout)

    def test_up_mounts_license_override(self):
        result = self.run_compose(["up", "-d", "--wait", "api"])
        self.assertIn("local-license.compose.json", result.stdout)

    def test_profile_value_is_not_mistaken_for_subcommand(self):
        result = self.run_compose(["--profile", "bootstrap", "build", "api"])
        self.assertNotIn("local-license.compose.json", result.stdout)

    def test_read_only_query_can_skip_override_but_start_still_mounts_it(self):
        skipped = self.run_compose(["config", "--format", "json"], skip_override=True)
        self.assertNotIn("local-license.compose.json", skipped.stdout)
        mounted = self.run_compose(["config", "--format", "json"])
        self.assertIn("local-license.compose.json", mounted.stdout)


class BuildImagesTest(unittest.TestCase):
    def run_build(self, pinned=(), present=(), force=False, skip_preparation=False, image_map=None):
        mapping = image_map if image_map is not None else "".join(s + "\tlocal/" + s + ":local\n" for s in ALL_SERVICES)
        harness = HEADER + "".join([
            "force_build=" + ("true" if force else "false") + "\n",
            "SKIP_IMAGE_PREPARATION=" + ("1" if skip_preparation else "0") + "\n",
            "prepare_base_images() { :; }\n",
            "compose() { printf 'BUILD %s\\n' \"$*\" >&3; }\n",
            "compose_default_local_images() { printf '%s' \"$IMAGE_MAP\"; }\n",
            "local_image_present() { case \" $PRESENT \" in *\" $1 \"*) return 0 ;; *) return 1 ;; esac; }\n",
            "pinned_local_service() { case \" $PINNED \" in *\" $1 \"*) return 0 ;; *) return 1 ;; esac; }\n",
            function("build_images"),
            "build_images\n",
        ])
        return subprocess.run(["bash", "-c", harness], text=True, capture_output=True, env={
            "PATH": "/usr/bin:/bin", "IMAGE_MAP": mapping,
            "PINNED": " ".join(pinned), "PRESENT": " ".join(present),
        })

    def test_registered_services_are_not_rebuilt_on_plain_up(self):
        result = self.run_build(pinned=ALL_SERVICES, present=())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("BUILD", result.stdout)
        self.assertIn("跳过镜像构建", result.stdout)

    def test_missing_unregistered_image_is_built(self):
        result = self.run_build(pinned=(), present=())
        self.assertIn("BUILD", result.stdout)

    def test_explicit_build_prepares_candidates_without_claiming_adoption(self):
        result = self.run_build(force=True)
        self.assertIn("BUILD", result.stdout)
        self.assertIn("不会替换已批准固定记录", result.stdout)

    def test_skip_image_preparation_does_not_bypass_preflight(self):
        result = self.run_build(skip_preparation=True)
        self.assertNotIn("BUILD", result.stdout)
        self.assertIn("镜像存在性预检仍然生效", result.stdout)

    def test_unparsable_image_map_falls_back_to_building_all(self):
        result = self.run_build(image_map="")
        self.assertIn("BUILD", result.stdout)
        self.assertIn("保守策略", result.stdout)

    def test_core_only_build_scope_excludes_business_services(self):
        mapping = "".join(s + "\tlocal/" + s + ":local\n" for s in ALL_SERVICES)
        harness = HEADER + "".join([
            "core_only=true\n",
            "force_build=false\n",
            "SKIP_IMAGE_PREPARATION=0\n",
            "prepare_base_images() { :; }\n",
            "compose() { printf 'BUILD %s\\n' \"$*\" >&3; }\n",
            "compose_default_local_images() { printf '%s' \"$IMAGE_MAP\"; }\n",
            "local_image_present() { return 1; }\n",
            "pinned_local_service() { return 1; }\n",
            function("build_images"),
            "build_images\n",
        ])
        result = subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                                env={"PATH": "/usr/bin:/bin", "IMAGE_MAP": mapping})
        self.assertIn("BUILD", result.stdout)
        for business in ("customer-api", "contract-api", "settlement-api", "project-api", "dashboard-api"):
            self.assertNotIn(business, result.stdout)


class BuildCandidateImagesTest(unittest.TestCase):
    def run_entry(self):
        harness = HEADER + "".join([
            "prepare_base_images() { printf 'PREPARE\\n' >&3; }\n",
            "compose() { printf 'COMPOSE %s\\n' \"$*\" >&3; }\n",
            "compose_run() { printf 'COMPOSE-RUN %s\\n' \"$*\" >&3; }\n",
            "compose_default_local_images() { printf 'api\\tbasic-platform/backend:local\\n'; }\n",
            "local_image_id() { printf 'sha256:fake'; }\n",
            "preflight_local_license_images() { printf 'PREFLIGHT\\n' >&3; }\n",
            function("build_candidate_images"),
            "build_candidate_images\n",
        ])
        return subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                              env={"PATH": "/usr/bin:/bin"})

    def test_candidate_build_runs_without_preflight_migrations_or_containers(self):
        result = self.run_entry()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("COMPOSE", result.stdout)
        self.assertIn("build", result.stdout)
        self.assertNotIn("PREFLIGHT", result.stdout)
        self.assertNotIn("COMPOSE-RUN", result.stdout)

    def test_candidate_build_lists_images_and_next_step(self):
        result = self.run_entry()
        self.assertIn("basic-platform/backend:local", result.stdout)
        self.assertIn("prepare-license", result.stdout)
        self.assertIn("受控更新", result.stdout)


class ControlPlaneOnlyTest(unittest.TestCase):
    def run_entry(self):
        harness = HEADER + "".join([
            "ensure_platform_env_file() { printf 'ENSURE platform\\n' >&3; }\n",
            "ensure_contract_env_file() { printf 'ENSURE contract %s\\n' \"$*\" >&3; }\n",
            "build_images() { printf 'BUILD\\n' >&3; }\n",
            "bootstrap_admin_if_needed() { printf 'ADMIN-BOOTSTRAP\\n' >&3; }\n",
            "ensure_subsystem_discovery_candidates() { printf 'DISCOVERY\\n' >&3; }\n",
            "verify_local_file_gateway() { printf 'VERIFY\\n' >&3; }\n",
            "compose_up_wait() { printf 'UP %s\\n' \"$*\" >&3; }\n",
            "compose_run() { printf 'RUN %s\\n' \"$*\" >&3; }\n",
            "validate_all_local_runtime_targets() { printf 'LEAK validate_runtime\\n' >&3; return 1; }\n",
            "validate_all_keycloak_runtimes() { printf 'LEAK validate_keycloak\\n' >&3; return 1; }\n",
            "prepare_gateway_config() { printf 'LEAK gateway\\n' >&3; return 1; }\n",
            "ensure_catalog_publisher_credentials_consistent() { printf 'LEAK catalog\\n' >&3; return 1; }\n",
            "disable_contract_startup_catalog_sync() { printf 'LEAK contract_sync\\n' >&3; return 1; }\n",
            "run_migrations() { printf 'LEAK business_migrations\\n' >&3; return 1; }\n",
            function("start_control_plane_only"),
            "start_control_plane_only\n",
        ])
        return subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                              env={"PATH": "/usr/bin:/bin"})

    def test_control_plane_start_is_ordered(self):
        result = self.run_entry()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        lines = result.stdout.splitlines()
        order = ["UP 控制面基础依赖", "RUN run --rm --no-deps migrate", "ADMIN-BOOTSTRAP", "UP 统一文件网关",
                 "UP 部署 Agent", "UP Keycloak", "UP 基础平台 API", "UP 统一前端", "DISCOVERY"]
        positions = []
        for marker in order:
            index = next((i for i, line in enumerate(lines) if line.startswith(marker)), None)
            self.assertIsNotNone(index, "缺少阶段 " + marker + " in " + repr(lines))
            positions.append(index)
        self.assertEqual(positions, sorted(positions), result.stdout)

    def test_control_plane_bootstraps_the_super_admin(self):
        result = self.run_entry()
        self.assertIn("ADMIN-BOOTSTRAP", result.stdout, result.stdout)
        lines = result.stdout.splitlines()
        admin = next(i for i, line in enumerate(lines) if line == "ADMIN-BOOTSTRAP")
        api = next(i for i, line in enumerate(lines) if line.startswith("UP 基础平台 API"))
        self.assertLess(admin, api)

    def test_control_plane_start_uses_no_deps_everywhere(self):
        result = self.run_entry()
        ups = [line for line in result.stdout.splitlines() if line.startswith("UP ")]
        self.assertTrue(ups, result.stdout)
        for line in ups:
            self.assertIn("--no-deps", line, result.stdout)

    def test_control_plane_prepares_discovery_after_frontend(self):
        result = self.run_entry()
        lines = result.stdout.splitlines()
        frontend = next(i for i, line in enumerate(lines) if line.startswith("UP 统一前端"))
        discovery = next(i for i, line in enumerate(lines) if line == "DISCOVERY")
        self.assertLess(frontend, discovery)

    def test_control_plane_start_never_touches_business_services_or_validation(self):
        result = self.run_entry()
        for forbidden in ("temporal", "contract-api", "settlement-api", "customer-api",
                          "project-api", "dashboard-api", "portal-api", "LEAK"):
            self.assertNotIn(forbidden, result.stdout, result.stdout)

    def test_core_only_does_not_run_business_migrations(self):
        result = self.run_entry()
        for business in ("customer-migrate", "contract-migrate", "settlement-migrate",
                         "project-migrate", "portal-migrate", "dashboard-migrate"):
            self.assertNotIn(business, result.stdout)


class DevUpTest(unittest.TestCase):
    def run_entry(self, docker_ok=True, extra=""):
        harness = HEADER + "".join([
            "script_dir=/tmp\nproject_root=/tmp\nworkspace_root=/tmp\nadmin_account_name=localadmin\n",
            "docker_control_available() { return " + ("0" if docker_ok else "1") + "; }\n",
            "start_control_plane_only() { printf 'CONTROL-PLANE\\n' >&3; }\n",
            "python3() { printf 'PY %s\\n' \"$*\" >&3; return 0; }\n",
            function("dev_up"),
            function("dev_approve"),
            "dev_up\n" if not extra else extra,
        ])
        return subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                              env={"PATH": "/usr/bin:/bin"})

    def test_dev_up_runs_control_plane_then_candidates_then_status(self):
        result = self.run_entry()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        lines = result.stdout.splitlines()
        control = next(i for i, line in enumerate(lines) if line == "CONTROL-PLANE")
        candidates = next(i for i, line in enumerate(lines) if "refresh-local-runtime-approvals" in line)
        status = next(i for i, line in enumerate(lines) if "local-status.py" in line)
        self.assertLess(control, candidates)
        self.assertLess(candidates, status)

    def test_dev_up_never_publishes_approvals(self):
        result = self.run_entry()
        self.assertIn("只生成，不发布", result.stdout)
        for line in result.stdout.splitlines():
            if "refresh-local-runtime-approvals" in line:
                self.assertNotIn("--approve", line, result.stdout)
                self.assertIn("--operator", line, result.stdout)

    def test_dev_up_stops_immediately_when_docker_is_unavailable(self):
        result = self.run_entry(docker_ok=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docker context use orbstack", result.stdout)
        self.assertNotIn("CONTROL-PLANE", result.stdout)

    def test_dev_approve_forwards_caller_flags_and_requires_them(self):
        # 发布必须由调用方显式给出 --approve：本命令只做转发，不自行加上批准标志。
        body = block("dev_approve")
        self.assertIn("refresh-local-runtime-approvals.py", body)
        self.assertIn("--operator", body)
        self.assertIn('"$@"', body)
        self.assertNotIn("--approve", body)


class NoArgumentGuardTest(unittest.TestCase):
    def test_bare_invocation_does_not_default_to_up(self):
        # 无参数必须只打印用法：守卫要早于 prepare-license 早起分支与最终分发。
        guard = SCRIPT.index("if (($# == 0)); then")
        self.assertLess(guard, SCRIPT.index('if [[ "${1:-}" == "prepare-license" ]]'))
        self.assertLess(guard, SCRIPT.index('case "$command_name" in'))
        self.assertIn("本地启动没有隐式默认动作", SCRIPT)

    def test_bare_invocation_exits_without_running_docker(self):
        result = subprocess.run(["bash", str(ROOT / "scripts/docker-local.sh")],
                                text=True, capture_output=True, env={"PATH": "/usr/bin:/bin"})
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("没有隐式默认动作", combined)
        for marker in ("检查并串行准备基础镜像", "控制面单独恢复", "执行基础平台数据库迁移"):
            self.assertNotIn(marker, combined)


class RecoveryOrderTest(unittest.TestCase):
    def test_start_stack_builds_candidates_before_checking_running_images(self):
        body = block("start_stack")
        self.assertLess(body.index("build_images"), body.index("preflight_local_license_images"))
        self.assertLess(body.index("preflight_local_license_images"), body.index("run_migrations"))

    def test_explicit_build_marks_candidates_prepared_for_the_preflight_message(self):
        body = block("start_stack")
        self.assertIn("preflight_candidates_prepared=true", body)

    def test_preflight_recovery_message_does_not_loop(self):
        body = block("preflight_local_license_images")
        self.assertIn("preflight_candidates_prepared", body)
        self.assertIn("不要重复执行构建", body)
        self.assertIn("bash scripts/docker-local.sh build-images", body)
        self.assertNotIn("候选镜像需要重新准备：bash scripts/docker-local.sh up --build", body)

    def test_discovery_candidate_skip_path_has_no_shell_errors(self):
        # 走"跳过"分支：变量后紧跟全角括号会吞掉字节，在 set -u 下报 unbound variable。
        harness = HEADER + "".join([
            "compose_project=basic-platform-local\n",
            "docker() { return 0; }\n",  # 此场景没有已运行容器；不触及真实 Docker。
            "subsystem_discovery_service() { printf 'svc'; }\n",
            "compose_run() { return 1; }\n",
            function("ensure_subsystem_discovery_candidates"),
            "ensure_subsystem_discovery_candidates\n",
        ])
        # 必须带 UTF-8 locale：否则 bash 按字节处理多字节括号，复现不出该缺陷。
        result = subprocess.run(["bash", "-c", harness], text=True, capture_output=True,
                                env={"PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8", "LC_ALL": "en_US.UTF-8"})
        self.assertNotIn("unbound variable", result.stderr, result.stderr)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("[跳过]", result.stdout)

    def test_discovery_candidates_cover_every_subsystem(self):
        services = {}
        for line in block("subsystem_discovery_service").splitlines():
            parts = line.strip().split(")")
            if len(parts) == 2 and parts[1].strip().startswith("printf"):
                services[parts[0]] = parts[1].split("'")[1]
        self.assertEqual(sorted(services), sorted([
            "contract_management", "customer_and_opportunity", "customer_portal",
            "project_management", "settlement", "data_analysis"]))
        self.assertEqual(sorted(set(services.values())), sorted([
            "contract-api", "customer-api", "portal-api", "project-api",
            "settlement-api", "dashboard-api"]))

    def test_start_stack_and_core_only_both_prepare_discovery(self):
        self.assertIn("ensure_subsystem_discovery_candidates", block("start_stack"))
        self.assertIn("ensure_subsystem_discovery_candidates", block("start_control_plane_only"))

    def test_prepare_discovery_command_is_guarded_by_preflight(self):
        dispatch = SCRIPT.split('case "$command_name" in', 1)[1]
        section = dispatch.split("prepare-discovery)", 1)[1].split(";;", 1)[0]
        self.assertIn("preflight_local_license_images", section)
        self.assertIn("ensure_subsystem_discovery_candidates", section)

    def test_bootstrap_admin_command_reuses_the_guarded_initializer(self):
        dispatch = SCRIPT.split('case "$command_name" in', 1)[1]
        section = dispatch.split("bootstrap-admin)", 1)[1].split(";;", 1)[0]
        self.assertIn("bootstrap_admin_if_needed", section)
        self.assertIn("ensure_platform_env_file", section)
        self.assertIn("ensure_contract_env_file", section)

    def test_super_admin_prompts_state_the_rules(self):
        body = block("bootstrap_admin_if_needed")
        self.assertIn("仅字母数字与 . _ -", body)
        self.assertIn("含大写/小写/数字/符号", body)
        self.assertIn("不回显", body)

    def test_super_admin_password_reads_with_prompt_on_a_terminal(self):
        body = block("bootstrap_admin_if_needed")
        self.assertIn('if [[ -t 0 ]]; then\n            read -r -s -p', body)
        self.assertIn('elif [[ "$admin_password_stdin" == true ]]; then\n            IFS= read -r admin_password || true', body)

    def test_bootstrap_admin_is_idempotent_by_status_probe(self):
        body = block("bootstrap_admin_if_needed")
        self.assertIn("--status", body)
        self.assertIn("超级管理员已存在，跳过初始化", body)
        self.assertIn("password-stdin", body)

    def test_core_only_routes_to_the_isolated_entry(self):
        dispatch = SCRIPT.split('case "$command_name" in', 1)[1]
        self.assertIn("start_control_plane_only", dispatch)
        self.assertIn("build-images)", dispatch)
        self.assertIn("build_candidate_images", dispatch)


if __name__ == "__main__":
    unittest.main()
