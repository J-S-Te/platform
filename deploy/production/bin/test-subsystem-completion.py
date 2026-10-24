#!/usr/bin/env python3
"""使用完全隔离的 Docker 桩验证实际阶段、继续执行、成员及镜像门禁。"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


BIN = Path(__file__).resolve().parent
PRODUCTION = BIN.parent
HARNESS = r'''
set -Eeuo pipefail
source "$DEPLOY_SOURCE"
deploy_dir="$FIXTURE_ROOT"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
profiles_dir="$deploy_dir/subsystems.d"
compose_file="$deploy_dir/docker-compose.yml"
mkdir -p "$manifest_dir" "$deploy_dir/runtime" "$profiles_dir"
cp "$PROFILE_SOURCE" "$profiles_dir/contract_management-prod.yaml"
printf 'COMPOSE_PROJECT_NAME=completion-fixture\n' > "$runtime_file"
printf 'CONTRACT_IMAGE=%s\n' "$TARGET_IMAGE" > "$release_file"
printf 'COMPONENT=contract\nVERSION=fixture\nIMAGE_REF=%s\n' "$TARGET_IMAGE" > "$manifest_dir/contract-fixture.imported"
printf 'fixture\n' > "$compose_file"
touch "$deploy_dir/candidate-present"
require_initialized() { :; }
runtime_ready() { :; }
flock() { :; }
sleep() { :; }
scope_services() { printf '%s\n' contract-api contract-worker contract-migrate contract-mysql temporal; }
compose() {
  if [[ "$1" == ps && "$2" == -q ]]; then
    [[ "$3" != contract-worker || "$SCENARIO" != missing-worker ]] || return 0
    printf 'fixture-%s\n' "$3"
  elif [[ "$*" == 'config --format json' ]]; then
    local reference="$TARGET_IMAGE"
    [[ "$SCENARIO" != mutable-image ]] || reference=registry.invalid/contract:latest
    printf '{"services":{"contract-api":{"image":"%s"},"contract-worker":{"image":"%s"}}}\n' "$reference" "$reference"
  elif [[ "$*" == 'run -T --rm --no-deps platform-api ./license-install verify-deployment --application contract_management --timeout 60s' ]]; then
    printf 'managed-status scenario=%s\n' "$SCENARIO" >> "$TRACE"
    [[ "$SCENARIO" != platform-failed && "$SCENARIO" != stale-manifest ]]
  else
    printf 'unexpected Compose call: %s\n' "$*" >&2
    return 98
  fi
}
docker() {
  if [[ "$1" == container && "$2" == inspect ]]; then
    [[ -e "$deploy_dir/candidate-present" ]] || return 1
    if [[ "${3:-}" == -f ]]; then
      case "$4" in
        *application_code*) printf 'contract_management\n' ;;
        *environment*) printf 'prod\n' ;;
        *) return 97 ;;
      esac
    fi
    return 0
  fi
  if [[ "$1" == image && "$2" == inspect ]]; then
    printf 'image-identity %s\n' "$3" >> "$TRACE"
    if [[ "$3" == "$OLD_IMAGE_ID" ]]; then printf '%s\n' "$OLD_IMAGE_ID";
    else printf '%s\n' "$TARGET_IMAGE_ID"; fi
    return 0
  fi
  if [[ "$1" == inspect ]]; then
    local id format count=0
    if [[ "$2" == -f ]]; then format="$3"; id="$4"; else id="$2"; format="$4"; fi
    case "$format" in
      *State.Running*) printf 'true\n' ;;
      *State.Status*)
        if [[ "$SCENARIO" == exited-worker && "$id" == fixture-contract-worker ]]; then printf 'exited\n';
        else printf 'running\n'; fi
        ;;
      *State.Health*)
        if [[ "$SCENARIO" == restarting-worker && "$id" == fixture-contract-worker ]]; then printf 'none\n';
        else printf 'healthy\n'; fi
        ;;
      *RestartCount*)
        if [[ "$SCENARIO" == restarting-worker && "$id" == fixture-contract-worker ]]; then
          [[ ! -e "$deploy_dir/restart-counter" ]] || read -r count < "$deploy_dir/restart-counter"
          printf '%s\n' "$((count + 1))" > "$deploy_dir/restart-counter"
          printf '%s\n' "$count"
        else printf '0\n'; fi
        ;;
      *'.Image'*)
        if [[ "$SCENARIO" == old-worker-image && "$id" == fixture-contract-worker ]]; then printf '%s\n' "$OLD_IMAGE_ID";
        else printf '%s\n' "$TARGET_IMAGE_ID"; fi
        ;;
      *) printf 'unexpected inspect format: %s\n' "$format" >&2; return 96 ;;
    esac
    return 0
  fi
  if [[ "$1" == rm && "$2" == uip-contract-candidate ]]; then
    printf 'candidate-remove\n' >> "$TRACE"
    rm "$deploy_dir/candidate-present"
    return 0
  fi
  printf 'unexpected Docker call: %s\n' "$*" >&2
  return 95
}
UIP_VERIFY_STABILITY_SECONDS=1
if [[ "$SCENARIO" == healthy ]]; then
  [[ "$(subsystem_phase contract)" == READY_TO_CONTINUE ]]
  continue_subsystem contract
  [[ ! -e "$deploy_dir/candidate-present" ]]
  [[ "$(subsystem_phase contract)" == VERIFIED ]]
else
  if verify_selected_runtime contract > "$FIXTURE_ROOT/member-failure.log" 2>&1; then
    echo 'incomplete runtime unexpectedly verified' >&2; exit 11
  fi
  cat "$FIXTURE_ROOT/member-failure.log"
  [[ "$(subsystem_phase contract)" == PROVISIONING_OR_FAILED ]]
  if continue_subsystem contract; then echo 'incomplete runtime deleted candidate' >&2; exit 12; fi
  [[ -e "$deploy_dir/candidate-present" ]]
  if [[ "$SCENARIO" == mutable-image ]]; then
    rm "$deploy_dir/candidate-present"
    if verify_subsystem_current_health contract > "$FIXTURE_ROOT/manual-verify.log" 2>&1; then
      echo 'manual verify accepted mutable runtime image' >&2; exit 13
    fi
    grep -q '未绑定不可变发行镜像' "$FIXTURE_ROOT/manual-verify.log"
    cat "$FIXTURE_ROOT/manual-verify.log"
  fi
fi
'''


class CompletionGateTests(unittest.TestCase):
    def run_scenario(self, scenario, expected=None):
        with tempfile.TemporaryDirectory(prefix="subsystem-completion-") as directory:
            fixture = Path(directory).resolve()
            trace = fixture / "trace"
            trace.write_text("")
            env = dict(os.environ, DEPLOY_SOURCE=str(BIN / "deploy.sh"),
                       PROFILE_SOURCE=str(PRODUCTION / "subsystems.d/contract_management-prod.yaml"),
                       FIXTURE_ROOT=str(fixture), TRACE=str(trace), SCENARIO=scenario,
                       TARGET_IMAGE="127.0.0.1:5000/uip/contract@sha256:" + "a" * 64,
                       TARGET_IMAGE_ID="sha256:" + "b" * 64, OLD_IMAGE_ID="sha256:" + "c" * 64)
            result = subprocess.run(["bash", "-c", HARNESS], env=env, capture_output=True, text=True, timeout=20)
            transcript = result.stdout + result.stderr + "\n--- mock trace ---\n" + trace.read_text()
            output = os.environ.get("QA_RUNTIME_OUTPUT")
            if output:
                destination = Path(output)
                destination.mkdir(parents=True, exist_ok=True)
                (destination / (scenario + ".log")).write_text(transcript)
            self.assertEqual(result.returncode, 0, transcript)
            if expected:
                self.assertIn(expected, transcript)
            if scenario == "healthy":
                self.assertIn("candidate-remove", trace.read_text())
                self.assertIn("managed-status", trace.read_text())
            else:
                self.assertNotIn("candidate-remove", trace.read_text())
            return trace.read_text()

    def test_complete_current_runtime_can_finish_idempotently(self):
        self.run_scenario("healthy", "VERIFIED")

    def test_missing_worker_keeps_candidate(self):
        self.run_scenario("missing-worker", "service=contract-worker container=missing")

    def test_old_worker_native_image_keeps_candidate(self):
        self.run_scenario("old-worker-image", "实际镜像与当前审核编排不一致")

    def test_exited_worker_keeps_candidate(self):
        self.run_scenario("exited-worker", "status=exited")

    def test_worker_restart_during_stability_keeps_candidate(self):
        self.run_scenario("restarting-worker", "restarts=0->1")

    def test_failed_managed_deployment_keeps_candidate(self):
        self.assertIn("managed-status", self.run_scenario("platform-failed", "平台部署状态或清单核验尚未完成"))

    def test_stale_manifest_status_keeps_candidate(self):
        self.assertIn("managed-status", self.run_scenario("stale-manifest", "平台部署状态或清单核验尚未完成"))

    def test_mutable_runtime_image_is_not_a_completed_release(self):
        self.run_scenario("mutable-image", "未绑定不可变发行镜像")


if __name__ == "__main__":
    unittest.main()
