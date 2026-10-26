#!/usr/bin/env python3
"""Local wiring and read-only Compose projections; no Docker daemon is contacted."""
from pathlib import Path
import re
import unittest
import tempfile
import subprocess
import json
import shutil
import os
ROOT = Path(__file__).resolve().parents[1]

class LocalProjectBackgroundTests(unittest.TestCase):
    def test_local_compose_runs_separate_background_with_bootstrap_probe(self):
        text = (ROOT / 'compose.local.yaml').read_text()
        api = re.search(r'\n  project-api:\n(.*?)(?=\n  [\w-]+:)', text, re.S).group(1)
        worker = re.search(r'\n  sla-notifier:\n(.*?)(?=\n  [\w-]+:)', text, re.S).group(1)
        self.assertIn('PROJECT_RUN_WORKER_WITH_API: "false"', api)
        self.assertIn('PROJECT_BACKGROUND_BUILD_ID:', api)
        self.assertIn('TEMPORAL_ADDRESS: temporal:7233', worker)
        self.assertIn('PROJECT_BACKGROUND_BUILD_ID:', worker)
        self.assertIn('/healthz', worker)
        self.assertIn('stop_grace_period: 420s', worker)
        self.assertNotIn('SLA_SCAN_TENANTS:', worker)

    def test_standalone_replaces_empty_worker_with_same_background_entrypoint(self):
        text = (ROOT.parent / 'project_management/compose.yaml').read_text()
        self.assertNotIn('profiles: ["split-runtime"]', text)
        self.assertIn('entrypoint: ["/usr/local/bin/sla-notifier"]', text)
        self.assertIn('${PROJECT_RUNTIME_ENV_FILE:-./.env.local}', text)
        self.assertIn('PROJECT_RUN_WORKER_WITH_API: "false"', text)

    def test_local_start_refresh_and_stop_use_lifecycle_boundary(self):
        text = (ROOT / 'scripts/docker-local.sh').read_text()
        start = text[text.index('start_stack()'):text.index('refresh_project_backend()')]
        refresh = text[text.index('refresh_project_backend()'):text.index('refresh_data_analysis_backend()')]
        self.assertLess(start.index('local_project_background prepare'), start.index('run_migrations'))
        self.assertIn('project-api sla-notifier', start)
        self.assertIn('local_project_background activate', start)
        self.assertLess(refresh.index('local_project_background prepare'), refresh.index('project-migrate'))
        self.assertIn('local_project_background activate', refresh)
        for action in ('down)', 'stop)'):
            block = text[text.rindex('    '+action):]
            self.assertLess(block.index('local_project_background teardown'), block.index('compose_run '+action[:-1]))

    def test_activate_failures_are_only_tolerated_for_license_pause(self):
        # 本地未导入厂商 JWS 时生产状态机按设计拒绝完成切换并保留候选；
        # 启动链不得放宽状态机，也不得把其他失败静默放行。
        text = (ROOT / 'scripts/docker-local.sh').read_text()
        helper = (ROOT / 'scripts/local-project-background.sh').read_text()
        self.assertIn('local_project_background_license_paused() {', helper)
        self.assertEqual(helper.count('local_project_background_license_paused'), 1)
        self.assertEqual(text.count('if ! local_project_background activate; then'), 2)
        self.assertEqual(text.count('local_project_background_license_paused'), 2)
        self.assertIn('PAUSED_LICENSE', text)

class ComposeRuntimeEnvironmentTests(unittest.TestCase):
    build_id = 'project-background-sha256-' + 'a' * 64

    def compose_model(self, source, address='127.0.0.1:9193'):
        # Copy the real compose file into an isolated directory: relative optional
        # license files and implicit .env must never read the developer's runtime.
        with tempfile.TemporaryDirectory(prefix='uip-compose-runtime-') as tmp:
            root = Path(tmp)
            text = source.read_text()
            compose = root / 'compose.yaml'
            compose.write_text(text)
            runtime = root / 'runtime.env'
            runtime.write_text('PROJECT_BACKGROUND_BUILD_ID=' + self.build_id +
                               '\nPROJECT_BACKGROUND_HEALTH_ADDRESS=' + address + '\n')
            empty = root / 'empty.env'
            empty.write_text('')
            env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin')}
            # Only synthetic mandatory host inputs; Build/address deliberately absent.
            for name in re.findall(r'\$\{([A-Z_]+):\?', text):
                env[name] = 'qa-synthetic'
            for name in re.findall(r'\$\{([A-Z_]*ENV_FILE)', text):
                env[name] = str(empty)
            env['PROJECT_RUNTIME_ENV_FILE'] = str(runtime)
            result = subprocess.run(
                ['docker', 'compose', '--env-file', str(empty), '-f', str(compose),
                 '--profile', '*', 'config', '--format', 'json'],
                cwd=root, env=env, text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads(result.stdout)

    def test_standalone_preserves_runtime_build_without_host_variable(self):
        model = self.compose_model(ROOT.parent / 'project_management/compose.yaml')
        for name in ('project-api', 'sla-notifier'):
            self.assertEqual(model['services'][name]['environment']['PROJECT_BACKGROUND_BUILD_ID'],
                             self.build_id, name)

    def test_health_probe_uses_runtime_address_without_host_variable(self):
        for source in (ROOT / 'compose.local.yaml',
                       ROOT.parent / 'project_management/compose.yaml'):
            with self.subTest(compose=str(source)):
                worker = self.compose_model(source)['services']['sla-notifier']
                probe = worker['healthcheck']['test']
                # Compose config serializes dollar escapes. The container receives
                # one dollar; execute that actual shell command with a wget argv spy.
                command = probe[1].replace('$$', '$') if probe[0] == 'CMD-SHELL' else None
                with tempfile.TemporaryDirectory(prefix='uip-probe-argv-') as tmp:
                    wget = Path(tmp) / 'wget'
                    wget.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
                    wget.chmod(0o700)
                    env = dict(worker['environment'])
                    env['PATH'] = tmp
                    argv = ['/bin/sh', '-c', command] if command else probe[1:]
                    result = subprocess.run(argv, env=env,
                                            text=True, capture_output=True, timeout=5)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.splitlines(),
                                     ['-q', '-O', '/dev/null', 'http://127.0.0.1:9193/healthz'])

class LocalProjectControlAdapterTests(unittest.TestCase):
    def run_adapter(self, fail=''):
        with tempfile.TemporaryDirectory(prefix='uip-qa-local-background-') as tmp:
            root=Path(tmp).resolve()
            (root/'deploy/production/bin').mkdir(parents=True)
            (root/'runtime').mkdir()
            shutil.copyfile(ROOT/'deploy/production/bin/project-background.sh',root/'deploy/production/bin/project-background.sh')
            image='sha256:'+'a'*64
            service={'image':image,'environment':{'PROJECT_BACKGROUND_BUILD_ID':'project-background-sha256-'+'a'*64,'TEMPORAL_ADDRESS':'temporal:7233','TEMPORAL_NAMESPACE':'default'},'depends_on':{'temporal':{'condition':'service_healthy'}},'healthcheck':{'test':['CMD','wget','http://127.0.0.1:9093/healthz']}}
            (root/'model.json').write_text(json.dumps({'services':{'project-api':service,'sla-notifier':service}}))
            (root/'control.json').write_text(json.dumps({'protocol':1,'engine':'temporal','tenant_id':'qa-tenant','environment':'dev','build_id':'project-background-sha256-'+'a'*64,'bootstrap_ready':True,'routing_ready':True,'execution_ready':True,'active_activities':0,'active_leases':0,'drained':True}))
            script=r'''set -Eeuo pipefail
source "$ADAPTER"
project_root="$TASK_ROOT"; project_env_file="$TASK_ROOT/project.env";env_file="$TASK_ROOT/platform.env";compose_project=qa-local
QA_IMAGE="sha256:$(printf '%064d' 0 | tr 0 a)"
env_value() { case "$2" in OIDC_TENANT_ID) printf 'qa-tenant\n';;PLATFORM_ENVIRONMENT_CODE) printf 'dev\n';;esac; }
compose() {
 printf 'compose %s\n' "$*" >> "$TASK_ROOT/calls"
 case "$*" in
  *'config --format json project-api sla-notifier') cat "$TASK_ROOT/model.json";;
  'ps -q sla-notifier') printf 'owned\n';;
  'exec -T sla-notifier /usr/local/bin/project-background-control '*)
   local action='';shift 4
   while (($#));do if [[ "$1" == --action ]];then action="$2";shift 2;else shift;fi;done
   cat "$TASK_ROOT/control.json";[[ "$action" != "$FAIL_ACTION" ]];;
  *) return 81;;
 esac
}
docker() {
 printf 'docker %s\n' "$*" >> "$TASK_ROOT/calls"
 case "$*" in
  'image inspect --format {{.Id}} '*) printf '%s\n' "$QA_IMAGE";;
  'inspect --format {{.Image}} '*) printf '%s\n' "$QA_IMAGE";;
  *'io.uip.project-background.protocol'*) printf '1\n';;
  'ps -aq '*) printf 'owned\n';;
  *) return 82;;
 esac
}
local_project_background prepare
local_project_background activate
'''
            result=subprocess.run(['/bin/bash','-c',script],env={'PATH':'/usr/bin:/bin:/usr/sbin:/sbin','ADAPTER':str(ROOT/'scripts/local-project-background.sh'),'TASK_ROOT':str(root),'FAIL_ACTION':fail},text=True,capture_output=True,timeout=20)
            state=root/'runtime/project-background/operation.json'
            return result,(root/'calls').read_text(),json.loads(state.read_text()) if state.exists() else None

    def test_local_control_uses_config_digest_and_fixed_service(self):
        result,calls,state=self.run_adapter()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(state['stage'],'complete')
        self.assertEqual(state['release_image'],'sha256:'+'a'*64)
        self.assertIn('inspect --format {{.Image}} owned',calls)
        for action in ('inspect','pause','drain','ensure','resume','verify'):
            self.assertIn('exec -T sla-notifier /usr/local/bin/project-background-control --action '+action,calls)

    def test_local_drain_failure_propagates_and_keeps_operation(self):
        result,calls,state=self.run_adapter('drain')
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(state['stage'],'drain-pending')
        self.assertNotIn('--action ensure',calls)

if __name__ == '__main__': unittest.main()
