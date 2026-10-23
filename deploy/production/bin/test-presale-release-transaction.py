#!/usr/bin/env python3
"""验证离线 CRM 镜像与 Temporal 身份的提交，不连接 Docker 守护进程。"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


BIN = Path(__file__).resolve().parent
HARNESS = r'''
set -Eeuo pipefail
source "$DEPLOY_SOURCE"
deploy_dir="$FIXTURE_ROOT"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
mkdir -p "$deploy_dir/runtime" "$deploy_dir/subsystems.d" "$manifest_dir"
printf 'COMPOSE_PROJECT_NAME=presale-release-fixture\n' > "$runtime_file"
printf 'CUSTOMER_CRM_IMAGE=%s\nCUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID=customer-presale-aaaaaaaaaaaaaaaa\nUNRELATED=fixture-value\n' "$IMAGE_A" > "$release_file"
chmod 600 "$runtime_file" "$release_file"
cp "$release_file" "$FIXTURE_ROOT/previous"
touch "$deploy_dir/subsystems.d/customer_and_opportunity-prod.yaml"
fixture_record="$manifest_dir/customer-opportunity-fixture.imported"
printf 'COMPONENT=customer-opportunity\nVERSION=fixture\nIMAGE_REF=%s\n' "$IMAGE_B" > "$fixture_record"
require_initialized() { :; }
scope_require() { :; }
latest_record() { printf '%s\n' "$fixture_record"; }
subsystem_phase() {
  if [[ -f "$FIXTURE_ROOT/candidate" ]]; then printf 'WAITING_FOR_PLATFORM_ADOPTION\n'; else printf 'IMPORTED\n'; fi
}
flock() { printf 'lock %s\n' "$*" >> "$TRACE"; }
id() { if [[ "${1:-}" == -u ]]; then printf '501\n'; else command id "$@"; fi; }
refresh_subsystem_provisioner() {
  local count=0
  [[ ! -f "$FIXTURE_ROOT/refresh-count" ]] || read -r count < "$FIXTURE_ROOT/refresh-count"
  printf '%s\n' "$((count+1))" > "$FIXTURE_ROOT/refresh-count"
  local image build
  image="$(env_get "$release_file" CUSTOMER_CRM_IMAGE)"
  build="$(env_get "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID)"
  [[ "$build" == "$(customer_presale_build_id "$image")" ]] || { echo 'half-committed tuple' >&2; exit 43; }
  printf 'refresh %s %s\n' "$image" "$build" >> "$TRACE"
  if [[ ( "$SCENARIO" == refresh-fails || "$SCENARIO" == restore-fails ) && "$count" == 0 || "$SCENARIO" == refresh-always-fails ]]; then
    die 'fixture refresh failed'
  fi
}
docker() {
  [[ "$1" == create ]] || { echo "unexpected Docker call: $*" >&2; return 96; }
  [[ " $* " == *" $IMAGE_B "* ]] || return 97
  [[ "$(env_get "$release_file" CUSTOMER_CRM_IMAGE)" == "$IMAGE_B" ]]
  [[ "$(env_get "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID)" == customer-presale-bbbbbbbbbbbbbbbb ]]
  [[ "$SCENARIO" != candidate-fails ]] || return 44
  touch "$FIXTURE_ROOT/candidate"
  printf 'candidate-create\n' >> "$TRACE"
}
assert_old_tuple() {
  cmp "$release_file" "$FIXTURE_ROOT/previous"
  [[ ! -e "$FIXTURE_ROOT/candidate" ]]
  [[ ! -e "$deploy_dir/runtime/.release-next." && ! -e "$deploy_dir/runtime/.release-previous." ]]
  [[ -z "$(find "$deploy_dir/runtime" -name '.release-next.*' -o -name '.release-previous.*')" ]]
  grep -q '^lock -u 7$' "$TRACE"
}
[[ "$(customer_presale_build_id "$IMAGE_A")" == customer-presale-aaaaaaaaaaaaaaaa ]]
[[ "$(customer_presale_build_id "$IMAGE_B")" == customer-presale-bbbbbbbbbbbbbbbb ]]
case "$SCENARIO" in
  prepare)
    prepare_subsystem customer-opportunity
    [[ -f "$FIXTURE_ROOT/candidate" ]]
    [[ "$(env_get "$release_file" CUSTOMER_CRM_IMAGE)" == "$IMAGE_B" ]]
    [[ "$(env_get "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID)" == customer-presale-bbbbbbbbbbbbbbbb ]]
    prepare_subsystem customer-opportunity
    [[ "$(cat "$FIXTURE_ROOT/refresh-count")" == 1 ]]
    ;;
  stage|repeat)
    stage_upgrade customer-opportunity
    [[ "$(env_get "$release_file" CUSTOMER_CRM_IMAGE)" == "$IMAGE_B" ]]
    [[ "$(env_get "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID)" == customer-presale-bbbbbbbbbbbbbbbb ]]
    [[ ! -e "$FIXTURE_ROOT/candidate" ]]
    if [[ "$SCENARIO" == repeat ]]; then
      cp "$release_file" "$FIXTURE_ROOT/committed"
      stage_upgrade customer-opportunity
      cmp "$release_file" "$FIXTURE_ROOT/committed"
    fi
    ;;
  refresh-fails|refresh-always-fails)
    if stage_upgrade customer-opportunity; then echo 'failed refresh accepted' >&2; exit 1; fi
    assert_old_tuple
    [[ "$(cat "$FIXTURE_ROOT/refresh-count")" == 2 ]]
    ;;
  candidate-fails)
    if prepare_subsystem customer-opportunity; then echo 'failed candidate accepted' >&2; exit 1; fi
    assert_old_tuple
    [[ "$(cat "$FIXTURE_ROOT/refresh-count")" == 2 ]]
    ;;
  restore-fails)
    mv() {
      if [[ "$*" == *'.release-previous.'* ]]; then return 46; fi
      command mv "$@"
    }
    if stage_upgrade customer-opportunity; then echo 'failed restoration accepted' >&2; exit 1; fi
    backup="$(find "$deploy_dir/runtime" -name '.release-previous.*')"
    [[ -n "$backup" ]]
    cmp "$backup" "$FIXTURE_ROOT/previous"
    [[ "$(env_get "$release_file" CUSTOMER_CRM_IMAGE)" == "$IMAGE_B" ]]
    [[ "$(env_get "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID)" == customer-presale-bbbbbbbbbbbbbbbb ]]
    [[ "$(cat "$FIXTURE_ROOT/refresh-count")" == 1 ]]
    rm "$backup"
    ;;
  write-fails)
    eval "$(declare -f env_set | sed '1s/env_set/original_env_set/')"
    env_set() { [[ "$2" != CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID ]] || return 45; original_env_set "$@"; }
    if stage_upgrade customer-opportunity; then echo 'failed Build ID write accepted' >&2; exit 1; fi
    assert_old_tuple
    [[ ! -f "$FIXTURE_ROOT/refresh-count" ]]
    ;;
  mutable)
    printf 'COMPONENT=customer-opportunity\nVERSION=fixture\nIMAGE_REF=registry.invalid/crm:latest\n' > "$fixture_record"
    if stage_upgrade customer-opportunity; then echo 'mutable image accepted' >&2; exit 1; fi
    assert_old_tuple
    [[ ! -f "$FIXTURE_ROOT/refresh-count" ]]
    if customer_presale_build_id registry.invalid/crm:latest; then exit 1; fi
    ;;
  *) exit 99 ;;
esac
[[ "$(env_get "$release_file" UNRELATED)" == fixture-value ]]
[[ -z "$(find "$deploy_dir/runtime" -name '.release-next.*' -o -name '.release-previous.*')" ]]
'''


class PresaleReleaseTransactionTests(unittest.TestCase):
    def run_scenario(self, scenario):
        with tempfile.TemporaryDirectory(prefix="presale-release-") as directory:
            root = Path(directory).resolve()
            trace = root / "trace"
            trace.touch()
            env = dict(os.environ, DEPLOY_SOURCE=str(BIN / "deploy.sh"),
                       FIXTURE_ROOT=str(root), TRACE=str(trace), SCENARIO=scenario,
                       IMAGE_A="127.0.0.1:5000/uip/customer-opportunity@sha256:" + "a" * 64,
                       IMAGE_B="127.0.0.1:5000/uip/customer-opportunity@sha256:" + "b" * 64)
            result = subprocess.run(["bash", "-c", HARNESS], env=env,
                                    capture_output=True, text=True, timeout=20)
            transcript = result.stdout + result.stderr + "\n--- trace ---\n" + trace.read_text()
            if os.environ.get("QA_PRESALE_OUTPUT"):
                output = Path(os.environ["QA_PRESALE_OUTPUT"])
                output.mkdir(parents=True, exist_ok=True)
                (output / (scenario + ".log")).write_text(transcript)
            self.assertEqual(result.returncode, 0, transcript)
            return transcript

    def test_first_prepare_commits_pair_before_candidate_and_retry_is_idempotent(self):
        self.assertIn("candidate-create", self.run_scenario("prepare"))

    def test_stage_changes_build_id_with_image(self):
        self.run_scenario("stage")

    def test_same_image_retry_keeps_same_build_identity(self):
        self.run_scenario("repeat")

    def test_refresh_failure_restores_pair_and_rebinds_previous(self):
        self.run_scenario("refresh-fails")

    def test_candidate_failure_restores_pair(self):
        self.run_scenario("candidate-fails")

    def test_failed_restore_preserves_private_previous_release_for_recovery(self):
        self.assertIn("旧指针备份保留", self.run_scenario("restore-fails"))

    def test_failed_rebind_still_restores_pair_and_cleans_temporaries(self):
        self.assertIn("控制面重新绑定仍需重试", self.run_scenario("refresh-always-fails"))

    def test_build_id_write_failure_does_not_publish_half_a_release(self):
        self.run_scenario("write-fails")

    def test_mutable_image_cannot_be_staged(self):
        self.run_scenario("mutable")


if __name__ == "__main__":
    unittest.main()
