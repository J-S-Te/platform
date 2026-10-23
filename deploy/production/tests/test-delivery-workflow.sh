#!/usr/bin/env bash
set -Eeuo pipefail
# 生产入口及其固定组件映射要求 GNU Bash 4 或更高版本。
((BASH_VERSINFO[0] >= 4)) || { echo 'test-delivery-workflow.sh requires GNU Bash 4+ (use the isolated Linux QA runner)' >&2; exit 1; }
test_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec python3 - "$test_dir" "$@" <<'PY'
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(sys.argv[1]).resolve().parents[0]
BIN = ROOT / 'bin'
WORKSPACE = ROOT.parents[2]
WRAPPER = WORKSPACE / '服务器一键离线部署.sh'
SOURCE = WRAPPER.read_text()
FINISH = 'workflow_finish_controlled() {' + SOURCE.split('workflow_finish_controlled() {', 1)[1].split('\n}', 1)[0] + '\n}'
APPLICATION_OF = 'subsystem_application_of() {' + SOURCE.split('subsystem_application_of() {', 1)[1].split('\n}', 1)[0] + '\n}'
REF = '127.0.0.1:18250/uip/contract@sha256:' + 'a' * 64


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='delivery-workflow-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.target = self.root / 'deployment'
        self.media = self.root / 'media'
        self.proof = self.root / 'proof'
        self.tools = self.root / 'tools'
        self.events = self.root / 'events'
        for path in (self.target / 'bin', self.target / 'runtime', self.target / 'manifests', self.media, self.proof, self.tools):
            path.mkdir(parents=True)
        self.packages = {
            'common': 'common-infrastructure-linux-amd64.tar.gz',
            'platform': 'platform-backend-fixture-v1-linux-amd64.tar.gz',
            'frontend': 'frontend-fixture-v1-linux-amd64.tar.gz',
            'contract': 'contract-backend-fixture-v1-linux-amd64.tar.gz',
        }
        for filename in self.packages.values():
            (self.media / filename).write_text('transport boundary fixture\n')
        (self.media / 'SHA256SUMS').write_text('bound fixture identity\n')
        (self.target / '.env').write_text('PUBLIC_ACCESS_MODE=ip\nPUBLIC_PLATFORM_HOST=192.0.2.10\nPUBLIC_SSO_HOST=192.0.2.10\nPUBLIC_HTTP_PORT=8081\nKEYCLOAK_HTTP_PORT=18090\n')
        (self.target / '.release.env').write_text('CONTRACT_IMAGE=' + REF + '\n')
        (self.target / '.env').chmod(0o600)
        (self.target / '.release.env').chmod(0o600)
        (self.target / 'docker-compose.yml').write_text('services: {}\n')
        (self.root / 'enabled').write_text('platform-api\nfrontend\ncontract-api\n')
        (self.root / 'phase').write_text('IMPORTED\n')
        (self.tools / 'docker').write_text('''#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "$1 ${2:-} ${3:-}" == 'container inspect uip-contract-candidate' ]]; then
  [[ -f "$FLOW_ROOT/candidate.json" ]] || exit 1
  if [[ "${4:-}" == --format ]]; then cat "$FLOW_ROOT/candidate.json"; fi
  exit 0
fi
printf 'unexpected-docker:%s\\n' "$*" >> "$FLOW_EVENTS"
exit 98
''')
        (self.tools / 'docker').chmod(0o755)
        (self.target / 'bin/deploy.sh').write_text('''#!/usr/bin/env bash
set -Eeuo pipefail
printf 'child:%s\\n' "$*" >> "$FLOW_EVENTS"
case "$1" in
  delivery-enabled)
    [[ "${2:-}" == contract ]] || exit 99
    if grep -Fxq contract-api "$FLOW_ROOT/enabled"; then printf 'ENABLED\\n'; else printf 'DISABLED\\n'; fi ;;
  delivery-proof) [[ -f "$FLOW_ROOT/proof/contract.runtime" ]] ;;
  delivery-final-proof) [[ -f "$FLOW_ROOT/proof/contract.runtime" && ! -f "$FLOW_ROOT/candidate.json" ]] ;;
  delivery-license-proof)
    [[ -f "$FLOW_ROOT/proof/license.enforced" ]]
    printf '{"status":"ENFORCED","applications":["contract_management"]}\\n' ;;
  *) exit 99 ;;
esac
''')
        (self.target / 'bin/deploy.sh').chmod(0o755)
        self.write_candidate_template()

    def write_candidate_template(self):
        self.candidate = {'Name': 'uip-contract-candidate', 'Image': 'sha256:' + 'a' * 64,
                          'Config': {'Image': REF, 'Labels': {
                              'com.basic-platform.discovery': 'v1', 'com.basic-platform.application_code': 'contract_management',
                              'com.basic-platform.environment': 'prod', 'com.basic-platform.service_name': 'contract-api',
                              'com.basic-platform.service_role': 'business', 'com.basic-platform.version': 'fixture-v1'}}}
        (self.root / 'candidate-template.json').write_text(json.dumps(self.candidate))

    def run_flow(self, resume=False, fail_action='', success=True, license_file=''):
        assignments = {'deploy_dir': self.target, 'target_dir': self.target, 'runtime_file': self.target / '.env',
                       'release_file': self.target / '.release.env', 'compose_file': self.target / 'docker-compose.yml',
                       'manifest_dir': self.target / 'manifests', 'package_dir': self.media, 'media_dir': self.media,
                       'delivery_session_tool': BIN / 'deployment-session.py', 'script_path': WRAPPER,
                       'deployment_mode': 'install', 'skip_host_deps': 'true', 'skip_configure': 'false',
                       'apply_compose': 'false', 'legacy_license_mode': '', 'license_customer': '', 'license_file_arg': license_file,
                       'resume_installation': 'true' if resume else 'false', 'FAIL_ACTION': fail_action,
                       'workflow_stages': 'common-import,platform-import,platform,frontend-import,frontend,license-prepare,contract-import,contract-prepare,license,verify,controlled'}
        script = 'set -Eeuo pipefail\nsource ' + shlex.quote(str(BIN / 'deploy.sh')) + '\n'
        script += ''.join(key + '=' + shlex.quote(str(value)) + '\n' for key, value in assignments.items())
        script += 'script_dir=' + shlex.quote(str(WORKSPACE)) + '\n'
        script += 'workflow_components=(common platform frontend contract)\ndeclare -A package_components=()\n'
        script += ''.join('package_components[' + shlex.quote(key) + ']=' + shlex.quote(value) + '\n' for key, value in self.packages.items())
        script += r'''
scope_services() { cat "$FLOW_ROOT/enabled"; }
scope_report() { :; }
require_initialized() { [[ -f "$runtime_file" && -f "$release_file" ]]; }
event() { printf '%s\n' "$*" >> "$FLOW_EVENTS"; }
component_of_archive() {
  case "${1##*/}" in
    common-infrastructure-*) printf common ;;
    platform-backend-*) printf platform ;;
    frontend-*) printf frontend ;;
    contract-backend-*) printf contract ;;
    *) return 91 ;;
  esac
}
import_package() {
  local component; component="$(component_of_archive "$1")" || return 1
  event "import:$component"
  [[ "$FAIL_ACTION" != "import-$component" ]] || return 7
  printf '%s' "${1##*/}" > "$FLOW_ROOT/proof/import-$component"
  touch "$FLOW_ROOT/proof/registry-$component"
}
delivery_import_component() { import_package "$2"; }
delivery_import_matches() {
  event "probe-import:$1"
  [[ -f "$FLOW_ROOT/proof/import-$1" && -f "$FLOW_ROOT/proof/registry-$1" ]] &&
    [[ "$(cat "$FLOW_ROOT/proof/import-$1")" == "${2##*/}" ]]
}
deploy_component() {
  local archive
  event "deploy:$1"
  [[ "$FAIL_ACTION" != "$1" ]] || return 8
  archive="${package_components[$1]}"
  printf '%s' "$archive" > "$FLOW_ROOT/proof/release-$1"
  touch "$FLOW_ROOT/proof/worker-$1"
}
delivery_deploy_component() { deploy_component "$1"; }
delivery_release_matches() {
  event "probe-release:$1"
  if [[ "$1" == contract ]]; then [[ -f "$FLOW_ROOT/proof/contract.runtime" ]]; return; fi
  [[ -f "$FLOW_ROOT/proof/release-$1" && -f "$FLOW_ROOT/proof/worker-$1" ]] &&
    [[ "$(cat "$FLOW_ROOT/proof/release-$1")" == "${2##*/}" ]]
}
delivery_import_facts() {
  [[ "$1" == contract && -f "$FLOW_ROOT/proof/import-contract" ]] || return 94
  printf '{"image_ref":"%s","version":"fixture-v1","acceptable_ids":{"%s":["sha256:%s"]}}\n' "$FLOW_REF" "$FLOW_REF" "$(printf '%064d' 0 | tr 0 a)"
}
prepare_subsystem() {
  event "prepare:$1"
  [[ ! -f "$FLOW_ROOT/candidate.json" ]] || return 95
  cp "$FLOW_ROOT/candidate-template.json" "$FLOW_ROOT/candidate.json"
}
subsystem_phase() { cat "$FLOW_ROOT/phase"; }
wait_for_required_service_health() { [[ -f "$FLOW_ROOT/proof/worker-platform" ]]; }
verify() { [[ -f "$FLOW_ROOT/proof/worker-platform" && -f "$FLOW_ROOT/proof/worker-frontend" ]]; }
install_delivery_license() {
  if [[ -f "$FLOW_ROOT/allow-license-action" ]]; then event "license:$1"; return 0; fi
  event "UNEXPECTED-license:$1"; return 96
}
'''
        script += APPLICATION_OF + '\n' + FINISH + '\nworkflow_open\nauto_install_packages\nworkflow_start verify\nverify\nworkflow_done verify\nworkflow_finish_controlled\n'
        environment = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ['PATH'], FLOW_ROOT=str(self.root),
                           FLOW_EVENTS=str(self.events), FLOW_REF=REF, TMPDIR=str(self.root))
        result = subprocess.run(['bash', '-c', script], env=environment, text=True, capture_output=True, timeout=45)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def status(self):
        result = subprocess.run(['python3', str(BIN / 'deployment-session.py'), 'status', '--state-dir', str(self.target)+'.installation-state', '--format', 'json'],
                                text=True, capture_output=True, check=True)
        return json.loads(result.stdout)

    def stage(self, name):
        return next(stage for stage in self.status()['stages'] if stage['key'] == name)

    def actions(self):
        return self.events.read_text().splitlines() if self.events.exists() else []

    def test_failed_frontend_resume_preserves_completed_platform(self):
        self.run_flow(fail_action='frontend', success=False)
        self.assertEqual(self.stage('platform')['status'], 'DONE')
        self.assertEqual(self.stage('frontend')['status'], 'FAILED')
        self.run_flow(resume=True)
        self.assertEqual(self.actions().count('deploy:platform'), 1)
        self.assertEqual(self.actions().count('deploy:frontend'), 2)
        self.assertEqual(self.actions().count('import:common'), 1)
        self.assertEqual(self.stage('controlled')['status'], 'WAITING')
        self.assertEqual(self.stage('controlled')['reason_code'], 'WAITING_PLATFORM_ADOPTION')

    def test_old_done_missing_worker_refuses_republish_and_false_success(self):
        self.run_flow(fail_action='frontend', success=False)
        (self.proof / 'worker-platform').unlink()
        self.run_flow(resume=True, success=False)
        self.assertEqual(self.actions().count('deploy:platform'), 1)
        self.assertEqual(self.actions().count('deploy:frontend'), 1)
        self.assertEqual(self.stage('platform')['status'], 'FAILED')
        self.assertNotEqual(self.status()['overall_status'], 'COMPLETED')
        # The failed proof must not erase the earlier successful publication.
        # A third resume after repairing the worker still cannot replay SQL.
        (self.proof / 'worker-platform').touch()
        self.run_flow(resume=True)
        self.assertEqual(self.actions().count('deploy:platform'), 1)

    def test_old_done_wrong_image_refuses_republish(self):
        self.run_flow(fail_action='frontend', success=False)
        (self.proof / 'release-platform').write_text('another-target-package')
        self.run_flow(resume=True, success=False)
        self.assertEqual(self.actions().count('deploy:platform'), 1)
        self.assertEqual(self.stage('platform')['status'], 'FAILED')

    def test_action_failure_is_not_hidden_by_a_passing_proof(self):
        (self.proof / 'release-platform').write_text(self.packages['platform'])
        (self.proof / 'worker-platform').touch()
        self.run_flow(fail_action='platform', success=False)
        self.assertEqual(self.stage('platform')['status'], 'FAILED')
        self.assertNotIn('deploy:frontend', self.actions())

    def test_disabled_scope_is_skipped_in_declared_order(self):
        (self.root / 'enabled').write_text('platform-api\nfrontend\n')
        self.run_flow()
        self.assertEqual(self.stage('contract-import')['status'], 'SKIPPED')
        self.assertEqual(self.stage('contract-prepare')['status'], 'SKIPPED')
        self.assertNotIn('import:contract', self.actions())
        self.assertNotIn('prepare:contract', self.actions())
        self.assertEqual(self.status()['overall_status'], 'COMPLETED')

    def test_ready_candidate_waits_without_auto_continue_or_adopt(self):
        (self.root / 'candidate.json').write_text(json.dumps(self.candidate))
        (self.root / 'phase').write_text('READY_TO_CONTINUE\n')
        (self.proof / 'contract.runtime').touch()
        (self.proof / 'license.enforced').touch()
        result = self.run_flow()
        self.assertIn('等待操作员明确执行 continue', result.stdout)
        self.assertTrue((self.root / 'candidate.json').is_file())
        self.assertEqual(self.stage('controlled')['status'], 'WAITING')
        self.assertNotIn('prepare:contract', self.actions())
        self.assertFalse(any('adopt' in action or action.startswith('continue:') or 'unexpected-docker' in action for action in self.actions()))

    def test_unsigned_ready_runtime_waits_for_authorization(self):
        (self.root / 'phase').write_text('VERIFIED\n')
        (self.proof / 'contract.runtime').touch()
        self.run_flow()
        self.assertEqual(self.stage('controlled')['reason_code'], 'WAITING_AUTHORIZATION')
        self.assertFalse(any(action.startswith('UNEXPECTED-license:') for action in self.actions()))

    def test_completed_license_stage_does_not_reimport_old_delivery_after_web_renewal(self):
        (self.target / 'license').mkdir()
        (self.target / 'license/commercial-license.jws').write_text('original delivery artifact')
        (self.root / 'allow-license-action').touch()
        self.run_flow()
        self.assertEqual(self.actions().count('license:import'), 1)
        self.assertEqual(self.actions().count('license:activate'), 1)
        self.assertEqual(self.stage('controlled')['status'], 'WAITING')
        # The operator completes adoption/continue and independently renews in
        # the platform; recovery must inspect current authority, not reimport
        # the earlier delivery artifact or request another activation.
        (self.root / 'allow-license-action').unlink()
        (self.root / 'candidate.json').unlink()
        (self.root / 'phase').write_text('VERIFIED\n')
        (self.proof / 'contract.runtime').touch()
        (self.proof / 'license.enforced').touch()
        self.run_flow(resume=True)
        self.assertEqual(self.status()['overall_status'], 'COMPLETED')
        self.assertEqual(self.actions().count('license:import'), 1)
        self.assertEqual(self.actions().count('license:activate'), 1)
        self.assertFalse(any(action.startswith('UNEXPECTED-license:') for action in self.actions()))

    def test_resume_rejects_changed_delivery_identity_before_actions(self):
        self.run_flow(fail_action='frontend', success=False)
        before = self.actions()
        (self.media / 'SHA256SUMS').write_text('another signed delivery identity\n')
        result = self.run_flow(resume=True, success=False)
        self.assertIn('IDENTITY_CONFLICT', result.stderr)
        self.assertEqual(self.actions(), before)

    def test_external_license_same_path_changed_bytes_is_an_identity_conflict(self):
        license_file = self.root / 'commercial-license.jws'
        license_file.write_text('first opaque signed artifact\n')
        self.run_flow(fail_action='frontend', success=False, license_file=str(license_file))
        before = self.actions()
        license_file.write_text('different opaque signed artifact\n')
        self.run_flow(resume=True, success=False, license_file=str(license_file))
        self.assertEqual(self.actions(), before)


unittest.main(verbosity=2, argv=['test-delivery-workflow', *sys.argv[2:]])
PY
