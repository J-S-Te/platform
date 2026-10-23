#!/usr/bin/env python3
"""使用模拟的 Docker 边界运行实际候选命令行与构建器，不部署。"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

ROOT = Path(__file__).resolve().parents[4]
BINARY = os.environ.get('APPROVAL_QA_BINARY', '/private/tmp/uip-runtime-approval-workflow-qa')
DOCKER = r'''#!/usr/bin/env python3
import io,json,os,sys,tarfile
args=sys.argv[1:]
with open(os.environ['QA_DOCKER_LOG'],'a') as f: f.write(json.dumps(args)+'\n')
cid=os.environ['QA_IMAGE_ID']; version='qa-crm-v1'
if args[:2]==['buildx','version']: sys.exit(0)
if args[:2]==['image','inspect']:
    fmt=args[args.index('--format')+1]
    if fmt=='{{.Id}}': print(os.environ.get('QA_WRONG_ID',cid))
    elif fmt=='{{.Architecture}}/{{.Os}}': print(os.environ.get('QA_PLATFORM','amd64/linux'))
    elif 'org.opencontainers.image.version' in fmt and fmt.startswith('{{index'): print(version)
    elif 'com.basic-platform.license.protocol' in fmt and fmt.startswith('{{index'): print('1')
    else: print(json.dumps({'id':cid,'version':version,'protocol':'1'}))
    sys.exit(0)
if args[:1]==['tag']: sys.exit(0)
if args[:2]==['save','--output']:
    config=open(os.environ['QA_CONFIG'],'rb').read(); name=cid.split(':')[1]+'.json'
    manifest=json.dumps([{'Config':name,'RepoTags':args[3:],'Layers':[]}]).encode()
    with tarfile.open(args[2],'w') as t:
        for n,b in [('manifest.json',manifest),(name,config)]:
            h=tarfile.TarInfo(n);h.size=len(b);t.addfile(h,io.BytesIO(b))
    sys.exit(0)
print('Unexpected Docker command',file=sys.stderr);sys.exit(99)
'''


class Workflow(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='crm-approval-workflow-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        tools = self.root / 'tools'; tools.mkdir()
        self.log = self.root / 'docker.log'
        config = json.dumps({'architecture': 'amd64', 'os': 'linux', 'config': {'Labels': {'org.opencontainers.image.version': 'qa-crm-v1', 'com.basic-platform.license.protocol': '1'}}}, sort_keys=True).encode()
        self.cid = 'sha256:' + hashlib.sha256(config).hexdigest()
        configfile = self.root / 'config.json'; configfile.write_bytes(config)
        docker = tools / 'docker'; docker.write_text(DOCKER); docker.chmod(0o755)
        go = tools / 'go'; go.write_text('#!/bin/sh\n[ "$1" = run ] && [ "$2" = ./cmd/runtime-approval ] || exit 99\nshift 2\nexec "$APPROVAL_QA_BINARY" "$@"\n'); go.chmod(0o755)
        self.env = dict(os.environ, PATH=str(tools) + ':' + os.environ['PATH'], QA_DOCKER_LOG=str(self.log), QA_IMAGE_ID=self.cid, QA_CONFIG=str(configfile), APPROVAL_QA_BINARY=BINARY, PYTHONDONTWRITEBYTECODE='1')
        now = datetime.now(timezone.utc) - timedelta(seconds=1)
        self.inventory = self.root / 'old.json'
        self.inventory.write_text(json.dumps({'version': 1, 'project': 'qa-crm-only', 'deploy_root': '/opt/qa-crm-only', 'collected_at': now.isoformat(), 'compose_digest': 'sha256:' + 'c' * 64, 'release_digest': 'sha256:' + 'd' * 64, 'containers': [{'container_id': '1' * 64, 'service': 'customer-api', 'image_digest': 'sha256:' + 'b' * 64, 'version': '', 'protocol': '', 'running': True, 'one_off': False, 'started_at': (now - timedelta(hours=1)).isoformat(), 'architecture': 'amd64', 'os': 'linux'}]}))
        self.candidate = self.root / 'candidate'

    def prepare(self, extra=()):
        # 相对清单和输出路径用于验证进入 Go 工作目录之前的规范化。
        return subprocess.run(['bash', str(ROOT / '一键制作离线镜像包.sh'), '--prepare-crm-runtime-approvals', 'old.json', '--crm-image', 'qa-crm:version', '--operator', 'qa-only', '--output', 'candidate', '--no-pause', *extra], cwd=self.root, env=self.env, capture_output=True, text=True)

    def builder(self):
        return subprocess.run(['bash', str(ROOT / 'platform/deploy/production/bin/build-offline-packages.sh'), '--component', 'customer-opportunity', '--version', 'qa-reuse-v1', '--output', str(self.root / 'delivery'), '--runtime-approval-dir', str(self.candidate), '--runtime-approval-approved', '--reuse-approved-crm-image'], cwd=ROOT, env=self.env, capture_output=True, text=True)

    def test_prepare_and_package_exact_approved_image(self):
        result = self.prepare(); self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(list(self.candidate.glob('*.json'))), 4)
        review = json.loads((self.candidate / 'approval-review.json').read_text())
        self.assertTrue(review['candidate']); self.assertFalse(review['input_reviewed'])
        release = json.loads((self.candidate / 'runtime-license-customer_and_opportunity-prod.json').read_text())
        self.assertEqual(len(release['components']), 9)
        self.assertIn('customer-contract-transfer-worker', {value['service_id'] for value in release['components']})
        migration = json.loads((self.candidate / 'license-migration-evidence.json').read_text())
        self.assertEqual(len(migration['services']), 1, 'new target worker cannot become a legacy observed member')
        result = self.builder(); self.assertEqual(result.returncode, 0, result.stderr)
        package = self.root / 'delivery/customer-opportunity-backend-qa-reuse-v1-linux-amd64.tar.gz'
        with tarfile.open(package) as t:
            metadata = t.extractfile('package.env').read().decode()
        self.assertIn('IMAGE_CONFIG_DIGESTS=' + self.cid, metadata)
        commands = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertTrue(any(c[:1] == ['tag'] and c[1] == self.cid for c in commands))
        self.assertFalse(any(c[:2] == ['buildx', 'build'] for c in commands))

    def test_prepare_cannot_claim_approval(self):
        result = self.prepare(['--runtime-approval-approved'])
        self.assertNotEqual(result.returncode, 0); self.assertFalse(self.candidate.exists())

    def test_wrong_target_platform_rejected_before_output(self):
        self.env['QA_PLATFORM'] = 'arm64/linux'
        result = self.prepare(); self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.candidate.exists())

    def test_reuse_image_identity_drift_refused(self):
        self.assertEqual(self.prepare().returncode, 0)
        self.env['QA_WRONG_ID'] = 'sha256:' + 'e' * 64
        result = self.builder(); self.assertNotEqual(result.returncode, 0)
        self.assertFalse(list((self.root / 'delivery').glob('*.tar.gz')))

    def test_duplicate_component_cannot_replace_missing_worker(self):
        self.assertEqual(self.prepare().returncode, 0)
        path = self.candidate / 'runtime-license-customer_and_opportunity-prod.json'
        release = json.loads(path.read_text()); release['components'][1] = release['components'][0]
        path.write_text(json.dumps(release))
        result = self.builder(); self.assertNotEqual(result.returncode, 0)
        self.assertFalse(list((self.root / 'delivery').glob('*.tar.gz')))

    def test_old_eight_member_target_approval_cannot_omit_transfer_worker(self):
        self.assertEqual(self.prepare().returncode, 0)
        path = self.candidate / 'runtime-license-customer_and_opportunity-prod.json'
        release = json.loads(path.read_text())
        release['components'] = [value for value in release['components']
                                 if value['service_id'] != 'customer-contract-transfer-worker']
        self.assertEqual(len(release['components']), 8)
        path.write_text(json.dumps(release))
        result = self.builder()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(list((self.root / 'delivery').glob('*.tar.gz')))

    def test_target_metadata_version_drift_refused(self):
        self.assertEqual(self.prepare().returncode, 0)
        path = self.candidate / 'license-evidence.json'
        evidence = json.loads(path.read_text()); evidence['services'][0]['version'] = 'other-version'
        path.write_text(json.dumps(evidence))
        result = self.builder(); self.assertNotEqual(result.returncode, 0)
        self.assertFalse(list((self.root / 'delivery').glob('*.tar.gz')))


if __name__ == '__main__':
    if not Path(BINARY).is_file():
        raise SystemExit('Build the real QA runtime-approval binary and set APPROVAL_QA_BINARY first')
    unittest.main()
