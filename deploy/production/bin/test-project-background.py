#!/usr/bin/env python3
"""Exercise the project restart path with isolated external command doubles."""

from pathlib import Path
import json
import subprocess
import tempfile
import unittest
import re


SOURCE = Path(__file__).resolve().parent
WORKSPACE = SOURCE.parents[3]


class ProjectBackgroundAssemblyTests(unittest.TestCase):
    def test_image_contains_control_protocol_and_executable(self):
        dockerfile = (WORKSPACE / "project_management/Dockerfile").read_text()
        self.assertIn("./cmd/background-control", dockerfile)
        self.assertIn("/usr/local/bin/project-background-control", dockerfile)
        self.assertIn('io.uip.project-background.protocol="1"', dockerfile)

    def test_production_bootstrap_probe_and_temporal_are_wired_to_both_components(self):
        production = SOURCE.parent
        compose = (production / "docker-compose.yml").read_text()
        for filename in ("docker-compose.yml", "compose.yaml"):
            with self.subTest(filename=filename):
                text = (production / filename).read_text()
                notifier = re.search(r"\n  project-sla-notifier:\n(.*?)(?=\n  [a-zA-Z][a-zA-Z0-9_-]*:|\Z)", text, re.S).group(1)
                self.assertIn("TEMPORAL_ADDRESS:", notifier)
                self.assertIn("PROJECT_BACKGROUND_BUILD_ID:", notifier)
                self.assertIn("PROJECT_RUNTIME_ENV_FILE:-runtime/project.env", text)
                self.assertNotIn('PROJECT_BACKGROUND_ENGINE: ${PROJECT_BACKGROUND_ENGINE:-}', notifier)
                self.assertIn("PROJECT_BACKGROUND_HEALTH_ADDRESS:-127.0.0.1:9093", notifier)
                self.assertNotIn('"kill -0 1"', notifier)
                self.assertNotIn('PROJECT_RUN_WORKER_WITH_API: "true"', text)

    def test_release_and_runtime_select_temporal_without_inventing_a_third_consumer(self):
        production = SOURCE.parent
        self.assertIn("PROJECT_BACKGROUND_BUILD_ID=", (production / ".release.env.example").read_text())
        profile = (production / "subsystems.d/project_management-prod.yaml").read_text()
        self.assertIn("PROJECT_BACKGROUND_ENGINE: temporal", profile)
        self.assertIn("runtime_services: [project-api, project-sla-notifier]", profile)


class ProjectBackgroundLifecycleTests(unittest.TestCase):
    def test_project_build_id_uses_complete_immutable_image_digest(self):
        image = "qa.invalid/project@sha256:" + "a" * 64
        script = 'source "$1/compose-scope.sh"; project_background_build_id "$2"'
        result = subprocess.run(
            ["/bin/bash", "-c", script, "qa", str(SOURCE), image],
            env={"PATH": "/usr/bin:/bin:/usr/sbin:/sbin"},
            text=True, capture_output=True, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "project-background-sha256-" + "a" * 64)

    def test_project_build_id_rejects_mutable_tag(self):
        result = subprocess.run(
            ["/bin/bash", "-c", 'source "$1/compose-scope.sh"; project_background_build_id "$2"', "qa", str(SOURCE), "qa.invalid/project:latest"],
            env={"PATH": "/usr/bin:/bin:/usr/sbin:/sbin"},
            text=True, capture_output=True, check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("不可变", result.stderr)

    def restart_project(self, *, prepare_fails=False, activate_fails=False):
        with tempfile.TemporaryDirectory(prefix="uip-project-background-test-") as temporary:
            root = Path(temporary)
            (root / "runtime").mkdir()
            (root / "bin").mkdir()
            backup = root / "bin" / "backup-all.sh"
            backup.write_text('#!/bin/bash\nprintf "backup\\n" >> "$CALLS"\n')
            backup.chmod(0o700)
            script = r'''
set -Eeuo pipefail
source "$TASK_SOURCE/start-enabled.sh"
deploy_dir="$TASK_ROOT"
script_dir="$TASK_ROOT/bin"
release_file="$TASK_ROOT/.release.env"
die() { printf '%s\n' "$*" >&2; exit 1; }
scope_require() { return 0; }
scope_project() { printf 'qa-project\n'; }
scope_runtime_services() { printf '%s\n' project-api project-sla-notifier; }
subsystem_metadata() {
  subsystem_runtime="$TASK_ROOT/runtime/project.env"
  subsystem_key=PROJECT_IMAGE
  subsystem_service=project-api
  subsystem_candidate=uip-project-candidate
}
runtime_ready() { return 0; }
env_get() { printf 'qa.invalid/project@sha256:%064d\n' 1; }
flock() { return 0; }
require_control_plane_reload_clearance() { return 0; }
refresh_subsystem_provisioner_config() { return 0; }
docker() {
  case "$*" in
    'image inspect '*) return 0 ;;
    'container inspect '*) return 1 ;;
    'ps -aq '*) printf 'existing-qa-project\n' ;;
    *) printf 'unexpected docker invocation\n' >&2; return 77 ;;
  esac
}
compose() { printf 'compose %s\n' "$*" >> "$CALLS"; }
verify_current_services() { printf 'verify-runtime %s\n' "$*" >> "$CALLS"; }
project_background_prepare() {
  printf 'background prepare\n' >> "$CALLS"
  [[ "$PREPARE_FAILS" != true ]]
}
project_background_activate() {
  printf 'background activate\n' >> "$CALLS"
  [[ "$ACTIVATE_FAILS" != true ]]
}
start_registered project
'''
            environment = {
                "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
                "TASK_SOURCE": str(SOURCE),
                "TASK_ROOT": str(root),
                "CALLS": str(root / "calls"),
                "PREPARE_FAILS": str(prepare_fails).lower(),
                "ACTIVATE_FAILS": str(activate_fails).lower(),
            }
            result = subprocess.run(
                ["/bin/bash", "-c", script],
                cwd=root,
                env=environment,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=15,
                check=False,
            )
            calls_path = root / "calls"
            calls = calls_path.read_text().splitlines() if calls_path.exists() else []
            return result, calls

    def test_project_restart_prepares_before_replacement_and_activates_before_verify(self):
        result, calls = self.restart_project()
        self.assertEqual(result.returncode, 0, result.stderr)
        required = [
            "background prepare",
            "compose run --no-deps project-migrate",
            "compose up -d --no-deps --wait --wait-timeout 180 project-api project-sla-notifier",
            "background activate",
            "verify-runtime project-api project-sla-notifier",
        ]
        positions = []
        for entry in required:
            self.assertIn(entry, calls)
            positions.append(calls.index(entry))
        self.assertEqual(positions, sorted(positions))

    def test_prepare_failure_does_not_migrate_or_replace_project(self):
        result, calls = self.restart_project(prepare_fails=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("background prepare", calls)
        self.assertFalse(any("project-migrate" in call for call in calls), calls)
        self.assertFalse(any("project-api project-sla-notifier" in call for call in calls), calls)

    def test_activate_failure_does_not_report_verified_project(self):
        result, calls = self.restart_project(activate_fails=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("background activate", calls)
        self.assertFalse(any(call.startswith("verify-runtime ") for call in calls), calls)


class ProjectBackgroundControlTests(unittest.TestCase):
    def run_prepare(self, *, marker="1", identity="/usr/local/bin/sla-notifier", fail_action="", drained=True, tenant="qa-tenant", activate=False, ready=True, repeat=False, engine="temporal", paused="", scenario="", present=True, compatible=True, owned_error=False, binding="${PROJECT_BACKGROUND_BUILD_ID:-}", bootstrap=True):
        with tempfile.TemporaryDirectory(prefix="uip-project-control-test-") as temporary:
            root = Path(temporary).resolve()
            (root / "runtime").mkdir()
            image = "qa.invalid/project@sha256:" + "a" * 64
            (root / "runtime/project.env").write_text("OIDC_TENANT_ID=qa-tenant\nPLATFORM_ENVIRONMENT_CODE=qa\n")
            (root / "runtime/project.env").chmod(0o600)
            state = dict(protocol=1, engine=engine, tenant_id=tenant, environment="qa", build_id="project-background-sha256-" + "a" * 64,
                         bootstrap_ready=bootstrap, routing_ready=ready or paused == "PAUSED_OPERATOR", execution_ready=ready, paused_reason=paused, active_activities=0,
                         active_leases=0, drained=drained, schedules=[], runtime_config=dict(sla_interval=900000000000, dispatch_interval=15000000000, max_items=50, run_budget=240000000000, activity_timeout=55000000000, sla_activity_timeout=120000000000, workflow_run_timeout=360000000000))
            if scenario == "rollback-config-missing":
                state.pop("runtime_config")
            if scenario == "rollback-config-invalid":
                state["runtime_config"]["max_items"] = 5000
            (root / "control.json").write_text(json.dumps(state))
            (root / "compose.json").write_text(json.dumps({"services": {"project-sla-notifier": {
                "image": "${PROJECT_IMAGE:-pending}", "environment": {"PROJECT_BACKGROUND_BUILD_ID": binding, "TEMPORAL_ADDRESS": "temporal:7233", "TEMPORAL_NAMESPACE": "default"},
                "depends_on": {"temporal": {"condition": "service_healthy"}}, "healthcheck": {"test": ["CMD-SHELL", "wget http://127.0.0.1:9093/healthz"]}}}}))
            script = r'''
set -Eeuo pipefail
source "$TASK_SOURCE/project-background.sh"
deploy_dir="$TASK_ROOT"; release_file="$TASK_ROOT/.release.env"
env_get() {
  case "$2" in
    OIDC_TENANT_ID) printf 'qa-tenant\n' ;;
    PLATFORM_ENVIRONMENT_CODE) printf 'qa\n' ;;
    COMPOSE_PROJECT_NAME) printf 'qa-project\n' ;;
    PROJECT_BACKGROUND_BUILD_ID) printf 'project-background-sha256-%064d\n' 0 | tr 0 a ;;
    *) printf '%s\n' "$IMAGE" ;;
  esac
}
compose() {
  printf 'compose %s\n' "$*" >> "$TASK_ROOT/calls"
  case "$*" in
    'config --no-interpolate --format json')
      if [[ "$COMPATIBLE" == true ]]; then cat "$TASK_ROOT/compose.json"; else printf '{"services":{}}'; fi ;;
    'ps -q project-sla-notifier') [[ "$PRESENT" != true ]] || printf 'qa-owned-container\n' ;;
    'exec -T project-sla-notifier /usr/local/bin/project-background-control '*)
      local action=''; shift 4
      while (($#)); do
        if [[ "$1" == --action ]]; then action="$2"; shift 2; else shift; fi
      done
      cat "$TASK_ROOT/control.json"
      [[ "$action" != "$FAIL_ACTION" ]]
      ;;
    'stop --timeout 300 project-sla-notifier') return 0 ;;
    *) return 91 ;;
  esac
}
docker() {
  if [[ "$1" == ps ]]; then [[ "$OWNED_ERROR" != true ]] || return 76; [[ "$PRESENT" != true ]] || printf 'qa-owned-container\n'; return; fi
  case "$3" in
    '{{.Config.Image}}') printf '%s\n' "$IMAGE" ;;
    '{{index .Config.Labels "io.uip.project-background.protocol"}}') printf '%s\n' "$MARKER" ;;
    '{{.Path}}') printf '%s\n' "$IDENTITY" ;;
    *) return 92 ;;
  esac
}
if [[ "$SCENARIO" == installed-no-image ]]; then IMAGE=''; project_background_teardown; exit; fi
if [[ "$SCENARIO" == core-only ]]; then
  IMAGE=''; PRESENT=false; project_background_teardown; exit
fi
project_background_prepare
if [[ "$SCENARIO" == stop-start ]]; then
  project_background_teardown
  project_background_stopped
  PRESENT=false
  IMAGE="qa.invalid/project@sha256:$(printf '%064d' 0 | tr 0 b)"
  project_background_prepare
  project_background_prepare
elif [[ "$SCENARIO" == own-legacy-repeat ]]; then
  python3 -c 'import json,sys; p=sys.argv[1]; s=json.load(open(p)); s["paused_reason"]="PAUSED_OPERATION"; json.dump(s,open(p,"w"))' "$TASK_ROOT/control.json"
  project_background_prepare
elif [[ "$SCENARIO" == complete-missing ]]; then
  project_background_activate; PRESENT=false; project_background_teardown
elif [[ "$SCENARIO" == missing ]]; then
  PRESENT=false; project_background_prepare
elif [[ "$SCENARIO" == existing-rollback || "$SCENARIO" == rollback-activate-retry || "$SCENARIO" == rollback-config* ]]; then
  IMAGE="qa.invalid/project@sha256:$(printf '%064d' 0 | tr 0 b)"
  python3 -c 'import json,sys;p=sys.argv[1];s=json.load(open(p));s["build_id"]="project-background-sha256-"+"b"*64;json.dump(s,open(p,"w"))' "$TASK_ROOT/control.json"
  if [[ "$SCENARIO" == rollback-config* ]]; then
    printf 'OIDC_TENANT_ID=qa-tenant\nPLATFORM_ENVIRONMENT_CODE=qa\nSLA_SCAN_INTERVAL=10m\nPROJECT_BACKGROUND_MAX_ITEMS=3\nOIDC_CLIENT_SECRET=fixture-kept\nUNRELATED_SETTING=keep\n' >"$TASK_ROOT/runtime/project.env"
  fi
  if [[ "$SCENARIO" == rollback-config-missing || "$SCENARIO" == rollback-config-invalid ]]; then
    if project_background_rollback; then echo 'rollback accepted unverifiable old config' >&2; exit 95; fi
    [[ "$(_project_background_state get stage)" != rollback-drained ]] || exit 96
    grep -Fxq 'SLA_SCAN_INTERVAL=10m' "$TASK_ROOT/runtime/project.env" || exit 97
    exit 0
  fi
  project_background_rollback
  if [[ "$SCENARIO" == rollback-config ]]; then
    grep -Fxq 'SLA_SCAN_INTERVAL=900000000000ns' "$TASK_ROOT/runtime/project.env" || { echo 'old SLA config not restored before rebuild' >&2; exit 98; }
    grep -Fxq 'PROJECT_BACKGROUND_MAX_ITEMS=50' "$TASK_ROOT/runtime/project.env" || exit 99
    grep -Fxq 'OIDC_CLIENT_SECRET=fixture-kept' "$TASK_ROOT/runtime/project.env" || exit 100
    grep -Fxq 'UNRELATED_SETTING=keep' "$TASK_ROOT/runtime/project.env" || exit 101
  fi
  if [[ "$SCENARIO" == rollback-activate-retry ]]; then
    IMAGE="$(_project_background_state get old_image)"
    python3 -c 'import json,sys;p=sys.argv[1];s=json.load(open(p));s["build_id"]="project-background-sha256-"+"a"*64;json.dump(s,open(p,"w"))' "$TASK_ROOT/control.json"
    FAIL_ACTION=restore
    if project_background_activate; then exit 93; fi
    [[ "$(_project_background_state get stage)" == restoring ]] || exit 94
    FAIL_ACTION=''
    project_background_activate
  fi
elif [[ "$SCENARIO" == pointer-rollback ]]; then
  IMAGE="qa.invalid/project@sha256:$(printf '%064d' 0 | tr 0 b)"
  project_background_prepare
fi
if [[ "$REPEAT_PREPARE" == true ]]; then
  FAIL_ACTION=drain
  project_background_prepare
fi
if [[ "$SCENARIO" == first-failure-* ]]; then
  PRESENT=true
  if project_background_activate; then echo 'unexpected activation success' >&2; exit 81; fi
  [[ -z "$(_project_background_state get old_image)" ]] || { echo 'candidate became old runtime' >&2; exit 82; }
  operation_before="$(_project_background_state get operation_id)"
  if [[ "$SCENARIO" == first-failure-direct-release ]]; then
    previous_release="$TASK_ROOT/.release.env.previous"
    printf 'PROJECT_IMAGE=project-management/project:pending\n' >"$previous_release"
    printf 'PROJECT_IMAGE=%s\n' "$IMAGE" >"$release_file"
    service=project
    rollback_runtime() { printf 'unsafe rollback_runtime\n' >>"$TASK_ROOT/calls"; }
    refresh_subsystem_provisioner_config() { printf 'unsafe refresh\n' >>"$TASK_ROOT/calls"; }
    # Execute the actual direct-release failure tail, including its exit and mv propagation.
    awk '/^echo "发布失败，恢复上一镜像/ {inside=1} inside {print}' "$TASK_SOURCE/deploy-service.sh" >"$TASK_ROOT/failure-tail.sh"
    if (source "$TASK_ROOT/failure-tail.sh"); then exit 83; fi
    grep -Fxq "PROJECT_IMAGE=$IMAGE" "$release_file" || exit 84
    [[ -f "$previous_release" ]] || exit 85
  elif project_background_rollback; then
    echo 'rollback accepted without pre-operation runtime' >&2; exit 86
  fi
  [[ "$(_project_background_state get release_image)" == "$IMAGE" ]] || exit 87
  [[ "$(_project_background_state get operation_id)" == "$operation_before" ]] || exit 88
  [[ "$(_project_background_state get stage)" != rollback-drained ]] || exit 89
  python3 -c 'import json,sys;p=sys.argv[1];s=json.load(open(p));s.update(bootstrap_ready=True,routing_ready=True,execution_ready=True,paused_reason="");json.dump(s,open(p,"w"))' "$TASK_ROOT/control.json"
  # Resume through prepare too: it must not relabel the same candidate as old.
  FAIL_ACTION=''
  project_background_prepare
  [[ -z "$(_project_background_state get old_image)" ]] || exit 90
  project_background_activate
fi
if [[ "$SCENARIO" == first-install ]]; then PRESENT=true; project_background_activate; fi
if [[ "$CALL_ACTIVATE" == true ]]; then project_background_activate; fi
'''
            result = subprocess.run(
                ["/bin/bash", "-c", script], text=True, capture_output=True, check=False, timeout=15,
                env={"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "TASK_SOURCE": str(SOURCE), "TASK_ROOT": str(root),
                     "IMAGE": image, "MARKER": marker, "IDENTITY": identity, "FAIL_ACTION": fail_action, "CALL_ACTIVATE": str(activate).lower(), "REPEAT_PREPARE": str(repeat).lower(), "SCENARIO": scenario, "PRESENT": str(present).lower(), "COMPATIBLE": str(compatible).lower(), "OWNED_ERROR": str(owned_error).lower()},
            )
            calls = (root / "calls").read_text().splitlines() if (root / "calls").exists() else []
            operation = root / "runtime/project-background/operation.json"
            return result, calls, json.loads(operation.read_text()) if operation.exists() else None

    def test_controlled_stop_then_new_release_preserves_prior_operation(self):
        result, calls, state = self.run_prepare(scenario="stop-start")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "stopped")
        self.assertEqual(state["old_build_id"], "project-background-sha256-" + "a" * 64)
        self.assertEqual(state["prior"]["engine"], "temporal")
        self.assertEqual(state["release_image"], "qa.invalid/project@sha256:" + "b" * 64)

    def test_unexpected_missing_consumer_without_stop_proof_fails(self):
        result, calls, state = self.run_prepare(scenario="missing")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["stage"], "prepared")

    def test_previously_completed_consumer_disappearance_is_not_core_only(self):
        result, calls, state = self.run_prepare(scenario="complete-missing")
        self.assertNotEqual(result.returncode, 0)

    def test_core_only_teardown_needs_no_project_image(self):
        result, calls, state = self.run_prepare(scenario="core-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIsNone(state)

    def test_protocol_legacy_manual_pause_blocks_before_replacement(self):
        result, calls, state = self.run_prepare(engine="legacy", paused="PAUSED_OPERATOR")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["engine"], "legacy")
        self.assertFalse(any("--action pause " in call for call in calls))
        self.assertIn("受控", result.stderr)

    def test_protocol_legacy_pending_operation_blocks_replacement(self):
        result, calls, state = self.run_prepare(engine="legacy", paused="PAUSED_OPERATION")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("--action pause " in call for call in calls))

    def test_legacy_own_pending_pause_can_retry_with_fresh_drain(self):
        result, calls, state = self.run_prepare(engine="legacy", scenario="own-legacy-repeat")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sum("--action drain " in call for call in calls), 2)
        self.assertEqual(state["prior"]["paused_reason"], "")

    def test_temporal_operator_pause_survives_activation_metadata(self):
        result, calls, state = self.run_prepare(paused="PAUSED_OPERATOR", activate=True, ready=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "complete")
        self.assertEqual(state["prior"]["paused_reason"], "PAUSED_OPERATOR")

    def test_owned_consumer_without_project_image_cannot_skip_guard(self):
        result, calls, state = self.run_prepare(scenario="installed-no-image")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("--action pause " in call for call in calls))

    def test_owned_inventory_error_never_means_no_project(self):
        result, calls, state = self.run_prepare(scenario="core-only", owned_error=True)
        self.assertNotEqual(result.returncode, 0)

    def test_actual_legacy_engine_is_saved(self):
        result, calls, state = self.run_prepare(engine="legacy")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["engine"], "legacy")

    def test_first_install_failures_cannot_rollback_candidate_and_can_resume(self):
        cases = [(True, "", False, "PAUSED_LICENSE"), (False, "", True, ""), (True, "verify", True, "")]
        for bootstrap, failed_action, ready, paused in cases:
            with self.subTest(bootstrap=bootstrap, failed_action=failed_action, paused=paused):
                result, calls, state = self.run_prepare(present=False, bootstrap=bootstrap, ready=ready, paused=paused, fail_action=failed_action, scenario="first-failure-rollback")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(state["old_image"], "")
                self.assertEqual(state["old_build_id"], "")
                self.assertIsNone(state["prior"])
                self.assertEqual(state["stage"], "complete")
                self.assertEqual(state["current_image"], "qa.invalid/project@sha256:" + "a" * 64)
                self.assertEqual(state["current_engine"], "temporal")

    def test_real_old_runtime_keeps_metadata_and_permits_drained_rollback(self):
        result, calls, state = self.run_prepare(scenario="existing-rollback", paused="PAUSED_OPERATOR")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "rollback-drained")
        self.assertEqual(state["old_image"], "qa.invalid/project@sha256:" + "a" * 64)
        self.assertEqual(state["release_image"], state["old_image"])
        self.assertEqual(state["old_build_id"], "project-background-sha256-" + "a" * 64)
        self.assertEqual(state["prior"]["paused_reason"], "PAUSED_OPERATOR")
        self.assertEqual(sum("--action drain " in call for call in calls), 2)

    def test_restore_intent_survives_failed_activation_retry(self):
        result, calls, state = self.run_prepare(scenario="rollback-activate-retry", paused="PAUSED_OPERATOR", ready=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "complete")
        self.assertEqual(sum("--action restore " in call for call in calls), 2)
        self.assertFalse(any("--action ensure " in call for call in calls))

    def test_cold_rollback_restores_only_original_runtime_tuning(self):
        result, calls, state = self.run_prepare(scenario="rollback-config")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "rollback-drained")

    def test_cold_rollback_without_verified_old_config_keeps_candidate(self):
        for scenario in ("rollback-config-missing", "rollback-config-invalid"):
            with self.subTest(scenario=scenario):
                result, calls, state = self.run_prepare(scenario=scenario)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotEqual(state["stage"], "rollback-drained")

    def test_direct_release_first_failure_preserves_release_candidate_and_operation(self):
        result, calls, state = self.run_prepare(present=False, ready=False, paused="PAUSED_LICENSE", scenario="first-failure-direct-release")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "complete")
        self.assertEqual(state["old_image"], "")
        self.assertFalse(any("unsafe " in call for call in calls))

    def test_first_install_bootstrap_before_promote(self):
        result, calls, state = self.run_prepare(present=False, scenario="first-install")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "complete")
        actions = [call.split("--action ")[1].split()[0] for call in calls if "--action " in call]
        self.assertEqual(actions, ["inspect", "ensure", "resume", "verify"])

    def test_resume_does_not_guess_after_pointer_rollback(self):
        result, calls, state = self.run_prepare(scenario="pointer-rollback")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(sum("--action drain " in call for call in calls), 1)
        self.assertEqual(state["release_image"], "qa.invalid/project@sha256:" + "a" * 64)

    def test_unbootstrapped_candidate_cannot_be_promoted(self):
        result, calls, state = self.run_prepare(activate=True, bootstrap=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("--action ensure " in call for call in calls))
        self.assertNotEqual(state["stage"], "complete")

    def test_expired_license_bootstraps_but_is_not_business_ready(self):
        result, calls, state = self.run_prepare(activate=True, ready=False, paused="PAUSED_LICENSE")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["stage"], "activating")
        self.assertTrue(state["operation_id"])

    def test_compose_must_bind_exact_release_build(self):
        result, calls, state = self.run_prepare(binding="${PROJECT_BACKGROUND_BUILD_ID_WRONG:-}")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("--action pause " in call for call in calls))

    def test_old_custom_compose_blocks_before_pause(self):
        result, calls, state = self.run_prepare(compatible=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("--action pause " in call for call in calls))
        self.assertIn(".dist", result.stderr)

    def test_unknown_missing_control_tool_is_not_legacy(self):
        result, calls, state = self.run_prepare(fail_action="inspect")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call.startswith("compose stop") for call in calls))
        self.assertEqual(state["stage"], "inspect")

    def test_foreign_tenant_control_response_cannot_pause_or_replace_worker(self):
        result, calls, state = self.run_prepare(tenant="another-tenant")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("--action pause " in call for call in calls))

    def test_drain_failure_keeps_old_worker_and_operation(self):
        result, calls, state = self.run_prepare(fail_action="drain")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["stage"], "drain-pending")
        self.assertFalse(any(call.startswith("compose stop") for call in calls))

    def test_resume_rechecks_current_drain_instead_of_trusting_saved_phase(self):
        result, calls, state = self.run_prepare(repeat=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["stage"], "drain-pending")
        self.assertEqual(sum("--action drain " in call for call in calls), 2)

    def test_zero_exit_with_incomplete_drain_evidence_is_rejected(self):
        result, calls, state = self.run_prepare(drained=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["stage"], "drain-pending")

    def test_activation_zero_exit_without_readiness_does_not_complete(self):
        result, calls, state = self.run_prepare(activate=True, ready=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["stage"], "activating")

    def test_only_known_legacy_entrypoint_can_be_stopped_without_protocol(self):
        result, calls, state = self.run_prepare(marker="", identity="unknown-worker")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call.startswith("compose stop") for call in calls))
        result, calls, state = self.run_prepare(marker="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["stage"], "stopped")
        self.assertIn("compose stop --timeout 300 project-sla-notifier", calls)


if __name__ == "__main__":
    unittest.main()
