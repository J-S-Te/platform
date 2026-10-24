#!/usr/bin/env python3
"""公开的只读入口可在工作站上运行，无需 sudo 或 Docker。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

sys.dont_write_bytecode=True
HERE=Path(__file__).resolve().parent
ROOT=HERE.parents[3]
ENTRY=ROOT/'服务器一键离线部署.sh'
SESSION=HERE.parent/'bin/deployment-session.py'
spec=importlib.util.spec_from_file_location('delivery_entry_fixtures',HERE/'test-delivery-inspect.py')
fixture=importlib.util.module_from_spec(spec);spec.loader.exec_module(fixture)


class EntryTests(unittest.TestCase):
    def setUp(self):
        holder=fixture.DeliveryTests();holder.setUp();self.addCleanup(holder.doCleanups)
        self.holder=holder;self.root=holder.root;self.target=self.root/'target'
        tools=self.root/'tools';tools.mkdir()
        for name in ('docker','sudo'):
            path=tools/name;path.write_text('#!/bin/sh\necho forbidden-tool >&2\nexit 91\n');path.chmod(0o755)
        self.env=dict(os.environ,PATH=str(tools)+os.pathsep+os.environ['PATH'],PYTHONDONTWRITEBYTECODE='1')

    def command(self,*args):
        return subprocess.run(['bash',str(ENTRY),*map(str,args)],env=self.env,capture_output=True,text=True,timeout=30)

    def test_directory_check_has_one_report_and_no_target_or_docker(self):
        result=self.command('--media',self.holder.media,'--target',self.target,'--check-delivery','--format','json')
        self.assertEqual(result.returncode,0,result.stderr)
        data=json.loads(result.stdout);self.assertEqual(data['status'],'NEEDS_INPUT')
        self.assertEqual(data['consumer_count'],2)
        self.assertFalse(self.target.exists());self.assertFalse(Path(str(self.target)+'.installation-state').exists())

    def test_check_reports_corruption_without_executing_media(self):
        (self.holder.media/'frontend-inspection-v1-linux-amd64.tar.gz').write_bytes(b'truncated')
        result=self.command('--media',self.holder.media,'--check-delivery','--format','json')
        self.assertEqual(result.returncode,2,result.stderr)
        self.assertEqual(json.loads(result.stdout)['status'],'INVALID')

    def test_v2_directory_and_signed_tar_use_the_public_readonly_entry(self):
        spec=importlib.util.spec_from_file_location('delivery_entry_v2_fixtures',HERE/'test-delivery-inspect-v2.py')
        v2=importlib.util.module_from_spec(spec);spec.loader.exec_module(v2)
        holder=v2.V2DeliveryTests();holder.setUp();self.addCleanup(holder.doCleanups)
        holder.v2()
        reports=[]
        for media in (holder.media,holder.outer()):
            result=self.command('--media',media,'--target',self.target,'--check-delivery','--format','json')
            self.assertEqual(result.returncode,0,result.stdout+result.stderr)
            reports.append(json.loads(result.stdout))
        self.assertEqual(reports[0],reports[1])
        checks={item['id']:item for item in reports[0]['checks']}
        self.assertEqual(checks['authorization.release.contract_management']['status'],'PASS')
        self.assertNotIn('authorization.evidence',checks)
        self.assertEqual(checks['authority.release-signature']['status'],'PENDING')
        self.assertFalse(self.target.exists())

    def test_status_is_readonly_and_needs_no_media(self):
        state=Path(str(self.target)+'.installation-state')
        identity=self.root/'identity.json'
        identity.write_text(json.dumps({'version':1,'target':str(self.target),'media':str(self.holder.media),
            'delivery_id':'sha256:'+hashlib.sha256((self.holder.media/'SHA256SUMS').read_bytes()).hexdigest(),
            'mode':'install','components':['common','contract','frontend','platform'],
            'options':{'skip_host_deps':True,'skip_configure':False,'apply_compose':False,'legacy_license_mode':'','customer':'','license_file_path':''}}))
        identity.chmod(0o600)
        result=subprocess.run([sys.executable,str(SESSION),'begin','--state-dir',str(state),'--identity-file',str(identity),'--stages','assets,verify'],capture_output=True,text=True)
        self.assertEqual(result.returncode,0,result.stderr)
        before={p.name:p.read_bytes() for p in state.iterdir()}
        result=self.command('--target',self.target,'--status','--format','json')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(json.loads(result.stdout)['current_stage'],'assets')
        self.assertEqual(before,{p.name:p.read_bytes() for p in state.iterdir()})
        self.assertFalse(self.target.exists())

    def test_readonly_flags_cannot_start_or_resume_installation(self):
        for flag in ('--install','--upgrade','--resume'):
            result=self.command('--media',self.holder.media,'--check-delivery',flag)
            self.assertNotEqual(result.returncode,0)
            self.assertNotIn('forbidden-tool',result.stderr)


if __name__=='__main__': unittest.main()
