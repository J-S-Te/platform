#!/usr/bin/env python3
"""使用隔离且摘要有效的 OCI 夹具运行实际根目录交付构建器。

不使用 Docker 套接字，不变更守护进程或源码检出目录，也不执行真实构建。
Docker 替身拒绝未知调用。Python 跟踪器始终调用实际检查器；
可选故障仅注入已经生成的测试介质。
"""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import unittest


FIXTURE = Path(__file__).with_name("test-offline-delivery-contract.py")
SPEC = importlib.util.spec_from_file_location("frozen_delivery_contract_fixture", FIXTURE)
BASE = importlib.util.module_from_spec(SPEC)
exec(compile(FIXTURE.read_bytes(), str(FIXTURE), "exec"), BASE.__dict__)
VERSION = "qa-workflow-r1"
HELPERS = ("delivery-inspect.py", "delivery-inspect-images.py", "delivery-inspect-v2.py", "subsystem-runtime.py",
           "deployment-session.py", "deployment-workflow.sh", "delivery-files.py")

DOCKER = r'''
import hashlib, io, json, os, pathlib, sys, tarfile
a=sys.argv[1:]
with open(os.environ['PROBE_DOCKER_LOG'],'a') as f: f.write(json.dumps(a)+'\n')
store=pathlib.Path(os.environ['PROBE_IMAGE_STORE'])
db=json.loads(store.read_text()) if store.exists() else {}
bases={'mysql:8.4','quay.io/keycloak/keycloak:26.2','temporalio/auto-setup:1.29.7',
       'metabase/metabase:v0.53.7','prom/prometheus:v3.5.0','prom/node-exporter:v1.9.1',
       'registry:2.8.3','ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459'}
def raw(value): return json.dumps(value,sort_keys=True,separators=(',',':')).encode()
def digest(value): return hashlib.sha256(value).hexdigest()
def persist(): store.write_text(json.dumps(db,sort_keys=True))
def image(ref):
    if ref not in db:
        if ref not in bases: sys.exit('Unbuilt/unapproved fixture image: '+ref)
        db[ref]={'architecture':'amd64','os':'linux','config':{'Cmd':[ref],'Labels':{}}}; persist()
    return db[ref]
if a==['buildx','version']: print('buildx packaging-fixture-v1'); sys.exit(0)
if len(a)==3 and a[:2]==['version','--format'] and a[2] in ('{{.Client.Version}}','{{.Server.Version}}'):
    print('packaging-fixture-v1'); sys.exit(0)
if a==['info']: sys.exit(0)
if a==['info','--format','{{.DockerRootDir}}']: print(os.environ['PROBE_DOCKER_ROOT']); sys.exit(0)
if len(a)==3 and a[:2] in (['context','inspect'],['buildx','inspect']): sys.exit(0)
if a[:2]==['buildx','build']:
    opts={}; buildargs={}; i=2
    while i<len(a)-1:
        key=a[i]
        if key in ('--load','--pull=false'): opts[key]=True; i+=1; continue
        if key not in ('--platform','--file','--tag','--target','--build-arg') or i+1>=len(a)-1:
            sys.exit('Unexpected build option: '+repr(a))
        value=a[i+1]; i+=2
        if key=='--build-arg':
            k,v=value.split('=',1); buildargs[k]=v
        else: opts[key]=value
    workspace=pathlib.Path(os.environ['PROBE_WORKSPACE']).resolve()
    context=pathlib.Path(a[-1]).resolve(); dockerfile=pathlib.Path(opts['--file']).resolve()
    if opts.get('--platform')!='linux/amd64' or not opts.get('--load') or not opts.get('--pull=false') or not context.is_dir() or not dockerfile.is_file() or not context.is_relative_to(workspace) or not dockerfile.is_relative_to(workspace):
        sys.exit('Unsafe/incomplete fixture build')
    tag=opts['--tag']
    if not tag.startswith('uip-package/'): sys.exit('Unexpected tag')
    version=buildargs.get('APP_VERSION',tag.rsplit(':',1)[1])
    db[tag]={'architecture':'amd64','os':'linux','config':{'Cmd':[tag,opts.get('--target','')],
        'Labels':{'org.opencontainers.image.version':version,'com.basic-platform.license.protocol':'1'}}}
    persist(); sys.exit(0)
if len(a)==3 and a[0]=='tag': db[a[2]]=image(a[1]); persist(); sys.exit(0)
if len(a)==5 and a[:2]==['image','inspect'] and a[3]=='--format':
    config=image(a[2]); fmt=a[4]
    if fmt=='{{.Id}}': print('sha256:'+digest(raw(config)))
    elif fmt=='{{.Architecture}}/{{.Os}}': print('amd64/linux')
    elif fmt=='{{.Os}}/{{.Architecture}}': print('linux/amd64')
    elif fmt=='{{range .RepoDigests}}{{println .}}{{end}}': print(a[2].split(':',1)[0]+'@sha256:'+digest(raw(config)))
    else: sys.exit('Unexpected inspect format: '+fmt)
    sys.exit(0)
if len(a)>=4 and a[:2]==['save','--output']:
    blobs={}; descriptors=[]
    for tag in a[3:]:
        config=raw(image(tag)); cd=digest(config); blobs[cd]=config
        manifest=raw({'schemaVersion':2,'mediaType':'application/vnd.oci.image.manifest.v1+json',
            'config':{'mediaType':'application/vnd.oci.image.config.v1+json','digest':'sha256:'+cd,'size':len(config)},'layers':[]})
        md=digest(manifest); blobs[md]=manifest
        descriptors.append({'mediaType':'application/vnd.oci.image.manifest.v1+json','digest':'sha256:'+md,'size':len(manifest),
            'annotations':{'org.opencontainers.image.ref.name':tag}})
    entries={'index.json':raw({'schemaVersion':2,'manifests':descriptors}),'oci-layout':b'{"imageLayoutVersion":"1.0.0"}'}
    entries.update({'blobs/sha256/'+k:v for k,v in blobs.items()})
    with tarfile.open(a[2],'w') as archive:
        for name,value in entries.items():
            member=tarfile.TarInfo(name); member.size=len(value); archive.addfile(member,io.BytesIO(value))
    if os.environ.get('PROBE_CHANGE_SOURCE_ON_SAVE')=='1':
        changed=pathlib.Path(os.environ['PROBE_WORKSPACE'])/'platform/third_party/license-core/license.go'
        changed.write_bytes(changed.read_bytes()+b'changed during isolated fixture build\n')
    sys.exit(0)
sys.exit('Unexpected Docker fixture invocation: '+repr(a))
'''

PYTHON_TRACE = r'''
import hashlib, io, json, os, pathlib, sys, tarfile
a=sys.argv[1:]
with open(os.environ['PROBE_PYTHON_LOG'],'a') as log: log.write(json.dumps(a)+'\n')
def sha(value): return hashlib.sha256(value).hexdigest()
def read_tar(source,mode):
    with tarfile.open(fileobj=io.BytesIO(source),mode=mode) as archive:
        return [(item,archive.extractfile(item).read() if item.isfile() else None) for item in archive]
def write_tar(entries,mode):
    out=io.BytesIO()
    with tarfile.open(fileobj=out,mode=mode) as archive:
        for item,data in entries:
            if data is not None: item.size=len(data)
            archive.addfile(item,io.BytesIO(data) if data is not None else None)
    return out.getvalue()
fault=os.environ.get('PROBE_CORRUPT_DELIVERY','')
if a and pathlib.Path(a[0]).name=='delivery-inspect.py' and '--media' in a and fault:
    media=pathlib.Path(a[a.index('--media')+1]); version='qa-workflow-r1'
    if fault=='image':
        package=media/('contract-backend-'+version+'-linux-amd64.tar.gz')
        entries=read_tar(package.read_bytes(),'r:gz')
        image_bytes=next(data for item,data in entries if item.name=='images.tar')
        images=read_tar(image_bytes,'r:'); index=json.loads(next(data for item,data in images if item.name=='index.json'))
        manifest_name='blobs/sha256/'+index['manifests'][0]['digest'].split(':')[1]
        manifest=json.loads(next(data for item,data in images if item.name==manifest_name))
        config_name='blobs/sha256/'+manifest['config']['digest'].split(':')[1]
        images=[(item,data+b' ' if item.name==config_name else data) for item,data in images]
        damaged=write_tar(images,'w')
        entries=[(item,damaged if item.name=='images.tar' else (sha(damaged)+'  images.tar\n').encode() if item.name=='SHA256SUMS' else data) for item,data in entries]
        package.write_bytes(write_tar(entries,'w:gz'))
    elif fault=='approval':
        package=media/('deployment-assets-'+version+'.tar.gz')
        entries=read_tar(package.read_bytes(),'r:gz')
        db=json.loads(pathlib.Path(os.environ['PROBE_IMAGE_STORE']).read_text())
        config=json.dumps(db['uip-package/contract-backend:'+version],sort_keys=True,separators=(',',':')).encode()
        approval=json.dumps({'version':1,'application':'contract_management','environment':'prod','components':[
            {'service_id':'contract-api','protocol':1,'coverage_digest':'sha256:'+'a'*64,'image_digest':'sha256:'+sha(config)}]}).encode()
        member=tarfile.TarInfo('./runtime-license-contract_management-prod.json'); member.size=len(approval); member.mode=0o600
        entries.append((member,approval)); package.write_bytes(write_tar(entries,'w:gz'))
        lock=media/'PLATFORM_ASSETS_COMPATIBILITY.env'
        lock.write_text(''.join('ASSETS_PACKAGE_SHA256='+sha(package.read_bytes())+'\n' if line.startswith('ASSETS_PACKAGE_SHA256=') else line+'\n' for line in lock.read_text().splitlines()))
    else: sys.exit('Unknown fixture fault')
    pathlib.Path(str(package)+'.sha256').write_text(sha(package.read_bytes())+'  '+package.name+'\n')
    manifest=media/'SHA256SUMS'
    names=[line.split(None,1)[1] for line in manifest.read_text().splitlines()]
    manifest.write_text(''.join(sha((media/name).read_bytes())+'  '+name+'\n' for name in names))
    pathlib.Path(os.environ['PROBE_FAULT_LOG']).write_text(fault+'\n')
os.execv(sys.executable,[sys.executable]+a)
'''


class DeliveryPackaging(unittest.TestCase):
    def setUp(self):
        BASE.DeliveryContract.setUp(self)
        self.addCleanup(self.temp.cleanup)
        self.output = self.root / "delivery"
        self.archive = self.root / ("uip-offline-delivery-" + VERSION + "-linux-amd64.tar")
        self.python_log = self.root / "python.log"
        self.fault_log = self.root / "fault.log"
        self.env.update(PROBE_WORKSPACE=str(self.workspace), PROBE_IMAGE_STORE=str(self.root / "images.json"),
                        PROBE_PYTHON_LOG=str(self.python_log), PROBE_FAULT_LOG=str(self.fault_log))
        for name, program in (("docker", DOCKER), ("python3", PYTHON_TRACE)):
            path = self.tools / name
            path.write_text("#!" + sys.executable + "\n" + program)
            path.chmod(0o755)

    def command(self, args, *, success=True, env=None):
        result = subprocess.run(args, env=self.env if env is None else env, capture_output=True,
                                text=True, timeout=240)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def build(self, *, success=True, env=None):
        return self.command(["bash", str(self.workspace / "一键制作离线镜像包.sh"),
                             "--components", "contract", "--version", VERSION,
                             "--skip-host-deps", "--no-pause", "--output", str(self.output)], success=success, env=env)

    def manifest(self):
        return {name: digest for digest, name in (line.split(None, 1) for line in (self.output / "SHA256SUMS").read_text().splitlines())}

    def test_full_root_build_ships_checked_helpers_and_identical_dir_tar_identity(self):
        result = self.build()
        self.assertIn("统一交付检查", result.stdout)
        expected = self.manifest()
        for name, source in (("清理首次部署产物.sh", self.workspace / "清理首次部署产物.sh"),
                             ("FIRST_INSTALL_RESET.md", self.workspace / "操作说明/失败首装清理与重新部署.md")):
            self.assertIn(name, expected)
            self.assertEqual((self.output / name).read_bytes(), source.read_bytes())
        self.assertEqual((self.output / "清理首次部署产物.sh").stat().st_mode & 0o777, 0o755)
        help_result = self.command(["bash", str(self.output / "清理首次部署产物.sh"), "--help"])
        self.assertIn('--discard-fresh-data', help_result.stdout)
        for name in HELPERS:
            self.assertIn(name, expected)
            self.assertEqual((self.output / name).read_bytes(), (self.production / "bin" / name).read_bytes())
        for name, digest in expected.items():
            self.assertEqual(hashlib.sha256((self.output / name).read_bytes()).hexdigest(), digest, name)
        asset = self.output / ("deployment-assets-" + VERSION + ".tar.gz")
        with tarfile.open(asset) as archive:
            names = {member.name.removeprefix("./"): member for member in archive if member.isfile()}
            for name in ("bin/deployment-workflow.sh", "bin/deployment-session.py", "bin/delivery-runtime.py",
                         "bin/delivery-runtime.sh", "bin/subsystem-runtime.py", "bin/delivery-inspect-v2.py", "DELIVERY_WORKFLOW.md"):
                self.assertIn(name, names)
                self.assertEqual(archive.extractfile(names[name]).read(), (self.production / name).read_bytes())
        digest, name = Path(str(self.archive) + ".sha256").read_text().split()
        self.assertEqual(name, self.archive.name)
        self.assertEqual(digest, hashlib.sha256(self.archive.read_bytes()).hexdigest())
        with tarfile.open(self.archive) as archive:
            files = {member.name for member in archive if member.isfile()}
            self.assertEqual(files, {self.output.name + "/" + name for name in expected} | {self.output.name + "/SHA256SUMS"})
        checker_calls = [json.loads(line) for line in self.python_log.read_text().splitlines() if "delivery-inspect.py" in line]
        self.assertEqual(len(checker_calls), 1)
        self.assertEqual(checker_calls[0][0], str(self.production / "bin/delivery-inspect.py"))
        docker_before = self.log.read_bytes()
        reports = []
        for media in (self.output, self.archive):
            checked = self.command(["bash", str(self.output / "服务器一键离线部署.sh"), "--media", str(media), "--check-delivery", "--format", "json"])
            reports.append(json.loads(checked.stdout))
        expected_id = hashlib.sha256((self.output / "SHA256SUMS").read_bytes()).hexdigest()
        self.assertEqual([report["delivery_id"] for report in reports], [expected_id, expected_id])
        self.assertEqual(reports[0], reports[1])
        self.assertEqual(reports[0]["components"], ["common", "contract", "frontend", "platform"])
        self.assertEqual(reports[0]["status"], "NEEDS_INPUT")
        self.assertEqual(reports[0]["consumer_count"], 2)
        self.assertEqual(self.log.read_bytes(), docker_before, "read-only checker called Docker")

    def test_rebound_corrupt_approval_or_oci_blob_blocks_final_tar_publication(self):
        for fault, expected_check in (("approval", "authorization.release.contract_management"), ("image", "package.content")):
            with self.subTest(fault=fault):
                self.output = self.root / ("delivery-" + fault)
                result = self.build(success=False, env=dict(self.env, PROBE_CORRUPT_DELIVERY=fault))
                self.assertEqual(self.fault_log.read_text().strip(), fault)
                self.assertIn("拒绝发布总归档", result.stderr)
                self.assertFalse(self.archive.exists())
                self.assertFalse(Path(str(self.archive) + ".sha256").exists())
                self.assertTrue((self.output / "SHA256SUMS").is_file())
                # 全部外层封装都已重新绑定，失败必须来自真实语义检查器，不能只是容易触发的外层校验和不匹配。
                expected = self.manifest()
                for name, digest in expected.items():
                    self.assertEqual(hashlib.sha256((self.output / name).read_bytes()).hexdigest(), digest)
                checked = self.command([sys.executable, str(self.production / "bin/delivery-inspect.py"),
                                        "--media", str(self.output), "--format", "json"], success=False)
                report = json.loads(checked.stdout)
                failures = [check for check in report["checks"] if check["status"] == "FAIL"]
                self.assertEqual(failures[0]["id"], expected_check)

    def test_source_change_during_real_builder_blocks_mixed_release(self):
        result = self.build(success=False, env=dict(self.env, PROBE_CHANGE_SOURCE_ON_SAVE="1"))
        self.assertIn("构建期间源码输入或工具链基线发生变化", result.stderr)
        self.assertFalse(self.archive.exists())
        self.assertFalse(Path(str(self.archive) + ".sha256").exists())
        self.assertFalse((self.output / ("platform-backend-" + VERSION + "-linux-amd64.tar.gz")).exists())
        self.assertTrue((self.output / "BUILD_INFO.txt").is_file())

    def test_extension_accepts_baseline_with_reset_tool_and_rebinds_current_tool(self):
        self.build()
        baseline = self.output
        baseline_bytes = {name:(baseline/name).read_bytes() for name in
                          ('清理首次部署产物.sh','FIRST_INSTALL_RESET.md','SHA256SUMS')}
        output = self.root/'extension'
        version = 'qa-workflow-r2'
        self.command(['bash', str(self.workspace/'一键制作离线镜像包.sh'),
                      '--extend-from',str(baseline),'--components','contract','--version',version,
                      '--skip-host-deps','--no-pause','--output',str(output)])
        expected = {name:digest for digest,name in (line.split(None,1) for line in
                                                   (output/'SHA256SUMS').read_text().splitlines())}
        for name in ('清理首次部署产物.sh','FIRST_INSTALL_RESET.md'):
            self.assertIn(name, expected)
            self.assertEqual(expected[name], hashlib.sha256((output/name).read_bytes()).hexdigest())
        self.assertEqual((output/'清理首次部署产物.sh').read_bytes(),
                         (self.workspace/'清理首次部署产物.sh').read_bytes())
        for name,raw in baseline_bytes.items():
            self.assertEqual((baseline/name).read_bytes(),raw,'baseline changed: '+name)
        archive_path = self.root/('uip-offline-delivery-'+version+'-linux-amd64.tar')
        with tarfile.open(archive_path) as archive:
            self.assertEqual(archive.extractfile('extension/清理首次部署产物.sh').read(),
                             (output/'清理首次部署产物.sh').read_bytes())
        checked = self.command([sys.executable,str(self.production/'bin/delivery-inspect.py'),
                                '--media',str(output),'--phase','prepare','--format','json'])
        self.assertNotEqual(json.loads(checked.stdout)['status'], 'INVALID')


if __name__ == "__main__":
    unittest.main()
