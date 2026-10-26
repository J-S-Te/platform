#!/usr/bin/env python3
"""The supplied plan is checked before an initial upgrade mutates the target."""
from pathlib import Path
import subprocess
import tempfile
import unittest
import shlex
import json

ROOT=Path(__file__).resolve().parents[4]
SOURCE=(ROOT/'服务器一键离线部署.sh').read_text()
class ExecutionPlanGate(unittest.TestCase):
    def test_gate_wires_baseline_scope_and_stops_on_plan_failure(self):
        self.assertTrue('deployment_baseline_gate() {' in SOURCE, 'execution baseline gate is missing')
        function='deployment_baseline_gate() {'+SOURCE.split('deployment_baseline_gate() {',1)[1].split('\n}',1)[0]+'\n}'
        self.assertLess(SOURCE.index('\ndeployment_baseline_gate\n'),SOURCE.index('workflow_run backup backup_before_upgrade'))
        workflow=(ROOT/'platform/deploy/production/bin/deployment-workflow.sh').read_text()
        self.assertLess(workflow.index('flock -n 6'),workflow.index('then deployment_baseline_gate; fi'))
        self.assertLess(workflow.index('then deployment_baseline_gate; fi'),workflow.index('begin_args=(begin'))
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);(root/'bin').mkdir()
            tool=root/'bin/deployment-plan.py'
            tool.write_text('import sys\nassert "--baseline-report" in sys.argv\nassert "project" in sys.argv\nassert "platform" in sys.argv\nsys.exit(0)\n')
            prefix='\n'.join(['set -e','die() { exit 9; }', 'plan_baseline=/baseline.json','check_only=false','resume_installation=false','deployment_mode=upgrade', 'forward_recovery=false', 'recovery_evidence=', 'target_dir='+shlex.quote(str(root)), 'media_dir='+shlex.quote(str(root)), 'requested_subsystems=(project)',function])
            self.assertEqual(0,subprocess.run(['bash','-c',prefix+'\ndeployment_baseline_gate'],capture_output=True).returncode)
            tool.write_text('import sys\nsys.exit(2)\n')
            self.assertEqual(9,subprocess.run(['bash','-c',prefix+'\ndeployment_baseline_gate'],capture_output=True).returncode)
            self.assertEqual(0,subprocess.run(['bash','-c',prefix+'\nresume_installation=true\ndeployment_baseline_gate'],capture_output=True).returncode)
    def test_session_identity_binds_plan_contents_without_changing_legacy_identity(self):
        workflow=(ROOT/'platform/deploy/production/bin/deployment-workflow.sh').read_text()
        start=workflow.index('import hashlib, json, os, sys\nout,target,media')
        code=workflow[start:].split('\nPY\n',1)[0]
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);media=root/'media';media.mkdir()
            (media/'SHA256SUMS').write_text('fixture')
            plan=root/'plan.json';plan.write_text('first')
            output=root/'identity.json'
            def identity(planpath):
                subprocess.run(['python3','-',str(output),str(root),str(media),'upgrade',
                    'true','true','true','','','',planpath,'platform','project'],input=code,text=True,check=True)
                return json.loads(output.read_text())
            before=identity(str(plan))
            self.assertEqual(['platform','project'],before['components'])
            plan.write_text('changed')
            after=identity(str(plan))
            self.assertNotEqual(before['options']['plan_baseline_sha256'],after['options']['plan_baseline_sha256'])
            self.assertNotIn('plan_baseline_sha256',identity('')['options'])

if __name__=='__main__':unittest.main()
